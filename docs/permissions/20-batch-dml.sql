-- SqlGoPace: the batched-DML tier, per database.
--
--   sqlcmd -S <server> -v LOGIN=sqlgopace DATABASE=<database> -i 20-batch-dml.sql
--
-- Covers: batch_update, batch_delete.
--
-- Both roles are required, and db_datareader is the one that surprises people. Every
-- batch is an UPDATE or DELETE TOP (n): SQL Server needs SELECT on the table for the
-- TOP itself, for any predicate columns, and for the key_range strategy's own
-- SELECT MAX walk. A login holding db_datawriter alone fails with "The SELECT
-- permission was denied on the object", with or without a where clause.
--
-- Apply 10-ddl.sql first if the same login also runs DDL: this file does not grant it.

:setvar LOGIN "sqlgopace"
:setvar DATABASE "CHANGE_ME"

USE [$(DATABASE)];
GO

IF DATABASE_PRINCIPAL_ID('$(LOGIN)') IS NULL
    CREATE USER [$(LOGIN)] FOR LOGIN [$(LOGIN)];
GO

ALTER ROLE [db_datareader] ADD MEMBER [$(LOGIN)];
ALTER ROLE [db_datawriter] ADD MEMBER [$(LOGIN)];
GO

-- Narrower alternative, if the login must not read or write the whole database.
-- Grant per table instead of the two roles above, and repeat per target table.
--
--   GRANT SELECT, UPDATE ON [dbo].[MEASUREMENT] TO [$(LOGIN)];   -- batch_update
--   GRANT SELECT, DELETE ON [dbo].[MEASUREMENT] TO [$(LOGIN)];   -- batch_delete
