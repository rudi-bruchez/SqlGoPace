package mssql

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"time"
)

// SchedulerMonitorPeriod is how often the scheduler-monitor ring buffer emits a record.
// Reading it more often than this asks the monitored server to materialize every ring
// buffer it holds for a value that cannot have changed — the caller paces itself by this.
const SchedulerMonitorPeriod = time.Minute

// Load is a coarse snapshot of how busy the whole server is, for the console header. It
// is an ambiance indicator, not a monitored dimension: nothing in the reaction hierarchy
// reads it, so a server that cannot answer simply shows less.
type Load struct {
	CPURead       bool // the ring buffer was asked and answered, whether or not it held a record
	CPUKnown      bool // false when the scheduler-monitor ring buffer had no usable record
	BusyPercent   int  // the whole machine (100 - SystemIdle), not the SQL Server share
	SQLPercent    int  // the SQL Server process's share of the machine, <= BusyPercent
	RunnableTasks int  // tasks waiting for a CPU, summed over the online schedulers
	Schedulers    int  // online schedulers the runnable tasks are spread over
}

// schedulerMonitor is the part of a RING_BUFFER_SCHEDULER_MONITOR record we read. The
// fields are pointers so a record missing one is an error rather than a zero: a missing
// SystemIdle read as 0 would report a saturated machine on an idle one.
type schedulerMonitor struct {
	ProcessUtilization *int `xml:"SchedulerMonitorEvent>SystemHealth>ProcessUtilization"`
	SystemIdle         *int `xml:"SchedulerMonitorEvent>SystemHealth>SystemIdle"`
}

// parseSchedulerMonitor extracts machine-wide and SQL Server CPU percentages from one
// RING_BUFFER_SCHEDULER_MONITOR record. The record's schema is not a documented contract
// (sys.dm_os_ring_buffers documents the DMV, not the record), which is why every field is
// checked rather than assumed.
func parseSchedulerMonitor(record string) (busy, sqlPercent int, err error) {
	var rec schedulerMonitor
	if err := xml.Unmarshal([]byte(record), &rec); err != nil {
		return 0, 0, fmt.Errorf("parse scheduler monitor record: %w", err)
	}
	if rec.SystemIdle == nil || rec.ProcessUtilization == nil {
		return 0, 0, errors.New("scheduler monitor record has no SystemIdle/ProcessUtilization")
	}
	busy = min(max(100-*rec.SystemIdle, 0), 100)
	// The two figures are independent samples and need not agree; a SQL share above the
	// machine's busy time would read as a bug rather than as rounding.
	sqlPercent = min(max(*rec.ProcessUtilization, 0), busy)
	return busy, sqlPercent, nil
}

// schedulerMonitorRecordSQL is the expensive half of the load read: sys.dm_os_ring_buffers
// has no index, so the server materializes every ring buffer it holds before this filter
// and TOP apply. It is substituted into serverLoadSQL only on the polls that are due one.
const schedulerMonitorRecordSQL = `(SELECT TOP (1) rb.record
       FROM sys.dm_os_ring_buffers rb
      WHERE rb.ring_buffer_type = 'RING_BUFFER_SCHEDULER_MONITOR'
      ORDER BY rb.timestamp DESC)`

// serverLoadSQL reads the load facts in one round trip. Every column is a scalar subquery
// so the row comes back even when the ring buffer holds no scheduler-monitor record (an
// Azure SQL Database service objective, a server just restarted): the runnable figures
// still arrive, and only the CPU half goes missing. The %s is one of two fixed constants,
// never anything from outside this file.
const serverLoadSQL = `
SELECT %s,
    (SELECT SUM(s.runnable_tasks_count) FROM sys.dm_os_schedulers s WHERE s.status = 'VISIBLE ONLINE'),
    (SELECT COUNT(*) FROM sys.dm_os_schedulers s WHERE s.status = 'VISIBLE ONLINE');`

// nullRecordSQL stands in for the ring-buffer subquery on the polls that are not due one,
// and on servers that refuse it: the row still comes back, without the CPU half.
const nullRecordSQL = "CAST(NULL AS nvarchar(max))"

// ServerLoad reads how busy the whole server is. The runnable figures are live and come
// back on every call; withCPU asks for the ring-buffer record too, which the caller should
// do at most once per SchedulerMonitorPeriod — that is how often it changes.
//
// It degrades twice over, because the CPU half is the fragile one. A record that is present
// but unreadable leaves CPUKnown false. A server that refuses the ring buffer outright —
// Azure SQL Database on Basic, S0, S1 or in an elastic pool wants
// ##MS_ServerPerformanceStateReader## for it — fails the whole statement, subqueries and
// all, so the read is retried without it: the console keeps its live runnable figures
// instead of losing the line to a permission it does not need. Only a read that reached
// the ring buffer sets CPURead, so a caller pacing itself by SchedulerMonitorPeriod does
// not count a failure as a reading — a refused or interrupted statement costs nothing,
// having never scanned, and is worth retrying on the next poll.
func (c *Conn) ServerLoad(ctx context.Context, withCPU bool) (Load, error) {
	if withCPU {
		if l, err := c.readLoad(ctx, schedulerMonitorRecordSQL); err == nil {
			l.CPURead = true
			return l, nil
		}
	}
	return c.readLoad(ctx, nullRecordSQL)
}

// readLoad runs serverLoadSQL with one of the two fixed record expressions.
func (c *Conn) readLoad(ctx context.Context, recordExpr string) (Load, error) {
	var (
		record     sql.NullString
		runnable   sql.NullInt64
		schedulers int
	)
	query := fmt.Sprintf(serverLoadSQL, recordExpr)
	if err := c.pool.QueryRowContext(ctx, query).Scan(&record, &runnable, &schedulers); err != nil {
		return Load{}, fmt.Errorf("read server load: %w", err)
	}
	l := Load{RunnableTasks: int(runnable.Int64), Schedulers: schedulers}
	if record.Valid {
		if busy, sqlPercent, err := parseSchedulerMonitor(record.String); err == nil {
			l.CPUKnown, l.BusyPercent, l.SQLPercent = true, busy, sqlPercent
		}
	}
	return l, nil
}
