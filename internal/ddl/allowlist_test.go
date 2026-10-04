package ddl_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// data_compression and a column's type reach the generated SQL verbatim. A manifest is a
// trusted input (SECURITY.md), so this is hardening rather than a fix: a field that reads
// as an enumeration should be one, for an operator reviewing a manifest as much as for
// least privilege.
func TestDataCompressionIsAnAllowlist(t *testing.T) {
	for _, v := range []string{"NONE", "ROW", "PAGE", "page", "COLUMNSTORE", "COLUMNSTORE_ARCHIVE"} {
		src := "operations:\n  - operation: rebuild_index\n    schema: dbo\n    table: T\n    index: IX\n    data_compression: " + v + "\n"
		if _, err := ddl.ParseManifest(strings.NewReader(src)); err != nil {
			t.Errorf("data_compression %q rejected: %v", v, err)
		}
	}
	for _, op := range []string{
		"rebuild_index\n    index: IX",
		"create_index\n    index: IX\n    columns: [a]",
		"rebuild_heap",
	} {
		src := "operations:\n  - operation: " + op + "\n    schema: dbo\n    table: T\n    data_compression: \"PAGE) WITH (ONLINE = OFF\"\n"
		_, err := ddl.ParseManifest(strings.NewReader(src))
		if !errors.Is(err, ddl.ErrInvalidManifest) || !strings.Contains(err.Error(), "data_compression") {
			t.Errorf("%s: err = %v, want the bad data_compression refused", strings.SplitN(op, "\n", 2)[0], err)
		}
	}
}

func TestColumnTypeHasTheShapeOfAType(t *testing.T) {
	for _, v := range []string{"BIT", "NVARCHAR(400)", "nvarchar(max)", "DECIMAL(18, 2)", "datetime2(7)", "dbo.PhoneNumber"} {
		src := "operations:\n  - operation: add_column\n    schema: dbo\n    table: T\n    column: C\n    nullable: true\n    type: \"" + v + "\"\n"
		if _, err := ddl.ParseManifest(strings.NewReader(src)); err != nil {
			t.Errorf("type %q rejected: %v", v, err)
		}
	}
	for _, v := range []string{"INT; DROP TABLE dbo.T", "INT NULL --", "VARCHAR(50) COLLATE Latin1_General_CI_AS"} {
		for _, op := range []string{"add_column", "alter_column"} {
			src := "operations:\n  - operation: " + op + "\n    schema: dbo\n    table: T\n    column: C\n    nullable: true\n    type: \"" + v + "\"\n"
			_, err := ddl.ParseManifest(strings.NewReader(src))
			if !errors.Is(err, ddl.ErrInvalidManifest) || !strings.Contains(err.Error(), "type") {
				t.Errorf("%s type %q: err = %v, want it refused", op, v, err)
			}
		}
	}
}
