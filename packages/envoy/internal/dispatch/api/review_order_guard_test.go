package api

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	ordersRows = regexp.MustCompile(`\border by\b`)
	// readsReviewTable matches the table after a from or a join, since a statement reaches it
	// either way and a join's newest-row pick is as ordinary a shape as a plain read's.
	readsReviewTable = regexp.MustCompile(`\b(?:from|join)\s+` + reviewTable)
	// reviewOrderText is the leading term of docs.ReviewOrder, the one a statement written with
	// another order cannot carry: the two after it, created_at and id, are what the wrong order
	// is made of.
	reviewOrderText = "event_seq desc nulls last"
)

// The guard reads an order by clause and nothing else, so one edge is outside it: a newest-row
// pick written with no order by at all, such as
// `where created_at = (select max(created_at) from artifact_reviews where artifact_id = $1)`,
// reads the table without ordering its rows and is not refused. Reading that shape needs a
// predicate of another kind, a larger change than this guard; a reviewer catches it.

// TestEveryReviewReadOrdersByReviewOrder enforces, over the whole module, what
// docs/review_order_plan_test.go enforces over the three reads it explains: a statement that
// reads artifact_reviews and orders its rows orders them by docs.ReviewOrder, the order the
// reviews' events committed. artifact_reviews.created_at is its transaction's start and its id is
// random, so `created_at desc, id desc` returns a review that committed first ahead of one that
// committed after it - an approval committed while a version write was open ahead of the
// approval that write's own carry made, for one - and a reader of the newest review then reads
// the wrong verdict. Migration 0120 added event_seq and ReviewOrder for that, and this test
// exists because the plan test could not catch a read written outside the docs package, as
// api.newestReviewAsApproval first was.
//
// Its reach is the statement as the module scan can assemble it (TestNoForUpdateOnOwnerTables):
// string literals and chains of literals and package-level constants, in every non-test Go file
// of the module. A statement that orders no rows is outside it, as are the migrations, which are
// SQL files the scan does not read, and test sources, which hold the previous release's
// statements on purpose.
func TestEveryReviewReadOrdersByReviewOrder(t *testing.T) {
	sources := moduleSources(t, "../../..")
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	seen := map[string]struct{}{}
	for _, source := range sources {
		for _, statement := range sqlStatements(source.file, constants) {
			if !readsReviewTable.MatchString(statement) || !ordersRows.MatchString(statement) {
				continue
			}
			if strings.Contains(statement, reviewOrderText) {
				continue
			}
			offence := filepath.ToSlash(source.path) + ": " + statement
			if _, repeat := seen[offence]; repeat {
				continue
			}
			seen[offence] = struct{}{}
			offences = append(offences, offence)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("a read of artifact_reviews orders its rows otherwise than docs.ReviewOrder:\n\t%s\n"+
			"Order by docs.ReviewOrder, which reads artifact_reviews_by_artifact_event_seq: "+
			"created_at is the writing transaction's start and id is random, so the two alone "+
			"rank a review that committed later below one that committed first.",
			strings.Join(offences, "\n\t"))
	}
}

// The guard reads the order of a statement written as the repository writes one - several raw
// string lines spliced with the ReviewOrder constant - and refuses the order migration 0120
// replaced, whether the clause sits in the same literal as the table or in another.
func TestTheReviewOrderGuardReadsBothOrders(t *testing.T) {
	right := "select id from artifact_reviews where artifact_id = $1 order by " +
		"event_seq desc nulls last, created_at desc, artifact_reviews.id desc limit 1"
	wrong := "select id from artifact_reviews where artifact_id = $1 order by created_at desc, id desc limit 1"
	for _, test := range []struct {
		name      string
		statement string
		refused   bool
	}{
		{name: "ReviewOrder", statement: right},
		{name: "the order 0120 replaced", statement: wrong, refused: true},
		{name: "a read that orders nothing", statement: "select 1 from artifact_reviews where artifact_id = $1"},
		{name: "another table's order", statement: "select id from artifact_versions order by created_at desc"},
		{
			name: "the order 0120 replaced, reached through a join",
			statement: "select r.id from asks a join artifact_reviews r on r.artifact_id = a.artifact_id " +
				"order by r.created_at desc, r.id desc limit 1",
			refused: true,
		},
		{
			name: "a join that picks no newest row",
			statement: "select count(*) from asks a join artifact_reviews r on r.artifact_id = a.artifact_id " +
				"where a.id = $1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			refused := readsReviewTable.MatchString(test.statement) &&
				ordersRows.MatchString(test.statement) &&
				!strings.Contains(test.statement, reviewOrderText)
			if refused != test.refused {
				t.Fatalf("refused = %t, want %t for %q", refused, test.refused, test.statement)
			}
		})
	}
}
