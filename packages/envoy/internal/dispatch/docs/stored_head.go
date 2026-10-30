package docs

import (
	"context"
	"fmt"
	"slices"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// The stored head - the greatest version a document's stored history holds - is what this
// process's view of a document is judged against while another envoy-dispatch process writes the
// same document: what a read compares a resident room's cursor against (readStoredHead,
// roomHoldsHead, docTree), what a cold write's fork is brought up to under the document's lock
// (followStoredHead), what a committed write's lost operations are judged on (lostAtStoredHead),
// and what the other processes are asked to store before any of that is read (flushOtherRooms).
// A room of this process is the head only while it holds every stored row (roomCursor,
// roomCatchUps); behind it, a read takes the stored document instead and waits for no catch-up.

// flushOtherRooms has every other envoy-dispatch process that holds a room of artifactID store the
// updates that room has applied (a flush query, room_query.go), and waits for their answers, for
// at most roomQueryWait. A write forked after it - from the store, or from this process's room once
// it is at the head (forkLive) - then holds every update any process applied before it asked: an
// edit whose target a person on another process has just deleted fails as it would on one process
// (ErrQuoteNotFound), rather than writing into a paragraph that is gone.
//
// What the wait holds, and why it cannot deadlock: the calling transaction's own connection
// (tx, through which it reads the members, so it takes no second connection of the pool), and no
// lock of this document - not its owner row, not its writer slot, not its advisory lock. The query
// goes through the process's NOTIFY sender, its own connection, and is delivered at once, not at
// tx's commit. What a member's flush waits on is its room's appends, which take this document's
// advisory lock and a key-share lock on its artifact row, neither of which the caller holds. A
// caller holding the advisory lock is a mistake in the lock order the flush depends on; the query
// reads that from the database and skips the flush rather than wait out every member
// (roomQueries.ask).
func (s *Service) flushOtherRooms(ctx context.Context, tx Queryer, artifactID string) error {
	_, _, err := s.roomQueries.ask(ctx, tx, artifactID, roomQueryFlush)
	return err
}

// followStoredHead brings write's kept cold fork up to the stored head when its transaction holds
// the document's advisory lock, which it does from its first append to its end (applyLive): every
// stored row past the fork's basis (forkHold) that the transaction did not append itself is merged
// into the fork, and enters forkHold, since the fork now holds it. Another envoy-dispatch process
// can have appended those rows between the fork's read of the store and the transaction's first
// append - a person's delete whose append was waiting for the lock, say - and under the lock no
// more can land. So the version the write renders next, and the loss judged on its fork
// (refuseLostWrite), stand on every stored row the version's update cursor claims. The rows are
// read through the transaction, which holds the lock; they are durable, with their pending
// authors, so the fork may version them. A write that has not appended yet holds no lock, and its
// fork is left as its read made it.
func (s *Service) followStoredHead(ctx context.Context, write *liveWrite) error {
	tx, joined := txFromContext(ctx)
	if !joined || len(write.appended) == 0 {
		return nil
	}
	held := write.forkHold.snapshot()
	_, _, rows, err := readCatchUp(ctx, tx, write.artifactID, held.through)
	if err != nil {
		return err
	}
	versions := make([]int64, 0, len(rows))
	updates := make([][]byte, 0, len(rows))
	for _, row := range rows {
		if slices.Contains(held.since, row.version) || slices.Contains(write.appended, persistence.Version(row.version)) {
			continue
		}
		versions = append(versions, row.version)
		updates = append(updates, row.update)
	}
	if len(updates) == 0 {
		return nil
	}
	batch, err := mergeUpdates(updates)
	if err == nil {
		err = crdt.ApplyUpdateV1(write.fork, batch, nil)
	}
	if err != nil {
		// A fork an update failed to reach is in an unknown state: the next operation rebuilds it.
		write.releaseFork()
		return fmt.Errorf("bring a cold write's fork up to the stored head: %w", err)
	}
	for _, version := range versions {
		write.forkHold.add(version)
	}
	// The rows moved the fork, so the rendering taken from it no longer describes the document.
	write.dropRendering()
	return nil
}

// readStoredHead is the stored head a read of artifactID compares a resident room's cursor
// against (roomHoldsHead): the greatest stored version, as the gate and a version capture read it
// (currentUpdateCursor). It reads through the caller's transaction when there is one, since a
// second connection of the shared pool taken under it is what deadlocks the pool
// (store.ErrNestedAcquire), and otherwise on the pool that owns loads, as a cold read reads the
// store's stamp (PgVersioned.DocumentStamp), so a caller holding a pooled connection it did not
// join, settlement's transaction, takes no second one either. It takes no lock and waits for
// nothing. It is the one place a read learns the head.
func (s *Service) readStoredHead(ctx context.Context, artifactID string) (int64, error) {
	var q Queryer
	if tx, joined := txFromContext(ctx); joined {
		q = tx
	} else {
		rooms, err := s.store.Pool.Rooms()
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
		}
		q = rooms
	}
	head, err := currentUpdateCursor(ctx, q, artifactID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	return head, nil
}

// roomHoldsHead reports whether live, a resident room, holds every stored update through head:
// its cursor's contiguous run (roomCursor.heldThrough) stands at or past it. Each cursor advance
// follows the update into the document, so a room it reports holds that much, and a room only
// gains updates, so it holds them for every read after this. A room behind head is missing an
// update another process stored that its catch-up has not applied yet (roomCatchUps), and a read
// takes the stored document instead (storedTree) rather than wait for that catch-up. A room that
// keeps no cursor is not judged behind: nothing says it is (roomBehindHead).
func (s *Service) roomHoldsHead(live *crdt.Doc, head int64) bool {
	cursor := s.cursorOf(live)
	return cursor == nil || cursor.heldThrough() >= head
}

// roomHeld reports whether this process holds a room of room, resident or still loading.
func (s *Service) roomHeld(room string) bool {
	return s.srv.GetDoc(room) != nil || slices.Contains(s.srv.Rooms(), room)
}

// lostAtStoredHead is write's lost operations on the document at its stored head: this process's
// room - caught up through rooms first when others says another process may have stored what it
// lacks - or the stored document when no room is resident here or its catch-up did not bring it
// to the head.
func (s *Service) lostAtStoredHead(ctx context.Context, rooms Queryer, write *liveWrite, others bool) ([]int, error) {
	room := write.artifactID
	if live := s.srv.GetDoc(room); live != nil {
		current := true
		if others {
			outcome, err := s.catchUpRoom(ctx, rooms, room, live)
			current = err == nil && (outcome == catchUpCurrent || outcome == catchUpApplied)
		}
		if current {
			var lost []int
			var lossErr error
			if err := s.readLive(room, live, func(doc *crdt.Doc) {
				lost, lossErr = write.loss.lost(doc)
			}); err != nil {
				return nil, fmt.Errorf("copy the room: %w", err)
			}
			return lost, lossErr
		}
	}
	loaded, err := s.persistence.Load(ctx, room)
	if err != nil {
		return nil, fmt.Errorf("load the stored document: %w", err)
	}
	doc := newDocumentCopy()
	if len(loaded.Update) > 0 {
		if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
			return nil, fmt.Errorf("decode the stored document: %w", err)
		}
	}
	return write.loss.lost(doc)
}
