package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// getCommentBlocks answers GET /api/v1/comments/{id}/blocks: the comment's current blocks, read as
// GET /api/v1/comments/{id} reads the comment (itemBlocks).
func (s *server) getCommentBlocks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !requireUUIDPath(w, r, "comment") {
		return
	}
	s.writeItemBlocks(w, r, refs.ItemComment)
}

// getMessageBlocks answers GET /api/v1/messages/{id}/blocks: the message's current blocks, under
// the read rule of GET /api/v1/messages/{id} (readableMessageThread), so a session reads the blocks
// of a direct message only in a conversation it is in.
func (s *server) getMessageBlocks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.readableMessageThread(w, r); !ok {
		return
	}
	s.writeItemBlocks(w, r, refs.ItemMessage)
}

func (s *server) writeItemBlocks(w http.ResponseWriter, r *http.Request, kind refs.ItemKind) {
	answer, err := s.itemBlocks(r.Context(), kind, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// itemBlocks is comment or message id's blocks as the blocks routes answer them. A read never
// indexes: an item owed an index, or whose index was made from another body than the one it
// stores, answers Indexed false and no blocks, and the dashboard's editor then edits without
// carrying block ids. Each block's text, From and To are the stored body's split
// (itemblocks.Split), paired with the item's current block rows by position and identity hash:
// the stored body is the one the index split, so its split holds each indexed block where the row
// says. Were the parser to read the body otherwise since, the two disagree and the item reads as
// not indexed, as an owed one does, until the reconciliation indexes it again.
func (s *server) itemBlocks(ctx context.Context, kind refs.ItemKind, id string) (model.ItemBlocks, error) {
	stored, err := readItemBlockRows(ctx, s.deps.Store.Pool, kind, id)
	if err != nil {
		return model.ItemBlocks{}, err
	}
	item, found, err := refs.Node(ctx, s.deps.Store.Pool, string(kind), id, false)
	if err != nil {
		return model.ItemBlocks{}, err
	}
	if !found {
		// Deleted between the two reads.
		return model.ItemBlocks{}, pgx.ErrNoRows
	}
	answer := model.ItemBlocks{Item: item, Blocks: []model.ItemBlock{}}
	if kind == refs.ItemComment {
		revision := 0
		if stored.revision != nil {
			revision = *stored.revision
		}
		answer.Revision = &revision
	}
	if stored.owed || stored.bodySHA256 == nil || *stored.bodySHA256 != refs.BodySHA256(stored.body) {
		return answer, nil
	}
	split := itemblocks.Split(stored.body)
	if len(split) != len(stored.rows) {
		return answer, nil
	}
	blocks := make([]model.ItemBlock, len(split))
	// UTF-16 offsets counted once over the body: each block's offsets from the end of the one before.
	byteAt, unitAt := 0, 0
	units := func(byteOffset int) (int, error) {
		counted, err := itemblocks.UTF16Offset(stored.body[byteAt:], byteOffset-byteAt)
		if err != nil {
			return 0, fmt.Errorf("the offset of a block of %s %s: %w", kind, id, err)
		}
		byteAt, unitAt = byteOffset, unitAt+counted
		return unitAt, nil
	}
	for index, block := range split {
		row := stored.rows[index]
		if row.position != index || row.textSHA256 != itemblocks.Hash(block.Text) {
			return answer, nil
		}
		from, err := units(block.From)
		if err != nil {
			return model.ItemBlocks{}, err
		}
		to, err := units(block.To)
		if err != nil {
			return model.ItemBlocks{}, err
		}
		blocks[index] = model.ItemBlock{
			ID:              row.blockID,
			Type:            row.nodeType,
			Text:            block.Text,
			Ref:             refs.BlockRef(item.Ref, row.blockID),
			From:            from,
			To:              to,
			ChangedRevision: row.changedRevision,
			ChangedAt:       row.changedAt,
		}
	}
	answer.Indexed, answer.Blocks = true, blocks
	return answer, nil
}

// storedItemBlocks is an item's stored body, its block state (revision a comment's, nil for a
// message or with no state row), and its current block rows in body order.
type storedItemBlocks struct {
	body       string
	revision   *int
	owed       bool
	bodySHA256 *string
	rows       []itemBlockRow
}

// itemBlockRow is one current block row of an item; a message block has no changed revision or
// time.
type itemBlockRow struct {
	blockID, nodeType, textSHA256 string
	position                      int
	changedRevision               *int
	changedAt                     *time.Time
}

// itemBlockRowsQueries reads an item, its state and its current blocks in one statement, so one
// snapshot holds the body, the hash it was indexed from and the rows that index wrote.
var itemBlockRowsQueries = map[refs.ItemKind]string{
	refs.ItemComment: `
		select c.body, st.revision, st.owed_at is not null, st.body_sha256,
		       b.block_id, b.type, b.text_sha256, b.position, b.changed_revision, b.changed_at
		from comments c
		left join comment_block_state st on st.comment_id = c.id
		left join comment_blocks b on b.comment_id = c.id and b.position is not null
		where c.id = $1
		order by b.position`,
	refs.ItemMessage: `
		select m.body, null::integer, st.owed_at is not null, st.body_sha256,
		       b.block_id, b.type, b.text_sha256, b.position, null::integer, null::timestamptz
		from messages m
		left join message_block_state st on st.message_id = m.id
		left join message_blocks b on b.message_id = m.id and b.position is not null
		where m.id = $1
		order by b.position`,
}

// readItemBlockRows reads comment or message id's body, block state and current block rows;
// pgx.ErrNoRows when there is no such item.
func readItemBlockRows(ctx context.Context, q queryer, kind refs.ItemKind, id string) (storedItemBlocks, error) {
	rows, err := q.Query(ctx, itemBlockRowsQueries[kind], id)
	if err != nil {
		return storedItemBlocks{}, fmt.Errorf("read the blocks of %s %s: %w", kind, id, err)
	}
	defer rows.Close()
	var stored storedItemBlocks
	found := false
	for rows.Next() {
		var blockID, nodeType, textSHA256 *string
		var position *int
		var row itemBlockRow
		if err := rows.Scan(&stored.body, &stored.revision, &stored.owed, &stored.bodySHA256,
			&blockID, &nodeType, &textSHA256, &position, &row.changedRevision, &row.changedAt); err != nil {
			return storedItemBlocks{}, fmt.Errorf("scan a block of %s %s: %w", kind, id, err)
		}
		found = true
		if blockID == nil {
			// The item holds no current block: the one row the left join answers.
			continue
		}
		row.blockID, row.nodeType, row.textSHA256, row.position = *blockID, *nodeType, *textSHA256, *position
		stored.rows = append(stored.rows, row)
	}
	if err := rows.Err(); err != nil {
		return storedItemBlocks{}, fmt.Errorf("read the blocks of %s %s: %w", kind, id, err)
	}
	if !found {
		return storedItemBlocks{}, pgx.ErrNoRows
	}
	return stored, nil
}
