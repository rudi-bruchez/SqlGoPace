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
	obj              mssql.LockedObject
	firstSeen        string
	lastSeen         string
	count            int
	byTail           bool // upgraded by a tail-object walk
	transient        bool
	blockedByCommand string
	blockedBySPID    int
	indexID          int
	pageFromEnd      int
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

// addTail records a tail-position blocker. On an existing key (the object was also
// lock-captured) it upgrades the entry to tail_position and fills the tail fields while
// preserving the lock stats; a fresh key creates a tail-only entry (no lock stats).
func (c *contendedCapture) addTail(f TailFinding, now string) {
	if c.byID == nil {
		c.byID = make(map[int64]*capturedObject)
	}
	e, ok := c.byID[f.ObjectID]
	if !ok {
		e = &capturedObject{obj: mssql.LockedObject{ObjectID: f.ObjectID, Schema: f.Schema, Table: f.Table}, firstSeen: now}
		c.byID[f.ObjectID] = e
		c.order = append(c.order, f.ObjectID)
	}
	// A tail object was observed (walked) now — record it like the lock path does, without
	// counting it as a block (count is left untouched; times_blocked stays 0 for a tail-only
	// entry, and a merged lock+tail entry keeps its real block count).
	e.lastSeen = now
	e.byTail = true
	e.indexID = f.IndexID
	e.pageFromEnd = f.PageFromEnd
	if f.Transient {
		e.transient = true
		e.blockedByCommand = f.BlockedByCommand
		e.blockedBySPID = f.BlockedBySPID
	}
}

// doc builds the machine document in first-seen order.
func (c *contendedCapture) doc(database string) maint.ContendedDoc {
	doc := maint.ContendedDoc{Database: database}
	for _, id := range c.order {
		e := c.byID[id]
		confirmedBy := "lock"
		switch {
		case e.transient:
			confirmedBy = "transient_maintenance"
		case e.byTail:
			confirmedBy = "tail_position"
		}
		doc.Observed = append(doc.Observed, maint.ContendedObject{
			ObjectID: e.obj.ObjectID, Schema: e.obj.Schema, Table: e.obj.Table,
			LockMode: e.obj.Mode, TimesBlocked: e.count,
			FirstSeen: e.firstSeen, LastSeen: e.lastSeen,
			IndexID: e.indexID, ConfirmedBy: confirmedBy, PageFromEnd: e.pageFromEnd,
			BlockedByCommand: e.blockedByCommand, BlockedBySPID: e.blockedBySPID,
		})
	}
	return doc
}

const contendedHeader = `# Contended-object capture for %s
# Objects this shrink could not get past, by three confirmation kinds:
#   confirmed_by: lock          — held a Sch-M lock on the object while blocking other
#                                 sessions (empirical, partial: the shrink stops at the first).
#   confirmed_by: tail_position — owns the file's last allocated page (the tail the shrink
#                                 must relocate past), found by the backward page walk.
#   confirmed_by: transient_maintenance — the tail was pinned by a concurrent maintenance op
#                                 (e.g. ALTER INDEX) at capture time. Informational only —
#                                 NOT fed to a pre-shrink reorganize.
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
		e.writeContended(name, database, acc)
	}
}

// writeContended flushes the accumulator to the sidecar next to the manifest.
func (e *Engine) writeContended(name, database string, acc *contendedCapture) {
	path := filepath.Join(e.dirs.Processing, name+contendedCaptureSuffix)
	// 0600, not 0644: this sidecar names other people's sessions — login, host, application
	// and the text of the statement they were running. On a shared administrative host that
	// is third-party data, readable by every local user at 0644.
	if err := os.WriteFile(path, renderContended(name, database, acc), 0o600); err != nil {
		fmt.Fprintf(e.out, "write contended capture %s: %v\n", name, err)
	}
}

// captureTail records a tail-position blocker the shrink driver found (via a ReactionEvent)
// and flushes the sidecar. Best-effort; called only for shrink operations.
func (e *Engine) captureTail(acc *contendedCapture, name, database string, f TailFinding) {
	acc.addTail(f, e.now())
	e.writeContended(name, database, acc)
}
