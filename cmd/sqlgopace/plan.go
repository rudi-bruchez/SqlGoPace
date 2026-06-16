package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rudi-bruchez/SqlGoPace/internal/config"
	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
	"github.com/rudi-bruchez/SqlGoPace/internal/report"
	"github.com/rudi-bruchez/SqlGoPace/internal/version"
)

// analysisReader is the narrow set of maintenance reads the planner needs.
// *mssql.Conn satisfies it; tests pass a fake.
type analysisReader interface {
	ObjectInventory(ctx context.Context) ([]mssql.InventoryObject, error)
	PhysicalStats(ctx context.Context, objectID int64, indexID int, partition *int, mode string) ([]mssql.PhysicalStats, error)
	EstimateCompression(ctx context.Context, schema, table string, indexID int, partition *int, setting string) ([]mssql.CompressionSaving, error)
	IndexOperationalStats(ctx context.Context, objectID int64, indexID int, partition *int) ([]mssql.OperationalStats, error)
	StatsProperties(ctx context.Context, objectID int64) ([]mssql.StatProperty, error)
}

// allCategories is the set of selectable analysis categories.
var allCategories = []string{"index", "compression", "heaps", "statistics", "checkdb"}

// categorySet filters which analysis categories run. An empty set means "all".
type categorySet map[string]bool

func (c categorySet) has(name string) bool { return len(c) == 0 || c[name] }

// parseCategories parses a comma-separated category list, rejecting unknown names.
func parseCategories(csv string) (categorySet, error) {
	if strings.TrimSpace(csv) == "" {
		return categorySet{}, nil
	}
	set := categorySet{}
	for _, raw := range strings.Split(csv, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if !slices.Contains(allCategories, name) {
			return nil, fmt.Errorf("unknown category %q (want any of %s)", name, strings.Join(allCategories, ", "))
		}
		set[name] = true
	}
	return set, nil
}

// buildInput runs the cheap inventory sweep, selects candidates per category, and
// gathers the expensive reads only for survivors (spec §4.0/§4.2/§4.6). Per-object
// read failures are logged and skipped, not fatal (spec §4.7).
func buildInput(ctx context.Context, r analysisReader, p *maint.Profile, cats categorySet, db string, logw io.Writer) (maint.Input, error) {
	inv, err := r.ObjectInventory(ctx)
	if err != nil {
		return maint.Input{}, fmt.Errorf("object inventory: %w", err)
	}

	in := maint.Input{ConnDatabase: db}
	wantIndex := cats.has("index")
	wantComp := cats.has("compression") && p.Compression.Enabled

	groups, tables := groupInventory(inv)
	for _, g := range groups {
		head := g[0]
		switch {
		case head.IsHeap():
			if cats.has("heaps") && p.Heap.Enabled {
				if hm, ok := heapMeasurement(ctx, r, p, wantComp, head, logw); ok {
					in.Heaps = append(in.Heaps, hm)
				}
			}
		case head.Type == 1 || head.Type == 2: // rowstore clustered / nonclustered
			if wantIndex || wantComp {
				in.Indexes = append(in.Indexes, indexMeasurements(ctx, r, p, cats, g, logw)...)
			}
		}
	}

	if cats.has("statistics") && p.Statistics.Enabled {
		for _, tbl := range tables {
			props, err := r.StatsProperties(ctx, tbl.objectID)
			if err != nil {
				fmt.Fprintf(logw, "-- skip statistics on %s.%s: %v\n", tbl.schema, tbl.table, err)
				continue
			}
			for _, sp := range props {
				in.Statistics = append(in.Statistics, maint.StatMeasurement{
					Schema: tbl.schema, Table: tbl.table, Statistic: sp.Name,
					Rows: sp.Rows, ModificationCounter: sp.ModificationCounter,
				})
			}
		}
	}
	return in, nil
}

// indexMeasurements turns one (object, index) partition group into measurements,
// per-partition when the profile asks and the index is partitioned, else whole-index.
func indexMeasurements(ctx context.Context, r analysisReader, p *maint.Profile, cats categorySet, group []mssql.InventoryObject, logw io.Writer) []maint.IndexMeasurement {
	head := group[0]
	clustered := head.IndexID == 1

	// Fragmentation (cheap LIMITED scan), mapped by partition.
	fragByPart := map[int]float64{}
	pageByPart := map[int]int64{}
	if cats.has("index") {
		ps, err := r.PhysicalStats(ctx, head.ObjectID, head.IndexID, nil, mssql.PhysicalLimited)
		if err != nil {
			fmt.Fprintf(logw, "-- skip index %s.%s.%s: physical stats: %v\n", head.Schema, head.Table, head.IndexName, err)
			return nil
		}
		for _, s := range ps {
			fragByPart[s.PartitionNumber] = s.AvgFragmentationPercent
			pageByPart[s.PartitionNumber] = s.PageCount
		}
	}

	wantComp := cats.has("compression") && p.Compression.Enabled
	granular := wantComp && p.Compression.PerPartition && len(group) > 1

	if granular {
		out := make([]maint.IndexMeasurement, 0, len(group))
		for _, row := range group {
			part := row.PartitionNumber
			m := maint.IndexMeasurement{
				Schema: row.Schema, Table: row.Table, Index: row.IndexName, Clustered: clustered,
				Partition: &part, PageCount: pageByPart[part], SizeMB: int64(row.SizeMB),
				FragmentationPercent: fragByPart[part], Current: parseCompression(row.Compression),
			}
			if estimable(row) {
				m.Estimate = estimateFor(ctx, r, row.Schema, row.Table, row.IndexID, &part, logw)
				m.Write = writeFor(ctx, r, row.ObjectID, row.IndexID, &part)
			}
			out = append(out, m)
		}
		return out
	}

	// Whole-index: aggregate size, take the worst fragmentation.
	var sizeMB, pageCount int64
	var maxFrag float64
	for _, row := range group {
		sizeMB += int64(row.SizeMB)
		pageCount += pageByPart[row.PartitionNumber]
		if f := fragByPart[row.PartitionNumber]; f > maxFrag {
			maxFrag = f
		}
	}
	m := maint.IndexMeasurement{
		Schema: head.Schema, Table: head.Table, Index: head.IndexName, Clustered: clustered,
		PageCount: pageCount, SizeMB: sizeMB, FragmentationPercent: maxFrag, Current: parseCompression(head.Compression),
	}
	if wantComp && estimable(head) {
		m.Estimate = estimateFor(ctx, r, head.Schema, head.Table, head.IndexID, nil, logw)
		m.Write = writeFor(ctx, r, head.ObjectID, head.IndexID, nil)
	}
	return []maint.IndexMeasurement{m}
}

// heapMeasurement gathers the SAMPLED scan (forwarded records) for one heap, after
// a cheap size pre-filter. ok is false when the heap is out of size bounds or the
// scan fails.
func heapMeasurement(ctx context.Context, r analysisReader, p *maint.Profile, wantComp bool, head mssql.InventoryObject, logw io.Writer) (maint.HeapMeasurement, bool) {
	sizeMB := int64(head.SizeMB)
	if sizeMB < p.Heap.MinSizeMB || sizeMB > p.Heap.MaxSizeMB {
		return maint.HeapMeasurement{}, false
	}
	ps, err := r.PhysicalStats(ctx, head.ObjectID, 0, nil, mssql.PhysicalSampled)
	if err != nil {
		fmt.Fprintf(logw, "-- skip heap %s.%s: sampled scan: %v\n", head.Schema, head.Table, err)
		return maint.HeapMeasurement{}, false
	}
	if len(ps) == 0 {
		return maint.HeapMeasurement{}, false
	}
	// Aggregate across partitions (heaps are usually single-partition).
	var forwarded, records int64
	var maxFrag, minPageSpace float64 = 0, 100
	for _, s := range ps {
		forwarded += s.ForwardedRecordCount
		records += s.RecordCount
		if s.AvgFragmentationPercent > maxFrag {
			maxFrag = s.AvgFragmentationPercent
		}
		if s.AvgPageSpaceUsedPercent < minPageSpace {
			minPageSpace = s.AvgPageSpaceUsedPercent
		}
	}
	m := maint.HeapMeasurement{
		Schema: head.Schema, Table: head.Table, SizeMB: sizeMB,
		ForwardedRecordCount: forwarded, RecordCount: records,
		FragmentationPercent: maxFrag, PageSpaceUsedPercent: minPageSpace,
		Current: parseCompression(head.Compression),
	}
	if wantComp {
		m.Estimate = estimateFor(ctx, r, head.Schema, head.Table, 0, nil, logw)
		m.Write = writeFor(ctx, r, head.ObjectID, 0, nil)
	}
	return m, true
}

// estimateFor runs sp_estimate for ROW and PAGE and folds them into one estimate,
// summing across partitions. Best-effort: returns nil on error (the decision then
// proceeds without a compression opinion).
func estimateFor(ctx context.Context, r analysisReader, schema, table string, indexID int, partition *int, logw io.Writer) *maint.CompressionEstimate {
	rowRes, err := r.EstimateCompression(ctx, schema, table, indexID, partition, "ROW")
	if err != nil {
		fmt.Fprintf(logw, "-- skip compression estimate on %s.%s: %v\n", schema, table, err)
		return nil
	}
	pageRes, err := r.EstimateCompression(ctx, schema, table, indexID, partition, "PAGE")
	if err != nil {
		fmt.Fprintf(logw, "-- skip compression estimate on %s.%s: %v\n", schema, table, err)
		return nil
	}
	var current, row, page float64
	for _, s := range rowRes {
		current += float64(s.CurrentKB)
		row += float64(s.RequestedKB)
	}
	for _, s := range pageRes {
		page += float64(s.RequestedKB)
	}
	return &maint.CompressionEstimate{CurrentKB: current, RowKB: row, PageKB: page}
}

// writeFor reads operational stats and folds them into a write/read split.
// Best-effort: returns nil on error (the decision then proceeds without a cap).
func writeFor(ctx context.Context, r analysisReader, objectID int64, indexID int, partition *int) *maint.WriteActivity {
	os, err := r.IndexOperationalStats(ctx, objectID, indexID, partition)
	if err != nil {
		return nil
	}
	var w maint.WriteActivity
	for _, s := range os {
		w.Writes += s.LeafInsert + s.LeafUpdate + s.LeafDelete
		w.Reads += s.RangeScan + s.SingletonLookup
	}
	return &w
}

// estimable reports whether an object is worth estimating compression for: a
// rowstore index/heap that is not already PAGE-compressed (nothing higher to try).
func estimable(o mssql.InventoryObject) bool {
	if o.Type != 0 && o.Type != 1 && o.Type != 2 {
		return false // columnstore / XML / spatial use a different model
	}
	c := parseCompression(o.Compression)
	return c == maint.CompressionNone || c == maint.CompressionRow
}

func parseCompression(desc string) maint.Compression {
	switch strings.ToUpper(strings.TrimSpace(desc)) {
	case "ROW":
		return maint.CompressionRow
	case "PAGE":
		return maint.CompressionPage
	case "NONE":
		return maint.CompressionNone
	default:
		return "" // columnstore etc.
	}
}

// tableRef identifies a distinct table for statistics analysis.
type tableRef struct {
	schema, table string
	objectID      int64
}

// groupInventory splits the inventory into contiguous (object, index) partition
// groups (the query is ordered so they are adjacent) and the distinct table list.
func groupInventory(inv []mssql.InventoryObject) ([][]mssql.InventoryObject, []tableRef) {
	var groups [][]mssql.InventoryObject
	var tables []tableRef
	seenTable := map[int64]bool{}

	// Each (object, index) group is a contiguous run in inv (the query orders them
	// adjacent), so groups are sub-slices of inv rather than freshly accumulated.
	start := 0
	for i, o := range inv {
		if i > start && (inv[start].ObjectID != o.ObjectID || inv[start].IndexID != o.IndexID) {
			groups = append(groups, inv[start:i])
			start = i
		}
		if !seenTable[o.ObjectID] {
			seenTable[o.ObjectID] = true
			tables = append(tables, tableRef{schema: o.Schema, table: o.Table, objectID: o.ObjectID})
		}
	}
	if len(inv) > start {
		groups = append(groups, inv[start:])
	}
	return groups, tables
}

// namedManifest is one generated manifest with its target filename and category.
type namedManifest struct {
	filename string
	category string
	manifest *ddl.Manifest
}

// manifestCategories fixes the emission order and filename label per category. The
// category is the maint decision key; fileLabel is the (plural) filename name. The
// slice index is the category's position within a database's block.
var manifestCategories = []struct {
	category  string
	fileLabel string
	label     string // human description in the manifest header
}{
	{"checkdb", "checkdb", "integrity check"},
	{"index", "index", "index reorg/rebuild + compression"},
	{"heap", "heaps", "heap rebuilds"},
	{"statistics", "statistics", "statistics"},
}

// manifestsFromPlan groups one database's operations into per-category manifests,
// named so the engine processes them in a safe order (single-database form).
func manifestsFromPlan(pl maint.Plan, db string) []namedManifest {
	return manifestsForDatabase(pl, db, 0, 3)
}

// manifestsForDatabase emits one database's manifests as a contiguous, ordered
// block. ordinal is the database's 0-based position in a multi-database run and
// width the zero-padding of the numeric prefix, so DB0's manifests sort before
// DB1's, etc. (spec §17.5). With ordinal 0 and width 3 it reproduces the
// single-database 010/020/030/040 naming exactly.
func manifestsForDatabase(pl maint.Plan, db string, ordinal, width int) []namedManifest {
	var out []namedManifest
	for pos, mc := range manifestCategories {
		ops := pl.OperationsByCategory(mc.category)
		if len(ops) == 0 {
			continue
		}
		n := (ordinal*len(manifestCategories) + pos + 1) * 10
		out = append(out, namedManifest{
			filename: fmt.Sprintf("%0*d_maint_%s_%s.yaml", width, n, db, mc.fileLabel),
			category: mc.category,
			manifest: &ddl.Manifest{
				Description: fmt.Sprintf("Maintenance plan for %s — %s (generated by sqlgopace)", db, mc.label),
				Database:    db,
				Operations:  ops,
			},
		})
	}
	return out
}

// prefixWidth returns the zero-padding width for a multi-database run's manifest
// prefixes, wide enough for the largest prefix and never narrower than the
// single-database default (3), so small runs keep the familiar 010… naming.
func prefixWidth(numDatabases int) int {
	maxPrefix := numDatabases * len(manifestCategories) * 10
	width := len(strconv.Itoa(maxPrefix))
	if width < 3 {
		width = 3
	}
	return width
}

// runPlan is the "plan" subcommand: analyze the connected database and materialize
// reviewable maintenance manifests (or, with --dry-run, print them).
func runPlan(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("sqlgopace plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath   = fs.String("config", "", "path to config.yaml (connection + default output directory)")
		profPath     = fs.String("profile", "maintenance_profile.yaml", "path to the maintenance profile")
		database     = fs.String("database", "", "single-database mode: the database to plan (default: the connected database)")
		allDatabases = fs.Bool("all-databases", false, "multi-database mode: plan every eligible user database (spec §17)")
		databases    = fs.String("databases", "", "multi-database mode: comma-separated list of databases to plan")
		categories   = fs.String("categories", "", "comma-separated subset of: "+strings.Join(allCategories, ", ")+" (default: all)")
		outDir       = fs.String("out", "", "directory to write manifests into (default: the config's to_run)")
		dryRun       = fs.Bool("dry-run", false, "print the manifests instead of writing them")
		explain      = fs.Bool("explain", false, "show the reasoning behind each decision")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configPath == "" {
		return errors.New("plan requires --config (for the connection)")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	profile, err := maint.LoadFile(*profPath)
	if err != nil {
		return err
	}
	cats, err := parseCategories(*categories)
	if err != nil {
		return err
	}
	out := *outDir
	if out == "" {
		out = cfg.Directories.ToRun
	}

	ctx := context.Background()
	conn, err := mssql.Open(ctx, cfg.Database.ConnectionString, version.Version())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	fmt.Fprintf(stdout, "-- sqlgopace %s — plan\n", version.Version())

	// Multi-database mode (spec §17): --all-databases or --databases.
	if *allDatabases || strings.TrimSpace(*databases) != "" {
		return planMulti(ctx, stdout, conn, cfg, profile, cats, out, *allDatabases, splitDatabases(*databases), *dryRun, *explain)
	}

	// Single-database mode (the default).
	db := *database
	if db == "" {
		if db, err = conn.CurrentDatabase(ctx); err != nil {
			return err
		}
	}
	plan, err := analyseDatabase(ctx, conn, profile, cats, db, stdout)
	if err != nil {
		return err
	}
	manifests := manifestsFromPlan(plan, db)

	if *explain {
		renderDecisions(stdout, plan)
	}
	if *dryRun {
		return renderManifests(stdout, manifests)
	}
	if err := writeManifests(stdout, out, manifests); err != nil {
		return err
	}
	recordPlanHistory(ctx, stdout, cfg, db, plan, manifests)
	return nil
}

// planMulti runs the plan over several databases (spec §17.5): it surveys
// eligibility, resolves the scope, and for each selected database opens a
// connection in that database's context, analyses it, and emits its manifests as a
// contiguous per-database block. Ineligible/excluded databases are skipped+logged;
// a per-database connection or analysis failure skips that database, never the run.
func planMulti(ctx context.Context, stdout io.Writer, conn *mssql.Conn, cfg *config.Config, profile *maint.Profile, cats categorySet, out string, allFlag bool, explicit []string, dryRun, explain bool) error {
	all, err := conn.UserDatabases(ctx)
	if err != nil {
		return err
	}
	connected, err := conn.CurrentDatabase(ctx)
	if err != nil {
		return err
	}
	selected, skipped := resolveDatabases(all, allFlag, explicit, connected, profile)
	for _, s := range skipped {
		fmt.Fprintf(stdout, "-- skip database %s: %s\n", s.name, s.reason)
	}
	if len(selected) == 0 {
		fmt.Fprintln(stdout, "-- nothing to do: no databases in scope")
		return nil
	}
	fmt.Fprintf(stdout, "-- planning %d database(s): %s\n", len(selected), strings.Join(selected, ", "))

	width := prefixWidth(len(selected))
	var allManifests []namedManifest
	for i, db := range selected {
		dbConn, err := mssql.OpenDatabase(ctx, cfg.Database.ConnectionString, db, version.Version())
		if err != nil {
			fmt.Fprintf(stdout, "-- skip database %s: connect: %v\n", db, err)
			continue
		}
		plan, err := analyseDatabase(ctx, dbConn, profile, cats, db, stdout)
		_ = dbConn.Close()
		if err != nil {
			fmt.Fprintf(stdout, "-- skip database %s: analyze: %v\n", db, err)
			continue
		}
		if explain {
			fmt.Fprintf(stdout, "-- %s:\n", db)
			renderDecisions(stdout, plan)
		}
		manifests := manifestsForDatabase(plan, db, i, width)
		allManifests = append(allManifests, manifests...)
		if !dryRun {
			recordPlanHistory(ctx, stdout, cfg, db, plan, manifests)
		}
	}

	if dryRun {
		return renderManifests(stdout, allManifests)
	}
	return writeManifests(stdout, out, allManifests)
}

// analyseDatabase runs the analysis (cheap inventory → gated reads → decision core)
// for one database and returns the decision plan. It is the shared core of single-
// and multi-database planning and of --auto.
func analyseDatabase(ctx context.Context, r analysisReader, profile *maint.Profile, cats categorySet, db string, logw io.Writer) (maint.Plan, error) {
	in, err := buildInput(ctx, r, profile, cats, db, logw)
	if err != nil {
		return maint.Plan{}, err
	}
	return maint.Decide(in, profile), nil
}

// autoPlan analyses the database through r and returns the generated manifests and
// the full decision plan. It is the shared core of single-database `plan` and `--auto`.
func autoPlan(ctx context.Context, r analysisReader, profile *maint.Profile, cats categorySet, db string, logw io.Writer) ([]namedManifest, maint.Plan, error) {
	plan, err := analyseDatabase(ctx, r, profile, cats, db, logw)
	if err != nil {
		return nil, maint.Plan{}, err
	}
	return manifestsFromPlan(plan, db), plan, nil
}

// runAuto implements the run path's --auto flag: analyze the connected database and
// materialize the generated maintenance manifests into the queue (the config's
// to_run), so the engine then processes them like any other manifests. It records
// the analysis to history, same as the plan subcommand.
func runAuto(ctx context.Context, w io.Writer, conn *mssql.Conn, cfg *config.Config, a autoConfig) error {
	profile, err := maint.LoadFile(a.profile)
	if err != nil {
		return err
	}
	cats, err := parseCategories(a.categories)
	if err != nil {
		return err
	}

	// Multi-database --auto: materialize per-database manifest blocks into the
	// queue; runEngine's per-database loop then executes them.
	if a.allDatabases || strings.TrimSpace(a.databases) != "" {
		return planMulti(ctx, w, conn, cfg, profile, cats, cfg.Directories.ToRun, a.allDatabases, splitDatabases(a.databases), false, false)
	}

	db := a.database
	if db == "" {
		if db, err = conn.CurrentDatabase(ctx); err != nil {
			return err
		}
	}

	fmt.Fprintf(w, "-- auto: analyzing %s\n", db)
	manifests, plan, err := autoPlan(ctx, conn, profile, cats, db, w)
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		fmt.Fprintln(w, "-- auto: nothing to do")
		return nil
	}
	if err := writeManifests(w, cfg.Directories.ToRun, manifests); err != nil {
		return err
	}
	recordPlanHistory(ctx, w, cfg, db, plan, manifests)
	return nil
}

// recordPlanHistory persists the plan's emitted decisions to the local SQLite
// history, when enabled. It is best-effort: a failure is logged, never fatal.
func recordPlanHistory(ctx context.Context, w io.Writer, cfg *config.Config, db string, plan maint.Plan, manifests []namedManifest) {
	if !cfg.History.Enabled {
		return
	}
	h, err := report.OpenHistory(sqlitePath(cfg.History.Destination))
	if err != nil {
		fmt.Fprintf(w, "-- history: %v\n", err)
		return
	}
	defer func() { _ = h.Close() }()

	var recs []report.MaintenanceDecisionRecord
	for _, d := range plan.Decisions {
		if d.Op == nil { // record only emitted operations (the actionable set)
			continue
		}
		recs = append(recs, report.MaintenanceDecisionRecord{
			Category: d.Category, Target: d.Target, Decision: d.Kind, Reason: d.Reason,
			PageCount: d.Metrics.PageCount, SizeMB: d.Metrics.SizeMB,
			FragmentationPercent: d.Metrics.FragmentationPercent, ForwardedPercent: d.Metrics.ForwardedPercent,
			WriteRatio: d.Metrics.WriteRatio, CurrentCompression: d.Metrics.CurrentCompression,
			ChosenCompression: d.Metrics.ChosenCompression,
		})
	}
	run := report.MaintenancePlanRecord{
		Database: db, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Manifests: len(manifests), Operations: len(recs),
	}
	if err := h.RecordMaintenance(ctx, run, recs); err != nil {
		fmt.Fprintf(w, "-- history: %v\n", err)
		return
	}
	fmt.Fprintf(w, "-- recorded %d decision(s) to history (%s)\n", len(recs), cfg.History.Destination)
}

// renderDecisions prints the reasoning trail grouped by category.
func renderDecisions(w io.Writer, plan maint.Plan) {
	fmt.Fprintln(w, "-- analysis:")
	for _, d := range plan.Decisions {
		fmt.Fprintf(w, "--   [%s] %s → %s (%s)\n", d.Category, d.Target, d.Kind, d.Reason)
	}
}

// renderManifests prints each manifest's YAML to w (dry-run).
func renderManifests(w io.Writer, manifests []namedManifest) error {
	if len(manifests) == 0 {
		fmt.Fprintln(w, "-- nothing to do: no operations selected")
		return nil
	}
	for _, nm := range manifests {
		data, err := ddl.MarshalManifest(nm.manifest)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\n-- %s (%d operations)\n%s", nm.filename, len(nm.manifest.Operations), data)
	}
	return nil
}

// writeManifests writes each manifest into dir, creating it if needed.
func writeManifests(w io.Writer, dir string, manifests []namedManifest) error {
	if len(manifests) == 0 {
		fmt.Fprintln(w, "-- nothing to do: no operations selected")
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	written := make([]string, 0, len(manifests))
	for _, nm := range manifests {
		data, err := ddl.MarshalManifest(nm.manifest)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, nm.filename)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		written = append(written, nm.filename)
	}
	sort.Strings(written)
	for _, name := range written {
		fmt.Fprintf(w, "wrote %s\n", filepath.Join(dir, name))
	}
	fmt.Fprintf(w, "%d manifest(s) written to %s — review, then run: sqlgopace --config <config>\n", len(written), dir)
	return nil
}
