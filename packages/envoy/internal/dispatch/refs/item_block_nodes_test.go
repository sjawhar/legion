package refs

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const itemBlockServer = "https://dispatch.example"

// insertIndexedItem inserts a comment or an issue message of issueKey with body and indexes it
// as a write of this build does (ReplaceCountedItem), returning its id.
func insertIndexedItem(t *testing.T, tx pgx.Tx, kind ItemKind, issueKey, body string) string {
	t.Helper()
	ctx := context.Background()
	table := "comments"
	if kind == ItemMessage {
		table = "messages"
	}
	var id string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `insert into `+table+` (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text, created_at`, issueKey, body).Scan(&id, &createdAt); err != nil {
		t.Fatalf("insert %s: %v", kind, err)
	}
	if _, err := ReplaceCountedItem(ctx, tx, kind, id, body, itemBlockServer, createdAt, nil); err != nil {
		t.Fatalf("index %s %s: %v", kind, id, err)
	}
	return id
}

// asAnOlderBuild makes this build's graph reads those of a build that predates item blocks for the
// rest of the test: its loaders lack comment_block and message_block.
func asAnOlderBuild(t *testing.T) {
	loaders, kinds := nodeLoaders, nodeKinds
	older := maps.Clone(loaders)
	delete(older, ItemComment.BlockKind())
	delete(older, ItemMessage.BlockKind())
	nodeLoaders, nodeKinds = older, slices.Sorted(maps.Keys(older))
	t.Cleanup(func() { nodeLoaders, nodeKinds = loaders, kinds })
}

// edgeSummary is one edge as <direction> <kind> <node kind>:<node id>[@<block id>][^<parent kind>:<parent id>].
func edgeSummary(edge model.GraphEdge) string {
	summary := edge.Direction + " " + edge.Kind + " " + edge.Node.Kind + ":" + edge.Node.ID
	if edge.Block != nil {
		summary += "@" + edge.Block.BlockID
	}
	if edge.Node.Kind == "block" && edge.Node.Parent != nil {
		summary += "^" + edge.Node.Parent.Kind + ":" + edge.Node.Parent.ID
	}
	return summary
}

func edgeSummaries(t *testing.T, tx pgx.Tx, query Query) []string {
	t.Helper()
	edges, err := Edges(context.Background(), tx, query)
	if err != nil {
		t.Fatalf("read %s edges of %s %s: %v", query.Direction, query.Kind, query.ID, err)
	}
	summaries := make([]string, len(edges))
	for i, edge := range edges {
		summaries[i] = edgeSummary(edge)
	}
	slices.Sort(summaries)
	return summaries
}

// A build that predates item blocks knows neither comment_block nor message_block: during the
// rolling deploy, and after a rollback, it lists and counts each item's own mention once and
// skips every item-block row, rather than doubling a count or failing a read. This build lists
// each block's edge in place of its item's.
func TestAnOlderBuildSkipsItemBlockRows(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	if _, err := tx.Exec(ctx, `insert into issues (key, project_key, number, title, created_by, rank) values ('CORE-2', 'CORE', 2, 'Cited', '{"kind":"user","id":"alice"}', 'V')`); err != nil {
		t.Fatalf("create CORE-2: %v", err)
	}
	comment := insertIndexedItem(t, tx, ItemComment, "CORE-1", "First cites dispatch://CORE-2.\n\nSecond cites dispatch://CORE-2 again.")
	message := insertIndexedItem(t, tx, ItemMessage, "CORE-1", "Hello.\n\nSee dispatch://CORE-2.")
	commentBlock := func(block string) string { return "comment/" + comment + "#" + block }
	messageBlock := "message/" + message + "#b2"

	incoming := Query{Direction: "in", Kind: "issue", ID: "CORE-2", Kinds: []string{"mentions"}}
	if got, want := edgeSummaries(t, tx, incoming), []string{
		"in mentions block:" + commentBlock("b1") + "^comment:" + comment,
		"in mentions block:" + commentBlock("b2") + "^comment:" + comment,
		"in mentions block:" + messageBlock + "^message:" + message,
	}; !slices.Equal(got, want) {
		t.Errorf("this build's CORE-2 backlinks = %v; want %v", got, want)
	}
	if got := edgeSummaries(t, tx, Query{Direction: "out", Kind: "comment", ID: comment}); !slices.Equal(got, []string{"out mentions issue:CORE-2@b1", "out mentions issue:CORE-2@b2"}) {
		t.Errorf("this build's comment links = %v; want the mention from b1 and from b2", got)
	}
	counts, err := BacklinkCounts(ctx, tx, "issue", []string{"CORE-2"}, nil)
	if err != nil {
		t.Fatalf("count CORE-2 backlinks: %v", err)
	}
	if want := map[string]int{"CORE-2": 3}; !reflect.DeepEqual(counts, want) {
		t.Errorf("this build's CORE-2 backlink count = %v; want %v", counts, want)
	}

	asAnOlderBuild(t)
	if got, want := edgeSummaries(t, tx, incoming), []string{
		"in mentions comment:" + comment,
		"in mentions message:" + message,
	}; !slices.Equal(got, want) {
		t.Errorf("an older build's CORE-2 backlinks = %v; want %v", got, want)
	}
	if got := edgeSummaries(t, tx, Query{Direction: "out", Kind: "comment", ID: comment}); !slices.Equal(got, []string{"out mentions issue:CORE-2"}) {
		t.Errorf("an older build's comment links = %v; want the comment's own mention", got)
	}
	if got := edgeSummaries(t, tx, Query{Direction: "in", Kind: "comment", ID: comment}); len(got) != 0 {
		t.Errorf("an older build's comment backlinks = %v; want none (its blocks' part_of edges skipped)", got)
	}
	counts, err = BacklinkCounts(ctx, tx, "issue", []string{"CORE-2"}, nil)
	if err != nil {
		t.Fatalf("count CORE-2 backlinks as an older build: %v", err)
	}
	if want := map[string]int{"CORE-2": 2}; !reflect.DeepEqual(counts, want) {
		t.Errorf("an older build's CORE-2 backlink count = %v; want %v", counts, want)
	}
}

// A block of a comment reaches its item: a document a comment's paragraph cites joins the
// issue's closure through the comment, and the document's referenced-by lists the comment once,
// not the block holding the link. A document an item block's reference names reaches nothing:
// the item is no document.
func TestAnItemBlocksLinkReachesItsItem(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	_, handbook := createReferenceArtifact(t, tx, "", "CORE", "handbook-md")
	comment := insertIndexedItem(t, tx, ItemComment, "CORE-1", "Intro.\n\nSee dispatch://CORE/artifact/handbook-md.")
	insertIndexedItem(t, tx, ItemComment, "CORE-1", "Cites dispatch://CORE-1/comment/"+comment+"#b-b2.")

	members, truncated, err := Closure(ctx, tx, "CORE-1")
	if err != nil {
		t.Fatalf("read closure: %v", err)
	}
	if want := []Member{{RefKey: handbook, Depth: 1, Via: model.ReferenceVia{Kind: "comment", ID: comment}}}; truncated || !reflect.DeepEqual(members, want) {
		t.Errorf("closure = %#v (truncated %t); want %#v", members, truncated, want)
	}
	citers, err := ReferencedBy(ctx, tx, handbook)
	if err != nil {
		t.Fatalf("read referenced-by: %v", err)
	}
	if len(citers) != 1 || citers[0].Kind != "comment" || citers[0].ID != comment {
		t.Errorf("referenced-by = %#v; want the comment alone", citers)
	}
}
