package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

type fakeExecutor struct {
	pauseCalls, resumeCalls, killCalls int
}

func (f *fakeExecutor) SPID() int                                   { return 57 }
func (f *fakeExecutor) ExecDDL(context.Context, string) error       { return nil }
func (f *fakeExecutor) Kill(context.Context, int) error             { f.killCalls++; return nil }
func (f *fakeExecutor) Pause(context.Context, ddl.Operation) error  { f.pauseCalls++; return nil }
func (f *fakeExecutor) Resume(context.Context, ddl.Operation) error { f.resumeCalls++; return nil }

var (
	testStart = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	testOp    = ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX"}
)

func TestSuperviseSuccess(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	exec := &fakeExecutor{}
	result := make(chan error, 1)
	go func() {
		result <- supervise(context.Background(), NewManualClock(testStart), exec, testOp,
			Capabilities{}, 60*time.Second, samples, done)
	}()

	samples <- Sample{} // no pressure
	done <- nil
	if err := <-result; err != nil {
		t.Errorf("supervise() = %v, want nil", err)
	}
}

func TestSuperviseCancelOnSustainedBlocking(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := NewManualClock(testStart)
	exec := &fakeExecutor{}
	result := make(chan error, 1)
	go func() {
		result <- supervise(context.Background(), clk, exec, testOp,
			Capabilities{}, 60*time.Second, samples, done)
	}()

	samples <- Sample{BlockingOthers: true} // starts the blocking timer
	clk.Advance(90 * time.Second)
	samples <- Sample{BlockingOthers: true} // 90s >= 60s, not resumable -> cancel

	if err := <-result; !errors.Is(err, ErrCancelled) {
		t.Errorf("supervise() = %v, want ErrCancelled", err)
	}
}

func TestSupervisePauseThenResume(t *testing.T) {
	samples := make(chan Sample)
	done := make(chan error)
	clk := NewManualClock(testStart)
	exec := &fakeExecutor{}
	result := make(chan error, 1)
	go func() {
		result <- supervise(context.Background(), clk, exec, testOp,
			Capabilities{Resumable: true}, 60*time.Second, samples, done)
	}()

	samples <- Sample{BlockingOthers: true}
	clk.Advance(90 * time.Second)
	samples <- Sample{BlockingOthers: true} // pressure + resumable -> pause
	samples <- Sample{}                     // relief -> resume
	done <- nil

	if err := <-result; err != nil {
		t.Errorf("supervise() = %v, want nil", err)
	}
	if exec.pauseCalls != 1 {
		t.Errorf("pauseCalls = %d, want 1", exec.pauseCalls)
	}
	if exec.resumeCalls != 1 {
		t.Errorf("resumeCalls = %d, want 1", exec.resumeCalls)
	}
}

type fakeProbe struct {
	ls       mssql.LogSpace
	sessions []mssql.Session
}

func (f fakeProbe) LogSpace(context.Context) (mssql.LogSpace, error) { return f.ls, nil }
func (f fakeProbe) ActiveSessions(context.Context) ([]mssql.Session, error) {
	return f.sessions, nil
}

func TestServerSampler(t *testing.T) {
	t.Run("blocking and over cap", func(t *testing.T) {
		probe := fakeProbe{
			ls:       mssql.LogSpace{TotalBytes: 100, UsedPercent: 90},
			sessions: []mssql.Session{{SPID: 60, BlockingSPID: 57}},
		}
		s := NewServerSampler(probe, 57, 1000, 80)
		got, err := s.Sample(context.Background())
		if err != nil {
			t.Fatalf("Sample() error = %v", err)
		}
		if !got.BlockingOthers || !got.LogOverCap {
			t.Errorf("Sample() = %+v, want both true", got)
		}
	})

	t.Run("healthy", func(t *testing.T) {
		probe := fakeProbe{
			ls:       mssql.LogSpace{TotalBytes: 100, UsedPercent: 10},
			sessions: []mssql.Session{{SPID: 60, BlockingSPID: 0}},
		}
		s := NewServerSampler(probe, 57, 1000, 80)
		got, err := s.Sample(context.Background())
		if err != nil {
			t.Fatalf("Sample() error = %v", err)
		}
		if got.BlockingOthers || got.LogOverCap {
			t.Errorf("Sample() = %+v, want both false", got)
		}
	})
}
