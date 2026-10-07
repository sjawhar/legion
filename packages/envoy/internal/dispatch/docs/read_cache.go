package docs

import (
	"context"
	"fmt"
)

// DocumentStamp names one stored state of a document: Version is the head Load folds the stored
// updates through (PgVersioned.head: a pending prune's ceiling, else the newest update's version,
// else 0, a document with no stored update), and Row is the id of the transaction that wrote the
// update row at that version, Postgres's xmin ("" where no row stands there).
//
// The version alone does not name a state: a prune deletes the updates past its target, and the
// next append takes the number the first of them had (appendUpdateTxClass), with other content.
// The row's transaction does: every writer of doc_updates either adds a row above the head (an
// append, a rebuild's seed), deletes every row (a rebuild, Delete), deletes rows above a target and
// leaves the target's row as it was (a prune), or rewrites the row at the head with the state the
// rows it deletes made (a compaction keeping one row), which gives it a new transaction id over the
// same state. So two reads of one stamp read one state, on any task sharing the database. xmin is
// 32 bits and wraps: a false match needs a row re-appended at the same version exactly 2^32
// transactions after the one it replaced, while a reading of the old row is still held.
type DocumentStamp struct {
	Version int64
	Row     string
}

// readDocumentStamp is the room's DocumentStamp, read through q in one statement whose two lookups
// are each on a primary key.
func readDocumentStamp(ctx context.Context, q Queryer, room string) (DocumentStamp, error) {
	var stamp DocumentStamp
	if err := q.QueryRow(ctx, `
		select h.version, coalesce(u.xmin::text, '')
		from (select coalesce(
				(select ceiling from doc_checkpoints where artifact_id = $1),
				(select max(version) from doc_updates where artifact_id = $1),
				0
			) as version) h
		left join doc_updates u on u.artifact_id = $1 and u.version = h.version
	`, room).Scan(&stamp.Version, &stamp.Row); err != nil {
		return DocumentStamp{}, fmt.Errorf("read document stamp: %w", err)
	}
	return stamp, nil
}
