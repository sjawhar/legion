package docs

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

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
