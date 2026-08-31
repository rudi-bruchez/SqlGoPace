-- SqlGoPace: the login itself, and the one server-level grant every run needs.
-- Run in master, as sysadmin.
--
--   sqlcmd -S <server> -v LOGIN=sqlgopace PASSWORD=<strong password> -i 00-login.sql
--
-- For a Windows or Microsoft Entra login, skip the CREATE LOGIN below and create it
-- the way your environment does; only the GRANT matters here.
--
-- The password must not contain the login name. With CHECK_POLICY on, SQL Server refuses
-- that with Msg 33064, whose wording suggests a complexity problem rather than the real one.

:setvar LOGIN "sqlgopace"
:setvar PASSWORD "CHANGE_ME"

USE [master];
GO

IF SUSER_ID('$(LOGIN)') IS NULL
    CREATE LOGIN [$(LOGIN)] WITH PASSWORD = N'$(PASSWORD)',
        DEFAULT_DATABASE = [master],
        CHECK_EXPIRATION = ON,
        CHECK_POLICY = ON;
GO

-- VIEW SERVER STATE is not optional. The monitoring connection reads server-scoped
-- DMVs on every poll: active sessions and their waits, blocking chains, operation
-- progress, log-space usage. Without it the sampling loop fails and the engine loses
-- the very signal its reaction hierarchy is built on, even for a rebuild that never
-- blocks anyone.
GRANT VIEW SERVER STATE TO [$(LOGIN)];
GO
