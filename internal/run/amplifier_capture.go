package run

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// amplifierCaptureSuffix names the advisory capture file written next to a manifest.
const amplifierCaptureSuffix = ".amplifiers.yaml"

// capturedAmplifier is one maintenance victim this run killed.
type capturedAmplifier struct {
	event    AmplifierKillEvent
	killedAt string
}

// amplifierCapture accumulates the amplifying victims killed during one manifest, in
// kill order, and the distinct jobs they came from.
//
// Unlike blockerCapture, which is touched only from the engine goroutine, a kill
// happens on the pump goroutine inside Sampler.Blocking — so this accumulator carries
// its own mutex. It is small and append-mostly, which is exactly the case a mutex
// suits; a channel funnel would be more machinery for no gain.
type amplifierCapture struct {
	mu       sync.Mutex
	killed   []capturedAmplifier
	jobOrder []string
	jobSeen  map[string]bool
}

// jobKey identifies a distinct source: (job id, step) when attribution resolved,
// otherwise the raw program name — so an unattributed CmdExec step still collapses to
// one entry rather than one per kill.
func jobKey(ev AmplifierKillEvent) string {
	if ev.Job.Resolved {
		return fmt.Sprintf("%s:%d", ev.Job.JobID, ev.Job.StepID)
	}
	return "program:" + ev.Program
}

// jobLabel is the human-readable form of a distinct source, for the TUI line.
func jobLabel(ev AmplifierKillEvent) string {
	if ev.Job.Resolved {
		return fmt.Sprintf("%s (step %d)", ev.Job.JobName, ev.Job.StepID)
	}
	return ev.Program
}

func (c *amplifierCapture) add(ev AmplifierKillEvent, now string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jobSeen == nil {
		c.jobSeen = make(map[string]bool)
	}
	c.killed = append(c.killed, capturedAmplifier{event: ev, killedAt: now})
	if key := jobKey(ev); !c.jobSeen[key] {
		c.jobSeen[key] = true
		c.jobOrder = append(c.jobOrder, jobLabel(ev))
	}
}

func (c *amplifierCapture) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.killed)
}

// jobs returns the distinct sources seen, in first-seen order, for the TUI's sticky
// conflicting-jobs line.
func (c *amplifierCapture) jobs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.jobOrder))
	copy(out, c.jobOrder)
	return out
}

// renderAmplifiers builds the advisory capture file. SqlGoPace never reads it back —
// disabling a job is a deliberate operator step.
func renderAmplifiers(name string, acc *amplifierCapture) []byte {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "# Amplifying maintenance victims killed during %s\n", name)
	b.WriteString("# Advisory only — SqlGoPace never reads this file back.\n")
	b.WriteString("# Each entry is a session this run terminated because it requested a Sch-M lock\n")
	b.WriteString("# behind our operation while other sessions queued behind it.\n\n")

	b.WriteString("killed:\n")
	for _, ca := range acc.killed {
		ev := ca.event
		fmt.Fprintf(&b, "  - session_id: %d\n", ev.SPID)
		writeYAMLString(&b, "    command", ev.Command)
		writeYAMLString(&b, "    statement", ev.Statement)
		writeYAMLString(&b, "    database", ev.Database)
		writeYAMLString(&b, "    login_name", ev.Login)
		writeYAMLString(&b, "    host_name", ev.Host)
		writeYAMLString(&b, "    app_name", ev.Program)
		fmt.Fprintf(&b, "    blocked_behind: %d\n", ev.BlockedBehind)
		fmt.Fprintf(&b, "    waited_ms: %d\n", ev.WaitedMS)
		if !ev.FirstEligible.IsZero() {
			writeYAMLString(&b, "    first_eligible", ev.FirstEligible.UTC().Format(time.RFC3339))
		}
		writeYAMLString(&b, "    killed_at", ca.killedAt)
		b.WriteString("    agent_job:\n")
		fmt.Fprintf(&b, "      resolved: %t\n", ev.Job.Resolved)
		if !ev.Job.Resolved {
			continue
		}
		writeYAMLString(&b, "      job_id", ev.Job.JobID)
		writeYAMLString(&b, "      job_name", ev.Job.JobName)
		fmt.Fprintf(&b, "      step_id: %d\n", ev.Job.StepID)
		writeYAMLString(&b, "      step_name", ev.Job.StepName)
	}

	var stmts []string
	seen := make(map[string]bool)
	for _, ca := range acc.killed {
		s := ca.event.Job.DisableStatement()
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		stmts = append(stmts, s)
	}
	if len(stmts) > 0 {
		b.WriteString("\n# Distinct SQL Agent jobs terminated by this run. Review before disabling:\n")
		for _, s := range stmts {
			fmt.Fprintf(&b, "#   %s\n", s)
		}
	}
	return []byte(b.String())
}
