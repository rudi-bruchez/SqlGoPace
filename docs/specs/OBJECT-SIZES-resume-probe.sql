-- OBJECT-SIZES.md, open question 1: what does the size read return while a resumable
-- index rebuild is paused?
--
-- If TableStructureSizes returns the source index alone (same used_kb before the rebuild
-- and during the pause, no extra row), a "before" read at RESUME is the true old size and
-- the §5.2 exception goes. If the partial target shows up (more pages, or another row),
-- the exception stays.
--
-- Requirements: SQL Server 2017+ Enterprise or Developer (RESUMABLE). The SQL Server 2022
-- container image runs Developer edition by default. Use a THROWAWAY database: the script
-- creates and drops dbo.sqlgopace_resume_probe (~1 GB).
--
-- How to run (podman, two terminals):
--   podman run -d --name sqlprobe -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD='<password>' \
--     -p 1433:1433 mcr.microsoft.com/mssql/server:2022-latest
--   podman exec -it sqlprobe /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -P '<password>' -C
--
-- Step 1 and step 2 in terminal A; step 3 in terminal A while step 4 runs in terminal B;
-- then steps 5-7 in terminal A.

-------------------------------------------------------------------------------
-- Step 1 (terminal A): throwaway database and a table large enough that the rebuild
-- takes long enough to pause.
-------------------------------------------------------------------------------
IF DB_ID(N'SqlGoPaceProbe') IS NULL CREATE DATABASE SqlGoPaceProbe;
GO
USE SqlGoPaceProbe;
GO
DROP TABLE IF EXISTS dbo.sqlgopace_resume_probe;
CREATE TABLE dbo.sqlgopace_resume_probe (id int NOT NULL, b char(500) NOT NULL);
INSERT INTO dbo.sqlgopace_resume_probe (id, b)
SELECT TOP (2000000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)), 'x'
FROM sys.all_objects o1 CROSS JOIN sys.all_objects o2;
CREATE CLUSTERED INDEX CIX_resume_probe ON dbo.sqlgopace_resume_probe (id);
GO

-------------------------------------------------------------------------------
-- Step 2 (terminal A): the size read BEFORE the rebuild. Keep this output.
-- The first query is the TableStructureSizes query from OBJECT-SIZES.md §1, verbatim.
-------------------------------------------------------------------------------
DECLARE @schema sysname = N'dbo', @table sysname = N'sqlgopace_resume_probe', @partition int = 0;
SELECT i.index_id, i.name, i.type_desc, i.is_disabled,
       COALESCE(SUM(ps.used_page_count), 0) * 8 AS used_kb
FROM sys.indexes i
LEFT JOIN sys.dm_db_partition_stats ps
  ON ps.object_id = i.object_id AND ps.index_id = i.index_id
 AND (@partition = 0 OR ps.partition_number = @partition)
WHERE i.object_id = OBJECT_ID(QUOTENAME(@schema) + '.' + QUOTENAME(@table))
  AND i.is_hypothetical = 0
GROUP BY i.index_id, i.name, i.type_desc, i.is_disabled
ORDER BY i.index_id;

-- Every index row, hypothetical included, and every partition-stats row for the object:
-- shows a hidden or extra structure the query above would group away or filter out.
SELECT i.index_id, i.name, i.type_desc, i.is_hypothetical, i.is_disabled
FROM sys.indexes i
WHERE i.object_id = OBJECT_ID(N'dbo.sqlgopace_resume_probe')
ORDER BY i.index_id;

SELECT ps.index_id, ps.partition_id, ps.partition_number, ps.used_page_count, ps.row_count
FROM sys.dm_db_partition_stats ps
WHERE ps.object_id = OBJECT_ID(N'dbo.sqlgopace_resume_probe')
ORDER BY ps.index_id, ps.partition_id;
GO

-------------------------------------------------------------------------------
-- Step 3 (terminal A): start the resumable rebuild. It blocks this terminal until
-- step 4 pauses it (the statement then returns an error saying it was paused; expected).
-------------------------------------------------------------------------------
ALTER INDEX CIX_resume_probe ON dbo.sqlgopace_resume_probe
REBUILD WITH (ONLINE = ON, RESUMABLE = ON);
GO

-------------------------------------------------------------------------------
-- Step 4 (terminal B, a few seconds after step 3 started):
-------------------------------------------------------------------------------
-- USE SqlGoPaceProbe;
-- GO
-- ALTER INDEX CIX_resume_probe ON dbo.sqlgopace_resume_probe PAUSE;
-- GO

-------------------------------------------------------------------------------
-- Step 5 (terminal A): confirm the rebuild is PAUSED and part-way through.
-- If percent_complete is 0 or the state is not PAUSED, the table was too small or the
-- pause came too early: ABORT (step 7), then rerun from step 3 and pause later.
-------------------------------------------------------------------------------
SELECT name, state_desc, percent_complete, page_count
FROM sys.index_resumable_operations
WHERE object_id = OBJECT_ID(N'dbo.sqlgopace_resume_probe');
GO

-------------------------------------------------------------------------------
-- Step 6 (terminal A): the size read DURING the pause. Rerun the three queries of
-- step 2 and compare with the output kept from step 2.
-------------------------------------------------------------------------------

-------------------------------------------------------------------------------
-- Step 7 (terminal A): clean up.
-------------------------------------------------------------------------------
ALTER INDEX CIX_resume_probe ON dbo.sqlgopace_resume_probe ABORT;
GO
DROP TABLE IF EXISTS dbo.sqlgopace_resume_probe;
GO
