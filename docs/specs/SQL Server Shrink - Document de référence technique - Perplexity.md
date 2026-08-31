# SQL Server Shrink — Référence technique complète

> Document de synthèse à usage interne — base technique pour le développement de la fonctionnalité de shrink dans SqlGoPace et pour la rédaction d'un article technique détaillé.

***

## 1. Vue d'ensemble : qu'est-ce que le shrink ?

Le **shrink** (réduction) désigne l'ensemble des opérations qui permettent de réduire la taille physique des fichiers d'une base de données SQL Server sur le disque. Il existe deux commandes principales :[^1][^2]

- `DBCC SHRINKDATABASE` — agit sur tous les fichiers de la base (données + journaux) en une seule commande.
- `DBCC SHRINKFILE` — cible un fichier précis (par nom logique ou `file_id`), avec un contrôle fin.

La documentation officielle Microsoft est explicite : **le shrink ne doit pas être considéré comme une opération de maintenance régulière**. Les fichiers qui grossissent en raison de l'activité normale de la base n'ont pas besoin d'être réduits.[^1]

***

## 2. Fonctionnement général et algorithme

### 2.1 Principe du déplacement de pages (fichier de données)

Le moteur SQL Server organise les fichiers de données en **pages** de 8 Ko, regroupées en **extents** de 8 pages (64 Ko). Lors d'un shrink de fichier de données :[^3][^4]

1. SQL Server calcule une **cible** (`target extent`) — la frontière au-delà de laquelle le fichier sera tronqué.
2. Un **scan GAM** (Global Allocation Map) est lancé depuis le début du fichier pour identifier les extents alloués au-delà de la cible.[^5]
3. Pour chaque extent alloué au-delà de la cible, SQL Server déplace les pages vers le premier espace libre disponible **en amont** de la cible.
4. Une fois la zone à libérer vide, le fichier est tronqué et l'espace est rendu à l'OS.[^3]

Le déplacement est effectué **par batches de ~32 pages** depuis SQL Server 2005. Chaque batch est une transaction indépendante : si l'opération est interrompue, seul le batch en cours est annulé, le travail antérieur est conservé. C'est ce qui rend l'opération **restartable**.[^6][^1]

### 2.2 Comportement selon le type de page

- **Pages de données ordinaires** : déplacement par paire delete/insert.[^5]
- **Pages d'index** : déplacement sans préservation de l'ordre logique → fragmentation immédiate.[^7]
- **Pages BLOB (TEXT/IMAGE/LOB)** : traitement particulièrement coûteux. Un scan IAM complet de toute la chaîne BLOB associée est déclenché pour chaque page BLOB rencontrée. Sur une base de 1 To avec 500 Go de BLOB, cela génère un volume massif d'I/O.[^5]
- **Pages LOB dans des columnstores compressés** : **non traitées** par `DBCC SHRINKFILE` et `DBCC SHRINKDATABASE` — limitation documentée connue.[^1]

### 2.3 Ce que le shrink NE fait PAS

`DBCC SHRINKFILE` opère **au niveau de l'extent**, pas au niveau de la page individuelle :[^8]

- Il ne compacte pas les pages partiellement remplies.
- Il ne fusionne pas les extents mixtes.
- Il ne supprime pas les pages vides à l'intérieur d'un extent alloué.

Conséquence : si une base contient beaucoup d'extents avec seulement 1 ou 2 pages utilisées, le shrink sera peu efficace — il ne pourra pas libérer autant d'espace que l'espace logiquement "vide" le laisse espérer.[^8]

### 2.4 Modes de shrink disponibles

| Mode | Effet sur les pages | Espace rendu à l'OS | Fragmentation |
|------|---------------------|---------------------|----------------|
| Normal (target_size) | Déplace les pages de la fin vers l'avant | Oui | Élevée |
| `TRUNCATEONLY` | Aucun déplacement | Oui (espace libre final uniquement) | Nulle |
| `NOTRUNCATE` | Déplace les pages vers l'avant | Non | Élevée |
| `EMPTYFILE` | Migre tout le contenu vers les autres fichiers du filegroup | N/A | Élevée |

`TRUNCATEONLY` est la seule option qui ne génère pas de fragmentation : elle se contente de couper la fin du fichier si elle est vide. C'est la méthode à privilégier quand l'espace libre se trouve déjà en fin de fichier (après un `DROP TABLE` ou un `TRUNCATE TABLE` massif).[^9][^1]

***

## 3. Pourquoi éviter le shrink — les problèmes fondamentaux

### 3.1 Fragmentation d'index massive et immédiate

C'est le problème le plus grave. L'algorithme place les pages déplacées **dans le premier espace libre disponible**, sans tenir compte de l'ordre logique des clés d'index. Le résultat : une fragmentation d'index proche de 100% sur toute la zone touchée. Cette fragmentation :[^4][^10][^7]

- Dégrade les performances des requêtes sur des plages de données (`range scans`).
- Augmente les lectures logiques et physiques pour toutes les opérations batch.
- Nécessite une reconstruction d'index après le shrink — ce qui re-consomme de l'espace et peut annuler le bénéfice.

### 3.2 Génération massive de journaux de transactions

Chaque déplacement de page est une opération **entièrement loguée** dans le transaction log. Déplacer 1 Go de données depuis la fin vers le début du fichier génère au minimum 1 Go d'entrées dans le `.ldf`. Sur un shrink lourd, cela peut :

- Faire exploser la taille du journal, potentiellement sur le même disque.
- Déclencher des auto-growths du log.
- Provoquer des attentes `WRITELOG` intenses.
- En mode `FULL`, bloquer la troncature du log si les sauvegardes ne suivent pas.

### 3.3 La spirale mort auto-shrink / auto-grow

Si la base a besoin de son espace libre pour fonctionner (insertions futures), le shrink ne fait que forcer un auto-grow ultérieur — souvent en petits incréments inefficaces :[^7]

- Chaque auto-grow génère de nouveaux VLF mal dimensionnés.
- La fragmentation du fichier au niveau système de fichiers s'accumule.
- Le cycle shrink → grow → shrink est pur gaspillage de ressources I/O et CPU.[^11][^7]

Paul Randal (ex-ingénieur Microsoft, auteur du code du moteur de stockage) a qualifié `AUTO_SHRINK` d'option à supprimer du produit, ne pouvant identifier aucun cas d'usage légitime.[^7]

### 3.4 Pollution du buffer pool

Pendant le shrink, SQL Server lit et écrit massivement des pages qui passent par le **buffer pool**. Les pages "chaudes" (fréquemment accédées par les requêtes applicatives) sont expulsées du cache pour laisser place aux pages déplacées — dégradant les performances de toutes les requêtes concurrentes.[^7]

### 3.5 Cas légitimes pour un shrink

Le shrink n'est justifié que dans des scénarios de libération d'espace **définitif et massif** :[^12]

- Après purge de millions de lignes d'historique qui ne seront pas réalimentées.
- Après suppression de grandes tables ou indexes inutilisés.
- Lors d'une migration ou d'une réduction permanente du périmètre de la base.
- Jamais en maintenance récurrente planifiée.

***

## 4. Shrink du fichier de données — comportement détaillé

### 4.1 Conditions préalables

- L'espace libre calculé doit dépasser la taille cible.[^1]
- `DBCC SHRINKFILE` ne peut pas réduire un fichier en dessous de la taille nécessaire pour stocker les données réellement présentes — si 7 Mo sont utilisés dans un fichier de 10 Mo, une cible de 6 Mo donnera un résultat de 7 Mo.[^13]
- Des données dans des extents proches de la fin du fichier (même si peu nombreuses) peuvent limiter fortement la réduction.

### 4.2 Estimation de l'espace récupérable avant l'opération

```sql
SELECT
    name AS NomFichier,
    physical_name AS CheminPhysique,
    size                                                           AS TailleTotale_Pages,
    CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)                  AS PagesUtilisees,
    size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)           AS PagesVides,
    (size / 128.0)                                                AS TailleTotale_MB,
    (CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) / 128.0)        AS EspaceUtilise_MB,
    ((size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)) / 128.0) AS EspaceVide_MB
FROM sys.database_files
WHERE type_desc = 'ROWS';
```

### 4.3 Comportement lors du déplacement des pages

L'opération est **online** — les autres utilisateurs peuvent lire et écrire pendant le shrink. SQL Server acquiert des verrous page par page lors du déplacement, pas un verrou exclusif sur toute la base. Toutefois, des verrous `Sch-M` (schema modify) sont nécessaires lors de la manipulation des pages IAM (Index Allocation Map), ce qui crée des conflits avec les verrous `Sch-S` (schema stability) des requêtes actives.[^14][^8][^1]

Le shrink travaille **par extents entiers** et non par pages individuelles. Il ne peut donc pas libérer l'espace d'un extent dont une seule page est utilisée.[^8]

***

## 5. Shrink du journal de transactions — comportement spécifique

### 5.1 Architecture interne : les VLF

Le fichier journal `.ldf` est divisé en **Virtual Log Files (VLF)**. Les VLF sont des unités logiques internes dont la taille est déterminée dynamiquement par SQL Server :[^15][^16]

- Lors de la création ou de l'extension du fichier, SQL Server alloue des VLF dont la taille dépend de l'incrément de croissance.
- Un VLF peut être dans l'état `active` (contient des entrées de log actives) ou `inactive` (déjà sauvegardé ou tronqué).

La fragmentation du log (trop de VLF, trop petits) ralentit les sauvegardes, la récupération après crash, et les restaurations.[^17][^18]

### 5.2 Différence fondamentale avec le fichier de données

**Le shrink du journal ne déplace pas de pages.** Il ne peut que **supprimer les VLF inactifs situés à la fin du fichier**. C'est une troncature pure, pas un compactage.[^19]

Conséquence directe : si le VLF actif (contenant la portion active du log) se trouve à la fin du fichier, **le shrink est impossible** — SQL Server ne peut pas shrink le journal au-delà du dernier VLF actif.[^20]

### 5.3 Conditions préalables au shrink du journal

Le shrink du journal ne fonctionne que si les VLF en fin de fichier sont **inactifs**. Pour libérer les VLF :[^21][^19]

| Mode de récupération | Condition de troncature du log |
|----------------------|-------------------------------|
| `SIMPLE` | Automatique après chaque CHECKPOINT |
| `FULL` | Après chaque sauvegarde du journal de transactions |
| `BULK_LOGGED` | Après chaque sauvegarde du journal |

**Procédure correcte pour shrink du log en mode FULL :**

```sql
-- 1. Sauvegarder le journal pour le tronquer
BACKUP LOG [MaBase] TO DISK = 'NUL'; -- ou vers un vrai fichier backup

-- 2. Vérifier l'espace libre dans le journal
DBCC SQLPERF(LOGSPACE);

-- 3. Shrink du fichier journal
USE [MaBase];
DBCC SHRINKFILE (N'MaBase_log', 512); -- target en Mo

-- 4. Éventuellement répéter si l'espace n'est pas totalement libéré
```

**Procédure pour le mode SIMPLE :**

```sql
USE [MaBase];
CHECKPOINT;
DBCC SHRINKFILE (N'MaBase_log', 512);
```

### 5.4 Conséquences du shrink sur les VLF

Shrink + re-grow du journal génère des VLF mal dimensionnés. La meilleure pratique après un shrink du log est de pré-allouer le journal à sa taille cible en un seul ALTER DATABASE, pour obtenir un nombre minimal de VLF bien dimensionnés. Les multiples auto-grows en petits incréments créent des dizaines ou centaines de petits VLF — seuil critique au-delà de 50 VLFs.[^18][^22]

```sql
-- Après shrink, recréer le journal proprement
ALTER DATABASE [MaBase]
MODIFY FILE (NAME = N'MaBase_log', SIZE = 8000MB); -- en un seul bloc
```

### 5.5 Cas TRUNCATEONLY sur le journal

L'option `TRUNCATEONLY` **fonctionne sur le journal** — elle supprime les VLF inactifs en fin de fichier sans aucun déplacement. C'est l'équivalent d'un shrink "non destructif" pour le log.[^23]

***

## 6. Problèmes de concurrence et de verrouillage

### 6.1 Les verrous Sch-S / Sch-M

Le shrink requiert un verrou `Sch-M` (Schema Modify) pour manipuler les pages IAM. Ce verrou est **incompatible** avec les verrous `Sch-S` (Schema Stability) que toutes les requêtes actives maintiennent sur leurs tables. Le résultat est une chaîne de blocage :[^14][^1]

```
Requête active → tient Sch-S
   ↓ bloque
Shrink → attend Sch-M
   ↓ bloque
Toutes les nouvelles requêtes → attendent derrière le shrink
```

Ce phénomène de "train de blocage" peut paralyser toute l'activité applicative.[^14]

### 6.2 Blocage par les transactions snapshot (RCSI/SI)

Les transactions utilisant un niveau d'isolation basé sur le versioning de lignes (Read Committed Snapshot Isolation ou Snapshot Isolation) **bloquent le shrink** :[^14][^1]

```
DBCC SHRINKFILE for file ID 1 is waiting for the snapshot
transaction with timestamp 15 and other snapshot transactions linked to
timestamp 15 or with timestamps older than 109 to finish.
```

Ce message (erreur informative 5203 dans le log SQL Server) est enregistré toutes les 5 minutes pendant la première heure, puis toutes les heures. Pour identifier la transaction bloquante :[^1]

```sql
SELECT transaction_sequence_num, first_snapshot_sequence_num, *
FROM sys.dm_tran_active_snapshot_database_transactions
WHERE transaction_sequence_num < 109; -- remplacer par le timestamp du message
```

### 6.3 WAIT_AT_LOW_PRIORITY (SQL Server 2022+)

Depuis SQL Server 2022, la syntaxe `WITH WAIT_AT_LOW_PRIORITY` permet au shrink d'acquérir le verrou `Sch-M` en mode basse priorité :[^24][^1]

```sql
DBCC SHRINKFILE (5, 1024)
WITH WAIT_AT_LOW_PRIORITY (ABORT_AFTER_WAIT = SELF);
```

Comportement :[^1]
- Les nouvelles requêtes ne sont **pas** bloquées par l'attente du shrink.
- Si le shrink ne peut pas obtenir le verrou Sch-M dans la minute (par défaut), il expire **silencieusement**.
- L'erreur **49516** est écrite dans le log SQL Server à l'expiration.
- `ABORT_AFTER_WAIT = SELF` : le shrink s'annule si le timeout expire.
- `ABORT_AFTER_WAIT = BLOCKERS` : le shrink tue les sessions bloquantes (requiert `ALTER ANY CONNECTION`).

Pour SqlGoPace : détecter la version du serveur avant d'utiliser cette option (SQL Server 2022+ seulement). En cas d'erreur 49516, attendre quelques minutes et relancer.

***

## 7. Erreurs et messages à connaître

| Erreur / Message | Type | Signification | Action |
|-----------------|------|---------------|--------|
| **5202** | Informatif (log SQL Server) | `SHRINKDATABASE` bloqué par une transaction snapshot | Attendre ou tuer la transaction bloquante |
| **5203** | Informatif (log SQL Server) | `SHRINKFILE` bloqué par une transaction snapshot | Idem — répété toutes les 5 min (1h), puis toutes les heures |
| **49516** | Erreur Level 16 | Timeout `WAIT_AT_LOW_PRIORITY` — impossible d'obtenir le verrou Sch-M | Relancer l'opération après quelques minutes |
| **Pas de réduction visible** | Comportement normal | Pas assez d'espace libre, ou espace libre non situé en fin de fichier | Vérifier avec `sys.database_files` + `FILEPROPERTY` |
| **Log non shrinkable** | Comportement normal | VLF actifs en fin de fichier | Sauvegarder le log (mode FULL) ou exécuter CHECKPOINT (mode SIMPLE) |
| **9002** | Erreur | Transaction log full | Le shrink lui-même génère du log — réduire la taille des batches (StepSize) |
| **Corruption potentielle** | Critique | Shrink interrompu brutalement | Vérifier avec `DBCC CHECKDB` — le travail complété est conservé, seul le batch actif est rollbacké |

***

## 8. Surveillance complète du shrink

### 8.1 Progression en temps réel

```sql
-- Progression et temps restant estimé
SELECT
    session_id,
    command,
    status,
    percent_complete,
    estimated_completion_time / 1000 / 60          AS estimated_minutes_left,
    total_elapsed_time / 1000 / 60                 AS elapsed_minutes,
    wait_type,
    wait_time / 1000                               AS wait_seconds,
    blocking_session_id,
    last_wait_type
FROM sys.dm_exec_requests
WHERE command IN ('DbccFilesCompact', 'DbccSpaceReclaim')
   OR command LIKE 'DBCC%';
```

> **Note** : `DbccFilesCompact` correspond à `DBCC SHRINKFILE`, `DbccSpaceReclaim` à `DBCC SHRINKDATABASE`. Le `percent_complete` et `estimated_completion_time` fluctuent fortement en cas de pages BLOB ou de forte fragmentation.

### 8.2 Surveillance des I/O pendant l'opération

```sql
-- Latences I/O par fichier (capture delta)
SELECT
    DB_NAME(fs.database_id)                           AS [Database],
    mf.physical_name,
    fs.io_stall_read_ms,
    fs.io_stall_write_ms,
    fs.io_stall,
    fs.num_of_reads,
    fs.num_of_writes,
    CASE WHEN fs.num_of_reads = 0 THEN 0
         ELSE fs.io_stall_read_ms / fs.num_of_reads END  AS avg_read_ms,
    CASE WHEN fs.num_of_writes = 0 THEN 0
         ELSE fs.io_stall_write_ms / fs.num_of_writes END AS avg_write_ms
FROM sys.dm_io_virtual_file_stats(NULL, NULL) fs
JOIN sys.master_files mf
    ON fs.database_id = mf.database_id AND fs.file_id = mf.file_id
WHERE DB_NAME(fs.database_id) = DB_NAME()
ORDER BY fs.io_stall DESC;
```

Seuils d'alerte indicatifs :[^25]
- Lectures : > 20-25 ms de latence moyenne → pression I/O significative
- Écritures : > 5-10 ms → saturation du sous-système (surtout pour le log)

### 8.3 Surveillance des wait statistics

```sql
-- Wait types critiques pendant un shrink
SELECT wait_type, wait_time_ms, waiting_tasks_count,
       wait_time_ms / NULLIF(waiting_tasks_count, 0) AS avg_wait_ms
FROM sys.dm_os_wait_stats
WHERE wait_type IN (
    'PAGEIOLATCH_EX', 'PAGEIOLATCH_SH',   -- pression I/O données
    'WRITELOG',                             -- pression écriture log
    'LCK_M_SCH_M',                         -- attente verrou Sch-M (shrink bloqué)
    'LCK_M_SCH_M_LOW_PRIORITY',            -- attente Sch-M en mode WLP
    'LCK_M_SCH_M_ABORT_BLOCKERS'           -- mode WLP BLOCKERS actif
)
ORDER BY wait_time_ms DESC;
```

### 8.4 Surveillance du journal de transactions

```sql
-- Espace utilisé dans le journal
SELECT
    name,
    log_reuse_wait_desc,                    -- Raison pour laquelle le log ne peut pas être tronqué
    log_size_mb     = size * 8.0 / 1024,
    log_used_mb     = FILEPROPERTY(name, 'SpaceUsed') * 8.0 / 1024,
    recovery_model_desc
FROM sys.databases
WHERE name = DB_NAME();

-- Détail par DMV (SQL Server 2016+)
SELECT
    database_id,
    total_log_size_mb       = total_log_size_bytes / 1048576.0,
    used_log_space_mb       = used_log_space_in_bytes / 1048576.0,
    used_log_space_pct      = used_log_space_in_percent,
    log_space_since_backup  = log_space_in_bytes_since_last_backup / 1048576.0
FROM sys.dm_db_log_space_usage;

-- Contenu des VLFs
DBCC LOGINFO;
```

### 8.5 Surveillance des blocages

```sql
-- Sessions bloquées par ou bloquant le shrink
SELECT
    r.session_id,
    r.blocking_session_id,
    r.command,
    r.status,
    r.wait_type,
    r.wait_time / 1000 AS wait_seconds,
    s.login_name,
    s.program_name,
    t.text AS sql_text
FROM sys.dm_exec_requests r
JOIN sys.dm_exec_sessions s ON r.session_id = s.session_id
CROSS APPLY sys.dm_exec_sql_text(r.sql_handle) t
WHERE r.blocking_session_id > 0
   OR r.command LIKE 'DBCC%';

-- Transactions snapshot actives (pour diagnostic blocage 5202/5203)
SELECT
    transaction_id,
    transaction_sequence_num,
    first_snapshot_sequence_num,
    elapsed_time_seconds,
    is_snapshot,
    session_id
FROM sys.dm_tran_active_snapshot_database_transactions
ORDER BY transaction_sequence_num;
```

### 8.6 Taille et espace libre des fichiers

```sql
-- Vue synthétique des fichiers de la base courante
SELECT
    name,
    type_desc,
    physical_name,
    size / 128.0                                                   AS size_mb,
    CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) / 128.0           AS used_mb,
    (size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)) / 128.0  AS free_mb,
    CAST(
        (size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS FLOAT)) / size * 100
    AS DECIMAL(5,1))                                               AS free_pct
FROM sys.database_files
ORDER BY type_desc, file_id;
```

***

## 9. Gestion dynamique du StepSize (chunking)

### 9.1 Pourquoi le chunking est nécessaire

Un shrink monolithique sur un gros fichier :
- Génère une transaction massive → le log explose
- Sature le sous-système I/O
- Bloque les autres sessions pendant de longues périodes
- Est très difficile à interrompre proprement

Le chunking (inspiration `Invoke-DbaDbShrink -StepSize`) décompose l'opération en boucle d'appels `DBCC SHRINKFILE` successifs avec une taille cible décrémentée à chaque itération.

### 9.2 Algorithme de calibration du StepSize

**Heuristique de départ :**

| Volume à récupérer | StepSize recommandé |
|--------------------|---------------------|
| < 5 Go | 100–250 Mo |
| 5–50 Go | 250–500 Mo |
| > 50 Go | 500 Mo–1 Go |
| Mode FULL, sauvegardes < 15 min | ≤ volume log libéré par sauvegarde |

**Signaux d'alerte pour réduire le StepSize :**
- `WRITELOG` avg_wait_ms > 10 ms → le log est sous pression
- `PAGEIOLATCH_EX` avg_wait_ms > 20 ms → le disque de données est saturé
- `blocking_session_id` > 0 depuis plus de 30 s → des sessions sont bloquées

**Signaux permettant d'augmenter le StepSize :**
- Latences I/O < 5 ms
- Pas de wait type significatif
- `log_space_in_bytes_since_last_backup` reste faible

### 9.3 Pattern d'implémentation Go pour SqlGoPace

```go
// Pseudo-code de la boucle de shrink avec ajustement dynamique
for currentTarget > finalTarget {
    nextTarget := currentTarget - stepSizeMB
    if nextTarget < finalTarget {
        nextTarget = finalTarget
    }

    startTime := time.Now()
    err := executeShrinkFile(db, fileName, nextTarget)
    elapsed := time.Since(startTime)

    // Mesure des waits après le batch
    waits := queryWaitStats(db)
    ioStats := queryIOStats(db)

    // Ajustement dynamique
    if waits.WRITELOG > 10 || waits.PAGEIOLATCH_EX > 20 {
        stepSizeMB = max(stepSizeMB/2, minStepSizeMB)
    } else if elapsed < targetBatchDuration && waits.AllOK() {
        stepSizeMB = min(stepSizeMB*2, maxStepSizeMB)
    }

    logBatch(currentTarget, nextTarget, elapsed, stepSizeMB, waits, ioStats)
    currentTarget = nextTarget
}
```

***

## 10. Surveillance du journal pendant un shrink de données

C'est un point critique souvent ignoré : **un shrink de fichier de données génère lui-même du log**. Il faut surveiller en continu :

```sql
-- À exécuter en polling pendant l'opération
SELECT
    used_log_space_in_percent,
    used_log_space_in_bytes / 1048576.0 AS used_log_mb,
    log_space_in_bytes_since_last_backup / 1048576.0 AS since_backup_mb
FROM sys.dm_db_log_space_usage;
```

Si `used_log_space_in_percent` dépasse 70–80% pendant le shrink :
- En mode FULL : déclencher une sauvegarde du journal immédiatement.
- En mode SIMPLE : émettre un `CHECKPOINT`.
- Réduire le StepSize pour le batch suivant.

***

## 11. Actions selon les problèmes rencontrés

| Problème | Diagnostic | Action |
|----------|-----------|--------|
| Le fichier ne rétrécit pas | `sys.database_files` → free_mb ≈ 0 | Pas d'espace libre réel — opération inutile |
| Le log ne rétrécit pas | `sys.databases.log_reuse_wait_desc` | Mode FULL → sauvegarder le log ; Mode SIMPLE → CHECKPOINT |
| Shrink bloqué (5202/5203) | `sys.dm_tran_active_snapshot_database_transactions` | Attendre la fin de la transaction, ou tuer avec précaution |
| Timeout WAIT_AT_LOW_PRIORITY (49516) | Log SQL Server | Attendre quelques minutes, identifier les longues requêtes actives, relancer |
| Saturation I/O (`PAGEIOLATCH_EX`) | `sys.dm_io_virtual_file_stats` | Réduire le StepSize, déplacer l'opération en heures creuses |
| Journal plein pendant le shrink (9002) | `sys.dm_db_log_space_usage` | Stopper le shrink, sauvegarder le log, reprendre avec StepSize réduit |
| Lenteur excessive (pages BLOB/LOB) | `percent_complete` progresse très lentement | Normal pour les tables avec varbinary(max), text, image — prévoir des heures |
| Blocage du shrink par index rebuild | `sys.dm_exec_requests` wait_type = LCK_M_SCH_M | Ne pas lancer en même temps qu'un rebuild d'index — scheduler correctement |
| Fragmentation post-shrink | `sys.dm_db_index_physical_stats` | Reconstruire les index après — prévoir le double du temps total |

***

## 12. Bonnes pratiques opérationnelles

1. **Toujours mesurer avant** : vérifier le `free_pct` et l'espace réellement libre avec `sys.database_files`. Un shrink qui déplace 0 page est instantané ; un shrink qui déplace 50 Go peut prendre des heures.

2. **Préférer `TRUNCATEONLY` quand applicable** : si l'espace libre se trouve déjà en fin de fichier (après un `TRUNCATE TABLE` récent), `TRUNCATEONLY` est instantané et sans fragmentation.[^9]

3. **Ne jamais lancer simultanément plusieurs fichiers du même filegroup** : la contention sur les tables système provoque des délais et des blocages supplémentaires.[^1]

4. **Planifier la reconstruction d'index** après tout shrink avec déplacement de pages. Cette étape est obligatoire pour restaurer les performances. Elle réclame de l'espace libre — prévoir 20–30% d'espace supplémentaire.[^19]

5. **Désactiver `AUTO_SHRINK` sur toutes les bases de production** :
   ```sql
   ALTER DATABASE [MaBase] SET AUTO_SHRINK OFF;
   ```

6. **Après un shrink du journal, pré-allouer le log** à sa taille cible en un seul bloc pour contrôler le nombre de VLF.[^22]

7. **Utiliser `WAIT_AT_LOW_PRIORITY` sur SQL Server 2022+** pour éviter l'effet de "train de blocage". Surveiller les erreurs 49516 dans le log SQL Server et retry automatiquement.[^24][^1]

8. **Sur SQL Server 2022+**, `MAXDOP` est supporté pour certaines opérations de shrink — permettant de paralléliser le déplacement de pages. À utiliser avec précaution pour ne pas saturer les I/O.

***

## Références et documentation officielle

- [DBCC SHRINKFILE — Microsoft Learn](https://learn.microsoft.com/en-us/sql/t-sql/database-console-commands/dbcc-shrinkfile-transact-sql)
- [Manage Transaction Log File Size — Microsoft Learn](https://learn.microsoft.com/en-us/sql/relational-databases/logs/manage-the-size-of-the-transaction-log-file)
- [How It Works: More on DBCC Shrink* Activities — Bob Dorr, Microsoft CSS](https://techcommunity.microsoft.com/blog/sqlserverstudioteam/how-it-works-more-on-dbcc-shrink-activities/315499)
- [Turn AUTO_SHRINK off!! — Paul Randal, Microsoft SQL Server Team](https://techcommunity.microsoft.com/blog/sqlserver/turn-auto-shrink-off/383234)
- [Invoke-DbaDbShrink — dbatools](https://dbatools.io/Invoke-DbaDbShrink/)
- [WAIT_AT_LOW_PRIORITY with shrink — SQL Server 2022 new feature](https://learn.microsoft.com/en-us/sql/t-sql/database-console-commands/dbcc-shrinkfile-transact-sql#wait_at_low_priority-with-shrink-operations)

---

## References

1. [DBCC SHRINKFILE (Transact-SQL) - SQL Server - Microsoft Learn](https://learn.microsoft.com/en-us/sql/t-sql/database-console-commands/dbcc-shrinkfile-transact-sql?view=sql-server-ver17) - Moves allocated pages from a data file's end to unallocated pages in a file's front with or without ...

2. [Master SQL Server SHRINKDB: Pros, Cons, Best Practices](https://stevestedman.com/2024/07/shrinking-databases/) - Explore the pros and cons of using SHRINKDB in SQL Server. Learn best practices for optimizing datab...

3. [Shrink a File - SQL Server](https://learn.microsoft.com/en-us/sql/relational-databases/databases/shrink-a-file?view=sql-server-ver17) - Learn how to shrink a data or log file in SQL Server by using SQL Server Management Studio or Transa...

4. [Shrinking Database Data Files - Simple SQL Server](https://simplesqlserver.com/2016/01/19/shrinking-database-data-files/) - The most common way to shrink a file is to have it reorganize pages before releasing free space, so ...

5. [How It Works: More on DBCC Shrink* Activities | Microsoft Community Hub](https://techcommunity.microsoft.com/blog/sqlserversupport/how-it-works-more-on-dbcc-shrink-activities/315499) - First published on MSDN on Jun 18, 2008 My peers are starting to tease me about becoming a dbcc shri...

6. [DBCC SHRINKFILE](https://sqldeepdives.blogspot.com/2015/08/dbcc-shrinkfile.html) - Select Query

7. [Turn AUTO_SHRINK off!!](https://techcommunity.microsoft.com/blog/sqlserver/turn-auto-shrink-off/383234) - First published on MSDN on Mar 28, 2007 This week's topic is data file shrinking.

8. [收缩数据文件](https://blog.csdn.net/weixin_30384031/article/details/98269907) - 文章浏览阅读306次。在执行DBCC ShrinkFile命令，收缩数据文件的时候，SQL Server首先将文件尾部的区（extent）移动到文件的开头，文件结尾的空闲的Disk空间会被收缩，释放给...

9. [Execute SQL Server DBCC SHRINKFILE Without Causing ...](https://www.mssqltips.com/sqlservertip/4368/execute-sql-server-dbcc-shrinkfile-without-causing-index-fragmentation/) - Learn how to execute SQL Server DBCC SHRINKFILE without causing index fragmentation and example cond...

10. [Shrink a File - SQL Server](https://learn.microsoft.com/sr-cyrl-rs/sql/relational-databases/databases/shrink-a-file?view=sql-server-ver17) - Learn how to shrink a data or log file in SQL Server by using SQL Server Management Studio or Transa...

11. [Autoshrink set to on](https://databasehealth.com/database-overview/database-warnings/autoshrink-set-to-on/) - Learn why Autoshrink set to on is a bad idea on SQL Server, and some better options for shrinking da...

12. [Why Shrinking SQL Server Databases Is Almost Always a Terrible Idea](https://www.linkedin.com/posts/markvarnas_shrinking-sql-server-databases-is-not-a-good-activity-7307400335709384704-WWSh) - Why Shrinking SQL Server Databases Is Almost Always a Terrible Idea DBAs often think shrinking SQL S...

13. [DBCC SHRINKFILE - Transact-SQL Reference Documentation](https://documentation.help/tsqlref/ts_dbcc_8b51.htm)

14. [SHRINK-2.md](https://ppl-ai-file-upload.s3.amazonaws.com/web/direct-files/attachments/12101329/d91536fd-322c-465f-8e93-a2c3ae89d1f1/SHRINK-2.md?AWSAccessKeyId=ASIA2F3EMEYETBN7BOJ2&Signature=v3zQy8C16AkXd%2FzSzLQUIiAzUuc%3D&x-amz-security-token=IQoJb3JpZ2luX2VjEKn%2F%2F%2F%2F%2F%2F%2F%2F%2F%2FwEaCXVzLWVhc3QtMSJGMEQCIE5Au9cyOj8C%2FB%2F0ZxRqxjHxE%2BtXBPUypMI4ddZxpyO9AiAu4cZGetPFu6Idj1Np4xnvu1GTkQR6vZuF1myCt2FI1irzBAhyEAEaDDY5OTc1MzMwOTcwNSIMcY18i7%2FmrXBKWfLpKtAEj5e0ayHG14U%2B%2Bw6yHLvev8kj%2FTZ6BmJJXHYFi8LWjGhcPHstYXjiQHzGdPrxqxktkOFsaRRQoifCyl0MXY%2BDwQddDgjEpitM1kKaWLfZl8HS5%2B70YZtks9yFdlrO89o8Tr0fBMqP6yrc0mv9Lbf4a0KuzE%2FgQD4r89%2BOhxYbPtc65v%2BhrTy%2BtO6Qgc23LJlnS0mySvczQoumkFc9pmvhPecOu6CuefT1s8NwkZeNwukX76imblxe16zVYT0Z4lKpOoqZMRfW%2Fk5uDNCcFD7Kh%2Fx7eabVnOiALFwJu214%2BcbJhh8bpb40uGdySUQACFF2iVpuO%2B%2F%2BQMZd6a4ez%2B7%2Bf4LnTHUFtW4Om3yBmHu66fEozVbWBSEDuOR1AzleuNHZOoSTE1p1KGCbKEmZcslhJeKxKJHrINHA%2BtfAf%2F2KpSCy1bN7smHB2QFt9odf4X1atu4guIj5pN5wU0k%2FEfRuy37D60ueLGCyjPRk8nq9kLT4nLqJkRyC6pEmIDhhISbecjLiIyEnyrw70zwQBXACaXKG2U2jSWyFpnUeNRnlDfa2qz3SuXIeuiPhOeYoWdDWIDEVvsjJp9FubdapN7TmaQlFuZRjTPbru1ZL%2FeFafb0TTmbP5XqpBRtLjH2yrksGvfVfo%2FYeyyFX7yxT7fzXj%2F1tlNkX8ECxI%2BPqd32snZUvBdufU8Ng4Zy2%2F7F%2F2sn4GIujlGvcrKy75Jj5IFB%2FyeED9t4tHq%2FUHFrD2IhKE2Rzxb29QQZoLIuBAirwGTDvxZpVrYGJoaPq540akTSOajDjlsTRBjqZAaXwZ8MEuWuYlxdvAqLO2ckcXJLvQIEoapZihozbCowsYg1R74uRqcrfrPTr08Lwrq7kkLlyYBcpM14NKCUBQDzBPXTcf835EaqidQnVyNTPIMPZUhBszSMdBjPfzROIM%2B55WcD2Brq95CBzXnqfD337tJW4SNjSBmhP3MutJCchyuBBoMgWr2lFVHkCZYoGgo48PU3aMNSeUQ%3D%3D&Expires=1781602614) - # Ajouter la fonctionnalité de shrink

Objectif : ajouter une commande et un suivi de shrink dans ...

15. [SQL-Server: What are VLF's and why should I care about them?](https://www.dbi-services.com/blog/sql-server-what-are-vlfs-and-why-should-i-care-about-them/) - The answer is quite simple: Too many virtual log files can slow down the recovery time of a database...

16. [Setting a Fixed Size for Transaction Log VLFs](https://www.sql.kiwi/2023/11/fixed-size-vlfs/) - Using undocumented procedure sp_start_fixed_vlf so a SQL Server 2022 database uses fixed sized VLFs....

17. [Stairway to Transaction Log Management in SQL Server, Level 7](https://www.sqlservercentral.com/steps/stairway-to-transaction-log-management-in-sql-server-level-7-dealing-with-excessive-log-growth) - This level will examine the most common problems and forms of mismanagement that lead to excessive g...

18. [Log File Fragmentation A hidden cause of poor performance](https://sqlconsulting.com/archives/log-file-fragmentation-a-hidden-cause-of-poor-performance/) - Internal fragmentation in a database log file is a frequently overlooked cause of poor performance i...

19. [Manage Transaction Log File Size - SQL Server - Microsoft Learn](https://learn.microsoft.com/en-us/sql/relational-databases/logs/manage-the-size-of-the-transaction-log-file?view=sql-server-ver17) - Shrinking a log file removes one or more VLFs that hold no part of the logical log (that is, inactiv...

20. [Lisenet.com :: Linux | Security | Networking | Admin Blog](https://www.lisenet.com/2013/shrink-mssql-logs-and-rebuild-database-table-indexes/) - Technical admin blog about Linux, Security, Networking and IT.

21. [SQL Server Recovery Models & Log Truncation](https://www.sqlbackupmaster.com/wordpress/2020/07/23/sql-server-recovery-models-log-truncation/) - We get a fair number of questions from SQL Backup Master users about transaction log files, often ac...

22. [TransactionLog. VLF Fragmentation. – SQLServerCentral Forums](https://www.sqlservercentral.com/forums/topic/transactionlog-vlf-fragmentation) - TransactionLog. VLF Fragmentation. Forum – Learn more on SQLServerCentral

23. [Troubleshooting the Issues with DBCC ShrinkDatabase or ...](http://sqlserverandme.blogspot.com/2014/08/troubleshooting-issues-with-dbcc.html) - Summary : To shrink all data and log files for a specific database, use DBCC SHRINKDATABASE command....

24. [Новое в SQL Server 2022: опция WAIT_AT_LOW_PRIORITY ... - Habr](https://habr.com/ru/articles/775468/) - Новая опция WAIT_AT_LOW_PRIORITY в команде DBCC SHRINKDATABASE предоставляет возможность снизить кон...

25. [What Virtual Filestats Do, and Do Not, Tell You About I/O ...](https://sqlperformance.com/2013/10/t-sql-queries/io-latency) - Erin Stellato (@erinstellato) of SQLskills.com shows us why I/O latency or high I/O-related waits ar...

