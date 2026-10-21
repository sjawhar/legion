package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A document's pending authors - every author whose change no committed version lists yet - are
// in one of two places. F is the room's in-flight credits (roomState.inflight): a browser edit's
// authors from the moment the room's update observer credits them (creditContentChange) until the
// edit's append lands. R is the durable record (doc_pending_authors, migration 0084): a browser
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
	// hold is the room instance whose update this is (roomHold), which holds the update once its
	// append commits.
	hold *roomHold
	done bool
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

// held records that the credit's room instance holds the update its append stored as version.
// The caller holds the document's advisory lock and has committed the append, so no other
// process's append can take that version.
func (credit *UpdateCredit) held(version persistence.Version) {
	if credit != nil {
		credit.hold.add(int64(version))
	}
}

// roomHold is which durable updates one instance of a room holds: every update through loaded,
// the head the instance loaded at, and each later update the instance itself took whose write
// can have recorded a pending author - a browser update it appended, and a committed live write
// it published. Another process's append after the load is in neither, since only a reload brings
// it into this instance. An upload deletes only the pending-author rows whose writing update its
// room's instance holds (heldUpdates).
//
// It stays bounded: an update just past loaded folds into loaded, with the run of updates since
// that follows it, and an upload's commit drops the updates it consumed the rows of (consumed).
type roomHold struct {
	mu     sync.Mutex
	loaded int64
	since  []int64
}

func (hold *roomHold) add(version int64) {
	if hold == nil {
		return
	}
	hold.mu.Lock()
	defer hold.mu.Unlock()
	if version <= hold.loaded {
		return
	}
	// Appends arrive in ascending order, so the search lands at the end and the insert appends.
	at, found := slices.BinarySearch(hold.since, version)
	if found {
		return
	}
	hold.since = slices.Insert(hold.since, at, version)
	hold.foldLocked()
}

// foldLocked folds into loaded the run of held updates that follows it, version by version. since
// is kept sorted (add), so the run is its prefix. The caller holds mu.
func (hold *roomHold) foldLocked() {
	folded := 0
	for folded < len(hold.since) && hold.since[folded] == hold.loaded+1 {
		hold.loaded++
		folded++
	}
	hold.since = slices.Delete(hold.since, 0, folded)
}

// consumed drops the updates past loaded an upload's commit deleted the pending-author rows of: a
// row is written only at the version of the update that recorded it, and versions only increase,
// so no row written later can match one of them.
func (hold *roomHold) consumed(held heldUpdates) {
	if hold == nil {
		return
	}
	hold.mu.Lock()
	defer hold.mu.Unlock()
	hold.since = slices.DeleteFunc(hold.since, func(version int64) bool {
		return slices.Contains(held.since, version)
	})
}

// heldUpdates is a roomHold as of one moment, which an upload's commit deletes the pending
// authors of.
type heldUpdates struct {
	through int64
	since   []int64
}

func (hold *roomHold) snapshot() heldUpdates {
	if hold == nil {
		return heldUpdates{}
	}
	hold.mu.Lock()
	defer hold.mu.Unlock()
	return heldUpdates{through: hold.loaded, since: slices.Clone(hold.since)}
}

// roomInstance is what one resident room instance records about the durable history behind its
// document: hold, the updates whose writes can have recorded a pending author (roomHold), and
// cursor, the durable version the instance's document stands at (roomCursor). They are different
// sets, which is why the cursor is kept beside the hold rather than read off it: a browser append
// that changed no rendered content carries a nil credit and never enters the hold, and
// settlement's own repair append carries none either, so a hold equal to the head would say
// nothing about whether the instance holds it.
type roomInstance struct {
	hold   roomHold
	cursor roomCursor
}

// keepInstance starts the bookkeeping of live, a room instance loaded at head, and lists it for
// the instance's writes (instanceOf, holdOf) while live is resident (listResident). The load is
// the first advance of the instance's cursor: the instance holds every durable update through the
// head its load read.
func (s *Service) keepInstance(live *crdt.Doc, head persistence.Version) *roomInstance {
	instance := &roomInstance{hold: roomHold{loaded: int64(head)}, cursor: roomCursor{through: int64(head)}}
	listResident(&s.holds, live, instance)
	return instance
}

// instanceOf is live's bookkeeping, nil for a document no room load listed.
func (s *Service) instanceOf(live *crdt.Doc) *roomInstance {
	if listed, ok := s.holds.Load(weak.Make(live)); ok {
		return listed.(*roomInstance)
	}
	return nil
}

// holdOf is live's roomHold, nil for a document no room load listed.
func (s *Service) holdOf(live *crdt.Doc) *roomHold {
	if instance := s.instanceOf(live); instance != nil {
		return &instance.hold
	}
	return nil
}

// cursorOf is live's roomCursor, nil for a document no room load listed.
func (s *Service) cursorOf(live *crdt.Doc) *roomCursor {
	if instance := s.instanceOf(live); instance != nil {
		return &instance.cursor
	}
	return nil
}

// roomCursor is the durable version one instance of a room stands at: the head its load read, and
// every version appended since whose update that instance applied - a browser update its own
// append stored, a committed live write published into it, and settlement's own repair. Two
// processes serve one database, and a room resident in this one never receives the updates the
// other appended, so an append that raised the head elsewhere is in no cursor here: only a reload
// brings it in. Every write that renders durable state from a room - a settlement's version and
// its ask reconciliation, an upload's or a conditional edit's version, a rebuild - is refused
// while this is not the durable head (ErrRoomBehindHead), because what it would write is rendered
// from a document missing that update.
//
// It advances only by the version that follows the one it stands at, through advanceRoomCursor.
// A version with a gap before it - another process's append this instance never took - leaves the
// cursor where it was, since the instance does not hold the history through that version, and
// only a reload fills the gap: every gate refuses until then, which is the point.
type roomCursor struct {
	mu      sync.Mutex
	through int64
}

// advanceRoomCursor records that cursor's room instance now holds the durable update stored at
// version. It is the one funnel every advance goes through, so the sites that have to advance a
// cursor are one grep away. They are every site that makes a version durable for a document a
// room of this process holds:
//
//   - the room's load, which starts the cursor at the head it read (keepInstance);
//   - a browser append that committed, inside the lock it holds (PgVersioned.appendUpdate);
//   - a live write's commit, for the versions its publish is about to apply to the room
//     (Ledger.takeRoomCursors), and that publish itself (publishLiveUpdate);
//   - settlement's own repair append, its superseded path included (settleRoomWithin);
//   - the block-id backfill's stamp (backfillBlockIDs).
//
// A nil cursor is a document no room instance holds, whose writes no cursor gates.
func advanceRoomCursor(cursor *roomCursor, version persistence.Version) {
	if cursor == nil {
		return
	}
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if int64(version) == cursor.through+1 {
		cursor.through = int64(version)
	}
}

// heldThrough is the durable version the instance holds every update through.
func (cursor *roomCursor) heldThrough() int64 {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	return cursor.through
}

// documentCursor is the durable version the document a write was made on holds every update
// through: a room instance's cursor for an update made in a room, or a transaction's own view of
// it (liveWrite.heldThrough), which adds the versions that transaction has appended itself and no
// room holds until it publishes.
type documentCursor interface {
	heldThrough() int64
}

// ErrRoomBehindHead refuses a write that would record durable state for a document from a room
// whose instance does not stand at the durable head: with two envoy-dispatch processes on one
// Postgres, the room resident here never received the updates the other process appended, and a
// version, an ask reconciliation or a conditional edit rendered from it would be written over text
// this process has never seen. It is ErrServiceUnavailable, which is what it is for a caller: this
// process cannot serve that write until the room reloads.
var ErrRoomBehindHead = fmt.Errorf("%w: the document's room is behind its durable head", ErrServiceUnavailable)

// refuseRoomBehindHead is the gate itself: cursor, the durable version the document the write was
// made on holds every update through, must be the head, read in the same transaction under the
// document's advisory lock so no append can land between the two reads.
//
// Equality, not `cursor < head`: a prune lowers the head (PruneAfter's checkpoint, finished by
// recoverPruneTx) and a document's deletion removes it altogether, so a room resident across one
// of those stands ahead of the head and holds a history the store no longer has; `<` would admit
// it. A nil cursor is a write with no room's document behind it to be behind the head - a new
// document's seed (SeedText), a test's own append - and is not gated.
func refuseRoomBehindHead(room string, cursor documentCursor, head int64) error {
	if cursor == nil {
		return nil
	}
	if at := cursor.heldThrough(); at != head {
		return fmt.Errorf("%w: room %s holds the document through update %d, the stored document stands at %d",
			ErrRoomBehindHead, room, at, head)
	}
	return nil
}

// roomCursorContextKey marks a context with the cursor of the room instance whose document an
// append's update was made on.
type roomCursorContextKey struct{}

// withRoomCursor marks ctx with cursor for the gate in appendUpdateTxClass. The appends made from
// a room outside any live write mark their own context, since no ledger of theirs names the
// document they were made on: the room's own appends (servicePersistenceAdapter.StoreUpdateContext,
// which takes the cursor the update's observer recorded with it), settlement's repairs
// (settleRoomWithin) and the block-id backfill's stamp (backfillBlockIDs).
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

// appendingCursor is the cursor the gate reads for an append to room: the calling transaction's
// own write to room when it has one, whose fork is what the write was made on, otherwise the room
// cursor its caller marked the context with. Nil is an append no cursor gates.
func appendingCursor(ctx context.Context, room string) documentCursor {
	if write := ledgerFrom(ctx).liveWriteFor(room); write != nil && write.forkCursor != nil {
		return write
	}
	if cursor := roomCursorFrom(ctx); cursor != nil {
		return cursor
	}
	return nil
}

// authorCapture is whom a version credits and what its commit takes out of the pending authors:
// rAuthors, the authors of the R rows it read, and inflight, the unconsumed F credits it read from
// state. authors is the version's own list, those and the writing transaction's own credits and
// actor. upload is set only for an upload's capture (uploadCapture), whose version lists the
// uploader alone; its commit deletes the R rows whose writing updates the upload's room instance
// held, since the replacement holds or removed their changes.
type authorCapture struct {
	state    *roomState
	authors  map[string]model.Actor
	rAuthors []model.Actor
	inflight []*inflightCredit
	upload   *uploadHold
}

// uploadHold is what an upload's commit takes out of its room instance's record of the updates it
// holds: held, that record as of the upload's last read of the room, whose pending-author rows
// the commit deletes, and hold, the live record, which drops them once the commit has landed
// (roomHold.consumed).
type uploadHold struct {
	held heldUpdates
	hold *roomHold
}

// capturePendingAuthors reads R in tx, which holds the document's advisory lock, and takes whom a
// version lists (captureAuthors) under room's state lock.
func (s *Service) capturePendingAuthors(ctx context.Context, tx pgx.Tx, room string, write *liveWrite, actor *model.Actor) (authorCapture, error) {
	owed, err := readPendingAuthors(ctx, tx, room)
	if err != nil {
		return authorCapture{}, err
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	return captureAuthors(state, owed, write, actor), nil
}

// captureAuthors is whom a version lists: owed, the pending authors its transaction read under
// the document's advisory lock; the room's in-flight credits no committed version has listed; the
// authors the calling transaction's own write records once it commits; and actor. The version's
// commit deletes the pending authors it read and marks the in-flight credits it took consumed
// (commitConsuming); a credit observed after this take is not among them and stays pending for
// the next version. The caller holds state.mu.
func captureAuthors(state *roomState, owed map[string]model.Actor, write *liveWrite, actor *model.Actor) authorCapture {
	capture := authorCapture{
		state:    state,
		authors:  make(map[string]model.Actor, len(owed)+1),
		rAuthors: make([]model.Actor, 0, len(owed)),
	}
	for key, author := range owed {
		capture.authors[key] = author
		capture.rAuthors = append(capture.rAuthors, author)
	}
	capture.inflight = state.unconsumedInflightLocked(state.creditSeq.Load())
	for _, record := range capture.inflight {
		maps.Copy(capture.authors, record.authors)
	}
	if write != nil {
		maps.Copy(capture.authors, write.credits)
	}
	if actor != nil {
		capture.authors[actorKey(*actor)] = *actor
	}
	return capture
}

// uploadCapture is an upload's capture over write: the in-flight credits observed by its last read
// of the room (liveWrite.forkSeq), and the updates the room instance it read held then
// (liveWrite.forkHold). The upload holds the document's advisory lock from before that read, so no
// append lands between it and the commit.
func uploadCapture(write *liveWrite) authorCapture {
	state := write.state
	state.mu.Lock()
	inflight := state.unconsumedInflightLocked(write.forkSeq)
	state.mu.Unlock()
	return authorCapture{state: state, inflight: inflight, upload: &uploadHold{held: write.forkHold.snapshot(), hold: write.forkHold}}
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
// has committed: their appends then write nothing to R. An upload's capture also drops from its
// room's hold the updates whose rows its commit deleted (roomHold.consumed). The caller holds
// capture.state.mu.
func (capture authorCapture) consumeLocked() {
	for _, record := range capture.inflight {
		record.consumed = true
	}
	if capture.upload != nil {
		capture.upload.hold.consumed(capture.upload.held)
	}
}

// deleteCapturedPendingAuthors deletes, in tx, the R rows a committed version's capture takes out:
// the rows of the authors it read, or for an upload the rows whose writing update its room held.
func deleteCapturedPendingAuthors(ctx context.Context, tx pgx.Tx, room string, capture authorCapture) error {
	if capture.upload != nil {
		return deleteHeldPendingAuthors(ctx, tx, room, capture.upload.held)
	}
	return deletePendingAuthors(ctx, tx, room, capture.rAuthors)
}

// pendingAuthor is one author's R row: the author, and the version of the update whose write
// recorded it.
type pendingAuthor struct {
	actor          model.Actor
	writtenThrough persistence.Version
}

// pendingAuthorsOf is each of authors with the version of the update whose write recorded it,
// which writtenThrough names for each author's key. authors is keyed by actorKey, so no author
// appears twice.
func pendingAuthorsOf(authors map[string]model.Actor, writtenThrough func(key string) persistence.Version) []pendingAuthor {
	pending := make([]pendingAuthor, 0, len(authors))
	for _, actor := range actorSlice(authors) {
		pending = append(pending, pendingAuthor{actor: actor, writtenThrough: writtenThrough(actorKey(actor))})
	}
	return pending
}

// pendingAuthorsAt is each of authors as recorded by the update stored at version.
func pendingAuthorsAt(authors map[string]model.Actor, version persistence.Version) []pendingAuthor {
	return pendingAuthorsOf(authors, func(string) persistence.Version { return version })
}

// upsertPendingAuthors writes authors to R in tx, which holds the document's advisory lock, in one
// statement, each as recorded by its own update. An author keeps one row per update that recorded
// it, so an upload that holds one of those updates and not another deletes only the first.
// authors must name each author at most once per update: one statement cannot write one row twice,
// so a repeated author and version is refused, naming it, before anything is written.
func upsertPendingAuthors(ctx context.Context, tx pgx.Tx, room string, authors []pendingAuthor) error {
	if len(authors) == 0 {
		return nil
	}
	type row struct {
		key     string
		version persistence.Version
	}
	seen := make(map[row]struct{}, len(authors))
	kinds := make([]string, 0, len(authors))
	ids := make([]string, 0, len(authors))
	encoded := make([]string, 0, len(authors))
	versions := make([]int64, 0, len(authors))
	for _, author := range authors {
		pair := row{key: actorKey(author.actor), version: author.writtenThrough}
		if _, repeated := seen[pair]; repeated {
			return fmt.Errorf("record the document's pending authors: %s/%s at update %d is named twice", author.actor.Kind, author.actor.ID, author.writtenThrough)
		}
		seen[pair] = struct{}{}
		raw, err := json.Marshal(author.actor)
		if err != nil {
			return fmt.Errorf("encode the document's pending authors: %w", err)
		}
		kinds = append(kinds, author.actor.Kind)
		ids = append(ids, author.actor.ID)
		encoded = append(encoded, string(raw))
		versions = append(versions, int64(author.writtenThrough))
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_pending_authors (artifact_id, actor_kind, actor_id, actor, written_through)
		select $1::uuid, kind, id, actor, version
		from unnest($2::text[], $3::text[], $4::jsonb[], $5::bigint[]) as author(kind, id, actor, version)
		on conflict (artifact_id, actor_kind, actor_id, written_through) do update set actor = excluded.actor
	`, room, kinds, ids, encoded, versions); err != nil {
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

// deleteHeldPendingAuthors deletes, in an upload's transaction, every R row of room whose writing
// update the upload's room instance held: written through the head it loaded, or by an update it
// took since.
func deleteHeldPendingAuthors(ctx context.Context, tx pgx.Tx, room string, held heldUpdates) error {
	if _, err := tx.Exec(ctx, `
		delete from doc_pending_authors
		where artifact_id = $1 and (written_through <= $2 or written_through = any($3::bigint[]))
	`, room, held.through, held.since); err != nil {
		return fmt.Errorf("delete the pending authors an upload's room held: %w", err)
	}
	return nil
}

// recordLatestEditSource records lastActor as the latest edit source on room's pending-settlement
// row in tx, which holds the document's advisory lock, without moving the row's age: a committed
// write, a seed, or an issue's close names who made the document's latest change.
func recordLatestEditSource(ctx context.Context, tx pgx.Tx, room string, lastActor *model.Actor) error {
	return markSettlementPending(ctx, tx, []string{room}, lastActor, true, false)
}

// markUpdateOwed records, in the transaction appending a document update, that the document owes
// a settlement from now, keeping the latest edit source the row names.
func markUpdateOwed(ctx context.Context, tx pgx.Tx, room string) error {
	return markSettlementPending(ctx, tx, []string{room}, nil, false, true)
}

// markUpdateOwedBy is markUpdateOwed for an update credited to an edit, which names lastActor as
// its latest edit source, nil for an edit no one peer can be credited with.
func markUpdateOwedBy(ctx context.Context, tx pgx.Tx, room string, lastActor *model.Actor) error {
	return markSettlementPending(ctx, tx, []string{room}, lastActor, true, true)
}

// markSettlementPending records, in the transaction that appends a document update, that each
// document of rooms owes a settlement. With setLastActor it also records lastActor as the latest
// edit source that settlement names on its events, nil for an edit no one peer can be credited
// with (creditContentChange); without it each row keeps the one it has. The timer that runs the
// settlement lives only in memory, so one a shutdown cuts short is found here by the room's next
// load (onLoadDocument) and by the resumption (RunSettlementResumption). The settlement that
// covers the update deletes the row in the transaction that commits its writes
// (clearSettlementPending). A caller marking its own document holds that document's advisory lock,
// which orders the row's writers as it orders updates; owedCopiesOf marks the copies of an ask on
// other documents, in one statement however many there are. updateMarkedAt is true for a newly
// appended update and for an owed copy: recording the latest edit source after a write or while
// closing an issue must not make an old row wait another resumption age.
func markSettlementPending(ctx context.Context, tx pgx.Tx, rooms []string, lastActor *model.Actor, setLastActor, updateMarkedAt bool) error {
	var encoded any
	if lastActor != nil {
		actor, err := json.Marshal(lastActor)
		if err != nil {
			return fmt.Errorf("encode the document's latest edit source: %w", err)
		}
		encoded = string(actor)
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, last_actor)
		select unnest($1::uuid[]), $2::jsonb
		on conflict (artifact_id) do update set
			last_actor = case when $3 then excluded.last_actor else doc_settlements_pending.last_actor end,
			marked_at = case when $4 then now() else doc_settlements_pending.marked_at end
	`, rooms, encoded, setLastActor, updateMarkedAt); err != nil {
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
	if err := recordLatestEditSource(ctx, tx, room, &lastActor); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the document's latest edit source: %w", err)
	}
	return nil
}
