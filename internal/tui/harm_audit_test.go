package tui

// This file audits the console's destructive surface, the third of the three defect
// classes that are found by walking a type rather than by reading a diff.
//
// The class: an action that kills, blocks or destroys, reachable with a weaker gesture
// than a strictly less harmful neighbor. It is invisible in every diff that built it,
// because each key handler is a few obviously-correct lines, and it is invisible to TDD,
// because the test for a key asserts that the key does what it was meant to do. It is
// visible only in a table of every action next to every gate — which nothing in the
// repository held until this file.
//
// The repository has paid for that four times, one instance per release, each looking
// like a one-off: `abort-resumable` given a target and a confirmation (0.23.0), the TUI
// `x` semantics (0.24.0), `k` killing our own running DDL on one unconfirmed keystroke
// while `x` had been gated two releases earlier (0.28.0), and the console killing with
// `kill_blockers.enabled: false` (0.28.0). Four fixes, no test; this is the test.
//
// It is not a list of remembered keystrokes. Three things are derived:
//
//   - Completeness comes from the source: every ActionKind declared in model.go must be
//     ranked here, so a new action cannot ship unranked.
//   - The gate comes from the real key handler, driven through Model.Update, not from a
//     claim in this table. A gate removed in model.go is a failure here.
//   - The ordering is the one thing stated rather than derived, because the code states
//     it nowhere. That is the point: writing it down is most of the value.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Harm ranks what an action costs and who pays, ordered by what the operator cannot
// undo. The axis is deliberately not "does it issue a KILL": arming a rule issues no
// kill at the moment of the keystroke and is the most dangerous thing in the console.
const (
	harmNone       = iota // strictly safety-increasing
	harmRunPacing         // changes how the run proceeds; nothing is lost
	harmDelayOther        // another party waits longer; no work is lost
	harmEndOne            // ends one other party's transaction now; their work is lost
	harmOurWork           // destroys our own in-flight work and starts an uninterruptible
	// rollback that holds the same locks
	harmEndManyOngoing // ends other parties' transactions, matched by attribute, for the
	// remainder of the run and without a further gesture
)

// A gate is what the operator must do beyond reaching the action.
const (
	gateNone    = 0 // the triggering keystroke emits the action
	gateConfirm = 1 // a modal prompt must be answered before the action is emitted
)

// destructiveAction is one row of the ledger: an action, what it costs, and how to
// reach it. gate is not recorded — it is measured from the key handler.
type destructiveAction struct {
	kind ActionKind
	name string // the ActionKind identifier, matched against model.go
	harm int
	why  string

	// reach prepares a model to the point where key triggers the action.
	reach func(m Model) Model
	key   tea.KeyMsg
	// confirm answers the modal prompt, for actions that have one. A row whose action
	// is emitted neither by key nor by confirm is unreachable and fails the audit.
	confirm tea.KeyMsg
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// oneBlocker is the state most rows need: a session blocking us, selected.
func oneBlocker(m Model) Model {
	m.suspension = SuspensionMsg{Blockers: []SuspensionBlocker{
		{SPID: 104, Login: "SVCACCT", Host: "APPHOST", Count: 2, TotalMS: 20000},
	}}
	m.blockers = []Blocker{{SPID: 104, Login: "SVCACCT", Host: "APPHOST", Program: "APPNAME"}}
	return m
}

// ledger ranks every operator intent the console can emit.
func ledger() []destructiveAction {
	return []destructiveAction{
		{
			kind: ActionDisarmKillRule, name: "ActionDisarmKillRule", harm: harmNone,
			why: "removes a kill rule; it can only reduce what the run will terminate",
			reach: func(m Model) Model {
				m = oneBlocker(m)
				m.rosterOpen = true
				m.armed["login_name=SVCACCT"] = true
				return m
			},
			key: tea.KeyMsg{Type: tea.KeySpace},
		},
		{
			kind: ActionDrain, name: "ActionDrain", harm: harmRunPacing,
			why:   "finishes the current operation, then stops; this is the safe stop",
			reach: func(m Model) Model { return m },
			key:   runes("d"),
		},
		{
			kind: ActionQuit, name: "ActionQuit", harm: harmRunPacing,
			why:   "leaves the console; the run continues without a watcher",
			reach: func(m Model) Model { return m },
			key:   runes("q"),
		},
		{
			kind: ActionIgnoreBlocker, name: "ActionIgnoreBlocker", harm: harmDelayOther,
			why:   "our DDL stops yielding to that session, so it waits on us for as long as we hold the lock",
			reach: func(m Model) Model { m = oneBlocker(m); m.mode = modeCriterion; return m },
			key:   runes("s"),
		},
		{
			kind: ActionKillBlocker, name: "ActionKillBlocker", harm: harmEndOne,
			why:   "kills one session waiting on us: it frees nothing and rolls back whatever that session had open",
			reach: oneBlocker,
			key:   runes("x"), confirm: runes("y"),
		},
		{
			kind: ActionKillDDL, name: "ActionKillDDL", harm: harmOurWork,
			why:   "kills our own running DDL: a non-resumable rebuild loses every hour it has done and starts a rollback that holds the same locks and cannot be interrupted",
			reach: func(m Model) Model { return m },
			key:   runes("k"), confirm: runes("y"),
		},
		{
			kind: ActionArmKillRule, name: "ActionArmKillRule", harm: harmEndManyOngoing,
			why:   "arms a rule matched by app_name / login_name / host_name: every session that later blocks the DDL and matches is terminated, for the rest of the run, with no further gesture",
			reach: func(m Model) Model { m = oneBlocker(m); m.rosterOpen = true; return m },
			key:   tea.KeyMsg{Type: tea.KeySpace}, confirm: runes("y"),
		},
	}
}

// observedGate drives the real key handler and reports what the operator must actually
// do. It is the half of the audit that cannot be satisfied by editing this table.
func observedGate(t *testing.T, d destructiveAction) int {
	t.Helper()
	actions := make(chan Action, 8)
	m := d.reach(New("op", actions))

	after, _ := m.Update(d.key)
	if _, emitted := drain(actions, d.kind); emitted {
		return gateNone
	}
	if !hasConfirm(d) {
		t.Fatalf("%s: its key %v emits nothing and the ledger names no confirmation. "+
			"Either the keystroke changed or the action is unreachable.", d.name, d.key)
	}
	after.(Model).Update(d.confirm)
	if _, emitted := drain(actions, d.kind); emitted {
		return gateConfirm
	}
	t.Fatalf("%s: emitted neither by %v nor after the confirmation %v. The ledger is "+
		"stale — find how the action is reached now and update this row.", d.name, d.key, d.confirm)
	return -1
}

// hasConfirm reports whether the row names a confirmation keystroke.
func hasConfirm(d destructiveAction) bool {
	return d.confirm.Type != 0 || len(d.confirm.Runes) > 0
}

// drain reports whether the given kind was emitted, discarding other kinds (pressing a
// key can legitimately emit a different action first).
func drain(actions chan Action, kind ActionKind) (Action, bool) {
	for {
		select {
		case a := <-actions:
			if a.Kind == kind {
				return a, true
			}
		default:
			return Action{}, false
		}
	}
}

// TestEveryActionIsRanked fails when model.go declares an ActionKind the ledger does not
// rank. Without it the audit would silently stop covering the console as it grows, which
// is exactly how the four historical instances shipped: each new destructive action was
// added on its own, next to correct-looking neighbors.
func TestEveryActionIsRanked(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "model.go", nil, 0)
	if err != nil {
		t.Fatalf("parse model.go: %v", err)
	}

	declared := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		typed := false
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// The block's first spec carries "ActionKind = iota"; the rest inherit it.
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "ActionKind" {
				typed = true
			}
			if !typed {
				continue
			}
			for _, name := range vs.Names {
				declared[name.Name] = true
			}
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatal("no ActionKind constants found in model.go: the audit would pass vacuously")
	}

	ranked := map[string]bool{}
	for _, d := range ledger() {
		ranked[d.name] = true
	}
	for name := range declared {
		if !ranked[name] {
			t.Errorf("%s is declared in model.go but not ranked in this audit's ledger. "+
				"Add it with the harm it can do and how it is reached: an action nobody "+
				"ranked is an action nobody compared against its neighbors.", name)
		}
	}
	for name := range ranked {
		if !declared[name] {
			t.Errorf("%s is ranked here but no longer declared in model.go. Delete the row.", name)
		}
	}
}

// TestNoDestructiveActionIsCheaperThanALesserOne is the audit proper: a more harmful
// action must not be reachable with a weaker gesture than a less harmful one.
//
// Stated as a pairwise comparison rather than as "these keys must confirm" on purpose.
// The defect is never that one key lacks a prompt in the abstract — it is that it lacks
// one *that its neighbor has*, which is why it survives review: the handler is correct
// on its own terms, and only the comparison is wrong.
func TestNoDestructiveActionIsCheaperThanALesserOne(t *testing.T) {
	rows := ledger()
	gates := make(map[string]int, len(rows))
	for _, d := range rows {
		gates[d.name] = observedGate(t, d)
	}

	for _, worse := range rows {
		for _, milder := range rows {
			if worse.harm <= milder.harm {
				continue
			}
			if gates[worse.name] >= gates[milder.name] {
				continue
			}
			t.Errorf("%s is more harmful than %s but reachable with a weaker gesture "+
				"(gate %d vs %d).\n  %s: %s\n  %s: %s\n"+
				"Gate the first at least as strongly as the second, or correct the ranking "+
				"if the ordering is what is wrong.",
				worse.name, milder.name, gates[worse.name], gates[milder.name],
				worse.name, worse.why, milder.name, milder.why)
		}
	}

	for _, d := range rows {
		t.Logf("harm %d  gate %d  %s", d.harm, gates[d.name], d.name)
	}
}
