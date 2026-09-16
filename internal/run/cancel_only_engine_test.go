package run_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/run"
)

// heapManifest is a 2-operation manifest of rebuild_heap operations: rebuild_heap has
// no RESUMABLE form on any target (ddl_compatibility.yaml), so both operations are
// rollback-on-cancel regardless of the Enterprise/major-16 target setupEngine uses —
// which keeps these tests independent of the tier/version rebuild_index would need.
const heapManifest = `
description: cancel-only test
on_failure: continue
operations:
  - operation: rebuild_heap
    schema: dbo
    table: T1
  - operation: rebuild_heap
    schema: dbo
    table: T2
`

// TestManifestStartNoticeCountsRollbackOnCancelOps pins CANCEL-ONLY.md §2: the engine
// writes one line naming how many of the manifest's planned operations are
// rollback-on-cancel, out of how many total, with the configured max_retry_attempts.
func TestManifestStartNoticeCountsRollbackOnCancelOps(t *testing.T) {
	runner := &seqOpRunner{} // every operation succeeds outright, no cancels
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithMaxRetries(3))
	writeOnly(t, dirs, "100_h.yaml", heapManifest)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}

	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "100_h.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(logBytes)
	want := "2 of 2 operation(s) can only be canceled under pressure; a cancel rolls back all their work and is retried up to max_retry_attempts (3)"
	if !strings.Contains(log, want) {
		t.Errorf("log missing manifest-start notice %q\n%s", want, log)
	}
}

// TestManifestStartNoticeAbsentWhenNoOperationQualifies covers the common case: a
// manifest with no rollback-on-cancel operation gets no notice at all.
func TestManifestStartNoticeAbsentWhenNoOperationQualifies(t *testing.T) {
	// engineManifest (setupEngine's default) is a single rebuild_index on the
	// Enterprise/major-16 target, which resolves Resumable=true (auto, supported) —
	// not rollback-on-cancel.
	runner := &seqOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "010_a.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if log := string(logBytes); strings.Contains(log, "can only be canceled") {
		t.Errorf("log has a manifest-start notice where no operation qualifies:\n%s", log)
	}
}

// TestManifestStartNoticeCountsFromResumeCursor pins H7 (docs/specs/REVIEW-2026-09-15-harm.md):
// on a resumed manifest the notice must count only the operations from the resume
// cursor onward, for both N and M — an operation already completed in a previous run
// is not exposure this run will incur.
func TestManifestStartNoticeCountsFromResumeCursor(t *testing.T) {
	runner := &seqOpRunner{} // every operation succeeds outright, no cancels
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithMaxRetries(1))
	writeOnly(t, dirs, "100_h.yaml", heapManifest)
	// A previous drained run completed operation 0 (heapManifest has two rebuild_heap
	// operations, both rollback-on-cancel); this run resumes from operation 1.
	writeSidecarState(t, dirs, "100_h.yaml", run.State{Manifest: "100_h.yaml", ResumeFromOp: 1})

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "100_h.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(logBytes)
	want := "1 of 1 operation(s) can only be canceled under pressure; a cancel rolls back all their work and is retried up to max_retry_attempts (1)"
	if !strings.Contains(log, want) {
		t.Errorf("log missing resumed manifest-start notice %q (want it counted from the resume cursor, not the full plan)\n%s", want, log)
	}
	if strings.Contains(log, "2 of 2") {
		t.Errorf("log counts the already-completed operation into the notice:\n%s", log)
	}
}

// TestNoticeSinkReceivesManifestStartNotice pins H2 (docs/specs/REVIEW-2026-09-15-harm.md):
// the manifest-start rollback-on-cancel notice must reach a wired notice sink — the
// path a --tui run uses to put it on screen, since e.out is io.Discard there and the
// notice would otherwise surface only in the .log, after the run.
func TestNoticeSinkReceivesManifestStartNotice(t *testing.T) {
	runner := &seqOpRunner{}
	var mu sync.Mutex
	var notices []string
	eng, dirs := setupEngine(t, fakePreflighter{}, runner, run.WithMaxRetries(3),
		run.WithNoticeSink(func(s string) {
			mu.Lock()
			notices = append(notices, s)
			mu.Unlock()
		}))
	writeOnly(t, dirs, "100_h.yaml", heapManifest)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The sink carries every manifest-start notice in the order they happen, so the
	// preflight phase line comes first and this one replaces it on screen.
	want := "2 of 2 operation(s) can only be canceled under pressure; a cancel rolls back all their work and is retried up to max_retry_attempts (3)"
	if len(notices) == 0 || notices[len(notices)-1] != want {
		t.Errorf("notice sink got %v, want it to end with %q", notices, want)
	}
}

// TestNoticeSinkNotCalledWhenNoOperationQualifies covers the negative case: a manifest
// with no rollback-on-cancel operation never sends that notice, matching the .log
// notice's own absence (TestManifestStartNoticeAbsentWhenNoOperationQualifies). Other
// manifest-start notices still travel on the same sink, so this asserts on content.
func TestNoticeSinkNotCalledWhenNoOperationQualifies(t *testing.T) {
	runner := &seqOpRunner{}
	var mu sync.Mutex
	var notices []string
	eng, _ := setupEngine(t, fakePreflighter{}, runner,
		run.WithNoticeSink(func(s string) {
			mu.Lock()
			notices = append(notices, s)
			mu.Unlock()
		}))

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, n := range notices {
		if strings.Contains(n, "can only be canceled") {
			t.Errorf("notice sink got %q where no operation qualifies", n)
		}
	}
}

// TestCancelOnlySummarySplitsSucceededAndFailed pins CANCEL-ONLY.md §3: the summary
// line counts rollback-on-cancel operations that recorded at least one cancel
// reaction, split by whether a retry saved them, and points at the recovery manifest
// under on_failure: continue.
func TestCancelOnlySummarySplitsSucceededAndFailed(t *testing.T) {
	runner := &seqOpRunner{
		cancelsBefore: []int{1, 2}, // op 0: one cancel, then succeeds; op 1: two cancels, then fails
		errs:          []error{nil, os.ErrDeadlineExceeded},
	}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	writeOnly(t, dirs, "100_h.yaml", heapManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Fatalf("Summary = %+v, want Failed:1 (PARTIAL)", sum)
	}

	logBytes, err := os.ReadFile(filepath.Join(dirs.Failed, "100_h.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(logBytes)
	want := "2 rollback-on-cancel operation(s) were canceled under pressure: 1 succeeded after a retry, 1 failed"
	if !strings.Contains(log, want) {
		t.Errorf("log missing cancel-only summary %q\n%s", want, log)
	}
	if !strings.Contains(log, "100_h.yaml.recovery.yaml") {
		t.Errorf("log missing the recovery-manifest pointer alongside the summary:\n%s", log)
	}
	mustExist(t, filepath.Join(dirs.Failed, "100_h.yaml.recovery.yaml"))
}

// mixedCancelOnlyManifest pairs one rollback-on-cancel operation (rebuild_heap) with
// one that is not (reorganize_index — cancelSafe), for TestCancelOnlySummaryOmitsRecoveryPointerWhenNothingRollbackOnCancelFailed.
const mixedCancelOnlyManifest = `
description: cancel-only h4 test
on_failure: continue
operations:
  - operation: rebuild_heap
    schema: dbo
    table: T1
  - operation: reorganize_index
    schema: dbo
    table: T2
    index: IX2
`

// TestCancelOnlySummaryOmitsRecoveryPointerWhenNothingRollbackOnCancelFailed pins H4
// (docs/specs/REVIEW-2026-09-15-harm.md): the recovery-manifest pointer must be
// appended to CancelOnlySummary only when a rollback-on-cancel operation actually
// failed. Here the only rollback-on-cancel operation (op 0) was canceled once and then
// SAVED by its retry; the operation that ends up quarantined (op 1) is not
// rollback-on-cancel and never recorded a cancel at all. The recovery manifest holds
// none of the operations the cancel-only summary is about, so pointing at it there
// would mislead (rep.Error's own "recovery manifest:" pointer, about the quarantined
// op, is unaffected).
func TestCancelOnlySummaryOmitsRecoveryPointerWhenNothingRollbackOnCancelFailed(t *testing.T) {
	runner := &seqOpRunner{
		cancelsBefore: []int{1, 0}, // op 0: one cancel, then succeeds; op 1: no cancel
		errs:          []error{nil, os.ErrDeadlineExceeded},
	}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	writeOnly(t, dirs, "100_h.yaml", mixedCancelOnlyManifest)

	sum, err := eng.ProcessAll(context.Background())
	if err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	if sum.Failed != 1 {
		t.Fatalf("Summary = %+v, want Failed:1 (PARTIAL)", sum)
	}

	logBytes, err := os.ReadFile(filepath.Join(dirs.Failed, "100_h.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(logBytes)
	summary := "1 rollback-on-cancel operation(s) were canceled under pressure: 1 succeeded after a retry, 0 failed"
	if !strings.Contains(log, summary) {
		t.Fatalf("log missing cancel-only summary %q\n%s", summary, log)
	}
	if strings.Contains(log, summary+"; recovery manifest:") {
		t.Errorf("cancel-only summary points at the recovery manifest though no rollback-on-cancel operation failed:\n%s", log)
	}
}

// TestCancelOnlySummaryAbsentWithNoCancel covers the negative case: rollback-on-cancel
// operations that ran cleanly (no cancel reaction) produce no summary line.
func TestCancelOnlySummaryAbsentWithNoCancel(t *testing.T) {
	runner := &seqOpRunner{}
	eng, dirs := setupEngine(t, fakePreflighter{}, runner)
	writeOnly(t, dirs, "100_h.yaml", heapManifest)

	if _, err := eng.ProcessAll(context.Background()); err != nil {
		t.Fatalf("ProcessAll() error = %v", err)
	}
	logBytes, err := os.ReadFile(filepath.Join(dirs.Done, "100_h.yaml.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if log := string(logBytes); strings.Contains(log, "were canceled under pressure") {
		t.Errorf("log has a cancel-only summary where nothing was canceled:\n%s", log)
	}
}
