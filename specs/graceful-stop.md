# Spec métier — Arrêt en douceur (drain) après l'instruction en cours

> **Statut : v1 implémenté (drain sans curseur `State`).** Le moteur expose `WithDrainSignal(<-chan
> struct{})` ; une fois le canal **fermé** (signal latché), il finit l'op en cours puis s'arrête
> **avant la suivante** (`finalizeDrained` : manifeste laissé en `02.processing/`, compté
> `Interrupted`), et n'entame plus les manifestes restants. Déclencheurs : **Ctrl+C 1× = drain / 2× =
> hard stop** (handler `os/signal` dans `cmd/sqlgopace/main.go`, `sync.Once` pour fermer le canal une
> fois, 2ᵉ signal → `cancelRun()`), et l'action TUI **`d`** (`ActionDrain` → statut `DRAINING`).
> Reprise : le **curseur d'opération** `State.ResumeFromOp` (§3.3.1) est écrit par `finalizeDrained`
> (= nb d'ops faites) ; la recovery **conserve** le sidecar au requeue quand le curseur > 0
> (`requeue(..., keepCursor)` + `queue.InToRun` pour tolérer un manifeste déjà re-enfilé) ; au re-run
> `writeSidecar` **préserve** le curseur et le retourne, et la boucle **saute** les ops `i < curseur`
> (outcome `skipped`, raison « already done in a previous run ») — via le helper partagé
> `recordSkipped` (mutualisé avec `skip_if_satisfied`). Le curseur est désormais **aussi écrit
> progressivement par opération** (`advanceCursor`, `crash-resumable.md` §6), pas seulement au drain :
> un *crash* renseigne donc le curseur comme un drain, et `WriteState` est rendu **atomique**
> (temp+rename) puisque le sidecar est réécrit après chaque op. Le **skip métadonnée** (`crash-resumable.md`
> §9) reste complémentaire (rend le préfixe compression bon marché). **Non implémenté (§3.2, §6)** : le
> drain **par chunk** pendant un shrink ; annuler un drain. Créé le 2026-06-17, suite au besoin d'arrêter
> un run **sans avorter l'opération en cours**.

## 1. Objectif

Offrir une commande (TUI, et idéalement Ctrl+C) qui **arrête le traitement à la fin de
l'instruction/opération en cours**, au lieu de l'interrompre brutalement :

- l'**opération en cours va jusqu'au bout** (pas de rollback, pas de travail perdu) ;
- le moteur **ne démarre pas l'opération suivante** ;
- il **enregistre le point de reprise** pour que le prochain run **continue à l'op suivante**,
  pas depuis le début.

C'est le chaînon manquant entre les deux leviers actuels : `pause` (suspend *au milieu* d'une
instruction resumable) et `kill` (avorte l'instruction, rollback). Le **drain** se place *entre*
deux opérations.

## 2. État actuel observé

- **Boucle d'exécution** : `for i, step := range planned` (`internal/run/engine.go:286`) ; le moteur
  connaît `i`, `len(planned)` et démarre chaque op sans point d'arrêt négociable entre elles.
- **Leviers TUI existants** : `kill DDL`, `kill blocker`, `pause`, `extend`
  (`internal/tui/model.go` ~95-113 ; aide en bas d'écran `model.go:244-245`). **Pas de drain.**
- **Pas de handler de signal** : Ctrl+C tue le process net (cf. `crash-resumable.md §3.1`).
- **Pas de curseur d'opération** dans l'état persistant (`internal/run/state.go:12-20`).
- **Recovery = re-jeu depuis le début** : un manifeste laissé dans `02.processing/` est ré-enfilé
  et relancé op 1 (`internal/run/recovery.go:174-184`).
- **Compteur déjà présent** : `Summary.Interrupted` + message « interrupted manifest(s) left in
  processing; the next run will resume them » (`cmd/sqlgopace/main.go:283`) — un manifeste drainé
  s'y range naturellement.

Donc aujourd'hui : arrêter = avorter l'op en cours (rollback offline) **et** tout refaire au run
suivant. Le drain supprime ces deux pertes.

## 3. Conception proposée

### 3.1 Déclencheurs

- **Action TUI `drain`** : une nouvelle touche (p. ex. `d`) qui pose un flag « stop after current
  op ». Le moteur le vérifie **en tête de boucle**, avant de démarrer l'op suivante
  (`engine.go:286`). Affichage : statut `DRAINING — s'arrêtera après l'op 31/74`.
- **Ctrl+C = drain (recommandé)** : installer enfin un handler de signal (cf. `crash-resumable.md
  §3.1`) avec la sémantique **1× = drain** (arrêt propre après l'op en cours), **2× = hard stop**
  (annulation immédiate, comportement actuel). UX standard et sûre par défaut.

### 3.2 Granularité

- Cas normal (`rebuild_index`, etc.) : on s'arrête **après l'opération courante** (un step).
- **Shrink** : un step = une boucle multi-chunks ; le driver échantillonne déjà entre chunks
  (`ShrinkRunner`). Le drain s'y traduit par « s'arrêter **après le chunk courant** » — le re-jeu
  repart de l'espace libre courant (déjà idempotent).

### 3.3 Où enregistrer le point de reprise

Deux options ; **ne pas muter le manifeste** (entrée déclarative de l'utilisateur ; réécrire le
YAML est risqué et lossy) :

1. **Étendre le sidecar `State`** (recommandé) — il existe déjà, vit à côté du manifeste dans
   `02.processing/`, et est **déjà lu par la recovery**. Ajouter un **curseur** :
   `ResumeFromOp int` (prochaine op à exécuter) + `CompletedOps int` + `Reason` (« drain demandé à
   l'op 31/74 le … »). C'est exactement le « curseur d'opération » évoqué en `crash-resumable.md
   §6`, ici avec un déclencheur **intentionnel** (pas un crash).
2. **Fichier de contrôle de vol dédié** (alternative que tu proposais) : un
   `<manifeste>.flight.json` à côté. Plus explicite/lisible isolément, mais **duplique** le rôle du
   sidecar `State` et ajoute un fichier à gérer dans le cycle de file. À retenir seulement si on
   veut un format de reprise indépendant de `State`.

→ Recommandation : **réutiliser `State`** (moins de surface, déjà câblé recovery), sauf besoin
explicite d'un artefact séparé.

### 3.4 Reprise

La recovery (`recovery.go`) honore le curseur : si `ResumeFromOp` est posé, **continuer à cette
op** au lieu de re-jouer depuis le début. Se combine avec le **skip par métadonnée**
(`crash-resumable.md §9`) : même si le curseur manquait, les ops déjà faites seraient sautées.

### 3.5 Nouvel état terminal

Un manifeste drainé **reste dans `02.processing/`** (ni `done`, ni `failed`) et compte comme
**`Interrupted`** (réutilise le compteur existant, `main.go:283`), avec un message clair :
« drainé à l'op 31/74 — reprise au prochain run ».

## 4. Différence avec les leviers existants

| Levier | Sur l'instruction en cours | Reprise | Travail perdu |
|---|---|---|---|
| `pause` (existant) | suspend *au milieu* (resumable) | même run, via `RESUME` | aucun (online/resumable) |
| `kill DDL` (existant) | **avorte** (rollback offline) | run suivant, **depuis le début** | l'op en cours |
| **`drain` (proposé)** | **laisse finir** | run suivant, **à l'op suivante** | **aucun** |

## 5. Liens avec les autres itérations

- **`progress-tui.md`** : le drain a besoin de `i`/`N` (déjà exposés par le *step-sink* proposé) pour
  afficher « s'arrêtera après l'op i/N » et écrire le curseur.
- **`crash-resumable.md`** : le drain **matérialise** le « curseur d'opération » (§6) avec un
  déclencheur volontaire, et profite du **skip métadonnée** (§9) à la reprise. Concevoir les deux
  ensemble (même champ `ResumeFromOp` dans `State`).
- **`remote-tui.md`** : `drain` devient une `Action` diffusable — un client distant peut demander un
  arrêt propre (avec les mêmes garde-fous de sécurité que `kill`).

## 6. Questions ouvertes

- **Sémantique Ctrl+C** : 1× drain / 2× hard — ou réserver le drain au TUI et garder Ctrl+C = hard ?
- **Annuler un drain** : permettre de revenir en arrière (« finalement, continue ») avant que l'op
  courante se termine ?
- **`State` vs fichier dédié** (§3.3) : trancher selon qu'on veut un artefact de reprise autonome.
- **Drain pendant un shrink** : s'arrêter après le chunk courant suffit-il, ou faut-il un sous-curseur
  de chunk persistant ?
- **Multi-base** (§17) : un drain arrête-t-il le manifeste courant seulement, ou toute la file ?

## 7. Estimation d'effort

**Petit-moyen.** Le moteur a déjà la boucle, l'index `i`, et un point naturel de vérification entre
ops ; `State` et la recovery existent. À écrire : l'action/flag `drain`, le handler de signal
(optionnel mais souhaitable), l'extension de `State` (curseur) + sa prise en compte par la recovery,
et l'affichage. Le plus gros est partagé avec `crash-resumable.md` (le curseur).

## 8. Références code (au 2026-06-17)

| Sujet | Emplacement |
|---|---|
| Boucle d'ops (point de vérification du flag drain) | `internal/run/engine.go:286` |
| Actions/touches TUI (où ajouter `drain`) | `internal/tui/model.go` ~95-113 ; `model.go:244-245` |
| État persistant à étendre (curseur de reprise) | `internal/run/state.go:12-20` |
| Recovery à rendre « curseur-aware » | `internal/run/recovery.go:174-184` |
| Compteur `Interrupted` réutilisable | `cmd/sqlgopace/main.go:283` |
| Absence de handler de signal (à ajouter pour Ctrl+C) | `specs/crash-resumable.md §3.1` |
| Curseur d'opération (idée sœur) + skip métadonnée | `specs/crash-resumable.md §6, §9` |
