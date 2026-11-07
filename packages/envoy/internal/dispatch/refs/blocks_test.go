package refs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// windowServer is the server a window test's references are read against; no test of this file
// writes one.
const windowServer = "https://dispatch.example"

// part is one block of a test document: its id, its node type and its text.
type part struct{ id, nodeType, text string }

// documentOf renders parts as one markdown document, a blank line between each, with the blocks
// as a renderer hands them to IndexBlocks: in document order, each with its offsets into the
// markdown.
func documentOf(parts []part) (string, []model.ArtifactBlock) {
	var markdown strings.Builder
	blocks := make([]model.ArtifactBlock, len(parts))
	for i, p := range parts {
		if i > 0 {
			markdown.WriteString("\n\n")
		}
		from := markdown.Len()
		markdown.WriteString(p.text)
		blocks[i] = model.ArtifactBlock{ID: p.id, Type: p.nodeType, From: from, To: markdown.Len()}
	}
	return markdown.String(), blocks
}

// windowRow is one embeddings window row as its owner's indexer wrote it: the blocks it covers,
// the text it embeds, the hash of that text and xmin, which any write of the row moves, so an
// unchanged xmin proves the row was not rewritten.
type windowRow struct {
	covers []string
	text   string
	hash   string
	xmin   string
}

// untouched reports whether two readings of a window row are the same row, with no write
// between them: equal values and the same xmin.
func (row windowRow) untouched(other windowRow) bool {
	return slices.Equal(row.covers, other.covers) && row.text == other.text && row.hash == other.hash && row.xmin == other.xmin
}

// windowRowsOf is every window row of owner ownerKey of kind kind, by the id suffix after the
// owner key.
func windowRowsOf(t *testing.T, tx pgx.Tx, kind, ownerKey string) map[string]windowRow {
	t.Helper()
	rows, err := tx.Query(context.Background(), `
		select id, covers, text_snapshot, content_hash, xmin::text from embeddings
		where kind = $1 and split_part(id, '#', 1) = $2
	`, kind, ownerKey)
	if err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, ownerKey, err)
	}
	defer rows.Close()
	windows := map[string]windowRow{}
	for rows.Next() {
		var id string
		var row windowRow
		if err := rows.Scan(&id, &row.covers, &row.text, &row.hash, &row.xmin); err != nil {
			t.Fatalf("scan a window of %s %s: %v", kind, ownerKey, err)
		}
		windows[strings.TrimPrefix(id, ownerKey+"#")] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the windows of %s %s: %v", kind, ownerKey, err)
	}
	return windows
}

// windowFixture opens a database, begins the transaction the test reads, rolled back when it
// ends, and creates the project and the issue its items hang on.
func windowFixture(t *testing.T) pgx.Tx {
	t.Helper()
	database := storetest.Open(t)
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the fixture transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	createReferenceProject(t, tx, "WIND")
	createReferenceIssue(t, tx, "WIND-1", "WIND")
	return tx
}

// A window's id is its owner's key and the first block it covers, its covers are the blocks
// whose text it holds in order, and its text is the context and then those blocks: the
// document's title and the heading above for a document, under the artifact id; the issue's
// title for a comment, under comment/<uuid>; the question for an ask's parts, under
// ask/<uuid>.
func TestAWindowRecordsTheBlocksItCovers(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "notes-md")
	markdown, blocks := documentOf([]part{
		{"b1", "heading", "# Windows"},
		{"b2", "paragraph", "A window holds neighbouring blocks."},
		{"b3", "heading", "# Pieces"},
		{"b4", "paragraph", "A long block becomes pieces."},
	})
	if _, err := IndexBlocks(ctx, tx, artifact, 1, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index the document: %v", err)
	}
	document := windowRowsOf(t, tx, DocumentWindowKind, artifact)
	if got := slices.Sorted(maps.Keys(document)); !slices.Equal(got, []string{"b1", "b3"}) {
		t.Fatalf("the document's windows are %v, want one for each heading section", got)
	}
	if got := document["b1"].covers; !slices.Equal(got, []string{"b1", "b2"}) {
		t.Errorf("window b1 covers %v, want the heading and the paragraph under it", got)
	}
	if want := "notes-md\n\n# Windows\n\n# Windows\n\nA window holds neighbouring blocks."; document["b1"].text != want {
		t.Errorf("window b1 embeds %q, want %q: the title and the heading above it, then its blocks", document["b1"].text, want)
	}
	if got := document["b3"].covers; !slices.Equal(got, []string{"b3", "b4"}) {
		t.Errorf("window b3 covers %v, want the second section's blocks", got)
	}

	var comment string
	if err := tx.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('WIND-1', '{"kind":"user","id":"alice"}', $1) returning id::text
	`, "The first paragraph.\n\nThe second paragraph.").Scan(&comment); err != nil {
		t.Fatalf("insert the comment: %v", err)
	}
	if _, err := IndexItem(ctx, tx, ItemComment, comment, "The first paragraph.\n\nThe second paragraph.", time.Now(), nil, nil); err != nil {
		t.Fatalf("index the comment: %v", err)
	}
	comments := windowRowsOf(t, tx, "comment_block", "comment/"+comment)
	if got := slices.Sorted(maps.Keys(comments)); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("the comment's windows are %v, want one covering both its blocks", got)
	}
	if got := comments["b1"]; !slices.Equal(got.covers, []string{"b1", "b2"}) ||
		got.text != "Reference issue\n\nThe first paragraph.\n\nThe second paragraph." {
		t.Errorf("the comment's window is %+v, want both blocks under the issue's title", got)
	}

	ask := insertAsk(t, tx, "WIND-1", "Which region?", model.AskOption{Label: "eu", Description: "for latency"})
	reader := NewReader(ctx, tx, "ask", ask.ID, windowServer)
	if _, err := IndexAskParts(ctx, tx, reader, ask, time.Now()); err != nil {
		t.Fatalf("index the ask's parts: %v", err)
	}
	parts := windowRowsOf(t, tx, "ask_part", "ask/"+ask.ID)
	if got := slices.Sorted(maps.Keys(parts)); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("the ask's windows are %v, want one covering its question and its option", got)
	}
	if got := parts["b1"]; !slices.Equal(got.covers, []string{"b1", "b2"}) ||
		got.text != "Which region?\n\nWhich region?\n\neu — for latency" {
		t.Errorf("the ask's window is %+v, want its parts under the question that gives them meaning", got)
	}
}

// An edit rewrites the windows the edited block is in and leaves every other row untouched: a
// reworded paragraph moves the hash of its own window alone, and an edited heading moves the
// hash of every window of its section, since the heading is the context they embed.
func TestOneParagraphEditReEmbedsOnlyItsWindows(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "notes-md")
	parts := []part{
		{"b1", "heading", "# Windows"},
		{"b2", "paragraph", "A window holds neighbouring blocks."},
		{"b3", "heading", "# Pieces"},
		{"b4", "paragraph", "A long block becomes pieces."},
	}
	markdown, blocks := documentOf(parts)
	if _, err := IndexBlocks(ctx, tx, artifact, 1, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index version 1: %v", err)
	}
	before := windowRowsOf(t, tx, DocumentWindowKind, artifact)

	edited := slices.Clone(parts)
	edited[1].text = "A window holds the blocks beside each other."
	markdown, blocks = documentOf(edited)
	if _, err := IndexBlocks(ctx, tx, artifact, 2, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index the edited paragraph: %v", err)
	}
	after := windowRowsOf(t, tx, DocumentWindowKind, artifact)
	if before["b1"].hash == after["b1"].hash {
		t.Errorf("window b1 still embeds %q after its paragraph was reworded", after["b1"].text)
	}
	if !before["b3"].untouched(after["b3"]) {
		t.Errorf("window b3 was rewritten (%+v, was %+v); an edit under another heading must not touch it", after["b3"], before["b3"])
	}

	edited = slices.Clone(edited)
	edited[0].text = "# What a window is"
	markdown, blocks = documentOf(edited)
	if _, err := IndexBlocks(ctx, tx, artifact, 3, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index the edited heading: %v", err)
	}
	headed := windowRowsOf(t, tx, DocumentWindowKind, artifact)
	if headed["b1"].hash == after["b1"].hash {
		t.Errorf("window b1 still embeds %q after its heading was edited", headed["b1"].text)
	}
	if !strings.HasPrefix(headed["b1"].text, "notes-md\n\n# What a window is\n\n") {
		t.Errorf("window b1 embeds %q, want the edited heading as its context", headed["b1"].text)
	}
	if !headed["b3"].untouched(after["b3"]) {
		t.Errorf("window b3 was rewritten (%+v, was %+v); an edited heading moves its own section's windows and no other", headed["b3"], after["b3"])
	}
}

// A version that no longer holds a block leaves no window of it: the rows the new set does not
// name are deleted, so a deleted section's window goes with it.
func TestADeletedBlockTakesItsWindowRow(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	artifact, _ := createReferenceArtifact(t, tx, "WIND-1", "WIND", "notes-md")
	parts := []part{
		{"b1", "heading", "# Windows"},
		{"b2", "paragraph", "A window holds neighbouring blocks."},
		{"b3", "heading", "# Pieces"},
		{"b4", "paragraph", "A long block becomes pieces."},
	}
	markdown, blocks := documentOf(parts)
	if _, err := IndexBlocks(ctx, tx, artifact, 1, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index version 1: %v", err)
	}
	markdown, blocks = documentOf(parts[:2])
	if _, err := IndexBlocks(ctx, tx, artifact, 2, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index the version without the second section: %v", err)
	}
	if got := slices.Sorted(maps.Keys(windowRowsOf(t, tx, DocumentWindowKind, artifact))); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("the document's windows are %v, want the first section's alone", got)
	}
	markdown, blocks = documentOf(nil)
	if _, err := IndexBlocks(ctx, tx, artifact, 3, time.Now(), markdown, blocks, nil, nil); err != nil {
		t.Fatalf("index the empty version: %v", err)
	}
	if got := windowRowsOf(t, tx, DocumentWindowKind, artifact); len(got) != 0 {
		t.Fatalf("the emptied document holds %d windows, want none", len(got))
	}
}

// A mention starts at the tightest block whose [From, To) holds its start: a child inside its
// parent, the parent between its children, and no block at all between top-level blocks or past
// the last one. A block ending where a mention starts does not hold it.
func TestInnermostBlocksPicksTheTightestRangeHoldingEachStart(t *testing.T) {
	blocks := []model.ArtifactBlock{{ID: "a", From: 0, To: 100}, {ID: "b", From: 10, To: 20}, {ID: "c", From: 30, To: 40}, {ID: "d", From: 120, To: 130}}
	got := innermostBlocks(blocks, []int{5, 15, 20, 25, 35, 50, 110, 125, 200})
	want := []int{0, 1, 0, 0, 2, 0, -1, 3, -1}
	if !slices.Equal(got, want) {
		t.Fatalf("innermostBlocks = %v, want %v", got, want)
	}
}

// A rendered version can repeat an id, and document_blocks holds one row per id: the first holder
// is the node. An id Postgres cannot store in a text[] (empty, holding a NUL, or not UTF-8) is
// no node, nor is one longer than text.MaxBlockIDBytes, which no key of the index can hold; those
// are counted, with the longest one's bytes. An id at the bound is a node.
func TestUniqueBlocksKeepsTheFirstHolderOfEachID(t *testing.T) {
	atBound := strings.Repeat("é", text.MaxBlockIDBytes/2)
	got, long, longest := uniqueBlocks([]model.ArtifactBlock{
		{ID: "a", From: 0, To: 10},
		{ID: "a", From: 20, To: 30},
		{ID: "", From: 40, To: 50},
		{ID: "x\x00", From: 60, To: 70},
		{ID: "\xff", From: 70, To: 75},
		{ID: atBound + "x", From: 75, To: 78},
		{ID: atBound, From: 78, To: 79},
		{ID: strings.Repeat("f", 3200), From: 79, To: 80},
		{ID: "b", From: 80, To: 90},
	})
	type node struct {
		ID       string
		From, To int
	}
	nodes := make([]node, len(got))
	for i, block := range got {
		nodes[i] = node{block.ID, block.From, block.To}
	}
	want := []node{{"a", 0, 10}, {atBound, 78, 79}, {"b", 80, 90}}
	if !slices.Equal(nodes, want) || long != 2 || longest != 3200 {
		t.Fatalf("uniqueBlocks = %#v, %d long, longest %d; want %#v, 2 long, longest 3200", nodes, long, longest, want)
	}
}
