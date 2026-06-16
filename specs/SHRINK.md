# SHRINK — réduction de fichiers (données & journal)

> Source de vérité du comportement visé pour la fonctionnalité de shrink de SqlGoPace.
> Les docs d'analyse brutes restent dans `SQL Server Shrink - Document de référence
> technique - Perplexity.md` et `gemini-shrink.md` ; ce document fixe le design arrêté.

## 1. Objectif et périmètre

Ajouter une opération `shrink` qui réduit la taille physique des fichiers d'une base
SQL Server, exécutée et surveillée par le moteur existant (queue `01.to_run` →
`02.processing` → `03.done`/`04.failed`, monitoring de blocage et de journal, hiérarchie
de réaction la moins destructive).

Le shrink n'est **pas** une opération de maintenance récurrente. L'outil l'exécute quand
on le lui demande explicitement (purge massive, suppression de tables, réduction de
périmètre) et fait le maximum pour qu'elle reste **non bloquante, mesurée, reprenable**.

### Périmètre v1 (cette spec)

- Opération `operation: shrink` (`type: data` | `type: log`) exécutable depuis `01.to_run`.
- **Estimation en preflight** : espace récupérable, détection des no-op, bascule
  automatique sur `TRUNCATEONLY` quand l'espace libre est en fin de fichier.
- Shrink de données **par chunks** avec stepsize calibré et ajusté dynamiquement.
- Shrink de journal **en troncature** (pas de chunking), avec détection du modèle de
  récupération et refus propre quand la troncature est impossible.
- `files: all` étendu en une opération par fichier, séquentiellement.
- Monitoring + réactions intégrés (pause entre chunks, self-wait, no-progress, blocage,
  journal au-dessus du seuil).

### Hors périmètre v1 (→ Phase 2, §12)

- Rapport de fragmentation avant/après et génération automatique de manifests de
  défragmentation.
- Mode `EMPTYFILE` + `ALTER DATABASE … REMOVE FILE`.
- Détection des fichiers shrinkables côté sous-commande `plan` (planner).
- Pré-allocation/redimensionnement du journal en un bloc après shrink (contrôle des VLF).

## 2. Modèle d'opération (YAML)

```yaml
- operation: shrink
  type: data            # "data" | "log"
  files: all            # "all" (tous les fichiers du type) | nom logique d'un fichier
  emptyfile: false      # réservé Phase 2 ; doit être false (ou absent) en v1
  targetfreespace: 10%  # espace libre VISÉ dans le fichier final : "10%" ou "100MB"
  options:
    wait_at_low_priority: true   # 2022+ uniquement (gaté par le matrix) ; auto si absent
```

Champs :

- **`type`** (obligatoire) : `data` ou `log`. Détermine quels fichiers sont éligibles et
  quel algorithme s'applique (chunking pour `data`, troncature pour `log`).
- **`files`** (défaut `all`) : `all` étend en une opération par fichier du type donné
  (voir §6). Sinon, nom logique d'un fichier (`sys.database_files.name`).
- **`targetfreespace`** : objectif d'espace libre, exprimé **en % de l'espace utilisé**.
  `N%` ⇒ `final_mb = ceil(used_mb × (1 + N/100))`. `N MB` ⇒ `final_mb = used_mb + N`.
  Toujours borné par le plancher `SpaceUsed` (§5.1).
- **`emptyfile`** : réservé Phase 2. En v1, une valeur `true` est rejetée à la validation.
- **`options.wait_at_low_priority`** : `*bool` « auto » (comme les autres options).
  `nil` ⇒ décidé par le matrix (ON si 2022+). `ABORT_AFTER_WAIT` reste `SELF` sauf si
  `Policy.AllowAbortBlockers` est activé globalement (réutilise le champ existant).

**Pas de `maxdop`** : `DBCC SHRINKFILE` n'accepte pas d'option MAXDOP. Toute clé `maxdop`
sous un shrink est ignorée (ou rejetée à la validation).

`Validate` : `type` ∈ {data, log} ; `targetfreespace` parsable (`%` ou `MB`) et > 0 ;
`emptyfile` non `true` en v1.

## 3. Grammaire T-SQL générée

Grammaire officielle ciblée :

```
DBCC SHRINKFILE
(
    { file_name | file_id }
    { [ , EMPTYFILE ]
    | [ [ , target_size ] [ , { NOTRUNCATE | TRUNCATEONLY } ] ]
    }
)
[ WITH
  {
      [ WAIT_AT_LOW_PRIORITY [ ( ABORT_AFTER_WAIT = { SELF | BLOCKERS } ) ] ]
      [ , NO_INFOMSGS ]
  }
]
```

Conséquences pour le générateur (générateur **dédié**, pas le `withClause` des index) :

- `target_size` est un **entier en Mo**.
- `WAIT_AT_LOW_PRIORITY` du shrink **n'a pas de `MAX_DURATION`** (timeout fixe ~1 min) et
  **n'est pas** imbriqué dans `ONLINE = ON`. Il n'existe pas d'`ONLINE` pour DBCC.
- `TRUNCATEONLY`, `NOTRUNCATE` et `EMPTYFILE` sont mutuellement exclusifs avec un chunk de
  déplacement ; avec `TRUNCATEONLY`, `target_size` est ignoré par le moteur.
- On ajoute `NO_INFOMSGS` pour réduire le bruit.

Statements types générés :

```sql
-- chunk de déplacement (un par itération) ; WAIT_AT_LOW_PRIORITY seulement si résolu ON
DBCC SHRINKFILE (N'MyDb_Data', 8192) WITH WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF), NO_INFOMSGS;

-- truncate-only (phase A, §7) ; pas de déplacement, pas de fragmentation
DBCC SHRINKFILE (N'MyDb_Data', TRUNCATEONLY) WITH NO_INFOMSGS;

-- journal
DBCC SHRINKFILE (N'MyDb_Log', 512) WITH NO_INFOMSGS;
```

## 4. Détection de version / édition (matrix)

Entrées matrix dédiées, clées par command type `shrink_data` / `shrink_log` × version × tier :

- `wait_at_low_priority` : éligible **SQL Server 2022 (16.x) et +** uniquement, pour
  `shrink_data`. (Non pertinent pour `shrink_log` : pas de déplacement de pages, donc pas
  de verrou Sch-M sur IAM à attendre.)

`Resolve` produit les `Decision` habituelles pour `--explain`, **sans** appliquer la règle
« WALP requires ONLINE » (inexistante pour DBCC). `ABORT_AFTER_WAIT = BLOCKERS` n'est émis
que si `Policy.AllowAbortBlockers` est vrai ; sinon `SELF`.

## 5. Preflight

### 5.1 Données

Lire `sys.database_files` + `FILEPROPERTY(name,'SpaceUsed')` (rafraîchir au besoin) :

```sql
SELECT name, type_desc, file_id,
       size/128.0                                              AS size_mb,
       CAST(FILEPROPERTY(name,'SpaceUsed') AS INT)/128.0       AS used_mb,
       (size - CAST(FILEPROPERTY(name,'SpaceUsed') AS INT))/128.0 AS free_mb
FROM sys.database_files
WHERE type_desc = 'ROWS';
```

- **Plancher** : le fichier ne peut pas descendre sous `used_mb`. `final_mb` calculé depuis
  `targetfreespace` (§2) est clampé à `max(final_mb, ceil(used_mb))`.
- **No-op** : si `free_mb ≈ 0` ou `final_mb ≥ size_mb`, l'opération est inutile → skip
  explicite (succès « rien à faire », consigné dans le rapport).
- **Bascule TRUNCATEONLY** : on tente toujours d'abord une phase truncate-only (§7), gratuite
  et instantanée, avant tout déplacement.

### 5.2 Journal

```sql
SELECT recovery_model_desc, log_reuse_wait_desc
FROM sys.databases WHERE name = DB_NAME();
```

Plancher récupérable = dernier VLF actif (`sys.dm_db_log_info`, `vlf_active = 1`) : on ne
peut pas tronquer au-delà.

Décision selon le modèle de récupération (**responsabilité limitée, décision arrêtée**) :

- **SIMPLE** : un `CHECKPOINT` (inoffensif) est autorisé pour libérer les VLF, puis shrink.
- **FULL / BULK_LOGGED** :
  - `log_reuse_wait_desc = NOTHING` ⇒ le log est déjà tronqué (sauvegarde récente) → shrink.
  - sinon (`LOG_BACKUP`, `ACTIVE_TRANSACTION`, …) ⇒ **attente bornée**, **pas** de refus
    immédiat. SqlGoPace **n'émet jamais** de `BACKUP LOG` (il ne touche pas la chaîne de
    sauvegarde), mais il **laisse passer** la sauvegarde de log planifiée de l'environnement :
    boucle d'attente (réutilise le pattern `awaitRelief`/`waitForRelief`) qui relit
    `log_reuse_wait_desc` et le plancher VLF sur la cadence de poll du log, en émettant un
    événement `pause` avec la raison à chaque cycle.
    - dès que `reuse_wait` repasse à `NOTHING` (sauvegarde passée, VLF de fin devenus
      inactifs) → shrink.
    - au-delà de `log_reuse_wait_timeout` → **abandon propre** avec la dernière raison observée.
  - Note : `LOG_BACKUP` / `ACTIVE_TRANSACTION` sont transitoires (résolus par le job de
    sauvegarde ou la fin d'une transaction). Des raisons structurelles (`REPLICATION`,
    `AVAILABILITY_REPLICA`, `DATABASE_MIRRORING`) peuvent ne jamais se résoudre : la même
    attente bornée s'applique, la raison rapportée à chaque cycle permet à l'opérateur
    d'interrompre, et le timeout garantit qu'on ne pend pas indéfiniment.

## 6. Expansion `files: all`

Comme `index: ALL` (voir `expand.go`) : résolu contre `sys.database_files` (filtré par
`type` : `ROWS` pour data, `LOG` pour log) en **une opération shrink par fichier**, exécutées
**séquentiellement**. Ne jamais shrinker deux fichiers du même filegroup en parallèle
(contention sur les tables système) — garanti gratuitement par l'exécution séquentielle du
moteur.

## 7. Driver de chunking (shrink de données)

**Driver dédié** dans `internal/run` (à côté de `MonitoredRunner`, pas une généralisation),
réutilisant `Executor` (`SPID`/`ExecDDL`/`Kill`), `ServerSampler`, `supervise`/`Pressure`,
et le pattern de waits en **delta** (`snapshotWaits`/`operationWaits`).

### 7.1 Algorithme

```
finalTarget := preflight.FinalTargetMB           // §5.1, clampé au plancher
startSize   := preflight.SizeMB

// Phase A — truncate-only (gratuit, sans fragmentation)
exec: DBCC SHRINKFILE(file, TRUNCATEONLY)
relire size ; si size <= finalTarget → terminé

// Phase B — chunks de déplacement
step := initialStep(startSize - finalTarget)      // heuristique §7.2
current := size
noProgress := 0
for current > finalTarget {
    next := max(current - step, finalTarget)
    t0 := clk.Now()
    action := runChunk(file, next, walp)          // §8 : Continue | pause | abort
    if action == abort { return reprenable }      // travail conservé
    elapsed := clk.Since(t0)

    newSize := readFileSizeMB(file)
    dWaits  := deltaWaits()                        // WRITELOG, PAGEIOLATCH_EX
    dLog    := deltaLogSpace()

    if newSize >= current {                        // aucun gain (cf. §8.3)
        noProgress++
        if noProgress >= maxNoProgress { return reprenable } // ou attente + retry
        attendre backoff ; continue
    }
    noProgress = 0
    step = adjustStep(step, elapsed, dWaits, dLog) // §7.2
    current = newSize
    logChunk(current, next, elapsed, step, dWaits, dLog)
}
```

### 7.2 Calibration du stepsize

Step initial par volume à récupérer (Perplexity §9.2), borne basse de chaque tranche comme
défaut prudent (l'ajustement dynamique fera monter si l'I/O suit) :

| Volume à récupérer | step initial (défaut) |
|--------------------|-----------------------|
| < 5 Go             | 100 Mo                |
| 5–50 Go            | 250 Mo                |
| > 50 Go            | 500 Mo                |

Ajustement entre chunks, sur les **deltas** (jamais les valeurs cumulées de
`sys.dm_os_wait_stats`) :

- **Réduire** (`step/2`, borné par `minStep`) si : `WRITELOG` avg > 10 ms, ou
  `PAGEIOLATCH_EX` avg > 20 ms, ou blocage d'autres sessions > 30 s.
- **Augmenter** (`step*2`, borné par `maxStep`) si : latences I/O < 5 ms, pas de wait
  significatif, `log_space_since_backup` faible, et `elapsed < targetBatchDuration`.

### 7.3 Défauts proposés (bloc `shrink:` de `config.yaml`)

```yaml
shrink:
  initial_step_small_mb:  100   # volume à récupérer < 5 Go
  initial_step_medium_mb: 250   # 5–50 Go
  initial_step_large_mb:  500   # > 50 Go
  min_step_mb:             50    # en deçà, l'overhead par boucle domine le gain
  max_step_mb:           1024    # plafond pour ne pas saturer l'I/O d'un coup
  target_batch_seconds:     5    # un chunk « idéal » dure quelques s → réactions vives
  max_no_progress:          3    # chunks sans gain consécutifs avant arrêt propre
  no_progress_backoff_seconds:      30   # attente avant retry, doublée à chaque no-progress
  no_progress_backoff_max_seconds: 300   # plafond du backoff (5 min)
  self_wait_timeout_minutes: 5   # attente max sur Sch-M / snapshot avant arrêt propre (§8.2)
  log_reuse_wait_timeout_minutes: 30  # attente max qu'un BACKUP LOG planifié libère le log (§5.2)
```

`log_reuse_wait_timeout_minutes` par défaut à 30 min : marge pour un ou deux cycles d'une
cadence de sauvegarde de log courante (~15 min). L'attente est gratuite (le shrink n'a pas
démarré) et émet un `pause` par cycle, donc interruptible.

**Configurabilité** : bloc **global**, **tous les champs optionnels** (absent ⇒ défaut
appliqué, comme `MonitoringConfig`) — un `config.yaml` sans bloc `shrink:` fonctionne. Niveau
**global uniquement, jamais par-manifest** : ces valeurs dépendent du stockage et du SLA de
l'instance, pas de l'opération ; le manifest ne porte que ses `options:` métier. Elles sont
des **points de départ et des bornes** que l'ajustement dynamique (§7.2) fait varier ;
l'esprit reste « auto-calibré », un opérateur n'y touche que pour un stockage atypique.

Seuils de tranches (`< 5 Go`, `5–50 Go`) en constantes documentées (non exposés). Les
durées de réaction transverses (`blocking_timeout_minutes`, `log_drain_timeout_minutes`,
`kill_grace_seconds`) restent celles de `MonitoringConfig`, réutilisées par le driver.

## 8. Réactions et pression

Le shrink est l'opération la **plus** sûre à interrompre : chaque batch interne (~32 pages)
est une transaction propre ; arrêter le shrink **conserve le travail déjà fait** et il est
ré-entrant (relancer vers la même cible reprend). La réaction la moins destructive n'est
donc même pas un cancel.

### 8.1 Pause « gratuite » entre chunks

Sous pression (blocage d'autres sessions au-delà du timeout, ou journal au-dessus du seuil),
le driver **ne lance pas le chunk suivant** et attend la détente (réutilise la logique de
`awaitRelief`/`waitForRelief` et le `logDrainTimeout`). Aucun rollback : le travail des
chunks précédents est déjà commité. C'est plus doux que le pause/resume d'un rebuild.

Événements `ReactionSink` réutilisés : `pause` (attente), `resume` (« pressure cleared »),
`abort` (arrêt propre, travail conservé). Un chunk en cours qui doit être stoppé passe par
le même chemin que `runStatement` : annulation douce via le `context` de la connexion
d'exécution, puis **`KILL` en fallback émis depuis le pool de monitoring** (voir §8.5).

### 8.2 Self-wait (nouvelle dimension de pression)

Le shrink peut être **bloqué par** d'autres sessions (transactions snapshot RCSI/SI →
messages 5202/5203 ; attente `LCK_M_SCH_M`). Le modèle actuel ne couvre que
`Pressure.BlockingOthers`. Ajouter la détection « on attend » via les waits de **notre**
session (`sys.dm_exec_requests` / `SessionWaits` sur notre SPID) :

- `LCK_M_SCH_M` (ou `LCK_M_SCH_M_LOW_PRIORITY`) prolongé, ou blocage par snapshot :
  privilégier **l'attente** (le shrink reprendra), puis **arrêt propre** si l'attente
  dépasse un seuil (le travail est conservé).

### 8.3 No-progress / timeout WALP 49516 silencieux

En `WAIT_AT_LOW_PRIORITY`, si le verrou Sch-M n'est pas obtenu en ~1 min, le shrink
**se termine sans erreur visible et sans avoir rien fait** (erreur 49516 seulement dans le
log SQL Server). On ne peut donc pas se fier au code retour. Détection par **comparaison de
taille** : si `newSize >= current` après un chunk (49516, ou données en fin de fichier qui
ne peuvent pas être déplacées), incrémenter un compteur no-progress → backoff + retry, puis
arrêt propre au-delà de `maxNoProgress`.

### 8.4 Journal pendant un shrink de données

Un shrink de données **génère lui-même du log** (chaque page déplacée est loguée). Le
sampler `Log` existant (seuil `LogOverCap`) s'applique tel quel : au-dessus du seuil, pause
entre chunks et réduction du stepsize au chunk suivant.

### 8.5 KILL de sa propre session — détail d'implémentation critique

On ne peut pas `KILL` sa propre session depuis la connexion qui exécute le DDL. `Conn` garde
**deux** connexions : `exec` (épinglée, `@@SPID` stable) et `pool` (monitoring). `Conn.Kill`
émet `KILL <spid>` **sur le pool**, jamais sur `exec`. Le driver shrink **doit** passer par
l'interface `Executor` (`ExecDDL` sur `exec`, `Kill` via le pool) pour hériter de cette
garantie ; ne pas ouvrir de chemin d'exécution parallèle qui contournerait ce partage.

## 9. Progression, reporting, TUI

- **Progression déterministe** (avantage du chunking, contrairement au `percent_complete`
  fluctuant de `dm_exec_requests`) : `(startSize - currentSize) / (startSize - finalTarget)`.
  À alimenter dans le `Model` du TUI à la place de `operationPercent` pour le shrink.
- **Log par chunk** : cible visée, taille obtenue, durée, stepsize, deltas de waits / log.
- **Rapport `.log` + history** : `initial_size`, `final_size`, espace gagné, durée totale,
  nombre de chunks, réactions, et la version `--version` ayant produit le run (comme le reste).

## 10. Recovery / ré-entrance

Un shrink interrompu (crash moteur, perte de connexion) est trivialement reprenable :
relancer `DBCC SHRINKFILE` vers la même cible reprend là où on en était (le travail commité
persiste). Le `Recoverer` database-aware existant s'applique : un orphelin dont la base est
injoignable (ex. devenue secondaire AG) est laissé pour un run ultérieur. Aucune sous-commande
`abort-shrink` n'est nécessaire (l'opération est déjà sûre à arrêter).

## 11. Erreurs et messages à connaître

| Code / signal | Type | Signification | Action SqlGoPace |
|---------------|------|---------------|------------------|
| 5202 / 5203 | Informatif (log SQL) | Shrink bloqué par une transaction snapshot | Self-wait (§8.2) : attendre, puis arrêt propre |
| 49516 | Erreur Level 16 (log SQL) | Timeout WALP, Sch-M non obtenu | No-progress (§8.3) : backoff + retry |
| 9002 | Erreur | Transaction log full | Pause + réduire le stepsize (§8.4) |
| Pas de réduction | Normal | Espace libre absent ou pas en fin de fichier | TRUNCATEONLY déjà tenté ; no-progress → arrêt propre |
| Log non shrinkable | Normal | VLF actifs en fin, ou `log_reuse_wait ≠ NOTHING` | Attente bornée que le log se libère (§5.2), jamais de BACKUP LOG ; abandon propre au timeout |

## 12. Phase 2 (hors v1)

- **Fragmentation avant/après** : `sys.dm_db_index_physical_stats` ; comparer et **générer
  des manifests `rebuild_index`/`reorganize_index` dans `01.to_run`** (réutilisation du
  pipeline et du planner) ; option d'automatisation.
- **`EMPTYFILE`** : migration du contenu vers les autres fichiers du filegroup, puis
  `ALTER DATABASE … REMOVE FILE`.
- **Détection côté `plan`** : repérage des fichiers shrinkables comme la maintenance.
- **Pré-allocation du journal** après shrink (un seul `ALTER DATABASE … MODIFY FILE`) pour
  contrôler le nombre de VLF.

## 13. Requêtes de référence

### Espace libre des fichiers de données
```sql
SELECT name, type_desc,
       size/128.0                                              AS size_mb,
       CAST(FILEPROPERTY(name,'SpaceUsed') AS INT)/128.0       AS used_mb,
       (size - CAST(FILEPROPERTY(name,'SpaceUsed') AS INT))/128.0 AS free_mb
FROM sys.database_files;
```

### Portion active du journal (plancher récupérable)
```sql
SELECT file_id, vlf_begin_offset, vlf_size_mb, vlf_sequence_number, vlf_active, vlf_status
FROM sys.dm_db_log_info(DB_ID())
WHERE vlf_active = 1;

SELECT total_log_size_in_bytes/1048576.0 AS total_log_mb,
       used_log_space_in_bytes/1048576.0 AS active_log_mb,
       used_log_space_in_percent         AS active_pct
FROM sys.dm_db_log_space_usage;
```

### Raison de non-troncature du journal
```sql
SELECT name, recovery_model_desc, log_reuse_wait_desc
FROM sys.databases WHERE name = DB_NAME();
```

### Progression native (diagnostic ; on préfère la progression par chunks, §9)
```sql
SELECT session_id, command, percent_complete,
       estimated_completion_time/1000/60 AS est_min_left,
       wait_type, blocking_session_id
FROM sys.dm_exec_requests
WHERE command IN ('DbccFilesCompact', 'DbccSpaceReclaim');
```

### Waits critiques (à lire en delta)
```sql
SELECT wait_type, wait_time_ms, waiting_tasks_count
FROM sys.dm_os_wait_stats
WHERE wait_type IN ('PAGEIOLATCH_EX','PAGEIOLATCH_SH','WRITELOG',
                    'LCK_M_SCH_M','LCK_M_SCH_M_LOW_PRIORITY');
```

## 14. Points encore ouverts

- Aucun point bloquant. Conventions arrêtées : `targetfreespace` en % de l'espace **utilisé**
  (`final = ceil(used × (1 + N/100))`, §2) ; défauts du driver figés (§7.3). Ces défauts
  restent à valider empiriquement lors des tests e2e (calibration I/O réelle).
</content>
</invoke>
