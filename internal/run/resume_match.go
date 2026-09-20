package run

import (
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/ddl"
)

// sameRebuildStatement reports whether a paused resumable rebuild is the same work as
// the statement about to be issued.
//
// SQL Server resumes a paused rebuild when the original statement is re-executed "with
// the same parameters" (ALTER INDEX, Online index operations) and rejects a differing
// one. Deciding that from text is a proxy for what the server compares, so the proxy is
// deliberately STRICT: it ignores only layout — surrounding and internal whitespace, and
// one trailing semicolon — and nothing else.
//
// It does NOT case-fold. Identifiers are compared under the database's collation, which
// may be case-sensitive, and string literals are data; folding either would let two
// statements that build different indexes compare equal. The cost of being strict is a
// statement typed by hand with different spacing around punctuation failing to match, and
// that is the right direction to fail: a missed match asks the operator to act, while a
// false match silently resumes a rebuild carrying options nobody chose.
//
// Known limitation: collapsing whitespace runs collapses them inside delimited
// identifiers too, so [A B] and [A  B] compare equal. That cannot produce a wrong resume,
// because the caller matches the index name against sys.index_resumable_operations before
// it gets here and those two names differ. Parsing SQL to close it would cost far more
// than the case it covers.
//
// An empty stored statement means the server did not tell us, which is not a match.
func sameRebuildStatement(stored, generated string) bool {
	if strings.TrimSpace(stored) == "" {
		return false
	}
	return normalizeStatement(stored) == normalizeStatement(generated)
}

// isIndexOperation reports whether op names an index that could hold a paused resumable
// rebuild. ResumableControlSQL is the authority on that — it is what would have to build
// the control statement — so this asks it rather than keeping a second list of operation
// types in step with it. An operation with no index is not an error, just not a candidate.
func isIndexOperation(op ddl.Operation) bool {
	_, err := ddl.ResumableControlSQL(op, "ABORT")
	return err == nil
}

// normalizeStatement collapses whitespace runs to one space and drops a trailing
// semicolon, so two renderings of one statement compare equal. Nothing else is touched.
func normalizeStatement(s string) string {
	return strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(s), ";")), " ")
}
