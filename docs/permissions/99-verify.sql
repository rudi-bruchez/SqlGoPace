-- SqlGoPace: what can this login actually run?
--
-- Run it AS THE SQLGOPACE LOGIN, connected to the database it will target. Run it
-- again per database: the database-scoped answers change with the connection, the
-- server-scoped ones do not.
--
--   sqlcmd -S <server> -U sqlgopace -P <password> -d <database> -i 99-verify.sql
--
-- These are the same probes SqlGoPace runs at preflight, so a "no" here is the
-- failure you would get from a run, seen before the run.

SET NOCOUNT ON;

SELECT
    SUSER_NAME()  AS login_name,
    DB_NAME()     AS connected_database,
    @@VERSION     AS server_version;

SELECT capability, granted, needed_for FROM (VALUES
    ('monitoring (VIEW SERVER STATE)',
     CASE WHEN HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1
            OR IS_SRVROLEMEMBER('sysadmin') = 1 THEN 'yes' ELSE 'NO' END,
     'every run: blocking, waits, progress, log space'),

    ('DDL (db_ddladmin)',
     CASE WHEN IS_ROLEMEMBER('db_ddladmin') = 1 OR IS_ROLEMEMBER('db_owner') = 1
            OR IS_SRVROLEMEMBER('sysadmin') = 1 THEN 'yes' ELSE 'NO' END,
     'rebuild/reorganize/create/drop index, columns, constraints, statistics, heaps'),

    ('read data (db_datareader)',
     CASE WHEN IS_ROLEMEMBER('db_datareader') = 1 OR IS_ROLEMEMBER('db_owner') = 1
            OR IS_SRVROLEMEMBER('sysadmin') = 1 THEN 'yes' ELSE 'NO' END,
     'batch_update, batch_delete (SELECT is required for the TOP and the predicate)'),

    ('write data (db_datawriter)',
     CASE WHEN IS_ROLEMEMBER('db_datawriter') = 1 OR IS_ROLEMEMBER('db_owner') = 1
            OR IS_SRVROLEMEMBER('sysadmin') = 1 THEN 'yes' ELSE 'NO' END,
     'batch_update, batch_delete'),

    ('DBCC in this database (db_owner)',
     CASE WHEN IS_ROLEMEMBER('db_owner') = 1 OR IS_SRVROLEMEMBER('sysadmin') = 1
          THEN 'yes' ELSE 'NO' END,
     'shrink (data and log), check_db'),

    ('shrink tempdb (sysadmin)',
     CASE WHEN IS_SRVROLEMEMBER('sysadmin') = 1 THEN 'yes' ELSE 'NO' END,
     'shrink_tempdb only; db_owner elsewhere does not carry into tempdb'),

    ('kill a session (ALTER ANY CONNECTION)',
     CASE WHEN HAS_PERMS_BY_NAME(NULL, NULL, 'ALTER ANY CONNECTION') = 1
            OR IS_SRVROLEMEMBER('processadmin') = 1
            OR IS_SRVROLEMEMBER('sysadmin') = 1 THEN 'yes' ELSE 'NO' END,
     'kill_blocking_sessions, kill_amplifying_maintenance, allow_abort_blockers')
) AS c(capability, granted, needed_for);

-- Per-table check for batched DML. Set the two variables and uncomment.
--
-- DECLARE @schema sysname = N'dbo', @table sysname = N'MEASUREMENT';
-- SELECT
--     QUOTENAME(@schema) + '.' + QUOTENAME(@table) AS target,
--     HAS_PERMS_BY_NAME(QUOTENAME(@schema) + '.' + QUOTENAME(@table), 'OBJECT', 'SELECT') AS can_select,
--     HAS_PERMS_BY_NAME(QUOTENAME(@schema) + '.' + QUOTENAME(@table), 'OBJECT', 'UPDATE') AS can_update,
--     HAS_PERMS_BY_NAME(QUOTENAME(@schema) + '.' + QUOTENAME(@table), 'OBJECT', 'DELETE') AS can_delete;
