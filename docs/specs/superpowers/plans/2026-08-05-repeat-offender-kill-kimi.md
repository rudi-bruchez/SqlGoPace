# Plan Review: Repeat-Offender Kill Acceleration

Reviewed plan: `docs/specs/superpowers/plans/2026-08-05-repeat-offender-kill.md`
Design spec: `docs/specs/superpowers/specs/2026-08-05-repeat-offender-kill-design.md`

## Status

Approved with issues to fix before implementation.

The plan covers every requirement in the design spec: the per-identity debt clock, the two identity keys, the cap and escalation for both killers, the suppression trap for capped victims, the reservation pattern in `VictimKiller`, and the sampler-seam regression test. The task decomposition is sensible and the tests are concrete.

## Issues

These are inconsistencies or gaps that could mislead an implementer or leave stale documentation behind.

### Task 2: wrong file location in the Files block

The Files block lists `internal/run/executor.go:69-78` for adding `key()` and `String()`. Lines 69-78 are the `sessionRule` struct definition. Step 3 correctly adds the methods after `matches()` around line 147.

Fix: change the Files block entry to point at the `matches` method area (around lines 130-147) or drop the line range and say "add after `matches`".

### Task 2: import instruction is redundant

Step 3 says to add `"strconv"` and `"strings"` to `internal/run/executor.go` imports. Both are already imported at lines 6-7. This is harmless but confusing.

Fix: remove the import instruction, or add a note that the imports already exist.

### Missing doc-comment update for `Waited` fields

The plan changes the meaning of `KillEvent.Waited` and `AmplifierKillEvent.Waited` from "elapsed time of this episode" to "the offender identity's accumulated debt", but it does not include a step to update the field comments in `internal/run/kill.go` (line 17) and `internal/run/victim.go` (line 44). Stale comments will mislead future maintainers.

Fix: add a step in Task 3 and Task 5 to update the `Waited` field comments. For example:

```go
// Waited is how long the identity has blocked the DDL, accumulated across sessions.
```

### Task 6: wording of `considerLocked` return updates

Step 3 says "update its two early returns to `return nil, nil, sink, onKill` / the final one to `return targets, capped, sink, onKill`". There is only one early return (the `!k.armed` guard) plus the final return.

Fix: reword to "update the early return and the final return".

## Recommendations

These are advisory and do not block approval.

- Note in Task 5 that `internal/run/victim.go` already imports `"fmt"`, so `victimKey` needs no import change.
- The `git stash && go test ...; git stash pop` step in Task 7 is a good regression demonstration, but consider warning that any other unstaged work will also be stashed. A temporary worktree is safer if the implementer has unrelated changes.
- Task 8's grep step is useful. Consider also grepping for `"blocked continuously"` in comments that describe `max_block_minutes`, but those are correctly left alone per the plan.

## Cross-checks against the current tree

I verified the following while reading the plan:

- `internal/run/executor.go` already imports `"strconv"` and `"strings"`.
- `internal/run/victim.go` already imports `"fmt"`.
- `mssql.ParseJobStepProgram` exists in `internal/mssql/agent.go`.
- `sampleProbe` and `ServerSampler` signatures match the probe used in Task 7.
- The design spec's requirements are all represented in the plan.
