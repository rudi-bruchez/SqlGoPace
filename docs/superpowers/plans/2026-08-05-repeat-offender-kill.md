# Repeat-Offender Kill Acceleration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop refunding the full kill dwell to every new SPID, so a blocker (or amplifying victim) that is killed and immediately returns under a new session id is killed again without serving a fresh 60 seconds.

**Architecture:** Both killers key their dwell on the SPID today. Add a small pure type, `recidivism`, that banks observed blocking time per *offender identity* — the matched `kill_blocking_sessions` rule for `BlockerKiller`, the Agent job or connection triplet for `VictimKiller`. Debt is banked one poll at a time and the kill fires when `debt(key) >= after`. A windowed cap (3 kills per identity per 5 minutes) bounds the rollback exposure and escalates to the operator instead of looping.

**Tech Stack:** Go 1.x, standard library only. Tests are `go test -race`, table-free plain subtests in the style already used in `internal/run/kill_test.go` and `internal/run/victim_test.go`, driven by `*ManualClock` or a `func() time.Time` closure over a test-local variable.

**Design spec:** `docs/superpowers/specs/2026-08-05-repeat-offender-kill-design.md` (read it first; the review that shaped it is `…-kimi.md`).

## Global Constraints

- **English only** — every identifier, comment, commit message and doc line.
- **US spelling** in comments and identifiers (`behavior`, not `behaviour`).
- **Idiomatic Go, KISS** — no new interfaces, no generics, no options structs beyond what a task explicitly specifies.
- **No `context.WithTimeout` around executing DDL.** This feature adds no timeouts at all.
- **`make lint` is expected to fail repo-wide** on CRLF/gofmt for pre-existing files; gate on `go build ./...`, `go vet ./...` and `go test -race ./...` instead. Do **not** reformat files you did not otherwise change.
- **No new configuration keys.** `recidivismWindow` and `maxRepeatKills` are unexported constants, following the `killGraceWindow` precedent in `internal/run/victim.go:21`.
- **Never commit client identifiers** (real database, host, table, login or company names). Tests use `SVC_RPT`, `BATCH01`, `SQLPROD01`, `CORP\svc_sqlagent`, `dbo.MEASUREMENT`, `PRODDB`.
- **Nothing that talks to the server, and no sink/callback, may run while a killer's mutex is held.** This is a load-bearing invariant of `VictimKiller.consider` (`internal/run/victim.go:191-211`); Task 6 depends on preserving it.

---

### Task 1: The `recidivism` debt clock

**Files:**
- Create: `internal/run/recidivism.go`
- Test: `internal/run/recidivism_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `newRecidivism(now func() time.Time) *recidivism`; methods `debt(key string) time.Duration`, `accrue(key string, d time.Duration)`, `kills(key string) int`, `recordKill(key string) int`, `undoKill(key string)`, `escalate(key string) bool`, `prune()`, `reset()`. Constants `recidivismWindow = 5 * time.Minute` and `maxRepeatKills = 3`.

- [ ] **Step 1: Write the failing test**

Create `internal/run/recidivism_test.go`:

```go
package run

import (
	"testing"
	"time"
)

// recidivismFor builds a recidivism whose clock is the variable the test advances.
func recidivismFor(now *time.Time) *recidivism {
	return newRecidivism(func() time.Time { return *now })
}

func TestRecidivismAccruesPerKey(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)

	r.accrue("rule-a", 10*time.Second)
	r.accrue("rule-a", 20*time.Second)
	r.accrue("rule-b", 5*time.Second)

	if got := r.debt("rule-a"); got != 30*time.Second {
		t.Errorf("debt(rule-a) = %s, want 30s", got)
	}
	if got := r.debt("rule-b"); got != 5*time.Second {
		t.Errorf("debt(rule-b) = %s, want 5s", got)
	}
	if got := r.debt("never-seen"); got != 0 {
		t.Errorf("debt of an unknown key = %s, want 0", got)
	}
}

func TestRecidivismPrunesOnlyIdleBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)
	r.accrue("stale", 30*time.Second)

	now = now.Add(recidivismWindow - time.Second)
	r.accrue("fresh", 30*time.Second) // touches "fresh" only
	r.prune()
	if r.debt("stale") != 30*time.Second {
		t.Errorf("a bucket idle for less than the window must survive, debt = %s", r.debt("stale"))
	}

	now = now.Add(2 * time.Second) // "stale" is now window+1s idle, "fresh" is 1s idle
	r.prune()
	if r.debt("stale") != 0 {
		t.Errorf("a bucket idle beyond the window must be pruned, debt = %s", r.debt("stale"))
	}
	if r.debt("fresh") != 30*time.Second {
		t.Errorf("the recently touched bucket must survive, debt = %s", r.debt("fresh"))
	}
}

func TestRecidivismKillBudget(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)

	if n := r.recordKill("k"); n != 1 {
		t.Errorf("first recordKill = %d, want 1", n)
	}
	if n := r.recordKill("k"); n != 2 {
		t.Errorf("second recordKill = %d, want 2", n)
	}
	r.undoKill("k")
	if got := r.kills("k"); got != 1 {
		t.Errorf("undoKill must give the budget back, kills = %d, want 1", got)
	}
	r.undoKill("k")
	r.undoKill("k") // one withdrawal too many must not go negative
	if got := r.kills("k"); got != 0 {
		t.Errorf("kills = %d, want 0 and never negative", got)
	}
}

func TestRecidivismEscalatesOncePerBucket(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)
	r.recordKill("k")

	if !r.escalate("k") {
		t.Fatal("the first escalate must return true")
	}
	if r.escalate("k") {
		t.Error("escalate must return false on every later call for the same bucket")
	}

	// A pruned bucket is a new episode of trouble: it may warn again.
	now = now.Add(recidivismWindow + time.Second)
	r.prune()
	if !r.escalate("k") {
		t.Error("a bucket forgotten by prune must be able to escalate again")
	}
}

func TestRecidivismResetDropsEverything(t *testing.T) {
	now := time.Unix(0, 0)
	r := recidivismFor(&now)
	r.accrue("k", time.Minute)
	r.recordKill("k")

	r.reset()

	if r.debt("k") != 0 || r.kills("k") != 0 {
		t.Errorf("reset must clear every bucket, debt = %s kills = %d", r.debt("k"), r.kills("k"))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/run -run TestRecidivism`
Expected: FAIL — `undefined: newRecidivism`, `undefined: recidivismWindow`.

- [ ] **Step 3: Write the implementation**

Create `internal/run/recidivism.go`:

```go
package run

import "time"

// recidivismWindow is how long an offender identity's accumulated blocking debt survives
// without a new block before it is forgotten. Not configurable, for the same reason
// killGraceWindow is not: it separates "one episode of trouble seen through a succession of
// sessions" from "this came back an hour later", and that boundary is a property of the
// mechanism rather than something an operator tunes per run.
const recidivismWindow = 5 * time.Minute

// maxRepeatKills bounds how many times one identity may be killed inside the window before
// the run stops killing it and escalates to the operator. Every KILL buys a rollback, and a
// rollback of a large maintenance statement can cost more than the block it ends; a killer
// that never gives up would trade a blocked run for an unbounded rollback storm. Because the
// counter lives in the bucket, it is forgotten with the debt — this is a rate limit (3 kills
// per identity per window), not a per-manifest quota that would permanently disarm a long run.
const maxRepeatKills = 3

// bucket is one offender identity's accumulated blocking debt inside the window.
type bucket struct {
	accrued    time.Duration // blocking time already observed under this identity
	kills      int
	escalated  bool // the cap warning has been emitted for this bucket
	lastActive time.Time
}

// recidivism accumulates blocking debt per offender identity so a killed offender that
// returns under a new SPID does not buy a fresh dwell. It carries no lock of its own: both
// callers (BlockerKiller, VictimKiller) already hold theirs across every call.
type recidivism struct {
	now     func() time.Time
	buckets map[string]*bucket
}

// newRecidivism returns an empty accumulator. A nil clock defaults to time.Now.
func newRecidivism(now func() time.Time) *recidivism {
	if now == nil {
		now = time.Now
	}
	return &recidivism{now: now, buckets: make(map[string]*bucket)}
}

// touch returns key's bucket, creating it, and marks it active now. Only the mutating
// methods call it: a plain read must not keep a bucket alive past the window.
func (r *recidivism) touch(key string) *bucket {
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{}
		r.buckets[key] = b
	}
	b.lastActive = r.now()
	return b
}

// accrue banks d against key. A non-positive d still marks the identity active, which is
// what an episode's first poll (contributing zero) needs.
func (r *recidivism) accrue(key string, d time.Duration) {
	b := r.touch(key)
	if d > 0 {
		b.accrued += d
	}
}

// debt reports the blocking time banked under key.
func (r *recidivism) debt(key string) time.Duration {
	if b, ok := r.buckets[key]; ok {
		return b.accrued
	}
	return 0
}

// kills reports how many kills key has spent inside the window.
func (r *recidivism) kills(key string) int {
	if b, ok := r.buckets[key]; ok {
		return b.kills
	}
	return 0
}

// recordKill spends one kill from key's budget and returns the new count. VictimKiller calls
// it as a reservation while selecting a target (its KILLs are issued after the lock is
// released, and a scan can select several victims of one identity at once); BlockerKiller
// calls it after a successful KILL.
func (r *recidivism) recordKill(key string) int {
	b := r.touch(key)
	b.kills++
	return b.kills
}

// undoKill returns a reservation whose KILL never landed. It never goes negative.
func (r *recidivism) undoKill(key string) {
	if b, ok := r.buckets[key]; ok && b.kills > 0 {
		b.kills--
	}
}

// escalate reports whether the caller should emit the cap warning: true the first time for
// this bucket, false afterwards, so a capped offender does not warn on every poll.
func (r *recidivism) escalate(key string) bool {
	b := r.touch(key)
	if b.escalated {
		return false
	}
	b.escalated = true
	return true
}

// prune forgets every bucket idle for longer than recidivismWindow. Callers run it at the
// top of a consideration, before any debt is read, so an offender returning after a quiet
// window is judged against a pruned map and serves the full dwell again.
func (r *recidivism) prune() {
	now := r.now()
	for key, b := range r.buckets {
		if now.Sub(b.lastActive) > recidivismWindow {
			delete(r.buckets, key)
		}
	}
}

// reset drops every bucket. Called when a killer is re-armed or disarmed: debt is
// per-manifest state and is never carried across manifests.
func (r *recidivism) reset() {
	r.buckets = make(map[string]*bucket)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/run -run TestRecidivism -v`
Expected: PASS, 5 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/run/recidivism.go internal/run/recidivism_test.go
git commit -m "feat(run): add the per-identity blocking-debt accumulator"
```

---

### Task 2: A stable identity key for a kill rule

**Files:**
- Modify: `internal/run/executor.go` (add `key()` and `String()` on `sessionRule`, after `matches` at `:133-147`)
- Modify: `internal/run/kill.go:20-25` (add `key` to `killRule`), `internal/run/kill.go:42-63` (fill it in `compileKilledSessions`)
- Test: `internal/run/kill_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `func (r sessionRule) key() string` — canonical, injective, stable; `func (r sessionRule) String() string` — operator-facing rendering; `killRule.key string`, populated by `compileKilledSessions`.

**Why the key is quoted:** the design calls the key `sid|app|host|login|stmt`. A regexp source can itself contain `|` (alternation), so a raw join is not injective: `{app: "x|y"}` and `{app: "x", host: "y"}` would collide and merge two rules' debts. Each field is therefore rendered with `strconv.Quote`, which delimits it unambiguously.

- [ ] **Step 1: Write the failing test**

Append to `internal/run/kill_test.go`:

```go
func TestSessionRuleKeyIsStableAndInjective(t *testing.T) {
	compile := func(t *testing.T, rules ...ddl.KilledSession) []killRule {
		t.Helper()
		out, err := compileKilledSessions(rules, 0)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return out
	}

	t.Run("same rule text compiles to the same key", func(t *testing.T) {
		a := compile(t, killRuleFor("^SVC_RPT$", 60))
		b := compile(t, killRuleFor("^SVC_RPT$", 600)) // the delay is not part of the identity
		if a[0].key != b[0].key {
			t.Errorf("same matcher must give the same key: %q vs %q", a[0].key, b[0].key)
		}
	})

	t.Run("different rule text gives a different key", func(t *testing.T) {
		rules := compile(t, killRuleFor("^SVC_RPT$", 0), killRuleFor("^SVC_ETL$", 0))
		if rules[0].key == rules[1].key {
			t.Errorf("different logins must give different keys, both %q", rules[0].key)
		}
	})

	t.Run("an alternation cannot collide with two fields", func(t *testing.T) {
		alt := ddl.KilledSession{IgnoredSession: ddl.IgnoredSession{AppName: "x|y"}}
		split := ddl.KilledSession{IgnoredSession: ddl.IgnoredSession{AppName: "x", HostName: "y"}}
		rules := compile(t, alt, split)
		if rules[0].key == rules[1].key {
			t.Errorf("a | inside a regexp must not collide with the field separator, both %q", rules[0].key)
		}
	})
}

func TestSessionRuleStringOmitsUnsetFields(t *testing.T) {
	rules, err := compileKilledSessions([]ddl.KilledSession{{
		IgnoredSession: ddl.IgnoredSession{AppName: "^SQLAgent", LoginName: `^CORP\\svc_`},
	}}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := rules[0].match.String()
	want := `{app=~"^SQLAgent", login=~"^CORP\\\\svc_"}`
	if got != want {
		t.Errorf("String() = %s, want %s", got, want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/run -run 'TestSessionRule'`
Expected: FAIL — `rules[0].key undefined`, `r.match.String undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/run/executor.go`, add after `matches` (which ends at line 147). **Both `"strconv"` and `"strings"` must be added to that file's import block** — as of this writing it imports only `context`, `errors`, `fmt`, `regexp`, `time`, `ddl` and `mssql` (`executor.go:3-12`). Do not skip this on the assumption that a sibling file's imports apply: `victim.go` has them, `executor.go` does not.

```go
// key renders the rule as a canonical map key for the blocking-debt accumulator: the
// session id and every regexp source, in a fixed order. Each field is quoted rather than
// raw-joined because a regexp source can contain the separator itself ("x|y" as an
// alternation), which would make an unquoted join collide with a two-field rule and merge
// two rules' debts. Derived from the rule's text, so it is stable across the hot reload in
// manifestKillSource.Current: appending a rule mid-run leaves the other keys untouched.
func (r sessionRule) key() string {
	return fmt.Sprintf("%d|%s|%s|%s|%s", r.sessionID,
		strconv.Quote(reSource(r.app)), strconv.Quote(reSource(r.host)),
		strconv.Quote(reSource(r.login)), strconv.Quote(reSource(r.stmt)))
}

// String renders the rule for an operator-facing message, naming only the fields it sets.
// It is deliberately a different rendering from key(): the key must be total and stable,
// this must be readable.
func (r sessionRule) String() string {
	var parts []string
	if r.sessionID != 0 {
		parts = append(parts, fmt.Sprintf("session_id=%d", r.sessionID))
	}
	for _, f := range []struct {
		name string
		re   *regexp.Regexp
	}{
		{"app", r.app}, {"host", r.host}, {"login", r.login}, {"statement", r.stmt},
	} {
		if f.re != nil {
			parts = append(parts, fmt.Sprintf("%s=~%q", f.name, f.re.String()))
		}
	}
	if len(parts) == 0 {
		return "{}" // a rule that sets nothing matches everything
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// reSource returns a regexp's source, empty for an unset (nil) field.
func reSource(re *regexp.Regexp) string {
	if re == nil {
		return ""
	}
	return re.String()
}
```

In `internal/run/kill.go`, extend the rule and populate the key:

```go
// killRule is one compiled kill_blocking_sessions entry: a session matcher (shared with the
// ignore rules), the delay the blocker must persist before it is killed, and the identity
// key its blocking debt accumulates under. The key is computed here, once per compile, not
// per poll: compileKilledSessions rebuilds every killRule on each reload, so a compile-time
// key has exactly the matcher's lifetime and cannot go stale.
type killRule struct {
	match sessionRule
	after time.Duration
	key   string
}
```

and inside `compileKilledSessions`'s loop, replace the assignment:

```go
		out[i] = killRule{match: compiled[i], after: after, key: compiled[i].key()}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/run -run 'TestSessionRule|TestBlockerKiller|TestManifestKillSource' -v`
Expected: PASS — the new tests plus every pre-existing `BlockerKiller` test, unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/run/executor.go internal/run/kill.go internal/run/kill_test.go
git commit -m "feat(run): give each kill rule a stable identity key"
```

---

### Task 3: `BlockerKiller` banks debt per poll

**Files:**
- Modify: `internal/run/kill.go:117-127` (struct), `:131-136` (constructor), `:139-141` (`resetEpisode`), `:145-153` (`SetSource`), `:158-194` (`consider`)
- Test: `internal/run/kill_test.go`

**Interfaces:**
- Consumes: `newRecidivism`, `accrue`, `debt`, `prune`, `reset` (Task 1); `killRule.key` (Task 2).
- Produces: `BlockerKiller.rec *recidivism`, `BlockerKiller.lastPoll time.Time`, `func (k *BlockerKiller) sincePoll(now time.Time) time.Duration`. `KillEvent.Waited` now carries the identity's debt rather than the episode's elapsed time.

- [ ] **Step 1: Write the failing test**

Append to `internal/run/kill_test.go`:

```go
func TestBlockerKillerRecidivistDoesNotBuyAFreshDwell(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)})
	first := blockedSnapshot(100, 104, "SVC_RPT")

	k.consider(context.Background(), first, 100) // banks 0, starts the episode
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), first, 100) // banks 60s -> kill
	if len(rec.spids) != 1 || rec.spids[0] != 104 {
		t.Fatalf("expected the first blocker killed after its dwell, got %v", rec.spids)
	}

	// One second later the same job is back under a new SPID: the debt is already paid.
	now = now.Add(time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)
	if len(rec.spids) != 2 || rec.spids[1] != 155 {
		t.Fatalf("a returning offender must be killed at once, got %v", rec.spids)
	}
}

func TestBlockerKillerDebtIsPerRule(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{
		killRuleFor("^SVC_RPT$", 60),
		killRuleFor("^SVC_ETL$", 60),
	})

	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("setup: the first rule's blocker should have been killed, got %v", rec.spids)
	}

	// A blocker matching a DIFFERENT rule has its own debt and serves the full dwell.
	now = now.Add(time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 200, "SVC_ETL"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("another rule's blocker must not inherit the first rule's debt, got %v", rec.spids)
	}
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 200, "SVC_ETL"), 100)
	if len(rec.spids) != 2 || rec.spids[1] != 200 {
		t.Fatalf("the second rule's blocker should be killed after its own dwell, got %v", rec.spids)
	}
}

func TestBlockerKillerDebtIsForgottenAfterTheWindow(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)})

	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("setup: expected one kill, got %v", rec.spids)
	}

	// Quiet for longer than the window: the bucket is forgotten, the dwell is full again.
	now = now.Add(recidivismWindow + time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("after a quiet window the offender must serve the full dwell, got %v", rec.spids)
	}
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)
	if len(rec.spids) != 2 || rec.spids[1] != 155 {
		t.Fatalf("expected the kill once the full dwell was served again, got %v", rec.spids)
	}
}

func TestBlockerKillerSetSourceClearsTheDebt(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := killerFor(t, rec, &now, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)})

	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("setup: expected one kill, got %v", rec.spids)
	}

	// A new manifest re-arms the killer: debt must not leak across manifests.
	compiled, err := compileKilledSessions([]ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	k.SetSource(staticKill{rules: compiled})

	now = now.Add(time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)
	if len(rec.spids) != 1 {
		t.Fatalf("SetSource must clear the debt, got %v", rec.spids)
	}
}

func TestBlockerKillerReportsTheDebtAsWaited(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	var events []KillEvent
	k := NewBlockerKiller(rec.kill, func(ev KillEvent) { events = append(events, ev) },
		func() time.Time { return now })
	compiled, err := compileKilledSessions([]ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	k.SetSource(staticKill{rules: compiled})

	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 155, "SVC_RPT"), 100)

	if len(events) != 2 {
		t.Fatalf("expected two kill events, got %d", len(events))
	}
	// The recidivist's own episode is one second old; the identity has blocked us for 60s,
	// and that is what the operator must read.
	if events[1].Waited != 60*time.Second {
		t.Errorf("Waited = %s, want the identity's debt (60s)", events[1].Waited)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/run -run 'TestBlockerKillerRecidivist|TestBlockerKillerDebt|TestBlockerKillerSetSourceClears|TestBlockerKillerReportsTheDebt'`
Expected: FAIL — the recidivist is not killed (it serves a fresh dwell) and `Waited` is 0.

- [ ] **Step 3: Write the implementation**

In `internal/run/kill.go`, extend the struct:

```go
type BlockerKiller struct {
	kill   func(context.Context, int) error
	onKill func(KillEvent)
	now    func() time.Time

	mu       sync.Mutex
	src      KillSource
	rec      *recidivism // blocking debt per matched rule, so a returning blocker keeps its dwell
	current  int         // SPID of the blocker in the current episode, 0 when unblocked
	since    time.Time   // when the current blocker first blocked us
	lastPoll time.Time   // previous observation of the current episode; zero on its first poll
	killed   bool        // already issued KILL for the current episode
}
```

Constructor — add the accumulator (the clock defaulting stays where it is, so `rec` gets the same non-nil `now`):

```go
func NewBlockerKiller(kill func(context.Context, int) error, onKill func(KillEvent), now func() time.Time) *BlockerKiller {
	if now == nil {
		now = time.Now
	}
	return &BlockerKiller{kill: kill, onKill: onKill, now: now, rec: newRecidivism(now)}
}
```

Episode helpers — `resetEpisode` keeps its exact meaning (clear the episode, touch no bucket), so no method both accrues and resets:

```go
// resetEpisode clears the current-blocker tracking (called when unblocked or re-sourced).
// It deliberately does not touch the debt: debt is banked poll by poll while the blocker is
// observed, so there is never an unflushed remainder to fold in here.
func (k *BlockerKiller) resetEpisode() {
	k.current, k.since, k.lastPoll, k.killed = 0, time.Time{}, time.Time{}, false
}

// sincePoll returns the time to bank for this poll: zero on an episode's first observation
// (we do not know how long the blocker was there before we saw it, exactly as the previous
// per-episode timer assumed), the inter-poll gap afterwards.
func (k *BlockerKiller) sincePoll(now time.Time) time.Duration {
	if k.lastPoll.IsZero() {
		return 0
	}
	return now.Sub(k.lastPoll)
}
```

`SetSource` — debt is per-manifest state, like the rules it is keyed on:

```go
func (k *BlockerKiller) SetSource(src KillSource) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.src = src
	k.rec.reset()
	k.resetEpisode()
	k.mu.Unlock()
}
```

`consider` — prune first, bank the poll, then decide on the debt:

```go
func (k *BlockerKiller) consider(ctx context.Context, sessions []mssql.Session, ddlSPID int) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.src == nil {
		return
	}
	k.rec.prune() // before any debt is read, so a quiet window really restores the full dwell
	blocker, ok := blockerSession(sessions, ddlSPID)
	if !ok {
		k.resetEpisode()
		return
	}
	now := k.now()
	if blocker.SPID != k.current {
		k.current, k.since, k.lastPoll, k.killed = blocker.SPID, now, time.Time{}, false
	}
	for _, r := range k.src.Current() {
		if !r.match.matches(blocker) {
			continue
		}
		k.rec.accrue(r.key, k.sincePoll(now))
		k.lastPoll = now
		if k.killed {
			// Already killed in this episode. The banking above still runs: a killed session
			// that lingers in rollback is genuinely still blocking us, and counting that time
			// keeps the identity's bucket alive and its debt honest. It cannot cause a second
			// kill — this return is what prevents that — and the debt is already past `after`
			// anyway, so it changes no decision.
			return
		}
		if k.rec.debt(r.key) < r.after {
			return // matched, but this identity has not blocked us long enough yet
		}
		if err := k.kill(ctx, blocker.SPID); err == nil {
			k.killed = true
			k.rec.recordKill(r.key)
			if k.onKill != nil {
				// Waited is the identity's debt, not this episode's elapsed time: for a
				// recidivist the episode is seconds old, and "after 0s blocking the DDL"
				// would misstate why the kill was justified.
				k.onKill(KillEvent{SPID: blocker.SPID, Login: blocker.Login, Waited: k.rec.debt(r.key)})
			}
		}
		return // first matching rule decides; don't fall through to a longer-delay rule
	}
}
```

- [ ] **Step 4: Update the `Waited` doc comment**

`KillEvent.Waited` (`internal/run/kill.go:17`) still says "how long it blocked the DDL before the kill", which is now only true for a first offender. Replace that line:

```go
	Waited time.Duration // how long this identity has blocked the DDL, accumulated across sessions
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/run -run TestBlockerKiller -v`
Expected: PASS — the four new tests and all six pre-existing `BlockerKiller` tests.

- [ ] **Step 6: Run the whole package to catch collateral damage**

Run: `go test -race ./internal/run`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/run/kill.go internal/run/kill_test.go
git commit -m "feat(run): bank a blocker's dwell against its rule, not its SPID"
```

---

### Task 4: The `BlockerKiller` cap, its escalation, and the run report

**Files:**
- Modify: `internal/run/kill.go` (add `sink`, the cap branch, `capDetail`, `SetSink`)
- Modify: `internal/run/engine.go:687` (wire the sink per operation)
- Test: `internal/run/kill_test.go`

**Interfaces:**
- Consumes: `kills`, `recordKill`, `escalate` (Task 1); `sessionRule.String` (Task 2); the debt loop (Task 3).
- Produces: `func (k *BlockerKiller) SetSink(sink ReactionSink)`; a `ReactionEvent{Kind: "warn"}` emitted once per capped identity.

- [ ] **Step 1: Write the failing test**

Append to `internal/run/kill_test.go`:

```go
// killerWithSink builds a killer whose warns are collected, with rules already armed.
func killerWithSink(t *testing.T, rec *killRecorder, now *time.Time, warns *[]string, rules []ddl.KilledSession) *BlockerKiller {
	t.Helper()
	k := killerFor(t, rec, now, rules)
	k.SetSink(func(ev ReactionEvent) {
		if ev.Kind == "warn" {
			*warns = append(*warns, ev.Detail)
		}
	})
	return k
}

func TestBlockerKillerCapsRepeatKillsAndWarnsOnce(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	var warns []string
	k := killerWithSink(t, rec, &now, &warns, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)})

	// Serve the dwell once, then let the offender return under a new SPID repeatedly.
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	now = now.Add(60 * time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 104, "SVC_RPT"), 100)
	for i, spid := range []int{155, 162, 170, 181} {
		now = now.Add(time.Second)
		k.consider(context.Background(), blockedSnapshot(100, spid, "SVC_RPT"), 100)
		_ = i
	}

	if len(rec.spids) != maxRepeatKills {
		t.Fatalf("expected exactly %d kills inside the window, got %v", maxRepeatKills, rec.spids)
	}
	if len(warns) != 1 {
		t.Fatalf("the cap must warn exactly once per identity, got %d warns: %v", len(warns), warns)
	}
	for _, want := range []string{"stopped killing blockers", `login=~"^SVC_RPT$"`, "SPID 181"} {
		if !strings.Contains(warns[0], want) {
			t.Errorf("warn %q must contain %q", warns[0], want)
		}
	}
}

func TestBlockerKillerCapLiftsAfterTheWindow(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	var warns []string
	k := killerWithSink(t, rec, &now, &warns, []ddl.KilledSession{killRuleFor("^SVC_RPT$", 0)}) // kill on sight

	for _, spid := range []int{104, 155, 162, 170} {
		now = now.Add(time.Second)
		k.consider(context.Background(), blockedSnapshot(100, spid, "SVC_RPT"), 100)
	}
	if len(rec.spids) != maxRepeatKills {
		t.Fatalf("setup: expected the cap to bite at %d kills, got %v", maxRepeatKills, rec.spids)
	}

	// Quiet for a full window: the budget and the warn guard are forgotten with the bucket.
	now = now.Add(recidivismWindow + time.Second)
	k.consider(context.Background(), blockedSnapshot(100, 200, "SVC_RPT"), 100)
	if len(rec.spids) != maxRepeatKills+1 {
		t.Fatalf("the cap must lift after a quiet window, got %v", rec.spids)
	}
}

func TestBlockerKillerFailedKillSpendsNoBudget(t *testing.T) {
	failing := func(_ context.Context, _ int) error { return errors.New("permission denied") }
	now := time.Unix(0, 0)
	k := NewBlockerKiller(failing, nil, func() time.Time { return now })
	compiled, err := compileKilledSessions([]ddl.KilledSession{killRuleFor("^SVC_RPT$", 0)}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	k.SetSource(staticKill{rules: compiled})

	for _, spid := range []int{104, 155, 162, 170, 181} {
		now = now.Add(time.Second)
		k.consider(context.Background(), blockedSnapshot(100, spid, "SVC_RPT"), 100)
	}
	// Every KILL failed, so no budget was spent and the killer never reached the cap.
	if got := k.rec.kills(k.src.Current()[0].key); got != 0 {
		t.Errorf("a failed KILL must spend no budget, kills = %d", got)
	}
}
```

Add `"errors"` and `"strings"` to `kill_test.go`'s imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/run -run 'TestBlockerKillerCap|TestBlockerKillerFailedKill'`
Expected: FAIL — `k.SetSink undefined`, and the killer kills five times instead of three.

- [ ] **Step 3: Write the implementation**

In `internal/run/kill.go`, add the `sink` field to the struct (next to `src`):

```go
	sink     ReactionSink // per-operation run-report sink; nil between operations
```

Add the setter next to `SetSource`:

```go
// SetSink installs the reaction sink the cap escalation is emitted on. The engine sets it
// per operation and SetSource clears it between manifests, for the same reason
// VictimKiller.SetSink exists: a late event must never be attributed to the next
// operation's report. Kills themselves keep reporting through onKill to the console.
func (k *BlockerKiller) SetSink(sink ReactionSink) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.sink = sink
	k.mu.Unlock()
}
```

In `SetSource`, clear it as well — add `k.sink = nil` next to `k.src = src`.

Add the cap branch inside `consider`'s rule loop, between the debt check and the `k.kill` call:

```go
		if k.rec.kills(r.key) >= maxRepeatKills {
			// Capped: go quiet and let the normal reaction hierarchy take over — which is
			// exactly the behavior without this feature, so the run can never block longer
			// for having tried. The warn fires once, on the transition.
			if k.rec.escalate(r.key) && k.sink != nil {
				k.sink(ReactionEvent{Kind: "warn", Detail: capDetail(r, blocker)})
			}
			return
		}
```

And the narration, at the end of the file:

```go
// capDetail narrates a rule that hit the repeat-kill cap: what was killed, how often, and
// what the operator can do about it. It names the rule rather than only the last session,
// because with a population of offenders the individual SPIDs are noise.
func capDetail(r killRule, blocker mssql.Session) string {
	return fmt.Sprintf("stopped killing blockers matching rule %s: %d killed within %s and they keep returning "+
		"(last: SPID %d, login=%s host=%s program=%s) — the blocker is being restarted faster than it can be "+
		"cleared, so the run falls back to the normal blocking reaction; consider disabling the job behind it or "+
		"scheduling this run outside its window",
		r.match, maxRepeatKills, recidivismWindow, blocker.SPID, blocker.Login, blocker.Host, blocker.Program)
}
```

Add `"fmt"` to `kill.go`'s imports.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/run -run TestBlockerKiller -v`
Expected: PASS.

- [ ] **Step 5: Wire the sink in the engine**

In `internal/run/engine.go`, immediately after the existing `e.victims.SetSink(sink)` (line 687), add:

```go
			// The blocker killer emits its cap escalation on the same per-operation sink,
			// from the same pump goroutine, and is cleared by SetSource(nil) at the end of
			// the manifest.
			e.killer.SetSink(sink)
```

`SetSink` is nil-receiver safe, so no guard is needed — this mirrors `e.victims.SetSink`.

- [ ] **Step 6: Verify the build and the full package**

Run: `go build ./... && go vet ./... && go test -race ./internal/run ./cmd/sqlgopace`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/run/kill.go internal/run/kill_test.go internal/run/engine.go
git commit -m "feat(run): cap repeat blocker kills and escalate to the operator"
```

---

### Task 5: `VictimKiller` banks debt per offender identity

**Files:**
- Modify: `internal/run/victim.go:48-54` (episode), `:67-80` (struct), `:91-112` (constructor), `:126-153` (`Arm`/`Disarm`), `:278-317` (`considerLocked`), `:384-407` (`killEvent`)
- Test: `internal/run/victim_test.go`

**Interfaces:**
- Consumes: Task 1's accumulator.
- Produces: `func victimKey(v mssql.Session) string`; `victimEpisode.lastPoll time.Time`; `func (e *victimEpisode) sincePoll(now time.Time) time.Duration`; `VictimKiller.rec *recidivism`. `AmplifierKillEvent.Waited` now carries the identity's debt.

- [ ] **Step 1: Write the failing test**

Append to `internal/run/victim_test.go`. Reuse whatever snapshot helper that file already defines; the sessions below are written out in full so the test does not depend on one:

```go
func TestVictimKeyIdentifiesTheJobNotTheSession(t *testing.T) {
	// Two different sessions running two different statements for the same Agent job step
	// are one offender.
	a := mssql.Session{SPID: 79, Program: `SQLAgent - TSQL JobStep (Job 0x1A2B Step 1)`,
		Command: "UPDATE STATISTICS", ActiveQuery: "UPDATE STATISTICS dbo.MEASUREMENT"}
	b := mssql.Session{SPID: 91, Program: `SQLAgent - TSQL JobStep (Job 0x1A2B Step 1)`,
		Command: "ALTER INDEX", ActiveQuery: "ALTER INDEX ALL ON dbo.MEASUREMENT REORGANIZE"}
	if victimKey(a) != victimKey(b) {
		t.Errorf("one job step must be one identity: %q vs %q", victimKey(a), victimKey(b))
	}

	// A program name that is not a job step falls back to the connection triplet, and the
	// statement is still not part of the identity.
	c := mssql.Session{SPID: 54, Login: `CORP\svc_etl`, Host: "SQLPROD01", Program: "ETL",
		ActiveQuery: "UPDATE STATISTICS dbo.MEASUREMENT"}
	d := mssql.Session{SPID: 61, Login: `CORP\svc_etl`, Host: "SQLPROD01", Program: "ETL",
		ActiveQuery: "ALTER INDEX ALL ON dbo.OTHER REBUILD"}
	if victimKey(c) != victimKey(d) {
		t.Errorf("one connection identity must be one offender: %q vs %q", victimKey(c), victimKey(d))
	}
	if victimKey(a) == victimKey(c) {
		t.Error("a job step and a plain connection must not share an identity")
	}
}

func TestVictimKillerRecidivistDoesNotBuyAFreshDwell(t *testing.T) {
	killed := make(chan int, 8)
	clk := NewManualClock(time.Unix(0, 0))
	k := NewVictimKiller(
		func(_ context.Context, spid int) error { killed <- spid; return nil },
		nil, nil, clk, "SqlGoPace")
	k.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 60 * time.Second})

	// Our DDL is SPID 100. Victim 79 is directly blocked by us and has SPID 91 queued
	// behind it, which is what makes it an amplifier.
	snap := func(victim int) []mssql.Session {
		return []mssql.Session{
			{SPID: 100},
			{SPID: victim, BlockingSPID: 100, Command: "UPDATE STATISTICS",
				Program: `SQLAgent - TSQL JobStep (Job 0x1A2B Step 1)`},
			{SPID: 91, BlockingSPID: victim, Command: "SELECT"},
		}
	}

	k.consider(context.Background(), snap(79), 100, nil) // banks 0
	clk.Advance(60 * time.Second)
	k.consider(context.Background(), snap(79), 100, nil) // banks 60s -> kill
	if got := <-killed; got != 79 {
		t.Fatalf("expected the first victim killed after its dwell, got %d", got)
	}

	// The job restarts a second later under a new SPID: the identity already paid.
	clk.Advance(time.Second)
	k.consider(context.Background(), snap(155), 100, nil)
	select {
	case got := <-killed:
		if got != 155 {
			t.Fatalf("expected the returning job killed, got %d", got)
		}
	default:
		t.Fatal("a returning offender must be killed at once, no kill was issued")
	}
}
```

Add `"context"`, `"time"` and the `mssql` import to `victim_test.go` if they are not already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/run -run 'TestVictimKey|TestVictimKillerRecidivist'`
Expected: FAIL — `undefined: victimKey`, and the returning victim is not killed.

- [ ] **Step 3: Write the implementation**

In `internal/run/victim.go`, extend the episode:

```go
// victimEpisode is the per-SPID state of one blocking episode.
type victimEpisode struct {
	since      time.Time // when this victim first became kill-eligible
	lastPoll   time.Time // previous observation of this episode; zero on its first poll
	killed     bool      // a KILL was issued successfully
	killedAt   time.Time // when, for the grace window
	killFailed bool      // the KILL errored: stop suppressing, fall back to yielding
}

// sincePoll returns the time to bank for this poll: zero on the episode's first
// observation, the inter-poll gap afterwards.
func (e *victimEpisode) sincePoll(now time.Time) time.Duration {
	if e.lastPoll.IsZero() {
		return 0
	}
	return now.Sub(e.lastPoll)
}
```

Add the identity function next to it:

```go
// victimKey identifies the offender behind a victim session: the Agent job step when the
// program name resolves to one, else the connection triplet. It deliberately excludes the
// command verb and the statement text — a job that runs a different statement after a
// restart, or the same statement against another object, is the same offender.
func victimKey(v mssql.Session) string {
	if hex, step, ok := mssql.ParseJobStepProgram(v.Program); ok {
		return fmt.Sprintf("job:%s:%d", hex, step)
	}
	return fmt.Sprintf("sess:%s|%s|%s", v.Login, v.Host, v.Program)
}
```

Add `rec *recidivism` to the `VictimKiller` struct (under `mu`, with the other guarded state) and build it in `NewVictimKiller` after the clock default:

```go
		rec: newRecidivism(clk.Now),
```

`Arm` and `Disarm` clear it alongside the episodes — add `k.rec.reset()` next to each `k.episodes = make(map[int]*victimEpisode)`.

In `considerLocked`, prune first and swap the dwell comparison. The eligibility scan becomes:

```go
	now := k.clk.Now()
	k.rec.prune() // before any debt is read: a quiet window restores the full dwell
	seen := make(map[int]bool)
	for _, v := range DirectVictims(sessions, ddlSPID) {
		if !k.eligible(v, sessions, ddlSPID, ignore) {
			continue
		}
		key := victimKey(v)
		seen[v.SPID] = true
		ep, ok := k.episodes[v.SPID]
		if !ok {
			ep = &victimEpisode{since: now}
			k.episodes[v.SPID] = ep
		}
		k.rec.accrue(key, ep.sincePoll(now))
		ep.lastPoll = now
		if ep.killed || ep.killFailed || k.rec.debt(key) < k.policy.After {
			continue
		}
		ep.killed, ep.killedAt = true, now
		targets = append(targets, killTarget{
			ep: ep, key: key, spid: v.SPID, command: v.Command,
			kill: killEvent(v, sessions, ep.since, k.rec.debt(key)),
		})
	}
```

Add the `key` field to `killTarget` (Task 6 uses it; adding it now keeps the struct in one place):

```go
type killTarget struct {
	ep      *victimEpisode
	key     string // the offender identity this kill is charged to
	spid    int
	command string
	kill    *AmplifierKillEvent
}
```

`killEvent`'s last parameter keeps its name and type; only its meaning changes, so update its doc comment:

```go
// killEvent builds the not-yet-attributed event for a kill about to be issued: everything
// except Job, which attribute fills once the lock from considerLocked is released. waited is
// the offender identity's accumulated debt, not this episode's elapsed time — for a job that
// restarts under a new SPID the episode is seconds old, and the debt is what justified the
// kill. firstEligible still dates this episode.
```

And the field itself, `AmplifierKillEvent.Waited` (`internal/run/victim.go:44`), which still says "how long it was kill-eligible before we killed it":

```go
	Waited        time.Duration // how long this identity has been kill-eligible, accumulated across sessions
```

`victim.go` already imports `"fmt"` (`victim.go:5`), so `victimKey` needs no import change.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/run -run 'TestVictim' -v`
Expected: PASS — the new tests and every pre-existing `VictimKiller` test.

- [ ] **Step 5: Commit**

```bash
git add internal/run/victim.go internal/run/victim_test.go
git commit -m "feat(run): bank an amplifier's dwell against its job identity"
```

---

### Task 6: The `VictimKiller` cap, by reservation

**Files:**
- Modify: `internal/run/victim.go` (`considerLocked` cap branch and return signature, `consider`, `withdrawKill`, `abandonKill`, `victimCapDetail`)
- Test: `internal/run/victim_test.go`

**Interfaces:**
- Consumes: `kills`, `recordKill`, `undoKill`, `escalate` (Task 1); `killTarget.key` (Task 5).
- Produces: `considerLocked` returns `(targets []killTarget, capped []ReactionEvent, sink ReactionSink, onKill func(AmplifierKillEvent))`; `withdrawKill(ep *victimEpisode, key string)`; `abandonKill(ep *victimEpisode, key string)`; `func victimCapDetail(v mssql.Session) string`.

**The one place this departs from the review:** the counter is *reserved* in the locked scan rather than incremented after a successful KILL. `considerLocked` can select several victims of one identity in a single scan, and the KILLs are issued after the lock is released; an increment-on-success counter would still read zero for all of them and the cap would be exceeded by the number of targets in that scan. Reservation mirrors the optimistic `ep.killed` mark the code already uses, and the withdrawal paths give the budget back so a failed KILL still costs nothing.

**The suppression trap:** a capped identity must create no episode *and* must not be marked `seen`, so the existing end-of-scan sweep (`victim.go:310-315`) drops its episode once the post-kill grace expires. `Suppressed` then returns false and the victim counts toward `BlockState.Unignored` again — without this, a capped victim would be suppressed forever while no kill would ever come, and the run would block indefinitely.

- [ ] **Step 1: Write the failing test**

Append to `internal/run/victim_test.go`:

```go
// amplifierSnapshot builds a snapshot where each victim SPID is directly blocked by ddlSPID
// and has one session queued behind it, all belonging to the same Agent job step.
func amplifierSnapshot(ddlSPID int, victims ...int) []mssql.Session {
	out := []mssql.Session{{SPID: ddlSPID}}
	queued := 900
	for _, v := range victims {
		out = append(out, mssql.Session{SPID: v, BlockingSPID: ddlSPID, Command: "UPDATE STATISTICS",
			Program: `SQLAgent - TSQL JobStep (Job 0x1A2B Step 1)`})
		out = append(out, mssql.Session{SPID: queued, BlockingSPID: v, Command: "SELECT"})
		queued++
	}
	return out
}

func TestVictimKillerCapHoldsWithinOneScan(t *testing.T) {
	var mu sync.Mutex
	var killed []int
	clk := NewManualClock(time.Unix(0, 0))
	k := NewVictimKiller(
		func(_ context.Context, spid int) error {
			mu.Lock()
			defer mu.Unlock()
			killed = append(killed, spid)
			return nil
		},
		nil, nil, clk, "SqlGoPace")
	k.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 0}) // kill on sight

	// Five sessions of ONE identity, all eligible in a single scan. The cap must hold even
	// though every KILL is issued after the lock is released.
	k.consider(context.Background(), amplifierSnapshot(100, 79, 91, 104, 155, 162), 100, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(killed) != maxRepeatKills {
		t.Fatalf("one scan must not exceed the cap: killed %v, want %d kills", killed, maxRepeatKills)
	}
}

func TestVictimKillerCappedVictimStopsBeingSuppressed(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	k := NewVictimKiller(
		func(_ context.Context, _ int) error { return nil },
		nil, nil, clk, "SqlGoPace")
	k.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 0})

	for _, v := range []int{79, 91, 104} {
		k.consider(context.Background(), amplifierSnapshot(100, v), 100, nil)
		clk.Advance(time.Second)
	}

	// The identity is now capped. A fresh victim of it must not be suppressed, or the run
	// would never yield to it either.
	clk.Advance(killGraceWindow + time.Second)
	k.consider(context.Background(), amplifierSnapshot(100, 155), 100, nil)
	if k.Suppressed(155) {
		t.Error("a capped victim must not be suppressed: no kill is coming, so the run must yield")
	}
}

func TestVictimKillerCapWarnsOnce(t *testing.T) {
	var mu sync.Mutex
	var warns []string
	clk := NewManualClock(time.Unix(0, 0))
	k := NewVictimKiller(
		func(_ context.Context, _ int) error { return nil },
		nil, nil, clk, "SqlGoPace")
	k.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 0})
	k.SetSink(func(ev ReactionEvent) {
		if ev.Kind == "warn" {
			mu.Lock()
			warns = append(warns, ev.Detail)
			mu.Unlock()
		}
	})

	for _, v := range []int{79, 91, 104, 155, 162} {
		k.consider(context.Background(), amplifierSnapshot(100, v), 100, nil)
		clk.Advance(time.Second)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(warns) != 1 {
		t.Fatalf("the cap must warn exactly once per identity, got %d: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], "stopped killing amplifying maintenance sessions") {
		t.Errorf("unexpected warn text: %q", warns[0])
	}
}

func TestVictimKillerFailedKillReturnsTheBudget(t *testing.T) {
	clk := NewManualClock(time.Unix(0, 0))
	k := NewVictimKiller(
		func(_ context.Context, _ int) error { return errors.New("permission denied") },
		nil, nil, clk, "SqlGoPace")
	k.Arm(AmplifierPolicy{MinBlockedBehind: 1, After: 0})

	for _, v := range []int{79, 91, 104, 155} {
		k.consider(context.Background(), amplifierSnapshot(100, v), 100, nil)
		clk.Advance(time.Second)
	}

	key := victimKey(mssql.Session{Program: `SQLAgent - TSQL JobStep (Job 0x1A2B Step 1)`})
	k.mu.Lock()
	spent := k.rec.kills(key)
	k.mu.Unlock()
	if spent != 0 {
		t.Errorf("failed KILLs must spend no budget, kills = %d", spent)
	}
}
```

Add `"errors"`, `"strings"` and `"sync"` to `victim_test.go`'s imports if missing.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/run -run 'TestVictimKillerCap|TestVictimKillerFailedKill'`
Expected: FAIL — five kills instead of three, no warn emitted.

- [ ] **Step 3: Write the implementation**

In `internal/run/victim.go`, add the cap branch to `considerLocked`'s loop, immediately after `key := victimKey(v)` and **before** `seen[v.SPID] = true`:

```go
		if k.rec.kills(key) >= maxRepeatKills {
			// Capped: issue no kill, and deliberately create no episode and leave this SPID
			// out of `seen`, so the sweep below drops any episode it still has once the grace
			// window ends. Suppressed then goes false and the victim counts toward the yield
			// timer again — the alternative would suppress it forever while no kill is coming.
			if k.rec.escalate(key) {
				capped = append(capped, ReactionEvent{Kind: "warn", Detail: victimCapDetail(v)})
			}
			continue
		}
```

Reserve the kill where the target is selected, right after `ep.killed, ep.killedAt = true, now`:

```go
		k.rec.recordKill(key) // reserved here, not after the KILL: see the task notes
```

Change `considerLocked`'s signature and return to carry the capped warnings out of the lock — **the sink must never be called while `k.mu` is held**:

```go
func (k *VictimKiller) considerLocked(sessions []mssql.Session, ddlSPID int, ignore IgnoredSessions) (targets []killTarget, capped []ReactionEvent, sink ReactionSink, onKill func(AmplifierKillEvent)) {
```

then update both of its returns: the `!k.armed` guard becomes `return nil, nil, sink, onKill`, and the final one becomes `return targets, capped, sink, onKill`.

In `consider`, emit them after the lock is released and before the kills:

```go
	targets, capped, sink, onKill := k.considerLocked(sessions, ddlSPID, ignore)
	if sink != nil {
		for _, ev := range capped {
			sink(ev)
		}
	}
	if len(targets) == 0 {
		return
	}
```

Give the budget back on both withdrawal paths:

```go
func (k *VictimKiller) abandonKill(ep *victimEpisode, key string) {
	k.mu.Lock()
	ep.killed = false
	k.rec.undoKill(key)
	k.mu.Unlock()
}

func (k *VictimKiller) withdrawKill(ep *victimEpisode, key string) {
	k.mu.Lock()
	ep.killed, ep.killFailed = false, true
	k.rec.undoKill(key)
	k.mu.Unlock()
}
```

and update their two call sites in `consider` to `k.abandonKill(t.ep, t.key)` and `k.withdrawKill(t.ep, t.key)`.

Add the narration at the end of the file:

```go
// victimCapDetail narrates an amplifier identity that hit the repeat-kill cap. It names the
// job when the program name carries one, because that is what the operator acts on.
func victimCapDetail(v mssql.Session) string {
	return fmt.Sprintf("stopped killing amplifying maintenance sessions from %s: %d killed within %s and they keep "+
		"returning (last: SPID %d, %s, login=%s host=%s) — the run falls back to the normal blocking reaction; "+
		"consider disabling the job behind it or scheduling this run outside its window",
		v.Program, maxRepeatKills, recidivismWindow, v.SPID, v.Command, v.Login, v.Host)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/run -run TestVictim -v`
Expected: PASS.

- [ ] **Step 5: Verify the whole package and the CLI**

Run: `go build ./... && go vet ./... && go test -race ./internal/... ./cmd/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/run/victim.go internal/run/victim_test.go
git commit -m "feat(run): cap repeat amplifier kills by reservation"
```

---

### Task 7: The sampler seam, across a poll gap

**Files:**
- Test: `internal/run/executor_test.go` (append)

**Interfaces:**
- Consumes: everything from Tasks 1–4.
- Produces: nothing. This task only adds a regression test.

**Why here rather than in `shrink_driver_test.go`:** the killers are consulted from `ServerSampler.Blocking` (`executor.go:326`), which the shrink driver reaches through `pumpSamples` in both `runChunk` (`shrink.go:696`) and `runWatchedStatement` (`shrink.go:779`). From a killer's point of view a chunk boundary is indistinguishable from any other gap between polls — it only ever sees a sequence of `Blocking` calls with time between them. Driving `ServerSampler` directly with a probe that returns a different blocker on each call therefore reproduces exactly the shrink scenario, with real production types on the path being tested, and without the shrink driver's execution fakes.

- [ ] **Step 1: Write the failing test**

Append to `internal/run/executor_test.go`:

```go
// recurringBlockerProbe is a sampleProbe whose ActiveSessions returns a different blocker
// SPID on each call — one connection-pool retry or Agent job restart per poll.
type recurringBlockerProbe struct {
	calls  int
	blockers []int
}

func (p *recurringBlockerProbe) LogSpace(context.Context) (mssql.LogSpace, error) {
	return mssql.LogSpace{}, nil
}

func (p *recurringBlockerProbe) LogReuseWait(context.Context) (string, error) { return "NOTHING", nil }

func (p *recurringBlockerProbe) ActiveSessions(context.Context) ([]mssql.Session, error) {
	spid := p.blockers[min(p.calls, len(p.blockers)-1)]
	p.calls++
	return []mssql.Session{
		{SPID: 100, BlockingSPID: spid, WaitType: "LCK_M_SCH_M"},
		{SPID: spid, Login: "SVC_RPT", Host: "BATCH01", Program: "Reporting"},
	}, nil
}

func TestSamplerKillsARecurringBlockerAcrossPollGaps(t *testing.T) {
	rec := &killRecorder{}
	now := time.Unix(0, 0)
	k := NewBlockerKiller(rec.kill, nil, func() time.Time { return now })
	compiled, err := compileKilledSessions([]ddl.KilledSession{killRuleFor("^SVC_RPT$", 60)}, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	k.SetSource(staticKill{rules: compiled})

	probe := &recurringBlockerProbe{blockers: []int{104, 104, 155}}
	s := NewServerSampler(probe, 100, 0, 0)
	s.SetKiller(k)

	// Poll 1 and 2 are the first chunk: the blocker serves its dwell and is killed.
	if _, err := s.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking: %v", err)
	}
	now = now.Add(60 * time.Second)
	if _, err := s.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking: %v", err)
	}
	if len(rec.spids) != 1 || rec.spids[0] != 104 {
		t.Fatalf("expected SPID 104 killed after its dwell, got %v", rec.spids)
	}

	// The chunk ends; nothing is sampled for two minutes; the next chunk's first poll finds
	// the offender back under a new SPID. It must not buy a fresh 60 seconds.
	now = now.Add(2 * time.Minute)
	if _, err := s.Blocking(context.Background(), nil); err != nil {
		t.Fatalf("Blocking: %v", err)
	}
	if len(rec.spids) != 2 || rec.spids[1] != 155 {
		t.Fatalf("a blocker returning after a chunk gap must be killed at once, got %v", rec.spids)
	}
}
```

Ensure `executor_test.go` imports `"context"`, `"time"`, `"testing"`, `ddl` and `mssql`.

- [ ] **Step 2: Run the test to verify it fails against the pre-feature behavior**

This is a demonstration, not a requirement — skip it if you have unrelated unstaged work, because `git stash` takes *everything* in the working tree, not just this task's test file.

Run: `git stash push -- internal/run/kill.go internal/run/recidivism.go && go test ./internal/run -run TestSamplerKillsARecurringBlocker; git stash pop`
Expected: FAIL — without the debt clock the returning blocker serves a fresh dwell and is never killed. This is the regression the feature fixes.

A safer alternative if the working tree is dirty: `git worktree add ../sqlgopace-preverify <commit-before-task-3>`, copy the test file in, run it there, and remove the worktree.

- [ ] **Step 3: Run the test against the implementation**

Run: `go test -race ./internal/run -run TestSamplerKillsARecurringBlocker -v`
Expected: PASS. No production code changes are needed in this task; if it fails, the defect is in Tasks 3–4.

- [ ] **Step 4: Commit**

```bash
git add internal/run/executor_test.go
git commit -m "test(run): cover a recurring blocker across a poll gap"
```

---

### Task 8: Document the behavior

**Files:**
- Modify: `README.md` (the `kill_blocking_sessions` section added on 2026-08-05, and the `kill_amplifying_maintenance` section)
- Modify: `docs/llm-operator-guide.md` (the "Session policy: get the direction right" section)
- Modify: `internal/run/kill.go` (the `consider` doc comment, which still says "has blocked for at least that rule's delay")

Note: `specs/SPECS.md` does not document the blocker kill at all (`grep kill_blocking_sessions specs/SPECS.md` returns nothing) — the feature is specified in `docs/superpowers/specs/2026-07-22-blocker-roster-autokill-design.md`, which is a historical design record and is not retro-edited. There is nothing to change under `specs/`.

**Interfaces:**
- Consumes: the finished behavior.
- Produces: nothing in code.

- [ ] **Step 1: Add the README paragraph for `kill_blocking_sessions`**

In `README.md`, in the section "Killing the sessions that block you: `kill_blocking_sessions`", after the paragraph that begins "The delay is measured from when that session started blocking us", insert:

```markdown
**A returning offender does not buy a fresh delay.** The delay is served by the *rule*, not
by the session id. A blocker that is killed and comes straight back under a new SPID — an
Agent job restarting its step, a connection pool retrying, or the next session of a
population that all match the same rule — inherits the blocking time already served under
that rule, so it is killed on the next poll instead of buying another full delay. Blocking
time is banked per rule for five minutes of quiet, after which the rule is forgotten and the
full delay applies again.

To bound the cost of that, one rule kills at most three sessions in any five-minute window.
On the fourth, SqlGoPace stops killing, writes a `warn` into the run report naming the rule
and the last offender, and falls back to the normal blocking reaction — the behavior you
would get with the feature off. A blocker being restarted faster than it can be cleared is
an operator problem (disable the job, or move the run outside its window), and an unbounded
kill loop would trade a blocked run for a rollback storm.
```

- [ ] **Step 2: Add the matching note for `kill_amplifying_maintenance`**

In `README.md`, at the end of the `kill_amplifying_maintenance` section, after the paragraph about a failed `KILL` falling back to the normal yield, insert:

```markdown
The same repeat-offender rule applies here, keyed on the Agent job (or, when the program
name is not a job step, on the login/host/program triplet) rather than on a rule: a job
whose step restarts after being killed inherits the dwell it already served, and the same
three-kills-per-five-minutes cap applies before the run gives up on it and says so in the
report.
```

- [ ] **Step 3: Update the LLM operator guide**

In `docs/llm-operator-guide.md`, in the section "Session policy: get the direction right before you write a rule", append to the paragraph that begins "`kill_blocking_sessions` is inert unless":

```markdown
The delay is served by the rule, not by the session: an offender killed and returning under
a new SPID inherits the time already served, and one rule kills at most three sessions per
five minutes before the run reports and falls back to yielding. Do not suggest a very short
`after_seconds` to "keep up" with a restarting job — that is the case the mechanism already
handles, and a short delay only makes the run kill innocent short-lived sessions.
```

- [ ] **Step 4: Fix the stale doc comment and check for others**

In `internal/run/kill.go`, `consider`'s doc comment says the blocker is killed "when it matches a rule and has blocked for at least that rule's delay". Reword:

```go
// consider inspects one blocking snapshot and kills the session blocking ddlSPID when it
// matches a rule and the identity that rule names has blocked us for at least that rule's
// delay — accumulated across sessions, so a killed blocker returning under a new SPID does
// not restart the clock (see recidivism.go). A no-op when no source is set or no rule
// matches. Each blocker is killed at most once per episode, and each identity at most
// maxRepeatKills times per recidivismWindow.
```

Then run: `grep -rn "blocked continuously\|blocked for at least\|blocks continuously" README.md internal/ --include='*.md' --include='*.go'`
Expected: review each remaining hit; reword any that states the kill delay is per session. Hits about `max_block_minutes` (continuous blocking of *any* session, unrelated to this feature) are correct as they stand — leave them.

- [ ] **Step 5: Final verification**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add README.md docs/llm-operator-guide.md internal/run/kill.go
git commit -m "docs: document the repeat-offender kill acceleration"
```

---

## Verification checklist

Run before considering the feature done:

```bash
go build ./...
go vet ./...
go test -race ./...
```

Manual review points, in order of how badly they would hurt if wrong:

1. **No sink or callback is invoked while a killer's mutex is held.** Re-read `VictimKiller.consider` and confirm the `capped` events are emitted after `considerLocked` returns.
2. **A capped victim is not suppressed.** `TestVictimKillerCappedVictimStopsBeingSuppressed` covers it; the failure mode is a run that blocks forever.
3. **A failed KILL spends no budget** on both killers.
4. **Debt does not cross manifests** — `SetSource` and `Arm`/`Disarm` both call `rec.reset()`.
5. **`grep -rn "recidivism" internal/ | grep -v _test`** should show the accumulator used only by the two killers, nowhere else.
