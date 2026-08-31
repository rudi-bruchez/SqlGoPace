# Permission templates

Ready-to-edit T-SQL for provisioning the login SqlGoPace connects with. Replace
`$(LOGIN)`, `$(PASSWORD)` and `$(DATABASE)` before running, or run the files with
`sqlcmd -v LOGIN=... PASSWORD=... DATABASE=...`, which substitutes them for you.

Apply only the tiers the manifests you actually run need. See `docs/permissions.md`
for the operation-by-operation reference these files implement.

| File | Grants | Needed for |
|---|---|---|
| `00-login.sql` | the login, plus `VIEW SERVER STATE` | every run, without exception |
| `10-ddl.sql` | `db_ddladmin` in one database | index, column, constraint and statistics operations |
| `20-batch-dml.sql` | `db_datareader` + `db_datawriter` in one database | `batch_update`, `batch_delete` |
| `30-elevated.sql` | `db_owner` in one database | `shrink`, `check_db` |
| `40-kill.sql` | `ALTER ANY CONNECTION` | killing blockers or victims, `ABORT_AFTER_WAIT = BLOCKERS` |
| `50-tempdb-shrink.sql` | `sysadmin` | `shrink_tempdb`, and nothing else |
| `99-verify.sql` | nothing; reports what the current login can run | checking a provisioned login |

Run `99-verify.sql` as the SqlGoPace login itself, in the database it will target.
It answers the only question that matters: which operations this login can run
today.
