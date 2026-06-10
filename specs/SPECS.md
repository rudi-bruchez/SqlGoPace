# SqlGoPace — Orchestrateur DDL pour SQL Server

Application Go : un *task runner* qui exécute des opérations DDL exigeantes sur SQL Server
(`ALTER INDEX`, `CREATE INDEX`, `ALTER COLUMN`, ajout de colonne, contraintes…) en surveillant
en continu leur impact, et en réagissant intelligemment aux blocages et à la pression sur le
journal de transactions.

Architecture : un orchestrateur Go avec un **thread d'exécution** (connexion dédiée) et un
**thread de monitoring** (connexion distincte).

## Contexte — pourquoi un outil dédié

Ces opérations sont risquées en production :

- elles **bloquent** d'autres sessions ou **sont bloquées** : attentes `LCK_M_SCH_S`, `LCK_M_SCH_M`, `LCK_M_IX` ;
- elles peuvent **remplir le journal de transactions** ;
- un `KILL` déclenche un `ROLLBACK` parfois long et coûteux ;
- le bon jeu d'options (`ONLINE`, `RESUMABLE`, `WAIT_AT_LOW_PRIORITY`, `MAXDOP`…) dépend de la
  **version ET de l'édition** du serveur cible.

L'outil automatise la décision et la surveillance, et privilégie toujours les mécanismes les moins
destructifs (pause/reprise plutôt que kill/rollback).

## Modes d'exécution

- **Silencieux + log** (défaut) : exécution non interactive, tout est tracé dans les fichiers `.log`.
- **TUI** : flag `--tui` — console d'incident interactive (voir §14).
- **Dry-run** : flag `--dry-run` — affiche la commande DDL finale (options injectées comprises)
  **sans rien exécuter** et sans poser le moindre verrou.
- **Explain** : flag `--explain` — pour chaque opération, montre *pourquoi* chaque option a été
  ajoutée ou retirée (version/édition détectées + entrée de matrice + override de config).

---

## 1. Interface déclarative (DDL en YAML — pas de SQL brut)

**Décision d'architecture : l'outil n'accepte PAS de fichiers `.sql`.** Accepter du T-SQL arbitraire
imposerait de parser et de réécrire des clauses `WITH (...)` de façon fragile, et d'exécuter du code
non maîtrisé. C'est trop dangereux pour cet usage.

À la place, chaque tâche est décrite par un **manifeste YAML** d'opérations. L'outil **génère
lui-même** le T-SQL à partir de la description structurée : il connaît le type d'opération avec
certitude (zéro ambiguïté de parsing), construit la clause d'options sans risque de doublon, fait un
pré-vol précis sur l'objet ciblé, et gère l'idempotence et la reprise.

### 1.1 Schéma d'un manifeste

Un fichier = une tâche logique = une liste ordonnée d'opérations exécutées **séquentiellement**
(équivalent structuré des anciens « batchs `GO` »).

```yaml
# 01.to_run/010_rebuild_dispatch.yaml
description: "Recompression et ajout de colonne sur DISPATCH"
database: EXAMPLEDB          # optionnel : sinon la base de la chaîne de connexion
operations:
  - operation: rebuild_index
    schema: dbo
    table: DISPATCH
    index: IX_DISPATCH        # ou "ALL" pour reconstruire tous les index
    data_compression: PAGE
    # online / resumable / wait_at_low_priority / maxdop / sort_in_tempdb :
    # laissés vides → injectés automatiquement selon version/édition + matrice + config

  - operation: add_column
    schema: dbo
    table: DISPATCH
    column: PROCESSED
    type: BIT
    nullable: false
    default: 0               # constante → metadata-only en Enterprise (voir §1.4)
    options:
      maxdop: 4              # override explicite d'une option pour CETTE opération
```

### 1.2 Types d'opérations supportés

Chaque `operation` correspond directement à une clé de la matrice de compatibilité (§8). L'ajout d'un
nouveau type d'opération se fait dans le code **et** dans `ddl_compatibility.yaml`.

| `operation`        | T-SQL généré                          |
|--------------------|---------------------------------------|
| `rebuild_index`    | `ALTER INDEX … REBUILD WITH (…)`      |
| `create_index`     | `CREATE [UNIQUE] INDEX … WITH (…)`    |
| `drop_index`       | `DROP INDEX …`                        |
| `add_column`       | `ALTER TABLE … ADD …`                 |
| `alter_column`     | `ALTER TABLE … ALTER COLUMN … WITH (…)` |
| `drop_column`      | `ALTER TABLE … DROP COLUMN …`         |
| `add_constraint`   | `ALTER TABLE … ADD CONSTRAINT … WITH (…)` |
| `drop_constraint`  | `ALTER TABLE … DROP CONSTRAINT …`     |

Toute construction non modélisée est **hors périmètre** (pas d'échappatoire SQL brut).

### 1.3 Injection et override des options

Pour chaque option injectable (`online`, `resumable`, `wait_at_low_priority`, `maxdop`,
`sort_in_tempdb`, `data_compression`), la valeur effective est résolue dans cet ordre :

1. **Override par opération** (`operations[].options.<opt>`) — priorité maximale.
2. **Override global** (`config.yaml > options_override.<opt>.force`).
3. **Auto** (défaut) : injectée si et seulement si la matrice (§8) l'autorise pour la
   `version × édition × operation` cible.

Dépendances appliquées automatiquement :

- `resumable: true` ⇒ force `online: true` (RESUMABLE impose ONLINE pour un index).
- `wait_at_low_priority` n'est injecté que si `online: true`.
- une option non supportée par la cible est **silencieusement omise** (et tracée dans `--explain`).

### 1.4 Opérations metadata-only

Certaines opérations sont des changements de métadonnées seulement (ex. `add_column NOT NULL` avec
**default constant** en édition Enterprise, agrandissement `varchar(n)→varchar(m)` avec `m>n`…).
L'outil **classe** ces cas pour les signaler dans le pré-vol et l'`--explain` (« attendue
instantanée, metadata-only »), **mais ne désactive jamais le monitoring** : la détection fiable
dépend de l'état réel de la table (compression, colonnes sparse, type LOB, édition) et n'est pas
garantie au seul examen du manifeste. Le monitoring est peu coûteux ; un faux « instantané » ne l'est
pas.

### 1.5 Idempotence

L'outil enveloppe automatiquement la commande générée d'une garde d'existence quand c'est pertinent
(`IF NOT EXISTS (…)` pour `add_column`/`create_index`/`add_constraint`, `IF EXISTS` pour les `drop`).
Une reprise après échec partiel ne ré-applique donc pas une opération déjà effectuée.

---

## 2. Structure des dossiers

```
├── 01.to_run/        # manifestes *.yaml en attente
├── 02.processing/    # déplacé ici pendant l'exécution (évite les conflits + marque l'orphelin)
├── 03.done/          # terminé avec succès, + <nom>.log
└── 04.failed/        # erreur / abandon, + <nom>.log
```

Traitement **strictement séquentiel**, un manifeste à la fois, dans l'**ordre de tri du nom de
fichier** (convention `010_`, `020_`…). Jamais deux DDL lourds en parallèle sur la même base : ils se
bloqueraient et multiplieraient le journal.

Pendant l'exécution, le manifeste vit dans `02.processing/` accompagné d'un **sidecar d'état**
`<nom>.state.json` (voir §13) utilisé pour la récupération après crash.

---

## 3. Architecture des connexions (le piège du pool)

Le driver `database/sql` de Go utilise par défaut un pool de connexions dynamique — risque majeur ici.

- **Thread d'exécution** : ouvre une **connexion exclusive et dédiée** via `db.Conn(ctx)`. C'est la
  seule garantie que le `SELECT @@SPID` récupéré au départ corresponde exactement à la session qui
  exécute le DDL.
- **Thread de monitoring** : utilise une **connexion distincte** (voire un pool séparé) pour ne
  jamais être bloqué par le DDL surveillé.

Sur la connexion d'exécution, au démarrage de session :

```sql
SET XACT_ABORT ON;
SET DEADLOCK_PRIORITY LOW;   -- le DDL devient victime désignée, pas la requête utilisateur
```

Ne **pas** poser de `LOCK_TIMEOUT` : l'attente de verrou est gérée proprement par
`WAIT_AT_LOW_PRIORITY` (§11).

Driver recommandé : **`github.com/microsoft/go-mssqldb`**.

---

## 4. Pré-vol (preflight)

Avant de poser le moindre verrou, l'outil exécute pour chaque manifeste une batterie de vérifications.
**Tout échec de pré-vol envoie directement le manifeste en `04.failed/`** sans avoir touché aux données.

Le pré-vol vérifie l'état de santé **général** (on veut démarrer dans une situation saine) :

1. **Version & édition** du serveur cible (§7).
2. **Validité de la cible** : la base, le schéma, la table, l'index/colonne/contrainte existent (ou
   n'existent pas, pour les `create`).
3. **Recovery model** : `SELECT recovery_model_desc FROM sys.databases`.
4. **Journal** : taille actuelle, % utilisé, `max_size` du/des fichiers log, `log_reuse_wait_desc` —
   doit être sain au départ (pas de `LOG_BACKUP`/`ACTIVE_TRANSACTION` bloquant déjà).
5. **Blocages préexistants** : aucune chaîne de blocage anormale en cours.
6. **Espace data** : un rebuild/create ONLINE construit une **copie** de l'objet → il faut
   ≈ la taille de l'objet en espace libre dans le filegroup cible.
7. **tempdb** : si `SORT_IN_TEMPDB` sera injecté, vérifier l'espace tempdb.
8. **Availability Group** : état des réplicas (`sys.dm_hadr_database_replica_states`) — un gros DDL
   peut saturer la *send queue* et bloquer la troncature du log côté primaire si le secondaire ne
   suit pas. **On signale (live TUI + log) mais on continue** (configurable).
9. **ADR** : `is_accelerated_database_recovery_on` (influence la stratégie de §11).

L'estimation de taille d'objet utilise `sys.allocation_units` (peu coûteux). Éviter
`sys.dm_db_index_physical_stats` en mode `DETAILED` (coûteux et générateur d'IO) ; au besoin
`LIMITED`/`SAMPLED`.

---

## 5. Détection version / édition

```sql
SELECT
    SERVERPROPERTY('EngineEdition')        AS EngineEdition,
    SERVERPROPERTY('ProductMajorVersion')  AS ProductMajorVersion;
```

### EngineEdition → tier d'édition

| EngineEdition | Produit                                            | Tier matrice |
|---------------|----------------------------------------------------|--------------|
| 2             | Standard / Web / BI / Standard Developer           | `standard`   |
| 3             | Enterprise / Developer / Evaluation                | `enterprise` |
| 4             | Express                                            | `express`    |
| 5             | Azure SQL Database                                 | `azure` *    |
| 8             | Azure SQL Managed Instance                         | `azure` *    |
| 6 / 9 / 11 / 12 | Synapse / SQL Edge / Fabric                      | non supporté |

\* Les éditions Azure (5, 8) sont **evergreen** (≈ Enterprise, `ONLINE`/`RESUMABLE` toujours
disponibles) : elles ne s'indexent pas par année. L'outil leur attribue une **pseudo-version
majeure très élevée** (`9999`) afin que tous les `min_major` soient satisfaits, et le tier `azure`.

### ProductMajorVersion → année

| Major | Année | Major | Année |
|-------|-------|-------|-------|
| 13    | 2016  | 16    | 2022  |
| 14    | 2017  | 17    | 2025  |
| 15    | 2019  |       |       |

Cette table de correspondance est embarquée dans `ddl_compatibility.yaml` (clé `major_to_year`) pour
l'affichage, mais la **résolution de capacité se fait directement sur le numéro majeur**.

---

## 6. Logique d'injection des options (résolution déterministe)

Pour chaque opération du manifeste :

1. Déterminer `command_type` = le champ `operation` (ex. `rebuild_index`).
2. Charger l'entrée de matrice pour ce `command_type`.
3. Pour chaque option injectable : elle est **applicable** si
   `target_major >= min_major` **ET** `target_tier ∈ editions`.
4. Appliquer la résolution override/auto de §1.3 et les dépendances (RESUMABLE⇒ONLINE, etc.).
5. Construire la chaîne T-SQL finale, fusionnée dans **une seule** clause `WITH (...)`.

Aucune règle métier SQL Server n'est codée en dur : tout vit dans `ddl_compatibility.yaml`. Une
nouvelle version (2027…) = une ligne de YAML, pas de recompilation.

---

## 7. Matrice de compatibilité — `ddl_compatibility.yaml`

Structure **par `min_version` + édition** (et non plus une ligne par année). Pour chaque
`command_type`, chaque option déclare la **version majeure minimale**, les **éditions** autorisées,
et d'éventuelles **dépendances** (`requires`).

```yaml
# ddl_compatibility.yaml
major_to_year:  { 13: 2016, 14: 2017, 15: 2019, 16: 2022, 17: 2025 }
azure_pseudo_major: 9999          # EngineEdition 5 / 8

commands:

  rebuild_index:                  # ALTER INDEX ... REBUILD
    online:               { min_major: 9,  editions: [enterprise, azure] }
    wait_at_low_priority: { min_major: 12, editions: [enterprise, azure], requires: [online] }   # 2014
    resumable:            { min_major: 14, editions: [enterprise, azure], requires: [online] }   # 2017
    sort_in_tempdb:       { min_major: 9,  editions: [enterprise, standard, azure] }
    data_compression:     { min_major: 10, editions: [enterprise, azure] }
    maxdop:               { min_major: 9,  editions: [enterprise, standard, azure] }

  create_index:                   # CREATE [UNIQUE] INDEX
    online:               { min_major: 9,  editions: [enterprise, azure] }
    resumable:            { min_major: 15, editions: [enterprise, azure], requires: [online] }   # 2019
    wait_at_low_priority: { min_major: 16, editions: [enterprise, azure], requires: [online] }   # 2022
    sort_in_tempdb:       { min_major: 9,  editions: [enterprise, standard, azure] }
    data_compression:     { min_major: 10, editions: [enterprise, azure] }
    maxdop:               { min_major: 9,  editions: [enterprise, standard, azure] }

  alter_column:                   # ALTER TABLE ALTER COLUMN
    online:               { min_major: 13, editions: [enterprise, azure] }                       # 2016
    # NB : WAIT_AT_LOW_PRIORITY n'est PAS supporté avec ONLINE ALTER COLUMN, quelle que soit la version.

  add_column:                     # ALTER TABLE ADD
    # pas d'ONLINE ; rapidité = metadata-only conditionnel (cf. §1.4), non injectable

  add_constraint:                 # ALTER TABLE ADD CONSTRAINT (PK / UNIQUE)
    online:               { min_major: 13, editions: [enterprise, azure] }
    resumable:            { min_major: 16, editions: [enterprise, azure], requires: [online] }   # 2022

  drop_index:        {}
  drop_column:       {}
  drop_constraint:   {}
```

Types Go associés :

```go
type OptionRule struct {
    MinMajor int      `yaml:"min_major"`
    Editions []string `yaml:"editions"`
    Requires []string `yaml:"requires"`
}
type CommandRules map[string]OptionRule          // option -> règle
type Matrix struct {
    MajorToYear      map[int]int             `yaml:"major_to_year"`
    AzurePseudoMajor int                     `yaml:"azure_pseudo_major"`
    Commands         map[string]CommandRules `yaml:"commands"`
}
```

### Subtilités encodées dans la matrice (rappel factuel)

- **ONLINE** : `CREATE INDEX` / `ALTER INDEX REBUILD` depuis 2005 (Enterprise). `ALTER COLUMN ONLINE`
  depuis **SQL Server 2016** (et non 2022). Online index = **Enterprise/Developer/Azure uniquement**.
- **RESUMABLE** : `ALTER INDEX REBUILD` (2017) → `CREATE INDEX` (2019) → `ADD CONSTRAINT` PK/UNIQUE
  (2022). Impose `ONLINE = ON`.
- **WAIT_AT_LOW_PRIORITY** : *partition switch* + `REBUILD` (2014) → étendu à `CREATE INDEX` ONLINE
  (2022). **Non supporté avec `ONLINE ALTER COLUMN`, quelle que soit la version.**

---

## 8. Exécution & monitoring

Le thread de monitoring interroge le serveur sur des **intervalles découplés** (un seul intervalle de
60 s est trop grossier : 60 s de blocage en prod = incident) :

- `blocking_poll_seconds` (défaut **10**) — détection de blocage.
- `log_poll_seconds` (défaut **60**) — pression sur le journal.
- `progress_poll_seconds` (défaut **30**) — progression du DDL.

### 8.1 Journal de transactions

```sql
SELECT total_log_size_in_bytes, used_log_space_in_percent
FROM sys.dm_db_log_space_usage OPTION (RECOMPILE);
```

⚠️ `used_log_space_in_percent` est un pourcentage de la **taille courante** du fichier (qui
autograndit). Le seuil qui compte est l'**octet absolu utilisé vs le plafond accepté** (`log_max_size`
de config, borné par `max_size` lu dans `sys.database_files`). On surveille donc **l'absolu en
premier**, le pourcentage en secondaire.

Si on franchit le plafond, on déclenche la **hiérarchie de réaction** (§9). Interprétation de
`log_reuse_wait_desc` (paramétré par la base de la connexion via `DB_NAME()`) :

```sql
SELECT log_reuse_wait_desc FROM sys.databases WHERE database_id = DB_ID();
```

- `LOG_BACKUP` (recovery FULL/BULK_LOGGED) → il faut une sauvegarde de journal : **on attend** (elle
  arrive en général toutes les 5–15 min). On ne déclenche **jamais** soi-même un log backup (casserait
  la chaîne managée).
- `ACTIVE_TRANSACTION` → c'est notre propre DDL.
- `AVAILABILITY_REPLICA` / `REPLICATION` → log retenu par un secondaire / la réplication.
- En recovery **SIMPLE** seulement, un `CHECKPOINT` entre opérations peut aider (option de config
  `checkpoint_between_operations`) ; en FULL il ne tronque rien → inutile.

Si le journal ne se libère pas après `log_drain_timeout_minutes`, on abandonne l'opération et on
loggue le `log_reuse_wait_desc` constaté.

### 8.2 Blocages

```sql
SELECT
    r.session_id, r.status, r.command,
    s.login_name, s.host_name, s.program_name,
    DB_NAME(r.database_id) AS database_name,
    r.total_elapsed_time, r.wait_type, r.wait_time,
    r.blocking_session_id, r.open_transaction_count,
    SUBSTRING(qt.text, (r.statement_start_offset/2)+1,
        ((CASE r.statement_end_offset WHEN -1 THEN DATALENGTH(qt.text)
          ELSE r.statement_end_offset END - r.statement_start_offset)/2)+1) AS active_query,
    qt.text AS parent_query
FROM sys.dm_exec_requests r
INNER JOIN sys.dm_exec_sessions s ON r.session_id = s.session_id
OUTER APPLY sys.dm_exec_sql_text(r.sql_handle) qt
WHERE s.is_user_process = 1
  AND (s.status <> 'sleeping' OR r.open_transaction_count > 0)
ORDER BY r.cpu_time DESC
OPTION (RECOMPILE, MAXDOP 1);
```

L'outil connaît son propre `@@SPID`. Il distingue deux situations :

- **Notre DDL est bloqué** (par un verrou de schéma p.ex.) : on suit `r.blocking_session_id` sur
  *notre* session.
- **Notre DDL bloque les autres** : on identifie les sessions dont `blocking_session_id = SPID_DDL`.
  Il faut **remonter la chaîne jusqu'au *head blocker*** (le bloqueur direct n'est pas toujours la
  racine).

Quand notre DDL bloque d'autres sessions au-delà de `blocking_timeout_minutes`, on entre dans la
hiérarchie de réaction (§9). On **loggue le texte des requêtes bloquées**.

### 8.3 Progression

```sql
SELECT percent_complete, estimated_completion_time, total_elapsed_time
FROM sys.dm_exec_requests WHERE session_id = @SPID_DDL;
```

Disponible pour `REBUILD`/`ALTER` : affiché dans le TUI et loggué périodiquement → l'opérateur sait
si on est à 5 % ou 95 % avant de décider d'annuler.

---

## 9. Hiérarchie de réaction (pression : blocage ou log)

Quand l'outil doit soulager la pression, il choisit le mécanisme **le moins destructif disponible**.
RESUMABLE et WAIT_AT_LOW_PRIORITY ne sont pas au même niveau logique : WALP gère l'**acquisition du
verrou** (bascule SCH-M en début/fin), RESUMABLE gère l'**abandon de la longue phase centrale**. On
les combine quand c'est possible.

**Décision en fonction des capacités de l'opération en cours :**

1. **Opération RESUMABLE en cours → `PAUSE` / `RESUME`.** *Stratégie préférée.* Une opération
   resumable committe par incréments : `PAUSE` **conserve le travail déjà fait** *et* **libère la
   pression sur le journal** (le log redevient tronquable). On attend que la pression (blocage / log)
   retombe, puis `RESUME`. Coût à connaître : ~10–15 % plus lent, plus de log au total, et l'index
   partiel **consomme de l'espace data** tant qu'il n'est pas terminé/abandonné.

2. **WAIT_AT_LOW_PRIORITY** (injecté avec ONLINE) : laisse SQL Server gérer l'**attente du verrou**
   sans bloquer les lecteurs/écrivains. On injecte **toujours `ABORT_AFTER_WAIT = SELF`** (le DDL
   s'auto-annule s'il attend trop le verrou — jamais les requêtes utilisateur). Une option de config
   **dangereuse**, désactivée par défaut, permet `ABORT_AFTER_WAIT = BLOCKERS` (tue les bloqueurs
   utilisateur). Pendant la phase *centrale*, si ce sont d'autres requêtes utilisateur qui sont
   bloquées par le DDL, on applique le délai configuré puis on passe au point 3.

3. **Annulation Go puis `KILL`** (dernier recours, opération non resumable). Voir §10.

**Influence de l'ADR :** avec *Accelerated Database Recovery* activé, le `ROLLBACK` d'un `KILL` est
quasi instantané → le coût/bénéfice bascule en faveur du `KILL`, et l'insistance sur RESUMABLE peut
être assouplie. L'outil intègre l'état ADR dans le choix de stratégie.

Après annulation, on attend qu'il n'y ait plus de blocage / que le log soit redescendu, puis on
**réessaie la même opération** jusqu'à `max_retry_attempts` fois.

---

## 10. Stratégie de `KILL` propre (cancel vs kill)

N'utiliser **pas seulement** `context.WithCancel`. Si le driver ne propage pas l'annulation au
serveur, le DDL continue côté SQL Server. Donc :

1. Annulation via le contexte Go.
2. Si le DDL tourne toujours côté serveur après `kill_grace_seconds`, le thread de monitoring lance
   explicitement `KILL <SPID_DDL>` **sur sa propre connexion**.
3. **Suivi du rollback** : `KILL <SPID_DDL> WITH STATUSONLY` pour estimer le **% de rollback** et le
   logger/afficher — sinon l'opérateur croit à un plantage. Un 2ᵉ `KILL` ne fait rien : on **surveille**
   l'avancement, on ne le relance pas.

Le journal réserve l'espace nécessaire à son propre rollback (pas de risque d'épuisement *pendant* le
rollback), mais celui-ci continue de générer du log non tronquable jusqu'à la fin — d'où la
préférence pour RESUMABLE quand c'est possible.

---

## 11. Récupération après crash / opérations orphelines

Si l'outil meurt (ou est tué), le manifeste est resté dans `02.processing/` et le DDL **tourne
peut-être encore** côté serveur (session orpheline). Au redémarrage, pour chaque manifeste présent
dans `02.processing/` :

1. Lire le **sidecar d'état** `<nom>.state.json` écrit avant l'exécution, contenant la **signature de
   session** : `SPID` + `login_time` (`sys.dm_exec_sessions`) + un **GUID** placé dans
   `CONTEXT_INFO` (ou un `Application Name` unique dans la chaîne de connexion), + la commande exacte
   et l'horodatage de départ.
2. Le SPID seul est **non fiable** (réutilisé). On corrèle **SPID + login_time + GUID CONTEXT_INFO**
   pour confirmer qu'une session vivante est bien *notre* DDL orphelin.
3. Consulter `sys.dm_exec_session_wait_stats` / l'état de la requête, et surtout
   `sys.index_resumable_operations` :
   - opération **resumable orpheline** → la **reprendre** (`RESUME`) plutôt que la relancer à zéro ;
   - session orpheline non resumable identifiée → décider selon config : attendre sa fin, ou
     `KILL` + reprise idempotente ;
   - aucune trace → reprise propre depuis le début (gardes idempotentes de §1.5).

Les opérations resumable **survivent au crash de l'outil et au redémarrage du serveur** : leur état
est persistant côté moteur. Au démarrage, l'outil interroge **systématiquement**
`sys.index_resumable_operations` pour adopter une éventuelle opération orpheline au lieu d'en créer
une nouvelle (risque de double exécution).

---

## 12. TUI — console d'incident

Le flag `--tui` ouvre une console temps réel. Au-delà de l'affichage de progression
(`percent_complete`, temps estimé, % de rollback si KILL en cours), elle permet la **décision
manuelle** :

- **Liste live des sessions bloquées** par notre DDL, avec le détail : `login_name`, `host_name`,
  `program_name`, `wait_type`, durée, **texte de la requête**.
- Actions (avec **confirmation explicite**, en distinguant nettement les cibles) :
  - `KILL` d'un **bloqueur utilisateur** précis ;
  - `KILL` de **notre DDL** ;
  - `PAUSE` (si l'opération est resumable) ;
  - **prolonger** le timer d'attente ;
  - **snapshot** de l'état courant vers le `.log`.

---

## 13. Format des logs

Chaque manifeste terminé produit `<nom>.log` à **double rendu** : un bloc **JSON** structuré
(machine) + un **résumé humain** lisible. Champs attendus :

- horodatages (début/fin), durée totale et par opération ;
- pour chaque opération : commande **générée finale**, **options injectées + justification** (version,
  édition, entrée de matrice, override) ;
- batchs/opérations exécutés avec succès (pour la reprise) ;
- retries / cancels / pauses, avec **texte des requêtes bloquées** au moment de la décision ;
- `percent_complete` final, `log_reuse_wait_desc` si abandon pour cause de log ;
- erreurs SQL récupérées *gracefully* (numéro, sévérité, message).

Le sidecar `<nom>.state.json` (§11) est supprimé en fin de traitement réussi.

---

## 14. Notifications

Webhook / Slack / e-mail (configurable) sur les événements : **cancel**, **échec**, **pause**, **log
plein**, **abandon**. En production, l'équipe doit être alertée en direct, pas découvrir le `.log` le
lendemain.

---

## 15. Historique des runs

Persistance optionnelle (SQLite ou table dédiée, destination dans `config.yaml`) de chaque exécution :
durée, retries, pauses, blocages, options injectées, résultat. Permet d'analyser les tendances et
d'estimer les prochaines opérations.

---

## 16. Codes de sortie

Pour intégration CI / SQL Agent :

| Code | Signification                                  |
|------|------------------------------------------------|
| 0    | Tous les manifestes traités avec succès        |
| 1    | Au moins un manifeste en échec (`04.failed/`)  |
| 2    | Erreur de configuration / manifeste invalide   |
| 3    | Erreur de connexion au serveur                 |
| 4    | Échec de pré-vol global (état serveur malsain) |

---

## 17. Sécurité & permissions

- **Pas de credentials en clair.** La chaîne de connexion et le mot de passe viennent d'un fichier
  **`.env`** (non versionné), pas du YAML. Support **authentification Windows / Azure AD** et
  **`encrypt=true`** dans la chaîne. Ne jamais logger la chaîne complète / le mot de passe.
- **Permissions minimales** du compte de service à documenter :
  - `VIEW SERVER STATE` (et `VIEW DATABASE STATE`) pour les DMV de monitoring ;
  - `ALTER ANY CONNECTION` (ou `processadmin` / `sysadmin`) pour le `KILL` ;
  - `ALTER` sur les objets ciblés (tables / index).

---

## 18. Paramètres — `config.yaml`

```yaml
database:
  # secrets via .env : ${DB_PASSWORD}, etc. — jamais en clair ici
  connection_string: "server=localhost;database=EXAMPLEDB;encrypt=true;trustServerCertificate=true;app name=SqlGoPace"
  login_timeout_seconds: 15      # connexion uniquement — PAS de timeout de requête
  # Query timeout du driver = 0 (infini) : pas de Global Timeout sur un DDL.
  # Le contrôle de durée est délégué au monitoring (blocage / log), jamais à un timer fixe.

directories:
  to_run:     "./01.to_run"
  processing: "./02.processing"
  done:       "./03.done"
  failed:     "./04.failed"

monitoring:
  blocking_poll_seconds: 10
  log_poll_seconds: 60
  progress_poll_seconds: 30
  log_max_size_bytes: 53687091200   # 50 GB — plafond absolu (borné par max_size des fichiers log)
  log_max_percent: 80               # secondaire
  blocking_timeout_minutes: 5       # délai avant réaction quand notre DDL bloque d'autres sessions
  log_drain_timeout_minutes: 30     # abandon si le log ne se libère pas
  max_retry_attempts: 3
  kill_grace_seconds: 30            # délai avant KILL explicite si l'annulation Go ne prend pas
  checkpoint_between_operations: false  # n'a d'effet qu'en recovery model SIMPLE

preflight:
  require_data_free_space: true     # exige ≈ taille de l'objet libre dans le filegroup
  check_tempdb: true
  ag_send_queue_warn: true          # avertit mais n'empêche pas (configurable)

options_override:
  online:               { force: null }   # true / false / null(auto)
  resumable:            { force: null }
  wait_at_low_priority: { force: null }
  maxdop:               { force: null }
  sort_in_tempdb:       { force: null }
  # priorité du code vs auto : ici tout est généré par l'outil (pas de SQL brut),
  # donc seule la résolution override/opération/matrice s'applique (cf. §1.3).
  allow_abort_blockers: false        # DANGEREUX : autorise ABORT_AFTER_WAIT = BLOCKERS

notifications:
  webhook_url: ""                    # vide = désactivé
  on_events: [cancel, fail, pause, log_full, abort]

history:
  enabled: true
  destination: "sqlite://./sqlgopace_history.db"

matrix_file: "./ddl_compatibility.yaml"
```

---

## 19. Requêtes SQL de référence

```sql
-- ADR activé ?
SELECT is_accelerated_database_recovery_on FROM sys.databases WHERE database_id = DB_ID();

-- Recovery model
SELECT recovery_model_desc FROM sys.databases WHERE database_id = DB_ID();

-- Opérations resumable en cours / en pause (reprise après crash)
SELECT object_id, index_id, name, state_desc, last_pause_time, percent_complete, page_count
FROM sys.index_resumable_operations;

-- État des réplicas AG (send queue / troncature log)
SELECT database_id, is_local, synchronization_state_desc,
       log_send_queue_size, redo_queue_size
FROM sys.dm_hadr_database_replica_states;

-- Taille / plafond des fichiers log
SELECT name, size, max_size, growth FROM sys.database_files WHERE type_desc = 'LOG';
```

(Voir aussi §8.1 / §8.2 / §8.3 pour les requêtes de monitoring.)

---

## 20. Contraintes techniques (récapitulatif)

- **Connexions séparées** exécution / monitoring (§3).
- **`KILL` explicite** en complément de l'annulation Go (§10).
- Driver **`microsoft/go-mssqldb`**, query timeout = 0, `login_timeout` côté connexion seulement.
- **Aucun parsing de SQL brut** : tout le DDL est généré depuis les manifestes YAML déclaratifs.
