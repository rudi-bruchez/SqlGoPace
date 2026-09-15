# Adversarial spec review: CANCEL-ONLY.md

**Verdict**: Must be revised first. The central design choice requires code changes omitted from the spec, and the handling of log pressure contradicts the spec's own stated motivation.

## Findings

### 1. BLOCKER
- **Claim**: "`runLoop` currently returns the bare sentinel `ErrCancelled`, losing which pressure caused it. It will return an error that wraps `ErrCancelled` and carries the `Pressure`"
- **Reality**: `runLoop` (`internal/run/monitored_runner.go:129`) invokes the `runStatement` closure with the signature `func(string) (Action, error)` and explicitly discards `Pressure`. `supervise` produces it, but `runStatement` only sends it to the `ReactionSink`.
- **Consequence**: Unimplementable as written. `runLoop` cannot wrap `Pressure` in an error because it never receives it from the execution layer.
- **Fix**: Change the `runStatement` signature in `runLoop`'s arguments to `func(string) (Action, Pressure, error)` so the pressure can be returned to `runLoop`.

### 2. MAJOR
- **Claim**: "Why blocking is not retried: [...] a retry restarts from zero against the same writers: it pays the cost again for the same expected result." (vs) "transaction log over cap, alone | wait for the log to drop back under the cap [...] then retry"
- **Reality**: A rollback-on-cancel operation cancelled due to log pressure *also* restarts from zero and pays the cost again. If the operation itself generates the log (e.g. an offline index rebuild of a large table), retrying it will simply run again, fill the log cap again, and cancel/rollback a second time.
- **Consequence**: The spec explicitly protects the operator from wasting work on blocking cancels, but leaves the exact same waste (plus massive I/O and log bloat) on log pressure cancels.
- **Fix**: Apply the same "no retry" policy to log pressure for rollback-on-cancel operations.

### 3. MINOR
- **Claim**: "The comment on `max_retry_attempts` in `config.yaml` and its embedded twin (`# Standard: a blocked offline rebuild is quarantined fast (2 attempts, not 4)`) becomes false and is rewritten."
- **Reality**: The original run in "From the field" failed 20 operations precisely because they were retried once each (2 attempts per operation under `max_retry_attempts: 1`).
- **Consequence**: The spec correctly notes the documentation change, but misses that the 20 operations will still fail under the new design (just on the first attempt instead of the second). 
- **Fix**: Acknowledge explicitly in the Migration or Problem section that the new design does not make the 20 operations succeed; it only halves the time spent failing.

## Sound elements

- Identifying `ShrinkRunner`, `TempdbShrink`, and `BatchDMLRunner` as keeping committed work is correct.
- `cancelSafe` accurately covers `reorganize_index`, `check_db`, and `update_statistics`.
- Emitting the `warn` event through the operation's sink at the start is the correct mechanism to reach all reports.
