# Spec métier — Reprise après interruption (« crash-resumable »)

> **Statut : §9 (skip par métadonnées) IMPLÉMENTÉ ; le reste reste DRAFT.** Le flag manifeste
> `skip_if_satisfied` (défaut off) fait qu'au run-time un `rebuild_index` dont **toutes les
> partitions** portent déjà la `data_compression` cible est **skippé** (outcome `skipped`, une
> ligne de log `— skipped in 0s (already PAGE)` / `.log` `skipped: already PAGE`, compteur history
> `runs.skipped`). Lecture étroite `mssql.IndexCompression(schema,table,index)` par partition ;
> comparaison pure `compressionSatisfied` (partition-aware) ; gardé côté moteur par
> `WithCompressionReader`. Rend le re-jeu d'un manifeste de compression interrompu **bon marché**
> (les ops déjà faites ne sont pas refaites) — cf. §9. **Le curseur d'opération `State.ResumeFromOp`
> (§3.4/§6) est désormais écrit progressivement, donc crash-safe** : `advanceCursor` le fait avancer
> à `i+1` **après chaque opération complétée** (succès ou skip) et le persiste (`WriteState` rendu
> **atomique** temp+rename, pour qu'un crash en cours d'écriture ne laisse jamais un sidecar tronqué).
> Il gèle sur un trou laissé par `on_failure: continue` (le curseur n'avance que si `*cursor == i`),
> pour que le re-run rejoue l'op échouée et les idempotentes suivantes plutôt que de sauter un effet
> jamais produit. La recovery le conserve au requeue (déjà en place), et le re-run saute les ops
> `i < curseur`. Un *crash* (≠ drain) renseigne donc maintenant le curseur : la reprise repart de
> l'op suivante, sans dépendre du skip métadonnée (§9), qui reste complémentaire (préfixe compression
> bon marché). L'**arrêt propre sur Ctrl+C (§3.1) est fait** (`graceful-stop.md` : 1× drain / 2× hard).
> **Non encore implémenté** : le vrai `ALTER INDEX … RESUME` (§4.2), l'orchestration `abort-resumable`
> (§3.6). Une itération de conception reste requise pour le RESUME.
>
> Créé le 2026-06-17, à la suite d'un essai de compression de masse (manifeste
> `01.to_run/030_compress_exampledb_indexes.yaml`, 74 index EXAMPLEDB).

## 1. Objectif métier

Quand une opération longue (typiquement un **rebuild d'index avec compression** sur une
très grosse table) est **interrompue** — `Ctrl+C`, arrêt/kill du process SqlGoPace, crash
de la machine, coupure réseau — l'utilisateur veut que, au run suivant, l'outil
**reprenne là où il s'était arrêté** plutôt que de tout refaire depuis le début.

Le coût d'un redémarrage à zéro sur ce type d'opération est élevé (heures de rebuild,
pression sur le journal de transactions, fenêtre de maintenance). C'est précisément le
scénario que la compression de masse rend fréquent.

## 2. Contexte : compression = REBUILD

Changer la compression d'un index existant **est** un `ALTER INDEX … REBUILD WITH
(DATA_COMPRESSION = …)`. Il n'existe pas d'opération SQL Server « compresser » distincte ;
un REORGANIZE ne peut pas changer la compression (`internal/ddl/manifest.go:401` :
« cannot change data compression — that requires a REBUILD »).

Conséquence : tout ce qui suit sur la reprise des rebuilds s'applique directement aux
opérations de compression. Une opération de compression interrompue est un rebuild
interrompu.

## 3. État actuel observé (constats techniques)

### 3.1 Aucune gestion de signal

Il n'y a **aucun handler de signal** dans le code (`signal.Notify` / `os.Interrupt` :
0 occurrence). Un `Ctrl+C` (SIGINT) tue donc le process **immédiatement** : la connexion
tombe, SQL Server avorte l'instruction en cours. Il n'y a pas d'arrêt « propre » qui
mettrait l'opération en pause de manière contrôlée avant de quitter.

### 3.2 Reprise *en cours de process* : vraie reprise (déjà implémentée)

La boucle de monitoring sait faire une **vraie** pause/reprise SQL, mais uniquement
**tant que le process vit** et **en réaction à la pression** (journal / verrous), pas sur
interruption utilisateur :

- `MonitoredRunner.runStatement` abandonne l'instruction par annulation de contexte (une
  *attention* qui **met en pause** un resumable côté serveur tout en gardant la connexion
  épinglée vivante), KILL en repli si l'instruction ne s'arrête pas à temps
  (`internal/run/monitored_runner.go:132-159`).
- `runLoop` attend l'accalmie puis ré-émet l'instruction de reprise
  (`internal/run/monitored_runner.go:105-130`), qui est un vrai `ALTER INDEX … RESUME`
  (`internal/run/monitored_runner.go:89`, `ddl.ResumableControlSQL(op, "RESUME")`).

C'est le mécanisme « WAIT_AT_LOW_PRIORITY → RESUMABLE pause/resume → KILL » de la doc
produit. **Il ne couvre pas l'interruption externe** (Ctrl+C / kill / crash).

### 3.3 Reprise *après crash* : actuellement un redémarrage, pas une reprise

Après une interruption, le manifeste reste dans `02.processing/`. Au run suivant, le
`Recoverer` le réconcilie :

- Les actions de récupération sont `Adopt` / `Resume` / `Restart`
  (`internal/run/recovery.go:16-61`). `DecideRecovery` choisit `Adopt` si un orphelin est
  encore vivant, sinon `Resume` si un resumable est connu, sinon `Restart`.
- **MAIS** dans `Recover()`, les branches `Resume` **et** `Restart` font exactement la même
  chose : `requeue` → « *re-enqueued for an idempotent re-run* »
  (`internal/run/recovery.go:174-184`).
- Le commentaire de l'action `Resume` est explicite : « *continues a resumable operation;
  re-enqueued for an idempotent re-run in this version (**true RESUME is a refinement**)* »
  (`internal/run/recovery.go:22-24`).

Autrement dit : **la vraie reprise après crash n'est pas implémentée.** Le message
utilisateur le confirme : « *interrupted manifest(s) left in processing; the next run will
resume them* » (`cmd/sqlgopace/main.go:283`) — mais « resume » signifie ici « re-jouer le
manifeste », pas « reprendre l'index au point d'arrêt ».

### 3.4 Pas de point de reprise par-opération

L'état persistant (`State`, `internal/run/state.go:12-20`) ne stocke **pas** de curseur
d'opération : il garde `manifest`, `database`, `spid`, `login_time`, `marker`, `command`,
`started_at`. Il n'y a aucune trace de « j'en étais à l'opération N sur 74 ».

Conséquence pour un manifeste multi-opérations (ex. nos 74 index) : au re-jeu, **tout le
manifeste repart de l'opération 1**. Les index déjà compressés sont **re-rebuildés**
(idempotent — résultat correct — mais travail refait).

### 3.5 Conditions d'éligibilité RESUMABLE

`RESUMABLE` n'est injectable que sous conditions (`ddl_compatibility.yaml:27/34/51`) :

- SQL Server **2017+** (major 14) pour `rebuild_index` ;
- éditions **Enterprise / Azure** uniquement ;
- **exige `ONLINE`** (`requires: [online]`).

Donc : pas de resumable sur **Standard**, ni sur un **rebuild de heap** (note
`ddl_compatibility.yaml:70` : pas de RESUMABLE ni WAIT_AT_LOW_PRIORITY pour un heap). Une
vraie reprise après crash ne sera **possible que là où RESUMABLE était actif**. Ailleurs,
seul un redémarrage de l'opération est envisageable.

### 3.6 Risque de blocage par un resumable en pause

Quand la session est tuée alors qu'un rebuild `RESUMABLE = ON` tournait, SQL Server laisse
l'opération **en pause** (progression conservée côté serveur). Relancer un **nouveau**
`REBUILD` sur cet index peut alors être **rejeté** par SQL Server tant que le resumable en
pause n'est pas repris ou abandonné. L'outil expose déjà une sous-commande
**`abort-resumable`** (`cmd/sqlgopace/abort.go`) pour purger un resumable en pause, mais le
flux de recovery ne l'orchestre pas automatiquement aujourd'hui.

## 4. Le besoin (ce qu'on veut acter)

1. **Reprise réelle après interruption externe** (Ctrl+C / kill / crash / coupure), pas
   seulement après détection de pression en cours de run.
2. Quand l'opération interrompue était un rebuild **RESUMABLE en pause**, la reprise doit
   émettre un `ALTER INDEX … RESUME` (réutiliser la progression conservée) au lieu d'un
   REBUILD complet.
3. Pour un **manifeste multi-opérations**, ne pas refaire les opérations déjà terminées :
   reprendre à la première opération non terminée.
4. **Dégradation propre** quand la reprise n'est pas possible (Standard, heap, resumable
   non injecté) : redémarrer l'opération, en le signalant clairement à l'utilisateur.
5. Gérer le **resumable en pause bloquant** (résoudre / abandonner avant de relancer) sans
   intervention manuelle systématique.

## 5. Scénarios métier (à valider au brainstorming)

- **S1 — Kill pendant l'index 31/74 (Enterprise, resumable actif).** Attendu : au run
  suivant, les index 1–30 ne sont pas retouchés, l'index 31 **reprend** via RESUME, puis
  32–74 s'enchaînent.
- **S2 — Kill pendant l'index 31/74 (Standard, pas de resumable).** Attendu : l'index 31
  redémarre de zéro (inévitable), mais 1–30 ne sont pas refaits ; message explicite « pas
  de reprise possible, redémarrage de l'opération ».
- **S3 — Crash machine complet.** Attendu : même comportement que S1/S2 au redémarrage de
  l'outil (état reconstruit depuis `02.processing/` + l'état serveur).
- **S4 — Resumable laissé en pause puis run relancé.** Attendu : pas d'échec « resumable
  operation already in progress » ; l'outil reprend ou abandonne proprement.

## 6. Questions ouvertes pour le brainstorming

- **Granularité de l'état** : faut-il un curseur d'opération dans `State` (op N/M + statut),
  ou s'appuyer uniquement sur l'idempotence + l'état serveur (resumable existant) ?
- **Détection « déjà fait »** : comment savoir qu'un index est déjà à la compression cible
  sans le rebuild ? (lecture `data_compression_desc` avant chaque op — déjà fait côté
  planner ; à porter côté run ?) Cela rendrait le re-jeu *cheap* même sans curseur.
- **Vraie reprise vs re-jeu idempotent** : implémenter `Resume` = vrai `ALTER INDEX RESUME`
  (réutilise la progression serveur) vs simplement « skip ce qui est fait ». Les deux sont
  utiles et combinables.
- **Arrêt propre sur Ctrl+C** : installer un handler de signal qui met en pause le
  resumable et écrit un état « pausé proprement » avant de quitter — réutiliser la
  mécanique `runLoop`/`ResumableControlSQL` existante ?
- **Orchestration `abort-resumable`** : la recovery doit-elle reprendre, ou abandonner
  automatiquement, un resumable en pause incompatible avec le re-jeu ?
- **Périmètre v1** : se limiter au cas `rebuild_index` RESUMABLE (le plus fréquent et le
  seul qui conserve une progression serveur), shrink et autres ops traités séparément
  (le shrink est déjà chunké, voir `specs/SHRINK.md`).
- **Découpage des manifestes** : alternative pragmatique sans nouveau code — recommander/
  générer des manifestes plus petits pour borner le coût d'un re-jeu. Palliatif, pas une
  vraie reprise.

## 7. Hors périmètre (pour mémoire)

- La pause/reprise **en réaction à la pression** est déjà en place (§3.2) et n'est pas
  l'objet de cette feature.
- Le **shrink** suit un driver chunké distinct (`ShrinkRunner`) qui reprend naturellement
  entre chunks ; son besoin de reprise est différent et déjà partiellement couvert.

## 8. Références code (au 2026-06-17)

| Sujet | Emplacement |
|---|---|
| Compression ⇒ REBUILD obligatoire | `internal/ddl/manifest.go:401` |
| Matrice RESUMABLE (2017+/Enterprise/Azure/online) | `ddl_compatibility.yaml:27,34,51` |
| Pas de RESUMABLE pour heap | `ddl_compatibility.yaml:70` |
| Vraie pause/reprise en cours de run | `internal/run/monitored_runner.go:89,105-159` |
| Génération SQL PAUSE/RESUME/ABORT | `internal/ddl/control.go` (`ResumableControlSQL`) |
| Actions de recovery + commentaire « true RESUME is a refinement » | `internal/run/recovery.go:16-61` |
| `Resume` == `Restart` == requeue | `internal/run/recovery.go:174-184` |
| État persistant sans curseur d'opération | `internal/run/state.go:12-20` |
| Message « next run will resume » (= re-jeu) | `cmd/sqlgopace/main.go:283` |
| Sous-commande de purge d'un resumable en pause | `cmd/sqlgopace/abort.go` (`abort-resumable`) |
| Absence de handler de signal | aucun `signal.Notify` dans le dépôt |
| Lecture compression côté planner (déjà-fait) | `internal/maint/decide.go` (`decideCompression`) + `data_compression_desc` |
| Check d'existence per-op au preflight (modèle à suivre) | `internal/mssql/existence.go`, `internal/preflight/preflight.go` |

## 9. Piste de conception : skip par métadonnées (proposé le 2026-06-17)

> Issu d'un essai réel : compresser 74 index en PAGE sur `EXAMPLEDB` (Standard, donc rebuilds
> **offline et atomiques**). Idée : rendre chaque op **idempotente à coût quasi nul** au re-jeu.

### 9.1 Principe

Avant d'exécuter chaque `rebuild_index` portant un `data_compression`, **lire la métadonnée
de compression de l'objet** et **ne rebuild que si elle diffère de la cible**. Exemple : cible
PAGE → on rebuild uniquement si la compression courante ≠ PAGE.

Cela couvre la **moitié « skip ce qui est fait »** du besoin (§4.3, §6) **sans** avoir besoin du
vrai `ALTER INDEX … RESUME`. Un manifeste de 74 ops tué à l'op 31 : au re-run, 1–30 sont
**skippés** (simple lecture), 31 refait, 32–74 enchaînent.

### 9.2 Pourquoi c'est propre sur Standard (et en général)

Un rebuild offline est **atomique** : une interruption fait un **rollback complet**, donc
l'index revient à sa compression d'avant. Au re-jeu il y a donc **deux états seulement** par
index — *déjà à la cible* (skip) ou *pas à la cible* (refait) — **jamais de demi-état**. Sur
Enterprise online/resumable, l'index interrompu est laissé *en pause* (≠ cible) : il sera donc
refait par ce mécanisme, sauf à combiner avec un vrai RESUME (§6).

### 9.3 Granularité partition

`sys.partitions` porte **une ligne par partition** (`data_compression`/`data_compression_desc`,
2 = PAGE). Donc :

- op sur l'index entier (`PARTITION = ALL`) → skip **uniquement si *toutes* les partitions**
  sont déjà à la cible ;
- op ciblant `PARTITION = n` → ne tester que cette partition.

### 9.4 Décisions à trancher

1. **Compression ≠ défrag.** Un `rebuild_index` peut aussi viser un défrag ; skipper « déjà
   compressé » zapperait ce défrag. → rendre le skip **opt-in** (flag manifeste/CLI, p. ex.
   `skip_if_satisfied: true`), **défaut off**, pour préserver le contrat « fais ce que je dis »
   du run direct. Pour un manifeste *de compression* pure (cas d'usage), on l'active.
2. **Où.** Au **moment d'exécuter chaque op** (au run-time, comme le shrink génère son SQL),
   pas au preflight global : on lit l'état le plus frais, ce qui gère naturellement la
   progression d'un run précédent interrompu.
3. **Réutilisation.** Le planner fait **déjà** cette comparaison (`decideCompression` lit
   `data_compression_desc` et n'émet rien si déjà à la cible). Porter cette logique sur le
   chemin run via un read `mssql` étroit, p. ex. `IndexCompression(schema, table, index) →
   desc par partition`. Pas de logique neuve.
4. **Reporting.** Logguer chaque skip (`[k] rebuild_index dbo.X.IX — already PAGE, skipped`)
   et distinguer `skipped` / `done` dans le `.log` et l'history, pour qu'un re-run montre
   clairement ce qui a été réutilisé.
5. **Portée.** Étendre l'idée aux autres ops à cible métadonnée (p. ex. `add_column` déjà
   présente, `alter_column` déjà au bon type) ? À discuter ; commencer par la compression.

### 9.5 Rapport au reste de la spec

- Mécanisme **complémentaire**, pas un substitut, au vrai RESUME (§6) : il rend le re-jeu
  *bon marché* mais ne reprend pas un index interrompu *en cours* de rebuild.
- Beaucoup plus simple qu'un curseur d'opération dans `State` (§6) : l'« état » est lu
  directement côté serveur, source de vérité, donc **aucune persistance de progression** à
  maintenir.
