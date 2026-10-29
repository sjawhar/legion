package docs

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"
)

// The updates a room instance holds and the store does not (roomInstance.unstored): an update is
// listed from the moment the room's update observer classified it until its append has returned,
// a live write forked from that instance stores the listed ones in its own transaction ahead of
// its own row (storeRoomUpdatesAhead), and storedAhead is where such an update is stored once it
// is, which its room's own append reads before it stores it again.

// storedAhead is where an update only its room held is stored: 0 while it is nowhere, else the
// version that holds it, which a live write of this process records when it stores the update in
// its own transaction (storeRoomUpdatesAhead) and the room's own append records when it commits
// (PgVersioned.appendUpdate). A version a write recorded is the update's only once that write has
// committed, and its transaction can roll back, so every reader confirms the stored row is the
// update before relying on it (storedAt), under the document's advisory lock, which no
// transaction holds across its own end.
type storedAhead struct{ version atomic.Int64 }

// storedAt is the version holding the update ahead recorded for, when the row stored there is that
// update, read through q under the document's advisory lock. A nil ahead is an update nothing
// stores ahead of its room's append.
func (ahead *storedAhead) storedAt(ctx context.Context, q Queryer, room string, update []byte) (persistence.Version, bool, error) {
	if ahead == nil {
		return 0, false, nil
	}
	at := ahead.version.Load()
	if at == 0 {
		return 0, false, nil
	}
	var stored bool
	if err := q.QueryRow(ctx, `
		select exists(select 1 from doc_updates where artifact_id = $1 and version = $2 and update = $3)
	`, room, at, update).Scan(&stored); err != nil {
		return 0, false, fmt.Errorf("read whether the room's update is stored already: %w", err)
	}
	return persistence.Version(at), stored, nil
}

// listUnstored lists class, an update only this instance holds, as unstored (roomInstance.unstored).
func (instance *roomInstance) listUnstored(class *documentUpdateClass) {
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	instance.unstored = append(instance.unstored, class)
}

// unlistUnstored takes class off the instance's unstored updates once its append has returned. A
// nil instance is a class nothing listed.
func (instance *roomInstance) unlistUnstored(class *documentUpdateClass) {
	if instance == nil {
		return
	}
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	if index := slices.Index(instance.unstored, class); index >= 0 {
		instance.unstored = slices.Delete(instance.unstored, index, index+1)
	}
}

// unstoredUpdates is the instance's unstored updates as of now, in the order the room committed
// them.
func (instance *roomInstance) unstoredUpdates() []*documentUpdateClass {
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	return slices.Clone(instance.unstored)
}

// storeRoomUpdatesAhead appends, in write's own transaction and ahead of the row it is about to
// append, every update the room instance its warm fork came from holds and the store does not
// (plan D3.8). A room's updates reach the store in the order its observers fire, and ygo
// broadcasts a peer's update only after they have, so the store holds no update before the ones it
// depends on - except for this writer: its fork can hold a peer's update whose append is still
// queued behind the room's persistence or waiting for the document's advisory lock, which the
// write holds from its first append until it ends, and its row, made on that update, would commit
// ahead of it. The store would then hold, until that append landed, a state the document never
// held, which every reader of it - a cold fork, a read, a load on another process - would take for
// the document. Storing those updates first keeps every stored prefix a state the room held.
//
// Every append of the write's runs it first (appendUpdateTxClass calls write.storeAhead), under
// the document's advisory lock that append has just taken. The updates are the instance's
// unstored ones (roomInstance.unstored), taken after draining the room's update log
// (observeRoomUpdates): every update committed to the room before the fork was read is classified,
// and listed, by then. They are all of them, in the order the room committed them, so each comes
// after the ones it depends on whether or not the fork holds it. One already stored - by its room's
// append, which committed before this write took the lock, or by an earlier append of this write's
// or a write that committed - is skipped, which storedAt tells from the stored row itself. Each is
// appended as its room's append would have been, with its classification and its browser's
// in-flight credit (AppendUpdateTxWithCredit): the pending authors and the latest edit source it
// writes are its browser's, not the write's actor's, and land, as the write's own, only if the
// transaction commits. Its room's append then finds it stored and writes nothing
// (PgVersioned.appendUpdate); a transaction that rolls back leaves that append to store it.
//
// A cold fork holds no room's update the store lacks, and stores nothing here; neither do the
// appends this makes, which pass through appendUpdateTxClass themselves (storingAhead).
func (s *Service) storeRoomUpdatesAhead(ctx context.Context, write *liveWrite, tx pgx.Tx) error {
	instance := write.forkInstance
	if write.cold || instance == nil || write.storingAhead {
		return nil
	}
	write.storingAhead = true
	defer func() { write.storingAhead = false }()
	if live := write.forkedFrom.Value(); live != nil {
		s.observeRoomUpdates(write.artifactID, live)
	}
	for _, class := range instance.unstoredUpdates() {
		// The transaction reads its own rows, so an update an earlier append of this write stored
		// counts as stored too.
		if _, stored, err := class.ahead.storedAt(ctx, tx, write.artifactID, class.update); err != nil {
			return err
		} else if stored {
			continue
		}
		version, err := s.persistence.AppendUpdateTxWithCredit(ctx, tx, write.artifactID, class.update, class.contentChanged, class.credit)
		if err != nil {
			return fmt.Errorf("store the room's update ahead of the write: %w", err)
		}
		class.ahead.version.Store(int64(version))
		write.copies = append(write.copies, version)
		write.appended = append(write.appended, version)
		if class.credit != nil && class.credit.record.seq <= write.forkSeq {
			write.heldCopies = append(write.heldCopies, int64(version))
		}
	}
	return nil
}
