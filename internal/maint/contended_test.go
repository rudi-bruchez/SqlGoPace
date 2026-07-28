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
