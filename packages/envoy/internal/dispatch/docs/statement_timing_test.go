package docs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
)

// anchoredMarkTimingLimit is how many of the document's mark ids the three reads that name marks
// are measured with: an edit sends the marks of the table rows it touches, a handful, not every
// mark the document carries. Twenty is the size of a large edit's set.
const anchoredMarkTimingLimit = 20

// TestStatementTimings measures this package's changed anchor reads on the seeded measurement
// database (benchtime), which DISPATCH_BENCH_DATABASE_URL names. CI never sets that variable, so
// CI never runs a measurement; api/search_bench_test.go and api/related_recall_test.go are gated
// the same way.
//
// All six reads are protocol 1 pairs: openAnchoredMarks over asks and over comments, the two
// reads of edits.go (rejectUnindexedTableMarks, rejectLiveTableAnchors) and the two of
// mutation.go (lockTableAnchorRows). Their gain is their own text and no index this change adds.
// Both indexes the new texts reach were already there - comments_anchor_artifact is migration
// 0032 and asks_anchor_artifact_id migration 0148, both below this branch's block, which starts
// at 0150 - and what each read gained is the uuid comparison beside its text comparison, which
// is what makes the index eligible at all. One schema therefore holds both sides, and the pair
// is the committed regression check: main's text cannot use the index, so the day a read loses
// that comparison its median goes back to main's and the pair fails.
//
// Each pair's main text is the oracle anchor_reads_test.go already holds, read from there rather
// than copied a second time: two copies of one statement drift apart, and that oracle is what
// the package's equality test is held to.
func TestStatementTimings(t *testing.T) {
	runner := benchtime.Open(t)
	document := runner.Subjects().AnchoredDocument
	markIDs := anchoredMarkIDs(t, runner, document)
	for _, read := range anchorReads() {
		// The new text adds no parameter: it reads $1 twice, once as text and once as a uuid, so
		// both sides of a pair bind exactly the same arguments.
		arguments := anchorReadTimingArguments(read.query, document, markIDs)
		runner.Pair(t, "docs."+read.name,
			benchtime.Statement{SQL: read.oracle, Args: arguments},
			benchtime.Statement{SQL: read.query, Args: arguments})
	}
}

// anchorReadTimingArguments are what a read binds: the document, and the mark ids for a read
// that names marks. The read's own text says which it is, as anchorReadArguments reads it for
// the plan test.
func anchorReadTimingArguments(statement, document string, markIDs []string) []any {
	if !strings.Contains(statement, "$2") {
		return []any{document}
	}
	return []any{document, markIDs}
}

// anchoredMarkIDs are mark ids the document actually carries, taken through each table's anchor
// index rather than by the text comparison this change is about: the subject read of a
// measurement must not itself read two tables whole, and it would warm pages unevenly between
// the sides of the first pair.
func anchoredMarkIDs(t *testing.T, runner *benchtime.Runner, document string) []string {
	t.Helper()
	ctx := context.Background()
	var markIDs []string
	for _, target := range anchorTargets {
		rows, err := runner.Conn().Query(ctx, `select anchor->>'mark_id' from `+target.table+
			` where `+target.anchoredOn()+` and anchor->>'mark_id' is not null order by 1 limit $2`,
			document, anchoredMarkTimingLimit)
		if err != nil {
			t.Fatalf("read the marks anchored on %s in %s: %v", document, target.table, err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the marks anchored on %s in %s: %v", document, target.table, err)
		}
		markIDs = append(markIDs, found...)
	}
	slices.Sort(markIDs)
	markIDs = slices.Compact(markIDs)
	if len(markIDs) == 0 {
		t.Fatalf("no ask or comment is anchored on %s, so the reads that name marks have no subject", document)
	}
	return markIDs[:min(len(markIDs), anchoredMarkTimingLimit)]
}
