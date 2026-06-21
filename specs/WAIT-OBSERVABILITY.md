# WAIT-OBSERVABILITY — suivi des attentes de notre opération (TUI live + log)

> **DRAFT** — source de vérité du comportement visé pour l'observabilité des attentes provoquées par
> l'opération en cours, via `sys.dm_exec_session_wait_stats`. Rien de neuf n'est codé ; **une grande
> partie existe déjà** (cf. §3) — ce document cadre surtout le **panneau TUI live**.

## 1. Objectif et contexte

Une opération exigeante (rebuild, compression, shrink, DML par lots) **attend** : sur des verrous, des
I/O de données, le flush du journal, le parallélisme, la mémoire, tempdb… Exposer **ce qui ralentit
notre session**, en temps réel dans le TUI et en synthèse dans le `.log`, aide l'opérateur à
**comprendre** un run et à décider *à la main* (prolonger l'attente, pause, kill).

Décision de cadrage (cf. §2) : c'est de l'**observabilité**, **pas** un nouveau signal de réaction.

## 2. Posture : information, pas alerte ni réaction automatique

Les attentes sont **diagnostiques** (le « pourquoi »), pas **prescriptives** (le « quoi faire ») :

- Les signaux qui méritent vraiment une réaction — **blocage** et **pression journal** — ont déjà des
  lectures **dédiées et précises** (sampler de blocage sur `BlockingSPID`, sampler d'espace log).
  Réagir à partir des wait stats **agrégées** serait moins précis et **redondant**.
- Le seul usage légitime « wait → action », le **throttle adaptatif** (WRITELOG / PAGEIOLATCH_EX →
  réduire le pas du shrink, le lot du DML), **existe déjà** per-driver (`shrink_calc.go` ;
  `batch_calc.go` dans `BATCH-DML.md`). On **ne le généralise pas** en moteur d'alerte.
- Donc : le panneau d'attentes **alimente la décision humaine** dans le TUI ; il ne déclenche **rien**
  de lui-même.

> Si une attente devait un jour piloter une réaction, ce serait via une **lecture dédiée** de ce
> signal précis, pas via ce panneau agrégé.

## 3. Ce qui existe déjà vs ce qui manque

**Déjà en place (à réutiliser tel quel) :**

- `internal/mssql/waits.go` : `SessionWaits(ctx, spid)` lit `sys.dm_exec_session_wait_stats` (2016+ ;
  best-effort, « pas de données » sur serveur plus ancien).
- `CategorizeWaits` : jeu **curé et ordonné** de catégories utiles (Locking, Data I/O, Transaction
  log, Parallelism, Memory, CPU & scheduling, Page latch (tempdb), Sort & spill, AG, Backup), le bruit
  étant écarté ; tri par temps décroissant + total.
- `DiffWaits(before, after)` : delta par type d'attente (le DMV est cumulatif pour la session).
- `engine.go` `snapshotWaits`/`operationWaits` : capture avant/après et **écrit déjà le résumé
  d'attentes par opération dans le `.log`** (`report.WaitLine`).

**Ce qui manque (l'objet de cette spec) :**

- Un **panneau TUI live** : les catégories d'attentes de **notre SPID d'exécution**, en **delta
  glissant depuis le début de l'op**, rafraîchies sur la cadence d'échantillonnage — au lieu du seul
  avant/après de fin d'op.
- (Optionnel) un **surlignage *advisory*** d'une poignée d'attentes « notables » (info, jamais stop).

## 4. La fonctionnalité

### 4.1 Panneau TUI live

- À l'entrée d'une opération, capturer le snapshot d'attentes (réutilise `snapshotWaits`) comme
  **base**. À chaque tick d'échantillonnage, relire `SessionWaits(spid)`, faire `DiffWaits(base,
  now)` puis `CategorizeWaits(delta)`, et **pousser** le résultat au TUI.
- Le TUI affiche les **top catégories** (nom, temps d'attente cumulé depuis le début de l'op, nb de
  tâches), triées décroissant — exactement ce que `CategorizeWaits` renvoie déjà. Mise à jour en place.
- C'est une **information de contexte** à côté des panneaux existants (progression, sessions
  bloquées) ; elle aide l'opérateur à choisir une action TUI (`extend` / `pause` / `kill`).

### 4.2 Synthèse log (déjà présente, à confirmer)

- Le `.log` continue de porter le **résumé d'attentes par opération** (catégories + total) via
  `operationWaits`. Aucun changement requis ; au plus, s'assurer que le **max** observé en cours (pic)
  est conservé si jugé utile.

### 4.3 (Optionnel, It2) surlignage advisory

Un petit ensemble curé d'attentes mérite un **highlight** visuel (couleur/pastille « ⚠ info »), sans
jamais déclencher d'action :

- `RESOURCE_SEMAPHORE` / `RESOURCE_SEMAPHORE_QUERY_COMPILE` — famine de grant mémoire ;
- `THREADPOOL` — famine de workers (problème **instance**, pas seulement nous) ;
- `PAGELATCH_*` (catégorie « Page latch (tempdb) ») — contention d'allocation tempdb → **lien avec
  `TEMPDB-GUARD.md`**.

Ce sont des repères **pour l'humain**, pas des alertes qui agissent.

## 5. Intégration / câblage

- **Aucune nouvelle lecture DMV** : tout passe par `SessionWaits`/`DiffWaits`/`CategorizeWaits`
  existants. On ne touche pas `internal/mssql`.
- **Flux moteur → TUI** : pousser le delta catégorisé via le **même canal** que les autres mises à
  jour TUI. Converge avec le **step-sink** introduit par `progress-tui.md` (`specs/TODO.md`) — à
  concevoir ensemble pour ne pas multiplier les canaux. En l'absence de TUI (`--tui` off), le panneau
  est simplement inactif ; le `.log` suffit.
- **Cadence** : réutiliser la cadence d'échantillonnage du run (le panneau n'a pas besoin d'être plus
  fréquent que les autres samples). Best-effort : un échec de lecture laisse le dernier état affiché.

## 6. Plancher de version

`sys.dm_exec_session_wait_stats` existe à partir de **SQL Server 2016**. Sur un serveur plus ancien,
`SessionWaits` renvoie « pas de données » (déjà géré) → le panneau reste **vide/masqué**, sans erreur.
Comportement identique au `.log` aujourd'hui.

## 7. Phasage

- **It1.** Panneau TUI live (delta glissant catégorisé de notre SPID) ; confirmation du résumé log.
  Aucune réaction.
- **It2 (option).** Surlignage advisory des attentes notables ; éventuel pic conservé au `.log`.

## 8. Tests (sans base ; `-race`)

- **run (pur) :** à partir de deux snapshots simulés, le delta glissant catégorisé poussé au TUI est
  correct (réutilise `DiffWaits`/`CategorizeWaits`, déjà testés — ici on teste le **streaming** et le
  choix de base = début d'op).
- **tui :** le modèle affiche les top catégories et se met à jour sur message ; vide quand pas de
  données (serveur < 2016).
- **Pas de test de réaction** : par conception, ce panneau n'en déclenche aucune.

## 9. Limites (délibérées)

1. **Diagnostic, pas prescriptif** : n'attendez pas du panneau qu'il « décide » — il informe l'humain.
2. **Périmètre = notre SPID d'exécution** : ce sont *nos* attentes, pas une vue serveur globale (ce
   n'est pas un remplacement d'un outil de monitoring d'instance).
3. **Cumulatif → delta** : le DMV est cumulatif ; tout l'intérêt est dans le **delta depuis le début
   de l'op** (sinon on mélange l'historique de la connexion).
4. **2016+** : invisible sur serveur plus ancien (best-effort), comme le résumé log actuel.
