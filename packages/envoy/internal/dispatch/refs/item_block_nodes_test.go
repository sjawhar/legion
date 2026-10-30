package refs

import (
	"context"
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
	if _, err := ReplaceCountedItem(ctx, tx, kind, id, body, itemBlockServer, createdAt); err != nil {
		t.Fatalf("index %s %s: %v", kind, id, err)
	}
	return id
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

// previousReleaseNodeKinds is every node kind the release before item blocks resolves: its
// nodeLoaders keys, written out so a change here cannot move them.
var previousReleaseNodeKinds = []string{"artifact", "ask", "block", "comment", "component", "issue", "message", "session"}

// A build that predates item blocks knows none of comment_block, message_block and ask_block:
// during the rolling deploy, and after a rollback, it skips every item-block row as a node kind it
// does not know (resolveNodes), and lists and counts each item's own mention once, rather than
// doubling a count or failing a read. It sees exactly this because every row an item block writes
// is stored under comment_block, message_block or ask_block, never block, which it would resolve
// through document_blocks. This build lists each block's edge in place of its item's, an ask's
// question part in place of the ask's own row.
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
	citer := insertIndexedItem(t, tx, ItemComment, "CORE-1", "See dispatch://CORE-1/comment/"+comment+"#b-b2.")
	ask := insertAsk(t, tx, "CORE-1", "Ship dispatch://CORE-2 first?", model.AskOption{Label: "Yes"})
	if _, err := ReplaceCounted(ctx, tx, "ask", ask.ID, ask.Question, itemBlockServer); err != nil {
		t.Fatalf("index the ask's own references: %v", err)
	}
	indexAskPartsAt(t, tx, ask, 1)
	askBlock := "ask/" + ask.ID + "#b1"
	insertIndexedItem(t, tx, ItemComment, "CORE-1", "See dispatch://CORE-1/ask/"+ask.ID+"#b-b1.")

	incoming := Query{Direction: "in", Kind: "issue", ID: "CORE-2", Kinds: []string{"mentions"}}
	if got, want := edgeSummaries(t, tx, incoming), []string{
		"in mentions block:" + askBlock + "^ask:" + ask.ID,
		"in mentions block:" + commentBlock("b1") + "^comment:" + comment,
		"in mentions block:" + commentBlock("b2") + "^comment:" + comment,
		"in mentions block:" + messageBlock + "^message:" + message,
	}; !slices.Equal(got, want) {
		t.Errorf("this build's CORE-2 backlinks = %v; want %v", got, want)
	}
	if got := edgeSummaries(t, tx, Query{Direction: "out", Kind: "comment", ID: comment}); !slices.Equal(got, []string{"out mentions issue:CORE-2@b1", "out mentions issue:CORE-2@b2"}) {
		t.Errorf("this build's comment links = %v; want the mention from b1 and from b2", got)
	}
	if got := edgeSummaries(t, tx, Query{Direction: "out", Kind: "ask", ID: ask.ID, Kinds: []string{"mentions"}}); !slices.Equal(got, []string{"out mentions issue:CORE-2@b1"}) {
		t.Errorf("this build's ask links = %v; want the mention from its question part", got)
	}
	counts, err := BacklinkCounts(ctx, tx, "issue", []string{"CORE-2"}, nil)
	if err != nil {
		t.Fatalf("count CORE-2 backlinks: %v", err)
	}
	if want := map[string]int{"CORE-2": 4}; !reflect.DeepEqual(counts, want) {
		t.Errorf("this build's CORE-2 backlink count = %v; want %v", counts, want)
	}

	// Every end of an edge an item block wrote, and the refs row of the comment citing a block,
	// is stored under comment_block, message_block or ask_block, never block.
	var stored []string
	if err := tx.QueryRow(ctx, `
		select coalesce(array_agg(distinct kind order by kind), '{}') from (
			select from_kind as kind from graph_edges where from_id like 'comment/%' or from_id like 'message/%' or from_id like 'ask/%'
			union all select to_kind from graph_edges where to_id like 'comment/%' or to_id like 'message/%' or to_id like 'ask/%'
			union all select to_kind from refs where to_id like 'comment/%' or to_id like 'message/%' or to_id like 'ask/%'
		) ends`).Scan(&stored); err != nil {
		t.Fatalf("read the kinds item blocks are stored under: %v", err)
	}
	if want := []string{ItemAsk.BlockKind(), ItemComment.BlockKind(), ItemMessage.BlockKind()}; !slices.Equal(stored, want) {
		t.Errorf("item blocks are stored under the kinds %v; want %v", stored, want)
	}
	var cited string
	if err := tx.QueryRow(ctx, `select to_kind from refs where from_kind = 'comment' and from_id = $1`, citer).Scan(&cited); err != nil || cited != ItemComment.BlockKind() {
		t.Errorf("the citing comment's refs row is stored under %q (%v); want %q", cited, err, ItemComment.BlockKind())
	}

	// The previous release's resolveNodes, which skips a kind its nodeLoaders lacks, resolves
	// every other end of those edges and no item block; this build resolves the blocks.
	rows, err := tx.Query(ctx, `
		select from_kind, from_id from graph_edges where from_kind in ('comment', 'message', 'ask', 'comment_block', 'message_block', 'ask_block')
		union select to_kind, to_id from graph_edges where to_kind in ('issue', 'comment_block', 'message_block', 'ask_block')`)
	if err != nil {
		t.Fatalf("read the edges' ends: %v", err)
	}
	ends, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (edgeRow, error) {
		var end edgeRow
		return end, row.Scan(&end.otherKind, &end.otherID)
	})
	if err != nil {
		t.Fatalf("scan the edges' ends: %v", err)
	}
	resolved := func(nodes map[[2]string]resolvedNode) []string {
		var kinds []string
		for _, end := range ends {
			if _, found := nodes[[2]string{end.otherKind, end.otherID}]; found && !slices.Contains(kinds, end.otherKind) {
				kinds = append(kinds, end.otherKind)
			}
		}
		slices.Sort(kinds)
		return kinds
	}
	current, err := resolveNodes(ctx, tx, false, ends)
	if err != nil {
		t.Fatalf("resolve the ends: %v", err)
	}
	if got, want := resolved(current), []string{"ask", "ask_block", "comment", "comment_block", "issue", "message", "message_block"}; !slices.Equal(got, want) {
		t.Errorf("this build resolves the kinds %v; want %v", got, want)
	}
	loaders := nodeLoaders
	previous := make(map[string]nodeLoader, len(previousReleaseNodeKinds))
	for _, kind := range previousReleaseNodeKinds {
		previous[kind] = loaders[kind]
	}
	nodeLoaders = previous
	older, err := resolveNodes(ctx, tx, false, ends)
	nodeLoaders = loaders
	if err != nil {
		t.Fatalf("resolve the ends as the previous release: %v", err)
	}
	if got, want := resolved(older), []string{"ask", "comment", "issue", "message"}; !slices.Equal(got, want) {
		t.Errorf("the previous release resolves the kinds %v; want %v", got, want)
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
