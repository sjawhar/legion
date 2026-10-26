package docs

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// reindexedEdge is one edge a reference source writes, with its provenance and excerpt.
type reindexedEdge struct {
	toKind, toID   string
	createdAt      time.Time
	sourceSeq      *int64
	excerptVersion *int
	excerptBlockID *string
	excerptText    *string
}

func reindexedEdges(t *testing.T, database *store.Store, fromKind, fromID string) []reindexedEdge {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select to_kind, to_id, created_at, source_seq, excerpt_version, excerpt_block_id, excerpt_text
		from refs where from_kind = $1 and from_id = $2 order by to_kind, to_id
	`, fromKind, fromID)
	if err != nil {
		t.Fatalf("read %s %s's edges: %v", fromKind, fromID, err)
	}
	defer rows.Close()
	edges := []reindexedEdge{}
	for rows.Next() {
		var edge reindexedEdge
		if err := rows.Scan(&edge.toKind, &edge.toID, &edge.createdAt, &edge.sourceSeq, &edge.excerptVersion, &edge.excerptBlockID, &edge.excerptText); err != nil {
			t.Fatalf("scan an edge: %v", err)
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate edges: %v", err)
	}
	return edges
}

func countRows(t *testing.T, database *store.Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	return count
}

// One tick re-indexes what an earlier release left owed - a document version and a comment anchored
// to it, each naming CORE-7 bare and the document's question by `#region` - by the current rules:
// both edges of each, dated at the source's own write and carrying no source_seq and no event. The
// document's mentions carry their excerpts at its version, cut where they are written, so no
// excerpt is left owed. A source that no longer exists loses its edges, and the queue is empty
// after the tick.
func TestOneReindexTickIndexesOwedTextAtItsWriteTime(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-7', 'CORE', 7, 'Seven', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("create CORE-7: %v", err)
	}
	const markdown = "Blocked on CORE-7, see #region.\n\n:::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
	document := createDocument(t, database, markdown)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, document, markdown)
	service.ScheduleSettlement(document)
	var region string
	waitFor(t, 5*time.Second, "settlement to make the region question's ask", func() bool {
		return database.Pool.QueryRow(ctx, `select id::text from asks where block_artifact_id = $1 and block_id = 'region'`, document).Scan(&region) == nil &&
			pendingSettlements(t, service) == 0
	})

	// What an earlier release leaves: the document's latest version indexed by the grammar before
	// bare keys and `#id`s (no edge), a comment written as a row alone, and a stale edge of a
	// comment since deleted, each owed.
	versionAt := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	commentAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	const gone = "00000000-0000-0000-0000-0000000000aa"
	if _, err := database.Pool.Exec(ctx, `delete from refs where from_kind = 'artifact' and from_id = $1`, document); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set created_at = $2 where artifact_id = $1`, document, versionAt); err != nil {
		t.Fatal(err)
	}
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body, anchor, created_at)
		values ('DOC-1', '{"kind":"user","id":"alice"}', 'Waiting on CORE-7 and #region.', jsonb_build_object('artifact_id', $1::text, 'mark_id', 'm1'), $2)
		returning id::text
	`, document, commentAt).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id) values ('comment', $1, 'issue', 'CORE-7')
	`, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into refs_owed (kind, id) values ('artifact', $1), ('comment', $2) on conflict do nothing
	`, document, gone); err != nil {
		t.Fatal(err)
	}
	eventsBefore := countRows(t, database, `select count(*) from events`)

	reindexed, full, err := service.ReindexReferences(ctx)
	if err != nil || full {
		t.Fatalf("tick = (%d, full %v, %v); want a partial batch and no error", reindexed, full, err)
	}

	var version int
	if err := database.Pool.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, document).Scan(&version); err != nil {
		t.Fatal(err)
	}
	wantTargets := []string{"ask " + region, "issue CORE-7"}
	sort.Strings(wantTargets)
	for _, source := range []struct {
		kind, id string
		at       time.Time
	}{{"artifact", document, versionAt}, {"comment", comment, commentAt}} {
		edges := reindexedEdges(t, database, source.kind, source.id)
		targets := []string{}
		for _, edge := range edges {
			targets = append(targets, edge.toKind+" "+edge.toID)
			if !edge.createdAt.Equal(source.at) || edge.sourceSeq != nil {
				t.Errorf("%s's edge to %s %s created %v with source_seq %v; want %v and none", source.kind, edge.toKind, edge.toID, edge.createdAt, edge.sourceSeq, source.at)
			}
			if source.kind != "artifact" {
				continue
			}
			if edge.excerptVersion == nil || *edge.excerptVersion != version || edge.excerptBlockID == nil || *edge.excerptBlockID == "" ||
				edge.excerptText == nil || !strings.Contains(*edge.excerptText, "Blocked on CORE-7, see #region.") {
				t.Errorf("the document's mention of %s %s has excerpt (version %v, block %v, text %v); want the paragraph citing it at version %d",
					edge.toKind, edge.toID, edge.excerptVersion, edge.excerptBlockID, edge.excerptText, version)
			}
		}
		if !reflect.DeepEqual(targets, wantTargets) {
			t.Errorf("%s cites %v after the tick; want %v", source.kind, targets, wantTargets)
		}
	}
	if edges := countRows(t, database, `select count(*) from refs where from_kind = 'comment' and from_id = $1`, gone); edges != 0 {
		t.Errorf("the deleted comment keeps %d edges; want none", edges)
	}
	if owed := countRows(t, database, `select count(*) from refs_owed`); owed != 0 {
		t.Errorf("%d rows still owed after the tick; want none", owed)
	}
	if after := countRows(t, database, `select count(*) from events`); after != eventsBefore {
		t.Errorf("the tick appended %d events; want none", after-eventsBefore)
	}
}

// scopeDrainTracer counts what a tick's drain of a scope row reads: each read of a document's
// question rows, which refs.QuestionMap makes once per map it builds, and each source whose edges
// are reconciled (refs.Reindex's delete), by "kind id". beforeSettle, when set, runs once ahead of
// the statement that settles the scope row (refs.SettleScope), between the drain's listing of the
// scope's citations and its re-index of the first of them.
type scopeDrainTracer struct {
	mu           sync.Mutex
	questionMaps int
	reindexed    []string
	beforeSettle func() error
	settleErr    error
	settled      bool
}

func (c *scopeDrainTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case strings.Contains(data.SQL, "k.block_id = any($2::text[])"):
		c.questionMaps++
	case strings.Contains(data.SQL, "delete from refs\n") && strings.Contains(data.SQL, "not exists"):
		c.reindexed = append(c.reindexed, data.Args[0].(string)+" "+data.Args[1].(string))
	case strings.Contains(data.SQL, "enqueued_at = $3") && !c.settled:
		c.settled = true
		if c.beforeSettle != nil {
			c.settleErr = c.beforeSettle()
		}
	}
	return ctx
}

func (c *scopeDrainTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// tracedReindexService is a document service whose every statement passes tracer, on database.
func tracedReindexService(t *testing.T, database *store.Store, tracer pgx.QueryTracer) *Service {
	t.Helper()
	config := database.Pool.Config().Copy()
	config.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	service := New(Deps{Store: &store.Store{Pool: store.NewPool(traced)}, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

func insertReindexRow(t *testing.T, database *store.Store, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	return id
}

// questionDocument is a document on DOC-1 declaring the question block q1, and the ask
// settlement made for that block.
func questionDocument(t *testing.T, database *store.Store) (document, question string) {
	t.Helper()
	document = createDocument(t, database, ":::ask{#q1}\nWhich region goes first?\n:::\n")
	question = insertReindexRow(t, database, `
		insert into asks (issue_key, author, question, block_id, block_artifact_id)
		values ('DOC-1', '{"kind":"user","id":"alice"}', 'Which region goes first?', 'q1', $1)
		returning id::text
	`, document)
	return document, question
}

// anchoredComment is a comment on DOC-1 anchored to document, saying body.
func anchoredComment(t *testing.T, database *store.Store, document, body string) string {
	t.Helper()
	return insertReindexRow(t, database, `
		insert into comments (issue_key, author, body, anchor)
		values ('DOC-1', '{"kind":"user","id":"alice"}', $2, jsonb_build_object('artifact_id', $1::text, 'mark_id', gen_random_uuid()::text))
		returning id::text
	`, document, body)
}

// oweOnlyScope leaves the scope of document the one row owed.
func oweOnlyScope(t *testing.T, database *store.Store, document string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `delete from refs_owed`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into refs_owed (kind, id) values ('scope', $1)`, document); err != nil {
		t.Fatal(err)
	}
}

// edgeTargets is every edge the source (kind, id) writes, as "to_kind to_id".
func edgeTargets(t *testing.T, database *store.Store, kind, id string) []string {
	t.Helper()
	targets := []string{}
	for _, edge := range reindexedEdges(t, database, kind, id) {
		targets = append(targets, edge.toKind+" "+edge.toID)
	}
	return targets
}

// A scope row re-indexes each source of its document's scope that may cite a question once, every
// one resolving `#id` against the document's question map read once for them all rather than once
// per source: a comment anchored to the document, an ask anchored to it and a reply to its
// question's ask each store their `#q1` as that question's ask, and nothing is owed after the tick.
func TestAScopeRowReadsItsQuestionMapOnceForEverySourceItReindexes(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document, question := questionDocument(t, database)
	comment := anchoredComment(t, database, document, "See #q1.")
	ask := insertReindexRow(t, database, `
		insert into asks (issue_key, author, question, anchor)
		values ('DOC-1', '{"kind":"user","id":"alice"}', 'After #q1?', jsonb_build_object('artifact_id', $1::text, 'mark_id', 'm-ask'))
		returning id::text
	`, document)
	reply := insertReindexRow(t, database, `
		insert into comments (issue_key, author, body, ask_id)
		values ('DOC-1', '{"kind":"user","id":"alice"}', 'Agreed on #q1.', $1)
		returning id::text
	`, question)
	oweOnlyScope(t, database, document)
	tracer := &scopeDrainTracer{}
	service := tracedReindexService(t, database, tracer)

	reindexed, full, err := service.ReindexReferences(ctx)
	if err != nil || full || reindexed != 3 {
		t.Fatalf("tick = (%d, full %v, %v); want the scope's three sources re-indexed in a partial batch", reindexed, full, err)
	}

	if tracer.questionMaps != 1 {
		t.Errorf("the tick read a question map %d times; want once for the scope", tracer.questionMaps)
	}
	sources := []string{"ask " + ask, "comment " + comment, "comment " + reply}
	sort.Strings(sources)
	sort.Strings(tracer.reindexed)
	if !reflect.DeepEqual(tracer.reindexed, sources) {
		t.Errorf("the tick re-indexed %v; want each of %v once", tracer.reindexed, sources)
	}
	for _, source := range sources {
		kind, id, _ := strings.Cut(source, " ")
		if got := edgeTargets(t, database, kind, id); !reflect.DeepEqual(got, []string{"ask " + question}) {
			t.Errorf("%s cites %v after the tick; want q1's ask %s", source, got, question)
		}
	}
	if owed := countRows(t, database, `select count(*) from refs_owed`); owed != 0 {
		t.Errorf("%d rows still owed after the tick; want none", owed)
	}
}

// A source anchored to another document after the drain listed it among a scope's citations is
// indexed against the scope it is in when it is re-indexed, never against the listed document's
// question map: its `#q1` is the other document's q1, while a source still in the scope cites the
// scope's own.
func TestAScopeRowIndexesASourceMovedOutOfItAgainstItsNewScope(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document, question := questionDocument(t, database)
	other, otherQuestion := questionDocument(t, database)
	staying := anchoredComment(t, database, document, "See #q1.")
	moving := anchoredComment(t, database, document, "Also #q1.")
	oweOnlyScope(t, database, document)
	tracer := &scopeDrainTracer{beforeSettle: func() error {
		_, err := database.Pool.Exec(ctx, `
			update comments set anchor = jsonb_build_object('artifact_id', $2::text, 'mark_id', 'm-moved') where id = $1
		`, moving, other)
		return err
	}}
	service := tracedReindexService(t, database, tracer)

	reindexed, _, err := service.ReindexReferences(ctx)
	if err != nil || reindexed != 2 {
		t.Fatalf("tick = (%d, %v); want both comments re-indexed", reindexed, err)
	}
	if !tracer.settled || tracer.settleErr != nil {
		t.Fatalf("moving the comment before the scope settled: ran %v, %v", tracer.settled, tracer.settleErr)
	}

	if got := edgeTargets(t, database, "comment", staying); !reflect.DeepEqual(got, []string{"ask " + question}) {
		t.Errorf("the comment still in the scope cites %v; want its document's q1 %s", got, question)
	}
	if got := edgeTargets(t, database, "comment", moving); !reflect.DeepEqual(got, []string{"ask " + otherQuestion}) {
		t.Errorf("the comment moved to the other document cites %v; want that document's q1 %s, not %s", got, otherQuestion, question)
	}
}
