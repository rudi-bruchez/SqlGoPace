# TODO — itérations à concevoir / implémenter

Index des specs d'itération en attente de brainstorming puis d'implémentation. Toutes en
**DRAFT** : aucune n'est encore codée. Datées du 2026-06-17.

## Itérations

- [ ] **[Reprise après interruption / skip métadonnée](crash-resumable.md)** — un Ctrl+C / kill
  / crash ne reprend pas où ça s'était arrêté (la recovery re-joue le manifeste depuis le début).
  Propose : vrai `ALTER INDEX … RESUME` quand possible, et surtout le **skip par métadonnée**
  (`sys.partitions` : ne rebuild que si la compression ≠ cible) pour un re-jeu idempotent bon marché.

- [ ] **[Progression d'un manifeste (compteur i/N, chrono, bloqués)](progress-tui.md)** — pas de
  suivi de progression lisible aujourd'hui. Propose : compteur **« opération i/N »**, **chrono**
  de l'op en cours, **nombre de sessions bloquées** (déjà dans le TUI, à surfacer aussi en stdout).
  Pièce maîtresse : un **step-sink** moteur → stdout & TUI.

- [ ] **[TUI distant (serveur / client)](remote-tui.md)** — suivre/agir sur un run depuis un autre
  process. Propose : `--serve :port` (hub de diffusion SSE) + `--connect host:port` (réutilise le
  TUI). Le vrai coût = **sécurité** des actions distantes (KILL). Converge avec le step-sink de
  `progress-tui.md`.

- [ ] **[Arrêt en douceur / drain](graceful-stop.md)** — commande TUI (et idéalement Ctrl+C 1×) qui
  s'arrête **après l'instruction en cours** (pas de rollback), écrit un **point de reprise** (curseur
  dans le sidecar `State`, ou fichier de contrôle dédié) et reprend à l'op suivante. Chaînon manquant
  entre `pause` (mi-instruction) et `kill` (brutal).

## Dépendances / ordre suggéré

1. `progress-tui.md` en premier — le **step-sink** qu'il introduit est réutilisé par `remote-tui.md`
   (même hub de messages).
2. `remote-tui.md` ensuite — s'appuie sur ce hub.
3. `crash-resumable.md` indépendant — peut se faire à part ; commencer par le **skip métadonnée**
   (petit, gros gain) avant le vrai RESUME.
4. `graceful-stop.md` partage le **curseur d'opération** avec `crash-resumable.md` (même champ
   `ResumeFromOp` dans `State`) — les concevoir ensemble.

## Contexte

Specs nées de l'essai de compression `01.to_run/030_compress_exampledb_indexes.yaml` (74 index PAGE
sur `EXAMPLEDB`, édition Standard → rebuilds offline). Voir aussi `docs/llm-operator-guide.md` et le
skill `.claude/skills/sqlgopace-operator/` (aide LLM à l'usage de l'outil).
