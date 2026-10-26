package api

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reviewWriter is the one source file allowed to write a row of artifact_reviews.
const reviewWriter = "internal/dispatch/docs/approval_review.go"

var (
	reviewInsertStatement = regexp.MustCompile(`(?i)\binsert\s+into\s+artifact_reviews\b`)
	reviewUpdateStatement = regexp.MustCompile(`(?i)\bupdate\s+artifact_reviews\b`)
)

// TestOnlyWriteReviewWritesReviews enforces what docs.ReviewOrder's `event_seq desc nulls last`
// rests on: every review committed after migration 0117 carries the sequence of its artifact.*
// event, so a row without one is older than every row with one. docs.WriteReview inserts the row
// and then writes the sequence of the event it appended; the migration's trigger covers the
// release before 0117 alone, and only when the inserting transaction appended that event on the
// document's owner. A review inserted anywhere else, with no event or with the event appended
// another way, would commit unsequenced and read as older than every sequenced review, so a
// person's newest approval or changes request would lose to the one before it; and an update
// written anywhere else could clear or move a sequence the read orders by.
//
// Its reach is the statement as the module scan can assemble it (TestNoForUpdateOnOwnerTables):
// string literals and chains of literals and package-level constants, in every non-test Go file
// of the module. A test writes reviews directly on purpose -- the previous release's insert, a
// row recorded before the column -- so test sources are outside it, as are the migrations, which
// are SQL files the scan does not read.
func TestOnlyWriteReviewWritesReviews(t *testing.T) {
	sources := moduleSources(t, "../../..")
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	var offences []string
	inserts, updates := 0, 0
	for _, source := range sources {
		path := filepath.ToSlash(source.path)
		for _, statement := range sqlStatements(source.file, constants) {
			insert := reviewInsertStatement.MatchString(statement)
			update := reviewUpdateStatement.MatchString(statement)
			if !insert && !update {
				continue
			}
			if strings.HasSuffix(path, reviewWriter) {
				if insert {
					inserts++
				}
				if update {
					updates++
				}
				continue
			}
			offences = append(offences, path+": "+statement)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("only docs.WriteReview (%s) may write artifact_reviews, and these statements also do:\n\t%s\n"+
			"Record a review through docs.WriteReview instead. It writes the sequence of the review's "+
			"artifact.* event in the review's own transaction, and docs.ReviewOrder ranks a review "+
			"without one below every review with one.", reviewWriter, strings.Join(offences, "\n\t"))
	}
	if inserts != 1 || updates != 1 {
		t.Fatalf("%s holds %d review inserts and %d review updates the scan can see, want 1 of each: "+
			"if WriteReview's statements moved or were written around a value this scan cannot "+
			"resolve, this guard now passes by seeing nothing", reviewWriter, inserts, updates)
	}
}
