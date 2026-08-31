-- SqlGoPace: the ability to terminate another session. Run in master, as sysadmin.
--
--   sqlcmd -S <server> -v LOGIN=sqlgopace -i 40-kill.sql
--
-- Needed by three features, all off by default:
--   kill_blockers            in config.yaml, with kill_blocking_sessions in a manifest
--   kill_amplifying_maintenance  the victim killer
--   allow_abort_blockers     which resolves ABORT_AFTER_WAIT = BLOCKERS
--
-- Without it those features are silent no-ops: SqlGoPace warns at preflight rather
-- than failing, because a run without them is still a valid run. It is a destructive
-- capability, so grant it deliberately.

:setvar LOGIN "sqlgopace"

USE [master];
GO

GRANT ALTER ANY CONNECTION TO [$(LOGIN)];
GO
