# TEMPDB-GUARD — garde-fou tempdb (alerte + arrêt auto-attribué)

> **DRAFT** — source de vérité du comportement visé pour la surveillance de tempdb.
> Mini-spec transversale : sert **tous** les drivers (rebuild `SORT_IN_TEMPDB`, shrink, DML par
> lots), pas seulement le DML. Rien n'est codé.

## 1. Objectif et contexte

tempdb est une ressource **partagée par toute l'instance** : la remplir ne casse pas que la base
cible, ça casse *toutes* les bases. Le rayon de souffle justifie un garde-fou même si l'événement est
rare. Plusieurs opérations de SqlGoPace **consomment** tempdb : tri d'un rebuild `SORT_IN_TEMPDB`,
spills de tri/hash d'un gros DML découpé (`specs/BATCH-DML.md`), et — sous RCSI — le version store
généré par un UPDATE/DELETE par lots.

Le piège central est l'**attribution** : la surveillance du journal de la base utilisateur marche
parce que la pression journal *est* surtout causée par notre DDL ; la pression tempdb, **souvent
non**. Si un *tiers* remplit tempdb, mettre **notre** opération en pause **ne libère rien** — on
s'auto-pénalise pour la faute d'un autre. Ce garde-fou résout ça en **mesurant notre propre usage**
de tempdb avant de décider d'arrêter.

> Principe directeur : **alerter toujours, arrêter seulement quand c'est nous** (et donc quand
> s'arrêter soulage réellement tempdb).

## 2. Trois postures, par sévérité croissante

1. **Preflight — ne pas démarrer.** Si tempdb est **déjà** au-dessus du seuil au moment du preflight,
   l'op échoue proprement (routée en `04.failed`, aucun verrou pris) avec un message actionnable.
   Étend `internal/preflight` sur le modèle de `CheckLog`.
2. **Runtime — alerte (TUI + log), sans arrêter.** Dès que tempdb franchit un seuil pendant un run,
   on émet un **incident informatif** vers le TUI et le `.log`. C'est de l'information, pas une
   décision : ça ne ment jamais et ne s'auto-pénalise jamais. Coût = une lecture DMV de plus sur la
   cadence d'échantillonnage existante.
3. **Runtime — arrêt conditionné à l'auto-attribution.** On escalade vers un **stop** (pause si
   resumable → cancel) **uniquement** si **les deux** sont vrais :
   - tempdb est au-dessus du cap, **et**
   - **notre** session d'exécution est un contributeur **matériel** (cf. §3).

   Sinon (pression tempdb mais ce n'est pas nous) → **alerte seulement**.

## 3. Détection (DMV, lectures — toutes bon marché)

- **Notre usage** : `sys.dm_db_session_space_usage` filtré sur le **SPID d'exécution** (le moteur le
  connaît déjà — c'est celui du KILL et des waits par session). Le **net détenu** =
  `(user_objects_alloc − user_objects_dealloc) + (internal_objects_alloc − internal_objects_dealloc)`
  pages × 8 Ko. C'est le signal *actionnable* (ce qu'on tient maintenant, pas le cumulatif), et il
  capte exactement les cas où s'arrêter **libère** : tri `SORT_IN_TEMPDB`, spill de tri/hash.
- **Vue globale tempdb** : `sys.dm_db_file_space_usage` (extents alloués/libres + `version_store_`,
  `internal_objects_`, `user_objects_reserved_page_count`) et les tailles de fichiers tempdb. Donne
  le « % de remplissage » comparé au cap, données + journal de tempdb.
- **Contributeur matériel** = net détenu par notre SPID au-dessus d'un seuil absolu (Mo) **ou** d'une
  part du tempdb utilisé (configurable). Défaut prudent : un seuil absolu en Mo.

`VIEW SERVER STATE` (déjà exigé pour le monitoring, cf. `docs/e2e.md`) suffit pour ces lectures.

## 4. L'angle mort assumé : le version store (RCSI)

Le cas RCSI / DML découpé est précisément celui où l'attribution par session **casse**, et on
l'assume :

- Le version store **n'est pas** compté dans `dm_db_session_space_usage` (ce sont des objets
  internes/utilisateur, pas les versions). Les versions sont attribuées à la *transaction* et
  épinglées par les *lecteurs* ; il n'existe pas de DMV **bon marché** par-session qui dise « tu as
  généré X Mo de version store ».
- Mais nos lots **commitent un par un** : *notre* transaction n'épingle quasi rien. Un version store
  qui gonfle est tenu par un **lecteur long**, pas par nous → **s'arrêter ne le libère pas**.

Donc pour le version store : **alerte seulement**, jamais d'arrêt auto. C'est cohérent avec le
principe (§1) — si ce n'est pas notre faute *actionnable*, on n'arrête pas. On ne prétend pas
attribuer ce qu'on ne peut pas attribuer proprement.

## 5. Intégration (réutilise la plomberie existante)

- **Nouvelle dimension de pression** dans `internal/run/reaction.go` :
  `Pressure.TempdbOverCap bool` + `Pressure.TempdbSelfDriven bool`. La décision de réaction reste
  inchangée pour le reste ; on ajoute : *alerter toujours sur `TempdbOverCap` ; n'escalader vers stop
  que si `TempdbOverCap && TempdbSelfDriven`*. (Même esprit que `BlockingOthers` + filtrage.)
- **Échantillonnage** : la lecture tempdb (globale + notre SPID) se branche sur `pumpSamples`
  (`internal/run/executor.go`), sur sa propre cadence comme le journal — un `TempdbSample` parallèle à
  `LogSample`. Couvre donc `MonitoredRunner` **et** `ShrinkRunner` **et** le futur `BatchDMLRunner`
  sans code par-driver.
- **Alerte** : surfacée comme un **incident informatif** via le canal déjà utilisé pour la narration
  (sink/feed moteur → TUI incident console + `.log`), avec un *kind* distinct (`tempdb`) pour qu'il se
  lise comme un avertissement, pas comme une réaction destructive.
- **Stop auto-attribué** : passe par la hiérarchie de réaction existante (pause resumable → cancel) ;
  un cancel/rollback d'un tri rend les pages internes → soulage réellement tempdb.

## 6. Câblage (par couche)

- **`internal/mssql`** — nouvelles lectures : `TempdbSpace(ctx) (TempdbSpace, error)` (global, via
  `sys.dm_db_file_space_usage` + tailles fichiers) et `SessionTempdbMB(ctx, spid int) (int, error)`
  (net détenu, via `sys.dm_db_session_space_usage`). Exposées à `internal/run` derrière une interface
  étroite (modèle `ShrinkReader`).
- **`internal/run/executor.go`** — `TempdbSample` + lecture dans `pumpSamples` ; calcul de
  `TempdbOverCap`/`TempdbSelfDriven` selon les seuils.
- **`internal/run/reaction.go`** — les deux champs `Pressure` + la règle « alerte vs stop ».
- **`internal/preflight/preflight.go`** — `CheckTempdb(space, cap)` sur le modèle de `CheckLog`
  (Fail si déjà au-dessus, Warn proche du seuil).
- **`internal/config/config.go`** — `TempdbConfig` : `preflight_max_*`, `alert_max_*`,
  `self_contribution_min_mb` (le seuil « matériel »), cadence de poll tempdb.
- **TUI / report** — afficher l'incident tempdb (TUI) et l'écrire dans le `.log`.

## 7. Phasage

- **It1.** Lectures `mssql` (global + par SPID) ; **preflight no-start** ; **alerte runtime** TUI/log
  sur seuil. Pas d'arrêt auto. Couvre déjà l'essentiel de la valeur (visibilité + ne pas aggraver).
- **It2.** **Stop auto-attribué** : `TempdbSelfDriven` via `sys.dm_db_session_space_usage`, escalade
  conditionnelle dans la réaction (pause/cancel) quand c'est nous le contributeur matériel.
- **It3 (option).** Affiner le « matériel » en part du tempdb utilisé plutôt qu'absolu ; surfacer la
  ventilation (objets utilisateur vs internes vs version store) dans l'incident.

## 8. Tests (sans base ; `-race`)

- **preflight :** `CheckTempdb` Fail au-dessus du cap, Warn proche, Pass sinon (table-driven, comme
  `CheckLog`).
- **run (pur) :** la règle de réaction — `TempdbOverCap` seul ⇒ **alerte, pas de stop** ;
  `TempdbOverCap && TempdbSelfDriven` ⇒ stop (pause si resumable, sinon cancel) ; ni l'un ni l'autre
  ⇒ rien. Calcul du net détenu par SPID à partir de compteurs alloc/dealloc simulés.
- **mssql :** les deux requêtes derrière le tag `integration`.

## 9. Limites (délibérées, documentées)

1. **Version store non attribuable par-session bon marché** → alerte seulement pour ce cas (§4).
2. **Le seuil « matériel » est heuristique** : trop bas, on s'arrête pour peu ; trop haut, on rate
   notre propre dérive. Configurable, défaut prudent en Mo absolus.
3. **Course d'échantillonnage** : la pression tempdb est lue sur une cadence ; une dérive très rapide
   entre deux échantillons peut n'être vue qu'au tick suivant — acceptable (rare, et le preflight
   couvre l'état initial).
4. **Multi-sessions** : on n'attribue qu'au **SPID d'exécution** ; une éventuelle session annexe
   (monitoring) ne consomme pas de tempdb significatif, donc ignorée.
