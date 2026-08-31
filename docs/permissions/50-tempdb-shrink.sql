-- SqlGoPace: shrink_tempdb, and nothing else, needs sysadmin.
--
--   sqlcmd -S <server> -v LOGIN=sqlgopace -i 50-tempdb-shrink.sql
--
-- DBCC SHRINKFILE for tempdb runs in tempdb, not in the database the connection sits
-- in, so db_owner of a user database does not carry. db_owner in tempdb would serve,
-- but tempdb is recreated from model at every restart and a membership granted there
-- does not survive one. That leaves sysadmin.
--
-- Think before running this file. A login that can shrink tempdb can do everything
-- else on the instance. If that is not acceptable, run shrink_tempdb manifests under
-- a separate operator-triggered login and keep the unattended one unprivileged.

:setvar LOGIN "sqlgopace"

USE [master];
GO

ALTER SERVER ROLE [sysadmin] ADD MEMBER [$(LOGIN)];
GO
