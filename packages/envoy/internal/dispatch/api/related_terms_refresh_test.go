package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// refreshTermStatsPass runs one refresh of the project and answers what it did: whether it
// applied the queue or counted every item again, and how many items it applied.
func refreshTermStatsPass(t *testing.T, database *store.Store, project string) searchTermRefresh {
	t.Helper()
	result, err := refreshSearchTermStats(context.Background(), database.Pool, project)
	if err != nil {
		t.Fatalf("refresh %s's search term statistics: %v", project, err)
	}
	return result
}

// storedTermCounts is what a project's rows hold: each lexeme's ndoc and the project's item
// count, which together are the df and the N the picker reads.
type storedTermCounts struct {
	ndoc  map[string]int
	items int
}

func readStoredTermCounts(t *testing.T, database *store.Store, project string) storedTermCounts {
	t.Helper()
	ctx := context.Background()
	counts := storedTermCounts{ndoc: map[string]int{}}
	rows, err := database.Pool.Query(ctx,
		`select lexeme, ndoc from search_term_stats where project_key = $1`, project)
	if err != nil {
		t.Fatalf("read %s's term statistics: %v", project, err)
	}
	defer rows.Close()
	for rows.Next() {
		var lexeme string
		var ndoc int
		if err := rows.Scan(&lexeme, &ndoc); err != nil {
			t.Fatalf("scan %s's term statistics: %v", project, err)
		}
		counts.ndoc[lexeme] = ndoc
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s's term statistics: %v", project, err)
	}
	if err := database.Pool.QueryRow(ctx,
		`select items from search_term_totals where project_key = $1`, project).Scan(&counts.items); err != nil {
		t.Fatalf("read %s's item count: %v", project, err)
	}
	return counts
}

// termCountDifference names the lexemes two readings disagree on. A project holds hundreds of
// lexemes, so printing both whole maps says nothing about which count is wrong.
func termCountDifference(applied, rebuilt map[string]int) []string {
	lexemes := map[string]bool{}
	for lexeme := range applied {
		lexemes[lexeme] = true
	}
	for lexeme := range rebuilt {
		lexemes[lexeme] = true
	}
	var out []string
	for _, lexeme := range slices.Sorted(maps.Keys(lexemes)) {
		if applied[lexeme] != rebuilt[lexeme] {
			out = append(out, fmt.Sprintf("%s applied=%d rebuilt=%d", lexeme, applied[lexeme], rebuilt[lexeme]))
		}
	}
	return out
}

// assertCountsEqualAFullRebuild holds what the project's rows hold now against what counting
// every item of the project again writes. Clearing the snapshot is how the test asks for that
// count: a project with a null snapshot is one no pass of this build has counted, which
// searchTermTotalsMode answers with a full build.
func assertCountsEqualAFullRebuild(t *testing.T, database *store.Store, project string) {
	t.Helper()
	applied := readStoredTermCounts(t, database, project)
	if _, err := database.Pool.Exec(context.Background(),
		`update search_term_totals set snapshot = null where project_key = $1`, project); err != nil {
		t.Fatalf("clear %s's snapshot: %v", project, err)
	}
	if rebuilt := refreshTermStatsPass(t, database, project); rebuilt.Mode != searchTermModeFull {
		t.Fatalf("the rebuild ran in mode %q, want %q", rebuilt.Mode, searchTermModeFull)
	}
	rebuilt := readStoredTermCounts(t, database, project)
	if applied.items != rebuilt.items {
		t.Fatalf("items = %d, want %d, the count a full rebuild writes", applied.items, rebuilt.items)
	}
	if difference := termCountDifference(applied.ndoc, rebuilt.ndoc); len(difference) > 0 {
		t.Fatalf("the applied counts differ from a full rebuild's: %v", difference)
	}
	if len(rebuilt.ndoc) == 0 {
		t.Fatal("the project holds no lexeme at all, so the comparison decided nothing")
	}
}

// termFixture is a project written straight into the tables, one item of each kind, with its
// counts built by one pass. No server runs over it: a test that measures what a pass reads, or
// that holds a pass's snapshot against the queue, needs nothing else reading or writing
// meanwhile, and the triggers see a write made in SQL exactly as they see a route's.
type termFixture struct {
	database *store.Store
	project  string
	issueKey string
	document string
}

func newTermFixture(t *testing.T, project string) termFixture {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	fixture := termFixture{database: database, project: project, issueKey: project + "-1"}
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ($1, $1)`, project); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'Astrolabe survey', '{"kind":"user","id":"alice"}', 'U')
	`, fixture.issueKey, project); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ($1, 'atlas', 'Atlas.md', 'doc', '{"kind":"user","id":"alice"}') returning id
	`, project).Scan(&fixture.document); err != nil {
		t.Fatalf("seed the document: %v", err)
	}
	for _, seed := range []struct {
		what      string
		statement string
		argument  any
	}{
		{"the document's version", `insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, '# Atlas' || chr(10) || chr(10) || 'The plateau ranges north.' || chr(10), '[]')`, fixture.document},
		{"the comment", `insert into comments (issue_key, author, body)
			values ($1, '{"kind":"user","id":"alice"}', 'The rete needs cleaning.')`, fixture.issueKey},
		{"the ask", `insert into asks (issue_key, author, question)
			values ($1, '{"kind":"user","id":"alice"}', 'Which quadrant do we keep?')`, fixture.issueKey},
		{"the message", `insert into messages (issue_key, author, body)
			values ($1, '{"kind":"user","id":"alice"}', 'The survey is booked.')`, fixture.issueKey},
	} {
		if _, err := database.Pool.Exec(ctx, seed.statement, seed.argument); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}
	// The first pass counts every item: the project's totals row carries no snapshot, so no pass
	// of this build has counted it.
	if first := refreshTermStatsPass(t, database, project); first.Mode != searchTermModeFull {
		t.Fatalf("the first pass ran in mode %q, want %q", first.Mode, searchTermModeFull)
	}
	return fixture
}

// addTermComment writes one comment straight into the table, which the trigger queues as a
// change of the issue's project.
func (f termFixture) addTermComment(t *testing.T, body string) {
	t.Helper()
	if _, err := f.database.Pool.Exec(context.Background(), `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2)
	`, f.issueKey, body); err != nil {
		t.Fatalf("write a comment: %v", err)
	}
}

// queuedChanges is how many changes wait in the queue, whichever project they belong to.
func queuedChanges(t *testing.T, database *store.Store) int {
	t.Helper()
	var queued int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from search_term_queue`).Scan(&queued); err != nil {
		t.Fatalf("count the queued changes: %v", err)
	}
	return queued
}

// A1. The counts a pass applies from the queue are the counts a full rebuild writes, after an
// add, an edit and a delete of each kind of item. Every change here is made through the route a
// caller uses, apart from the three deletes and the block an ask is attached to, which no route
// makes.
func TestRefreshAppliesEveryKindOfChangeAsAFullRebuildCountsIt(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	const project = "TERM"
	issue := createInteractionIssue(t, handler, project, "Astrolabe survey", "The astrolabe measures altitude.\n")
	document := createProjectDocument(t, handler, project, "atlas.md", "# Atlas\n\nThe plateau ranges north.\n")
	awaitTermStatsRefresh(t, srv, project)
	refreshTermStatsPass(t, database, project)

	var comment, ask, message string
	for _, change := range []struct {
		name  string
		apply func(t *testing.T)
	}{
		{"an issue title edit", func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
				map[string]any{"title": "Sextant survey"}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("edit the title: status=%d body=%s", response.Code, response.Body.String())
			}
		}},
		{"a new document version", func(t *testing.T) {
			uploadIssueDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nThe quadrant is calibrated.\n")
			uploadIssueDocument(t, handler, issue.Key, "notes.md", "# Notes\n\nThe gnomon is calibrated.\n")
		}},
		{"a comment on an issue", func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]any{"body": "The rete needs cleaning."}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("comment on the issue: status=%d body=%s", response.Code, response.Body.String())
			}
			comment = decodeBody[model.Comment](t, response).ID
		}},
		{"a comment on a document", func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments",
				map[string]any{"body": "The alidade is missing."}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("comment on the document: status=%d body=%s", response.Code, response.Body.String())
			}
		}},
		{"an ask attached to a document block", func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks",
				map[string]any{"question": "Which quadrant do we keep?"}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("ask on the issue: status=%d body=%s", response.Code, response.Body.String())
			}
			ask = decodeBody[model.Ask](t, response).ID
			// The block an ask carries is written by the document indexer, not by a route, and
			// the edit of its question beside it is what queues the ask again with the document
			// its project could come from (searchTermItemsSQL's coalesce).
			if _, err := database.Pool.Exec(context.Background(), `
				update asks set block_id = 'ask-1', block_artifact_id = $2, question = $3 where id = $1
			`, ask, issue.PrimaryArtifactID, "Which azimuth do we keep?"); err != nil {
				t.Fatalf("attach the ask to a block: %v", err)
			}
		}},
		{"a message", func(t *testing.T) {
			created := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "The survey is booked."}, "alice")
			message = created.ID
		}},
		{"the comment deleted", func(t *testing.T) {
			if _, err := database.Pool.Exec(context.Background(), `delete from comments where id = $1`, comment); err != nil {
				t.Fatalf("delete the comment: %v", err)
			}
		}},
		{"the ask deleted", func(t *testing.T) {
			if _, err := database.Pool.Exec(context.Background(), `delete from asks where id = $1`, ask); err != nil {
				t.Fatalf("delete the ask: %v", err)
			}
		}},
		{"the message deleted", func(t *testing.T) {
			if _, err := database.Pool.Exec(context.Background(), `delete from messages where id = $1`, message); err != nil {
				t.Fatalf("delete the message: %v", err)
			}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			change.apply(t)
			awaitTermStatsRefresh(t, srv, project)
			applied := refreshTermStatsPass(t, database, project)
			if applied.Mode != searchTermModeStep || applied.Changes == 0 {
				t.Fatalf("the pass ran mode=%s changes=%d, want a step that applied something", applied.Mode, applied.Changes)
			}
			assertCountsEqualAFullRebuild(t, database, project)
		})
	}
}

// A2. The old release's rebuild, run between two passes of this build, leaves the counts a full
// rebuild writes - and the plan's case beyond it: the next pass counts the project again rather
// than applying the queue on top of counts that rebuild already held. The old release updates
// items and refreshed_at and nothing else, so the row still carries this build's snapshot and a
// rule keyed on a null snapshot would read it as this build's own.
func TestRefreshRebuildsInFullAfterTheOldReleaseWroteTheRow(t *testing.T) {
	fixture := newTermFixture(t, "ROLL")
	fixture.addTermComment(t, "The gnomon casts a shadow.")
	if applied := refreshTermStatsPass(t, fixture.database, fixture.project); applied.Mode != searchTermModeStep {
		t.Fatalf("the pass ran in mode %q, want %q", applied.Mode, searchTermModeStep)
	}

	runMainSearchTermStatsRebuild(t, fixture.database, fixture.project)
	fixture.addTermComment(t, "The alidade needs a new pin.")

	next := refreshTermStatsPass(t, fixture.database, fixture.project)
	if next.Mode != searchTermModeFull {
		t.Fatalf("the pass after the old release's rebuild ran in mode %q, want %q", next.Mode, searchTermModeFull)
	}
	if queued := queuedChanges(t, fixture.database); queued != 0 {
		t.Fatalf("%d changes are still queued, want the full build to have dropped the ones it counted", queued)
	}
	assertCountsEqualAFullRebuild(t, fixture.database, fixture.project)
}

// A2. A check that finds a project it needs counts refreshes it (ensureSearchTermStats), and that
// refresh applies whatever waits in the queue, so its counts are a full rebuild's too.
func TestRefreshFromTheRelatedCheckAppliesTheQueuedChanges(t *testing.T) {
	handler, database, srv := relatedTermsServer(t)
	const project = "ENSURE"
	issue := createInteractionIssue(t, handler, project, "Astrolabe survey", "The astrolabe measures altitude.\n")
	awaitTermStatsRefresh(t, srv, project)
	refreshTermStatsPass(t, database, project)

	if _, err := database.Pool.Exec(context.Background(), `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'The gnomon casts a shadow.')
	`, issue.Key); err != nil {
		t.Fatalf("write a comment: %v", err)
	}
	select {
	case result := <-srv.ensureSearchTermStats(project):
		if result.Err != nil {
			t.Fatalf("the check's refresh failed: %v", result.Err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the check's refresh did not end")
	}
	if queued := queuedChanges(t, database); queued != 0 {
		t.Fatalf("%d changes are still queued after the check's refresh", queued)
	}
	assertCountsEqualAFullRebuild(t, database, project)
}

// A2. A writer whose transaction opens before a pass and commits after it is counted by the next
// pass and by no other: the full build cannot see its item, so it leaves that writer's queued
// change where it is, and the step pass that follows applies it once.
func TestRefreshCountsAWriterThatCommitsAfterAPass(t *testing.T) {
	fixture := newTermFixture(t, "RACE")
	ctx := context.Background()
	writer, err := fixture.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the writer's transaction: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'The vernier reads true.')
	`, fixture.issueKey); err != nil {
		t.Fatalf("write the comment: %v", err)
	}

	if _, err := fixture.database.Pool.Exec(ctx,
		`update search_term_totals set snapshot = null where project_key = $1`, fixture.project); err != nil {
		t.Fatalf("clear the snapshot: %v", err)
	}
	if rebuilt := refreshTermStatsPass(t, fixture.database, fixture.project); rebuilt.Mode != searchTermModeFull {
		t.Fatalf("the pass ran in mode %q, want %q", rebuilt.Mode, searchTermModeFull)
	}
	counted := readStoredTermCounts(t, fixture.database, fixture.project)
	if counted.ndoc["vernier"] != 0 {
		t.Fatalf("the full build counted 'vernier', which its own snapshot could not see")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the writer's transaction: %v", err)
	}

	applied := refreshTermStatsPass(t, fixture.database, fixture.project)
	if applied.Mode != searchTermModeStep || applied.Changes != 1 {
		t.Fatalf("the pass ran mode=%s changes=%d, want one step change", applied.Mode, applied.Changes)
	}
	if stored := readStoredTermCounts(t, fixture.database, fixture.project); stored.ndoc["vernier"] != 1 {
		t.Fatalf("'vernier' has df %d after the writer committed, want 1", stored.ndoc["vernier"])
	}
	assertCountsEqualAFullRebuild(t, fixture.database, fixture.project)
}

// A2. A project created since the last pass has no totals row, so the next pass counts it in
// full, and the queue holds nothing of it afterwards.
func TestRefreshCountsAProjectCreatedSinceTheLastPass(t *testing.T) {
	fixture := newTermFixture(t, "FIRST")
	ctx := context.Background()
	for _, seed := range []struct {
		what      string
		statement string
	}{
		{"the project", `insert into projects (key, name) values ('LATER', 'Later')`},
		{"its issue", `insert into issues (key, project_key, number, title, created_by, rank)
			values ('LATER-1', 'LATER', 1, 'Theodolite survey', '{"kind":"user","id":"alice"}', 'U')`},
		{"its comment", `insert into comments (issue_key, author, body)
			values ('LATER-1', '{"kind":"user","id":"alice"}', 'The theodolite is level.')`},
	} {
		if _, err := fixture.database.Pool.Exec(ctx, seed.statement); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}

	RefreshEverySearchTermStats(ctx, fixture.database.Pool)

	var items int
	if err := fixture.database.Pool.QueryRow(ctx,
		`select items from search_term_totals where project_key = 'LATER'`).Scan(&items); err != nil {
		t.Fatalf("read the new project's item count: %v", err)
	}
	if items != 2 {
		t.Fatalf("the new project holds %d items, want its issue and its comment", items)
	}
	if queued := queuedChanges(t, fixture.database); queued != 0 {
		t.Fatalf("%d changes are still queued after a pass over every project", queued)
	}
	assertCountsEqualAFullRebuild(t, fixture.database, "LATER")
}

// A change whose owner is gone names no project, so no pass can tell whose counts held that
// item. One pass over every project drops those rows rather than leaving them in the queue for
// ever, and the project's next count from its items is what puts its counts right - which is the
// recovery, and the reason no production code path deletes an issue or an artifact.
func TestAPassDropsTheQueuedChangesThatNameNoProject(t *testing.T) {
	fixture := newTermFixture(t, "GONE")
	ctx := context.Background()
	// The issue's own items first: no foreign key of theirs cascades. Every one of the four rows
	// this queues names an issue that is gone by the time a pass reads them.
	for _, deleted := range []string{
		`delete from messages where issue_key = $1`,
		`delete from asks where issue_key = $1`,
		`delete from comments where issue_key = $1`,
		`delete from issues where key = $1`,
	} {
		if _, err := fixture.database.Pool.Exec(ctx, deleted, fixture.issueKey); err != nil {
			t.Fatalf("delete the issue and what it holds: %v", err)
		}
	}
	if queued := queuedChanges(t, fixture.database); queued == 0 {
		t.Fatal("the delete queued nothing, so the test holds nothing")
	}

	RefreshEverySearchTermStats(ctx, fixture.database.Pool)

	if queued := queuedChanges(t, fixture.database); queued != 0 {
		t.Fatalf("%d changes that name no project are still queued after a pass", queued)
	}
	// The count from the project's items alone, which is what the words of the deleted issue
	// leave the stored counts until a pass takes it.
	if _, err := fixture.database.Pool.Exec(ctx,
		`update search_term_totals set snapshot = null where project_key = $1`, fixture.project); err != nil {
		t.Fatalf("clear the snapshot: %v", err)
	}
	refreshTermStatsPass(t, fixture.database, fixture.project)
	if counts := readStoredTermCounts(t, fixture.database, fixture.project); counts.ndoc["astrolab"] != 0 {
		t.Fatalf("the count from the project's items still holds the deleted issue's words: %v", counts.ndoc)
	}
}

// relatedTermDF is the df relatedTermsSQL answers for each lexeme of text with the owner
// excluded, which is the number the picker scores a term by.
func relatedTermDF(t *testing.T, database *store.Store, project, text, excludeIssue string) map[string]int {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), relatedTermsSQL, project, text, excludeIssue, nil, text)
	if err != nil {
		t.Fatalf("read the related terms of %s: %v", project, err)
	}
	defer rows.Close()
	found := map[string]int{}
	for rows.Next() {
		var lexeme, quoted, surface string
		var tf int
		var df, items *int
		if err := rows.Scan(&lexeme, &tf, &quoted, &df, &surface, &items); err != nil {
			t.Fatalf("scan the related terms of %s: %v", project, err)
		}
		if df != nil {
			found[lexeme] = *df
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the related terms of %s: %v", project, err)
	}
	return found
}

// A3. The df the picker reads, with the owner of an item written since the last full build left
// out, is the same after a pass that applied the queue as after one that counted every item.
func TestRelatedTermDFAfterAStepPassEqualsAFullRebuilds(t *testing.T) {
	fixture := newTermFixture(t, "DF")
	ctx := context.Background()
	if _, err := fixture.database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DF-2', 'DF', 2, 'Quadrant and gnomon', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("write the owner issue: %v", err)
	}
	fixture.addTermComment(t, "The gnomon casts a shadow.")

	if applied := refreshTermStatsPass(t, fixture.database, fixture.project); applied.Mode != searchTermModeStep {
		t.Fatalf("the pass ran in mode %q, want %q", applied.Mode, searchTermModeStep)
	}
	const text = "quadrant gnomon astrolabe"
	stepped := relatedTermDF(t, fixture.database, fixture.project, text, "DF-2")
	if stepped["gnomon"] == 0 {
		t.Fatalf("df after the step pass = %v, want the comment's 'gnomon' counted", stepped)
	}

	if _, err := fixture.database.Pool.Exec(ctx,
		`update search_term_totals set snapshot = null where project_key = $1`, fixture.project); err != nil {
		t.Fatalf("clear the snapshot: %v", err)
	}
	refreshTermStatsPass(t, fixture.database, fixture.project)
	rebuilt := relatedTermDF(t, fixture.database, fixture.project, text, "DF-2")
	if !maps.Equal(stepped, rebuilt) {
		t.Fatalf("df after the step pass = %v, want a full rebuild's %v", stepped, rebuilt)
	}
}

// itemRowsReadSQL sums every row the six item tables have handed out since the counters were
// reset: the rows read sequentially or fetched by an index scan of each table, and the index
// entries each of their indexes returned.
const itemRowsReadSQL = `
select coalesce((select sum(seq_tup_read + idx_tup_fetch) from pg_stat_user_tables
                  where relname in ('issues', 'artifacts', 'artifact_versions', 'comments', 'asks', 'messages')), 0)
     + coalesce((select sum(idx_tup_read) from pg_stat_user_indexes
                  where relname in ('issues', 'artifacts', 'artifact_versions', 'comments', 'asks', 'messages')), 0)`

// itemRowsRead reads that sum, after the connection it is taken on has flushed what it did:
// a backend flushes its own counters and no other's, and pg_stat_force_next_flush() makes it
// flush at the end of the transaction it runs in, so the reading that follows, in a transaction
// of its own, sees everything the connection has done.
func itemRowsRead(t *testing.T, pool *store.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `select pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush the connection's statistics: %v", err)
	}
	var read int64
	if err := pool.QueryRow(ctx, itemRowsReadSQL).Scan(&read); err != nil {
		t.Fatalf("read the item tables' counters: %v", err)
	}
	return read
}

// A3. A pass with nothing queued reads no item at all, which is what makes the refresh's work
// proportional to what changed. The pass runs on a pool of one connection, so the two readings
// are of the connection that ran it, and no server runs over this database, so nothing else
// reads an item table between them.
func TestARefreshWithNoChangeReadsNoItem(t *testing.T) {
	fixture := newTermFixture(t, "QUIET")
	config, err := pgxpool.ParseConfig(fixture.database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("read the test database's connection string: %v", err)
	}
	config.MaxConns, config.MinConns = 1, 1
	raw, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool of one connection: %v", err)
	}
	t.Cleanup(raw.Close)
	alone := store.NewPool(raw)

	// The pass that empties the queue, so that the measured one has nothing to apply.
	if _, err := refreshSearchTermStats(context.Background(), alone, fixture.project); err != nil {
		t.Fatalf("refresh before the reading: %v", err)
	}
	before := itemRowsRead(t, alone)
	measured, err := refreshSearchTermStats(context.Background(), alone, fixture.project)
	if err != nil {
		t.Fatalf("refresh with nothing queued: %v", err)
	}
	if measured.Mode != searchTermModeStep || measured.Changes != 0 {
		t.Fatalf("the pass ran mode=%s changes=%d, want a step with nothing to apply", measured.Mode, measured.Changes)
	}
	after := itemRowsRead(t, alone)
	if after != before {
		t.Fatalf("the pass read %d rows of the item tables, want none", after-before)
	}
}

// A3. The event broker advances issues.last_seq and artifacts.last_seq on every event, and
// neither column is in a trigger's column list, so an event append queues no change. The two
// statements are the broker's own (events/broker.go).
func TestAnEventAppendQueuesNoChange(t *testing.T) {
	fixture := newTermFixture(t, "EVENT")
	ctx := context.Background()
	if queued := queuedChanges(t, fixture.database); queued != 0 {
		t.Fatalf("%d changes are queued before the appends", queued)
	}
	if _, err := fixture.database.Pool.Exec(ctx, `
		update issues set last_seq = $2, updated_at = greatest(updated_at, $3) where key = $1
	`, fixture.issueKey, 7, time.Now()); err != nil {
		t.Fatalf("advance the issue's event sequence: %v", err)
	}
	if _, err := fixture.database.Pool.Exec(ctx, `
		update artifacts set last_seq = $2 where id = $1
	`, fixture.document, 7); err != nil {
		t.Fatalf("advance the document's event sequence: %v", err)
	}
	if queued := queuedChanges(t, fixture.database); queued != 0 {
		t.Fatalf("the two event appends queued %d changes, want none", queued)
	}
}

// After a step pass the totals row carries the snapshot that pass took, its refreshed_at is the
// moment of that snapshot, and every queue row left behind is one the snapshot cannot see - which
// is what makes the rows it dropped exactly the rows it applied.
func TestAStepPassLeavesOnlyChangesItsSnapshotCannotSee(t *testing.T) {
	fixture := newTermFixture(t, "SNAP")
	ctx := context.Background()
	writer, err := fixture.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the writer's transaction: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'The vernier reads true.')
	`, fixture.issueKey); err != nil {
		t.Fatalf("write the uncommitted comment: %v", err)
	}
	fixture.addTermComment(t, "The gnomon casts a shadow.")

	if applied := refreshTermStatsPass(t, fixture.database, fixture.project); applied.Changes != 1 {
		t.Fatalf("the pass applied %d changes, want the one its snapshot could see", applied.Changes)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the writer's transaction: %v", err)
	}

	var written, moved bool
	if err := fixture.database.Pool.QueryRow(ctx, `
		select snapshot is not null, refreshed_at = snapshot_at from search_term_totals where project_key = $1
	`, fixture.project).Scan(&written, &moved); err != nil {
		t.Fatalf("read the totals row: %v", err)
	}
	if !written || !moved {
		t.Fatalf("the totals row has snapshot written=%t refreshed_at = snapshot_at=%t, want both", written, moved)
	}
	var left, visible int
	if err := fixture.database.Pool.QueryRow(ctx, `
		select count(*), count(*) filter (where pg_visible_in_snapshot(q.xid, t.snapshot))
		  from search_term_queue q, search_term_totals t where t.project_key = $1
	`, fixture.project).Scan(&left, &visible); err != nil {
		t.Fatalf("read the queue against the pass's snapshot: %v", err)
	}
	if left != 1 {
		t.Fatalf("%d changes are queued, want the one the pass could not see", left)
	}
	if visible != 0 {
		t.Fatalf("%d queued changes are visible in the pass's own snapshot, want none", visible)
	}
}

// A queue deeper than one batch of writes is drained whole by one pass: a pass that applied a
// bounded batch and still moved refreshed_at would leave the counts behind for every item queued
// at or before that moment.
func TestOnePassDrainsAQueueDeeperThanOneBatch(t *testing.T) {
	fixture := newTermFixture(t, "DEEP")
	ctx := context.Background()
	if _, err := fixture.database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body)
		select $1, '{"kind":"user","id":"alice"}', 'Sighting ' || n || ' of the gnomon at azimuth ' || n || '.'
		  from generate_series(1, 150) n
	`, fixture.issueKey); err != nil {
		t.Fatalf("write the comments: %v", err)
	}

	applied := refreshTermStatsPass(t, fixture.database, fixture.project)
	if applied.Mode != searchTermModeStep || applied.Changes != 150 {
		t.Fatalf("the pass ran mode=%s changes=%d, want every one of the 150 queued comments", applied.Mode, applied.Changes)
	}
	if queued := queuedChanges(t, fixture.database); queued != 0 {
		t.Fatalf("%d changes are still queued after the pass", queued)
	}
	const text = "gnomon azimuth sighting astrolabe"
	stepped := relatedTermDF(t, fixture.database, fixture.project, text, "")
	if stepped["gnomon"] != 150 {
		t.Fatalf("df of 'gnomon' after the pass = %d, want the 150 comments", stepped["gnomon"])
	}
	if _, err := fixture.database.Pool.Exec(ctx,
		`update search_term_totals set snapshot = null where project_key = $1`, fixture.project); err != nil {
		t.Fatalf("clear the snapshot: %v", err)
	}
	refreshTermStatsPass(t, fixture.database, fixture.project)
	if rebuilt := relatedTermDF(t, fixture.database, fixture.project, text, ""); !maps.Equal(stepped, rebuilt) {
		t.Fatalf("df after the one pass = %v, want a full rebuild's %v", stepped, rebuilt)
	}
}

// A pass that applies no change writes nothing: the totals row keeps the counts, the moment and
// the snapshot of the pass that last changed it, so nothing a reader compares with refreshed_at
// moves while the project stands still.
func TestARefreshWithNoChangeLeavesTheRowAsItWas(t *testing.T) {
	fixture := newTermFixture(t, "STILL")
	fixture.addTermComment(t, "The gnomon casts a shadow.")
	if applied := refreshTermStatsPass(t, fixture.database, fixture.project); applied.Changes != 1 {
		t.Fatalf("the pass applied %d changes, want the one comment", applied.Changes)
	}
	const written = `select refreshed_at, snapshot::text, snapshot_at
from search_term_totals where project_key = $1`
	var refreshedAt, snapshotAt time.Time
	var snapshot string
	ctx := context.Background()
	if err := fixture.database.Pool.QueryRow(ctx, written, fixture.project).Scan(&refreshedAt, &snapshot, &snapshotAt); err != nil {
		t.Fatalf("read the totals row: %v", err)
	}

	if quiet := refreshTermStatsPass(t, fixture.database, fixture.project); quiet.Mode != searchTermModeStep || quiet.Changes != 0 {
		t.Fatalf("the pass ran mode=%s changes=%d, want a step with nothing to apply", quiet.Mode, quiet.Changes)
	}
	var stillRefreshedAt, stillSnapshotAt time.Time
	var stillSnapshot string
	if err := fixture.database.Pool.QueryRow(ctx, written, fixture.project).Scan(&stillRefreshedAt, &stillSnapshot, &stillSnapshotAt); err != nil {
		t.Fatalf("read the totals row again: %v", err)
	}
	if !stillRefreshedAt.Equal(refreshedAt) || !stillSnapshotAt.Equal(snapshotAt) || stillSnapshot != snapshot {
		t.Fatalf("the pass wrote refreshed_at=%s snapshot=%s snapshot_at=%s over %s, %s and %s",
			stillRefreshedAt, stillSnapshot, stillSnapshotAt, refreshedAt, snapshot, snapshotAt)
	}
}

// searchTermItemsSQL answers exactly what main's text answers over the same rows. Main's text is
// the oracle held in the test (related_terms_oracle_test.go), not a statement built from this
// package's own constants, so an edit to the document arm moves one side and not the other.
func TestSearchTermItemsAnswerWhatMainsStatementDoes(t *testing.T) {
	fixture := newTermFixture(t, "ITEMS")
	ctx := context.Background()
	// A second version of the document, so that a statement reading every version rather than
	// the latest would answer one row more, and a second project the project filter must leave
	// out.
	if _, err := fixture.database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 2, '# Atlas' || chr(10) || chr(10) || 'The plateau ranges south.' || chr(10), '[]')
	`, fixture.document); err != nil {
		t.Fatalf("write the second version: %v", err)
	}
	for _, seed := range []string{
		`insert into projects (key, name) values ('OTHER', 'Other')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
			values ('OTHER-1', 'OTHER', 1, 'Astrolabe survey elsewhere', '{"kind":"user","id":"alice"}', 'U')`,
	} {
		if _, err := fixture.database.Pool.Exec(ctx, seed); err != nil {
			t.Fatalf("seed the second project: %v", err)
		}
	}

	got := termItemVectors(t, fixture.database, searchTermItemsSQL, fixture.project)
	want := termItemVectors(t, fixture.database, mainSearchTermItemsSQL, fixture.project)
	if !slices.Equal(got, want) {
		t.Fatalf("the items statement answered\n%v\nwant main's rows\n%v", got, want)
	}
	if len(want) != 5 {
		t.Fatalf("main's statement answered %d rows, want the issue, the document, the comment, the ask and the message", len(want))
	}
}

// termItemVectors is every vector the items statement answers for the project, sorted, as text:
// the statement declares no order, so its rows are compared as a set.
func termItemVectors(t *testing.T, database *store.Store, statement, project string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select coalesce(search::text, '<null>') from (`+strings.ReplaceAll(statement, "{project}", "$1::text")+`) items`, project)
	if err != nil {
		t.Fatalf("read the project's item vectors: %v", err)
	}
	defer rows.Close()
	var vectors []string
	for rows.Next() {
		var vector string
		if err := rows.Scan(&vector); err != nil {
			t.Fatalf("scan a vector: %v", err)
		}
		vectors = append(vectors, vector)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the project's item vectors: %v", err)
	}
	slices.Sort(vectors)
	return vectors
}
