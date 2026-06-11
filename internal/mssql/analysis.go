package mssql

import (
	"context"
	"database/sql"
	"fmt"
)

// InventoryObject is one index — or one partition of one index, or a heap — from
// the cheap maintenance inventory sweep (spec §4.0). It carries only metadata and
// cached page counts: no data sampling, so it is safe to run over a whole database.
type InventoryObject struct {
	Schema          string
	Table           string
	ObjectID        int64
	IndexID         int
	IndexName       string // empty for a heap (index_id = 0)
	Type            int    // sys.indexes.type: 0=heap, 1=clustered, 2=nonclustered, 5/6=columnstore, 3=XML, 4=spatial
	TypeDesc        string
	PartitionNumber int
	Compression     string // data_compression_desc: NONE | ROW | PAGE | COLUMNSTORE | COLUMNSTORE_ARCHIVE
	Rows            int64
	SizeMB          float64
}

// IsHeap reports whether the object is a heap (no clustered index).
func (o InventoryObject) IsHeap() bool { return o.Type == 0 }

const objectInventorySQL = `
SELECT
    OBJECT_SCHEMA_NAME(i.object_id), OBJECT_NAME(i.object_id),
    i.object_id, i.index_id, i.name, i.type, i.type_desc,
    p.partition_number, p.data_compression_desc, p.rows,
    CAST(s.used_page_count AS float) * 8 / 1024
FROM sys.indexes i
JOIN sys.objects o            ON o.object_id = i.object_id AND o.is_ms_shipped = 0 AND o.type = 'U'
JOIN sys.partitions p         ON p.object_id = i.object_id AND p.index_id = i.index_id
JOIN sys.dm_db_partition_stats s ON s.partition_id = p.partition_id
ORDER BY OBJECT_SCHEMA_NAME(i.object_id), OBJECT_NAME(i.object_id), i.index_id, p.partition_number
OPTION (RECOMPILE, MAXDOP 1);`

// ObjectInventory runs the cheap first pass over the connected database, returning
// every user index/heap partition with its current compression, size, and row
// count. The maintenance planner filters this list before any expensive read.
func (c *Conn) ObjectInventory(ctx context.Context) ([]InventoryObject, error) {
	rows, err := c.pool.QueryContext(ctx, objectInventorySQL)
	if err != nil {
		return nil, fmt.Errorf("object inventory: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []InventoryObject
	for rows.Next() {
		var (
			o                  InventoryObject
			schema, table      sql.NullString
			indexName, compDsc sql.NullString
		)
		if err := rows.Scan(
			&schema, &table, &o.ObjectID, &o.IndexID, &indexName, &o.Type, &o.TypeDesc,
			&o.PartitionNumber, &compDsc, &o.Rows, &o.SizeMB,
		); err != nil {
			return nil, fmt.Errorf("scan inventory row: %w", err)
		}
		o.Schema, o.Table, o.IndexName, o.Compression = schema.String, table.String, indexName.String, compDsc.String
		out = append(out, o)
	}
	return out, rows.Err()
}

// Physical-stats scan modes for sys.dm_db_index_physical_stats.
const (
	// PhysicalLimited is the cheap leaf-only mode (fragmentation + page count).
	PhysicalLimited = "LIMITED"
	// PhysicalSampled samples pages; required for forwarded_record_count and
	// avg_page_space_used_in_percent (a LIMITED scan returns those as NULL/0).
	PhysicalSampled = "SAMPLED"
)

// PhysicalStats is one partition's row from sys.dm_db_index_physical_stats.
// RecordCount, ForwardedRecordCount, and AvgPageSpaceUsedPercent are populated
// only by a SAMPLED (or DETAILED) scan; a LIMITED scan leaves them zero.
type PhysicalStats struct {
	PartitionNumber         int
	AvgFragmentationPercent float64
	PageCount               int64
	RecordCount             int64
	ForwardedRecordCount    int64
	AvgPageSpaceUsedPercent float64
}

const physicalStatsSQL = `
SELECT partition_number, avg_fragmentation_in_percent, page_count,
       ISNULL(record_count, 0), ISNULL(forwarded_record_count, 0),
       ISNULL(avg_page_space_used_in_percent, 0)
FROM sys.dm_db_index_physical_stats(DB_ID(), @object_id, @index_id, @partition, @mode)
OPTION (RECOMPILE);`

// PhysicalStats reads sys.dm_db_index_physical_stats for one object/index in the
// given mode. Use PhysicalLimited for fragmentation (indexes) and PhysicalSampled
// for forwarded records (heaps, index_id = 0). partition nil scans every partition.
func (c *Conn) PhysicalStats(ctx context.Context, objectID int64, indexID int, partition *int, mode string) ([]PhysicalStats, error) {
	rows, err := c.pool.QueryContext(ctx, physicalStatsSQL,
		sql.Named("object_id", objectID), sql.Named("index_id", indexID),
		sql.Named("partition", nullableInt(partition)), sql.Named("mode", mode))
	if err != nil {
		return nil, fmt.Errorf("physical stats (object %d index %d): %w", objectID, indexID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PhysicalStats
	for rows.Next() {
		var ps PhysicalStats
		if err := rows.Scan(&ps.PartitionNumber, &ps.AvgFragmentationPercent, &ps.PageCount,
			&ps.RecordCount, &ps.ForwardedRecordCount, &ps.AvgPageSpaceUsedPercent); err != nil {
			return nil, fmt.Errorf("scan physical stats: %w", err)
		}
		out = append(out, ps)
	}
	return out, rows.Err()
}

// CompressionSaving is one partition's sizes from sp_estimate_data_compression_savings:
// the current size and the estimated size under the requested setting (both KB).
type CompressionSaving struct {
	PartitionNumber int
	CurrentKB       int64
	RequestedKB     int64
}

const estimateCompressionSQL = `
EXEC sys.sp_estimate_data_compression_savings
    @schema_name = @schema, @object_name = @table,
    @index_id = @index_id, @partition_number = @partition,
    @data_compression = @setting;`

// EstimateCompression runs sp_estimate_data_compression_savings for one setting
// ("ROW" or "PAGE") and returns the current vs estimated size per partition.
// partition nil estimates every partition. The proc samples real data into tempdb,
// so callers run it only for candidates that survive the cheap inventory filter
// (rowstore, not already PAGE, within size bounds) — see spec §4.2.
func (c *Conn) EstimateCompression(ctx context.Context, schema, table string, indexID int, partition *int, setting string) ([]CompressionSaving, error) {
	rows, err := c.pool.QueryContext(ctx, estimateCompressionSQL,
		sql.Named("schema", schema), sql.Named("table", table),
		sql.Named("index_id", indexID), sql.Named("partition", nullableInt(partition)),
		sql.Named("setting", setting))
	if err != nil {
		return nil, fmt.Errorf("estimate compression %s on %s.%s: %w", setting, schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []CompressionSaving
	for rows.Next() {
		// The proc returns: object_name, schema_name, index_id, partition_number,
		// size_with_current(KB), size_with_requested(KB), sample_current(KB), sample_requested(KB).
		var (
			objName, schName             sql.NullString
			indexIDOut                   int
			cs                           CompressionSaving
			sampleCurrent, sampleRequest sql.NullInt64
		)
		if err := rows.Scan(&objName, &schName, &indexIDOut, &cs.PartitionNumber,
			&cs.CurrentKB, &cs.RequestedKB, &sampleCurrent, &sampleRequest); err != nil {
			return nil, fmt.Errorf("scan compression estimate: %w", err)
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// OperationalStats is one partition's access counters from
// sys.dm_db_index_operational_stats, used to detect write-intensive objects.
// The counters are cumulative since the object entered the cache / the server
// started, so they are a heuristic (spec §4.3).
type OperationalStats struct {
	PartitionNumber int
	LeafInsert      int64
	LeafUpdate      int64
	LeafDelete      int64
	RangeScan       int64
	SingletonLookup int64
}

const operationalStatsSQL = `
SELECT partition_number,
       ISNULL(leaf_insert_count, 0), ISNULL(leaf_update_count, 0), ISNULL(leaf_delete_count, 0),
       ISNULL(range_scan_count, 0), ISNULL(singleton_lookup_count, 0)
FROM sys.dm_db_index_operational_stats(DB_ID(), @object_id, @index_id, @partition)
OPTION (RECOMPILE);`

// IndexOperationalStats reads the access counters for one object/index. partition
// nil returns every partition. An object absent from the cache yields no rows.
func (c *Conn) IndexOperationalStats(ctx context.Context, objectID int64, indexID int, partition *int) ([]OperationalStats, error) {
	rows, err := c.pool.QueryContext(ctx, operationalStatsSQL,
		sql.Named("object_id", objectID), sql.Named("index_id", indexID),
		sql.Named("partition", nullableInt(partition)))
	if err != nil {
		return nil, fmt.Errorf("operational stats (object %d index %d): %w", objectID, indexID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []OperationalStats
	for rows.Next() {
		var os OperationalStats
		if err := rows.Scan(&os.PartitionNumber, &os.LeafInsert, &os.LeafUpdate, &os.LeafDelete,
			&os.RangeScan, &os.SingletonLookup); err != nil {
			return nil, fmt.Errorf("scan operational stats: %w", err)
		}
		out = append(out, os)
	}
	return out, rows.Err()
}

// StatProperty is one statistic's freshness, from sys.dm_db_stats_properties.
type StatProperty struct {
	Name                string
	Rows                int64
	ModificationCounter int64
	LastUpdated         string // ISO 8601, empty when never updated
}

const statsPropertiesSQL = `
SELECT s.name, ISNULL(sp.rows, 0), ISNULL(sp.modification_counter, 0),
       CONVERT(varchar(30), sp.last_updated, 126)
FROM sys.stats s
CROSS APPLY sys.dm_db_stats_properties(s.object_id, s.stats_id) sp
WHERE s.object_id = @object_id
OPTION (RECOMPILE);`

// StatsProperties reads every statistic on the object with its row count and
// modification counter, used to decide UPDATE STATISTICS (spec §4.4, §5.4).
func (c *Conn) StatsProperties(ctx context.Context, objectID int64) ([]StatProperty, error) {
	rows, err := c.pool.QueryContext(ctx, statsPropertiesSQL, sql.Named("object_id", objectID))
	if err != nil {
		return nil, fmt.Errorf("stats properties (object %d): %w", objectID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []StatProperty
	for rows.Next() {
		var (
			sp          StatProperty
			lastUpdated sql.NullString
		)
		if err := rows.Scan(&sp.Name, &sp.Rows, &sp.ModificationCounter, &lastUpdated); err != nil {
			return nil, fmt.Errorf("scan stat property: %w", err)
		}
		sp.LastUpdated = lastUpdated.String
		out = append(out, sp)
	}
	return out, rows.Err()
}

// nullableInt returns a value suitable for a SQL parameter: the int when set, or
// nil (SQL NULL) when the pointer is nil. Used for optional partition arguments.
func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
