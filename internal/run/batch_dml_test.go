package run

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// fakeBatchServer models a draining table for the batch-DML driver: each ExecRows
// affects up to the statement's TOP (n) rows until none remain. It implements both
// BatchExecutor and BatchDMLReader.
type fakeBatchServer struct {
	remaining int64
	estRows   int64
	waits     []mssql.SessionWait

	execLog []string
	killed  bool

	onExec func(sql string) // test hook, called at the start of each ExecRows
}

func (s *fakeBatchServer) SPID() int                             { return 7 }
func (s *fakeBatchServer) ExecDDL(context.Context, string) error { return nil }
func (s *fakeBatchServer) Kill(context.Context, int) error       { s.killed = true; return nil }

func (s *fakeBatchServer) ExecRows(_ context.Context, sql string) (int64, error) {
	if s.onExec != nil {
		s.onExec(sql)
	}
	s.execLog = append(s.execLog, sql)
	n := min(int64(parseTop(sql)), s.remaining)
	s.remaining -= n
	return n, nil
}

func (s *fakeBatchServer) TableRowEstimate(context.Context, string, string) (int64, error) {
	return s.estRows, nil
}

func (s *fakeBatchServer) SessionWaits(context.Context, int) ([]mssql.SessionWait, error) {
	return s.waits, nil
}

func (s *fakeBatchServer) QueryInt(context.Context, string) (int64, bool, error) {
	return 0, false, nil
}

func (s *fakeBatchServer) ClusteringKeyColumns(context.Context, string, string) ([]mssql.KeyColumn, error) {
	return nil, nil
}

// memWatermark is an in-memory WatermarkStore for the key_range tests.
type memWatermark struct {
	loadVal int64
	loadOK  bool
	saved   int64
	saves   int
}

func (m *memWatermark) Load(context.Context) (int64, bool, error) { return m.loadVal, m.loadOK, nil }
func (m *memWatermark) Save(_ context.Context, wm int64) error {
	m.saved = wm
	m.saves++
	return nil
}

// fakeKeyServer models a table keyed 1..maxKey for the key_range walk: QueryInt
// returns the next batch's upper bound (walkPos + TOP, capped) and ExecRows applies
// the (walkPos, upper] range. It owns walkPos so the walk stays self-consistent.
type fakeKeyServer struct {
	maxKey  int64
	walkPos int64
	keyCols []mssql.KeyColumn
	waits   []mssql.SessionWait

	execLog []string
	killed  bool
}

func (s *fakeKeyServer) SPID() int                             { return 8 }
func (s *fakeKeyServer) ExecDDL(context.Context, string) error { return nil }
func (s *fakeKeyServer) Kill(context.Context, int) error       { s.killed = true; return nil }
func (s *fakeKeyServer) TableRowEstimate(context.Context, string, string) (int64, error) {
	return s.maxKey, nil
}
func (s *fakeKeyServer) SessionWaits(context.Context, int) ([]mssql.SessionWait, error) {
	return s.waits, nil
}
func (s *fakeKeyServer) ClusteringKeyColumns(context.Context, string, string) ([]mssql.KeyColumn, error) {
	return s.keyCols, nil
}
func (s *fakeKeyServer) QueryInt(_ context.Context, sql string) (int64, bool, error) {
	if s.walkPos >= s.maxKey {
		return 0, false, nil
	}
	return min(s.walkPos+int64(parseTop(sql)), s.maxKey), true, nil
}
func (s *fakeKeyServer) ExecRows(_ context.Context, sql string) (int64, error) {
	s.execLog = append(s.execLog, sql)
	next := parseUpperBound(sql)
	rows := next - s.walkPos
	s.walkPos = next
	return rows, nil
}

// parseTop extracts N from a statement containing "TOP (N)".
func parseTop(sql string) int {
	_, after, ok := strings.Cut(sql, "TOP (")
	if !ok {
		return 0
	}
	num, _, ok := strings.Cut(after, ")")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(num))
	return n
}

// parseUpperBound extracts N from a key_range update's "<= N" upper bound.
func parseUpperBound(sql string) int64 {
	_, after, ok := strings.Cut(sql, "<= ")
	if !ok {
		return 0
	}
	end := 0
	for end < len(after) && after[end] >= '0' && after[end] <= '9' {
		end++
	}
	n, _ := strconv.ParseInt(after[:end], 10, 64)
	return n
}

func testBatchTuning() BatchTuning {
	return BatchTuning{
		InitialSmallRows:  1000,
		InitialMediumRows: 5000,
		InitialLargeRows:  20000,
		MinRows:           100,
		MaxRows:           100000,
		EscalationCapRows: 4000,
		TargetBatch:       5 * time.Second,
		SelfWaitTimeout:   10 * time.Minute,
	}
}

func newBatchRunner(exec BatchExecutor, reader BatchDMLReader, clk *ManualClock, rcsi bool) *BatchDMLRunner {
	return NewBatchDMLRunner(exec, reader, noPressureSampler{}, clk, BatchDMLRunnerConfig{
		Tuning:          testBatchTuning(),
		RCSI:            rcsi,
		PollInterval:    time.Hour, // pump never fires during the instant batches
		LogPollInterval: time.Hour,
		BlockingTimeout: time.Minute,
		LogDrainTimeout: time.Minute,
		KillGrace:       time.Second,
	})
}

func newTestBatchRunner(s *fakeBatchServer, clk *ManualClock, rcsi bool) *BatchDMLRunner {
	return newBatchRunner(s, s, clk, rcsi)
}

// keyRangeOp is a literal whole-table UPDATE using the key_range strategy.
func keyRangeOp() ddl.BatchDML {
	return ddl.BatchDML{
		Verb: "update", Schema: "dbo", Table: "T",
		Set:              map[string]ddl.Literal{"Flag": {Raw: "1"}},
		ConfirmFullTable: true,
		Batch:            ddl.BatchSpec{Strategy: "key_range"},
	}
}

func TestBatchDMLConverges(t *testing.T) {
	s := &fakeBatchServer{remaining: 12345, estRows: 12345}
	r := newTestBatchRunner(s, NewManualClock(time.Unix(0, 0)), true)

	op := ddl.BatchDML{Verb: "delete", Schema: "dbo", Table: "T", WhereRaw: "A = 1"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, noWatermark{}, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Rows != 12345 {
		t.Errorf("Rows = %d, want 12345", res.Rows)
	}
	if res.Batches == 0 {
		t.Errorf("Batches = 0, want > 0")
	}
	if res.Reason != "" {
		t.Errorf("Reason = %q, want empty (clean completion)", res.Reason)
	}
	if s.remaining != 0 {
		t.Errorf("server remaining = %d, want 0", s.remaining)
	}
}

func TestBatchPredicateStopsBetweenBatches(t *testing.T) {
	drain := &DrainFlag{}
	s := &fakeBatchServer{remaining: 100000, estRows: 100000}
	// Request the graceful stop when the first batch runs, so the loop commits that batch
	// and stops before the next one.
	s.onExec = func(string) { drain.Request() }
	r := newTestBatchRunner(s, NewManualClock(time.Unix(0, 0)), true)
	r.stop = drain.Draining

	op := ddl.BatchDML{Verb: "delete", Schema: "dbo", Table: "T", WhereRaw: "A = 1"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, noWatermark{}, discard)
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("Run() error = %v, want ErrStopped", err)
	}
	if res.Batches != 1 {
		t.Errorf("Batches = %d, want 1 (the current batch committed, the next did not start)", res.Batches)
	}
	if !strings.Contains(res.Reason, "graceful stop") {
		t.Errorf("Reason = %q, want it to mention the graceful stop", res.Reason)
	}
	if s.remaining == 0 {
		t.Errorf("server drained fully; want a partial stop with rows remaining")
	}
}

func TestBatchDMLNoRows(t *testing.T) {
	s := &fakeBatchServer{remaining: 0, estRows: 0}
	r := newTestBatchRunner(s, NewManualClock(time.Unix(0, 0)), true)

	op := ddl.BatchDML{Verb: "update", Schema: "dbo", Table: "T", SetRaw: "A = 1", WhereRaw: "A <> 1"}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, noWatermark{}, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Rows != 0 || res.Batches != 0 {
		t.Errorf("got rows=%d batches=%d, want 0/0 (nothing matched)", res.Rows, res.Batches)
	}
	// One probing statement ran and affected nothing.
	if len(s.execLog) != 1 {
		t.Errorf("statements run = %d, want 1", len(s.execLog))
	}
}

func TestBatchDMLProgressReportsBatchSize(t *testing.T) {
	s := &fakeBatchServer{remaining: 3000, estRows: 3000}
	var last BatchDMLProgress
	var count int
	r := NewBatchDMLRunner(s, s, noPressureSampler{}, NewManualClock(time.Unix(0, 0)), BatchDMLRunnerConfig{
		Tuning: testBatchTuning(), RCSI: true,
		PollInterval: time.Hour, LogPollInterval: time.Hour,
		BlockingTimeout: time.Minute, LogDrainTimeout: time.Minute, KillGrace: time.Second,
	}, WithBatchDMLProgress(func(p BatchDMLProgress) { last = p; count++ }))

	op := ddl.BatchDML{Verb: "delete", Schema: "dbo", Table: "T", WhereRaw: "A = 1"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, noWatermark{}, discard); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if count == 0 {
		t.Fatal("progress callback never fired")
	}
	// BatchRows was left unset before It3 (always 0); it must now carry the batch size.
	if last.BatchRows <= 0 {
		t.Errorf("BatchRows = %d, want > 0", last.BatchRows)
	}
	if last.RowsDone != 3000 || last.EstRows != 3000 {
		t.Errorf("RowsDone/EstRows = %d/%d, want 3000/3000", last.RowsDone, last.EstRows)
	}
}

func TestPerSec(t *testing.T) {
	if got := perSec(4000, 2*time.Second); got != 2000 {
		t.Errorf("perSec(4000, 2s) = %v, want 2000", got)
	}
	if got := perSec(100, 0); got != 0 {
		t.Errorf("perSec(100, 0) = %v, want 0 (guards divide-by-zero)", got)
	}
}

func TestBatchDMLRCSIOffCapsBatchSize(t *testing.T) {
	// A large table: the large-tier initial (20000) and adaptive growth must both be
	// held under the escalation cap (4000) when RCSI is off.
	s := &fakeBatchServer{remaining: 50000, estRows: 2_000_000}
	r := newTestBatchRunner(s, NewManualClock(time.Unix(0, 0)), false)

	op := ddl.BatchDML{Verb: "update", Schema: "dbo", Table: "T", SetRaw: "A = 1", WhereRaw: "A <> 1"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, noWatermark{}, discard); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, sql := range s.execLog {
		if top := parseTop(sql); top > testBatchTuning().EscalationCapRows {
			t.Errorf("batch size %d exceeds the RCSI-off escalation cap %d (%s)", top, testBatchTuning().EscalationCapRows, sql)
		}
	}
}

func TestBatchDMLRCSIOnGrowsPastCap(t *testing.T) {
	// With RCSI on, the cap is lifted: under no pressure the batch grows past the
	// escalation cap toward MaxRows.
	s := &fakeBatchServer{remaining: 200000, estRows: 2_000_000}
	r := newTestBatchRunner(s, NewManualClock(time.Unix(0, 0)), true)

	op := ddl.BatchDML{Verb: "delete", Schema: "dbo", Table: "T", WhereRaw: "A = 1"}
	if _, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, noWatermark{}, discard); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var maxTop int
	for _, sql := range s.execLog {
		maxTop = max(maxTop, parseTop(sql))
	}
	if maxTop <= testBatchTuning().EscalationCapRows {
		t.Errorf("max batch size = %d, want > escalation cap %d (RCSI on lifts it)", maxTop, testBatchTuning().EscalationCapRows)
	}
}

func TestBatchDMLKeyRangeWalksAndPersists(t *testing.T) {
	s := &fakeKeyServer{maxKey: 25000, keyCols: []mssql.KeyColumn{{Name: "Id", IsInteger: true}}}
	store := &memWatermark{}
	r := newBatchRunner(s, s, NewManualClock(time.Unix(0, 0)), true)

	res, err := r.Run(context.Background(), keyRangeOp(), ddl.ResolvedOptions{}, nil, store, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Rows != 25000 {
		t.Errorf("Rows = %d, want 25000", res.Rows)
	}
	if res.Batches == 0 {
		t.Errorf("Batches = 0, want > 0")
	}
	if res.Reason != "" {
		t.Errorf("Reason = %q, want empty (clean completion)", res.Reason)
	}
	if store.saved != 25000 || store.saves == 0 {
		t.Errorf("persisted watermark = %d after %d saves, want 25000 with saves > 0", store.saved, store.saves)
	}
}

func TestBatchDMLKeyRangeResumes(t *testing.T) {
	// A crash left the walk at key 20000 of 25000; the run resumes from the watermark.
	s := &fakeKeyServer{maxKey: 25000, walkPos: 20000, keyCols: []mssql.KeyColumn{{Name: "Id", IsInteger: true}}}
	store := &memWatermark{loadVal: 20000, loadOK: true}
	r := newBatchRunner(s, s, NewManualClock(time.Unix(0, 0)), true)

	res, err := r.Run(context.Background(), keyRangeOp(), ddl.ResolvedOptions{}, nil, store, discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Rows != 5000 {
		t.Errorf("Rows = %d, want 5000 (resumed at 20000 of 25000)", res.Rows)
	}
}

func TestBatchDMLKeyRangeRejectsNonIntegerKey(t *testing.T) {
	s := &fakeKeyServer{maxKey: 100, keyCols: []mssql.KeyColumn{{Name: "Code", IsInteger: false}}}
	r := newBatchRunner(s, s, NewManualClock(time.Unix(0, 0)), true)

	if _, err := r.Run(context.Background(), keyRangeOp(), ddl.ResolvedOptions{}, nil, &memWatermark{}, discard); err == nil {
		t.Fatalf("Run() error = nil, want an error for a non-integer key")
	}
}

func TestInitialBatchRows(t *testing.T) {
	tn := testBatchTuning()
	tests := []struct {
		est  int64
		want int
	}{
		{50_000, 1000},     // small tier
		{500_000, 5000},    // medium tier
		{5_000_000, 20000}, // large tier
	}
	for _, tt := range tests {
		if got := InitialBatchRows(tt.est, tn); got != tt.want {
			t.Errorf("InitialBatchRows(%d) = %d, want %d", tt.est, got, tt.want)
		}
	}
}

func TestAdjustBatchRows(t *testing.T) {
	tn := testBatchTuning()
	lo, hi := tn.MinRows, tn.MaxRows
	tests := []struct {
		name string
		size int
		w    WaitDeltas
		want int
	}{
		{"reduce on writelog", 4000, WaitDeltas{WriteLogAvgMs: 20}, 2000},
		{"reduce on blocking", 4000, WaitDeltas{BlockingSeconds: 60}, 2000},
		{"grow when quiet", 4000, WaitDeltas{}, 8000},
		{"clamp to ceiling", 80000, WaitDeltas{}, hi},
		{"clamp to floor", 150, WaitDeltas{WriteLogAvgMs: 20}, lo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AdjustBatchRows(tt.size, time.Second, tt.w, tn.TargetBatch, lo, hi); got != tt.want {
				t.Errorf("AdjustBatchRows = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestBatchDMLPredicateRowCeiling pins the absolute backstop on the predicate loop.
// A set_raw that does not consume its own filter — "Counter = Counter + 1" WHERE
// "Status = 'A'" — matches the same rows on every pass, so the loop's only exit
// ("the last batch affected zero rows") is never reached. Every batch autocommits,
// so without a ceiling this churns rows at full write rate forever. The ceiling
// converts that into a bounded, reported failure.
func TestBatchDMLPredicateRowCeiling(t *testing.T) {
	s := &fakeBatchServer{remaining: math.MaxInt64, estRows: 1000} // never exhausts
	r := newTestBatchRunner(s, NewManualClock(time.Unix(0, 0)), true)

	op := ddl.BatchDML{
		Verb: "update", Schema: "dbo", Table: "T",
		SetRaw:   "Counter = Counter + 1",
		WhereRaw: "Status = 'A'",
	}
	res, err := r.Run(context.Background(), op, ddl.ResolvedOptions{}, nil, noWatermark{}, discard)
	if !errors.Is(err, ErrRowCeiling) {
		t.Fatalf("Run() error = %v, want ErrRowCeiling", err)
	}
	// est 1000 -> ceiling max(2*1000, 10000) = 10000.
	if res.Rows < 10000 {
		t.Errorf("Rows = %d, want at least the ceiling (10000)", res.Rows)
	}
	if res.Rows > 20000 {
		t.Errorf("Rows = %d, want the loop stopped near the ceiling, not far past it", res.Rows)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; the report must say why the operation stopped")
	}
	if res.Batches == 0 {
		t.Error("Batches = 0, want the committed batches counted")
	}
}

// TestPredicateRowCeiling pins the bound itself. A terminating predicate affects each
// row at most once, so cumulative rows cannot exceed the table's row count; twice
// that is the slack for a stale estimate and concurrent inserts.
func TestPredicateRowCeiling(t *testing.T) {
	tests := []struct {
		name string
		est  int64
		want int64
	}{
		{"unavailable estimate is generous", 0, 1_000_000},
		{"negative estimate is treated as unavailable", -1, 1_000_000},
		{"tiny table keeps a sane floor", 5, 10_000},
		{"small table keeps a sane floor", 1000, 10_000},
		{"large table scales with the estimate", 50_000_000, 100_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := predicateRowCeiling(tt.est); got != tt.want {
				t.Errorf("predicateRowCeiling(%d) = %d, want %d", tt.est, got, tt.want)
			}
		})
	}
}
