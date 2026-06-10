# Formation Claude Code — Guide structuré pour formateur

> Support de conception d'une formation présentielle « Utiliser Claude Code pour le développement ».
> **Tout le matériel ci-dessous est tiré d'un projet réel** mené avec Claude Code : *SqlGoPace*, un
> orchestrateur DDL pour SQL Server écrit en Go. Le projet n'a **pas commencé par du code** mais par
> des spécifications, leur critique, leur durcissement, puis un plan d'implémentation — ce qui en
> fait un excellent fil rouge pour enseigner *comment on travaille avec un agent*, pas seulement
> *comment on génère du code*.

---

## 0. Pourquoi ce projet est un bon cas d'école

- **Greenfield** : on part de zéro, donc chaque étape est observable.
- **Domaine pointu** (verrouillage SQL Server, journal de transactions, options DDL par version) :
  parfait pour montrer **la force ET les limites** des connaissances du modèle.
- **Cycle complet sans écrire une ligne de code au départ** : lire → critiquer → décider →
  documenter → planifier. La majorité de la valeur d'un agent se joue *avant* la première fonction.
- **Le développeur reste aux commandes** : il corrige, tranche, exige de la qualité. C'est le
  message central de la formation.

### Objectifs pédagogiques

À la fin de la formation, les participants savent :

1. Mettre en place le **contexte** d'un projet (instructions, mémoire, références de fichiers).
2. Conduire un **workflow spec-driven** : faire produire, critiquer et durcir des specs par l'agent.
3. Utiliser l'agent comme **relecteur critique**, tout en **vérifiant** ses affirmations.
4. **Piloter** le modèle : donner des décisions, corriger avec autorité, lui faire poser les
   bonnes questions avant d'agir.
5. Exploiter l'**outillage** du harness : appels d'outils parallèles, questions structurées,
   skills/plugins, commandes slash.
6. Imposer des **standards de qualité** (idiomaticité, sécurité, cohérence) que l'agent applique.

### Public & prérequis

- Développeurs (tous langages). Les exemples sont en Go mais les principes sont transverses.
- Prérequis : savoir lire un diff, notions de Git, un terminal. Aucune connaissance préalable de
  Claude Code requise.

---

## 1. Le fil rouge du projet (à rejouer en démo)

Chronologie réelle de la session, utile pour reproduire la démonstration de bout en bout :

| Étape | Action du développeur | Ce que fait l'agent | Compétence enseignée |
|------:|------------------------|---------------------|----------------------|
| 1 | « Lis les specs et réfléchis aux améliorations » | Lit 3 fichiers **en parallèle**, produit une critique de fond | Cadrer une tâche d'analyse ; parallélisme |
| 2 | Répond point par point dans `reponses.txt` | Relit, valide/raffine, **signale des erreurs factuelles** | Dialogue itératif ; esprit critique mutuel |
| 3 | « Réécris SPECS.md, intègre versions.md, supprime-le. Décisions : a) YAML déclaratif seul b) matrice par min_version » | Réécrit, intègre, supprime, restructure | Donner des **décisions explicites** |
| 4 | Corrige une affirmation fausse + lien doc Microsoft | Corrige la matrice, supprime fichier obsolète, crée le vrai YAML | **Vérifier** le modèle ; corriger avec source |
| 5 | « Génère config.yaml et .env_example » | Crée les fichiers, **vérifie le .gitignore**, ajoute exclusions | Hygiène projet/sécurité proactive |
| 6 | Installe un marketplace de skills Go | (plugins rechargés) | Étendre l'agent via skills/plugins |
| 7 | « Planifie l'implémentation » (puis interruption : « tout en anglais ») | Crée une **mémoire projet**, un plan en 10 phases | Mémoire ; interruption/redirection |
| 8 | « Pose-moi les questions d'abord » | Pose 4 questions **structurées** à choix | Inverser le sens des questions |
| 9 | « Relis pour l'élégance » | Refactore le plan (sum types, packages, interfaces) | L'agent comme relecteur de lui-même |

> **Conseil formateur :** projeter cette table en ouverture. Elle montre que 8 étapes sur 9 ne
> produisent *aucun code applicatif* — et créent pourtant l'essentiel de la valeur.

---

## 2. Module 1 — Contexte & mémoire

### Concept clé
Un agent n'est performant que par le **contexte** qu'on lui donne. Trois leviers :

1. **Instructions projet/globales** (`CLAUDE.md`) : conventions, préférences, commandes maison.
   Dans notre session, des instructions globales injectaient l'email de l'utilisateur, la date du
   jour, et un proxy maison (RTK). Tout cela conditionne **chaque** réponse.
2. **Références de fichiers `@`** : `@specs/SPECS.md`, `@versions.md` tirent des fichiers dans le
   contexte sans copier-coller. Rapide, traçable.
3. **Mémoire persistante** : des faits durables, écrits dans des fichiers, rechargés à chaque
   session.

### Exemple tiré du projet
Quand le développeur a exigé « tout le code en anglais », l'agent a créé une **mémoire de type
`project`** :

```markdown
---
name: sqlgopace-english-only
description: SqlGoPace project — all code, comments, and files must be in English
metadata:
  type: project
---
For the SqlGoPace project, all source code, comments, and files must be in **English**…
```

…plus une entrée dans `MEMORY.md` (l'index rechargé en début de session). De même, les **4 décisions
v1** (schéma colonne minimal, expansion `index: ALL`, validation fail-fast, resumable uniforme) ont
été mémorisées pour ne pas être re-discutées la prochaine fois.

### Points de discussion
- Différence **mémoire** (durable, inter-sessions) vs **contexte de conversation** (éphémère).
- Quatre types de mémoire : `user`, `feedback`, `project`, `reference`. Quand utiliser lequel.
- Ce qu'il **ne faut pas** mémoriser : ce que le code/Git dit déjà.

### Exercice
> Demandez à l'agent de retenir une convention d'équipe (« on préfixe les branches par `feat/` »).
> Vérifiez le fichier de mémoire créé et l'entrée d'index. Ouvrez une nouvelle session, vérifiez
> que la convention est rappelée.

---

## 3. Module 2 — Le workflow *spec-driven* (docs-first)

### Concept clé
Le code est la *dernière* étape. On fait d'abord **converger une spécification** avec l'agent. Cela :
- expose les décisions d'architecture tôt (et à faible coût) ;
- crée une trace documentaire ;
- transforme l'agent en **partenaire de conception**, pas en simple générateur.

### Exemple tiré du projet
Le passage **d'une spec floue à une spec exécutable** :
- v0 : un `SPECS.md` en vrac + deux notes (`versions.md`, `Parser-Suggestion.txt`).
- L'agent critique, le développeur répond dans `reponses.txt`, l'agent intègre.
- Décision structurante prise *dans la conversation* : **abandonner le parsing de SQL brut** au
  profit d'un **DDL déclaratif en YAML**. Conséquence : tout le débat « quel parser SQL ? »
  (`Parser-Suggestion.txt`) **disparaît** → le fichier devient obsolète et est supprimé.

C'est l'illustration parfaite d'une bonne conception : **le meilleur code est celui qu'on n'a pas à
écrire**. La décision a été prise sur des specs, pas après 2 000 lignes de parser fragile.

### Démo (10 min)
1. Donner une spec volontairement incomplète.
2. « Lis et réfléchis profondément aux faiblesses et aux décisions à trancher. »
3. Itérer : répondre aux points, faire réécrire la spec.
4. Montrer comment une décision (déclaratif vs impératif) **élimine** des pans entiers de travail.

### Points de discussion
- « Think deeply / réfléchis longuement » change la profondeur de réponse.
- Garder les specs **synchronisées** (voir Module 7 : l'agent a détecté un `versions.md` périmé qui
  contredisait une décision validée).

---

## 4. Module 3 — L'agent comme relecteur critique… et faillible

### Concept clé
L'agent a une **vaste connaissance de domaine** (ici : internes SQL Server). C'est un atout
considérable pour challenger une conception. **Mais il se trompe sur les détails pointus.** Le
développeur reste l'autorité finale, **muni de sources**.

### Exemples tirés du projet (les deux faces)

**Force** — l'agent a apporté des éléments non triviaux et corrects :
- `KILL` ⇒ `ROLLBACK` coûteux ; intérêt d'**ADR** (Accelerated Database Recovery) pour rendre le
  rollback quasi instantané ;
- une **hiérarchie de réaction** RESUMABLE (PAUSE/RESUME) → WAIT_AT_LOW_PRIORITY → KILL ;
- la reprise d'opérations orphelines via `sys.index_resumable_operations` + corrélation
  `SPID + login_time + CONTEXT_INFO` (le SPID seul est réutilisé donc peu fiable) ;
- le piège de `used_log_space_in_percent` (pourcentage de la taille *courante* du fichier, pas du
  plafond) → surveiller l'**octet absolu**.

**Faillibilité** — le développeur a dû **corriger** l'agent, source à l'appui :
- `ALTER COLUMN ... ONLINE` existe depuis **2016**, pas 2022 (doc Microsoft) ;
- `WAIT_AT_LOW_PRIORITY` **n'est pas supporté** avec `ONLINE ALTER COLUMN`, *quelle que soit la
  version* (lien doc fourni) → l'option a été retirée de la matrice.

### Message à marteler
> **Trust, but verify.** Utilisez l'agent pour *générer des hypothèses de qualité expert*, puis
> validez les points critiques avec la documentation officielle. Une affirmation confiante n'est pas
> une preuve.

### Exercice
> Faire critiquer une conception par l'agent, puis demander aux participants d'**identifier une
> affirmation à vérifier** et de la confronter à la doc. Discuter de ce qu'on trouve.

---

## 5. Module 4 — Piloter le modèle

Trois techniques de pilotage observées, à enseigner explicitement.

### 4.1 Donner des **décisions**, pas des souhaits
Mauvais : « tu penses qu'on devrait faire du YAML ou du SQL ? »
Bon (ce qui a été fait) : « **a) On passe au YAML déclaratif seul. b) Restructure la matrice comme
tu l'as suggéré.** » → l'agent exécute sans tergiverser. **Le développeur tranche, l'agent réalise.**

### 4.2 Corriger avec **autorité et source**
« *The WAIT_AT_LOW_PRIORITY option can't be used with online ALTER COLUMN* » + lien doc. L'agent
intègre la correction dans la spec **et** dans le YAML **et** dans le rappel factuel — partout, de
façon cohérente.

### 4.3 **Inverser les questions** : « pose-moi les questions d'abord »
Plutôt que de laisser l'agent deviner sur des points ambigus, on lui demande de **lister les
décisions ouvertes** et de proposer des options. C'est l'une des techniques les plus rentables.

### Exemple tiré du projet
Sur demande, l'agent a posé **4 questions structurées à choix** (via un sélecteur, pas du texte
libre) :

1. Richesse du schéma de colonne → *Minimal d'abord*.
2. Gestion de `index: ALL` → *Expansion interne (une opération par index)*.
3. Moment de validation des manifestes → *Fail-fast au démarrage*.
4. Stratégie PAUSE/RESUME → *Uniforme via DMV*.

Chaque réponse a été **actée dans le plan ET en mémoire**. Résultat : zéro hypothèse implicite, des
choix tracés.

### Points de discussion
- Quand laisser l'agent décider seul (détails réversibles) vs forcer la question (choix
  structurants, coûteux à défaire).
- L'**interruption** est une commande de pilotage : ici, le développeur a interrompu la
  planification en cours pour ajouter « tout en anglais ». L'agent a redémarré proprement avec la
  contrainte.

---

## 6. Module 5 — L'outillage du harness

### 5.1 Appels d'outils **en parallèle**
Dès la première tâche, l'agent a lu **trois fichiers simultanément** (un seul tour, trois lectures).
Enseigner : pour des opérations **indépendantes**, l'agent peut paralléliser → plus rapide, moins de
tours.

### 5.2 Questions **structurées** (`AskUserQuestion`)
Au lieu d'un paragraphe de questions noyé dans du texte, l'agent présente des **cartes de choix**
cliquables (avec recommandation, descriptions, option « Autre »). Idéal pour des décisions nettes.
→ Démontré au Module 4.3.

### 5.3 Outils dédiés vs shell
L'agent privilégie des outils spécialisés (lecture, édition précise, recherche) au lieu de
`cat`/`grep`/`sed`. Bénéfices : permissions, liens cliquables, sûreté. Le shell reste pour ce qui
n'a pas d'outil (`rm`, `ls`).

### 5.4 Édition chirurgicale vs réécriture
Distinguer **`Edit`** (remplacement exact d'un fragment, idéal pour un correctif ciblé comme retirer
une ligne de la matrice) de **`Write`** (réécriture complète, ex. la refonte de `SPECS.md`).

### Exercice
> Donner une tâche qui touche 3 fichiers indépendants ; observer la parallélisation. Puis une tâche
> à choix multiples ; observer la question structurée.

---

## 7. Module 6 — Skills & plugins

### Concept clé
On **étend** les compétences de l'agent par des *skills* (procédures spécialisées) et des *plugins*
(paquets de skills/agents). Dans la session, l'utilisateur a ajouté un marketplace puis installé
**`golang-skills`** : un jeu de skills d'idiomaticité Go (naming, error-handling, concurrency,
interfaces, testing, documentation…).

### Pourquoi c'est puissant
Ces skills deviennent une **grille de qualité** que l'agent applique. Le plan d'implémentation y
fait explicitement référence comme guide de style. Au lieu d'espérer du « Go correct », on **branche
un standard**.

### Exemple tiré du projet
Le passage en revue « élégance » (Module 8) s'appuie directement sur ces principes : petites
interfaces, sum types, concurrence par communication. Les skills donnent un **vocabulaire commun** de
qualité.

### Points de discussion
- Skills natifs vs marketplace tiers (confiance, revue avant install).
- Commandes : `/plugin`, `/reload-plugins`, `/skills`.

---

## 8. Module 7 — Qualité, idiomaticité, sécurité (l'agent comme garde-fou)

### 7.1 Sécurité proactive
À la création de `config.yaml`/`.env.example`, l'agent a **de lui-même** :
- déplacé les secrets hors du YAML (référence `${VAR}` + `.env`) ;
- **vérifié le `.gitignore`** (le `.env` y était déjà) ;
- ajouté l'exclusion de la base d'historique et des dossiers runtime.

Enseigner : un bon agent applique l'**hygiène** sans qu'on le demande, mais **on vérifie** (ex. que
les credentials ne fuient pas dans les logs, permissions minimales documentées).

### 7.2 Cohérence des artefacts
L'agent a détecté que `versions.md` **contredisait** une décision déjà validée par le développeur
(date d'introduction d'une option) et l'a signalé. Leçon : faire **chasser les incohérences** entre
docs, et corriger partout à la fois.

### 7.3 Idiomaticité — réviser pour l'élégance
Sur la demande « relis pour l'élégance », l'agent a identifié **ses propres dérives** par défaut et
les a corrigées. C'est un module entier à part entière (ci-dessous).

---

## 9. Module 8 — Étude de cas : « rendre le design élégant »

> Module phare : il montre que **les sorties par défaut d'un agent ne sont pas toujours
> idiomatiques**, et qu'un bon développeur sait le **réorienter** avec un standard de qualité.

Quatre corrections concrètes, à projeter en avant/après :

| Dérive par défaut | Pourquoi c'est un problème | Correction idiomatique |
|---|---|---|
| `Operation` **god-struct** : un `Type string` + 12 champs souvent vides, un champ `Type_` à underscore | Champs non pertinents à `nil`, switch sur chaîne, fragile | **Sum type** : une interface étroite + un petit struct par opération ; `switch op.(type)` |
| **13 packages** dont des minuscules (`state`, `notify`…) | Cérémonie d'imports sans cohésion | **6 packages cohésifs** ; les petits deviennent des *fichiers* |
| **God-interface** `SQLServer` (toutes les DMV + KILL/PAUSE…) | « Plus l'interface est grosse, plus l'abstraction est faible » | **Petites interfaces définies côté consommateur** (`logProbe`, `ddlControl`) |
| Interfaces `Notifier/History` créées *par avance* | Abstraction prématurée (YAGNI) | Types **concrets** d'abord ; interface quand un 2ᵉ implémenteur existe |

Et l'ajout d'une charte « **Guiding principles** » en tête du plan : data-over-code, **concurrence
par communication (un canal d'événements, un seul décideur → zéro mutex)**, erreurs enveloppées
(`%w`) + sentinelles, *functional options*, cœur pur / effets aux bords.

### Message
> Demandez explicitement la **qualité** (« rends ça idiomatique / KISS / élégant »). L'agent sait
> appliquer les bonnes pratiques **quand on les exige** — sinon il produit du « correct mais
> moyen ». La barre de qualité, c'est vous qui la fixez.

### Exercice
> Faire générer un module à l'agent, puis « relis pour l'élégance / l'idiomaticité » avec un skill
> de style activé. Comparer avant/après et nommer chaque amélioration.

---

## 10. Antipatterns & pièges à enseigner

1. **Confiance aveugle** dans les faits pointus → vérifier avec la doc (cf. corrections SQL Server).
2. **Specs qui divergent** du code/des décisions → faire chasser les incohérences régulièrement.
3. **Sur-ingénierie par défaut** (god-struct, packages trop fins, interfaces prématurées) → exiger
   KISS/idiomatique.
4. **Laisser l'agent deviner** sur des choix structurants → « pose-moi les questions d'abord ».
5. **Secrets en clair** → toujours `.env` + `.gitignore`, et relire ce que l'agent écrit.
6. **Prompts vagues** (« améliore ») → cadrer (« réfléchis profondément à X, propose des décisions »).

---

## 11. Aide-mémoire (à distribuer)

**Cadrer une tâche**
- « Lis @fichier et **réfléchis profondément** à … »
- « Propose des décisions à trancher, **pose-moi les questions d'abord**. »

**Décider / corriger**
- « **Décisions : a) … b) …** » (impératif, numéroté)
- « C'est faux : *<fait>* — voir <lien doc>. Corrige partout. »

**Qualité**
- « Relis pour l'**élégance / l'idiomaticité / KISS**. »
- Activer un **skill** de style (`golang-skills`, etc.) avant de générer.

**Mémoire & contexte**
- « Retiens que … » → vérifier le fichier de mémoire + l'index.
- `@chemin/fichier` pour injecter un fichier.

**Commandes utiles**
- `/plugin`, `/reload-plugins`, `/skills`, `/usage`
- Interrompre puis re-cadrer est une **action de pilotage** légitime.

**Outils**
- Opérations indépendantes ⇒ l'agent **parallélise**.
- `Edit` = correctif chirurgical ; `Write` = réécriture complète.

---

## 12. Annexe — Artefacts du projet à montrer en séance

Tous présents dans le dépôt *SqlGoPace*, à ouvrir en direct :

- `specs/SPECS.md` — spec finale (data-driven, hiérarchie de réaction, pré-vol, sécurité).
- `specs/reponses.txt` — le **dialogue** humain↔agent (round-trip de critique).
- `specs/IMPLEMENTATION.md` — plan en 10 phases + **Guiding principles** + décisions actées.
- `ddl_compatibility.yaml` — la **matrice data-driven** `min_version` + édition (cœur du « pas de
  règles métier codées en dur »).
- `config.yaml` / `.env.example` / `.gitignore` — hygiène de configuration et de secrets.
- `01.to_run/010_example_rebuild.yaml` — exemple de DDL **déclaratif** (le choix de conception clé).
- Fichiers de **mémoire** (`MEMORY.md`, `sqlgopace-english-only.md`, `sqlgopace-v1-decisions.md`) —
  la persistance inter-sessions.

> **Suggestion de déroulé (1 demi-journée)** : 30 min Modules 1–2, 30 min Module 3 (démo
> critique+vérification), 45 min Modules 4–5 (pilotage + questions structurées), pause, 30 min
> Modules 6–7, 45 min Module 8 (étude de cas élégance, le clou du spectacle), 20 min aide-mémoire +
> Q/R.

---

*Note : ce fichier est un support de formation (français) et non un artefact du produit. Le projet
SqlGoPace lui-même reste « tout en anglais » ; pensez à l'ignorer dans le packaging international si
besoin.*
