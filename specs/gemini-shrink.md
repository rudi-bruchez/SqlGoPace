> From: https://gemini.google.com/app/67cf4f9a5fd85308

# you asked

Y a-t-il un moyen d'évaluer combien de temps va prendre un shrink d'un fichier de base de données dans sql server ? Et combien de pages il devra déplacer ?

---

# gemini response

C'est une excellente question, et c'est un défi bien connu des administrateurs de bases de données (DBA). Pour être tout à fait franc : **il est extrêmement difficile d'estimer précisément le temps ou le nombre exact de pages à déplacer avant de lancer l'opération.**

Cependant, il existe des méthodes pour suivre l'avancement une fois l'opération lancée, et des moyens d'estimer la quantité de données concernées. Voici comment vous pouvez aborder le problème.

---

### 1. Évaluer le temps nécessaire (Pendant l'exécution)

Avant de lancer le *shrink*, le temps dépend de trop de variables : la vitesse de votre sous-système disque (I/O), la fragmentation interne, la présence de données LOB (Large Objects), et les verrous posés par d'autres requêtes actives.

Une fois que vous avez lancé le `DBCC SHRINKFILE` ou `DBCC SHRINKDATABASE`, SQL Server expose sa progression via les vues de gestion dynamique (DMV). Vous pouvez utiliser la requête suivante pour voir le pourcentage d'avancement et le temps restant estimé :

```sql
SELECT 
    session_id,
    command,
    status,
    percent_complete,
    estimated_completion_time / 1000 / 60 AS estimated_minutes_left,
    total_elapsed_time / 1000 / 60 AS elapsed_minutes,
    wait_type,
    wait_time,
    blocking_session_id
FROM sys.dm_exec_requests
WHERE command IN ('DbccFilesCompact', 'DbccSpaceReclaim') -- Commandes internes pour le shrink
   OR command LIKE 'DBCC%';
```

> **Attention :** Le `estimated_completion_time` (temps restant) est calculé en temps réel par SQL Server. Il peut fluctuer énormément. Si le moteur tombe sur une section du fichier très fragmentée ou des objets LOB, le temps estimé peut soudainement bondir.

---

### 2. Estimer le nombre de pages à déplacer (Avant l'exécution)

Pour comprendre pourquoi c'est difficile à prévoir, il faut comprendre comment fonctionne le *shrink* : SQL Server prend les pages de données situées **à la fin du fichier** physique et les déplace vers les **espaces vides (non alloués) situés au début du fichier**.

* **Le scénario idéal :** Si votre fichier fait 100 Go, que 50 Go sont utilisés, et que toutes ces données sont parfaitement entassées au début du fichier, le *shrink* déplacera **0 page**. Il se contentera de couper la fin du fichier.
* **Le scénario catastrophe :** Si les 50 Go de données sont éparpillés jusqu'à la toute fin du fichier de 100 Go, le moteur devra lire et réécrire une quantité massive de pages pour tasser les données vers le début.

Vous ne pouvez pas facilement compter les pages qui seront déplacées sans analyser physiquement l'emplacement de chaque extension, mais vous pouvez **estimer l'espace libre et l'espace utilisé** en pages (1 page = 8 Ko) pour connaître la limite théorique.

Utilisez cette requête pour analyser vos fichiers :

```sql
SELECT 
    name AS NomFichier,
    physical_name AS CheminPhysique,
    size AS TailleTotale_Pages,
    CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) AS PagesUtilisees,
    size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT) AS PagesVides,
    (size/128.0) AS TailleTotale_MB,
    (CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)/128.0) AS EspaceUtilise_MB,
    ((size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT))/128.0) AS EspaceVide_MB
FROM sys.database_files;
```

Si vous décidez de réduire la taille du fichier au plus près de l'`EspaceUtilise_MB`, la différence entre l'emplacement de la dernière page allouée et la taille cible déterminera le volume de pages à déplacer.

---

### ⚠️ Conseils de prudence (Rappel amical)

Puisque nous parlons de *shrink*, il est de mon devoir de vous rappeler quelques réalités de cette opération :

* **Fragmentation massive :** Le *shrink* (contrairement à la reconstruction d'index) déplace les pages dans le désordre pour combler les trous. Cela génère une fragmentation quasi-totale (proche de 100%) de vos index.
* **Activité du journal de transactions :** Chaque mouvement de page est loggué. Un *shrink* lourd va faire gonfler votre fichier journal (`.ldf`), ce qui peut annuler le gain d'espace si le journal est sur le même disque.
* **Croissance future :** Si la base est amenée à regrossir dans les mois qui viennent, réduire le fichier est contre-productif, car SQL Server devra à nouveau allouer de l'espace sur le disque (ce qui coûte en performances).

Si vous devez absolument le faire (par exemple, après avoir purgé des millions de lignes d'historique que vous ne remplacerez pas), il est souvent recommandé de le faire par **petits incréments** (par exemple, réduire de 5 Go en 5 Go) plutôt que de lancer une énorme opération d'un coup, afin de pouvoir l'interrompre proprement si nécessaire.

---

# you asked

Detaille le fonctionnement de https://dbatools.io/Invoke-DbaDbShrink/

---

# gemini response

La commande `Invoke-DbaDbShrink` fait partie du célèbre module PowerShell **dbatools**. C'est un "wrapper" (une surcouche intelligente) autour des commandes natives SQL Server `DBCC SHRINKDATABASE` et `DBCC SHRINKFILE`. 

Au lieu de simplement lancer un *shrink* brut (qui est souvent une opération risquée et aveugle en T-SQL), `Invoke-DbaDbShrink` ajoute des mécanismes de contrôle, de mesure et de sécurité très prisés par les administrateurs de bases de données (DBA).

Voici comment cette commande fonctionne en détail et ce qui la rend supérieure à une simple requête SQL.

### 1. Le fonctionnement en coulisses (L'algorithme de la commande)

Lorsque vous lancez `Invoke-DbaDbShrink`, le script PowerShell effectue plusieurs actions séquentiellement :

1. **Mise à jour des statistiques d'espace (`DBCC UPDATEUSAGE`) :** Par défaut, la commande commence par forcer la mise à jour des compteurs d'espace de la base de données. Cela garantit que les calculs d'espace libre et utilisé sont parfaitement exacts avant de prendre la moindre décision (désactivable via `-ExcludeUpdateUsage`).
2. **Analyse de la fragmentation initiale :** Avant de toucher aux fichiers, l'outil interroge les vues de gestion dynamique (DMV) pour mesurer la fragmentation moyenne et maximale de vos index. Cela servira de point de comparaison (désactivable via `-ExcludeIndexStats` pour gagner du temps).
3. **Exécution du Shrink :** La commande génère et exécute dynamiquement le code T-SQL nécessaire en fonction des paramètres que vous avez choisis (voir ci-dessous).
4. **Analyse de la fragmentation finale :** Une fois l'opération terminée, l'outil recalcule la fragmentation de vos index pour vous montrer l'impact exact du *shrink* (qui, généralement, fragmente massivement les données).
5. **Génération d'un rapport :** La commande renvoie un objet PowerShell complet listant ce qui a été fait, l'espace gagné et le temps écoulé.

### 2. Les fonctionnalités et paramètres clés

C'est ici que `Invoke-DbaDbShrink` brille vraiment par rapport au SQL natif :

* **Le "Chunking" ou découpage (`-StepSize`) :** C'est la fonctionnalité la plus importante. Si vous demandez de réduire un fichier de 50 Go d'un coup, SQL Server va générer une transaction massive, saturer le CPU, potentiellement bloquer d'autres requêtes et faire exploser la taille du journal des transactions (T-Log).
  Avec `-StepSize 25MB` ou `-StepSize 1GB`, `dbatools` va effectuer le *shrink* par petits morceaux (boucles successives). Cela réduit la pression sur le système, permet de suivre la progression via une barre de progression PowerShell, et évite la saturation du journal.

* **Ciblage de l'espace libre (`-PercentFreeSpace`) :**
  Au lieu de vider complètement le fichier et de laisser 0 octet d'espace libre, vous pouvez lui dire `-PercentFreeSpace 15`. L'outil calculera dynamiquement à quelle taille réduire le fichier pour qu'il conserve 15% d'espace libre, permettant ainsi à la base de respirer et de grossir un peu sans déclencher d'auto-growth immédiat.

* **Ciblage des fichiers (`-FileType`) :**
  Vous pouvez cibler spécifiquement les fichiers de données (`Data`), les fichiers journaux (`Log`), ou les deux (`All`). C'est très utile pour ne réduire que le fichier `.ldf` après un gros incident, sans toucher aux données.

* **Méthodes de réduction (`-ShrinkMethod`) :**
  Il supporte les options natives :
  * `TruncateOnly` : Coupe simplement l'espace vide à la fin du fichier et le rend à l'OS. Cela ne déplace aucune page de données, c'est instantané et cela ne crée aucune fragmentation.
  * `EmptyFile` : Vide un fichier de données pour pouvoir le supprimer (déplace son contenu vers les autres fichiers du même groupe).
  * `NoTruncate` : Tasse les données au début du fichier mais ne rend pas l'espace à l'OS.

* **Gestion des blocages (SQL Server 2022+) :**
  Si vous êtes sur une instance récente, vous pouvez utiliser `-WaitAtLowPriority`. Au lieu d'entrer en conflit avec les utilisateurs actifs pour verrouiller des objets, le *shrink* patientera en arrière-plan avec une priorité basse. Vous pouvez même lui dire de s'auto-annuler si l'attente est trop longue (`-AbortAfterWait Self`) ou de tuer les sessions bloquantes (`Blockers`).

### 3. Ce que l'outil vous retourne (Outputs)

À la fin de l'opération, au lieu d'un simple message "Commande réussie", `Invoke-DbaDbShrink` renvoie un objet PowerShell extrêmement riche contenant (entre autres) :
* `InitialSize` et `FinalSize` (Taille avant/après).
* `InitialAvailable` et `FinalAvailable` (Espace vide avant/après).
* L'heure de début, de fin, et la durée totale (`Elapsed`).
* `InitialAverageFragmentation` et `FinalAverageFragmentation` (Montrant visuellement à quel point le *shrink* a détruit l'organisation de vos index).

### En résumé

`Invoke-DbaDbShrink` transforme une opération lourde, aveugle et dangereuse en un processus contrôlé, morcelé, mesurable et automatisable. C'est aujourd'hui la méthode recommandée par la communauté d'administration SQL si vous êtes **absolument obligé** de faire un *shrink* de vos fichiers de base de données.

---

# you asked

Y a-t-il une bonne façon d'estimer le stepsize Le plus efficace?

---

# gemini response

C'est la véritable question complexe. Il n'existe pas de formule mathématique universelle pour le `StepSize`, car ce qui est "efficace" n'est pas seulement ce qui va le plus vite, mais ce qui **maintient la stabilité de l'instance** pendant l'opération.

Lorsqu'on conçoit des routines d'archivage massif ou de maintenance lourde de bases de données, le choix du *step size* se résume en réalité à un exercice d'équilibrage entre la pression sur les I/O, le verrouillage, et surtout, la gestion du journal de transactions (Write-Ahead Logging).

Voici la méthode architecturale pour estimer la taille de bloc la plus efficace pour votre environnement.

### 1. Le goulot d'étranglement principal : Le T-Log

C'est la règle d'or du *shrink* : **le déplacement des pages est entièrement loggué**. 
Déplacer 1 Go de données depuis la fin du fichier vers le début va générer au minimum 1 Go (souvent un peu plus à cause de l'overhead) d'écritures dans le fichier `.ldf`.

Si vous choisissez un `StepSize` trop grand, voici ce qui se passe :
1. La transaction devient massive.
2. Le journal des transactions se remplit plus vite que le processus de *Checkpoint* (en mode SIMPLE) ou que vos sauvegardes de log (en mode FULL) ne peuvent le vider.
3. Le `.ldf` subit un *Auto-Growth*, créant de nouveaux VLF (*Virtual Log Files*), et potentiellement une fragmentation physique du journal au niveau du stockage.
4. L'opération s'effondre en performances à cause des attentes `WRITELOG`.

**L'approche d'estimation :** Le `StepSize` idéal doit générer un volume de transaction inférieur à la quantité d'espace libre actuel dans votre journal, ou au moins inférieur à la taille de votre *Auto-Growth* du journal, pour laisser le temps au mécanisme de troncature de faire son travail entre deux boucles.

### 2. L'Overhead vs. Le Débit I/O

Il y a un point de bascule capacitaire à trouver :
* **Trop petit (ex: 5 Mo ou 10 Mo) :** L'overhead de l'exécution de la commande, de l'analyse des pages, et des allers-retours de la boucle d'exécution (que ce soit via PowerShell ou une boucle `WHILE` en T-SQL) consomme plus de CPU que le déplacement de données lui-même.
* **Trop grand (ex: 5 Go ou 10 Go) :** Le sous-système disque est saturé par les files d'attente de lecture/écriture, générant des attentes `PAGEIOLATCH_EX` intenses qui vont impacter toutes les autres requêtes sur la même LUN/grappe de disques.

### 3. Comment calibrer le StepSize (Méthodologie)

Puisque les systèmes de stockage varient (du vieux SAN aux baies NVMe direct-attach), la meilleure méthode reste empirique, mais guidée :

**Étape A : Établir une ligne de base (La zone refuge)**
En général, la communauté s'accorde pour dire que le point de départ sécuritaire se situe entre **50 MB et 500 MB**. 
* **250 MB** est souvent considéré comme le *sweet spot* pour démarrer sur du matériel moderne. Cela déplace environ 32 000 pages par itération, ce qui est traité en quelques secondes sans saturer le tampon.

**Étape B : Le test de charge (Monitorer les Wait Stats)**
Lancez le processus avec un `StepSize` de 250 MB et surveillez vos DMV :
```sql
-- Surveillez la croissance du journal pendant l'itération
SELECT database_id, log_space_in_bytes_since_last_backup 
FROM sys.dm_db_log_stats(DB_ID());
```
Si vous remarquez que vos attentes `WRITELOG` ou `PAGEIOLATCH_EX` s'envolent au-delà de 20-30 ms de latence moyenne, votre I/O ne suit pas, il faut réduire le *step size* (ex: 100 MB).

**Étape C : Ajuster en fonction du mode de récupération**
* **Mode SIMPLE :** Vous pouvez vous permettre un `StepSize` légèrement plus grand (ex: 500 MB), car le *checkpoint* implicite à la fin de chaque "chunk" tronquera le journal presque immédiatement.
* **Mode FULL :** Le journal ne se vide qu'à la sauvegarde du log. Si votre sauvegarde tourne toutes les 15 minutes, vous devez calculer combien de gigaoctets de *shrink* votre disque de log peut encaisser en 15 minutes sans saturer.

---

Êtes-vous en train de calibrer ces paramètres pour une tâche de maintenance ponctuelle, ou cherchez-vous à standardiser cette valeur pour l'intégrer dans un utilitaire d'archivage et de maintenance automatisé récurrent ?