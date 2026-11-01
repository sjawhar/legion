package docs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// The append gate: the one read, under the document's advisory lock the appending transaction
// holds, that says which version an append goes after and whether it is admitted at all
// (gateAppend), and what each append is judged against - the cursor of the document
// it was made on (documentCursor, appendingCursor) and, for a browser's update, what its room's
// observer recorded with it (BrowserAppend).

// documentCursor is the durable version the document a write was made on holds every update
// through: a room instance's cursor for an update made in a room, or a transaction's own view of
// it (liveWrite.heldThrough), which adds the versions that transaction has appended itself and no
// room holds until it publishes.
type documentCursor interface {
	heldThrough() int64
}

// ErrRoomBehindHead refuses an append the gate cannot admit from the room it was made in
// (gateAppend): a derived write from a room whose instance does not hold the durable head - with
// two envoy-dispatch processes on one Postgres, an update the other process appended has not been
// caught up into it yet, and a version, an ask reconciliation or a conditional edit rendered from
// it would be written over text this process has not seen - and any append from a room whose
// stored history was replaced under it (historyReplaced). It is ErrServiceUnavailable, which is
// what it is for a caller: this process cannot serve that write until the room catches up, or for
// a replaced history until it reloads.
var ErrRoomBehindHead = fmt.Errorf("%w: the document's room is behind its durable head", ErrServiceUnavailable)

// appendKind is which rule the gate applies to an append (gateAppend).
type appendKind int

const (
	// appendBrowser is a raw client delta (servicePersistenceAdapter.StoreUpdateContext): a Yjs
	// update commutes with whatever landed since, so it is admitted from a room that is behind
	// with its history intact, at the head, and its room's cursor folds it in once catch-up fills
	// the gap. A replaced history refuses it unless that history still holds every item it builds
	// on (storedHistoryHolds, a compaction's fold), and so does a closed issue (ErrIssueClosed),
	// unless the room's observer took the update before the close took effect
	// (observedBeforeClose).
	appendBrowser appendKind = iota
	// appendDerived is an update computed from the room's content - settlement's repair append
	// (settleRoomWithin), a live write's fork (applyLive through the transaction's liveWrite),
	// the block-id backfill's stamp - and keeps the strict rule: the room must hold the head. It
	// is not refused for a closed issue: a live write has already checked its issue under the
	// issue's row lock (lockArtifactOwner), the backfill stamps a closed issue's document on
	// purpose, and a settlement repairs only what was stored before the close.
	appendDerived
)

// BrowserAppend is what a room's own append of a browser's update carries to the store across
// VersionedStore (AppendUpdateWithCredit): cursor, the room instance the update was made in,
// which the gate reads (appendBrowser) and the append advances once it has committed; observed,
// when the room's update observer took the update (documentUpdateClass.observed), which tells an
// update the room took before its issue closed from one taken after (observedBeforeClose); and
// ahead, where a live write of this process may have stored that update already
// (storeRoomUpdatesAhead). Its fields are unexported, so a store in another package can only pass
// it on. A nil BrowserAppend, or one with no cursor, is an append no browser's room gates: a
// derived write is gated on the cursor of its own transaction's write or of the room that marked
// its context (appendingCursor).
type BrowserAppend struct {
	cursor   *roomCursor
	observed time.Time
	ahead    *storedAhead
}

// closeMargin is how long after an issue's close committed a browser update the room observed is
// still taken as one made while the issue was open (observedBeforeClose). The close's notification
// (dispatch_issue_open_changed) normally reaches every process's rooms within milliseconds of its
// commit, and closes them (setRoomIssueOpen); the margin covers that latency, the moment between
// the close's last statement and its commit, and the drift between this process's clock and the
// database's over the update's age. It is measured from closed_at, which the close's transaction
// stamps as its last statement before it commits (clock_timestamp(), in PATCH
// /api/v1/issues/{key}), not from the transaction's start: the close withdraws the issue's open
// asks first, writing their blocks under each document's lock until it commits, and a browser
// update waiting on that lock is no later for how long that took. An update observed later than
// this is the case the gate's issue read exists for: a room on a process that has not heard the
// close - its listener redialing, up to RedialBackoff's ceiling - still taking edits on a closed
// issue.
const closeMargin = 2 * time.Second

// appendGateStatement is the gate's one read, under the document's advisory lock the appending
// transaction holds: the head (max(version), 0 for none), whether any stored row at or below the
// room's cursor ($2) remains, which tells a room behind an intact history from one whose history a
// rebuild, a prune or a compaction replaced (historyReplaced), and how long ago the document's
// issue closed, in microseconds on the database's clock, null while it is open and for a document
// of no issue (as issueOpen has it open). It is one indexed scan of one document's rows at each end
// and one primary-key lookup each in artifacts and issues. The age is read at clock_timestamp(),
// the moment of the read, not now(), the start of the appending transaction.
//
// The issue is read here, in the append's own transaction, because a room whose process has not
// heard the issue close (its listener redialing, up to RedialBackoff's ceiling, or the notification
// still on its way) would otherwise store edits on a closed issue, which nothing settles until the
// issue reopens. The read takes no lock on the issue's row: an append whose read precedes the
// close's commit lands, as it would have a moment earlier, and is settled once the issue reopens.
// The prune checkpoint comes back with it, so the recovery a crashed prune owes
// (PgVersioned.recoverPruneTx) costs a round trip only where one stands rather than one on every
// append: normally the column is null and the append goes on from this read alone.
const appendGateStatement = `
	select coalesce(max(version), 0), coalesce(min(version) <= $2, false),
		(select (extract(epoch from clock_timestamp() - i.closed_at) * 1000000)::bigint
			from artifacts a join issues i on i.key = a.issue_key where a.id = $1),
		(select ceiling from doc_checkpoints where artifact_id = $1)
	from doc_updates where artifact_id = $1`

// appendGateRead is what one appendGateStatement read says about a document: latest, its head
// (0 for an empty history); intact, whether any stored row at or below the reading room's cursor
// remains; closedFor, how long ago its issue closed on the database's clock, nil while it is open
// and for a document of no issue; and pruneCeiling, a crashed prune's checkpoint, nil while none
// stands.
type appendGateRead struct {
	latest       int64
	intact       bool
	closedFor    *time.Duration
	pruneCeiling *int64
}

// readAppendGate is appendGateStatement for room against a room instance holding every version
// through through, read through q inside the appending transaction, under the document's advisory
// lock that transaction's session holds.
func readAppendGate(ctx context.Context, q Queryer, room string, through int64) (appendGateRead, error) {
	var read appendGateRead
	var closedMicros *int64
	if err := q.QueryRow(ctx, appendGateStatement, room, through).
		Scan(&read.latest, &read.intact, &closedMicros, &read.pruneCeiling); err != nil {
		return appendGateRead{}, fmt.Errorf("read the document's head for the append gate: %w", err)
	}
	if closedMicros != nil {
		closed := time.Duration(*closedMicros) * time.Microsecond
		read.closedFor = &closed
	}
	return read, nil
}

// observedBeforeClose is whether a browser update the room's observer took age ago, on this
// process's monotonic clock, was taken no later than closeMargin after its issue's close
// committed, closedFor ago on the database's clock (closed_at, which the close stamps as its last
// statement before its commit). Both are measured at the gate's read, so the two clocks need not
// agree, only run at about the same rate. Such an update was made while the issue was open, or
// before its room could have heard of the close, and is stored as it would have been had its
// append reached the store just ahead of the close: the document owes a settlement for it, which
// runs once the issue reopens. An update with no observed time is not taken as one.
func observedBeforeClose(observed time.Time, age, closedFor time.Duration) bool {
	return !observed.IsZero() && age+closeMargin >= closedFor
}

// gateAppend is the gate: the verdict over read, one readAppendGate of room taken under the
// document's advisory lock the appending transaction's session holds, so no append lands between
// that read and its own - the version the append goes after (it is stored at latest+1),
// ErrIssueClosed or ErrRoomBehindHead. The append comes from a room instance whose cursor holds
// every version through through, ruled by kind, of an update the room's observer took at observed
// (zero for a derived write or an append with no class). An appendBrowser append is refused when
// the document's issue is closed and its room took the update later than closeMargin after the
// close (observedBeforeClose), or when the room's history was replaced (historyReplaced); an
// appendDerived append unless the room holds the head exactly. Equality for the derived rule, not
// `through < head`: a room standing past the head holds a history the store no longer has.
//
// A browser's update refused for a replaced history above its room's cursor is admitted at the
// head after all when that history still holds every item the update builds on
// (storedHistoryHolds): a compaction folded the rows the room held into a later one, keeping their
// items, which is no replacement for a delta that commutes. Refused, the update was dropped with
// its room, and a browser whose issue closed meanwhile reconnects read-only and cannot send it
// again. replaced reports such an admission: the room cannot catch up to that history, so its
// caller marks the room's cursor (markReplaced), and once the append is stored the room is closed
// and reloaded at the head (Service.closeRoomOnReplacedHistory), rather than taking every later
// update of its browsers down this path, a decode of the whole stored document under the
// document's lock each.
func gateAppend(ctx context.Context, tx pgx.Tx, room string, through int64, kind appendKind, observed time.Time, update []byte, read appendGateRead) (latest int64, replaced bool, err error) {
	latest, intact, closedFor := read.latest, read.intact, read.closedFor
	// The update's age is taken at the read, as the database measures the close's.
	var age time.Duration
	if !observed.IsZero() {
		age = time.Since(observed)
	}
	switch {
	case kind == appendBrowser && closedFor != nil && !observedBeforeClose(observed, age, *closedFor):
		return 0, false, fmt.Errorf("%w: room %s's document belongs to an issue closed %s ago, and the room took the update %s ago",
			ErrIssueClosed, room, closedFor.Round(time.Millisecond), age.Round(time.Millisecond))
	case historyReplaced(through, latest, intact):
		if kind == appendBrowser && latest > through {
			holds, holdsErr := storedHistoryHolds(ctx, tx, room, latest, update)
			if holdsErr != nil {
				return 0, false, holdsErr
			}
			if holds {
				return latest, true, nil
			}
		}
		return 0, false, fmt.Errorf("%w: room %s holds the document through update %d, and the stored history was replaced under it (head %d)",
			ErrRoomBehindHead, room, through, latest)
	case kind == appendDerived && through != latest:
		return 0, false, fmt.Errorf("%w: room %s holds the document through update %d, the stored document stands at %d",
			ErrRoomBehindHead, room, through, latest)
	}
	return latest, false, nil
}

// storedHistoryHolds reports whether room's stored history through head holds every item update
// builds on: applied to the document that history decodes into (documentThrough), update parks
// nothing, neither an item whose origin or predecessor is missing nor a delete of an item the
// history lacks. A compaction's fold keeps every item it folds, so it holds what a room's update
// was made on; a rebuild's seed is a document of other items, which a room's update parks on. A
// history that already parks something answers false, as does an update it cannot apply.
func storedHistoryHolds(ctx context.Context, tx pgx.Tx, room string, head int64, update []byte) (bool, error) {
	doc, err := documentThrough(ctx, tx, room, persistence.Version(head))
	if err != nil || doc == nil {
		return false, err
	}
	if parks(doc) {
		return false, nil
	}
	if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
		return false, nil
	}
	return !parks(doc), nil
}

// historyReplaced is whether the stored history under a room instance holding every version
// through through is no longer the one it loaded and caught up from: latest, the head, is below
// through (a prune or a deletion lowered it), or through is past nothing and no stored row at or
// below it remains (intact false): a rebuild deleted every row and seeded head+1, or a compaction
// folded the rows the room holds into a later one. Neither leaves a row the room could take in on
// its own, so the room is closed and reloaded instead (catchUpRoom, closeRoomBehindHead). A
// compaction with compactKeep = 1 keeps the head row, so a room at the head is unaffected.
func historyReplaced(through, latest int64, intact bool) bool {
	return latest < through || (through > 0 && !intact)
}

// roomCursorContextKey marks a context with the cursor of the room instance a derived append's
// update was made in.
type roomCursorContextKey struct{}

// withRoomCursor marks ctx with cursor for the gate in appendUpdateTxClass, as a derived append
// (appendDerived). The appends made from a room outside any live write mark their own context,
// since no ledger of theirs names the document they were made on: settlement's repairs
// (settleRoomWithin) and the block-id backfill's stamp (backfillBlockIDs). A room's own browser
// append hands its cursor to the store with the update instead (BrowserAppend).
func withRoomCursor(ctx context.Context, cursor *roomCursor) context.Context {
	if cursor == nil {
		return ctx
	}
	return context.WithValue(ctx, roomCursorContextKey{}, cursor)
}

// roomCursorFrom is the cursor withRoomCursor marked ctx with, nil for a context no room marked.
func roomCursorFrom(ctx context.Context) *roomCursor {
	cursor, _ := ctx.Value(roomCursorContextKey{}).(*roomCursor)
	return cursor
}

// appendingCursor is the cursor the gate reads for a derived append to room (appendDerived): the
// calling transaction's own write to room when it has one, whose fork is what the write was made
// on, otherwise the cursor its caller marked the context with (withRoomCursor). A nil cursor is an
// append no cursor gates. A browser's append is gated on the cursor it carries itself
// (BrowserAppend), which no context marks.
//
// A transaction's write decides for itself: a cold write, whose fork came from the store because
// no room of this process held the document or its room was behind (coldFork), carries no cursor
// and is not gated. The gate refuses a write rendered from a room behind the head, and a cold
// write renders from the store instead - it holds no room, its update commutes with whatever
// landed since its read, and the room that loads after its publish reads the store, which holds
// both by then.
func appendingCursor(ctx context.Context, room string) documentCursor {
	if write := ledgerFrom(ctx).liveWriteFor(room); write != nil {
		if write.forkCursor == nil {
			return nil
		}
		return write
	}
	if cursor := roomCursorFrom(ctx); cursor != nil {
		return cursor
	}
	return nil
}
