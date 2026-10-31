package refs

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
)

// TestStatementTimings measures this package's changed statements on the seeded measurement
// database (benchtime), which DISPATCH_BENCH_DATABASE_URL names. CI never sets that variable, so
// CI never runs a measurement; api/search_bench_test.go and api/related_recall_test.go are gated
// the same way.
//
// Which protocol each statement is under, and why:
//
//   - The closure is a protocol 1 pair. Its gain is its own text: documentBlockHeld replaced an
//     `exists` probe that sat under an OR, which the planner could only run as a hashed subplan
//     over all of document_blocks. Both texts read the same tables and the same indexes, so one
//     schema holds both sides. The closure also gains from asks (issue_key) and
//     messages (issue_key, created_at, id), the two indexes migrations 0151 and 0152 add, and
//     that part of its gain is not in this pair: both sides here are read with those indexes
//     present, which only helps main's side, so the pair is the conservative reading of the
//     text's own gain. Seam 7's two-commit run takes the whole gain under protocol 2.
//
//   - The two backlink counts are protocol 2, head text alone. Main counted over the whole
//     graph_edges view (this package holds that read as backlinkCountOracle), and the before
//     side of record is that statement on a schema without ask_followers_session_id, which
//     migration 0150 adds and this branch's schema therefore has. One database cannot be both,
//     so the pair is seam 7's two-commit run and what is measured here is the head side of it,
//     with its plan, under the same protocol as every other reading.
func TestStatementTimings(t *testing.T) {
	runner := benchtime.Open(t)
	subjects := runner.Subjects()

	// mainClosureQuery is main's text, held in closure_test.go as the equality oracle. It is
	// read from there rather than copied again: two copies of one statement drift apart, and the
	// oracle is already what a comparison of this package is held to.
	runner.Pair(t, "refs.Closure",
		benchtime.Statement{SQL: mainClosureQuery, Args: []any{subjects.Issue}},
		benchtime.Statement{SQL: closureQuery, Args: []any{subjects.Issue}})

	// The issue read's count: one issue's inbound edges, the number its page carries.
	issueCount, ok := backlinkCountSQL("issue", nil)
	if !ok {
		t.Fatal("backlinkArms declares no arm for an issue, so the issue read's count has no statement")
	}
	runner.Head(t, "refs.BacklinkCounts over an issue",
		benchtime.Statement{SQL: issueCount, Args: []any{[]string{subjects.Issue}, nodeKinds}})

	// The ask page's count: every ask of one issue at once, each ask's own clarification thread
	// excluded, which is what the page's cards show.
	askIDs := issueAskIDs(t, runner, subjects.Issue)
	askCount, ok := backlinkCountSQL("ask", AskBacklinkExclusions)
	if !ok {
		t.Fatal("backlinkArms declares no arm for an ask, so the ask page's count has no statement")
	}
	runner.Head(t, "refs.BacklinkCounts over an issue's asks",
		benchtime.Statement{SQL: askCount, Args: []any{askIDs, nodeKinds, AskBacklinkExclusions}})
}

// issueAskIDs is every ask of the issue, the set the ask page counts backlinks for in one call.
// An issue with no ask would measure a count over an empty array, which answers no row whatever
// the statement reads, so the run fails on it instead.
func issueAskIDs(t *testing.T, runner *benchtime.Runner, issueKey string) []string {
	t.Helper()
	rows, err := runner.Conn().Query(context.Background(),
		`select a.id::text from asks a where a.issue_key = $1 order by a.created_at, a.id`, issueKey)
	if err != nil {
		t.Fatalf("read %s's asks: %v", issueKey, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s's asks: %v", issueKey, err)
	}
	if len(ids) == 0 {
		t.Fatalf("%s holds no ask, so the ask page's count has no subject", issueKey)
	}
	return ids
}
