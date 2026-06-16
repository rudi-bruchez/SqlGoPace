# SHRINK — plan d'implémentation

> Compagnon de `SHRINK.md` (design v1). Développement **linéaire, mono-développeur**, pensé
> pour économiser les tokens : étapes autonomes, ordonnées cœur-pur d'abord, chacune
> vérifiable par `make test && make vet` **sans base** ; l'intégration/e2e (DB réelle) n'arrive
> qu'aux étapes 5 et 8.
>
> Convention de travail : une étape = un commit (ou une petite PR). Ne pas démarrer l'étape
> N+1 tant que l'étape N ne passe pas `make test && make vet && make lint`.

## Principe d'architecture (rappel)

Le shrink **ne se plie pas** au modèle « une opération = un statement ». Il est piloté par un
**driver dédié** (`internal/run/shrink.go`) qui lit des DMV au runtime, construit le SQL par
chunk via des helpers de `ddl`, et mène sa propre boucle. L'engine **route** les opérations
`Shrink` vers ce driver au lieu de `MonitoredRunner`.

Toute la logique décisionnelle (calcul de cible, stepsize initial, ajustement, no-progress)
est écrite en **fonctions pures** testables sans DB, sur le modèle de `runLoop`/`supervise`/
`DecideReaction` (cœur pur + I/O injectées).

---

## Étape 0 — Matrix + command types (fondation, minuscule)

**But** : déclarer l'éligibilité des options shrink par version/édition.

- `internal/ddl/ddl_compatibility.yaml` : ajouter les command types `shrink_data` et
  `shrink_log`. Sous `shrink_data`, `wait_at_low_priority` éligible **SQL Server 2022 (16.x)+**
  seulement. Rien d'éligible sous `shrink_log` (pas de WALP).
- Vérifier que `matrix.go` charge ces entrées sans changement de code (juste data).

**Tests** : un cas dans `matrix_test.go` : `Applicable(16, tier, "shrink_data", "wait_at_low_priority")`
vrai en 2022, faux en 2019.

**Checkpoint** : `make test`.

---

## Étape 1 — Type d'opération `shrink` dans `ddl` (parse / validate / target)

**But** : parser et valider le YAML de l'opération.

- `internal/ddl/manifest.go` :
  - `case "shrink": return decodeInto[Shrink](node)` dans `decodeOperation`.
  - Struct `Shrink` :
    ```go
    type Shrink struct {
        Type            string          // "data" | "log"
        Files           string          // "all" | nom logique ; défaut "all"
        EmptyFile       bool            // réservé Phase 2 ; doit être false en v1
        TargetFreeSpace string          // brut "10%" | "100MB" ; parsé par TargetSpec
        Options         OptionOverrides // seul WaitAtLowPriority est pertinent
    }
    ```
  - `CommandType()` : `"shrink_data"` ou `"shrink_log"` selon `Type` (sert le matrix).
  - `Target()` : cible un fichier/une base, **pas** `schema.table` (cf. mémoire
    *check_db target shape* — ne pas détourner `ObjectRef.table`). Renvoyer
    `ObjectRef{Name: o.Files}` (ou un champ dédié si plus clair).
  - `Validate()` : `Type ∈ {data, log}` ; `EmptyFile == false` (sinon erreur « réservé
    Phase 2 ») ; `TargetFreeSpace` parsable et > 0.
- Nouveau `internal/ddl/shrink.go` (fonctions pures, sans DB) :
  - `type TargetSpec struct { Percent *int; AbsoluteMB *int }`
  - `ParseTargetFreeSpace(s string) (TargetSpec, error)` : `"10%"` → Percent ; `"100MB"`/`"100 MB"`
    → AbsoluteMB ; rejette le vide / négatif / unité inconnue.
  - `FinalTargetMB(usedMB int, spec TargetSpec) int` : `Percent` ⇒
    `ceil(used × (1 + N/100))` ; `AbsoluteMB` ⇒ `used + N`. (Le clamp au plancher `used`
    se fait ici aussi : jamais < usedMB.)

**Tests** (`manifest_test.go`, `shrink_test.go`) : décodage YAML data/log ; rejets
(`type` invalide, `emptyfile: true`, targetfreespace illisible) ; table de cas pour
`ParseTargetFreeSpace` et `FinalTargetMB` (arrondis, clamp).

**Checkpoint** : `make test`.

---

## Étape 2 — Résolution d'options + génération SQL par chunk

**But** : décider WALP (sans forcer ONLINE) et produire les statements `DBCC SHRINKFILE`.

- `internal/ddl/resolve.go` :
  - Branche dédiée pour les command types shrink : ne résoudre **que** `wait_at_low_priority`
    (via le matrix) ; **ne pas** toucher online/resumable/sort_in_tempdb ni appliquer la
    règle « WALP requires ONLINE ». `ABORT_AFTER_WAIT = BLOCKERS` seulement si
    `Policy.AllowAbortBlockers`, sinon `SELF`. Pas de `MAX_DURATION` (champ ignoré pour shrink).
  - `overridesOf` : ajouter le `case Shrink: return o.Options`.
- `internal/ddl/generate.go` (générateur **dédié**, pas `withClause`) :
  - `ShrinkChunkSQL(file string, targetMB int, res ResolvedOptions) string` →
    `DBCC SHRINKFILE (N'file', targetMB) WITH WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF), NO_INFOMSGS;`
    (clause WALP seulement si `res.WaitAtLowPriority`, sinon juste `WITH NO_INFOMSGS`).
  - `ShrinkTruncateOnlySQL(file string) string` →
    `DBCC SHRINKFILE (N'file', TRUNCATEONLY) WITH NO_INFOMSGS;`
  - `Generate()` pour un `Shrink` : renvoyer une chaîne **représentative** (ex. le SQL du
    premier chunk ou un commentaire), car le SQL réel est multi-statement et construit au
    runtime par le driver. Documenter que `PlannedOperation.SQL` d'un shrink est indicatif.

**Tests** (`resolve_test.go`, `generate_test.go`) : WALP résolu ON/OFF selon matrix et
override ; aucun `ONLINE`/`RESUMABLE` jamais injecté pour shrink ; quoting du nom de fichier
(`N'...'`, doublage des `'`) ; forme exacte des deux helpers.

**Checkpoint** : `make test`.

---

## Étape 3 — Cœur pur du chunking (stepsize, ajustement, décisions)

**But** : toute la logique de calibration en fonctions pures testables.

- `internal/ddl/shrink.go` ou `internal/run/shrink_calc.go` (au choix ; garder pur) :
  - `InitialStepMB(reclaimMB int, cfg ShrinkConfig) int` : tranches < 5 Go / 5–50 Go / > 50 Go.
  - `AdjustStepMB(step int, elapsed time.Duration, w WaitDeltas, cfg ShrinkConfig) int` :
    `/2` si `WRITELOG>10ms` ou `PAGEIOLATCH_EX>20ms` ou blocage>30s ; `*2` si I/O<5ms,
    pas de wait, `elapsed<targetBatch` ; borné `[min,max]`.
  - `NextTargetMB(current, step, final int) int` : `max(current-step, final)`.
  - Type `WaitDeltas` (WRITELOG, PAGEIOLATCH_EX avg ms, blockingSeconds) consommé par
    `AdjustStepMB`.

**Tests** : tables de cas pour chaque fonction (réduction, augmentation, bornes, dernier
chunk qui colle à `final`).

**Checkpoint** : `make test`.

---

## Étape 4 — Configuration `shrink:` (defaults)

**But** : exposer les défauts du §7.3 de `SHRINK.md`, tous optionnels.

- `internal/config/config.go` : struct `ShrinkConfig` (champs `initial_step_small_mb`, …,
  `self_wait_timeout_minutes`, `log_reuse_wait_timeout_minutes`) + accesseurs `time.Duration`
  comme `MonitoringConfig`. Application des défauts quand un champ est zéro (bloc absent ⇒
  tous défauts).
- Brancher `ShrinkConfig` dans la structure `Config` racine.

**Tests** (`config_test.go`) : bloc absent ⇒ défauts ; override partiel ⇒ seuls les champs
fournis changent ; valeurs négatives rejetées ou clampées.

**Checkpoint** : `make test`.

---

## Étape 5 — Lectures DMV dans `internal/mssql` (adapter, DB réelle)

**But** : les lectures runtime dont le driver a besoin. Code derrière interfaces, tests
`integration`-tagués (skippés sans `SQLGOPACE_TEST_DSN`).

- `internal/mssql` (nouveau `shrink.go` ou compléter `databases.go`/`recovery.go`) :
  - `FileSpace(ctx, fileType) ([]FileSpace, error)` : `name, type_desc, size_mb, used_mb,
    free_mb` depuis `sys.database_files` + `FILEPROPERTY`. (`fileType` = ROWS|LOG.)
  - `FileSizeMB(ctx, file string) (int, error)` : taille courante (pour la boucle / progression).
  - `LogReuse(ctx) (recoveryModel, reuseWaitDesc string, error)` depuis `sys.databases`.
  - `ActiveLogFloorMB(ctx) (int, error)` : somme des VLF actifs (`sys.dm_db_log_info`,
    `vlf_active=1`) → plancher de troncature du log.
  - Réutiliser `SessionWaits(ctx, spid)` existant pour la détection self-wait (étape 6).
- Déclarer les interfaces étroites côté `run` (comme `sampleProbe`) pour ces lectures, afin
  de fournir des fakes en test unitaire.

**Tests** : `*_integration_test.go` (tag `integration`) contre la base e2e. Pas de test
unitaire pur ici (c'est l'adapter SQL).

**Checkpoint** : `make test` (les tests integration sont skippés) puis, si DB dispo,
`make integration`.

---

## Étape 6 — Driver de shrink (`internal/run/shrink.go`)

**But** : orchestrer estimation → truncate-only → boucle de chunks, avec réactions.

- `ShrinkRunner` (ou `ShrinkDriver`) construit avec des I/O injectées :
  exécution (`Executor` : `ExecDDL`/`SPID`/`Kill` — **hérite du KILL via le pool**, §8.5 du
  design), lectures (`FileSpace`/`FileSizeMB`/`SessionWaits`), sampler (`ServerSampler`),
  horloge (`Clock`), `ShrinkConfig`, `ReactionSink`.
- `Run(ctx, op ddl.Shrink, res ddl.ResolvedOptions, caps Capabilities, sink ReactionSink) error` :
  1. **Estimation/gating** (réutilise les lectures) :
     - data : calc `final` via `FinalTargetMB` ; no-op si `free≈0`/`final≥size` → succès « rien à faire ».
     - log : `LogReuse` → SIMPLE = `CHECKPOINT` autorisé puis shrink ; FULL/BULK_LOGGED avec
       `reuseWait≠NOTHING` → **attente bornée** (`log_reuse_wait_timeout`) qu'une sauvegarde
       de log planifiée libère le log : relire `reuseWait`+plancher VLF sur la cadence de poll,
       émettre un `pause` par cycle avec la raison, shrink dès `reuseWait=NOTHING`, abandon
       propre au timeout. **Jamais** de BACKUP LOG émis. Plancher = `ActiveLogFloorMB`.
  2. **Phase A — TRUNCATEONLY** : `ExecDDL(ShrinkTruncateOnlySQL)` ; relire taille ; si ≤ final → fin.
  3. **Phase B — boucle de chunks** (data) : `InitialStepMB` → boucle
     `NextTargetMB`/`ShrinkChunkSQL`, mesure `elapsed`+deltas, `AdjustStepMB`, relit la taille.
     - **Pause gratuite** sous pression : ne pas lancer le chunk suivant, attendre la détente
       (réutiliser `awaitRelief`/`waitForRelief`, `logDrainTimeout`).
     - **Self-wait** : `SessionWaits` montre `LCK_M_SCH_M`/snapshot prolongé → attendre jusqu'à
       `self_wait_timeout` puis arrêt propre.
     - **No-progress** : taille inchangée après un chunk (49516, données en fin) → backoff
       croissant + retry, arrêt propre au-delà de `max_no_progress`.
     - Un chunk à stopper passe par annulation de `context` (attention) puis `Kill` via le pool.
  4. Émettre `ReactionEvent` (`pause`/`resume`/`abort`) via `sink` et la **progression
     déterministe** `(start-current)/(start-final)`.
  - Le log est shrinké (Phase A/troncature) sans chunking : un (ou deux) `DBCC SHRINKFILE`.
- Garder la boucle structurée façon `runLoop` : la mécanique de décision en fonctions pures
  (étape 3) + I/O injectées, pour un test unitaire déterministe avec fakes (pas de DB).

**Tests** (`shrink_test.go`, sans DB) : no-op ; bascule truncate-only suffisante ; boucle qui
converge vers `final` ; ajustement du step sous pression simulée ; no-progress → arrêt après
`max_no_progress` ; log FULL `reuseWait=LOG_BACKUP` → attente puis shrink quand ça repasse à
`NOTHING`, et abandon propre si le timeout expire ; succès log SIMPLE après CHECKPOINT.

**Checkpoint** : `make test && make vet`.

---

## Étape 7 — Câblage engine + TUI

**But** : router les opérations shrink et reporter.

- `internal/run/engine.go` (`processOne`) : si `step.Operation` est un `ddl.Shrink`, appeler
  le `ShrinkRunner` au lieu de `r.runner.Run`. Construire `caps` (shrink = cancel-safe,
  reprenable). Alimenter `OperationReport` (initial/final size, espace gagné, nb de chunks,
  réactions, waits, durée). `notify` sur `pause`/`abort` comme l'existant.
- Progression : pour un shrink, alimenter le `Model` TUI avec la progression par chunks
  (et non `operationPercent`). Étendre le feed (`feedConsole`/messages TUI) si nécessaire,
  ou exposer la progression du driver via un canal/`sink`.
- Recovery (`Recoverer`) : vérifier qu'un shrink interrompu est simplement requeue/relançable
  (aucune logique resumable spécifique ; relancer vers la même cible reprend).

**Tests** : `engine_test.go` — un manifest shrink est routé vers le driver (fake driver),
le rapport contient les champs attendus ; cas data + log.

**Checkpoint** : `make test && make vet && make lint`.

---

## Étape 8 — e2e + documentation

**But** : valider contre une vraie instance et documenter la surface utilisateur.

- e2e (`make e2e`, SQL Server 2022) : créer une base jetable, gonfler un fichier (insert +
  delete massif pour créer de l'espace libre), lancer un manifest shrink data, vérifier la
  réduction et le rapport ; cas log SIMPLE (CHECKPOINT + shrink) et FULL refusé.
- `README.md` (référence canonique) : documenter `operation: shrink` (champs, exemples data
  et log), le bloc `shrink:` de `config.yaml`, et le comportement (TRUNCATEONLY auto, refus
  log FULL, progression par chunks).
- `docs/e2e.md` : noter les permissions/contexte requis si différents (le shrink exige des
  droits sur la base ; `VIEW SERVER STATE` déjà requis pour le monitoring).
- Calibrer empiriquement les défauts (§14 du design) et ajuster si besoin.

**Checkpoint** : `make e2e` vert ; relire le diff complet.

---

## Récap dépendances / ordre

```
0 matrix ─▶ 1 type ─▶ 2 resolve+generate ─▶ 3 cœur pur chunking ─▶ 6 driver ─▶ 7 engine ─▶ 8 e2e+doc
                                          ╲                        ╱
                                4 config ──┴── 5 mssql (DB) ──────┘
```

- Étapes 0–4 : 100 % cœur pur, testables sans DB — l'essentiel de la logique y est figé.
- Étape 5 : seul adapter SQL ; tests `integration`-tagués.
- Étapes 6–7 : assemblage, tests unitaires avec fakes (pas de DB).
- Étape 8 : DB réelle + doc utilisateur.

## Points de vigilance (déjà tranchés dans `SHRINK.md`, à ne pas réinventer)

- **KILL de sa propre session** : toujours via `Executor.Kill` (pool), jamais sur la
  connexion d'exécution (design §8.5).
- **Log FULL** : refus propre, **jamais** de `BACKUP LOG` automatique (design §5.2).
- **Pas de MAXDOP** sur `DBCC SHRINKFILE` ; **pas de `MAX_DURATION`** sur le WALP du shrink.
- **`targetfreespace`** = % de l'espace **utilisé** (`final = ceil(used × (1 + N/100))`).
- **`files: all`** étendu séquentiellement (jamais deux fichiers d'un filegroup en parallèle).
- Cœur décisionnel en **fonctions pures** (pattern `runLoop`/`supervise`) pour rester
  testable sans DB.
```
</content>
