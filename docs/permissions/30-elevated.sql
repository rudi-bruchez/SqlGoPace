-- SqlGoPace: the elevated tier, per database.
--
--   sqlcmd -S <server> -v LOGIN=sqlgopace DATABASE=<database> -i 30-elevated.sql
--
-- Covers: shrink (data and log files), check_db.
--
-- Both issue DBCC, which requires db_owner in that database or sysadmin. db_ddladmin
-- is not enough, and SqlGoPace fails preflight with the grant named rather than
-- letting the DBCC fail after the manifest has been claimed.
--
-- This is a wide grant. Give it only in the databases whose files you actually
-- shrink or check, and prefer a separate login from the DDL one if your policy
-- separates them.

:setvar LOGIN "sqlgopace"
:setvar DATABASE "CHANGE_ME"

USE [$(DATABASE)];
GO

IF DATABASE_PRINCIPAL_ID('$(LOGIN)') IS NULL
    CREATE USER [$(LOGIN)] FOR LOGIN [$(LOGIN)];
GO

ALTER ROLE [db_owner] ADD MEMBER [$(LOGIN)];
GO
