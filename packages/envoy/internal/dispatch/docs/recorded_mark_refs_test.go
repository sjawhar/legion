package docs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// recordedMarkRefs reads the marks the rows anchored on one document record: its anchored
// comments, suggestions and asks. A row anchored on another document, a comment with no anchor
// and an ask block (block_artifact_id set, no anchor) record none of its marks.
func TestRecordedMarkRefsListsOnlyTheDocumentsAnchoredRows(t *testing.T) {
	database := storetest.Open(t)
	documentA := createDocument(t, database, "")
	documentB := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, documentA, "The quick brown fox")
	seedServiceText(t, service, documentB, "The quick brown fox")

	insertAnchoredCommentWithID(t, service, documentA, "00000000-0000-4000-8000-0000000000a1", "quick")
	insertAnchoredAsk(t, service, documentA, "brown")
	insertAnchoredCommentWithID(t, service, documentB, "00000000-0000-4000-8000-0000000000b1", "quick")
	insertRecordedMarkRow(t, service, `
		insert into comments (id, issue_key, author, body, anchor, suggestion)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Suggestion', $2, '{"kind":"replace","content":"slow"}')
	`, "00000000-0000-4000-8000-0000000000a2", markedAnchor(t, service, documentA, MarkSuggestion, "00000000-0000-4000-8000-0000000000a2", "fox"))
	insertRecordedMarkRow(t, service, `
		insert into comments (id, issue_key, author, body)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Unanchored comment')
	`, "00000000-0000-4000-8000-0000000000c1")
	insertRecordedMarkRow(t, service, `
		insert into asks (id, issue_key, author, question, anchor)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Ask on B', $2)
	`, "00000000-0000-4000-8000-0000000000b2", markedAnchor(t, service, documentB, MarkAsk, "00000000-0000-4000-8000-0000000000b2", "brown"))
	insertRecordedMarkRow(t, service, `
		insert into asks (id, issue_key, author, question, block_id, block_artifact_id)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Ask block on A', 'ttl', $2)
	`, "00000000-0000-4000-8000-0000000000a3", documentA)

	recorded, err := service.recordedMarkRefs(context.Background(), documentA)
	if err != nil {
		t.Fatalf("recordedMarkRefs: %v", err)
	}
	want := map[pmdoc.MarkRef]struct{}{
		{Type: "dispatchAsk", ID: "00000000-0000-4000-8000-000000000001"}:     {},
		{Type: "proofComment", ID: "00000000-0000-4000-8000-0000000000a1"}:    {},
		{Type: "proofSuggestion", ID: "00000000-0000-4000-8000-0000000000a2"}: {},
	}
	if !maps.Equal(recorded, want) {
		t.Fatalf("recorded marks = %v, want %v", recorded, want)
	}
}

// asks_anchor_artifact_id (migration 0148) guards its uuid cast with a regex, so an anchor whose
// artifact_id is not a lower-case document id is left out of the index rather than failing its
// build. recordedMarkRefs compares that same guarded expression, so such a row costs the read
// neither an error nor a row: the guard answers null for it, and the text comparison beside it
// never matched its spelling either. Both rows below also name an ask block on another document,
// so asks_anchor_artifact's coalesce (0032) never cast their anchor - they are exactly the shape
// an earlier release could have stored and no index has checked.
func TestRecordedMarkRefsSkipsAnAnchorArtifactIDThatDoesNotCast(t *testing.T) {
	database := storetest.Open(t)
	documentA := createDocument(t, database, "")
	documentB := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, documentA, "The quick brown fox")

	askID := insertAnchoredAsk(t, service, documentA, "brown")
	if _, err := database.Pool.Exec(context.Background(), `update asks set block_artifact_id = $2 where id = $1`, askID, documentB); err != nil {
		t.Fatalf("name the anchored ask's block document: %v", err)
	}
	insertRecordedMarkRow(t, service, `
		insert into asks (id, issue_key, author, question, anchor, block_artifact_id)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Ask whose anchor names no document',
			'{"artifact_id":"not-a-uuid","mark_id":"uncastable"}', $2)
	`, "00000000-0000-4000-8000-0000000000a4", documentB)
	insertRecordedMarkRow(t, service, `
		insert into asks (id, issue_key, author, question, anchor, block_artifact_id)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Ask whose anchor spells A in upper case',
			jsonb_build_object('artifact_id', $3::text, 'mark_id', 'upper-case'), $2)
	`, "00000000-0000-4000-8000-0000000000a5", documentB, strings.ToUpper(documentA))

	recorded, err := service.recordedMarkRefs(context.Background(), documentA)
	if err != nil {
		t.Fatalf("recordedMarkRefs: %v", err)
	}
	want := map[pmdoc.MarkRef]struct{}{
		{Type: "dispatchAsk", ID: askID}: {},
	}
	if !maps.Equal(recorded, want) {
		t.Fatalf("recorded marks = %v, want %v", recorded, want)
	}
}

// recordedMarkRefs sends the artifact as a bind parameter, so its prepared read can run a generic
// plan, which this test forces. With sequential scans disabled the planner still falls back to a
// Seq Scan when no index matches the predicate's expression, or, for asks, to a scan of all of
// asks_anchor_artifact_id, whose partial predicate the read's anchor is not null implies; so the
// test requires the index's node to carry an Index Cond, and fails exactly when an edit to a
// target's uuid expression breaks its exact-expression match with that table's anchor index.
func TestRecordedMarkRefsReadsThroughTheAnchorIndexes(t *testing.T) {
	database := storetest.Open(t)
	for _, target := range anchorTargets {
		t.Run(target.table, func(t *testing.T) {
			ctx := context.Background()
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin explain transaction: %v", err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
				t.Fatalf("disable sequential scans: %v", err)
			}
			if _, err := tx.Exec(ctx, "set local plan_cache_mode = force_generic_plan"); err != nil {
				t.Fatalf("force generic plan mode: %v", err)
			}
			if _, err := tx.Exec(ctx, "prepare recorded_marks(text) as "+recordedMarksQuery(target)); err != nil {
				t.Fatalf("prepare recorded %s marks query: %v", target.table, err)
			}
			rows, err := tx.Query(ctx, "explain (costs off) execute recorded_marks('00000000-0000-4000-8000-00000000000a')")
			if err != nil {
				t.Fatalf("explain recorded %s marks query: %v", target.table, err)
			}
			lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("read recorded %s marks plan: %v", target.table, err)
			}
			if _, err := tx.Exec(ctx, "deallocate recorded_marks"); err != nil {
				t.Fatalf("deallocate recorded %s marks query: %v", target.table, err)
			}
			plan := strings.Join(lines, "\n")
			searched := false
			for i := 1; i < len(lines); i++ {
				if slices.Contains(strings.Fields(lines[i-1]), target.index) && strings.HasPrefix(strings.TrimSpace(lines[i]), "Index Cond: ") {
					searched = true
				}
			}
			if !searched || strings.Contains(plan, "Seq Scan") {
				t.Fatalf("recorded %s marks plan does not search %s:\n%s", target.table, target.index, plan)
			}
		})
	}
}

func insertRecordedMarkRow(t *testing.T, service *Service, statement string, args ...any) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), statement, args...); err != nil {
		t.Fatalf("insert row: %v", err)
	}
}
