package docs

// The fold and decode of a room's stored history: the state its updates make (stateThrough), the
// document that state decodes into (documentThrough, LoadDocument), and the fold they and Compact
// share (foldThrough, fold, readsBackOtherwise).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// stateThrough returns the state the room's stored updates through version make, as one V1
// update. It applies them one at a time, oldest first, to one document that collects garbage, and
// encodes that document, so a deleted item keeps its id and length but none of its content, and
// what it holds at once is that document and the one update being applied, however much the
// stored updates inserted and later deleted. Nothing reads deleted content back from the store:
// every document built from it - a room ygo's server loads, a read, a fork, a validation - is a
// crdt.New, which collects an item's content in the transaction that deletes it, the transaction
// that applies a load included.
//
// A document parks an update whose dependencies have not arrived, and a delete of an item it does
// not hold, and its encoding carries neither, so when the document parked anything stateThrough
// merges the encoding with each stored update's structs past the document's state vector and with
// every stored update's deletes, for a room that loads the state to park them again.
//
// The state is kept only when it reads back as the document that made it: decoded into a fresh
// document, it must make the same state vector. That catches a re-encoding that renumbers or drops
// a client's clocks, as at ygo v1.49.6-sami.3, whose decoder integrated the items after a skipped
// clock range and so encoded them at lower clocks; v1.50.1-sami.2's decoder parks them instead. It
// does not catch a re-encoding that keeps every clock and moves text, such as a lost right origin:
// TestACompactedDocumentKeepsItsOrderThroughLaterUpdates guards that on each ygo bump. A state that
// reads back otherwise, or a log the fold's document cannot apply (one parking more than ygo's
// pending queue holds), is not used: stateThrough returns the stored updates merged whole, as they
// were read before it folded them, and Compact, which calls foldThrough itself, leaves them as
// stored. The merge of a log the fold cannot apply is what a load of it decodes and refuses
// (ErrDocumentUnloadable), which offers its rebuild.
//
// A log of one update is returned as stored: compaction leaves the state as one update, and a
// document's first update deletes nothing an earlier one inserted.
func stateThrough(ctx context.Context, tx pgx.Tx, room string, version persistence.Version) ([]byte, error) {
	state, misread, err := foldThrough(ctx, tx, room, version)
	if err != nil {
		return nil, err
	}
	if misread == nil {
		return state, nil
	}
	slog.Warn(misreadWarning+"; serving them merged",
		"room", room, "version", int64(version), "error", misread)
	return mergedThrough(ctx, tx, room, version)
}

// documentThrough is the document stateThrough's state decodes into, decoded once: the document
// the fold's read-back check decoded when the state reads back, and otherwise - a log of one
// update, which the fold returns as stored, or a log that does not fold, which is served merged -
// the state decoded into a document with the server's pending queue, as a room decodes it. A state
// that does not decode is ErrDocumentUnloadable; a log holding no update is no document.
func documentThrough(ctx context.Context, tx pgx.Tx, room string, version persistence.Version) (*crdt.Doc, error) {
	state, readBack, misread, err := fold(ctx, tx, room, version)
	switch {
	case err != nil:
		return nil, err
	case misread != nil:
		slog.Warn(misreadWarning+"; serving them merged",
			"room", room, "version", int64(version), "error", misread)
		if state, err = mergedThrough(ctx, tx, room, version); err != nil {
			return nil, err
		}
	case readBack != nil:
		return readBack, nil
	}
	if len(state) == 0 {
		return nil, nil
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		return nil, fmt.Errorf("%w: decode live document: %w", ErrDocumentUnloadable, err)
	}
	return doc, nil
}

// mergedThrough is the room's stored updates through version merged whole, as they were stored.
func mergedThrough(ctx context.Context, tx pgx.Tx, room string, version persistence.Version) ([]byte, error) {
	var updates [][]byte
	if err := eachUpdateThrough(ctx, tx, room, version, func(update []byte) error {
		updates = append(updates, update)
		return nil
	}); err != nil {
		return nil, err
	}
	merged, err := crdt.MergeUpdatesV1(updates...)
	if err != nil {
		return nil, fmt.Errorf("merge document updates: %w", err)
	}
	return merged, nil
}

// errUnfoldable marks a stored update the fold's document could not apply.
var errUnfoldable = errors.New("apply a stored document update")

// foldThrough is the fold stateThrough and Compact share: the state, or misread naming why the
// stored updates through version do not fold into a state that reads back as them - an update the
// fold's document could not apply, or a state that reads back otherwise.
func foldThrough(ctx context.Context, tx pgx.Tx, room string, version persistence.Version) (state []byte, misread error, err error) {
	state, _, misread, err = fold(ctx, tx, room, version)
	return state, misread, err
}

// fold is foldThrough with the document its read-back check decoded the state into, readBack, nil
// unless the state reads back. A log of one update is returned as stored, with no document. The
// fold holds one document at once: its own, until it has encoded the state, then the read-back's.
func fold(ctx context.Context, tx pgx.Tx, room string, version persistence.Version) (state []byte, readBack *crdt.Doc, misread error, err error) {
	var doc *crdt.Doc
	apply := func(update []byte) error {
		if doc == nil {
			doc = newDocumentCopy()
		}
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			return fmt.Errorf("%w: %w", errUnfoldable, err)
		}
		return nil
	}
	var first []byte
	seen := 0
	err = eachUpdateThrough(ctx, tx, room, version, func(update []byte) error {
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
		if err := eachUpdateThrough(ctx, tx, room, version, func(update []byte) error {
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
	readBack, misread = readsBackOtherwise(state, made)
	return state, readBack, misread, nil
}

// readsBackOtherwise decodes state into a fresh document and names how that document's state
// vector differs from made, the state vector of the document that encoded it. When it does not
// differ it returns the document, the state decoded as a room decodes it, and a nil error.
func readsBackOtherwise(state []byte, made crdt.StateVector) (*crdt.Doc, error) {
	doc := newDocumentCopy()
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
// decoded from, both read in the one repeatable-read transaction Load reads in. Its document is
// the one Load's state decodes into (documentThrough), so a read of it shows what a room load
// would. A history that does not decode is ErrDocumentUnloadable.
func (p *PgVersioned) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if err := ctx.Err(); err != nil {
		return LoadedDocument{}, err
	}
	rooms, err := p.store.Pool.Rooms()
	if err != nil {
		return LoadedDocument{}, err
	}
	tx, err := rooms.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return LoadedDocument{}, fmt.Errorf("begin document load: %w", err)
	}
	defer tx.Rollback(ctx)
	stamp, err := readDocumentStamp(ctx, tx, room)
	if err != nil {
		return LoadedDocument{}, err
	}
	loaded := LoadedDocument{Stamp: stamp}
	if stamp.Version != 0 {
		if loaded.Doc, err = documentThrough(ctx, tx, room, persistence.Version(stamp.Version)); err != nil {
			return LoadedDocument{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return LoadedDocument{}, fmt.Errorf("commit document load: %w", err)
	}
	return loaded, nil
}
