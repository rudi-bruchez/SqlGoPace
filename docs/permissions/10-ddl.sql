-- SqlGoPace: the DDL tier, per database.
--
--   sqlcmd -S <server> -v LOGIN=sqlgopace DATABASE=<database> -i 10-ddl.sql
--
-- Covers: rebuild_index, reorganize_index, create_index, drop_index, rebuild_heap,
-- add_column, alter_column, drop_column, add_constraint, drop_constraint,
-- update_statistics (FULLSCAN included), and the `plan` subcommand's fragmentation,
-- compression and heap analysis.
--
-- db_datareader is deliberately not granted here. It was measured as unnecessary for
-- every operation in this tier, FULLSCAN statistics included. Grant it only if you
-- also run batched DML (see 20-batch-dml.sql).

:setvar LOGIN "sqlgopace"
:setvar DATABASE "CHANGE_ME"

USE [$(DATABASE)];
GO

IF DATABASE_PRINCIPAL_ID('$(LOGIN)') IS NULL
    CREATE USER [$(LOGIN)] FOR LOGIN [$(LOGIN)];
GO

ALTER ROLE [db_ddladmin] ADD MEMBER [$(LOGIN)];
GO
