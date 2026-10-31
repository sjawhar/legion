package docs

// The fold and decode of a room's stored history: the read of its rows a load and a compaction
// share (foldRead), which folds rows totalling at most foldBufferBytes from memory once its
// transaction has ended and streams larger logs inside it; the state the rows make (stateThrough);
// the document that state decodes into (documentThrough, LoadDocument); and the fold itself (fold,
// readsBackOtherwise).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stateThrough returns the state the room's stored updates through version, which updates replays,
// make, as one V1 update. It applies the updates one at a time, oldest first, to one document that
// collects garbage, and encodes that document, so a deleted item keeps its id and length but none
// of its content, and what it holds at once is that document and the one update being applied,
// however much the stored updates inserted and later deleted. Nothing reads deleted content back
// from the store: every document built from it - a room ygo's server loads, a read, a fork, a
// validation - is a crdt.New, which collects an item's content in the transaction that deletes it,
// the transaction that applies a load included.
//
// A document parks an update whose dependencies have not arrived, and a delete of an item it does
// not hold. ygo's encoding of the document carries both (upstream since v1.51.0), and when the
// document parked anything stateThrough also merges in each stored update's structs past the
// document's state vector and every stored update's deletes, as it did while ygo's encoding
// carried neither; a room that loads the state parks them again.
//
// The state is kept only when it reads back as the document that made it: decoded into a fresh
// document, it must make the same state vector. That catches a re-encoding that renumbers or drops
// a client's clocks, as a decoder that integrated the items after a skipped clock range would, at
// lower clocks; ygo parks them (upstream since v1.51.0, reearth/ygo#257). It does not catch a
// re-encoding that keeps every clock and moves text, such as a lost right origin:
// TestACompactedDocumentKeepsItsOrderThroughLaterUpdates guards that on each ygo bump. A state that
// reads back otherwise, or a log the fold's document cannot apply (one parking more than its
// pending queue, maxPending, holds), is not used: stateThrough returns the same stored updates
// merged whole, as they were stored (mergedThrough), and a compaction, which calls fold itself,
// leaves them as stored. The merge of a log the fold cannot apply is what a load of it decodes and
// refuses (ErrDocumentUnloadable), which offers its rebuild.
//
// A log of one update is returned as stored: a compaction leaves the rows it folds as one update
// holding their state, and a document's first update deletes nothing an earlier one inserted.
func stateThrough(room string, version persistence.Version, updates updateSource, maxPending int) ([]byte, error) {
	state, _, misread, err := fold(updates, maxPending)
	if err != nil {
		return nil, err
	}
	if misread == nil {
		return state, nil
	}
	slog.Warn(misreadWarning+"; serving them merged",
		"room", room, "version", int64(version), "error", misread)
	return mergedThrough(updates)
}

// documentThrough is the document stateThrough's state decodes into, decoded once: the document
// the fold's read-back check decoded when the state reads back, and otherwise - a log of one
// update, which the fold returns as stored, or a log that does not fold, which is served merged -
// the state decoded into a document with the store's pending queue, maxPending, as a room decodes
// it. A state that does not decode is ErrDocumentUnloadable; a log holding no update is no
// document.
func documentThrough(room string, version persistence.Version, updates updateSource, maxPending int) (*crdt.Doc, error) {
	state, readBack, misread, err := fold(updates, maxPending)
	switch {
	case err != nil:
		return nil, err
	case misread != nil:
		slog.Warn(misreadWarning+"; serving them merged",
			"room", room, "version", int64(version), "error", misread)
		if state, err = mergedThrough(updates); err != nil {
			return nil, err
		}
	case readBack != nil:
		return readBack, nil
	}
	if len(state) == 0 {
		return nil, nil
	}
	doc := newCappedDocumentCopy(maxPending)
	if err := decodeStoredState(doc, state); err != nil {
		return nil, fmt.Errorf("%w: decode live document: %w", ErrDocumentUnloadable, err)
	}
	return doc, nil
}

// mergedThrough is the stored updates updates replays merged whole, as they were stored.
func mergedThrough(updates updateSource) ([]byte, error) {
	var stored [][]byte
	if err := updates(func(update []byte) error {
		stored = append(stored, update)
		return nil
	}); err != nil {
		return nil, err
	}
	merged, err := crdt.MergeUpdatesV1(stored...)
	if err != nil {
		return nil, fmt.Errorf("merge document updates: %w", err)
	}
	return merged, nil
}

// foldBufferBytes is the most stored update bytes a fold reads into memory (foldRead): a load or a
// compaction whose rows total at most this reads them, ends its read transaction and folds with no
// connection held, and one past it folds while it streams them inside that transaction, one row at
// a time. A fold holds its document beside the rows either way, so this bounds what reading the
// rows first adds to it.
const foldBufferBytes = 32 << 20

// bufferedFolds is how many folds of rows read into memory (foldRead) run at once in a process,
// across its room loads, cold reads and compactions (PgVersioned.foldSlots): each holds up to
// foldBufferBytes of rows beside its fold's document and the read-back's, so a process holds at most
// bufferedFolds × foldBufferBytes of buffered rows. It is the rooms pool's size
// (store.RoomsPoolSize): a load used to fold inside its transaction on that pool, whose connections
// bounded how many folds ran at once, and a buffered fold that has ended its transaction holds no
// connection to bound it.
const bufferedFolds = store.RoomsPoolSize

// foldRead hands use the room's stored updates through version, the rows of them tx reads, which
// number rows and total bytes, and ends tx, committing it as what names. Rows within foldBuffer
// are read into memory and tx commits before use runs, so a fold of them holds no connection while
// it works; past it use streams them inside tx (storedUpdates), which commits once use returns, so
// a fold of a log however large holds one row at once beside its document. Either source replays
// the rows as often as use asks (updateSource), from one snapshot.
//
// A buffered fold first takes one of the process's foldSlots, waiting within ctx, and gives it back
// once use returns, so at most bufferedFolds hold their rows at once. The wait cannot close a
// cycle: a waiter holds tx's connection, of the rooms pool or the compaction pool, but a holder of
// a slot waits for nothing - it holds only its own transaction's connection while it reads the rows
// and no connection at all while it folds them - so every holder gives its slot back.
func (p *PgVersioned) foldRead(ctx context.Context, tx pgx.Tx, room string, version persistence.Version, rows int, bytes int64, what string, use func(updateSource) error) error {
	commit := func() error {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", what, err)
		}
		return nil
	}
	if bytes > p.foldBuffer() {
		if p.beforeFold != nil {
			p.beforeFold(room)
		}
		if err := use(storedUpdates(ctx, tx, room, version)); err != nil {
			return err
		}
		return commit()
	}
	select {
	case p.foldSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.foldSlots }()
	updates := make([][]byte, 0, rows)
	if err := eachUpdateThrough(ctx, tx, room, version, func(update []byte) error {
		updates = append(updates, update)
		return nil
	}); err != nil {
		return err
	}
	if err := commit(); err != nil {
		return err
	}
	if p.beforeFold != nil {
		p.beforeFold(room)
	}
	return use(memoryUpdates(ctx, updates))
}

// foldBuffer is the most stored update bytes foldRead reads into memory: foldBufferBytes, or the
// lower bound a test set (foldBufferLimit).
func (p *PgVersioned) foldBuffer() int64 {
	if p.foldBufferLimit != 0 {
		return p.foldBufferLimit
	}
	return foldBufferBytes
}

// storedLog is what a load reads of a room's stored updates before it reads the updates
// themselves, in one statement of its transaction: the stamp of the state at their head
// (readDocumentStamp), the head included, and how many rows stand at or below the head and their
// total bytes, which decide where the fold reads them from (foldRead).
type storedLog struct {
	stamp DocumentStamp
	rows  int
	bytes int64
}

// readStoredLog is the room's storedLog as tx reads it.
func readStoredLog(ctx context.Context, tx pgx.Tx, room string) (storedLog, error) {
	var stored storedLog
	if err := tx.QueryRow(ctx, `
		select s.version, s.row_xmin, r.updates, r.bytes
		from (`+documentStampSQL+`
		) s
		cross join lateral (
			select count(*) as updates, coalesce(sum(octet_length(d.update)), 0)::bigint as bytes
			from doc_updates d where d.artifact_id = $1 and d.version <= s.version
		) r
	`, room).Scan(&stored.stamp.Version, &stored.stamp.Row, &stored.rows, &stored.bytes); err != nil {
		return storedLog{}, fmt.Errorf("read the document's stored updates: %w", err)
	}
	return stored, nil
}

// errUnfoldable marks a stored update the fold's document could not apply.
var errUnfoldable = errors.New("apply a stored document update")

// updateSource hands use each update of a stored log, oldest first, every time it is called: the
// fold reads the log a second time when its document parked anything. It replays either the rows
// a transaction reads (storedUpdates) or rows already read into memory (memoryUpdates), so a load
// and a compaction fold one way whichever holds the rows.
type updateSource func(use func([]byte) error) error

// storedUpdates replays the room's stored updates through version, reading them one row at a time
// in tx (eachUpdateThrough).
func storedUpdates(ctx context.Context, tx pgx.Tx, room string, version persistence.Version) updateSource {
	return func(use func([]byte) error) error {
		return eachUpdateThrough(ctx, tx, room, version, use)
	}
}

// memoryUpdates replays updates, a log's rows already read, holding no connection. It stops with
// ctx's error before any update once ctx has ended, so a cold read's bound (coldReadTimeout) and
// Shutdown's deadline end a fold of rows in memory as they end one that streams them.
func memoryUpdates(ctx context.Context, updates [][]byte) updateSource {
	return func(use func([]byte) error) error {
		for _, update := range updates {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := use(update); err != nil {
				return err
			}
		}
		return nil
	}
}

// fold is the state the updates make, the document its read-back check decoded that state into,
// readBack, nil unless the state reads back, or misread naming why the updates do not fold into a
// state that reads back as them - an update the fold's document could not apply within its pending
// queue of maxPending items, or a state that reads back otherwise. A log of one update is returned
// as stored, with no document. The fold holds one document at once: its own, until it has encoded
// the state, then the read-back's.
func fold(updates updateSource, maxPending int) (state []byte, readBack *crdt.Doc, misread error, err error) {
	var doc *crdt.Doc
	apply := func(update []byte) error {
		if doc == nil {
			doc = newCappedDocumentCopy(maxPending)
		}
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			return fmt.Errorf("%w: %w", errUnfoldable, err)
		}
		return nil
	}
	var first []byte
	seen := 0
	err = updates(func(update []byte) error {
		seen++
		switch seen {
		case 1:
			first = update
			return nil
		case 2:
			if err := apply(first); err != nil {
				return err
			}
			first = nil
		}
		return apply(update)
	})
	switch {
	case errors.Is(err, errUnfoldable):
		return nil, nil, err, nil
	case err != nil:
		return nil, nil, nil, err
	case seen <= 1:
		return first, nil, nil, nil
	}
	state = crdt.EncodeStateAsUpdateV1(doc, nil)
	made := doc.StateVector()
	if parked := doc.PendingStats(); parked.Items > 0 || parked.DeleteRanges > 0 {
		parts := [][]byte{state}
		if err := updates(func(update []byte) error {
			rest, err := crdt.DiffUpdateV1(update, made)
			if err != nil {
				return fmt.Errorf("read parked document update: %w", err)
			}
			parts = append(parts, rest)
			return nil
		}); err != nil {
			return nil, nil, nil, err
		}
		if state, err = crdt.MergeUpdatesV1(parts...); err != nil {
			return nil, nil, nil, fmt.Errorf("keep parked document updates: %w", err)
		}
	}
	doc = nil
	readBack, misread = readsBackOtherwise(state, made, maxPending)
	return state, readBack, misread, nil
}

// readsBackOtherwise decodes state into a fresh document of maxPending pending items and names how
// that document's state vector differs from made, the state vector of the document that encoded
// it. When it does not differ it returns the document, the state decoded as a room decodes it, and
// a nil error.
func readsBackOtherwise(state []byte, made crdt.StateVector, maxPending int) (*crdt.Doc, error) {
	doc := newCappedDocumentCopy(maxPending)
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		return nil, fmt.Errorf("decode the folded state: %w", err)
	}
	read := doc.StateVector()
	if maps.Equal(made, read) {
		return doc, nil
	}
	for client, clock := range made {
		if read[client] != clock {
			return nil, fmt.Errorf("client %d reads back at clock %d, the stored updates make %d", client, read[client], clock)
		}
	}
	for client, clock := range read {
		if made[client] != clock {
			return nil, fmt.Errorf("client %d reads back at clock %d, the stored updates make %d", client, clock, made[client])
		}
	}
	return doc, nil
}

// eachUpdateThrough calls use with each of the room's stored updates through version, oldest first,
// reading one row at a time.
func eachUpdateThrough(ctx context.Context, tx pgx.Tx, room string, version persistence.Version, use func([]byte) error) error {
	rows, err := tx.Query(ctx, `
		select update from doc_updates
		where artifact_id = $1 and version <= $2
		order by version asc
	`, room, int64(version))
	if err != nil {
		return fmt.Errorf("read document updates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			return fmt.Errorf("scan document update: %w", err)
		}
		if err := use(update); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read document updates: %w", err)
	}
	return nil
}

// LoadedDocument is a room's document as of one stored state: Doc decoded from it, nil for a
// document with no stored update, and Stamp naming it.
type LoadedDocument struct {
	Doc   *crdt.Doc
	Stamp DocumentStamp
}

// LoadDocument is the room's document at its head, decoded once, and the stamp of the state it was
// decoded from, read as Load reads (loadStored): in one repeatable-read transaction on the rooms
// pool, folded once that transaction has ended when the rows at or below the head total at most
// foldBufferBytes and while it streams them inside it otherwise (foldRead), and noted once it has
// read more than one row. Its document is the one Load's state decodes into (documentThrough), so
// a read of it shows what a room load would. A history that does not decode is
// ErrDocumentUnloadable.
func (p *PgVersioned) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	var doc *crdt.Doc
	stamp, err := p.loadStored(ctx, room, func(head persistence.Version, updates updateSource) (err error) {
		doc, err = documentThrough(room, head, updates, p.maxPendingItems)
		return err
	})
	if err != nil {
		return LoadedDocument{}, err
	}
	return LoadedDocument{Doc: doc, Stamp: stamp}, nil
}
