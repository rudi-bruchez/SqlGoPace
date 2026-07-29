package maint

import "testing"

func TestParseContendedValid(t *testing.T) {
	data := []byte(`
database: PRODDB
observed:
  - object_id: 261575970
    schema: dbo
    table: MEASUREMENT
    lock_mode: Sch-M
    times_blocked: 3
    first_seen: "2026-07-28T11:10:09Z"
    last_seen: "2026-07-28T11:19:09Z"
`)
	doc, err := ParseContended(data)
	if err != nil {
		t.Fatalf("ParseContended: %v", err)
	}
	if doc.Database != "PRODDB" || len(doc.Observed) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	o := doc.Observed[0]
	if o.ObjectID != 261575970 || o.Table != "MEASUREMENT" || o.TimesBlocked != 3 {
		t.Errorf("object = %+v", o)
	}
}

func TestParseContendedRejectsUnknownField(t *testing.T) {
	if _, err := ParseContended([]byte("database: X\nbogus: 1\n")); err == nil {
		t.Fatal("expected error on unknown field")
	}
}

func TestParseContendedRoundTripsNewFields(t *testing.T) {
	in := []byte(`database: MyDB
observed:
    - object_id: 42
      schema: dbo
      table: Big
      confirmed_by: tail_position
      index_id: 1
      page_from_end: 3
`)
	doc, err := ParseContended(in)
	if err != nil {
		t.Fatalf("ParseContended: %v", err)
	}
	if len(doc.Observed) != 1 {
		t.Fatalf("observed = %d, want 1", len(doc.Observed))
	}
	o := doc.Observed[0]
	if o.ConfirmedBy != "tail_position" || o.IndexID != 1 || o.PageFromEnd != 3 {
		t.Errorf("new fields not decoded: %+v", o)
	}
}

func TestParseContendedAcceptsLegacySidecar(t *testing.T) {
	// A sidecar written before this change has none of the new fields.
	in := []byte(`database: MyDB
observed:
    - object_id: 7
      schema: dbo
      table: Old
      lock_mode: Sch-M
      times_blocked: 2
      first_seen: t0
      last_seen: t1
`)
	if _, err := ParseContended(in); err != nil {
		t.Fatalf("legacy sidecar must still parse: %v", err)
	}
}
