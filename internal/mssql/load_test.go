package mssql

import "testing"

// A real RING_BUFFER_SCHEDULER_MONITOR record, as sys.dm_os_ring_buffers returns it.
const schedulerMonitorRecord = `<Record id="286" type="RING_BUFFER_SCHEDULER_MONITOR" time="93472921">` +
	`<SchedulerMonitorEvent><SystemHealth>` +
	`<ProcessUtilization>32</ProcessUtilization>` +
	`<SystemIdle>55</SystemIdle>` +
	`<UserModeTime>1181250000</UserModeTime>` +
	`<KernelModeTime>145312500</KernelModeTime>` +
	`<PageFaults>210154</PageFaults>` +
	`<WorkingSetDelta>-12288</WorkingSetDelta>` +
	`<MemoryUtilization>100</MemoryUtilization>` +
	`</SystemHealth></SchedulerMonitorEvent></Record>`

func TestParseSchedulerMonitor(t *testing.T) {
	busy, sqlPct, err := parseSchedulerMonitor(schedulerMonitorRecord)
	if err != nil {
		t.Fatalf("parseSchedulerMonitor: %v", err)
	}
	// Busy is the whole machine (100 - SystemIdle), not the SQL Server share: the
	// point of the line is to show an operator that something else is eating the box.
	if busy != 45 {
		t.Errorf("busy = %d, want 45", busy)
	}
	if sqlPct != 32 {
		t.Errorf("sql = %d, want 32", sqlPct)
	}
}

func TestParseSchedulerMonitorRejectsIncompleteRecord(t *testing.T) {
	// A record without SystemIdle must be an error, not 100% busy: a missing element
	// read as zero would report a saturated server on an idle one.
	const noIdle = `<Record><SchedulerMonitorEvent><SystemHealth>` +
		`<ProcessUtilization>32</ProcessUtilization>` +
		`</SystemHealth></SchedulerMonitorEvent></Record>`
	if _, _, err := parseSchedulerMonitor(noIdle); err == nil {
		t.Fatal("parseSchedulerMonitor on a record without SystemIdle = nil error, want an error")
	}
	if _, _, err := parseSchedulerMonitor(""); err == nil {
		t.Fatal("parseSchedulerMonitor on an empty record = nil error, want an error")
	}
}

func TestParseSchedulerMonitorClamps(t *testing.T) {
	// The two figures are independent samples and do not have to agree: idle 55 with
	// process 50 says SQL used more than the machine was busy. Reporting sql > busy
	// would read as a bug to the operator, so the SQL share is capped at busy.
	const overlapping = `<Record><SchedulerMonitorEvent><SystemHealth>` +
		`<ProcessUtilization>50</ProcessUtilization><SystemIdle>55</SystemIdle>` +
		`</SystemHealth></SchedulerMonitorEvent></Record>`
	busy, sqlPct, err := parseSchedulerMonitor(overlapping)
	if err != nil {
		t.Fatalf("parseSchedulerMonitor: %v", err)
	}
	if busy != 45 || sqlPct != 45 {
		t.Errorf("busy/sql = %d/%d, want 45/45", busy, sqlPct)
	}
}
