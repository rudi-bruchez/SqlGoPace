# Spec métier — TUI distant (mode serveur / client)

> **Statut : DRAFT — itération à concevoir/implémenter.** Acte le besoin et le design pressenti.
> Créé le 2026-06-17, suite à l'essai `030_compress_exampledb_indexes.yaml` (run lancé en
> arrière-plan, non-TUI) où l'on a voulu suivre l'état en direct **depuis une autre session**.

## 1. Objectif

Permettre de **suivre l'état live d'un run et d'agir dessus depuis un autre processus** :

- l'instance qui exécute le run ouvre un **serveur sur un port** et diffuse son état
  (progression, sessions bloquées, waits, statut) ;
- une **autre exécution de l'outil en mode client** se connecte à ce port et **affiche le
  TUI** (incident console) + l'état, et peut envoyer des **actions** (kill DDL, kill un
  bloqueur, pause/extend).

Cas d'usage : un run de maintenance long tourne sur un jump-host / en arrière-plan ; un DBA
veut ouvrir le TUI depuis sa machine sans relancer ni interrompre le run.

## 2. Pourquoi c'est tractable : le découplage existe déjà

Le TUI ne communique avec le moteur **que par messages**, c'est déjà le protocole :

- **État (serveur → TUI)** : `ProgressMsg`, `BlockersMsg`, `WaitsMsg`, `StatusMsg`, `LogMsg`
  (`internal/tui/model.go:66-84`).
- **Actions (TUI → serveur)** : un `chan Action` (`internal/tui/model.go:123,129`), routé par
  `dispatchActions` (`cmd/sqlgopace/main.go:438,514-515`).

Aujourd'hui `runWithTUI` (`cmd/sqlgopace/main.go:431-456`) câble tout ça **in-process** :

```
feedConsole (poll DB) ──Msg──▶ tui.Program ──Action──▶ dispatchActions (→ serveur SQL)
   main.go:459-488                  (Bubble Tea)              main.go:514
```

Le mode serveur/client **ne change pas le TUI** : il remplace ce câblage in-process par un
**transport réseau**. Le modèle Bubble Tea (rendu, touches) est réutilisé **tel quel** côté
client. C'est ce qui rend l'effort moyen et non énorme.

## 3. Conception proposée

### 3.1 Vue d'ensemble

```
            instance SERVEUR (exécute le run)                     instance CLIENT
  ┌───────────────────────────────────────────┐        ┌──────────────────────────┐
  feedConsole/engine ──Msg──▶  HUB de diffusion │  SSE   │  reader ──Msg──▶ tui.Model │
  (état déjà produit)         │  (dernier snapshot,      │        │  (Bubble Tea, inchangé) │
  dispatchActions ◀──Action── │   fan-out N clients) │◀──POST── writer ◀──Action── tui │
  (→ serveur SQL)             └───────────── :port ──┘        └──────────────────────────┘
```

- **Serveur** : l'instance qui exécute possède déjà la connexion SQL, le SPID et produit déjà
  les `Msg` (via `feedConsole` / le futur step-sink de `progress-tui.md`). On ajoute un **hub**
  qui (a) garde le **dernier snapshot** de chaque type de message, (b) **fan-out** vers les
  clients connectés, (c) reçoit les `Action` des clients et les pousse dans le `chan Action`
  existant.
- **Client** : un nouveau mode (`--connect host:port`) qui ouvre le **même TUI**, mais alimente
  son `tui.Program` depuis le **flux réseau** au lieu de `feedConsole`, et renvoie les `Action`
  vers `POST /action` au lieu de `dispatchActions` local.

### 3.2 Transport recommandé : HTTP SSE + POST

- **État** : `GET /state` en **Server-Sent Events** (flux de `Msg` encodés JSON). Avantages :
  simple, debuggable (`curl`), traverse les proxys, et **ouvre gratuitement la voie d'un futur
  dashboard web** (même flux).
- **Actions** : `POST /action` (corps JSON = une `Action`).
- **Snapshot au join** : à la connexion SSE, le hub renvoie d'abord le **dernier snapshot** de
  chaque message (sinon un client tardif ne voit que les deltas et a un écran vide).
- Alternatives écartées pour la v1 : WebSocket (bidirectionnel mais plus lourd) ; TCP + JSON
  lignes (le plus simple mais pas debuggable au navigateur/curl). À garder en tête si SSE coince.

### 3.3 Sérialisation

Les `Msg`/`Action` sont des structs simples → JSON direct. Définir un petit type enveloppe
`{ "type": "progress|blockers|waits|status|log", "data": {…} }` côté serveur ; côté client,
décoder vers le `tea.Msg` correspondant. Les types vivent dans `internal/tui` (ou un nouveau
`internal/tui/wire`) pour rester l'unique source du protocole.

### 3.4 Modes & flags

- `sqlgopace -config config.yaml --serve :7070` : exécute le run **et** sert l'état (TUI local
  optionnel en plus, ou pas de TUI local).
- `sqlgopace --connect host:7070` : **n'exécute aucun DDL** ; ouvre seulement le TUI alimenté par
  le flux distant. Ne nécessite ni `config.yaml` ni connexion SQL côté client.

## 4. Sécurité — le vrai coût de la feature

Un port qui accepte des actions, c'est un port qui peut **`KILL` une DDL ou une session SQL** à
distance. Décisions structurantes :

1. **Bind `127.0.0.1` par défaut.** Exposition réseau (`--serve 0.0.0.0:…`) = opt-in explicite,
   avec avertissement.
2. **Clients lecture seule par défaut.** L'état est diffusé à tous ; les **actions** sont une
   capability séparée, refusée sauf si le client présente un **jeton** (`--token` / en-tête
   `Authorization`).
3. **TLS** dès qu'on dépasse localhost (sinon jeton et état en clair sur le réseau).
4. **Pas d'exécution côté client** : le mode `--connect` ne touche jamais une base ; il ne fait
   qu'afficher et relayer des actions au serveur, qui reste le seul à parler à SQL Server.

C'est ici que part l'essentiel de l'effort, **pas** dans le rendu.

## 5. Limites inhérentes (à assumer)

- **Serveur éphémère.** Le run est *one-shot* (le moteur traite la file puis sort) : le serveur
  ne vit que **le temps de l'opération**. Il faut donc **lancer avec `--serve` dès le départ** —
  on ne peut pas « brancher » un serveur sur un run déjà lancé en mode nu (même limite que le TUI
  actuel, cf. `specs/progress-tui.md §5`). Cette feature **résout** ce manque, à condition de
  démarrer en mode serveur.
- **Reconnexion client.** Un client peut se (re)connecter à tout moment ; il reçoit le snapshot
  courant. Si le serveur s'arrête (fin du run), le client doit l'afficher proprement et quitter.
- **Un seul producteur.** Le serveur est l'instance qui exécute ; les clients sont passifs
  (affichage + actions relayées). Pas de multi-serveur.

## 6. Liens avec les autres itérations

- **`progress-tui.md`** : le *step-sink* « opération i/N » + chrono passe **dans le même hub**,
  donc les clients distants voient aussi la progression manifeste. Concevoir les deux ensemble :
  le hub est le point de convergence des messages (poll serveur + step events).
- **`crash-resumable.md`** : sans rapport direct, mais un client distant rend le suivi d'un long
  run de maintenance (et ses éventuels skips métadonnée) bien plus pratique.

## 7. Estimation d'effort

**Moyen** (quelques jours), dominé par le **transport + le modèle de sécurité**, pas par le TUI :

| Lot | Taille |
|---|---|
| Hub de diffusion (snapshot + fan-out) côté serveur | petit |
| Transport SSE (`GET /state`) + `POST /action` | petit-moyen |
| Mode client `--connect` (réutilise `tui.Model`, reader/writer réseau) | petit |
| Sérialisation `Msg`/`Action` + type enveloppe | petit |
| Sécurité : bind localhost, lecture seule, jeton, (TLS) | **moyen — le gros** |
| Flags/config, tests, doc | moyen |

Le modèle Bubble Tea et le protocole de messages **existent déjà** : c'est l'économie principale.

## 8. Questions ouvertes

- **SSE vs WebSocket** pour la v1 ? (SSE + POST recommandé ; WS si on veut un seul canal bidi.)
- **Périmètre des actions à distance** : autorise-t-on `KILL` à distance, ou seulement
  pause/extend, le KILL restant réservé au TUI local ? (sécurité)
- **Découverte** : port fixe via flag, ou écrit dans un sidecar (`02.processing/…`) pour qu'un
  `--connect` local le retrouve sans le saisir ?
- **Plusieurs runs en parallèle** (multi-base, §17) : un port par run, ou un hub multiplexant
  plusieurs runs sous un même port ?
- **Réutilisation web** : fige-t-on le format SSE pour pouvoir brancher un dashboard web plus
  tard sans le casser ?

## 9. Références code (au 2026-06-17)

| Sujet | Emplacement |
|---|---|
| Types de messages d'état (protocole) | `internal/tui/model.go:66-84` |
| Canal d'actions TUI → serveur | `internal/tui/model.go:123,129` ; `internal/tui/program.go:25` |
| Câblage TUI in-process (à remplacer par le réseau) | `cmd/sqlgopace/main.go:431-456` |
| Production d'état par poll serveur | `feedConsole` (`cmd/sqlgopace/main.go:459-488`) |
| Routage des actions vers SQL Server | `dispatchActions` (`cmd/sqlgopace/main.go:514-515`) |
| Limite « pas d'attache à un run déjà lancé » | `specs/progress-tui.md §5` |
| Step-sink à faire converger dans le hub | `specs/progress-tui.md §3.0` |
