package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// R7's storage: a mention in a comment starts at the block holding it, one row per (block,
// target), so a comment citing one target from two paragraphs holds two; the comment's own refs
// row stays. A link that moves between two blocks of a comment is a change the edit's event
// names, though the comment still cites the target.
//
// The graph reads of the same edges (?from= the comment listing the edge with block b2, ?to= the
// issue listing the block node with the comment as its parent, and referenced_by_count 2 for two
// paragraphs) need the item block node kinds, which come with references to item blocks.
func TestAMentionStartsAtItsBlock(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Mentions in blocks", "before")
	target := createInteractionIssue(t, handler, "ACME", "Cited issue", "before")
	cited := "dispatch://" + target.Key

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

	// Both paragraphs are reworded, so the ids follow position: the link moves from b2 to b1.
	requireEdit(t, "move the link", editCommentAs(t, handler, once.ID, "Intro cites "+cited+".\n\nSee it for context.", nil, nil))
	if got := itemBlockRefs(t, database, "comment", once.ID); strings.Join(got, ",") != "b1->issue:"+target.Key {
		t.Fatalf("block mentions after the edit = %v, want b1 -> %s", got, target.Key)
	}
	requireReferencesChanged(t, handler, issue.Key, "comment.edited", once.ID, []model.ChangedReference{{Kind: "issue", ID: target.Key}})
}
