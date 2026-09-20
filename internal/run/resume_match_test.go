package run

import "testing"

// SQL Server resumes a paused resumable rebuild when the original statement is
// re-executed "with the same parameters" (ALTER INDEX, Online index operations), and
// rejects a differing one with Msg 10637. These tests pin what "the same parameters"
// means to SqlGoPace: the same work, not the same bytes.

func TestSameRebuildStatementIgnoresFormatting(t *testing.T) {
	stored := "ALTER INDEX [IX] ON [dbo].[T] REBUILD WITH (ONLINE = ON, RESUMABLE = ON);"
	generated := "ALTER  INDEX [IX] ON [dbo].[T]\n\tREBUILD WITH (ONLINE = ON, RESUMABLE = ON)"
	if !sameRebuildStatement(stored, generated) {
		t.Error("whitespace runs and a trailing semicolon do not change the work; want a match")
	}
}

func TestSameRebuildStatementRejectsDifferentCompression(t *testing.T) {
	row := "ALTER INDEX [IX] ON [dbo].[T] REBUILD WITH (RESUMABLE = ON, DATA_COMPRESSION = ROW);"
	page := "ALTER INDEX [IX] ON [dbo].[T] REBUILD WITH (RESUMABLE = ON, DATA_COMPRESSION = PAGE);"
	if sameRebuildStatement(row, page) {
		t.Error("ROW and PAGE are different work; resuming one as the other would be wrong")
	}
}

// An unknown stored statement must never be read as a match: sys.index_resumable_operations
// can return it empty, and "I could not tell" is not "it is mine".
func TestSameRebuildStatementRejectsUnknownText(t *testing.T) {
	if sameRebuildStatement("", "ALTER INDEX [IX] ON [dbo].[T] REBUILD WITH (RESUMABLE = ON);") {
		t.Error("an empty stored statement is unknown, not a match")
	}
}

// Identifiers are compared under the database's collation, which may be case-sensitive:
// [T] and [t] can be two different tables. Case-folding the statement would make a rebuild
// of one look like a rebuild of the other.
func TestSameRebuildStatementRejectsDifferentIdentifierCase(t *testing.T) {
	upper := "ALTER INDEX [IX] ON [dbo].[T] REBUILD WITH (RESUMABLE = ON);"
	lower := "ALTER INDEX [IX] ON [dbo].[t] REBUILD WITH (RESUMABLE = ON);"
	if sameRebuildStatement(upper, lower) {
		t.Error("[T] and [t] may be different objects under a case-sensitive collation")
	}
}

// Collapsing whitespace runs also collapses them inside a delimited identifier, so
// [A B] and [A  B] compare equal here. That is a known limitation of comparing text, and
// it is not reachable as a wrong resume: the caller matches the index name against
// sys.index_resumable_operations separately before consulting this function, and those
// two names differ. Documented rather than parsed around — see resume_match.go.
func TestSameRebuildStatementCollapsesSpacingInsideIdentifiers(t *testing.T) {
	a := "ALTER INDEX [A B] ON [dbo].[T] REBUILD WITH (RESUMABLE = ON);"
	b := "ALTER INDEX [A  B] ON [dbo].[T] REBUILD WITH (RESUMABLE = ON);"
	if !sameRebuildStatement(a, b) {
		t.Error("pins the known limitation: internal whitespace is collapsed everywhere")
	}
}
