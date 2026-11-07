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
// holds, that says which version an append's run of updates goes after and whether it is admitted
// at all (gateAppend), and what each append is judged against - the cursor of the document it was
// made on (documentCursor, appendingCursor) and, for a browser's update, what its room's observer
// recorded with it (BrowserUpdate).

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
// transaction holds: the head (max(version), 0 for none), the oldest stored version (min(version),
// 0 for none), which tells a room behind an intact history from one whose history a rebuild, a
// prune or a compaction replaced (appendGateRead.historyIntact, historyReplaced), and how long ago
// the document's issue closed, in microseconds on the database's clock, null while it is open and
// for a document of no issue (as issueOpen has it open). It is one indexed scan of one document's
// rows at each end and one primary-key lookup each in artifacts and issues. The age is read at
// clock_timestamp(), the moment of the read, not now(), the start of the appending transaction.
//
// The room's cursor is no parameter of it: the bound comes back and the comparison is made in Go,
// so the read takes the document's id alone and a batch can queue it into the very pipeline that
// takes the document's lock (queueAppendGate), reading the cursor after that pipeline returns.
// Under the lock neither the cursor nor the head can move, so the later read is in the gate's
// order.
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
	select coalesce(max(version), 0), coalesce(min(version), 0),
		(select (extract(epoch from clock_timestamp() - i.closed_at) * 1000000)::bigint
			from artifacts a join issues i on i.key = a.issue_key where a.id = $1),
		(select ceiling from doc_checkpoints where artifact_id = $1)
	from doc_updates where artifact_id = $1`

// appendGateRead is what one appendGateStatement read says about a document: latest, its head
// (0 for an empty history); oldest, the lowest stored version (0 for an empty history); closedFor,
// how long ago its issue closed on the database's clock, nil while it is open and for a document
// of no issue; and pruneCeiling, a crashed prune's checkpoint, nil while none stands.
type appendGateRead struct {
	latest       int64
	oldest       int64
	closedFor    *time.Duration
	pruneCeiling *int64
}

// historyIntact is whether any stored row at or below through, the reading room's cursor, remains:
// the column main's gate used to read as `min(version) <= $2`, computed here instead so the read
// takes no cursor.
func (read appendGateRead) historyIntact(through int64) bool {
	return read.oldest != 0 && read.oldest <= through
}

// scan reads one appendGateStatement row into read.
func (read *appendGateRead) scan(row pgx.Row) error {
	var closedMicros *int64
	if err := row.Scan(&read.latest, &read.oldest, &closedMicros, &read.pruneCeiling); err != nil {
		return fmt.Errorf("read the document's head for the append gate: %w", err)
	}
	if closedMicros != nil {
		closed := time.Duration(*closedMicros) * time.Microsecond
		read.closedFor = &closed
	}
	return nil
}

// readAppendGate is appendGateStatement for room, read through q inside the appending transaction,
// under the document's advisory lock that transaction's session holds.
func readAppendGate(ctx context.Context, q Queryer, room string) (appendGateRead, error) {
	var read appendGateRead
	if err := read.scan(q.QueryRow(ctx, appendGateStatement, room)); err != nil {
		return appendGateRead{}, err
	}
	return read, nil
}

// appendGate is the gate's read queued into a caller's batch (queueAppendGate) and its answer once
// that batch has run (result). A batch of a room's browser updates queues the read into the
// pipeline that takes the document's lock (appendRoomTx), so the read costs it no round trip of
// its own.
type appendGate struct {
	room string
	// ran is set once the read has scanned; err is its failure.
	ran  bool
	err  error
	read appendGateRead
}

// queueAppendGate queues the gate's read of room into b, the caller's batch. The caller holds the
// document's advisory lock in the transaction b runs in, and once b has run asks result.
func queueAppendGate(b *pgx.Batch, room string) *appendGate {
	gate := &appendGate{room: room}
	b.Queue(appendGateStatement, room).QueryRow(gate.scan)
	return gate
}

func (g *appendGate) scan(row pgx.Row) error {
	if err := g.read.scan(row); err != nil {
		g.err = err
		return err
	}
	g.ran = true
	return nil
}

// result is the gate's read once its batch has run. A read that failed, or never ran because a
// statement before it in the batch failed, is an error too.
func (g *appendGate) result() (appendGateRead, error) {
	if g.err != nil {
		return appendGateRead{}, g.err
	}
	if !g.ran {
		return appendGateRead{}, fmt.Errorf("read the document's head for the append gate: room %s's read did not run", g.room)
	}
	return g.read, nil
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

// updateAge is how long ago the room's observer took an update, measured at the gate's read as the
// database measures its issue's close. An update with no observed time has no age.
func updateAge(observed time.Time) time.Duration {
	if observed.IsZero() {
		return 0
	}
	return time.Since(observed)
}

// gateAppend is the gate: the verdict over read, one appendGateStatement read of room taken under
// the document's advisory lock the appending transaction's session holds, so no append lands
// between that read and its own - the version the run's first update goes after (it is stored at
// latest+1), how many of the run are admitted, ErrIssueClosed or ErrRoomBehindHead. The run comes
// from a room instance whose cursor holds every version through through, ruled by kind, each
// update carrying when the room's observer took it (BrowserUpdate.Observed, zero for a derived
// write or an append with no class).
//
// An appendBrowser run is cut at the first update the document's closed issue no longer admits
// (observedBeforeClose): the admitted updates are a prefix, because the run ascends in the order
// the room took them and the rule is monotone in an update's age, so this stores exactly what the
// same updates would have stored one transaction each, and ErrIssueClosed refuses the run only
// when the prefix is empty. The rest stay queued, and each meets the gate again on its own call.
// The whole run is refused when the room's history was replaced (historyReplaced); an
// appendDerived run unless the room holds the head exactly. Equality for the derived rule, not
// `through < head`: a room standing past the head holds a history the store no longer has.
//
// A browser's run refused for a replaced history above its room's cursor is admitted at the head
// after all when that history still holds every item the run builds on (storedHistoryHolds): a
// compaction folded the rows the room held into a later one, keeping their items, which is no
// replacement for deltas that commute. Refused, the updates were dropped with their room, and a
// browser whose issue closed meanwhile reconnects read-only and cannot send them again. replaced
// reports such an admission: the room cannot catch up to that history, so its caller marks the
// room's cursor once for the run (markReplaced), and once the append is stored the room is closed
// and reloaded at the head (Service.closeRoomOnReplacedHistory), rather than taking every later
// update of its browsers down this path, a decode of the whole stored document under the
// document's lock each.
func gateAppend(ctx context.Context, q Queryer, room string, through int64, kind appendKind, updates []BrowserUpdate, read appendGateRead, maxPending int) (latest int64, admitted int, replaced bool, err error) {
	if len(updates) == 0 {
		return 0, 0, false, fmt.Errorf("append document updates: no update to append")
	}
	latest, admitted = read.latest, len(updates)
	if kind == appendBrowser && read.closedFor != nil {
		for admitted = 0; admitted < len(updates); admitted++ {
			observed := updates[admitted].Observed
			if !observedBeforeClose(observed, updateAge(observed), *read.closedFor) {
				break
			}
		}
		if admitted == 0 {
			return 0, 0, false, fmt.Errorf("%w: room %s's document belongs to an issue closed %s ago, and the room took the update %s ago",
				ErrIssueClosed, room, read.closedFor.Round(time.Millisecond), updateAge(updates[0].Observed).Round(time.Millisecond))
		}
		updates = updates[:admitted]
	}
	switch {
	case historyReplaced(through, latest, read.historyIntact(through)):
		if kind == appendBrowser && latest > through {
			holds, holdsErr := storedHistoryHolds(ctx, q, room, latest, updates, maxPending)
			if holdsErr != nil {
				return 0, 0, false, holdsErr
			}
			if holds {
				return latest, admitted, true, nil
			}
		}
		return 0, 0, false, fmt.Errorf("%w: room %s holds the document through update %d, and the stored history was replaced under it (head %d)",
			ErrRoomBehindHead, room, through, latest)
	case kind == appendDerived && through != latest:
		return 0, 0, false, fmt.Errorf("%w: room %s holds the document through update %d, the stored document stands at %d",
			ErrRoomBehindHead, room, through, latest)
	}
	return latest, admitted, false, nil
}

// storedHistoryHolds reports whether room's stored history through head holds every item updates,
// the run the append would store, builds on: applied in the room's commit order to the document
// that history decodes into (documentThrough), no update parks anything, neither an item whose
// origin or predecessor is missing nor a delete of an item the history lacks. The history is
// decoded once for the whole run, not once per update. A compaction's fold keeps every item it
// folds, so it holds what a room's update was made on; a rebuild's seed is a document of other
// items, which a room's update parks on. A history that already parks something answers false, as
// does a run it cannot apply: the run is admitted whole or not at all.
func storedHistoryHolds(ctx context.Context, q Queryer, room string, head int64, updates []BrowserUpdate, maxPending int) (bool, error) {
	version := persistence.Version(head)
	doc, err := documentThrough(room, version, storedUpdates(ctx, q, room, version), maxPending)
	if err != nil || doc == nil {
		return false, err
	}
	if parks(doc) {
		return false, nil
	}
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(doc, update.Update, nil); err != nil {
			return false, nil
		}
		if parks(doc) {
			return false, nil
		}
	}
	return true, nil
}

// historyReplaced is whether the stored history under a room instance holding every version
// through through is no longer the one it loaded and caught up from: latest, the head, is below
// through (a prune or a deletion lowered it), or through is past nothing and no stored row at or
// below it remains (intact false): a rebuild deleted every row and seeded head+1, or a compaction
// folded the rows the room holds into a later one. Neither leaves a row the room could take in on
// its own, so the room is closed and reloaded instead (catchUpRoom, closeRoomBehindHead). A
// compaction keeps the row it folds into, so a room at the head is unaffected.
func historyReplaced(through, latest int64, intact bool) bool {
	return latest < through || (through > 0 && !intact)
}

// roomCursorContextKey marks a context with the cursor of the room instance an append's update was
// made in.
type roomCursorContextKey struct{}

// withRoomCursor marks ctx with cursor for the gate (appendingCursor). The appends made from a
// room outside any live write mark their own context, since no ledger of theirs names the document
// they were made on: settlement's repairs (settleRoomWithin), the block-id backfill's stamp
// (backfillBlockIDs) and the room's own append of an update its observer recorded no class for
// (servicePersistenceAdapter.StoreUpdateContext). A room's own batch of browser updates is gated
// on the flush it stores instead (RoomFlush, appendRoomTx).
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
