package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// SessionWait is one wait type accumulated by a session, from
// sys.dm_exec_session_wait_stats.
type SessionWait struct {
	WaitType          string
	WaitingTasksCount int64
	WaitTimeMS        int64
	SignalWaitTimeMS  int64
	MaxWaitTimeMS     int64
}

const sessionWaitsSQL = `
SELECT wait_type, waiting_tasks_count, wait_time_ms, signal_wait_time_ms, max_wait_time_ms
FROM sys.dm_exec_session_wait_stats
WHERE session_id = @spid;`

// SessionWaits reads the cumulative wait statistics for a session. The DMV is
// available on SQL Server 2016+ and Azure; callers treat an error as "no data"
// on older servers.
func (c *Conn) SessionWaits(ctx context.Context, spid int) ([]SessionWait, error) {
	rows, err := c.pool.QueryContext(ctx, sessionWaitsSQL, sql.Named("spid", spid))
	if err != nil {
		return nil, fmt.Errorf("read session waits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var waits []SessionWait
	for rows.Next() {
		var w SessionWait
		if err := rows.Scan(&w.WaitType, &w.WaitingTasksCount, &w.WaitTimeMS, &w.SignalWaitTimeMS, &w.MaxWaitTimeMS); err != nil {
			return nil, fmt.Errorf("scan session wait: %w", err)
		}
		waits = append(waits, w)
	}
	return waits, rows.Err()
}

// WaitCategory is a group of related wait types with their summed time, for
// reporting what slowed an operation down.
type WaitCategory struct {
	Name        string
	Description string
	WaitTimeMS  int64
	SignalMS    int64 // CPU/scheduling portion (dominant for SOS_SCHEDULER_YIELD)
	Tasks       int64
}

// waitClass defines one meaningful wait category and how wait types map to it.
// Matching is by exact name or by prefix; the first matching category in the
// ordered table wins. Wait types that match nothing are noise and are dropped.
type waitClass struct {
	name        string
	description string
	exact       []string
	prefixes    []string
}

// waitCategories is the curated, ordered set of wait categories that matter for a
// demanding DDL operation. Everything else is treated as benign/idle and ignored.
var waitCategories = []waitClass{
	{
		name:        "Locking",
		description: "blocked acquiring locks (schema, row, page, object)",
		prefixes:    []string{"LCK_M_"},
	},
	{
		name:        "Data I/O",
		description: "reading/writing data pages from disk",
		prefixes:    []string{"PAGEIOLATCH_"},
	},
	{
		name:        "Transaction log",
		description: "flushing or throttling the transaction log",
		exact: []string{
			"WRITELOG", "LOGBUFFER",
			"LOG_RATE_GOVERNOR", "POOL_LOG_RATE_GOVERNOR",
			"INSTANCE_LOG_RATE_GOVERNOR", "HADR_THROTTLE_LOG_RATE_GOVERNOR",
		},
	},
	{
		name:        "Parallelism",
		description: "parallel-query exchange (MAXDOP)",
		exact:       []string{"CXPACKET", "CXCONSUMER", "CXSYNC_PORT", "CXSYNC_CONSUMER", "EXCHANGE"},
	},
	{
		name:        "Memory",
		description: "memory grants / allocation",
		exact:       []string{"MEMORY_ALLOCATION_EXT", "RESOURCE_SEMAPHORE", "RESOURCE_SEMAPHORE_QUERY_COMPILE", "CMEMTHREAD"},
	},
	{
		name:        "CPU & scheduling",
		description: "CPU pressure / worker scheduling (signal waits)",
		exact:       []string{"SOS_SCHEDULER_YIELD", "THREADPOOL"},
	},
	{
		name:        "Page latch (tempdb)",
		description: "in-memory page-latch contention, often tempdb allocation",
		prefixes:    []string{"PAGELATCH_"},
	},
	{
		name:        "Sort & spill I/O",
		description: "sort/spill and async I/O",
		exact:       []string{"IO_COMPLETION", "ASYNC_IO_COMPLETION"},
	},
	{
		name:        "Availability Group",
		description: "synchronous replica commit (AG)",
		exact:       []string{"HADR_SYNC_COMMIT", "HADR_GROUP_COMMIT"},
	},
	{
		name:        "Backup",
		description: "backup I/O (e.g. a concurrent log backup)",
		exact:       []string{"BACKUPIO", "BACKUPBUFFER"},
	},
}

// classifyWait returns the category a wait type belongs to, or ok=false when the
// wait is noise that should be ignored.
func classifyWait(waitType string) (waitClass, bool) {
	for _, c := range waitCategories {
		if slices.Contains(c.exact, waitType) {
			return c, true
		}
		for _, p := range c.prefixes {
			if strings.HasPrefix(waitType, p) {
				return c, true
			}
		}
	}
	return waitClass{}, false
}

// CategorizeWaits groups meaningful waits into categories sorted by wait time
// (descending), dropping noise and empty categories. It also returns the total
// kept wait time in milliseconds.
func CategorizeWaits(waits []SessionWait) (categories []WaitCategory, totalMS int64) {
	byName := make(map[string]*WaitCategory)
	var order []string
	for _, w := range waits {
		class, ok := classifyWait(w.WaitType)
		if !ok {
			continue
		}
		cat := byName[class.name]
		if cat == nil {
			cat = &WaitCategory{Name: class.name, Description: class.description}
			byName[class.name] = cat
			order = append(order, class.name)
		}
		cat.WaitTimeMS += w.WaitTimeMS
		cat.SignalMS += w.SignalWaitTimeMS
		cat.Tasks += w.WaitingTasksCount
	}

	for _, name := range order {
		cat := byName[name]
		if cat.WaitTimeMS == 0 && cat.Tasks == 0 {
			continue
		}
		categories = append(categories, *cat)
		totalMS += cat.WaitTimeMS
	}
	sort.SliceStable(categories, func(i, j int) bool {
		if categories[i].WaitTimeMS != categories[j].WaitTimeMS {
			return categories[i].WaitTimeMS > categories[j].WaitTimeMS
		}
		return categories[i].Name < categories[j].Name
	})
	return categories, totalMS
}

// DiffWaits returns the per-wait-type increase between two snapshots (after minus
// before), keeping only wait types that advanced. It is used to attribute waits to
// a single operation, since the DMV is cumulative for the session.
func DiffWaits(before, after []SessionWait) []SessionWait {
	prev := make(map[string]SessionWait, len(before))
	for _, w := range before {
		prev[w.WaitType] = w
	}

	var delta []SessionWait
	for _, a := range after {
		b := prev[a.WaitType]
		d := SessionWait{
			WaitType:          a.WaitType,
			WaitingTasksCount: a.WaitingTasksCount - b.WaitingTasksCount,
			WaitTimeMS:        a.WaitTimeMS - b.WaitTimeMS,
			SignalWaitTimeMS:  a.SignalWaitTimeMS - b.SignalWaitTimeMS,
			MaxWaitTimeMS:     a.MaxWaitTimeMS,
		}
		if d.WaitTimeMS > 0 || d.WaitingTasksCount > 0 {
			delta = append(delta, d)
		}
	}
	return delta
}
