package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// reviewWriter is the one source file allowed to write a row of artifact_reviews.
const reviewWriter = "internal/dispatch/docs/approval_review.go"

// reviewTable names artifact_reviews as a statement may write it: bare, schema-qualified, quoted,
// or both.
const reviewTable = `(?:"?public"?\.)?(?:"artifact_reviews"|artifact_reviews\b)`

var (
	reviewInsertStatement = regexp.MustCompile(`(?i)\b(?:insert\s+into|merge\s+into|copy)\s+` + reviewTable)
	reviewUpdateStatement = regexp.MustCompile(`(?i)\bupdate\s+(?:only\s+)?` + reviewTable)
)

// TestOnlyWriteReviewWritesReviews enforces what docs.ReviewOrder's `event_seq desc nulls last`
// rests on: every review committed after migration 0118 carries the sequence of its artifact.*
// event, so a row without one is older than every row with one. docs.WriteReview inserts the row
// and then writes the sequence of the event it appended; the migration's trigger covers the
// release before 0118 alone, and only when the inserting transaction appended that event on the
// document's owner. A review inserted anywhere else, with no event or with the event appended
// another way, would commit unsequenced and read as older than every sequenced review, so a
// person's newest approval or changes request would lose to the one before it; and an update
// written anywhere else could clear or move a sequence the read orders by.
//
// Its reach is the statement as the module scan can assemble it (TestNoForUpdateOnOwnerTables):
// string literals and chains of literals and package-level constants, in every non-test Go file
// of the module, matching an insert, a merge, a copy or an update of the table under any of the
// names reviewTable reads; and a pgx.Identifier literal naming the table, which a CopyFrom takes.
// A table name built at run time is beyond it. A test writes reviews directly on purpose -- the
// previous release's insert, a row recorded before the column -- so test sources are outside it,
// as are the migrations, which are SQL files the scan does not read.
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
		writer := strings.HasSuffix(path, reviewWriter)
		for _, statement := range sqlStatements(source.file, constants) {
			insert := reviewInsertStatement.MatchString(statement)
			update := reviewUpdateStatement.MatchString(statement)
			if !insert && !update {
				continue
			}
			if writer {
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
		if !writer {
			for _, line := range reviewTableIdentifiers(source.file, source.positions) {
				offences = append(offences, path+":"+strconv.Itoa(line)+": pgx.Identifier naming artifact_reviews")
			}
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

// reviewTableIdentifiers is the line of every pgx.Identifier literal in file that names
// artifact_reviews, which pgx's CopyFrom writes rows into.
func reviewTableIdentifiers(file *ast.File, positions *token.FileSet) []int {
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		selector, ok := literal.Type.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Identifier" {
			return true
		}
		for _, element := range literal.Elts {
			value, ok := element.(*ast.BasicLit)
			if !ok || value.Kind != token.STRING {
				continue
			}
			if name, err := strconv.Unquote(value.Value); err == nil && name == "artifact_reviews" {
				lines = append(lines, positions.Position(literal.Pos()).Line)
			}
		}
		return true
	})
	return lines
}

// The patterns read the table under every name a statement can give it, and refuse no other
// table whose name starts the same way.
func TestTheReviewWriterPatternsReadEveryNameOfTheTable(t *testing.T) {
	for _, statement := range []string{
		`insert into artifact_reviews (id) values ($1)`,
		`INSERT INTO public.artifact_reviews (id) values ($1)`,
		`insert into "artifact_reviews" (id) values ($1)`,
		`insert into "public"."artifact_reviews" (id) values ($1)`,
		`merge into artifact_reviews using x on true when matched then do nothing`,
		`copy artifact_reviews from stdin`,
		`update artifact_reviews set event_seq = 1`,
		`update only artifact_reviews set event_seq = 1`,
		`update public.artifact_reviews set event_seq = 1`,
	} {
		if !reviewInsertStatement.MatchString(statement) && !reviewUpdateStatement.MatchString(statement) {
			t.Errorf("%q writes artifact_reviews, but neither pattern matches it", statement)
		}
	}
	for _, statement := range []string{
		`insert into artifact_reviews_archive (id) values ($1)`,
		`update artifact_reviewsx set a = 1`,
		`select * from artifact_reviews`,
	} {
		if reviewInsertStatement.MatchString(statement) || reviewUpdateStatement.MatchString(statement) {
			t.Errorf("%q does not write artifact_reviews, but a pattern matches it", statement)
		}
	}
}

// A pgx.Identifier literal naming artifact_reviews, which a CopyFrom writes rows into, is found by
// its line, and one naming another table is not.
func TestTheReviewWriterScanFindsAnIdentifierNamingTheTable(t *testing.T) {
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "copy.go", `package x

import "github.com/jackc/pgx/v5"

var reviews = pgx.Identifier{"artifact_reviews"}
var others = pgx.Identifier{"artifact_reviews_archive"}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewTableIdentifiers(file, positions); len(got) != 1 || got[0] != 5 {
		t.Fatalf("identifier lines = %v, want [5]", got)
	}
}
