# Spec métier — Progression d'un manifeste (compteur, chrono, bloqués)

> **Statut : pièce maîtresse (§3.0 step-sink) implémentée.** Le *step-sink* moteur
> (`run.WithStepSink` / `run.StepEvent`, émis en haut/bas de la boucle d'ops) alimente **stdout**
> (`-- [i/N] cmd cible — started` / `— <outcome> in Xs`, items 1+2) et le **TUI** (compteur op i/N +
> chrono live via un tick 1 s ; item 2). Livré avec BATCH-DML It3. Restent (DRAFT) : item 3 côté
> non-TUI (compte/pic de sessions bloquées en stdout/history) et la persistance i/N dans `State`.
> Créé le 2026-06-17, suite à l'essai de compression `030_compress_exampledb_indexes.yaml`
> (74 rebuilds offline sur `EXAMPLEDB`), où l'on a constaté l'absence de progression lisible.

## 1. Objectif

Donner à l'opérateur une **progression lisible** pendant qu'un manifeste s'exécute, en mode
stdout (run en arrière-plan) **et** dans le TUI :

1. **Compteur niveau manifeste « opération i / N ».**
2. **Chronomètre de l'opération en cours** (quelle op, depuis combien de temps).
3. **Nombre de sessions bloquées** par l'opération (déjà présent dans le TUI — à confirmer/étendre).

Motivation : sur du **Standard**, les rebuilds sont **offline** et SQL Server **ne renseigne pas**
`percent_complete` (cf. §3.2) ; la seule jauge actuelle du TUI reste donc à 0 %. Une progression
*au niveau manifeste* (i/N + chrono) est indépendante de ce que le serveur veut bien rapporter.

## 2. État actuel observé

### 2.1 Le moteur a déjà l'information, il ne l'expose pas

La boucle d'exécution connaît tout ce qu'il faut (`internal/run/engine.go:286-287`) :

```go
for i, step := range planned {        // i = index 0-based ; len(planned) = N (après expansion/plan)
    opStart := e.clk.Now()            // début de l'op : le chrono est déjà calculé
    ...
}
```

`opTarget(step.Operation)` donne déjà le libellé de la cible (utilisé en `engine.go:299`). Mais le
moteur ne narre **aucune** ligne « début d'op i/N » : il n'écrit que les événements manifeste
(`skip`/`complete`/`fail`/`done`) et les événements de réaction (`engine.go:299`). D'où un log de run
quasi muet entre le départ et la fin (constaté : seules les 2 lignes d'en-tête).

### 2.2 Le TUI est découplé du moteur (poll serveur uniquement)

`runWithTUI` (`cmd/sqlgopace/main.go:431-456`) lance en parallèle :

- `engine.ProcessAll` dans une goroutine, qui écrit dans **`io.Discard`** en mode TUI ;
- `feedConsole` (`main.go:459-488`) qui **interroge le serveur** et envoie au TUI : `BlockersMsg`,
  `ProgressMsg` (le `percent_complete` du SPID), `WaitsMsg`.

Il n'existe **aucun canal moteur → TUI**. Le label `operation:` du modèle reste donc `(running)`
(`tui.New("(running)", …)`), jamais mis à jour par op — même si `StatusMsg.Operation` existe déjà
côté modèle (`internal/tui/model.go:78-81, 151-154, 215-216`).

### 2.3 `percent_complete` inexploitable en offline

`ProgressMsg.Percent` vient de `sys.dm_exec_requests.percent_complete` (`main.go:480-501`,
`internal/mssql/dmv.go:53-80`). Ce champ n'est renseigné que pour REORGANIZE, online/resumable, DBCC,
BACKUP/RESTORE, rollback… **pas** pour un `ALTER INDEX REBUILD` **offline**. Sur Standard il reste à 0.

### 2.4 Item 3 déjà là (TUI)

Le TUI affiche déjà `blocked sessions (%d)` + la liste (SPID/login/host/wait/requête) :
`internal/tui/model.go:222`, alimenté par `feedConsole` qui filtre les sessions dont
`BlockingSPID == ddlSPID` (`main.go:468-478`). **Rien à créer côté TUI** pour l'item 3 ; seul le
report stdout/non-TUI ne l'a pas.

## 3. Conception proposée

### 3.0 Pièce maîtresse : un *sink d'étape* moteur → consommateurs

Ajouter au moteur un canal d'événements d'étape, indépendant de la narration texte, pour que **stdout
et TUI** soient alimentés par la même source :

```go
type StepEvent struct {
    Index, Total int           // i+1 sur N (1-based pour l'affichage)
    Command      string        // "rebuild_index", "shrink", …
    Target       string        // opTarget(step.Operation)
    StartedAt    time.Time     // = opStart (chrono)
    Phase        StepPhase     // Started | Finished
    Duration     time.Duration // rempli sur Finished
    Outcome      string        // "done" | "failed" | "skipped" (cf. crash-resumable §9)
}
```

Câblage via une `EngineOption` (`WithStepSink(func(StepEvent))`, à côté de `WithProgress`/`WithOutput`,
`engine.go:147-156`). Émission en haut et en bas de la boucle `engine.go:286`.

- **Mode non-TUI** (cas réel : run en arrière-plan) : le sink formate vers `e.out` —
  `\[12/74] rebuild_index dbo.ORDERS.PK_ORDERS — started 23:45:01` puis
  `\[12/74] … done in 3m20s`. **C'est le gain de progression le plus important**, vu que l'usage
  courant est non-TUI.
- **Mode TUI** : `runWithTUI` mappe `StepEvent` → `tui.StatusMsg` (label + compteur + `StartedAt`).

### 3.1 Item 1 — compteur « opération i / N »

- Données : `i+1` et `len(planned)` déjà disponibles (§2.1).
- TUI : étendre `StatusMsg` (ou `tui.New`) avec `StepIndex`/`StepTotal` ; rendu en tête de `View`
  (`model.go:215`) : `operation 12/74: rebuild_index dbo.ORDERS.PK_ORDERS [RUNNING]`.
- stdout : voir 3.0.
- (Option) persister `i/N` dans `State` (`internal/run/state.go`) pour qu'un re-run affiche d'emblée
  « reprise à l'op k/N » — à arbitrer avec le mécanisme de skip métadonnée (crash-resumable §9), qui
  rend le curseur d'état moins nécessaire.

### 3.2 Item 2 — chronomètre de l'opération en cours

- Donnée : `opStart` existe déjà (§2.1) ; le pousser via `StartedAt`.
- TUI : stocker `opStartedAt` dans le modèle ; afficher `elapsed = now − opStartedAt`. Nécessite un
  rafraîchissement périodique → ajouter un **`tea.Tick` à 1 s** (le modèle/`program.go` n'en a pas
  aujourd'hui) qui ne fait que re-render. Rendu : `progress: op 12/74 — elapsed 03:20` (et garder le
  `percent`/ETA serveur quand il est disponible, p. ex. online/resumable/rollback).
- stdout/.log : la **durée par op** sur la ligne `Finished` (3.0) et dans le `.log`/history.

### 3.3 Item 3 — sessions bloquées

- **Déjà affiché dans le TUI** (§2.4). Delta proposé :
  - surfacer le **compte** en non-TUI : l'inclure quand une réaction se déclenche, ou en ligne d'op
    (`… blocked: 2`) lors d'un poll ;
  - enregistrer le **pic de sessions bloquées** par op dans le `.log`/history (utile post-mortem :
    « cette compression a bloqué jusqu'à 5 sessions pendant 4 min »).

## 4. Périmètre & limites

- L'item 2 (chrono live) est surtout une feature **TUI** (tick 1 s). En stdout, on se limite à la
  **durée par op** à la complétion (un chrono qui défile n'a pas de sens dans un log).
- Indépendant de `percent_complete` : i/N + chrono fonctionnent même quand le serveur ne rapporte
  rien (offline), ce qui est précisément le cas Standard.
- Ne dépend pas de la feature crash-resumable, mais s'y combine bien : le sink peut émettre
  `Outcome = "skipped"` quand le skip métadonnée (crash-resumable §9) saute une op déjà à la cible.

## 5. Questions ouvertes

- **Forme du sink** : un seul `WithStepSink` (Started/Finished) vs deux callbacks ? Réutiliser le
  patron des `WithX` existants (`engine.go:147-156`).
- **Attacher le TUI à un run déjà lancé ?** Aujourd'hui impossible (le TUI est intra-process). Hors
  périmètre, mais à noter : un run non-TUI en arrière-plan ne peut pas recevoir le TUI a posteriori.
- **Granularité shrink** : un `shrink` est multi-chunk (un seul `step`). Faut-il un sous-compteur
  « chunk j/k » en plus de l'op i/N ? Le driver shrink connaît déjà sa progression (`main.go:322`).
- **Persistance i/N dans `State`** : utile pour l'affichage de reprise, ou redondant avec le skip
  métadonnée ? (cf. crash-resumable §6, §9.5).

## 6. Références code (au 2026-06-17)

| Sujet | Emplacement |
|---|---|
| Boucle d'ops avec `i`, `len(planned)`, `opStart` | `internal/run/engine.go:286-287` |
| Libellé cible d'une op | `opTarget(step.Operation)` (`engine.go:299`) |
| Options moteur (`WithOutput`/`WithProgress`) | `internal/run/engine.go:147-156` |
| TUI : label op + `StatusMsg` | `internal/tui/model.go:78-81, 151-154, 215-216` |
| TUI découplé (moteur→`io.Discard`, poll serveur) | `cmd/sqlgopace/main.go:431-488` |
| `percent_complete` (0 en offline) | `cmd/sqlgopace/main.go:480-501`, `internal/mssql/dmv.go:53-80` |
| Sessions bloquées déjà affichées (item 3) | `internal/tui/model.go:222`, `cmd/sqlgopace/main.go:468-478` |
| Skip métadonnée (combinable, `Outcome=skipped`) | `specs/crash-resumable.md` §9 |
