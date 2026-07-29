package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// contendedCaptureSuffix names the machine sidecar written next to a shrink manifest.
const contendedCaptureSuffix = ".contended.yaml"

type capturedObject struct {
	obj       mssql.LockedObject
	firstSeen string
	lastSeen  string
	count     int
}

// contendedCapture accumulates the distinct objects a shrink held a Sch-M lock on across
// its reactions, in first-seen order, keyed by object_id.
type contendedCapture struct {
	order []int64
	byID  map[int64]*capturedObject
}

func (c *contendedCapture) add(o mssql.LockedObject, now string) {
	if c.byID == nil {
		c.byID = make(map[int64]*capturedObject)
	}
	e, ok := c.byID[o.ObjectID]
	if !ok {
		e = &capturedObject{obj: o, firstSeen: now}
		c.byID[o.ObjectID] = e
		c.order = append(c.order, o.ObjectID)
	}
	e.lastSeen = now
	e.count++
}

func (c *contendedCapture) len() int { return len(c.order) }

// doc builds the machine document in first-seen order.
func (c *contendedCapture) doc(database string) maint.ContendedDoc {
	doc := maint.ContendedDoc{Database: database}
	for _, id := range c.order {
		e := c.byID[id]
		doc.Observed = append(doc.Observed, maint.ContendedObject{
			ObjectID: e.obj.ObjectID, Schema: e.obj.Schema, Table: e.obj.Table,
			LockMode: e.obj.Mode, TimesBlocked: e.count,
			FirstSeen: e.firstSeen, LastSeen: e.lastSeen,
		})
	}
	return doc
}

const contendedHeader = `# Contended-object capture for %s
# Objects this shrink held a Sch-M lock on while blocking other sessions —
# i.e. the objects it was relocating and could not get past. These are
# EMPIRICALLY CONFIRMED tail blockers (partial: the shrink stops at the first).
# Feed this to the planner:  sqlgopace plan --confirmed <this file>
`

// renderContended builds the sidecar: a commented human header + the machine body.
func renderContended(name, database string, acc *contendedCapture) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, contendedHeader, name)
	body, err := yaml.Marshal(acc.doc(database))
	if err != nil {
		// yaml.Marshal of this fixed struct cannot fail; keep the header if it ever does.
		return []byte(b.String())
	}
	b.Write(body)
	return []byte(b.String())
}

// captureContended records the objects our shrink (spid) currently holds a Sch-M lock
// on, into acc, and flushes the sidecar. Best-effort: a nil reader or a read error is a
// no-op. Called only for shrink operations.
func (e *Engine) captureContended(ctx context.Context, spid int, acc *contendedCapture, name, database string) {
	if e.blockers == nil || spid == 0 {
		return
	}
	held, err := e.blockers.HeldObjectLocks(ctx, spid)
	if err != nil {
		return
	}
	now := e.now()
	for _, o := range held {
		acc.add(o, now)
	}
	// Only write when this snapshot captured something: a snapshot with no held
	// locks (e.g. a stall pause after the lock released) must not rewrite the
	// sidecar with identical content. When held is non-empty the file legitimately
	// changes (times_blocked/last_seen advance), so this is not a dedup — it only
	// skips the write when nothing was captured this snapshot.
	if len(held) > 0 {
		path := filepath.Join(e.dirs.Processing, name+contendedCaptureSuffix)
		if err := os.WriteFile(path, renderContended(name, database, acc), 0o644); err != nil {
			fmt.Fprintf(e.out, "write contended capture %s: %v\n", name, err)
		}
	}
}
