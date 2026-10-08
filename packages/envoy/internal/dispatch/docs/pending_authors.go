package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A document's pending authors - every author whose change no committed version lists yet - are
// in one of two places. F is the room's in-flight credits (roomState.inflight): a browser edit's
// authors from the moment the room's update observer credits them (creditContentChange) until the
// edit's append lands. R is the durable record (doc_pending_authors, migration 0083): a browser
// edit's append writes its authors there in the update's own transaction, and a committed API
// write writes its own in the transaction that commits its content. A version lists R and F's
// unconsumed credits it read under the document's advisory lock (lockDocumentRoom), deletes the R
// rows it read and marks the F credits it read consumed as it commits; an append whose credit a
// version consumed writes nothing to R. Every move between the two, and every version's read of
// them, holds that lock, and state.mu for F, so each author is in exactly one of F, R, or one
// committed version's list.

// inflightCredit is one browser edit's authors in F: the peers connected while the room applied
// it, and the latest edit source it names (lastActor, nil when the edit cannot be pinned on one
// peer). seq is the room's creditSeq when the observer credited it, which orders it against an
// upload's last read of the room (liveWrite.forkSeq). consumed marks a credit a committed version
// listed. The fields are guarded by the state.mu of the state whose inflight holds it.
type inflightCredit struct {
	seq       uint64
	authors   map[string]model.Actor
	lastActor *model.Actor
	consumed  bool
}

// UpdateCredit is a browser update's in-flight credit as it crosses VersionedStore: the store
// takes it in the transaction that appends the update, under the document's advisory lock (take),
// and reports it landed before it releases that lock (landed). Its fields are unexported, so a
// test store in another package can only pass it on. A nil credit is an update no browser edit
// credited, whose append writes no author.
type UpdateCredit struct {
	service *Service
	room    string
	state   *roomState
	record  *inflightCredit
	done    bool
}

// take is what the update's append writes: the credit's authors and latest edit source, and
// whether to write them at all, which is false once a committed version listed them. The caller
// holds the document's advisory lock, so no version can read or consume the credit until the
// append's transaction ends.
func (credit *UpdateCredit) take() (authors map[string]model.Actor, lastActor *model.Actor, include bool) {
	if credit == nil {
		return nil, nil, false
	}
	credit.state.mu.Lock()
	defer credit.state.mu.Unlock()
	if credit.record.consumed {
		return nil, nil, false
	}
	return credit.record.authors, credit.record.lastActor, true
}

// landed takes the credit out of F once its append has committed, or once the update will never
// be appended: its authors are in R, on a version, or dropped with an update the room failed on,
// which the browser resends. Its state may then be released (unlockState). A second call does
// nothing.
func (credit *UpdateCredit) landed() {
	if credit == nil {
		return
	}
	state := credit.state
	state.mu.Lock()
	if !credit.done {
		credit.done = true
		delete(state.inflight, credit.record.seq)
	}
	credit.service.unlockState(credit.room, state)
}

// authorCapture is whom a version credits and what its commit takes out of the pending authors:
// rKeys, the R rows it read (all of them when full), and inflight, the unconsumed F credits it
// read from state. authors is the version's own list, those and the writing transaction's own
// credits and actor. full is an upload's capture: its version lists the uploader alone, and its
// commit deletes every R row, since its replacement holds or removed every change made before its
// last read of the room.
type authorCapture struct {
	state    *roomState
	authors  map[string]model.Actor
	rKeys    []model.Actor
	inflight []*inflightCredit
	full     bool
}

// unconsumedInflightLocked is state's F credits no committed version has listed, observed no later
// than through. The caller holds state.mu.
func (state *roomState) unconsumedInflightLocked(through uint64) []*inflightCredit {
	var records []*inflightCredit
	for seq, record := range state.inflight {
		if seq <= through && !record.consumed {
			records = append(records, record)
		}
	}
	return records
}

// consumeLocked marks the F credits the capture read consumed, once the version that listed them
// has committed: their appends then write nothing to R. The caller holds capture.state.mu.
func (capture authorCapture) consumeLocked() {
	for _, record := range capture.inflight {
		record.consumed = true
	}
}

// upsertPendingAuthors writes authors to R in tx, which holds the document's advisory lock.
func upsertPendingAuthors(ctx context.Context, tx pgx.Tx, room string, authors map[string]model.Actor) error {
	if len(authors) == 0 {
		return nil
	}
	actors := make([]model.Actor, 0, len(authors))
	for _, actor := range authors {
		actors = append(actors, actor)
	}
	encoded, err := json.Marshal(actors)
	if err != nil {
		return fmt.Errorf("encode the document's pending authors: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_pending_authors (artifact_id, actor_kind, actor_id, actor)
		select $1::uuid, author->>'kind', author->>'id', author from jsonb_array_elements($2::jsonb) author
		on conflict (artifact_id, actor_kind, actor_id) do update set actor = excluded.actor
	`, room, string(encoded)); err != nil {
		return fmt.Errorf("record the document's pending authors: %w", err)
	}
	return nil
}

// readPendingAuthors is R for room, keyed by actorKey. A version reads it in its own transaction
// under the document's advisory lock, so no append or write changes it until that version ends.
func readPendingAuthors(ctx context.Context, q Queryer, room string) (map[string]model.Actor, error) {
	rows, err := q.Query(ctx, `select actor from doc_pending_authors where artifact_id = $1`, room)
	if err != nil {
		return nil, fmt.Errorf("read the document's pending authors: %w", err)
	}
	defer rows.Close()
	owed := make(map[string]model.Actor)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("read the document's pending authors: %w", err)
		}
		var actor model.Actor
		if err := json.Unmarshal(encoded, &actor); err != nil {
			return nil, fmt.Errorf("decode a pending author of the document: %w", err)
		}
		owed[actorKey(actor)] = actor
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the document's pending authors: %w", err)
	}
	return owed, nil
}

// deletePendingAuthors deletes the R rows of authors in tx, the transaction of the version that
// lists them.
func deletePendingAuthors(ctx context.Context, tx pgx.Tx, room string, authors []model.Actor) error {
	if len(authors) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(authors))
	ids := make([]string, 0, len(authors))
	for _, actor := range authors {
		kinds = append(kinds, actor.Kind)
		ids = append(ids, actor.ID)
	}
	if _, err := tx.Exec(ctx, `
		delete from doc_pending_authors
		where artifact_id = $1 and (actor_kind, actor_id) in (select * from unnest($2::text[], $3::text[]))
	`, room, kinds, ids); err != nil {
		return fmt.Errorf("delete the document's listed pending authors: %w", err)
	}
	return nil
}

// deleteAllPendingAuthors deletes every R row of room in tx, an upload's transaction.
func deleteAllPendingAuthors(ctx context.Context, tx pgx.Tx, room string) error {
	if _, err := tx.Exec(ctx, `delete from doc_pending_authors where artifact_id = $1`, room); err != nil {
		return fmt.Errorf("delete the document's pending authors: %w", err)
	}
	return nil
}

// markSettlementPending records, in the transaction that appends a document update, that the
// document owes a settlement. With setLastActor it also records lastActor as the latest edit
// source that settlement names on its events, nil for an edit no one peer can be credited with
// (creditContentChange); without it the row keeps the one it has. The timer that runs the
// settlement lives only in memory, so one a shutdown cuts short is found here by the room's next
// load (onLoadDocument) and by the resumption (RunSettlementResumption). The settlement that covers
// the update deletes the row in the transaction that commits its writes (clearSettlementPending).
// The caller holds the document's advisory lock, which orders this row's writers as it orders
// updates. updateMarkedAt is true only for a newly appended update: recording the latest edit
// source after a write or while closing an issue must not make an old row wait another
// resumption age.
func markSettlementPending(ctx context.Context, tx pgx.Tx, room string, lastActor *model.Actor, setLastActor, updateMarkedAt bool) error {
	var encoded any
	if lastActor != nil {
		actor, err := json.Marshal(lastActor)
		if err != nil {
			return fmt.Errorf("encode the document's latest edit source: %w", err)
		}
		encoded = string(actor)
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, last_actor) values ($1, $2::jsonb)
		on conflict (artifact_id) do update set
			last_actor = case when $3 then excluded.last_actor else doc_settlements_pending.last_actor end,
			marked_at = case when $4 then now() else doc_settlements_pending.marked_at end
	`, room, encoded, setLastActor, updateMarkedAt); err != nil {
		return fmt.Errorf("record the document's pending settlement: %w", err)
	}
	return nil
}

// readOwedSettlement reports whether room owes a settlement and the latest edit source its row
// names. A room's load reads it on the pool reserved for loads and takes no advisory lock, which
// a durable writer can hold for as long as its transaction runs.
func readOwedSettlement(ctx context.Context, q Queryer, room string) (owed bool, lastActor *model.Actor, err error) {
	var encoded []byte
	err = q.QueryRow(ctx, `
		select last_actor from doc_settlements_pending where artifact_id = $1
	`, room).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("read the document's pending settlement: %w", err)
	}
	if encoded == nil {
		return true, nil, nil
	}
	var actor model.Actor
	if err := json.Unmarshal(encoded, &actor); err != nil {
		return false, nil, fmt.Errorf("decode the document's latest edit source: %w", err)
	}
	return true, &actor, nil
}

// persistLastActor records lastActor on room's pending-settlement row, in a transaction of its own
// holding the document's advisory lock: closing an issue keeps the latest edit source its
// settlement names once the room has gone.
func (s *Service) persistLastActor(ctx context.Context, room string, lastActor model.Actor) error {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the latest-edit-source transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return err
	}
	if err := markSettlementPending(ctx, tx, room, &lastActor, true, false); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the document's latest edit source: %w", err)
	}
	return nil
}
