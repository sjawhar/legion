package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// commentBlockLayout reads a comment's blocks as one line: its current blocks in body order, each
// as <id>@<changed_revision>, then each removed block as -<id>@<changed_revision>, by id. It
// returns the comment's revision too, and fails the test while the comment is still owed an
// index: every write of this build indexes the comment it writes.
func commentBlockLayout(t *testing.T, database *store.Store, commentID string) (string, int) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, position is not null, changed_revision from comment_blocks
		where comment_id = $1 order by position nulls last, length(block_id), block_id
	`, commentID)
	if err != nil {
		t.Fatalf("read the blocks of comment %s: %v", commentID, err)
	}
	defer rows.Close()
	parts := []string{}
	for rows.Next() {
		var id string
		var live bool
		var changed int
		if err := rows.Scan(&id, &live, &changed); err != nil {
			t.Fatalf("scan a block of comment %s: %v", commentID, err)
		}
		if !live {
			id = "-" + id
		}
		parts = append(parts, fmt.Sprintf("%s@%d", id, changed))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of comment %s: %v", commentID, err)
	}
	var revision int
	var owed bool
	var sum *string
	if err := database.Pool.QueryRow(context.Background(), `
		select revision, owed_at is not null, body_sha256 from comment_block_state where comment_id = $1
	`, commentID).Scan(&revision, &owed, &sum); err != nil {
		t.Fatalf("read the block state of comment %s: %v", commentID, err)
	}
	if owed || sum == nil {
		t.Fatalf("comment %s is owed a block index after its write (owed=%v, body_sha256=%v)", commentID, owed, sum)
	}
	return strings.Join(parts, " "), revision
}

// requireCommentBlocks fails unless the comment's blocks read as layout at revision.
func requireCommentBlocks(t *testing.T, database *store.Store, commentID, step, layout string, revision int) {
	t.Helper()
	gotLayout, gotRevision := commentBlockLayout(t, database, commentID)
	if gotLayout != layout || gotRevision != revision {
		t.Fatalf("%s: blocks = %q at revision %d, want %q at revision %d", step, gotLayout, gotRevision, layout, revision)
	}
}

// commentBlockText is the text comment_block_texts holds for a block at revision (the latest row
// at or below it), and whether any row is there; a removed block's row holds no text.
func commentBlockText(t *testing.T, database *store.Store, commentID, blockID string, revision int) (*string, bool) {
	t.Helper()
	var text *string
	err := database.Pool.QueryRow(context.Background(), `
		select text from comment_block_texts where comment_id = $1 and block_id = $2 and revision <= $3
		order by revision desc limit 1
	`, commentID, blockID, revision).Scan(&text)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false
		}
		t.Fatalf("read the text of %s of comment %s at revision %d: %v", blockID, commentID, revision, err)
	}
	return text, true
}

// requireCommentBlockText fails unless the block's text at revision is want (nil: removed).
func requireCommentBlockText(t *testing.T, database *store.Store, commentID, blockID string, revision int, want *string) {
	t.Helper()
	got, found := commentBlockText(t, database, commentID, blockID, revision)
	if !found || (got == nil) != (want == nil) || (got != nil && *got != *want) {
		t.Fatalf("text of %s at revision %d = %q (found %v, nil %v), want %q (nil %v)", blockID, revision, deref(got), found, got == nil, deref(want), want == nil)
	}
}

// editCommentAs sends PATCH /api/v1/comments/{id} as alice with body and, when given, the
// carried blocks and base revision.
func editCommentAs(t *testing.T, handler http.Handler, commentID, body string, baseRevision *int, blocks []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	input := map[string]any{"body": body}
	if baseRevision != nil {
		input["base_revision"] = *baseRevision
	}
	if blocks != nil {
		input["blocks"] = blocks
	}
	return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+commentID, input, "alice")
}

// requireEdit fails unless the edit answered 200.
func requireEdit(t *testing.T, step string, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", step, response.Code, response.Body.String())
	}
}

// carriedBlocks is a blocks list of PATCH: alternating ids and UTF-16 offsets.
func carriedBlocks(pairs ...any) []map[string]any {
	blocks := []map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		blocks = append(blocks, map[string]any{"id": pairs[i], "from": pairs[i+1]})
	}
	return blocks
}

// An edit with no carried ids keeps each block's id by its text: a reworded block between two
// unchanged ones keeps its id at a new revision, a deleted one is removed, and the same text
// written back takes the id back.
func TestACommentsEditKeepsItsBlockIDs(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Comment blocks", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "First.\n\nSecond.\n\nThird."}, "alice")
	requireCommentBlocks(t, database, comment.ID, "create", "b1@1 b2@1 b3@1", 1)
	var madeAtCreate int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_blocks b join comments c on c.id = b.comment_id
		where b.comment_id = $1 and b.made_at = c.created_at and b.changed_at = c.created_at
		  and b.node_id = 'comment/' || c.id::text || '#' || b.block_id and b.type = 'paragraph'
	`, comment.ID).Scan(&madeAtCreate); err != nil || madeAtCreate != 3 {
		t.Fatalf("blocks made and changed at the comment's created_at = %d (%v), want 3", madeAtCreate, err)
	}
	requireCommentBlockText(t, database, comment.ID, "b2", 1, new("Second."))

	requireEdit(t, "reword", editCommentAs(t, handler, comment.ID, "First.\n\nSecond, reworded.\n\nThird.", nil, nil))
	requireCommentBlocks(t, database, comment.ID, "reword", "b1@1 b2@2 b3@1", 2)
	requireCommentBlockText(t, database, comment.ID, "b2", 2, new("Second, reworded."))
	requireCommentBlockText(t, database, comment.ID, "b2", 1, new("Second."))
	var changedAtEdit bool
	if err := database.Pool.QueryRow(context.Background(), `
		select b.changed_at = c.edited_at and b.made_at = c.created_at
		from comment_blocks b join comments c on c.id = b.comment_id
		where b.comment_id = $1 and b.block_id = 'b2'
	`, comment.ID).Scan(&changedAtEdit); err != nil || !changedAtEdit {
		t.Fatalf("b2 changed at the edit's edited_at and still made at created_at = %v (%v)", changedAtEdit, err)
	}

	requireEdit(t, "delete", editCommentAs(t, handler, comment.ID, "First.\n\nThird.", nil, nil))
	requireCommentBlocks(t, database, comment.ID, "delete", "b1@1 b3@1 -b2@2", 3)
	requireCommentBlockText(t, database, comment.ID, "b2", 3, nil)

	requireEdit(t, "restore", editCommentAs(t, handler, comment.ID, "First.\n\nSecond, reworded.\n\nThird.", nil, nil))
	requireCommentBlocks(t, database, comment.ID, "restore", "b1@1 b2@4 b3@1", 4)
	requireCommentBlockText(t, database, comment.ID, "b2", 4, new("Second, reworded."))

	// The same body again moves no block and records no revision.
	requireEdit(t, "same body", editCommentAs(t, handler, comment.ID, "First.\n\nSecond, reworded.\n\nThird.", nil, nil))
	requireCommentBlocks(t, database, comment.ID, "same body", "b1@1 b2@4 b3@1", 4)
}

// A comment edited before blocks existed has its blocks indexed for the first time by the
// reconciliation (refs.ReindexOwedItem): nothing recorded when those blocks were made, so each
// has no making time, while a block a later edit adds is made at that edit.
func TestAnOlderEditedCommentsFirstItemBlockIndexHasNoMakingTime(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Older comment", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Old one.\n\nOld two."}, "alice")
	// What an older build leaves: an edited comment with a seeded, owed state row and no blocks.
	for _, statement := range []string{
		`delete from comment_blocks where comment_id = $1`,
		`delete from comment_block_texts where comment_id = $1`,
		`update comment_block_state set revision = 0, next_block = 1, body_sha256 = null, owed_at = now() where comment_id = $1`,
		`update comments set edited_at = now() where id = $1`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement, comment.ID); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := refs.ReindexOwedItem(context.Background(), database.Pool, refs.OwedItem{Kind: "comment", ID: comment.ID, EnqueuedAt: time.Now()}, "https://dispatch.example"); err != nil {
		t.Fatalf("reindex the comment: %v", err)
	}
	requireCommentBlocks(t, database, comment.ID, "first indexing", "b1@1 b2@1", 1)
	var unmade int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_blocks where comment_id = $1 and made_at is null
	`, comment.ID).Scan(&unmade); err != nil || unmade != 2 {
		t.Fatalf("blocks with no making time = %d (%v), want 2", unmade, err)
	}

	requireEdit(t, "add a block", editCommentAs(t, handler, comment.ID, "Old one.\n\nOld two.\n\nNew three.", nil, nil))
	requireCommentBlocks(t, database, comment.ID, "add a block", "b1@1 b2@1 b3@2", 2)
	var madeAtEdit bool
	if err := database.Pool.QueryRow(context.Background(), `
		select b.made_at = c.edited_at from comment_blocks b join comments c on c.id = b.comment_id
		where b.comment_id = $1 and b.block_id = 'b3'
	`, comment.ID).Scan(&madeAtEdit); err != nil || !madeAtEdit {
		t.Fatalf("b3 made at the edit = %v (%v)", madeAtEdit, err)
	}
}

// A13 over the API: the editor carries each block's id through a move, a reword, a split, a
// deletion and a restore, and the cited block b2 keeps its id through each, where matching by text
// alone would lose it. A carried list made against a revision that is no longer the comment's is
// ignored, and the ids follow the text.
func TestTheEditorsCarriedIDsSurviveMoveRewordAndSplit(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Carried ids", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta.\n\nGamma."}, "alice")
	requireCommentBlocks(t, database, comment.ID, "create", "b1@1 b2@1 b3@1", 1)

	// Move: b2 first. Its text is unchanged, so its changed revision stays.
	requireEdit(t, "move", editCommentAs(t, handler, comment.ID, "Beta.\n\nAlpha.\n\nGamma.", new(1), carriedBlocks("b2", 0, "b1", 7, "b3", 15)))
	requireCommentBlocks(t, database, comment.ID, "move", "b2@1 b1@1 b3@1", 2)

	// Reword and move at once: text matching would mint a new id for the reworded paragraph at the
	// end, which stands after every block that kept its text; the carried id keeps b2.
	requireEdit(t, "reword", editCommentAs(t, handler, comment.ID, "Alpha.\n\nGamma.\n\nBeta, said otherwise.", new(2), carriedBlocks("b1", 0, "b3", 8, "b2", 16)))
	requireCommentBlocks(t, database, comment.ID, "reword", "b1@1 b3@1 b2@3", 3)
	requireCommentBlockText(t, database, comment.ID, "b2", 3, new("Beta, said otherwise."))

	// Split: the part holding b2's start keeps it; the other part is a new block.
	requireEdit(t, "split", editCommentAs(t, handler, comment.ID, "Alpha.\n\nGamma.\n\nBeta,\n\nsaid otherwise.", new(3), carriedBlocks("b1", 0, "b3", 8, "b2", 16)))
	requireCommentBlocks(t, database, comment.ID, "split", "b1@1 b3@1 b2@4 b4@4", 4)
	requireCommentBlockText(t, database, comment.ID, "b2", 4, new("Beta,"))

	// Delete b2.
	requireEdit(t, "delete", editCommentAs(t, handler, comment.ID, "Alpha.\n\nGamma.\n\nsaid otherwise.", new(4), carriedBlocks("b1", 0, "b3", 8, "b4", 16)))
	requireCommentBlocks(t, database, comment.ID, "delete", "b1@1 b3@1 b4@4 -b2@4", 5)
	requireCommentBlockText(t, database, comment.ID, "b2", 5, nil)

	// Restore: an uncarried block whose text equals a removed block's last text takes its id back.
	requireEdit(t, "restore", editCommentAs(t, handler, comment.ID, "Alpha.\n\nGamma.\n\nBeta,\n\nsaid otherwise.", new(5), carriedBlocks("b1", 0, "b3", 8, "b4", 23)))
	requireCommentBlocks(t, database, comment.ID, "restore", "b1@1 b3@1 b2@6 b4@4", 6)
	requireCommentBlockText(t, database, comment.ID, "b2", 6, new("Beta,"))

	// A stale base revision (another tab saved first): the carried list, which would name the
	// first paragraph b4 and holds an id the comment never had, is ignored, and the ids follow the
	// text, so the reworded last paragraph keeps b4 between b2 and the end.
	requireEdit(t, "stale", editCommentAs(t, handler, comment.ID, "Alpha.\n\nGamma.\n\nBeta,\n\nsaid otherwise, again.", new(1), carriedBlocks("b4", 0, "b9", 8)))
	requireCommentBlocks(t, database, comment.ID, "stale", "b1@1 b3@1 b2@6 b4@7", 7)
}

// A body the document parser refuses, one holding block HTML, is stored exactly as sent and
// splits at blank lines into blocks of type text; adding block HTML elsewhere in a comment keeps
// an unchanged paragraph's id and changed revision.
func TestABodyWithBlockHTMLReadsBackByteForByte(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Block HTML", "before")
	body := "Intro paragraph.\r\n\r\n<div align=\"center\">\n  raw <b>html</b>\n</div>\n\n\tClosing."
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": body}, "alice")
	if read := readThreadComment(t, handler, comment.ID); read.Body != body {
		t.Fatalf("read body = %q, want %q byte for byte", read.Body, body)
	}
	requireCommentBlocks(t, database, comment.ID, "create", "b1@1 b2@1 b3@1", 1)
	var types string
	if err := database.Pool.QueryRow(context.Background(), `
		select string_agg(type, ',' order by position) from comment_blocks where comment_id = $1
	`, comment.ID).Scan(&types); err != nil || types != "text,text,text" {
		t.Fatalf("block types = %q (%v), want text,text,text", types, err)
	}
	requireCommentBlockText(t, database, comment.ID, "b2", 1, new("<div align=\"center\">\n  raw <b>html</b>\n</div>"))

	cited := createThreadComment(t, handler, issue.Key, map[string]any{"body": "First.\n\nCited paragraph.\n\nLast."}, "alice")
	requireCommentBlocks(t, database, cited.ID, "create cited", "b1@1 b2@1 b3@1", 1)
	edited := "First.\n\nCited paragraph.\n\n<details>\n<summary>More</summary>\n</details>\n\nLast."
	requireEdit(t, "add block HTML", editCommentAs(t, handler, cited.ID, edited, nil, nil))
	if read := readThreadComment(t, handler, cited.ID); read.Body != edited {
		t.Fatalf("edited body = %q, want %q byte for byte", read.Body, edited)
	}
	requireCommentBlocks(t, database, cited.ID, "add block HTML", "b1@1 b2@1 b4@2 b3@1", 2)
}

// Each wrong field of a carried list is refused with 400 INVALID_COMMENT naming it, and the
// comment is left as it was.
func TestBadCarriedIDsAreRefused(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Bad carried ids", "before")
	body := "Alpha.\n\nBeta."
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": body}, "alice")
	emoji := "\U0001F600 Alpha.\n\nBeta."
	for _, testCase := range []struct {
		name     string
		body     string
		base     *int
		blocks   []map[string]any
		contains []string
	}{
		{"unknown id", body, new(1), carriedBlocks("b9", 0), []string{"blocks[0].id", "b9", "not a block"}},
		{"id twice", body, new(1), carriedBlocks("b1", 0, "b1", 8), []string{"blocks[1].id", "twice"}},
		{"from outside the body", body, new(1), carriedBlocks("b1", 0, "b2", 99), []string{"blocks[1].from", "outside"}},
		{"from before the one ahead", body, new(1), carriedBlocks("b1", 8, "b2", 0), []string{"blocks[1].from"}},
		{"negative from", body, new(1), carriedBlocks("b1", -1), []string{"blocks[0].from"}},
		{"from inside a character", emoji, new(1), carriedBlocks("b1", 1), []string{"blocks[0].from", "inside a character"}},
		{"blocks without base_revision", body, nil, carriedBlocks("b1", 0), []string{"blocks", "base_revision"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := editCommentAs(t, handler, comment.ID, testCase.body, testCase.base, testCase.blocks)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_COMMENT"`) {
				t.Fatalf("status=%d body=%s, want 400 INVALID_COMMENT", response.Code, response.Body.String())
			}
			for _, part := range testCase.contains {
				if !strings.Contains(response.Body.String(), part) {
					t.Fatalf("body=%s, want it to name %s", response.Body.String(), part)
				}
			}
			if read := readThreadComment(t, handler, comment.ID); read.Body != body || read.EditedAt != nil {
				t.Fatalf("comment after a refused edit = %q (edited_at %v), want it unchanged", read.Body, read.EditedAt)
			}
			requireCommentBlocks(t, database, comment.ID, testCase.name, "b1@1 b2@1", 1)
		})
	}
	// A carried list naming each block where it now starts is taken.
	requireEdit(t, "valid", editCommentAs(t, handler, comment.ID, "Beta.\n\nAlpha.", new(1), carriedBlocks("b2", 0, "b1", 7)))
	requireCommentBlocks(t, database, comment.ID, "valid", "b2@1 b1@1", 2)
}

// messageBlockLayout reads a message's current blocks in body order as <id>:<type>:<excerpt>,
// failing the test while the message is still owed an index, or when a block's making and
// changing times are not the message's created_at.
func messageBlockLayout(t *testing.T, database *store.Store, messageID string) string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select b.block_id, b.type, b.excerpt, b.made_at = m.created_at and b.changed_at = m.created_at
		       and b.node_id = 'message/' || m.id::text || '#' || b.block_id
		from message_blocks b join messages m on m.id = b.message_id
		where b.message_id = $1 and b.position is not null order by b.position
	`, messageID)
	if err != nil {
		t.Fatalf("read the blocks of message %s: %v", messageID, err)
	}
	defer rows.Close()
	parts := []string{}
	for rows.Next() {
		var id, nodeType, excerpt string
		var facts bool
		if err := rows.Scan(&id, &nodeType, &excerpt, &facts); err != nil {
			t.Fatalf("scan a block of message %s: %v", messageID, err)
		}
		if !facts {
			t.Fatalf("block %s of message %s: made_at, changed_at or node_id disagree with the message", id, messageID)
		}
		parts = append(parts, id+":"+nodeType+":"+excerpt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of message %s: %v", messageID, err)
	}
	var owed bool
	var sum *string
	if err := database.Pool.QueryRow(context.Background(), `
		select owed_at is not null, body_sha256 from message_block_state where message_id = $1
	`, messageID).Scan(&owed, &sum); err != nil {
		t.Fatalf("read the block state of message %s: %v", messageID, err)
	}
	if owed || sum == nil {
		t.Fatalf("message %s is owed a block index after its write (owed=%v, body_sha256=%v)", messageID, owed, sum)
	}
	return strings.Join(parts, " | ")
}

// blockOf reports whether node is a block of the comment or message kind id: the source a
// graph read lists for a mention the item writes, in place of the item (R7).
func blockOf(node model.GraphNode, kind, id string) bool {
	return node.Kind == "block" && node.Parent != nil && node.Parent.Kind == kind && node.Parent.ID == id
}

// itemBlockRefs reads the block mentions of a comment or message as <block>-><to_kind>:<to_id>,
// failing the test when a row's from_node is not its block's node id.
func itemBlockRefs(t *testing.T, database *store.Store, kind, itemID string) []string {
	t.Helper()
	table, column := "comment_block_refs", "comment_id"
	if kind == "message" {
		table, column = "message_block_refs", "message_id"
	}
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, to_kind, to_id, from_node = $2 || '/' || `+column+`::text || '#' || block_id
		from `+table+` where `+column+` = $1 order by block_id, to_kind, to_id
	`, itemID, kind)
	if err != nil {
		t.Fatalf("read the block mentions of %s %s: %v", kind, itemID, err)
	}
	defer rows.Close()
	found := []string{}
	for rows.Next() {
		var block, toKind, toID string
		var node bool
		if err := rows.Scan(&block, &toKind, &toID, &node); err != nil {
			t.Fatalf("scan a block mention of %s %s: %v", kind, itemID, err)
		}
		if !node {
			t.Fatalf("block mention %s of %s %s has the wrong from_node", block, kind, itemID)
		}
		found = append(found, block+"->"+toKind+":"+toID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the block mentions of %s %s: %v", kind, itemID, err)
	}
	return found
}

// Every write of a message indexes its blocks in its own transaction: an issue message, a
// session's reply to a targeted one, a direct message (blocks, but no mention), the session's
// reply to it, and a session's delivery callback reply to a comment. (Each copy of a broadcast is
// covered in TestBroadcastSendsOneMessagePerRecipientAndExcludesTheRest.)
func TestEveryMessageWriteIndexesItsBlocks(t *testing.T) {
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Message blocks", "before")
	cited := "dispatch://" + issue.Key

	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "One.\n\nTwo cites " + cited + "."}, "alice")
	if got := messageBlockLayout(t, database, message.ID); got != "b1:paragraph:One. | b2:paragraph:Two cites "+cited+"." {
		t.Fatalf("issue message blocks = %q", got)
	}
	if got := itemBlockRefs(t, database, "message", message.ID); strings.Join(got, ",") != "b2->issue:"+issue.Key {
		t.Fatalf("issue message block mentions = %v, want b2 -> the issue", got)
	}

	targeted := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "target": "session:planner", "delivery": "btw",
	}, "alice")
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+targeted.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 1, "body": "Yes.\n\n- tests pass\n- docs done",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("targeted reply: status=%d body=%s", reply.Code, reply.Body.String())
	}
	replyID := decodeBody[model.Message](t, reply).ID
	if got := messageBlockLayout(t, database, replyID); got != "b1:paragraph:Yes. | b2:bullet_list:- tests pass\n- docs done" {
		t.Fatalf("targeted reply blocks = %q", got)
	}

	direct := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/planner/messages", map[string]any{
		"body": "Direct one.\n\nDirect two cites " + cited + ".", "delivery": "btw",
	}, "alice")
	if direct.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", direct.Code, direct.Body.String())
	}
	directID := decodeBody[model.Message](t, direct).ID
	if got := messageBlockLayout(t, database, directID); got != "b1:paragraph:Direct one. | b2:paragraph:Direct two cites "+cited+"." {
		t.Fatalf("direct message blocks = %q", got)
	}
	if got := itemBlockRefs(t, database, "message", directID); len(got) != 0 {
		t.Fatalf("direct message block mentions = %v, want none", got)
	}
	directReply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+directID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 1, "body": "Direct answer cites " + cited + ".",
	})
	if directReply.Code != http.StatusCreated {
		t.Fatalf("direct reply: status=%d body=%s", directReply.Code, directReply.Body.String())
	}
	directReplyID := decodeBody[model.Message](t, directReply).ID
	if got := messageBlockLayout(t, database, directReplyID); got != "b1:paragraph:Direct answer cites "+cited+"." {
		t.Fatalf("direct reply blocks = %q", got)
	}
	if got := itemBlockRefs(t, database, "message", directReplyID); len(got) != 0 {
		t.Fatalf("direct reply block mentions = %v, want none", got)
	}

	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:planner"}},
	}))
	callback := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 1, "body": "Callback one.\n\nCallback two cites " + cited + ".",
	})
	if callback.Code != http.StatusCreated {
		t.Fatalf("callback reply: status=%d body=%s", callback.Code, callback.Body.String())
	}
	callbackID := decodeBody[mentionedCommentRead](t, callback).ID
	requireCommentBlocks(t, database, callbackID, "callback reply", "b1@1 b2@1", 1)
	if got := itemBlockRefs(t, database, "comment", callbackID); strings.Join(got, ",") != "b2->issue:"+issue.Key {
		t.Fatalf("callback reply block mentions = %v, want b2 -> the issue", got)
	}
}

// R7: a mention in a comment starts at the block holding it, one row per (block, target), so a
// comment citing one target from two paragraphs holds two; the comment's own refs row stays. The
// graph reads the block's edge in place of the comment's: ?from= the comment lists the mention
// with the block holding it, ?to= the target lists the block node, whose parent is the comment,
// and referenced_by_count counts each citing paragraph. A link that moves between two blocks of a
// comment is a change the edit's event names, though the comment still cites the target.
func TestAMentionStartsAtItsBlock(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Mentions in blocks", "before")
	target := createInteractionIssue(t, handler, "ACME", "Cited issue", "before")
	cited := "dispatch://" + target.Key
	// The count before any comment cites the issue: its spec's attached_to edge.
	uncited := decodeBody[struct {
		ReferencedByCount int `json:"referenced_by_count"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+target.Key, nil, "alice")).ReferencedByCount

	once := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Intro.\n\nSee " + cited + " for context."}, "alice")
	if got := itemBlockRefs(t, database, "comment", once.ID); strings.Join(got, ",") != "b2->issue:"+target.Key {
		t.Fatalf("block mentions = %v, want b2 -> %s", got, target.Key)
	}
	var stamped int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_block_refs r
		join events e on e.id = r.source_seq
		where r.comment_id = $1 and e.type = 'comment.created'
	`, once.ID).Scan(&stamped); err != nil || stamped != 1 {
		t.Fatalf("block mentions stamped with the comment.created event = %d (%v), want 1", stamped, err)
	}

	outgoing := readGraph(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/comment/" + once.ID}})
	if len(outgoing.Edges) != 1 || outgoing.Edges[0].Kind != "mentions" || outgoing.Edges[0].Node.ID != target.Key ||
		outgoing.Edges[0].Block == nil || outgoing.Edges[0].Block.BlockID != "b2" || outgoing.Edges[0].Block.ID != "comment/"+once.ID+"#b2" {
		t.Fatalf("?from= the comment = %#v, want the mention of %s held by block b2", outgoing.Edges, target.Key)
	}
	incoming := readGraph(t, handler, url.Values{"to": {cited}, "kind": {"mentions"}})
	if len(incoming.Edges) != 1 {
		t.Fatalf("?to= the issue lists %d edges, want the block's alone: %#v", len(incoming.Edges), incoming.Edges)
	}
	// The edge's block node carries no text, only the node a reference read names does; its
	// excerpt holds the block's.
	if source := incoming.Edges[0].Node; source.Kind != "block" || source.ID != "comment/"+once.ID+"#b2" || source.BlockID != "b2" ||
		source.Parent == nil || source.Parent.Kind != "comment" || source.Parent.ID != once.ID ||
		source.Ref != "dispatch://"+issue.Key+"/comment/"+once.ID+"#b-b2" || source.Text != "" {
		t.Fatalf("?to= the issue lists the source %#v, want block b2 of comment %s", source, once.ID)
	}
	if excerpt := incoming.Edges[0].Excerpt; excerpt == nil || excerpt.BlockID != "b2" || excerpt.Text != "See "+cited+" for context." {
		t.Fatalf("?to= the issue lists the excerpt %#v, want block b2's text", excerpt)
	}
	requireIssueBacklinkCount(t, handler, target.Key, uncited+1)

	twice := createThreadComment(t, handler, issue.Key, map[string]any{"body": "First cites " + cited + ".\n\nSecond cites " + cited + " again."}, "alice")
	if got := itemBlockRefs(t, database, "comment", twice.ID); strings.Join(got, ",") != "b1->issue:"+target.Key+",b2->issue:"+target.Key {
		t.Fatalf("block mentions = %v, want b1 and b2 -> %s", got, target.Key)
	}
	var own int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs where from_kind = 'comment' and from_id = $1 and to_kind = 'issue' and to_id = $2
	`, twice.ID, target.Key).Scan(&own); err != nil || own != 1 {
		t.Fatalf("the comment's own refs rows to the issue = %d (%v), want 1", own, err)
	}
	// twice cites the issue from both its paragraphs: two edges, each from its block, and two more
	// in the count beside once's one.
	var fromTwice []string
	for _, edge := range readGraph(t, handler, url.Values{"to": {cited}, "kind": {"mentions"}}).Edges {
		if edge.Node.Parent != nil && edge.Node.Parent.ID == twice.ID {
			fromTwice = append(fromTwice, edge.Node.Kind+":"+edge.Node.BlockID)
		}
	}
	slices.Sort(fromTwice)
	if strings.Join(fromTwice, ",") != "block:b1,block:b2" {
		t.Fatalf("?to= the issue lists %v from the comment citing it twice, want blocks b1 and b2", fromTwice)
	}
	requireIssueBacklinkCount(t, handler, target.Key, uncited+3)

	// Both paragraphs are reworded, so the ids follow position: the link moves from b2 to b1.
	requireEdit(t, "move the link", editCommentAs(t, handler, once.ID, "Intro cites "+cited+".\n\nSee it for context.", nil, nil))
	if got := itemBlockRefs(t, database, "comment", once.ID); strings.Join(got, ",") != "b1->issue:"+target.Key {
		t.Fatalf("block mentions after the edit = %v, want b1 -> %s", got, target.Key)
	}
	requireReferencesChanged(t, handler, issue.Key, "comment.edited", once.ID, []model.ChangedReference{{Kind: "issue", ID: target.Key}})
}

// A comment's edit names, in one event, both kinds of target it moves: every target the comment
// still cites when the edit moves the head of its text (each edge's excerpt), and a target whose
// link moved from one block to another. An edit that moves a link past the head names that target
// alone, and one that moves only the head names every cited target though no block row moved.
func TestACommentEditNamesItsMovedHeadAndItsMovedLinks(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Edits that move links", "before")
	headTarget := createInteractionIssue(t, handler, "ACME", "Cited in the head", "before")
	movedTarget := createInteractionIssue(t, handler, "OPS", "Cited further down", "before")
	inHead, moving := "dispatch://"+headTarget.Key, "dispatch://"+movedTarget.Key
	// The first paragraph is longer than the 240-rune head an edge's excerpt shows, so the
	// paragraphs after it lie past the head.
	tail := " " + strings.Repeat("x", 260)
	headNamed := model.ChangedReference{Kind: "issue", ID: headTarget.Key}
	movedNamed := model.ChangedReference{Kind: "issue", ID: movedTarget.Key}

	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Waiting on " + inHead + "." + tail + "\n\nSecond cites " + moving + ".\n\nThird is plain.",
	}, "alice")
	links := func(step, want string) {
		t.Helper()
		if got := strings.Join(itemBlockRefs(t, database, "comment", comment.ID), ","); got != want {
			t.Fatalf("%s: block mentions = %s, want %s", step, got, want)
		}
	}
	links("created", "b1->issue:"+headTarget.Key+",b2->issue:"+movedTarget.Key)

	// The head is reworded and the link moves from b2 to b3: both targets are named.
	requireEdit(t, "reword the head and move the link", editCommentAs(t, handler, comment.ID,
		"Still waiting on "+inHead+"."+tail+"\n\nSecond is plain.\n\nThird cites "+moving+".", nil, nil))
	links("head and link moved", "b1->issue:"+headTarget.Key+",b3->issue:"+movedTarget.Key)
	requireReferencesChanged(t, handler, issue.Key, "comment.edited", comment.ID, []model.ChangedReference{movedNamed, headNamed})

	// The head stays and the link moves back from b3 to b2: only the moved link's target is named.
	requireEdit(t, "move the link alone", editCommentAs(t, handler, comment.ID,
		"Still waiting on "+inHead+"."+tail+"\n\nSecond cites "+moving+" again.\n\nThird is plain again.", nil, nil))
	links("link moved", "b1->issue:"+headTarget.Key+",b2->issue:"+movedTarget.Key)
	requireReferencesChanged(t, handler, issue.Key, "comment.edited", comment.ID, []model.ChangedReference{movedNamed})

	// The head is reworded and no block row moves: every cited target is named.
	requireEdit(t, "reword the head alone", editCommentAs(t, handler, comment.ID,
		"Now waiting on "+inHead+"."+tail+"\n\nSecond cites "+moving+" again.\n\nThird is plain again.", nil, nil))
	links("head moved", "b1->issue:"+headTarget.Key+",b2->issue:"+movedTarget.Key)
	requireReferencesChanged(t, handler, issue.Key, "comment.edited", comment.ID, []model.ChangedReference{headNamed, movedNamed})

	// A block past the head is reworded and keeps its link: the panel shows that block's excerpt,
	// so its target is named, and the head's target is not.
	requireEdit(t, "reword a block past the head", editCommentAs(t, handler, comment.ID,
		"Now waiting on "+inHead+"."+tail+"\n\nSecond still cites "+moving+".\n\nThird is plain again.", nil, nil))
	links("block reworded", "b1->issue:"+headTarget.Key+",b2->issue:"+movedTarget.Key)
	requireReferencesChanged(t, handler, issue.Key, "comment.edited", comment.ID, []model.ChangedReference{movedNamed})

	// A change past a block's 480-rune excerpt moves nothing the panel shows: nothing is named.
	long := "Second still cites " + moving + ". " + strings.Repeat("y", 500)
	requireEdit(t, "lengthen a block", editCommentAs(t, handler, comment.ID,
		"Now waiting on "+inHead+"."+tail+"\n\n"+long+" One.\n\nThird is plain again.", nil, nil))
	requireEdit(t, "edit past the block's excerpt", editCommentAs(t, handler, comment.ID,
		"Now waiting on "+inHead+"."+tail+"\n\n"+long+" Two.\n\nThird is plain again.", nil, nil))
	requireReferencesChanged(t, handler, issue.Key, "comment.edited", comment.ID, nil)
}

// A block of a comment or an issue message is a node a reference names: ?to= the block lists the
// mention of it, from the block of the comment that holds the link, and reads the block with its
// whole text and R12's facts; ?to= its item lists the same edge at the block. A block the item
// does not hold answers 404, and a message block pinned to a revision is no reference. A reply
// that names a block of the item read is listed once, under that block, and belongs to it.
func TestABlockOfACommentIsAddressable(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Comment block references", "before")
	x := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta."}, "alice")
	xRef := "dispatch://" + issue.Key + "/comment/" + x.ID
	y := createThreadComment(t, handler, issue.Key, map[string]any{"body": "See " + xRef + "#b-b2."}, "alice")

	if got := itemBlockRefs(t, database, "comment", y.ID); strings.Join(got, ",") != "b1->comment_block:comment/"+x.ID+"#b2" {
		t.Fatalf("Y's block mentions = %v, want b1 -> comment_block comment/%s#b2", got, x.ID)
	}
	var stored string
	if err := database.Pool.QueryRow(context.Background(), `
		select string_agg(to_kind || ' ' || to_id, ',') from refs where from_kind = 'comment' and from_id = $1
	`, y.ID).Scan(&stored); err != nil || stored != "comment_block comment/"+x.ID+"#b2" {
		t.Fatalf("Y's own refs = %q (%v), want the comment_block target alone", stored, err)
	}

	block := readGraph(t, handler, url.Values{"to": {xRef + "#b-b2"}})
	node := block.Node
	if node.Kind != "block" || node.ID != "comment/"+x.ID+"#b2" || node.BlockID != "b2" || node.Ref != xRef+"#b-b2" ||
		node.Text != "Beta." || node.Document != "" || node.ChangedVersion != 1 {
		t.Fatalf("?to= block b2 reads the node %#v", node)
	}
	if node.Parent == nil || *node.Parent != (model.NodeParent{Kind: "comment", ID: x.ID, Ref: xRef}) || node.ParentVersion == nil || *node.ParentVersion != 1 {
		t.Fatalf("block b2's parent = %#v at version %v, want comment %s at revision 1", node.Parent, node.ParentVersion, x.ID)
	}
	if len(node.MadeBy) != 1 || node.MadeBy[0].ID != "alice" || node.MadeAt == nil || !node.MadeAt.Equal(x.CreatedAt) ||
		len(node.ChangedBy) != 1 || node.ChangedBy[0].ID != "alice" || node.ChangedAt == nil || !node.ChangedAt.Equal(x.CreatedAt) {
		t.Fatalf("block b2's facts = made %v at %v, changed %v at %v; want alice at the comment's created_at", node.MadeBy, node.MadeAt, node.ChangedBy, node.ChangedAt)
	}
	if len(block.Edges) != 1 || block.Edges[0].Kind != "mentions" || block.Edges[0].Node.Kind != "block" ||
		block.Edges[0].Node.ID != "comment/"+y.ID+"#b1" || block.Edges[0].Node.Parent == nil || block.Edges[0].Node.Parent.ID != y.ID {
		t.Fatalf("?to= block b2 lists %#v, want the mention from block b1 of comment %s", block.Edges, y.ID)
	}

	whole := readGraph(t, handler, url.Values{"to": {xRef}})
	if len(whole.Edges) != 1 || whole.Edges[0].Kind != "mentions" || whole.Edges[0].Block == nil || whole.Edges[0].Block.BlockID != "b2" ||
		whole.Edges[0].Node.ID != "comment/"+y.ID+"#b1" {
		t.Fatalf("?to= comment X lists %#v, want the mention at its block b2", whole.Edges)
	}
	if parts := readGraph(t, handler, url.Values{"to": {xRef}, "kind": {"part_of"}}).Edges; len(parts) != 2 {
		t.Fatalf("?to= comment X with kind=part_of lists %d edges, want its two blocks", len(parts))
	}

	missing := graphRequest(t, handler, url.Values{"to": {xRef + "#b-b9"}})
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"NOT_FOUND"`) ||
		!strings.Contains(missing.Body.String(), `no block \"b9\" in comment `+x.ID) {
		t.Fatalf("?to= block b9: status=%d body=%s", missing.Code, missing.Body.String())
	}

	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "One.\n\nTwo."}, "alice")
	mRef := "dispatch://" + issue.Key + "/message/" + message.ID
	z := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Re " + mRef + "#b-b2."}, "alice")
	messageBlock := readGraph(t, handler, url.Values{"to": {mRef + "#b-b2"}})
	if node := messageBlock.Node; node.Kind != "block" || node.ID != "message/"+message.ID+"#b2" || node.Text != "Two." ||
		node.Parent == nil || node.Parent.Kind != "message" || node.Parent.ID != message.ID || node.ParentVersion != nil ||
		node.MadeAt == nil || !node.MadeAt.Equal(message.CreatedAt) {
		t.Fatalf("?to= message block b2 reads the node %#v", node)
	}
	if len(messageBlock.Edges) != 1 || messageBlock.Edges[0].Node.ID != "comment/"+z.ID+"#b1" {
		t.Fatalf("?to= message block b2 lists %#v, want the mention from comment %s", messageBlock.Edges, z.ID)
	}
	pinned := graphRequest(t, handler, url.Values{"to": {mRef + "#b-b2@v1"}})
	if pinned.Code != http.StatusBadRequest || !strings.Contains(pinned.Body.String(), `"code":"INVALID_REFERENCE"`) {
		t.Fatalf("?to= a pinned message block: status=%d body=%s", pinned.Code, pinned.Body.String())
	}

	// A reply naming a block stores its thread root in reply_to and the block's node id in
	// replies_to_block.
	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On beta.", "reply_to": x.ID, "reply_to_block": "b2",
	}, "alice").ID
	replies := readGraph(t, handler, url.Values{"to": {xRef}, "kind": {"replies_to"}}).Edges
	if len(replies) != 1 || replies[0].Node.ID != reply || replies[0].Block == nil || replies[0].Block.BlockID != "b2" {
		t.Fatalf("?to= comment X lists the replies %#v, want the reply once, under block b2", replies)
	}
	if parent := replies[0].Node.Parent; parent == nil || *parent != (model.NodeParent{Kind: "block", ID: "comment/" + x.ID + "#b2", Ref: xRef + "#b-b2"}) {
		t.Fatalf("the reply's parent = %#v, want block b2 of comment %s", parent, x.ID)
	}
	if replies := readGraph(t, handler, url.Values{"to": {xRef + "#b-b2"}, "kind": {"replies_to"}}).Edges; len(replies) != 1 || replies[0].Node.ID != reply {
		t.Fatalf("?to= block b2 lists the replies %#v, want the reply", replies)
	}
	if all := readGraph(t, handler, url.Values{"to": {xRef}}).Edges; len(all) != 2 {
		t.Fatalf("?to= comment X lists %d edges, want the mention and the reply, each once: %#v", len(all), all)
	}

	// A message reply naming a block keeps the message it answers in in_reply_to.
	messageReply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "On two.", "in_reply_to": message.ID, "in_reply_to_block": "b2",
	}, "alice").ID
	messageReplies := readGraph(t, handler, url.Values{"to": {mRef}, "kind": {"replies_to"}}).Edges
	if len(messageReplies) != 1 || messageReplies[0].Node.ID != messageReply || messageReplies[0].Block == nil || messageReplies[0].Block.BlockID != "b2" {
		t.Fatalf("?to= the message lists the replies %#v, want the reply once, under block b2", messageReplies)
	}
	if parent := messageReplies[0].Node.Parent; parent == nil || *parent != (model.NodeParent{Kind: "block", ID: "message/" + message.ID + "#b2", Ref: mRef + "#b-b2"}) {
		t.Fatalf("the message reply's parent = %#v, want block b2 of message %s", parent, message.ID)
	}
}

// A15's comment half: a link pinned to revision N of a comment's block reads the block as N
// recorded it, and says how it stands now: unchanged, changed (after an edit rewords it) or
// removed (after an edit deletes it, with the revision that removed it). The unpinned link reads
// the current text, or 404 once the block is gone; a pin past the comment's revision answers 404.
func TestAPinnedCommentBlockReadsItsRevision(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Pinned comment blocks", "before")
	x := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta."}, "alice")
	blockRef := "dispatch://" + issue.Key + "/comment/" + x.ID + "#b-b2"
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "Now " + blockRef + ", then " + blockRef + "@v1."}, "alice")

	requirePin := func(step, ref, text, state string, removedVersion int, current string) model.GraphReferences {
		t.Helper()
		read := readGraph(t, handler, url.Values{"to": {ref}})
		want := model.BlockPin{Version: 1, Ref: blockRef + "@v1", Text: text, State: state, RemovedVersion: removedVersion}
		if read.Node.Pin == nil || *read.Node.Pin != want {
			t.Fatalf("%s: the pin reads %#v, want %#v", step, read.Node.Pin, want)
		}
		if read.Node.Text != current {
			t.Fatalf("%s: the pinned node's text = %q, want %q", step, read.Node.Text, current)
		}
		return read
	}
	requireUnpinned := func(step, text string) {
		t.Helper()
		if read := readGraph(t, handler, url.Values{"to": {blockRef}}); read.Node.Pin != nil || read.Node.Text != text {
			t.Fatalf("%s: the unpinned read = %#v, want the text %q and no pin", step, read.Node, text)
		}
	}
	requireNotFound := func(step, ref, message string) {
		t.Helper()
		response := graphRequest(t, handler, url.Values{"to": {ref}})
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), message) {
			t.Fatalf("%s: status=%d body=%s, want 404 saying %q", step, response.Code, response.Body.String(), message)
		}
	}

	if read := requirePin("before an edit", blockRef+"@v1", "Beta.", "unchanged", 0, "Beta."); len(read.Edges) != 1 {
		t.Fatalf("before an edit: the pinned read lists %d edges, want the one mention (pinned and unpinned are one target)", len(read.Edges))
	}
	requireUnpinned("before an edit", "Beta.")
	requireNotFound("a pin past the revision", blockRef+"@v2", "no revision 2 of comment "+x.ID)

	requireEdit(t, "reword", editCommentAs(t, handler, x.ID, "Alpha.\n\nBeta, reworded.", nil, nil))
	requirePin("after the reword", blockRef+"@v1", "Beta.", "changed", 0, "Beta, reworded.")
	requireUnpinned("after the reword", "Beta, reworded.")

	requireEdit(t, "delete", editCommentAs(t, handler, x.ID, "Alpha.", nil, nil))
	removed := requirePin("after the deletion", blockRef+"@v1", "Beta.", "removed", 3, "")
	if len(removed.Edges) != 0 || removed.Node.ChangedVersion != 0 || removed.Node.ParentVersion == nil || *removed.Node.ParentVersion != 2 ||
		removed.Node.Parent == nil || removed.Node.Parent.ID != x.ID {
		t.Fatalf("after the deletion: the removed block reads %#v with %d edges, want no edge, part of comment %s at revision 2", removed.Node, len(removed.Edges), x.ID)
	}
	if read := readGraph(t, handler, url.Values{"to": {blockRef + "@v2"}}); read.Node.Pin == nil || read.Node.Pin.Text != "Beta, reworded." || read.Node.Pin.State != "removed" {
		t.Fatalf("after the deletion: the pin at revision 2 reads %#v, want its reworded text, removed", read.Node.Pin)
	}
	requireNotFound("the unpinned read after the deletion", blockRef, `no block \"b2\" in comment `+x.ID)
	requireNotFound("a pin to the revision that removed it", blockRef+"@v3", `no block \"b2\" in comment `+x.ID+" at revision 3")
}

// describeItemBlocks reads a blocks route's blocks as one line: each block as
// <id>:<type>:<from>-<to>:<changed revision>:<text>:<ref>, the changed revision 0 when absent.
func describeItemBlocks(blocks []model.ItemBlock) string {
	parts := make([]string, len(blocks))
	for index, block := range blocks {
		changed := 0
		if block.ChangedRevision != nil {
			changed = *block.ChangedRevision
		}
		parts[index] = fmt.Sprintf("%s:%s:%d-%d:%d:%s:%s", block.ID, block.Type, block.From, block.To, changed, block.Text, block.Ref)
	}
	return strings.Join(parts, " | ")
}

// describeRevision is a blocks route's revision for a failure message: absent, or its number.
func describeRevision(revision *int) string {
	if revision == nil {
		return "absent"
	}
	return fmt.Sprint(*revision)
}

// The blocks routes list a comment's or message's current blocks in body order: each block's id,
// type, whole text, reference, and where it stands in the stored body in UTF-16 code units (an
// emoji counts two, as a JavaScript string index does); a comment's revision and each block's
// changed revision and time, which a message has none of. An item owed an index, or indexed from
// another body than it stores, reads indexed false with no blocks, and the read leaves it owed. A
// message is read under GET /api/v1/messages/{id}'s rule: a session reads a direct message's
// blocks only in a conversation it is in, and they have no reference.
func TestTheBlocksRoutesListAnItemsBlocks(t *testing.T) {
	handler, database, direct, reply, _ := directConversationFrom(t, "alice")
	issue := createInteractionIssue(t, handler, "TEST", "Blocks routes", "before")
	read := func(step, path string, human bool) (model.ItemBlocks, string) {
		t.Helper()
		var response *httptest.ResponseRecorder
		if human {
			response = dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		} else {
			response = bearerRequest(t, handler, http.MethodGet, path, nil)
		}
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET %s: status=%d body=%s", step, path, response.Code, response.Body.String())
		}
		return decodeBody[model.ItemBlocks](t, response), response.Body.String()
	}
	requireRead := func(step string, got model.ItemBlocks, kind, id string, revision *int, indexed bool, blocks string) {
		t.Helper()
		if got.Item.Kind != kind || got.Item.ID != id {
			t.Fatalf("%s: item = %s %s, want %s %s", step, got.Item.Kind, got.Item.ID, kind, id)
		}
		if (got.Revision == nil) != (revision == nil) || (revision != nil && *got.Revision != *revision) {
			t.Fatalf("%s: revision = %s, want %s", step, describeRevision(got.Revision), describeRevision(revision))
		}
		if got.Indexed != indexed || got.Blocks == nil {
			t.Fatalf("%s: indexed = %v with blocks %v, want indexed %v and a list", step, got.Indexed, got.Blocks, indexed)
		}
		if described := describeItemBlocks(got.Blocks); described != blocks {
			t.Fatalf("%s: blocks = %q, want %q", step, described, blocks)
		}
	}

	// A comment whose first block holds an emoji: the blocks after it start two units after
	// their character count, four bytes in the body.
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha 😀.\n\nBeta.\n\n- one\n- two"}, "alice")
	commentRef := "dispatch://" + issue.Key + "/comment/" + comment.ID
	created, _ := read("a new comment", "/api/v1/comments/"+comment.ID+"/blocks", true)
	requireRead("a new comment", created, "comment", comment.ID, new(1), true,
		"b1:paragraph:0-9:1:Alpha 😀.:"+commentRef+"#b-b1 | b2:paragraph:11-16:1:Beta.:"+commentRef+"#b-b2 | "+
			"b3:bullet_list:18-29:1:- one\n- two:"+commentRef+"#b-b3")
	if created.Item.Ref != commentRef || created.Item.Parent == nil || created.Item.Parent.ID != issue.Key {
		t.Fatalf("a new comment: item = %#v, want the comment's node under its issue", created.Item)
	}
	for _, block := range created.Blocks {
		if block.ChangedAt == nil || !block.ChangedAt.Equal(comment.CreatedAt) {
			t.Fatalf("a new comment: %s changed at %v, want the comment's created_at %v", block.ID, block.ChangedAt, comment.CreatedAt)
		}
	}
	// A bearer reads a comment as GET /api/v1/comments/{id} lets it.
	bearer, _ := read("a bearer", "/api/v1/comments/"+comment.ID+"/blocks", false)
	requireRead("a bearer", bearer, "comment", comment.ID, new(1), true, describeItemBlocks(created.Blocks))

	// An edit that rewords b2 moves its changed revision and time; the others keep theirs.
	requireEdit(t, "reword", editCommentAs(t, handler, comment.ID, "Alpha 😀.\n\nBeta, reworded.\n\n- one\n- two", nil, nil))
	edited, _ := read("after an edit", "/api/v1/comments/"+comment.ID+"/blocks", true)
	requireRead("after an edit", edited, "comment", comment.ID, new(2), true,
		"b1:paragraph:0-9:1:Alpha 😀.:"+commentRef+"#b-b1 | b2:paragraph:11-26:2:Beta, reworded.:"+commentRef+"#b-b2 | "+
			"b3:bullet_list:28-39:1:- one\n- two:"+commentRef+"#b-b3")
	var editedAt time.Time
	if err := database.Pool.QueryRow(context.Background(), `select edited_at from comments where id = $1`, comment.ID).Scan(&editedAt); err != nil {
		t.Fatalf("read the comment's edited_at: %v", err)
	}
	if at := edited.Blocks[1].ChangedAt; at == nil || !at.Equal(editedAt) || !edited.Blocks[0].ChangedAt.Equal(comment.CreatedAt) {
		t.Fatalf("after an edit: b2 changed at %v and b1 at %v, want the edit's %v and the comment's %v", at, edited.Blocks[0].ChangedAt, editedAt, comment.CreatedAt)
	}

	// A suggestion stored with no rationale has a first revision and no block.
	suggestion := createThreadComment(t, handler, issue.Key, map[string]any{"body": "", "suggestion": map[string]string{"replace_with": "after"}}, "alice")
	empty, _ := read("an empty suggestion", "/api/v1/comments/"+suggestion.ID+"/blocks", true)
	requireRead("an empty suggestion", empty, "comment", suggestion.ID, new(1), true, "")

	if malformed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/7430fab/blocks", nil, "alice"); malformed.Code != http.StatusBadRequest ||
		!strings.Contains(malformed.Body.String(), `"code":"COMMENT_ID_INPUT"`) {
		t.Fatalf("a non-uuid comment id: status=%d body=%s, want 400 COMMENT_ID_INPUT", malformed.Code, malformed.Body.String())
	}
	if missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/5a660655-04ad-4ce0-8a9b-93dd03c412b7/blocks", nil, "alice"); missing.Code != http.StatusNotFound {
		t.Fatalf("a missing comment: status=%d body=%s, want 404", missing.Code, missing.Body.String())
	}

	// An item owed an index, as an older build's write leaves it, or whose index was made from
	// another body, reads as not indexed; the read writes nothing, so it stays owed.
	stillOwed := func(step, table, column, id string) {
		t.Helper()
		var owed bool
		if err := database.Pool.QueryRow(context.Background(),
			`select owed_at is not null from `+table+` where `+column+` = $1`, id).Scan(&owed); err != nil || !owed {
			t.Fatalf("%s: owed after the read = %v (%v), want still owed", step, owed, err)
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `update comment_block_state set owed_at = now() where comment_id = $1`, comment.ID); err != nil {
		t.Fatalf("owe the comment an index: %v", err)
	}
	owed, _ := read("an owed comment", "/api/v1/comments/"+comment.ID+"/blocks", true)
	requireRead("an owed comment", owed, "comment", comment.ID, new(2), false, "")
	stillOwed("an owed comment", "comment_block_state", "comment_id", comment.ID)
	if _, err := database.Pool.Exec(context.Background(),
		`update comment_block_state set owed_at = null, body_sha256 = $2 where comment_id = $1`, comment.ID, itemblocks.BodySHA256("another body")); err != nil {
		t.Fatalf("index the comment from another body: %v", err)
	}
	stale, _ := read("a comment indexed from another body", "/api/v1/comments/"+comment.ID+"/blocks", true)
	requireRead("a comment indexed from another body", stale, "comment", comment.ID, new(2), false, "")

	// An issue message: its blocks have references and no revision; nor has the message.
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "One 😀.\n\nTwo."}, "alice")
	messageRef := "dispatch://" + issue.Key + "/message/" + message.ID
	onIssue, raw := read("an issue message", "/api/v1/messages/"+message.ID+"/blocks", false)
	requireRead("an issue message", onIssue, "message", message.ID, nil, true,
		"b1:paragraph:0-7:0:One 😀.:"+messageRef+"#b-b1 | b2:paragraph:9-13:0:Two.:"+messageRef+"#b-b2")
	var fields struct {
		Revision json.RawMessage              `json:"revision"`
		Blocks   []map[string]json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("an issue message: decode %s: %v", raw, err)
	}
	if fields.Revision != nil || len(fields.Blocks) != 2 {
		t.Fatalf("an issue message: %s, want two blocks and no revision", raw)
	}
	for _, block := range fields.Blocks {
		if _, changed := block["changed_revision"]; changed || block["changed_at"] != nil {
			t.Fatalf("an issue message: %s, want no block's changed revision or time", raw)
		}
	}

	// A direct message is read as its conversation is: a human reads it, a bearer as a session
	// in it, never as another session. Its blocks have no reference.
	directBlocks := "/api/v1/messages/" + direct.ID + "/blocks"
	const directLayout = "b1:paragraph:0-23:0:Where is the dashboard?:"
	human, raw := read("a human reading a direct message", directBlocks, true)
	requireRead("a human reading a direct message", human, "message", direct.ID, nil, true, directLayout)
	if strings.Contains(raw, `"ref"`) {
		t.Fatalf("a direct message: %s, want no reference anywhere", raw)
	}
	inIt, _ := read("its session", directBlocks+"?session=s1", false)
	requireRead("its session", inIt, "message", direct.ID, nil, true, directLayout)
	if response := reply("On it."); response.Code != http.StatusCreated {
		t.Fatalf("s1's reply: status=%d body=%s", response.Code, response.Body.String())
	}
	if other := bearerRequest(t, handler, http.MethodGet, directBlocks+"?session=s2", nil); other.Code != http.StatusForbidden ||
		!strings.Contains(other.Body.String(), `"code":"THREAD_FORBIDDEN"`) || strings.Contains(other.Body.String(), "dashboard") {
		t.Fatalf("another session: status=%d body=%s, want 403 THREAD_FORBIDDEN with no text", other.Code, other.Body.String())
	}
	if unnamed := bearerRequest(t, handler, http.MethodGet, directBlocks, nil); unnamed.Code != http.StatusBadRequest ||
		!strings.Contains(unnamed.Body.String(), `"code":"SESSION_REQUIRED"`) {
		t.Fatalf("a bearer naming no session: status=%d body=%s, want 400 SESSION_REQUIRED", unnamed.Code, unnamed.Body.String())
	}
	if malformed := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/7430fab/blocks?session=s1", nil); malformed.Code != http.StatusBadRequest ||
		!strings.Contains(malformed.Body.String(), `"code":"MESSAGE_ID_INPUT"`) {
		t.Fatalf("a non-uuid message id: status=%d body=%s, want 400 MESSAGE_ID_INPUT", malformed.Code, malformed.Body.String())
	}

	if _, err := database.Pool.Exec(context.Background(), `update message_block_state set owed_at = now() where message_id = $1`, message.ID); err != nil {
		t.Fatalf("owe the message an index: %v", err)
	}
	owedMessage, _ := read("an owed message", "/api/v1/messages/"+message.ID+"/blocks", true)
	requireRead("an owed message", owedMessage, "message", message.ID, nil, false, "")
	stillOwed("an owed message", "message_block_state", "message_id", message.ID)
}

// A direct message has no address, so none of its blocks can be named: the references route
// never reads one, whatever issue key the reference spells, and so never hands a session its text
// or the message's, which GET /api/v1/messages/{id}/blocks reads only to a session in the
// conversation.
func TestADirectMessagesBlockHasNoAddress(t *testing.T) {
	handler, _, direct, _, _ := directConversationFrom(t, "alice")
	for _, key := range []string{"TEST-1", "NONE-9"} {
		reference := "dispatch://" + key + "/message/" + direct.ID + "#b-b1"
		response := bearerRequest(t, handler, http.MethodGet, "/api/v1/references?to="+url.QueryEscape(reference), nil)
		if response.Code < http.StatusBadRequest || strings.Contains(response.Body.String(), "dashboard") {
			t.Fatalf("?to=%s: status=%d body=%s, want a refusal that carries none of the direct message's text", reference, response.Code, response.Body.String())
		}
	}
}
