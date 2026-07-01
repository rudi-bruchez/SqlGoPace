# BATCH-DML — `UPDATE` / `DELETE` découpés en lots

> Source de vérité du comportement visé pour les opérations DML par lots.
> **It1 + It2 + It3 (TUI live) implémentés.** It1 : `batch_update`/`batch_delete`, stratégie
> `predicate`, `set:`/`where:` déclaratifs + échappatoire `set_raw:`/`where_raw:`, calibrage adaptatif,
> réutilisation réaction/monitoring, preflight (permission + avis RCSI), RCSI/SI dans `ServerInfo`. It2 :
> stratégie `key_range` (clé entière simple) avec **curseur persistant** (sidecar `.op<i>.wm` dans
> `02.processing/`) pour reprise après crash, inférence de clé via `ClusteringKeyColumns`. It3 :
> **progression live** — un *step-sink* moteur (`WithStepSink`, cf. `specs/progress-tui.md`) alimente
> stdout (`[i/N] cmd cible — started/… in Xs`) et le TUI (compteur op i/N, chrono live), plus une ligne
> batch live (lignes faites/estimées, %, taille de lot, lignes/s ; `BatchDMLProgress` complété par
> `RowsPerSec`). It3 restant / It4 (calibrage RCSI fin plus poussé, surfaçage DMV d'escalade,
> exactement-une-fois, clés composites/non entières) : cf. §7.

## 1. Objectif et contexte

SqlGoPace n'exécute aujourd'hui que du **DDL** (rebuild/reorganize d'index, compression, shrink,
checkdb, statistiques). Un besoin DBA récurrent sur ces mêmes bases multi-To en 24/7 est une
**instruction DML massive en un seul coup** — « mettre une colonne à une valeur sur toute une
table », ou « supprimer tout (ou un large sous-ensemble) d'une table » — qui sur une base de
production chargée est dangereuse pour les raisons que SqlGoPace gère déjà côté DDL, **plus deux
spécifiques au DML** :

- **Escalade de verrous.** Un gros `UPDATE`/`DELETE` prend des verrous **X (exclusifs) de données**
  ligne/page ; dès qu'une seule instruction détient ~5000 verrous sur un objet, SQL Server
  **escalade en un verrou X de table**, gelant tout accès concurrent à la table entière.
- **Explosion du journal.** Une instruction = une transaction ; le journal ne peut pas être tronqué
  tant qu'elle n'est pas validée, donc un DML de plusieurs heures fait croître le journal sans borne.

La parade est le motif classique : **découper l'instruction en une boucle** de petits lots validés
individuellement, chacun maintenu sous le seuil d'escalade, en échantillonnant le serveur entre les
lots et en réagissant à la pression — ce qui est **architecturalement identique au driver de
shrink** existant.

C'est aussi là que la question **RCSI finit par servir** : contrairement au DDL (verrous de schéma,
où le RCSI est orthogonal — cf. la discussion sur `CheckServer`), un lot DML entre en conflit sur des
**verrous de données**, et le RCSI change directement à quel point l'escalade est perturbante. Cette
spec **expose donc et utilise** l'état RCSI/SI de la base.

## 2. Pourquoi le driver de shrink est le patron (carte de réutilisation)

Un shrink est déjà « une boucle de `DBCC SHRINKFILE` par chunks, pas une instruction DDL unique »
avec un **deuxième driver parallèle** (`ShrinkRunner`) qui ne passe pas par `MonitoredRunner`. Un DML
par lots a la même forme et réutilise les mêmes coutures :

| Couture | Shrink | Batch DML |
|---------|--------|-----------|
| Struct opération | `ddl.Shrink` (`internal/ddl/manifest.go`) | nouvelle `ddl.BatchDML` |
| SQL généré au run | `ShrinkChunkSQL` bâti par chunk dans la boucle | `BatchDMLChunkSQL` bâti par lot |
| Driver | `ShrinkRunner` + iface `ShrinkDriver`, `WithShrinkRunner` | `BatchDMLRunner` + `BatchDMLDriver`, `WithBatchDMLRunner` |
| Iface de lecture | `ShrinkReader` (`internal/run/shrink.go`) | nouvelle `BatchDMLReader` |
| Maths de pas (pures) | `shrink_calc.go` `InitialStepMB`/`AdjustStepMB`/clamp | nouveau `batch_calc.go` `InitialBatchRows`/`AdjustBatchRows`/clamp |
| Dispatch moteur | `processOne` type-assert `ddl.Shrink` → `e.shrink.Run(...)` | type-assert `ddl.BatchDML` → `e.batchDML.Run(...)` |
| Réaction/monitoring | `pumpSamples` + `supervise` + `ReactionSink` + `Capabilities` + `IgnoreSource` | **réutilisés tels quels** |
| Sûreté d'annulation | `CancelSafe: true` (valide par chunk) | `CancelSafe: true` (valide par lot) |
| Config | `ShrinkConfig` → `ShrinkTuning` | `BatchDMLConfig` → `BatchDMLTuning` |

Le DML par lots est **meilleur que le shrink sur un point** : il a un **point de reprise** naturel
(les lignes supprimées le restent / le curseur avance), donc il peut être rendu **réellement
reprenable après crash**, ce que le shrink n'est pas.

---

## 3. Forme du manifeste

Deux discriminants (`batch_update`, `batch_delete`) se décodent en une seule struct `BatchDML` — à
l'image de `Shrink` qui porte `type: data|log` et dont `CommandType()` renvoie
`shrink_data`/`shrink_log`.

```yaml
operations:
  # UPDATE idempotent d'une table entière vers une valeur littérale — le cas phare.
  - operation: batch_update
    schema: dbo
    table: Orders
    set:                                 # colonne -> littéral scalaire (string/number/bool/null)
      Status: 'Archived'
    where:                               # optionnel ; liste de conditions simples, ET-liées
      - { column: Status, op: '=', value: 'Pending' }
    batch:
      strategy: predicate                # predicate (défaut) | key_range
      key: OrderID                       # requis pour key_range seulement ; défaut = clé cluster
      initial_rows: 5000                 # optionnel ; auto-calibré sinon
    options:
      maxdop: 1                          # seule option « WITH » applicable au DML

  # DELETE découpé d'un sous-ensemble (ou, avec confirmation, de toute la table).
  - operation: batch_delete
    schema: dbo
    table: AuditLog
    where:
      - { column: CreatedAt, op: '<', value: '2024-01-01' }
    batch: { initial_rows: 10000 }

  # Échappatoire SQL brute, sous garde (manifestes signés DBA ; injectés verbatim).
  - operation: batch_update
    schema: dbo
    table: Invoice
    set_raw: "Status = 'Closed', ClosedAt = SYSUTCDATETIME()"   # exclusif avec set:
    where_raw: "Status = 'Open' AND DueDate < '2024-01-01'"     # exclusif avec where:
```

### Règles de champ (`Validate()`, fail-fast au chargement — comme la validation des ops existantes)

- Exactement un de (`set:` | `set_raw:`) pour `batch_update` ; **aucun des deux** pour `batch_delete`.
- Au plus un de (`where:` | `where_raw:`).
- Les valeurs de `set:` sont des **littéraux scalaires uniquement** (pas de référence de colonne) →
  **idempotence** garantie.
- `op` ∈ une petite liste blanche : `=`, `<>`, `<`, `<=`, `>`, `>=`, `IS NULL`, `IS NOT NULL`, `IN`.
- `set_raw` / `where_raw` sont des chaînes non vides injectées **verbatim** dans l'instruction
  générée. C'est une exception explicite et documentée au principe « jamais de SQL brut », justifiée
  par le fait que les manifestes sont écrits par des DBA (même périmètre de confiance qu'un script
  `.sql`), et elle a des conséquences (cf. §4).
- **Garde « table entière »** : un `batch_delete` (ou `batch_update`) **sans** `where`/`where_raw`
  exige un `confirm_full_table: true` explicite, sinon le preflight **échoue**. (Évite la purge
  accidentelle ; aligné sur le verrouillage des intentions destructrices ailleurs.) Le message
  rappelle que **`TRUNCATE TABLE`** est l'outil adapté pour un vidage total inconditionnel quand les
  FK/triggers/accès le permettent — le DELETE découpé sert quand TRUNCATE n'est pas viable.

---

## 4. Idempotence & reprise après crash (modèle de sûreté central)

Un lot est **validé individuellement**, donc un crash/kill ne perd au plus que le lot en cours. Que
ce lot soit **re-jouable** sans risque à la reprise dépend de l'idempotence :

| Cas | Idempotent ? | Stratégie de reprise |
|-----|--------------|----------------------|
| `batch_delete` (tout prédicat) | **Oui** (une ligne supprimée le reste) | boucle predicate, sans point de reprise |
| `batch_update` avec `set:` littéral | **Oui** (`col = 'X'` identique au re-jeu) | predicate (`WHERE col <> 'X'`) **ou** key_range + curseur |
| `batch_update` avec `set_raw` référençant la ligne (`Counter = Counter + 1`) | **Non** | **pas re-jouable sans risque** |

Règles :

- **Stratégie predicate (défaut).** `… TOP(@n) … WHERE <prédicat>` en boucle jusqu'à
  `@@ROWCOUNT = 0`. Pour DELETE et UPDATE littéral, le prédicat est **auto-limitant** (les lignes
  qu'il vient de changer ne matchent plus), donc une reprise continue simplement — **aucun point de
  reprise à gérer, sûr par construction.** Coût : chaque lot ré-évalue le prédicat, la colonne du
  prédicat doit donc être indexée pour les grosses tables.
- **Stratégie key_range (opt-in).** Parcourt une `key` unique/ordonnable par plages ascendantes
  (`WHERE key > @curseur ORDER BY key`), en persistant `MAX(key)` traité comme point de reprise.
  Prévisible sur multi-To et sans re-scan, **mais** le curseur vit dans le sidecar de processing (un
  fichier), pas dans la même transaction que le SQL → la reprise est **au-moins-une-fois** sur le lot
  frontière. Donc key_range n'est offert **que pour un SET idempotent** (`set:` littéral), où
  ré-appliquer le lot frontière est sans effet.
- **`set_raw` non idempotent** est autorisé mais **marqué non reprenable** : le preflight émet un
  `WARN`, l'op tourne sans point de reprise, et un crash en cours laisse la table partiellement mise à
  jour pour **réconciliation manuelle** (consignée dans le `.log`). Le MVP ne tente **pas** l'exactement-
  une-fois pour ce cas (il faudrait une table de contrôle côté base mise à jour dans la transaction du
  lot — reporté en itération ultérieure si demandé).

Stratégie par défaut selon le verbe : `batch_delete` → `predicate` ; `batch_update` → `predicate`
(basculer en `key_range` quand la table est énorme et le prédicat n'est pas indexé sélectivement).

---

## 5. Intégration RCSI / isolation snapshot (le fil conducteur)

Un lot DML détient des **verrous X de données** ; le RCSI/SI change qui en pâtit :

- **RCSI OFF (READ COMMITTED prend des verrous S) :** l'escalade en **verrou X de table bloque tous
  les lecteurs** → l'application gèle sur cette table. Maintenir chaque lot **sous le seuil d'escalade
  (~5000 verrous)** et honorer la réaction « bon citoyen » au blocage est **critique**.
- **RCSI / SI ON (lecteurs en versions de lignes) :** les lecteurs ne sont **pas bloqués** par les
  verrous X ligne/page du lot, donc l'escalade est bien moins perturbante pour les *lectures* (elle
  bloque toujours les *écrivains*). Le coût dominant se déplace vers le **version store de tempdb** :
  chaque ligne modifiée/supprimée génère une version conservée jusqu'au commit du lot → de petits
  lots + commit-par-lot bornent la croissance du version store/tempdb.

Concrètement, cette spec ajoute la conscience du RCSI/SI à trois endroits :

1. **`mssql.ServerInfo`** gagne `RCSIEnabled bool` et `SnapshotIsolation bool`, lus dans
   `DetectServer` depuis `sys.databases` (`is_read_committed_snapshot_on`,
   `snapshot_isolation_state_desc`). `CheckServer` les affiche sur la ligne de faits serveur, à côté
   des `adr=`/`recovery=` existants.
2. **Avis de preflight** (`WARN`, non bloquant) pour une op `batch_*` : si **RCSI OFF**, avertir que
   l'escalade bloquera les lecteurs et recommander un `initial_rows` prudent (< seuil d'escalade) ;
   si **RCSI/SI ON**, signaler la croissance du version store tempdb et de surveiller tempdb.
3. **Calibrage par défaut du lot** indexé sur le RCSI : RCSI **off** → lot initial auto plafonné sous
   le seuil d'escalade (p. ex. ≤ 4000 lignes) ; RCSI **on** → le plafond peut se détendre (le journal
   et le version store, pas le blocage des lecteurs, deviennent les signaux gouvernants). Défaut
   seulement — un `initial_rows` explicite l'emporte toujours.

L'escalade est sinon gérée **réactivement**, comme tout le reste : le calibreur adaptatif réduit le
lot dès que de la pression `LCK_M_*` / blocage apparaît entre lots (pas besoin de pré-lire les DMV
d'escalade). On n'émet **pas** `ALTER TABLE … SET (LOCK_ESCALATION = DISABLE)` automatiquement — cela
modifie la table et relève de l'opérateur ; calibrer sous le seuil est le levier non intrusif.

La pression **version store / tempdb** sous RCSI est traitée par le garde-fou transversal
`TEMPDB-GUARD.md` (preflight no-start + alerte runtime ; arrêt seulement si tempdb est plein **et**
que c'est notre session le contributeur matériel). Le version store n'étant pas attribuable par
session à bon marché, ce cas reste en **alerte seule** côté DML.

---

## 6. Conception du driver — `internal/run/batch_dml.go` (calque de `shrink.go`)

- **`BatchDMLReader`** (interface étroite, satisfaite par `*mssql.Conn`) :
  - `ClusteringKeyColumns(ctx, schema, table) ([]mssql.KeyColumn, error)` — **nouvelle** lecture des
    colonnes de la clé cluster/PK en ordre de clé (étendre `internal/mssql/indexes.go` ; via
    `sys.index_columns`). Nécessaire pour `key_range` et pour défaut de `batch.key`.
  - `EstimateRows(ctx, schema, table) (int64, error)` — réutiliser `ObjectInventory`
    (`analysis.go`) pour une estimation du nombre de lignes (progression %, calibrage initial).
  - `SessionWaits(ctx, spid) ([]mssql.SessionWait, error)` — réutilisé, pour les deltas de calibrage.
  - (lectures log-reuse + plancher de journal actif réutilisées du jeu de lectures du shrink.)
- **`BatchDMLRunner`** struct + `NewBatchDMLRunner(exec, reader, sampler, clk, cfg, opts...)`,
  `WithBatchDMLProgress(...)` — même patron de construction que `ShrinkRunner`.
- **`Run(ctx, op ddl.BatchDML, res ddl.ResolvedOptions, ignore IgnoreSource, sink ReactionSink)
  ([]BatchDMLResult, error)`** — même famille de signature que `ShrinkDriver.Run`. La boucle :
  1. estimer les lignes, choisir la taille de lot initiale (`InitialBatchRows`, plafonnée RCSI).
  2. boucler : bâtir le SQL du lot (`BatchDMLChunkSQL`), échantillonner les waits avant, exécuter le
     lot sous `supervise` (goroutines exécution + échantillonnage, cancel-safe), échantillonner après,
     lire `@@ROWCOUNT`.
  3. s'arrêter quand `@@ROWCOUNT == 0` (predicate) ou que le curseur dépasse la fin (key_range).
  4. entre les lots : `awaitRelief` sur pression blocage/journal (réutilisé), avancer/clamper via
     `AdjustBatchRows`, persister le curseur pour key_range, honorer le backoff no-progress +
     `SelfWaitTimeout`.
- **`batch_calc.go`** (pur, testé sans base) : `InitialBatchRows(estRows, rcsi, t)`,
  `AdjustBatchRows(size, elapsed, WaitDeltas, t)` (réduit sur WRITELOG/PAGEIOLATCH/`LCK_*`/blocage ;
  agrandit quand calme et sous la durée de lot cible), `clampBatchRows(min,max)`.

### Jeu de réactions DML (via `reaction.go` existant)

Les options propres au DDL (`ONLINE`/`RESUMABLE`/`WAIT_AT_LOW_PRIORITY`) **ne s'appliquent pas** — un
lot DML n'est pas resumable au sens SQL. Réactions utiles entre/pendant les lots :

- **continuer** / **réduire le lot** (calibreur adaptatif) / **attendre le répit** (`awaitRelief`) /
  **arrêt propre + point de reprise** (le travail fait est validé ; reprise ultérieure).
- Un **kill** en cours de lot ne rollback que le petit lot courant → bon marché.
  `Capabilities.CancelSafe = true` ; `Resumable = false` ; `Ignore`/`MaxBlock` réutilisés (donc
  `ignore_blocked_sessions` et le plafond par op `max_block_minutes` marchent aussi pour le DML,
  gratuitement).

---

## 7. Câblage du pipeline (ce que touche chaque couche)

- **`internal/ddl/manifest.go`** — ajouter la struct `BatchDML` (+ `Set map[string]any`, `SetRaw`,
  `Where []Condition`, `WhereRaw`, `Batch BatchSpec`, `ConfirmFullTable bool`, `Verb` interne) ;
  implémenter `CommandType()` (`batch_update`/`batch_delete`), `Target()` (`schema.table`, `Name`
  vide), `Validate()` ; ajouter `case "batch_update"` / `case "batch_delete"` à `decodeOperation`
  (positionne `Verb`).
- **`internal/ddl/resolve.go`** — ajouter `case BatchDML: return o.Options` à `overridesOf` ; comme
  `Shrink`, l'exempter de la résolution d'options façon index (seul `maxdop` est pertinent).
- **`internal/ddl/generate.go`** — `generateBatchDML` (instruction indicative pour le plan/rapport) +
  le runtime `BatchDMLChunkSQL(op, batchSize, watermark, res)` ; réutiliser `quoteIdent`/`qualified`/
  `nLiteral` ; rendre le `set:`/`where:` déclaratif en T-SQL (littéraux quotés, prédicat depuis la
  liste blanche d'`op`) ou insérer `set_raw`/`where_raw` verbatim.
- **`internal/ddl/plan.go` / `expand.go`** — génériques, **pass-through** (pas de changement ;
  `expand` forwarde déjà les ops non-`RebuildIndex`, mais **ajouter un test de non-régression** que
  `BatchDML` survit à l'expansion — la classe de bug déjà rencontrée avec `OnFailure`).
- **`internal/ddl/matrix.go` / `ddl_compatibility.yaml`** — ajouter `batch_update`/`batch_delete`
  avec `maxdop` seul (pas d'online/resumable/walp).
- **`internal/mssql`** — étendre `ServerInfo`/`DetectServer` (RCSI/SI) ; ajouter `ClusteringKeyColumns`
  (`indexes.go`) ; ajouter une sonde de permission `UPDATE`/`DELETE` sur la table (cf. preflight).
- **`internal/preflight/preflight.go`** — `CheckBatchDML` : table existe (réutilisé), colonnes de
  `set` existent + types compatibles (réutiliser `ColumnExists`, ajouter un contrôle de type),
  `batch.key` existe & unique pour `key_range`, **permission UPDATE/DELETE** sur la table, garde
  table-entière, **WARN** sur référence FK / trigger DELETE, et l'avis RCSI. (La logique de skip
  base-/fichier-scoped est inchangée ; les ops `batch_*` sont schema.table-scoped donc empruntent le
  chemin d'existence normal.)
- **`internal/run/engine.go`** — interface `BatchDMLDriver` + `WithBatchDMLRunner` ; branche dans
  `processOne` `if b, ok := step.Operation.(ddl.BatchDML); ok && e.batchDML != nil { … }` ; mapper
  `[]BatchDMLResult` → un nouveau `report.BatchDMLReport` (lignes affectées, lots, taille finale de
  lot, raison d'arrêt, curseur) ; persister/restaurer le curseur dans `finalizePartial`/recovery pour
  `key_range`.
- **`internal/config/config.go`** — `BatchDMLConfig` → `BatchDMLTuning` (rows initial/min/max, durée
  de lot cible, backoff no-progress, self-wait timeout, log-reuse-wait timeout — même forme que
  `ShrinkConfig`).
- **`cmd/sqlgopace/main.go`** — câbler `WithBatchDMLRunner(NewBatchDMLRunner(...))` dans
  `buildEngine` ; `--explain`/plan affiche la stratégie, la clé, l'estimation de lignes, la note
  idempotence/reprise, et l'avis RCSI.

---

## 8. Inconvénients / limites (délibérés, documentés)

1. **`set_raw` non idempotent n'est pas exactement-une-fois** au crash (frontière au-moins-une-fois)
   — marqué non reprenable ; réconciliation manuelle. L'exactement-une-fois (table de contrôle côté
   base) est reporté.
2. **DELETE en cascade / triggers DELETE** peuvent faire qu'un lot touche bien plus que `@n` lignes
   (FK `ON DELETE CASCADE`, effets de bord de trigger) → le preflight avertit ; le calibreur réagit
   tout de même, mais l'opérateur doit tenir compte du fan-out de cascade en fixant `initial_rows`.
3. **`set_raw`/`where_raw` injectés verbatim** — aucune analyse/validation. Modèle manifeste-de-
   confiance uniquement (écrit par DBA), jamais d'entrée utilisateur final.
4. **La stratégie predicate peut re-scanner** à chaque lot si la colonne du prédicat n'est pas
   indexée → lent sur grosses tables ; choisir `key_range` là.
5. **Lignes modifiées en concurrence par l'appli** : predicate ré-inclut naturellement toute ligne
   qui matche encore ; key_range traite chaque clé une fois (une ligne ré-introduite derrière le
   curseur avec l'ancienne valeur est manquée) — acceptable pour « converger une colonne vers une
   valeur » ; documenté.
6. **Pas de substitution `TRUNCATE`** — le DELETE découpé est volontairement journalisé ligne à
   ligne ; pour un vidage inconditionnel autorisé, l'opérateur doit utiliser `TRUNCATE` (le preflight
   le signale).

---

## 9. Phasage

- **It1 (MVP).** `batch_update` + `batch_delete` ; `set:` littéral/`where:` simple déclaratifs **et**
  l'échappatoire `set_raw:`/`where_raw:` sous garde ; stratégie **predicate** seule (sûre par
  construction pour DELETE + UPDATE littéral ; `set_raw` tourne non-reprenable avec un WARN) ;
  calibrage adaptatif (`batch_calc.go`) ; réutilisation réaction/monitoring ; attente log-reuse ;
  preflight (existence, permission, garde table-entière, avis RCSI) ; `ServerInfo` RCSI/SI + ligne
  `CheckServer`. Rapport + `--explain`.
- **It2.** Stratégie `key_range` avec **curseur** persistant pour reprise après crash (updates
  idempotents) ; `ClusteringKeyColumns` ; inférence de clé par défaut ; câblage recovery ; support des
  clés composites.
- **It3.** Calibrage par défaut conscient du RCSI ; intégration TUI live (lignes/s, taille de lot
  courante, curseur/progression %, raison d'arrêt) ; surfaçage optionnel des DMV d'escalade.
- **It4 (raffinements).** Exactement-une-fois pour updates non idempotents via table de contrôle
  côté base ; calibrage conscient des cascades/triggers ; un dry-run CLI estimant lots/journal par
  lot.

## 10. Tests (sans base ; `-race`)

- **ddl :** parse/validate `batch_update`/`batch_delete` (exclusivité set vs set_raw ; delete refuse
  set ; liste blanche where ; raw vide refusé ; table-entière exige confirm) ; `generate` round-trip
  des deux stratégies et de l'insertion brute ; non-régression pass-through d'`expand` ; matrice ne
  garde que `maxdop`.
- **run (pur) :** `InitialBatchRows`/`AdjustBatchRows`/clamp en table-driven (plafond RCSI ; réduit
  sur WRITELOG/`LCK_*`/blocage ; agrandit au calme) ; boucle du driver sur un faux `BatchDMLReader` +
  faux `Sampler` — boucle predicate s'arrête à `@@ROWCOUNT=0`, key_range avance le curseur, un arrêt
  sous pression est propre et le travail fait est validé, backoff no-progress + self-wait timeout.
- **preflight :** colonne/permission manquante échoue ; table-entière sans confirm échoue ; RCSI-off
  émet l'avis ; FK/trigger en WARN.
- **mssql :** `ClusteringKeyColumns` et les requêtes de détection RCSI/SI derrière le tag
  `integration`.

## 11. Vérification (bout-en-bout, base jetable)

1. `make test` au vert.
2. Écrire à la main un `batch_delete` sur une table semée de plusieurs millions de lignes ;
   `--explain` montre la stratégie, l'estimation de lots, et (sur une base non-RCSI) l'avis
   d'escalade de verrous.
3. L'exécuter ; depuis une 2ᵉ session, confirmer que les lectures sont/ne sont pas bloquées selon
   l'état RCSI, que le journal reste borné entre lots, et qu'un `KILL` en cours laisse un partiel
   propre (run predicate reprend par re-lancement sans double effet ; DELETE continue simplement).
4. Refaire un `batch_update` littéral avec `strategy: key_range` ; interrompre ; confirmer que le
   manifeste de recovery porte le curseur et que le run repris continue en milieu de table.
