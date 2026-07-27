package mssql

import (
	"errors"

	mssqldb "github.com/microsoft/go-mssqldb"
)

// errFileAllocation is SQL Server Msg 5240, "Could not adjust the space allocation for
// file '...'." DBCC SHRINKFILE raises it when it cannot bring a file down to the
// requested size right now — pages pinned at the file end, or concurrent allocation.
const errFileAllocation = 5240

// IsFileAllocationError reports whether err is Msg 5240 from a shrink that could not
// adjust a file's space allocation. The shrink driver treats it as a no-progress
// condition (back off, try a smaller step, then stop cleanly) rather than a hard
// failure: the reduction achieved so far is preserved on the server.
func IsFileAllocationError(err error) bool {
	var me mssqldb.Error
	return errors.As(err, &me) && me.Number == errFileAllocation
}

// errBufferLatchTimeout is SQL Server Msg 845, "Time-out occurred while waiting for
// buffer latch." It signals severe contention on a page rather than a genuine failure.
const errBufferLatchTimeout = 845

// IsBufferLatchTimeout reports whether err is Msg 845 (a time-out waiting for a
// buffer latch), a severe-contention signal the tempdb shrink treats as a retryable
// no-progress event rather than a hard failure.
func IsBufferLatchTimeout(err error) bool {
	var me mssqldb.Error
	return errors.As(err, &me) && me.Number == errBufferLatchTimeout
}
