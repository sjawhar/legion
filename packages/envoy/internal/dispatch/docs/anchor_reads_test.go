package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// anchorRead is one statement this package runs to find a document's anchored asks or comments:
// the function that owns it, its text, the indexes every scan of it must use, and whether its
// rows come back in an order the statement itself fixes. Both tests below are a walk over this
// one list, so a read added to the package is added here once and is then held to its index and
// to main's rows alike.
type anchorRead struct {
	name    string
	query   string
	oracle  string
	indexes []string
	ordered bool
}

// mainOpenAnchoredAsksQuery and the five oracles below it are the statements main runs, copied
// from main as literals rather than built from this package's own helpers: built from the
// helpers, an edit to a helper would move the oracle and the read together and the comparison
// would pass whatever either said. Each is the read before this change added the uuid
// comparison that its table's anchor index carries.
const (
	mainOpenAnchoredAsksQuery = `
		select id::text, anchor, 'dispatchAsk'
		from asks
		where anchor is not null and anchor->>'artifact_id' = $1 and state = 'open'
		order by created_at, id
	`
	mainOpenAnchoredCommentsQuery = `
		select id::text, anchor, case when suggestion is null then 'proofComment' else 'proofSuggestion' end
		from comments
		where anchor is not null and anchor->>'artifact_id' = $1 and not resolved
		order by created_at, id
	`
	mainTableAnchorRecordsQuery = `
		select anchor->>'mark_id' from asks
		where anchor->>'artifact_id' = $1 and anchor->>'mark_id' = any($2::text[])
		union
		select anchor->>'mark_id' from comments
		where anchor->>'artifact_id' = $1 and anchor->>'mark_id' = any($2::text[])
	`
	mainActiveTableAnchorsQuery = `
		select 'ask', id::text, anchor->>'mark_id'
		from asks
		where anchor->>'artifact_id' = $1 and state = 'open' and anchor->>'mark_id' = any($2::text[])
		union all
		select 'comment', id::text, anchor->>'mark_id'
		from comments
		where anchor->>'artifact_id' = $1 and not resolved and anchor->>'mark_id' = any($2::text[])
		order by 1, 2
	`
	mainLockTableAskAnchorsQuery = `
		select id::text, anchor->>'mark_id', state
		from asks
		where anchor->>'artifact_id' = $1 and anchor->>'mark_id' = any($2::text[])
		for share
	`
	mainLockTableCommentAnchorsQuery = `
		select id::text, anchor->>'mark_id', resolved
		from comments
		where anchor->>'artifact_id' = $1 and anchor->>'mark_id' = any($2::text[])
		for share
	`
)

// anchorReads is every read of this package that finds a document's rows by its anchor, with the
// statement main ran for each. recordedMarksQuery is left out: it already compared the uuid
// expression before this change, and TestRecordedMarkRefsReadsThroughTheAnchorIndexes holds it
// to its indexes.
func anchorReads() []anchorRead {
	return []anchorRead{
		{
			name:    "openAnchoredMarks asks",
			query:   openAnchoredMarksQuery(asksAnchorTarget),
			oracle:  mainOpenAnchoredAsksQuery,
			indexes: []string{asksAnchorTarget.index},
			ordered: true,
		},
		{
			name:    "openAnchoredMarks comments",
			query:   openAnchoredMarksQuery(commentsAnchorTarget),
			oracle:  mainOpenAnchoredCommentsQuery,
			indexes: []string{commentsAnchorTarget.index},
			ordered: true,
		},
		{
			name:    "rejectUnindexedTableMarks",
			query:   tableAnchorRecordsQuery,
			oracle:  mainTableAnchorRecordsQuery,
			indexes: []string{asksAnchorTarget.index, commentsAnchorTarget.index},
		},
		{
			name:    "rejectLiveTableAnchors",
			query:   activeTableAnchorsQuery,
			oracle:  mainActiveTableAnchorsQuery,
			indexes: []string{asksAnchorTarget.index, commentsAnchorTarget.index},
			ordered: true,
		},
		{
			name:    "lockTableAnchorRows asks",
			query:   lockTableAskAnchorsQuery,
			oracle:  mainLockTableAskAnchorsQuery,
			indexes: []string{asksAnchorTarget.index},
		},
		{
			name:    "lockTableAnchorRows comments",
			query:   lockTableCommentAnchorsQuery,
			oracle:  mainLockTableCommentAnchorsQuery,
			indexes: []string{commentsAnchorTarget.index},
		},
	}
}

// anchorReadParameters are the parameters every read above takes: the document, and, for the
// four that name marks, the mark ids. A read that takes one is prepared and executed with the
// first alone.
var anchorReadParameters = []string{"text", "text[]"}

// Every anchor read reaches its table through that table's anchor index, naming the index, and
// no scan of one reads a relation an index condition does not key. Each read sends the document
// as a bind parameter, so its prepared statement can settle on a generic plan, which this test
// forces: with sequential scans off a generic plan still falls back to a Seq Scan when no index
// matches the predicate's expression, or, for asks, to a scan of all of asks_anchor_artifact_id,
// whose partial predicate the read's `anchor is not null` implies. So the test fails exactly
// when a read's uuid comparison stops matching its index's expression, or when the clause that
// makes the partial index eligible is dropped.
func TestAnchorReadsSearchTheirTablesAnchorIndex(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	for number, read := range anchorReads() {
		t.Run(read.name, func(t *testing.T) {
			plan := anchorReadPlan(t, ctx, connection, fmt.Sprintf("anchor_read_%d", number), read.query)
			if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
				t.Errorf("%s reads %s, which no index condition keys:\n%s",
					read.name, strings.Join(unkeyed, ", "), plan)
			}
			searched := map[string]bool{}
			for _, scan := range explaintest.IndexScans(t, plan) {
				if scan.Cond != "" {
					searched[scan.Index] = true
				}
			}
			for _, index := range read.indexes {
				if !searched[index] {
					t.Errorf("%s searches no %s under an index condition:\n%s", read.name, index, plan)
				}
			}
		})
	}
}

// anchorReadPlan is the plan Postgres builds for statement without knowing the values it will
// be executed with, the plan a prepared statement settles on, with sequential scans off.
// EXECUTE takes no bind parameters, so the document and the mark ids are written into it as
// literals. The prepared statement outlives the transaction that explains it and is deallocated
// by name on the one connection that holds it, as refs' own plan test does it.
func anchorReadPlan(t *testing.T, ctx context.Context, connection *pgxpool.Conn, name, statement string) json.RawMessage {
	t.Helper()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range []string{
		"set local enable_seqscan = off",
		"set local plan_cache_mode = force_generic_plan",
	} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	parameters, literals := anchorReadArguments(statement)
	if _, err := tx.Exec(ctx, "prepare "+name+" ("+parameters+") as "+statement); err != nil {
		t.Fatalf("prepare the anchor read: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) execute "+name+"("+literals+")").Scan(&raw); err != nil {
		t.Fatalf("explain the anchor read: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the plan's transaction: %v", err)
	}
	if _, err := connection.Exec(ctx, "deallocate "+name); err != nil {
		t.Fatalf("deallocate the anchor read: %v", err)
	}
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode the explain answer: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain answered %d plans; want one:\n%s", len(plans), raw)
	}
	return plans[0].Plan
}

// anchorReadArguments are the parameter types a read is prepared with and the literals it is
// executed with: the document alone, or the document and a list of mark ids for a read that
// names marks. A read's own text says which it is, so no case of the list above declares it
// twice.
func anchorReadArguments(statement string) (parameters, literals string) {
	document := "'00000000-0000-4000-8000-00000000000a'"
	if !strings.Contains(statement, "$2") {
		return anchorReadParameters[0], document
	}
	return strings.Join(anchorReadParameters, ", "), document + ", '{a-mark,another-mark}'"
}

// anchorFixture is one document with every anchored row shape the reads must tell apart, and
// the mark ids and document they are read by.
type anchorFixture struct {
	database *store.Store
	document string
	markIDs  []string
}

// newAnchorFixture writes the rows as SQL rather than through this package's writers: the reads
// under test are a reading of the stored anchor, and rows written through a writer would say
// what that writer stores today. Three of them are what makes the comparison able to fail. Two
// asks and one comment carry an anchor that names the document in a spelling the server never
// writes - an upper-case uuid, and, on asks alone, a value that is no uuid at all - under a
// mark id the reads ask for, so a uuid comparison added without the text comparison beside it
// would return them and a correct one leaves them out. (A comment whose artifact_id is no uuid
// cannot be stored at all: comments_anchor_artifact casts every row, which is why migration
// 0148 guards the cast on asks.) Both uncastable rows also name an ask block on another
// document, the shape asks_anchor_artifact's coalesce (0032) never cast.
func newAnchorFixture(t *testing.T) anchorFixture {
	t.Helper()
	database := storetest.Open(t)
	document := createDocument(t, database, "# First")
	other := createIssueDocument(t, database, 2, "# Second")
	fixture := anchorFixture{
		database: database,
		document: document,
		markIDs:  []string{"mark-ask", "mark-comment", "mark-unanchored"},
	}
	anchor := func(artifactID, markID string) string {
		return fmt.Sprintf(`{"artifact_id":%q,"mark_id":%q,"version":1,"quote":"First"}`, artifactID, markID)
	}
	ctx := context.Background()
	for _, row := range []struct {
		what      string
		statement string
		args      []any
	}{
		{"the open anchored ask", `
			insert into asks (id, issue_key, author, question, anchor, state)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Open ask', $2::jsonb, 'open')
		`, []any{"00000000-0000-4000-8000-0000000000a1", anchor(document, "mark-ask")}},
		{"the answered anchored ask", `
			insert into asks (id, issue_key, author, question, anchor, state, answer)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Answered ask', $2::jsonb, 'answered',
				'{"text":"Yes."}')
		`, []any{"00000000-0000-4000-8000-0000000000a2", anchor(document, "mark-comment")}},
		{"the ask anchored on another document", `
			insert into asks (id, issue_key, author, question, anchor, state)
			values ($1, 'DOC-2', '{"kind":"user","id":"alice"}', 'Other document ask', $2::jsonb, 'open')
		`, []any{"00000000-0000-4000-8000-0000000000a3", anchor(other, "mark-ask")}},
		{"the ask whose anchor spells the document in upper case", `
			insert into asks (id, issue_key, author, question, anchor, state, block_artifact_id)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Upper-case ask', $2::jsonb, 'open', $3)
		`, []any{"00000000-0000-4000-8000-0000000000a4", anchor(strings.ToUpper(document), "mark-ask"), other}},
		{"the ask whose anchor names no document id", `
			insert into asks (id, issue_key, author, question, anchor, state, block_artifact_id)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Uncastable ask', $2::jsonb, 'open', $3)
		`, []any{"00000000-0000-4000-8000-0000000000a5", anchor("not-a-uuid", "mark-comment"), other}},
		{"the unresolved anchored comment", `
			insert into comments (id, issue_key, author, body, anchor, resolved)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Open comment', $2::jsonb, false)
		`, []any{"00000000-0000-4000-8000-0000000000c1", anchor(document, "mark-comment")}},
		{"the resolved anchored comment", `
			insert into comments (id, issue_key, author, body, anchor, resolved)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Resolved comment', $2::jsonb, true)
		`, []any{"00000000-0000-4000-8000-0000000000c2", anchor(document, "mark-ask")}},
		{"the comment whose anchor spells the document in upper case", `
			insert into comments (id, issue_key, author, body, anchor, resolved)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Upper-case comment', $2::jsonb, false)
		`, []any{"00000000-0000-4000-8000-0000000000c3", anchor(strings.ToUpper(document), "mark-comment")}},
		{"the comment with no anchor", `
			insert into comments (id, issue_key, author, body)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Unanchored comment')
		`, []any{"00000000-0000-4000-8000-0000000000c4"}},
	} {
		if _, err := database.Pool.Exec(ctx, row.statement, row.args...); err != nil {
			t.Fatalf("insert %s: %v", row.what, err)
		}
	}
	return fixture
}

// Every anchor read returns exactly the rows main's own statement returns over the same rows,
// the upper-case and uncastable anchors of newAnchorFixture among them.
func TestAnchorReadsReturnTheRowsMainsStatementsDo(t *testing.T) {
	fixture := newAnchorFixture(t)
	ctx := context.Background()
	for _, read := range anchorReads() {
		t.Run(read.name, func(t *testing.T) {
			got := anchorReadRows(t, ctx, fixture, read.query, read.ordered)
			want := anchorReadRows(t, ctx, fixture, read.oracle, read.ordered)
			if !slices.Equal(got, want) {
				t.Fatalf("%s returned\n%v\nwant main's rows\n%v", read.name, got, want)
			}
			if len(want) == 0 {
				t.Fatalf("%s and main's statement both returned nothing, so the comparison decides nothing", read.name)
			}
		})
	}
}

// anchorReadRows runs statement over the fixture and returns one string per row. Rows of a
// statement whose own order the plan does not fix are sorted, so two plans over the same rows
// are compared by the rows alone.
func anchorReadRows(t *testing.T, ctx context.Context, fixture anchorFixture, statement string, ordered bool) []string {
	t.Helper()
	arguments := []any{fixture.document}
	if strings.Contains(statement, "$2") {
		arguments = append(arguments, fixture.markIDs)
	}
	rows, err := fixture.database.Pool.Query(ctx, statement, arguments...)
	if err != nil {
		t.Fatalf("run %q: %v", statement, err)
	}
	defer rows.Close()
	read := []string{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a row of %q: %v", statement, err)
		}
		read = append(read, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %q: %v", statement, err)
	}
	if !ordered {
		slices.Sort(read)
	}
	return read
}

// The fixture holds an anchor that names the document in a spelling the server never writes.
// Both statements of every pair above leave such a row out, so it is what tells a correct added
// uuid comparison - one joined by `and` to the text comparison - from one written on its own,
// which would return it: take these rows out of the fixture and the comparison above passes
// either way.
func TestTheAnchorFixtureHoldsAnAnchorOutsideTheCanonicalForm(t *testing.T) {
	fixture := newAnchorFixture(t)
	var outside int
	if err := fixture.database.Pool.QueryRow(context.Background(), `
		select count(*) from (
			select anchor->>'artifact_id' as artifact_id from asks
			union all
			select anchor->>'artifact_id' from comments
		) anchored
		where artifact_id is not null and artifact_id <> $1 and upper(artifact_id) = upper($1)
	`, fixture.document).Scan(&outside); err != nil {
		t.Fatalf("count the anchors naming the document outside the canonical form: %v", err)
	}
	if outside < 2 {
		t.Fatalf("anchors naming the document outside the canonical form = %d, want the two upper-case ones", outside)
	}
	var uncastable int
	if err := fixture.database.Pool.QueryRow(context.Background(), `
		select count(*) from asks where `+store.CanonicalUUID(`anchor->>'artifact_id'`)+` is null and anchor is not null
	`).Scan(&uncastable); err != nil {
		t.Fatalf("count the anchors naming no document id: %v", err)
	}
	if uncastable < 1 {
		t.Fatalf("asks whose anchor names no document id = %d, want the uncastable one", uncastable)
	}
}
