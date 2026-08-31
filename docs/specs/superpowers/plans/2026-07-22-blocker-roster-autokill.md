# Blocker Roster with Pre-Armed Auto-Kill — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a TUI key (`b`) that opens a roster of every session that has blocked the running DDL this run, grouped by login or host, from which the operator toggles auto-kill (`kill_blocking_sessions`) rules on the running manifest.

**Architecture:** The roster is a grouped, actionable rendering of the existing in-memory suspension history (sessions that blocked us). Arming appends a manifest kill rule (no immediate kill); the engine hot-reloads it and the already-armed `BlockerKiller` terminates present-or-future matches. Un-arming removes the rule. The only new data captured is the blocker's host, threaded from `mssql.SelfBlock` through the suspension tracker to the TUI.

**Tech Stack:** Go, Bubble Tea (`github.com/charmbracelet/bubbletea`), lipgloss. Tests are pure/unit with `-race`, no database.

## Global Constraints

- Idiomatic Go, KISS — no new layers/interfaces/generics beyond what a task needs.
- English only in all code, comments, identifiers.
- US spelling in comments/identifiers.
- No `context.WithTimeout` around executing DDL (not relevant here, but keep the rule).
- Tests must pass under `make test` (runs `-race`, no DB).
- Manifest edits go through the existing atomic-write helpers in `internal/ddl/edit.go`.
- The design doc is `docs/specs/superpowers/specs/2026-07-22-blocker-roster-autokill-design.md`.

---

### Task 1: Capture the blocker's host in `mssql.SelfBlock`

**Files:**
- Modify: `internal/mssql/dmv.go` (`SelfBlock` struct ~line 192; `FindSelfBlock` fill loop ~line 226)
- Test: `internal/mssql/dmv_test.go`

**Interfaces:**
- Produces: `mssql.SelfBlock` gains field `Host string`, populated from the blocking session's `Session.Host`.

- [ ] **Step 1: Write the failing test**

Add to `internal/mssql/dmv_test.go`:

```go
func TestFindSelfBlockCapturesHost(t *testing.T) {
	sessions := []mssql.Session{
		{SPID: 119, WaitType: "LCK_M_SCH_M", WaitMS: 5000, BlockingSPID: 104},
		{SPID: 104, Login: "app_login", Host: "APPSRV01", Program: "SQLCMD"},
	}
	sb := mssql.FindSelfBlock(sessions, 119)
	if !sb.Blocked || sb.SPID != 104 {
		t.Fatalf("expected blocked by 104, got %+v", sb)
	}
	if sb.Host != "APPSRV01" {
		t.Errorf("Host = %q, want APPSRV01", sb.Host)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/mssql -run TestFindSelfBlockCapturesHost -v`
Expected: FAIL — compile error `sb.Host undefined` (field does not exist yet).

- [ ] **Step 3: Add the field and populate it**

In `internal/mssql/dmv.go`, add `Host` to the `SelfBlock` struct (after `Login`/`Program`):

```go
type SelfBlock struct {
	Blocked  bool
	SPID     int // the direct blocking session (blocking_session_id)
	WaitType string
	WaitMS   int64
	Login    string
	Program  string
	Host     string
	Query    string
}
```

In `FindSelfBlock`, the loop that fills the blocker's identity — change:

```go
		sb.Login, sb.Program = s.Login, s.Program
```

to:

```go
		sb.Login, sb.Program, sb.Host = s.Login, s.Program, s.Host
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/mssql -run TestFindSelfBlockCapturesHost -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mssql/dmv.go internal/mssql/dmv_test.go
git commit -m "feat(mssql): capture the blocker's host in SelfBlock"
```

---

### Task 2: Thread host through the suspension tracker to the TUI message

**Files:**
- Modify: `internal/tui/model.go` (`SuspensionBlocker` struct ~line 115)
- Modify: `cmd/sqlgopace/main.go` (`blockerAgg` ~line 681, `suspensionTracker.observe` ~line 708, `snapshot` ~line 740, `feedConsole` call ~line 784)
- Test: `cmd/sqlgopace/main_test.go` (`TestSuspensionTracker`, `TestSuspensionTrackerCountsRepeatBlocker`, and a new host test)

**Interfaces:**
- Consumes: `mssql.SelfBlock.Host` (Task 1).
- Produces:
  - `tui.SuspensionBlocker` gains field `Host string`.
  - `suspensionTracker.observe(blocked bool, spid int, login, host string, now time.Time)` — signature gains `host` after `login`.
  - `suspensionTracker.snapshot()` emits `Host` on each `tui.SuspensionBlocker`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/sqlgopace/main_test.go`:

```go
func TestSuspensionTrackerCapturesHost(t *testing.T) {
	tr := newSuspensionTracker()
	t0 := time.Unix(0, 0)
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0)
	tr.observe(false, 0, "", "", t0.Add(10*time.Second))

	snap := tr.snapshot()
	if len(snap.Blockers) != 1 {
		t.Fatalf("Blockers = %d, want 1", len(snap.Blockers))
	}
	if b := snap.Blockers[0]; b.Host != "APPSRV01" {
		t.Errorf("Blockers[0].Host = %q, want APPSRV01", b.Host)
	}
}
```

Also update the two existing tracker tests to the new `observe` signature (insert a host argument after each login argument). In `TestSuspensionTracker`:

```go
	tr.observe(false, 0, "", "", t0)
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0.Add(10*time.Second))
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0.Add(20*time.Second))
	tr.observe(false, 0, "", "", t0.Add(30*time.Second))
	tr.observe(true, 88, "SVC_X", "APPSRV02", t0.Add(40*time.Second))
	tr.observe(false, 0, "", "", t0.Add(50*time.Second))
```

In `TestSuspensionTrackerCountsRepeatBlocker`:

```go
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0)
	tr.observe(false, 0, "", "", t0.Add(10*time.Second))
	tr.observe(true, 104, "SVC_OBS", "APPSRV01", t0.Add(20*time.Second))
	tr.observe(false, 0, "", "", t0.Add(30*time.Second))
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race ./cmd/sqlgopace -run TestSuspensionTracker -v`
Expected: FAIL — compile error: `observe` takes the old 4-arg signature and `SuspensionBlocker.Host` is undefined.

- [ ] **Step 3: Add the field**

In `internal/tui/model.go`, add `Host` to `SuspensionBlocker`:

```go
	SuspensionBlocker struct {
		SPID    int
		Login   string
		Host    string
		Count   int
		TotalMS int64
	}
```

- [ ] **Step 4: Thread host through the tracker**

In `cmd/sqlgopace/main.go`:

Add `host` to `blockerAgg`:

```go
type blockerAgg struct {
	login string
	host  string
	count int
	total time.Duration
}
```

Change `observe`'s signature and record the host (the block that records `login`):

```go
func (t *suspensionTracker) observe(blocked bool, spid int, login, host string, now time.Time) {
```

and inside the new-episode branch, alongside `if login != "" { a.login = login }`:

```go
			a.count++
			if login != "" {
				a.login = login
			}
			if host != "" {
				a.host = host
			}
```

Emit it in `snapshot`:

```go
		msg.Blockers = append(msg.Blockers, tui.SuspensionBlocker{
			SPID: spid, Login: a.login, Host: a.host, Count: a.count, TotalMS: a.total.Milliseconds(),
		})
```

Pass the blocker's host at the `feedConsole` call site (the `susp.observe(...)` line ~784):

```go
				susp.observe(sb.Blocked, sb.SPID, sb.Login, sb.Host, time.Now())
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./cmd/sqlgopace -run TestSuspensionTracker -v`
Expected: PASS (all three tracker tests).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/model.go cmd/sqlgopace/main.go cmd/sqlgopace/main_test.go
git commit -m "feat: thread the blocker's host through the suspension tracker"
```

---

### Task 3: `ddl.RemoveKilledSession` — the inverse of `AppendKilledSession`

**Files:**
- Modify: `internal/ddl/edit.go` (add after `AppendKilledSession`, ~line 116)
- Test: `internal/ddl/edit_test.go`

**Interfaces:**
- Consumes: existing `ddl.KilledSessionFor(criterion, value string, spid int) (KilledSession, bool)`, `ddl.AppendKilledSession`, `ddl.LoadManifestFile`, `sameKilledSession`, `fsutil.AtomicWrite`, `MarshalManifest`.
- Produces: `ddl.RemoveKilledSession(path string, s KilledSession) error` — drops every field-equal kill rule; no-op if absent.

- [ ] **Step 1: Write the failing test**

Add to `internal/ddl/edit_test.go` (package `ddl_test`; `os`, `path/filepath`, `testing`, and `ddl` are already imported):

```go
func TestRemoveKilledSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	body := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rule, ok := ddl.KilledSessionFor("host_name", "SRV1", 0)
	if !ok {
		t.Fatal("KilledSessionFor(host_name) returned ok=false")
	}
	if err := ddl.AppendKilledSession(path, rule); err != nil {
		t.Fatalf("append: %v", err)
	}
	m, err := ddl.LoadManifestFile(path)
	if err != nil || len(m.KillBlockingSessions) != 1 {
		t.Fatalf("after append: %d rules, err=%v", len(m.KillBlockingSessions), err)
	}

	if err := ddl.RemoveKilledSession(path, rule); err != nil {
		t.Fatalf("remove: %v", err)
	}
	m, err = ddl.LoadManifestFile(path)
	if err != nil || len(m.KillBlockingSessions) != 0 {
		t.Fatalf("after remove: %d rules, err=%v", len(m.KillBlockingSessions), err)
	}

	// Removing an absent rule is a no-op (no error).
	if err := ddl.RemoveKilledSession(path, rule); err != nil {
		t.Fatalf("remove absent: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/ddl -run TestRemoveKilledSession -v`
Expected: FAIL — `undefined: ddl.RemoveKilledSession`.

- [ ] **Step 3: Implement `RemoveKilledSession`**

In `internal/ddl/edit.go`, after `AppendKilledSession`:

```go
// RemoveKilledSession drops every kill rule field-equal to s from the manifest at path and
// writes it back atomically, the inverse of AppendKilledSession. Removing a rule that is not
// present is a no-op — the file is rewritten only when something actually changed, so an
// unrelated concurrent reader never sees a needless torn write.
func RemoveKilledSession(path string, s KilledSession) error {
	m, err := LoadManifestFile(path)
	if err != nil {
		return err
	}
	kept := make([]KilledSession, 0, len(m.KillBlockingSessions))
	removed := false
	for _, e := range m.KillBlockingSessions {
		if sameKilledSession(e, s) {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		return nil
	}
	m.KillBlockingSessions = kept
	data, err := MarshalManifest(m)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(path, data); err != nil {
		return fmt.Errorf("write manifest %q: %w", path, err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/ddl -run TestRemoveKilledSession -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ddl/edit.go internal/ddl/edit_test.go
git commit -m "feat(ddl): RemoveKilledSession, the inverse of AppendKilledSession"
```

---

### Task 4: TUI model — roster state, grouping, key handling, and arm/disarm actions

**Files:**
- Modify: `internal/tui/model.go` (action enum ~line 218, `Action` struct, new message, `Model` struct ~line 260, `New` ~line 296, `Update` ~line 345, `handleKey` ~line 433; add `rosterGroup`, `rosterGroups`, `rosterKey`, `handleRosterKey`)
- Test: `internal/tui/model_internal_test.go` (package `tui`)

**Interfaces:**
- Consumes: `tui.SuspensionBlocker.Host` (Task 2).
- Produces:
  - `ActionKind` values `ActionArmKillRule`, `ActionDisarmKillRule`; `Action{Kind, Criterion, Value}` (SPID unused here).
  - `KillerArmedMsg struct{ Armed bool }`.
  - `Model` fields `rosterOpen bool`, `rosterCursor int`, `rosterByHost bool`, `armed map[string]bool`, `killerArmed bool`.
  - `type rosterGroup struct { Criterion, Value string; Count int; TotalMS int64 }`.
  - `func (m Model) rosterGroups() []rosterGroup` — first-seen-ordered aggregation of `m.suspension.Blockers` by the active grouping key; the empty-key group (login/host unknown) has `Value == ""`.
  - `func rosterKey(criterion, value string) string`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/tui/model_internal_test.go`. Extend the import block to:

```go
import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)
```

Then add:

```go
func TestRosterGroupsAggregate(t *testing.T) {
	m := New("op", nil)
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "APP01", Host: "SRV1", Count: 1, TotalMS: 1000},
		{SPID: 105, Login: "APP01", Host: "SRV2", Count: 2, TotalMS: 2000},
	}}
	// Default grouping is by login: one APP01 group, count 3, total 3000.
	g := m.rosterGroups()
	if len(g) != 1 || g[0].Criterion != "login_name" || g[0].Value != "APP01" || g[0].Count != 3 || g[0].TotalMS != 3000 {
		t.Fatalf("login groups = %+v", g)
	}
	// By host: two groups.
	m.rosterByHost = true
	if g := m.rosterGroups(); len(g) != 2 || g[0].Criterion != "host_name" {
		t.Fatalf("host groups = %+v, want 2 by host_name", g)
	}
}

func TestRosterOpenToggleAndClose(t *testing.T) {
	actions := make(chan Action, 8)
	m := New("op", actions)
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "APP01", Host: "SRV1", Count: 1, TotalMS: 1000},
		{SPID: 105, Login: "APP02", Host: "SRV2", Count: 1, TotalMS: 1000},
	}}
	// b opens.
	mo, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	m = mo.(Model)
	if !m.rosterOpen {
		t.Fatal("b should open the roster")
	}
	// g toggles grouping and resets the cursor.
	m.rosterCursor = 1
	mg, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	m = mg.(Model)
	if !m.rosterByHost || m.rosterCursor != 0 {
		t.Fatalf("g should group by host and reset cursor: byHost=%v cursor=%d", m.rosterByHost, m.rosterCursor)
	}
	// q closes the roster without quitting or emitting an action.
	mc, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = mc.(Model)
	if m.rosterOpen || m.quitting || cmd != nil {
		t.Fatalf("q should close roster only: open=%v quitting=%v cmd=%v", m.rosterOpen, m.quitting, cmd)
	}
	select {
	case a := <-actions:
		t.Errorf("closing roster emitted an action: %+v", a)
	default:
	}
}

func TestRosterArmThenDisarm(t *testing.T) {
	actions := make(chan Action, 8)
	m := New("op", actions)
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "APP01", Host: "SRV1", Count: 2, TotalMS: 20000},
	}}
	m.rosterOpen = true

	// space arms the selected (login) group.
	ma, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = ma.(Model)
	a := <-actions
	if a.Kind != ActionArmKillRule || a.Criterion != "login_name" || a.Value != "APP01" {
		t.Fatalf("arm action = %+v", a)
	}
	if !m.armed["login_name=APP01"] {
		t.Error("armed set not updated after arm")
	}

	// space again disarms.
	md, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = md.(Model)
	a = <-actions
	if a.Kind != ActionDisarmKillRule || a.Criterion != "login_name" || a.Value != "APP01" {
		t.Fatalf("disarm action = %+v", a)
	}
	if m.armed["login_name=APP01"] {
		t.Error("armed set not cleared after disarm")
	}
}

func TestRosterUnknownRowIsNotArmable(t *testing.T) {
	actions := make(chan Action, 8)
	m := New("op", actions)
	// A blocker with no login/host (blocker was not in the snapshot).
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{{SPID: 104, Count: 1, TotalMS: 1000}}}
	m.rosterOpen = true
	mu, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = mu.(Model)
	select {
	case a := <-actions:
		t.Errorf("arming the (unknown) row should emit nothing, got %+v", a)
	default:
	}
}

func TestKillerArmedMsg(t *testing.T) {
	m := New("op", nil)
	mo, _ := m.Update(KillerArmedMsg{Armed: true})
	if !mo.(Model).killerArmed {
		t.Error("KillerArmedMsg{Armed:true} should set killerArmed")
	}
}
```

(`strings` is imported now for the Task 5 view test; if Task 5 is implemented separately, it is still used there. If a build complains it is unused at this step, add the Task 5 view test in the same file before running — the two tasks share this test file.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race ./internal/tui -run 'TestRoster|TestKillerArmed' -v`
Expected: FAIL — undefined `rosterGroups`, `ActionArmKillRule`, `KillerArmedMsg`, `Model.rosterOpen`, etc.

- [ ] **Step 3: Add the action kinds, message, and model fields**

In `internal/tui/model.go`, insert into the `ActionKind` const block after `ActionKillBlockerAuto`:

```go
	// ActionArmKillRule appends a kill_blocking_sessions rule (Criterion/Value) to the running
	// manifest without killing now: a session that later blocks the DDL and matches the rule is
	// terminated by the armed BlockerKiller. Emitted from the blocker roster.
	ActionArmKillRule
	// ActionDisarmKillRule removes the matching kill_blocking_sessions rule from the running
	// manifest — the inverse of ActionArmKillRule.
	ActionDisarmKillRule
```

Add a message type to the `type ( ... )` message block:

```go
	// KillerArmedMsg tells the console whether kill_blockers is enabled in config, so the roster
	// can warn that armed rules will not fire until it is. Sent once at startup.
	KillerArmedMsg struct{ Armed bool }
```

Add fields to `Model` (near the other view flags):

```go
	rosterOpen   bool            // the blocker roster modal is showing
	rosterCursor int             // selected group within the roster
	rosterByHost bool            // roster grouping key: false = login, true = host
	armed        map[string]bool // roster-armed kill rules, keyed "criterion=value" (drives ✓)
	killerArmed  bool            // whether kill_blockers is enabled in config (roster warning)
```

Initialize `armed` in `New`:

```go
func New(operation string, actions chan<- Action) Model {
	return Model{operation: operation, status: StatusRunning, actions: actions, showHelp: true, armed: map[string]bool{}}
}
```

Handle the message in `Update` (add a case alongside the others):

```go
	case KillerArmedMsg:
		m.killerArmed = msg.Armed
```

- [ ] **Step 4: Add the grouping helpers**

In `internal/tui/model.go` (near the other `Model` helpers, e.g. after `setOpStatus`):

```go
// rosterGroup is one row of the blocker roster: a distinct login or host that has blocked the
// DDL this run, with its aggregate episode count and total blocked time. Value is "" for the
// non-armable "(unknown)" group (the blocking session was absent from the snapshot).
type rosterGroup struct {
	Criterion string // "login_name" | "host_name"
	Value     string
	Count     int
	TotalMS   int64
}

// rosterKey is the armed-set key for a group: "<criterion>=<value>".
func rosterKey(criterion, value string) string { return criterion + "=" + value }

// rosterGroups folds the suspension history (sessions that blocked us) by the active grouping
// key, summing episode count and total blocked time, preserving first-seen order.
func (m Model) rosterGroups() []rosterGroup {
	criterion := "login_name"
	pick := func(b SuspensionBlocker) string { return b.Login }
	if m.rosterByHost {
		criterion = "host_name"
		pick = func(b SuspensionBlocker) string { return b.Host }
	}
	idx := make(map[string]int)
	var out []rosterGroup
	for _, b := range m.suspension.Blockers {
		v := pick(b)
		i, ok := idx[v]
		if !ok {
			i = len(out)
			idx[v] = i
			out = append(out, rosterGroup{Criterion: criterion, Value: v})
		}
		out[i].Count += b.Count
		out[i].TotalMS += b.TotalMS
	}
	return out
}
```

- [ ] **Step 5: Route and handle roster keys**

In `handleKey`, after the `inCriterionMode` check, add a roster check:

```go
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.inCriterionMode() {
		return m.handleCriterionKey(msg)
	}
	if m.rosterOpen {
		return m.handleRosterKey(msg)
	}
	switch msg.String() {
```

Add the `b` case to the normal-mode switch (anywhere among the cases, e.g. after `"?"`):

```go
	case "b":
		m.rosterOpen = true
		m.rosterCursor = 0
```

Add the handler:

```go
// handleRosterKey drives the blocker-roster modal: navigate groups, toggle the login/host
// grouping, and arm/disarm a kill rule for the selected group. b/esc/q close it (q closes the
// roster, it does not quit the app while the roster is open).
func (m Model) handleRosterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	groups := m.rosterGroups()
	switch msg.String() {
	case "b", "esc", "q":
		m.rosterOpen = false
	case "up":
		if m.rosterCursor > 0 {
			m.rosterCursor--
		}
	case "down":
		if m.rosterCursor < len(groups)-1 {
			m.rosterCursor++
		}
	case "g":
		m.rosterByHost = !m.rosterByHost
		m.rosterCursor = 0
	case "enter", " ":
		if m.rosterCursor >= len(groups) {
			return m, nil
		}
		g := groups[m.rosterCursor]
		if g.Value == "" {
			return m, nil // the (unknown) row has no criterion value to match on
		}
		key := rosterKey(g.Criterion, g.Value)
		if m.armed[key] {
			m.emit(Action{Kind: ActionDisarmKillRule, Criterion: g.Criterion, Value: g.Value})
			delete(m.armed, key)
		} else {
			m.emit(Action{Kind: ActionArmKillRule, Criterion: g.Criterion, Value: g.Value})
			m.armed[key] = true
		}
	}
	return m, nil
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test -race ./internal/tui -run 'TestRoster|TestKillerArmed' -v`
Expected: PASS (the view test `TestRosterViewShowsArmedAndWarning` from Task 5 is still undefined; if it is not yet added, `strings` may be unused — either add Task 5's test now or temporarily drop `strings` from the import until Task 5). All roster logic tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/model.go internal/tui/model_internal_test.go
git commit -m "feat(tui): blocker-roster model state, grouping, and arm/disarm keys"
```

---

### Task 5: TUI view — render the roster modal and the footer hint

**Files:**
- Modify: `internal/tui/view.go` (`View` ~line 20; add `rosterView`; `helpBody` ~line 361)
- Test: `internal/tui/model_internal_test.go`

**Interfaces:**
- Consumes: `rosterGroups`, `rosterKey`, `armed`, `killerArmed`, `rosterByHost`, `rosterCursor` (Task 4); existing styles `titleStyle`, `selStyle`, `okStyle`, `alertStyle`, `helpStyle`; `humanizeMS`.
- Produces: `func (m Model) rosterView() string`.

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/model_internal_test.go`:

```go
func TestRosterViewShowsArmedAndWarning(t *testing.T) {
	m := New("op", nil)
	m.width = 100
	m.killerArmed = false // config has kill_blockers disabled
	m.rosterOpen = true
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "APP01", Host: "SRV1", Count: 2, TotalMS: 20000},
	}}
	m.armed["login_name=APP01"] = true

	out := m.View()
	for _, want := range []string{"APP01", "armed", "kill_blockers disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("roster view missing %q:\n%s", want, out)
		}
	}
}

func TestRosterViewEmpty(t *testing.T) {
	m := New("op", nil)
	m.width = 100
	m.killerArmed = true
	m.rosterOpen = true
	if out := m.View(); !strings.Contains(out, "no blockers yet") {
		t.Errorf("empty roster should say so:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/tui -run TestRosterView -v`
Expected: FAIL — `View` still renders the dashboard; the strings are absent (`rosterView` undefined / not wired).

- [ ] **Step 3: Render the roster and short-circuit `View`**

In `internal/tui/view.go`, at the very top of `View`, before computing `w`:

```go
func (m Model) View() string {
	if m.rosterOpen {
		return m.rosterView()
	}
	w := m.width
```

Add the renderer (near the other body helpers, e.g. after `blockedBody`):

```go
// rosterView renders the blocker-roster modal: every login (or host) that has stalled the DDL
// this run, with its episode count and total blocked time, and a ✓ on armed groups. It replaces
// the dashboard while open (the alt-screen is a fixed height, so a full-screen modal is simplest).
func (m Model) rosterView() string {
	groupBy, other := "login", "host"
	if m.rosterByHost {
		groupBy, other = "host", "login"
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("blockers that stalled this run") + " — grouped by " + groupBy + "\n\n")

	groups := m.rosterGroups()
	if len(groups) == 0 {
		b.WriteString("  (no blockers yet)\n")
	}
	for i, g := range groups {
		label := g.Value
		if label == "" {
			label = "(unknown)"
		}
		line := fmt.Sprintf("%-30s  %d×  blocked %s", label, g.Count, humanizeMS(g.TotalMS))
		if g.Value != "" && m.armed[rosterKey(g.Criterion, g.Value)] {
			line += "  " + okStyle.Render("[✓ armed]")
		}
		marker := "  "
		if i == m.rosterCursor {
			marker = "> "
			line = selStyle.Render(line)
		}
		b.WriteString(marker + line + "\n")
	}

	b.WriteString("\n")
	if !m.killerArmed {
		b.WriteString(alertStyle.Render("kill_blockers disabled in config — armed rules won't fire until it's enabled") + "\n")
	}
	b.WriteString(helpStyle.Render(fmt.Sprintf("[↑/↓] select  [space] arm/disarm  [g] group by %s  [b/esc/q] close", other)))
	return b.String()
}
```

- [ ] **Step 4: Add the footer hint**

In `helpBody`, add `[b] blockers` to the normal shortcut line:

```go
	return helpStyle.Render("[↑/↓] select  [enter] sql  [i] ignore  [x] kill  [X] kill+auto  [b] blockers  [k] kill DDL  [d] drain  [?] help  [q] quit")
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/tui -run 'TestRoster|TestKillerArmed' -v`
Expected: PASS (all roster model + view tests).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/view.go internal/tui/model_internal_test.go
git commit -m "feat(tui): render the blocker-roster modal and footer hint"
```

---

### Task 6: Wire the arm/disarm actions and killer-armed flag in the host

**Files:**
- Modify: `cmd/sqlgopace/main.go` (`dispatchActions` ~line 955; add `armKillRule`/`disarmKillRule` near `killBlockerAuto` ~line 1011; `runWithTUI` signature ~line 600 and its startup sends ~line 609; call site ~line 317)

**Interfaces:**
- Consumes: `tui.ActionArmKillRule`, `tui.ActionDisarmKillRule`, `tui.KillerArmedMsg` (Task 4); `ddl.KilledSessionFor`, `ddl.AppendKilledSession`, `ddl.RemoveKilledSession` (Task 3); `cfg.KillBlockers.Enabled`.
- Produces: no new exported surface — host wiring only.

This is a wiring task. Following the existing pattern in `main.go`, the `ignoreBlocker`/`killBlockerAuto` host handlers are not unit-tested (they need a live `*tui.Program` and `*mssql.Conn`); the arm/disarm handlers mirror them and are verified by the compile + full-suite gate plus the manual smoke below. Their edit logic is already covered by Task 3 (`ddl` layer) and their emission by Task 4 (`tui` layer).

- [ ] **Step 1: Add the arm/disarm handlers**

In `cmd/sqlgopace/main.go`, after `killBlockerAuto`:

```go
// armKillRule appends a kill_blocking_sessions rule (by login/host) to the running manifest
// without killing anyone now — a session that later blocks the DDL and matches is terminated by
// the armed BlockerKiller. It is killBlockerAuto without the immediate KILL: the roster arms
// recurrences, it does not act on a live session. The outcome is echoed to the console.
func armKillRule(program *tui.Program, current *currentManifest, a tui.Action) {
	rule, ok := ddl.KilledSessionFor(a.Criterion, a.Value, a.SPID)
	if !ok {
		program.Send(tui.LogMsg{Line: "arm: nothing to match on for that group"})
		return
	}
	path := current.get()
	if path == "" {
		program.Send(tui.LogMsg{Line: "arm: no manifest is running"})
		return
	}
	if err := ddl.AppendKilledSession(path, rule); err != nil {
		program.Send(tui.LogMsg{Line: "arm: " + err.Error()})
		return
	}
	program.Send(tui.LogMsg{Line: fmt.Sprintf("auto-kill armed by %s=%s — added to manifest", a.Criterion, a.Value)})
}

// disarmKillRule removes the matching kill_blocking_sessions rule from the running manifest — the
// inverse of armKillRule. The outcome is echoed to the console.
func disarmKillRule(program *tui.Program, current *currentManifest, a tui.Action) {
	rule, ok := ddl.KilledSessionFor(a.Criterion, a.Value, a.SPID)
	if !ok {
		program.Send(tui.LogMsg{Line: "disarm: nothing to match on for that group"})
		return
	}
	path := current.get()
	if path == "" {
		program.Send(tui.LogMsg{Line: "disarm: no manifest is running"})
		return
	}
	if err := ddl.RemoveKilledSession(path, rule); err != nil {
		program.Send(tui.LogMsg{Line: "disarm: " + err.Error()})
		return
	}
	program.Send(tui.LogMsg{Line: fmt.Sprintf("auto-kill disarmed by %s=%s — removed from manifest", a.Criterion, a.Value)})
}
```

- [ ] **Step 2: Route the new actions in `dispatchActions`**

In the `switch a.Kind` of `dispatchActions`, after the `tui.ActionKillBlockerAuto` case:

```go
			case tui.ActionArmKillRule:
				armKillRule(program, current, a)
			case tui.ActionDisarmKillRule:
				disarmKillRule(program, current, a)
```

- [ ] **Step 3: Thread the killer-armed flag into the console**

Change `runWithTUI`'s signature to accept the flag (add `killerArmed bool` after `banner`):

```go
func runWithTUI(ctx context.Context, conn *mssql.Conn, engine *run.Engine, current *currentManifest, fwd *tuiForwarder, drain *run.DrainFlag, banner tui.ServerInfoMsg, killerArmed bool, pollInterval, blockingTimeout time.Duration) (run.Summary, error) {
```

Send it at startup, next to the banner send (same goroutine-send pattern to avoid the pre-Run deadlock):

```go
	go program.Send(banner)
	go program.Send(tui.KillerArmedMsg{Armed: killerArmed})
```

Update the call site (~line 317) to pass `cfg.KillBlockers.Enabled`:

```go
			sum, err = runWithTUI(runCtx, dbConn, engine, current, fwd, drain, banner, cfg.KillBlockers.Enabled, cfg.Monitoring.ProgressPoll(), cfg.Monitoring.BlockingTimeout())
```

- [ ] **Step 4: Build, vet, and run the full suite**

Run:
```bash
go build ./... && go vet ./... && make test
```
Expected: build succeeds, vet clean, all tests pass (including the Task 1–5 tests). If `bin/sqlgopace.exe` is locked, stop any running instance first.

- [ ] **Step 5: Commit**

```bash
git add cmd/sqlgopace/main.go
git commit -m "feat: wire blocker-roster arm/disarm actions and killer-armed flag"
```

- [ ] **Step 6: Manual smoke (optional, needs a server + a live blocker)**

With `--tui` against a test DB and `kill_blockers` disabled in config: trigger a blocker, press `b`, confirm the roster lists the blocking login, the footer shows the "kill_blockers disabled" warning, `g` toggles login/host, `space` shows `[✓ armed]` and logs "auto-kill armed by …", `space` again clears it and logs "disarmed", and `q` returns to the dashboard. Inspect the manifest in `02.processing/` to confirm the `kill_blocking_sessions` rule was added then removed.

---

## Self-Review

**Spec coverage:**
- In-memory, this-run scope → reuses `m.suspension.Blockers`, no storage (Tasks 2, 4). ✓
- Population = sessions that blocked us → suspension history (Tasks 1, 2, 4). ✓
- Group by login/host with toggle → `rosterGroups` + `g` key (Task 4). ✓
- Arm timing = append only, killer handles live → `armKillRule` has no `conn.Kill` (Task 6). ✓
- Un-arm in v1 → `ActionDisarmKillRule` + `ddl.RemoveKilledSession` (Tasks 3, 4, 6). ✓
- Open key `b`, close `b/esc/q`, `q` does not quit → `handleRosterKey` + `TestRosterOpenToggleAndClose` (Task 4). ✓
- Config-disabled warning → `killerArmed` + `KillerArmedMsg` + `rosterView` warning (Tasks 4, 5, 6). ✓
- "(unknown)" non-armable row → `rosterGroups` empty-key group + `TestRosterUnknownRowIsNotArmable` (Tasks 4, 5). ✓
- Full-screen modal view, footer hint → `rosterView` + `helpBody` (Task 5). ✓
- Tests: `FindSelfBlock` host, tracker host, model open/close/group/arm/disarm/unknown, view, `RemoveKilledSession` — all present. ✓

**Placeholder scan:** No TBD/TODO; every code step shows complete code. ✓

**Type consistency:** `observe(blocked, spid, login, host, now)` used consistently in Task 2 body and test updates; `SuspensionBlocker.Host`, `rosterGroup{Criterion,Value,Count,TotalMS}`, `rosterKey`, `rosterGroups`, `ActionArmKillRule`/`ActionDisarmKillRule`, `KillerArmedMsg{Armed}`, `RemoveKilledSession(path, KilledSession)` names match across tasks. `runWithTUI` new param `killerArmed bool` placed after `banner` in both signature and call site. ✓

**Note on shared test file:** Tasks 4 and 5 both add to `internal/tui/model_internal_test.go` and share the `strings` import (used only by Task 5's view test). If implementing Task 4 in isolation before Task 5, either add Task 5's view test in the same commit or omit the `strings` import until Task 5 to keep the package compiling.
