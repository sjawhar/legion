package docs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// documentChangedChannel carries the artifact id of a document whose stored history has just
// changed, to every envoy-dispatch process. Two processes serve one Postgres (INFRA-66) and do
// not share a live document: a room resident in one of them never receives the other's updates,
// so without an announcement a reader sits on text another process replaced until the room leaves
// at roomIdleTimeout. Every write of this build sends documentHeadChannel's notice beside this, in
// the same statement, and that notice is what a process of this build decides its rooms on
// (staleRoomEvictor.headNotified): the process that holds a room the write left behind closes it,
// and the browser reconnects and re-syncs from the durable document. This channel keeps its bare
// payload for the builds before that notice, which listen on it alone, and for what a write of such
// a build has to reach on this one during a roll: a cold read cached before it
// (headTracker.invalidate).
const documentChangedChannel = "dispatch_doc_changed"

// documentHeadChannel carries, beside documentChangedChannel's bare id and in the same statement,
// a documentHeadNotice: which document changed, the head its stored history stands at once the
// sending transaction commits, and which process sent it (Service.ProcessID). A listener can then
// tell a change its room already holds, or one its own process made, from one it has to read.
// documentChangedChannel keeps its bare payload, so a process of an older build, which listens on
// that channel alone, still hears every change while two builds serve one database.
const documentHeadChannel = "dispatch_doc_head"

// notifyDocumentHeadStatement sends both notifications of a write in one statement: the bare id on
// documentChangedChannel ($1) and the documentHeadNotice on documentHeadChannel ($2).
const notifyDocumentHeadStatement = `select pg_notify('` + documentChangedChannel + `', $1), pg_notify('` + documentHeadChannel + `', $2)`

// documentHeadNotice is documentHeadChannel's payload, encoded as JSON with one-letter keys to keep
// it small inside Postgres's 8000-byte notification limit: {"r":"<artifact uuid>","v":<head after
// commit>,"p":"<process id>"}. Process is empty for a store no Service was built on, which no
// process takes for its own.
type documentHeadNotice struct {
	Room    string `json:"r"`
	Head    int64  `json:"v"`
	Process string `json:"p"`
}

// headNotice is the documentHeadChannel payload of a write of this store that leaves room's stored
// history at head.
func (p *PgVersioned) headNotice(room string, head int64) (string, error) {
	notice, err := json.Marshal(documentHeadNotice{Room: room, Head: head, Process: p.processID})
	if err != nil {
		return "", fmt.Errorf("encode the document's head notification: %w", err)
	}
	return string(notice), nil
}

// notifyDocumentChanged announces room's durable change inside tx, the transaction that made it,
// which holds the document's advisory lock (lockDocumentRoom): documentChangedChannel's bare id
// and documentHeadChannel's notice of head, the head the change leaves the history at, in one
// statement (notifyDocumentHeadStatement). It is the announcement of a rebuild, of a prune's
// checkpoint and of a deletion, one per write; an append folds the same two pg_notify calls into
// its own insert rather than spending a second round trip on them under that lock
// (appendUpdatesStatement). A compaction announces nothing: it changes no content and no head.
//
// Inside that transaction for two reasons. Postgres delivers a notification only if the
// transaction that sent it commits, so an append the gate refused (ErrRoomBehindHead), or one
// whose commit failed, evicts nobody's room: nothing announces a change the store does not hold.
// And the lock orders the announcement with the history it announces - a listener that hears this
// and reads the head (currentUpdateCursor) reads one at or past the version this transaction
// wrote, never one before it, and the notices of one document arrive in the order their writes
// committed, so the head the last of them names is the head the history stands at.
//
// It is a statement, not a trigger on doc_updates, and there is no queue table: a trigger or a
// table would need a migration, and Dispatch's pre-deploy census refuses a migration touching a
// table at or above 1 GiB, which doc_updates is. Nothing durable is wanted either. What a process
// does with this - close the room it holds - is worth nothing once that process is gone, so rows
// nobody would ever read back are the whole cost of a table.
func (p *PgVersioned) notifyDocumentChanged(ctx context.Context, tx pgx.Tx, room string, head int64) error {
	notice, err := p.headNotice(room, head)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, notifyDocumentHeadStatement, room, notice); err != nil {
		return fmt.Errorf("notify the document's change: %w", err)
	}
	return nil
}
