package ddl_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// indexes returns a lookup that always yields the given descriptors.
func indexes(descs ...ddl.IndexDescriptor) func(string, string) ([]ddl.IndexDescriptor, error) {
	return func(string, string) ([]ddl.IndexDescriptor, error) { return descs, nil }
}

func TestExpandRebuildAll(t *testing.T) {
	m := &ddl.Manifest{
		Description: "rebuild all",
		Operations: []ddl.Operation{
			ddl.RebuildIndex{
				Schema: "dbo", Table: "T", Index: "ALL",
				DataCompression: "PAGE",
				Options:         ddl.OptionOverrides{MaxDOP: intPtr(2)},
			},
		},
	}
	// Server returns nonclustered before clustered, plus a columnstore index.
	lookup := indexes(
		ddl.IndexDescriptor{Name: "IX_nc", Kind: ddl.KindRowstore},
		ddl.IndexDescriptor{Name: "PK_T", Clustered: true, Kind: ddl.KindRowstore},
		ddl.IndexDescriptor{Name: "CCI", Kind: ddl.KindColumnstore},
	)

	out, err := ddl.ExpandRebuildAll(m, lookup)
	if err != nil {
		t.Fatalf("ExpandRebuildAll() error = %v", err)
	}
	if len(out.Operations) != 3 {
		t.Fatalf("expanded to %d operations, want 3", len(out.Operations))
	}

	// Clustered first, then input order for the rest.
	wantNames := []string{"PK_T", "IX_nc", "CCI"}
	for i, want := range wantNames {
		ri, ok := out.Operations[i].(ddl.RebuildIndex)
		if !ok {
			t.Fatalf("operation %d is %T, want RebuildIndex", i, out.Operations[i])
		}
		if ri.Index != want {
			t.Errorf("operation %d index = %q, want %q", i, ri.Index, want)
		}
		if ri.Schema != "dbo" || ri.Table != "T" {
			t.Errorf("operation %d target = %s.%s, want dbo.T", i, ri.Schema, ri.Table)
		}
		if ri.Options.MaxDOP == nil || *ri.Options.MaxDOP != 2 {
			t.Errorf("operation %d MaxDOP override not carried", i)
		}
	}

	// data_compression carried to rowstore, dropped for columnstore.
	if ri := out.Operations[0].(ddl.RebuildIndex); ri.DataCompression != "PAGE" {
		t.Errorf("rowstore DataCompression = %q, want PAGE", ri.DataCompression)
	}
	if ri := out.Operations[2].(ddl.RebuildIndex); ri.DataCompression != "" {
		t.Errorf("columnstore DataCompression = %q, want empty (dropped)", ri.DataCompression)
	}
}

func TestExpandPassesThroughNonAll(t *testing.T) {
	add := ddl.AddColumn{Schema: "dbo", Table: "T", Column: "C", DataType: "BIT"}
	named := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "IX_one"}
	m := &ddl.Manifest{Operations: []ddl.Operation{add, named}}

	out, err := ddl.ExpandRebuildAll(m, func(string, string) ([]ddl.IndexDescriptor, error) {
		t.Fatalf("lookup should not be called for non-ALL operations")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ExpandRebuildAll() error = %v", err)
	}
	if diff := cmp.Diff([]ddl.Operation{add, named}, out.Operations); diff != "" {
		t.Errorf("operations changed (-want +got):\n%s", diff)
	}
}

func TestExpandEmptyIndexList(t *testing.T) {
	m := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "Heap", Index: "ALL"},
	}}
	out, err := ddl.ExpandRebuildAll(m, indexes())
	if err != nil {
		t.Fatalf("ExpandRebuildAll() error = %v", err)
	}
	if len(out.Operations) != 0 {
		t.Errorf("expanded to %d operations, want 0 (no rebuildable index)", len(out.Operations))
	}
}

func TestExpandLookupError(t *testing.T) {
	sentinel := errors.New("boom")
	m := &ddl.Manifest{Operations: []ddl.Operation{
		ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "ALL"},
	}}
	_, err := ddl.ExpandRebuildAll(m, func(string, string) ([]ddl.IndexDescriptor, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("ExpandRebuildAll() error = %v, want wrapping %v", err, sentinel)
	}
}

// A rowstore index from expansion resolves like a normal named index: RESUMABLE
// applies on 2022 Enterprise (the whole point of expanding ALL).
func TestResolveExpandedRowstoreKeepsResumable(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "PK_T", Kind: ddl.KindRowstore}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, _ := ddl.Resolve(op, target, m, ddl.Policy{})

	if !got.Online || !got.Resumable || !got.WaitAtLowPriority {
		t.Errorf("rowstore: Online/Resumable/WALP = %t/%t/%t, want all true",
			got.Online, got.Resumable, got.WaitAtLowPriority)
	}
}

// A columnstore index rejects the ONLINE family: it must rebuild offline.
func TestResolveColumnstoreRebuildsOffline(t *testing.T) {
	m := resolveMatrix()
	op := ddl.RebuildIndex{Schema: "dbo", Table: "T", Index: "CCI", Kind: ddl.KindColumnstore}
	target := ddl.Target{MajorVersion: 16, Tier: ddl.TierEnterprise}

	got, decisions := ddl.Resolve(op, target, m, ddl.Policy{})

	if got.Online || got.Resumable || got.WaitAtLowPriority {
		t.Errorf("columnstore: Online/Resumable/WALP = %t/%t/%t, want all false",
			got.Online, got.Resumable, got.WaitAtLowPriority)
	}
	if v, ok := decisionValue(decisions, "resumable"); !ok || v != "OFF" {
		t.Errorf("decision[resumable] = %q (present=%t), want OFF with a reason", v, ok)
	}
}
