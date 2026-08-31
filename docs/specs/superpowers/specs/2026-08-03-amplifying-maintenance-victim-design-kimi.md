# Review: Killing amplifying maintenance victims of a non-blocking operation

Status: reviewed, 2026-08-03.
Reviewer: Kimi.

## Overall assessment

The design is sound, tightly scoped, and addresses the observed PRODDB incident directly. The suppression rule is the most consequential behavioral change, and it is well bounded by the existing `max_block_minutes` cap.

## Strengths

- The suppression of `Unignored` while a kill is pending is bounded: pending victims still count toward `Any`, so `max_block_minutes` remains a hard backstop.
- Creating a new maintenance classifier instead of reusing `mssql.IsMaintenanceCommand` avoids silently changing the shrink driver.
- The `.amplifiers.yaml` sidecar is advisory-only and never read back, which keeps the state machine simple.
- The `ASYNC_STATS_UPDATE_WAIT_AT_LOW_PRIORITY` advisory states both the benefit and the limitation, which prevents a false sense of safety.

## Questions and concerns for the implementation plan

1. Overlap with `kill_blocking_sessions`. The same SPID could theoretically be both the session blocking us and an amplifying maintenance victim. The plan should state whether the two killers share episode state or are reconciled, and whether a double `KILL` is possible.

2. Poll-count grace window. Suppressing a killed victim for "two further blocking polls" makes the grace duration depend on the poll interval. Consider whether the implementation should convert this to a time-based window, capped to avoid over-generosity when polls are slow.

3. `ignore_blocked_sessions` and suppression. An ignored maintenance victim is never killed, but does it still suppress `Unignored`? The expected behavior is that it does not, so the normal yield timer applies; this should be explicit in the plan.

4. `program_name` parsing. The SQL Agent T-SQL step format should be parsed defensively; older versions or extra spaces may vary. Include an integration test against a live Agent job.

5. `msdb` lookup fallback. When attribution fails because the step is CmdExec or PowerShell, the sidecar should still record the raw `program_name`, `login_name`, and `host_name` so operators can act manually.

6. Sidecar schema. Define the `.amplifiers.yaml` structure, including how multiple kills for the same job are aggregated into one block of `sp_update_job` statements.

7. `commands` override semantics. Replacing the built-in list is the safer default, but the plan should clarify that an empty `commands` list falls back to the built-in allow-list, not to "match nothing."

8. TUI alert key and lifetime. The sticky alert should define its deduplication key and when it clears. Suggest keying by `(job_name, step_id)` and clearing at the end of the manifest.

9. Ordering in `ServerSampler.Blocking`. Confirm the consultation order between `BlockerKiller` and `VictimKiller` in the poll loop. If they can target the same SPID, order matters.

## Suggested next step

Draft the implementation plan from this design, with file-by-file changes, test cases, and verification steps.
