package docs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
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
	// askAuthors are the ask blocks the update's render first showed with no write's registration,
	// each with the author the update observer named (observeAskBlocks), which the append records
	// in its own transaction (recordAskAuthors) whether or not a version already listed the credit.
	askAuthors map[string]model.Actor
	done       bool
}

// takeAskAuthors is the ask block authors the update's append records. It is fixed before the
// update's class is recorded anywhere, so it needs no lock.
func (credit *UpdateCredit) takeAskAuthors() map[string]model.Actor {
	if credit == nil {
		return nil
	}
	return credit.askAuthors
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
	credit.landedLocked()
	credit.service.unlockState(credit.room, state)
}

// landedLocked is landed for a caller that holds the credit's state.mu (commitConsuming), which
// it leaves to that caller to release.
func (credit *UpdateCredit) landedLocked() {
	if credit == nil || credit.done {
		return
	}
	credit.done = true
	delete(credit.state.inflight, credit.record.seq)
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
// can have recorded a pending author - a browser update it appended, a committed live write it
// published, and another process's append its catch-up applied (Service.catchUpRoom). An upload
// deletes only the pending-author rows whose writing update its room's instance holds
// (heldUpdates).
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
	// latest is the person who made the document's latest edit when the capture was taken
	// (latestEditSource), nil when none is known: the person a version's edit notice names as the
	// one who edited last (recordEditNotice), whoever writes the version.
	latest *model.Actor
	// durable is the document's latest person sender the capture read (readLastPerson), kept so a
	// capture that drops its in-flight credits (withoutInflight) can name latest again without them.
	durable *model.Actor
}

// uploadHold is what an upload's commit takes out of its room instance's record of the updates it
// holds: held, that record as of the upload's last read of the room, whose pending-author rows
// the commit deletes, and hold, the live record, which drops them once the commit has landed
// (roomHold.consumed).
type uploadHold struct {
	held heldUpdates
	hold *roomHold
}

// capturePendingAuthors reads R in tx, which holds the document's advisory lock, with the
// document's latest person sender (readLastPerson), and takes whom a version lists
// (captureAuthors), with the in-flight credits observed through `through`, under room's state
// lock.
func (s *Service) capturePendingAuthors(ctx context.Context, tx pgx.Tx, room string, write *liveWrite, actor *model.Actor, through uint64) (authorCapture, error) {
	owed, err := readPendingAuthors(ctx, tx, room)
	if err != nil {
		return authorCapture{}, err
	}
	durableLatest, err := readLastPerson(ctx, tx, room)
	if err != nil {
		return authorCapture{}, err
	}
	state := s.lockState(room)
	defer s.unlockState(room, state)
	return captureAuthors(state, owed, durableLatest, write, actor, through), nil
}

// everyCredit bounds no in-flight credit: a capture through it takes every credit its room has
// observed (captureAuthors).
const everyCredit = math.MaxUint64

// captureAuthors is whom a version lists: owed, the pending authors its transaction read under
// the document's advisory lock; the room's in-flight credits no committed version has listed and
// observed through `through`, a credit sequence (inflightCredit.seq) taken before the version read
// the room, so each is for a change the version holds - for a version of the calling transaction's
// own write, those observed by the write's read of the room (liveWrite.forkSeq), and none for a
// fork read from the store; the authors the calling transaction's own write records once it
// commits; and actor. The version's commit deletes the pending authors it read and marks the
// in-flight credits it took consumed (commitConsuming); a credit observed after the bound is not
// among them and stays pending for the next version, unless the version's transaction stored its
// update (flushThrough), whose append wrote its authors to the pending authors read here. The
// caller holds state.mu.
//
// It also names who made the document's latest change (authorCapture.latest), which is the sender
// of the latest edit and never a name taken out of the credited set. Every edit this room
// observed carries one sequence (roomState.creditSeq): an in-flight browser credit takes one when
// the room observes the update (creditContentChange), the calling transaction's own write takes
// one as each of its operations applies (creditLiveWrite). The newest of those by that sequence
// wins, and its sender is the one it recorded: for a browser edit the sole connected peer, nil
// when several were connected and ygo does not say which of them sent it; for the write its own
// actor. A nil there is an unknown sender, not a reason to look further back: the latest edit is
// still that one. With no edit of either kind that decides, durableLatest is the document's latest
// person sender as its stored updates and commits recorded it (doc_settlements_pending.last_person,
// latestPersonOf). That is the case once a browser edit's append has landed, which takes it out of
// the in-flight credits within milliseconds, so the process that holds the room reads it as one
// that never loaded the document does. actor, the version's writer, names nobody by itself: an
// agent writing a named version over someone's edit did not make that edit.
func captureAuthors(state *roomState, owed map[string]model.Actor, durableLatest *model.Actor, write *liveWrite, actor *model.Actor, through uint64) authorCapture {
	capture := authorCapture{
		state:    state,
		authors:  make(map[string]model.Actor, len(owed)+1),
		rAuthors: make([]model.Actor, 0, len(owed)),
	}
	for key, author := range owed {
		capture.authors[key] = author
		capture.rAuthors = append(capture.rAuthors, author)
	}
	capture.inflight = state.unconsumedInflightLocked(through)
	for _, record := range capture.inflight {
		maps.Copy(capture.authors, record.authors)
	}
	if write != nil {
		maps.Copy(capture.authors, write.credits)
	}
	if actor != nil {
		capture.authors[actorKey(*actor)] = *actor
	}
	capture.latest = latestEditSource(capture.inflight, write, durableLatest)
	capture.durable = durableLatest
	return capture
}

// latestEditSource is the person who made the document's latest edit, read from the senders the
// room and the calling transaction recorded, newest first by the sequence each took when its edit
// was observed or applied, and then from durable, the document's latest person sender, which the
// stored updates and commits keep by the same rule (latestPersonOf). It never reads a name out of
// the set of people an edit credits.
//
// Walking newest first, each edit answers for itself:
//   - a known sender that is a person is the answer;
//   - a known sender that is a session is not an editor, so the walk goes on to the edit before
//     it: an agent's version written over someone's typing still names that person;
//   - an unknown sender ends the walk with nobody when that edit credits a person, which is what
//     a browser edit made while several peers were connected leaves: ygo does not say which of
//     them sent it, and an older person must not stand in for whoever did;
//   - an unknown sender that credits only sessions is no person's edit, and the walk goes on.
func latestEditSource(inflight []*inflightCredit, write *liveWrite, durable *model.Actor) *model.Actor {
	sources := make([]editSource, 0, len(inflight)+1)
	for _, record := range inflight {
		sources = append(sources, editSource{
			seq: record.seq, sender: record.lastActor, creditsPerson: anyPerson(record.authors),
		})
	}
	if write != nil && write.creditSeq > 0 {
		sources = append(sources, editSource{
			seq: write.creditSeq, sender: write.actor, creditsPerson: anyPerson(write.credits),
		})
	}
	slices.SortFunc(sources, func(a, b editSource) int { return cmp.Compare(b.seq, a.seq) })
	for _, source := range sources {
		switch {
		case source.sender == nil && source.creditsPerson:
			return nil
		case source.sender == nil:
			continue
		case source.sender.Kind == "user":
			return cloneActor(source.sender)
		}
	}
	if durable != nil && durable.Kind == "user" {
		return cloneActor(durable)
	}
	return nil
}

// editSource is one edit the capture can order: the room sequence it took, the sender it recorded
// (nil when no one connection can be credited with it), and whether it credits any person.
type editSource struct {
	seq           uint64
	sender        *model.Actor
	creditsPerson bool
}

// anyPerson reports whether authors holds a person.
func anyPerson(authors map[string]model.Actor) bool {
	for _, author := range authors {
		if author.Kind == "user" {
			return true
		}
	}
	return false
}

// cloneActor copies actor, so a capture never shares the room's or a write's own pointer.
func cloneActor(actor *model.Actor) *model.Actor {
	if actor == nil {
		return nil
	}
	return new(*actor)
}

// withoutInflight is capture for a version rendered from the stored document rather than the
// room, whose room stands behind the durable head (captureLiveTextAndAuthors). The store holds no
// change still in flight in the room, so the version lists none of the room's in-flight credits
// and its commit consumes none: each stays in F, to reach R when its append lands, or, when the
// gate refuses that append from a room whose stored history was replaced, to be credited again
// when its browser resends the edit to the reloaded room. Listed here, that author would be
// listed again on the version holding the resent edit. The pending authors it read (R) and actor
// stay, as captureAuthors gave them. The person who edited last is read again without those
// credits (latestEditSource over the stored person alone), since an edit the version does not
// hold cannot name its notice.
func (capture authorCapture) withoutInflight(actor *model.Actor) authorCapture {
	authors := make(map[string]model.Actor, len(capture.rAuthors)+1)
	for _, author := range capture.rAuthors {
		authors[actorKey(author)] = author
	}
	if actor != nil {
		authors[actorKey(*actor)] = *actor
	}
	capture.authors, capture.inflight = authors, nil
	capture.latest = latestEditSource(nil, nil, capture.durable)
	return capture
}

// uploadCapture is an upload's capture over write: the in-flight credits observed by its read of
// the room (liveWrite.forkSeq), and the updates the room instance it read held then
// (liveWrite.forkHold) with the room's updates the write's transaction stored ahead of its own
// (RoomFlush.appended), whose pending-author rows that transaction wrote: the fork held them all.
// The upload holds the document's advisory lock from before that read, so no other append lands
// between it and the commit.
func uploadCapture(write *liveWrite) authorCapture {
	state := write.state
	state.mu.Lock()
	inflight := state.unconsumedInflightLocked(write.forkSeq)
	state.mu.Unlock()
	held := write.forkHold.snapshot()
	for _, version := range write.flushed.appended(nil) {
		held.since = append(held.since, int64(version))
	}
	return authorCapture{state: state, inflight: inflight, upload: &uploadHold{held: held, hold: write.forkHold}}
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

// pendingAuthorsStatement writes each author of one document as recorded by its own update. An
// author keeps one row per update that recorded it, so an upload that holds one of those updates
// and not another deletes only the first.
const pendingAuthorsStatement = `
	insert into doc_pending_authors (artifact_id, actor_kind, actor_id, actor, written_through)
	select $1::uuid, kind, id, actor, version
	from unnest($2::text[], $3::text[], $4::jsonb[], $5::bigint[]) as author(kind, id, actor, version)
	on conflict (artifact_id, actor_kind, actor_id, written_through) do update set actor = excluded.actor`

// pendingAuthorsArgs is pendingAuthorsStatement's arguments for authors, nil when they name
// nobody and there is nothing to write. authors must name each author at most once per update:
// one statement cannot write one row twice, so a repeated author and version is refused, naming
// it, before anything is written. An append queues the statement with these arguments among its
// own writes (appendUpdateTxClass), which go in one round trip.
func pendingAuthorsArgs(room string, authors []pendingAuthor) ([]any, error) {
	if len(authors) == 0 {
		return nil, nil
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
			return nil, fmt.Errorf("record the document's pending authors: %s/%s at update %d is named twice", author.actor.Kind, author.actor.ID, author.writtenThrough)
		}
		seen[pair] = struct{}{}
		raw, err := json.Marshal(author.actor)
		if err != nil {
			return nil, fmt.Errorf("encode the document's pending authors: %w", err)
		}
		kinds = append(kinds, author.actor.Kind)
		ids = append(ids, author.actor.ID)
		encoded = append(encoded, string(raw))
		versions = append(versions, int64(author.writtenThrough))
	}
	return []any{room, kinds, ids, encoded, versions}, nil
}

// upsertPendingAuthors writes authors to R in tx, which holds the document's advisory lock, in one
// statement (pendingAuthorsStatement).
func upsertPendingAuthors(ctx context.Context, tx pgx.Tx, room string, authors []pendingAuthor) error {
	args, err := pendingAuthorsArgs(room, authors)
	if err != nil || args == nil {
		return err
	}
	if _, err := tx.Exec(ctx, pendingAuthorsStatement, args...); err != nil {
		return fmt.Errorf("record the document's pending authors: %w", err)
	}
	return nil
}

// readPendingAuthors is R for room, keyed by actorKey. It says nothing about who edited last: the
// rows of one update are every peer that was connected when it landed, which is a set of names and
// not an order (latestEditSource reads the sender instead). A version reads R in its own
// transaction under the document's advisory lock, so no append or write changes it until that
// version ends.
func readPendingAuthors(ctx context.Context, q Queryer, room string) (map[string]model.Actor, error) {
	rows, err := q.Query(ctx, `
		select actor from doc_pending_authors where artifact_id = $1
	`, room)
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
// write, a seed, or a rebuild names who made the document's latest change. A person as lastActor
// is also the document's latest person sender (last_person); a session leaves that as it is.
func recordLatestEditSource(ctx context.Context, tx pgx.Tx, room string, lastActor *model.Actor) error {
	person, setPerson := latestPersonOf(lastActor, false)
	return markSettlementPending(ctx, tx, []string{room}, lastActor, true, false, person, setPerson)
}

// markUpdateOwed records, in the transaction appending a document update, that the document owes
// a settlement from now, keeping the latest edit source the row names.
func markUpdateOwed(ctx context.Context, tx pgx.Tx, room string) error {
	return markSettlementPending(ctx, tx, []string{room}, nil, false, true, nil, false)
}

// updateOwedArgs is settlementPendingStatement's arguments for the transaction appending a
// document update: the document owes a settlement from now, as markUpdateOwed has it. An update
// credited to an edit names lastActor as its latest edit source, nil for an edit no one peer can
// be credited with, and creditsPerson says whether the edit credits a person, which decides what
// an unknown sender does to the latest person sender (latestPersonOf); an update credited to no
// edit keeps the latest edit source the row names. The append queues the statement with these
// arguments among its own writes (appendUpdateTxClass), which go in one round trip.
func updateOwedArgs(room string, credited bool, lastActor *model.Actor, creditsPerson bool) ([]any, error) {
	if !credited {
		return settlementPendingArgs([]string{room}, nil, false, true, nil, false)
	}
	person, setPerson := latestPersonOf(lastActor, creditsPerson)
	return settlementPendingArgs([]string{room}, lastActor, true, true, person, setPerson)
}

// latestPersonOf is what an edit does to the document's latest person sender
// (doc_settlements_pending.last_person), by the rule latestEditSource walks the room's edits with:
// a person sender becomes it; an unknown sender on an edit that credits a person clears it, since
// that edit may be any of the people it credits and no older person may stand in for it; and a
// session sender, or an unknown sender crediting only sessions, is no person's edit and leaves it
// as it is (set false).
//
// The value lives as long as the row: a settlement deletes the row whether or not it wrote a
// version (clearSettlementPending), while a pending author survives a settlement that wrote none.
// So after such a settlement a person's credit can still be pending with no person behind it, and
// an agent's edit before the next version leaves that version naming nobody. It is then held
// rather than lost (holdEditNotice), so the cost is one window's attribution, not a notice. The
// row's last_actor was dropped the same way before this column existed.
func latestPersonOf(sender *model.Actor, creditsPerson bool) (person *model.Actor, set bool) {
	switch {
	case sender != nil && sender.Kind == "user":
		return sender, true
	case sender == nil && creditsPerson:
		return nil, true
	default:
		return nil, false
	}
}

// markSettlementPending records, in the transaction that appends a document update, that each
// document of rooms owes a settlement. With setLastActor it also records lastActor as the latest
// edit source that settlement names on its events, nil for an edit no one peer can be credited
// with (creditContentChange); without it each row keeps the one it has. With setPerson it records
// person as the latest person sender a version's edit notice names (latestPersonOf), nil to clear
// it; without it each row keeps the one it has. The timer that runs the
// settlement lives only in memory, so one a shutdown cuts short is found here by the room's next
// load (onLoadDocument) and by the resumption (RunSettlementResumption). The settlement that
// covers the update deletes the row in the transaction that commits its writes
// (clearSettlementPending). A caller marking its own document holds that document's advisory lock,
// which orders the row's writers as it orders updates; owedCopiesOf marks the copies of an ask on
// other documents, in one statement however many there are. updateMarkedAt is true for a newly
// appended update and for an owed copy: recording the latest edit source after a write or while
// closing an issue must not make an old row wait another resumption age.
func markSettlementPending(ctx context.Context, tx pgx.Tx, rooms []string, lastActor *model.Actor, setLastActor, updateMarkedAt bool, person *model.Actor, setPerson bool) error {
	args, err := settlementPendingArgs(rooms, lastActor, setLastActor, updateMarkedAt, person, setPerson)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, settlementPendingStatement, args...); err != nil {
		return fmt.Errorf("record the document's pending settlement: %w", err)
	}
	return nil
}

// settlementPendingStatement is the one statement markSettlementPending runs, in one statement
// however many documents it marks.
const settlementPendingStatement = `
	insert into doc_settlements_pending (artifact_id, last_actor, last_person)
	select unnest($1::uuid[]), $2::jsonb, $5::jsonb
	on conflict (artifact_id) do update set
		last_actor = case when $3 then excluded.last_actor else doc_settlements_pending.last_actor end,
		marked_at = case when $4 then now() else doc_settlements_pending.marked_at end,
		last_person = case when $6 then excluded.last_person else doc_settlements_pending.last_person end`

// settlementPendingArgs is settlementPendingStatement's arguments. An append queues the statement
// with them among its own writes (appendUpdateTxClass), which go in one round trip.
func settlementPendingArgs(rooms []string, lastActor *model.Actor, setLastActor, updateMarkedAt bool, person *model.Actor, setPerson bool) ([]any, error) {
	encoded, err := encodeActor(lastActor)
	if err != nil {
		return nil, fmt.Errorf("encode the document's latest edit source: %w", err)
	}
	encodedPerson, err := encodeActor(person)
	if err != nil {
		return nil, fmt.Errorf("encode the document's latest person sender: %w", err)
	}
	return []any{rooms, encoded, setLastActor, updateMarkedAt, encodedPerson, setPerson}, nil
}

// encodeActor is actor as the jsonb text the pending-settlement row stores, nil for none.
func encodeActor(actor *model.Actor) (any, error) {
	if actor == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(actor)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

// readLastPerson reads the document's latest person sender (doc_settlements_pending.last_person,
// latestPersonOf), nil when the row is gone or names none. A version reads it in the transaction
// holding the document's advisory lock, so no append changes it until that version ends.
func readLastPerson(ctx context.Context, q Queryer, room string) (*model.Actor, error) {
	var encoded []byte
	err := q.QueryRow(ctx, `
		select last_person from doc_settlements_pending where artifact_id = $1
	`, room).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && encoded == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the document's latest person sender: %w", err)
	}
	var actor model.Actor
	if err := json.Unmarshal(encoded, &actor); err != nil {
		return nil, fmt.Errorf("decode the document's latest person sender: %w", err)
	}
	return &actor, nil
}

// readOwedSettlement reports whether room owes a settlement and the latest edit source its row
// names. A room's load reads it on the pool reserved for loads and takes no advisory lock, which
// a durable writer can hold for as long as its transaction runs; a settlement reads it in its own
// transaction under that lock, for the actor its version names (settlementAuthors).
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

// settlementRowFacts is one reading of a document's pending-settlement row - whether a row was
// there and, where it was, the age it carries (marked_at) and the latest edit source it names
// (last_actor, kept as the stored jsonb text, empty for none, which Postgres renders one way for
// one value however it was written) - with the document's stored update head read beside it (the
// max version currentUpdateCursor reads). A close for a closed issue reads it at its snapshot and
// again under the document's advisory lock before it writes, and writes the row only where the
// two readings agree (persistLastActor).
type settlementRowFacts struct {
	present   bool
	markedAt  time.Time
	lastActor string
	head      int64
}

// unchangedSince is whether the row reads now as it read at snapshot. Each fact catches a writer
// of its own:
//   - the row being there, or not, catches the settlement, which deletes the row whether or not
//     it wrote a version (clearSettlementPending), and the append that writes one again;
//   - marked_at catches every append, which stamps it now() as it marks the document owed
//     (markUpdateOwed), so a row deleted and written again carries an age no reading of the row
//     before it holds;
//   - last_actor catches a write that names a newer edit source without moving that age: a
//     committed write, a seed or a rebuild (recordLatestEditSource).
//
// The stored head is compared only where the row was absent at the snapshot, which is the one
// reading the three facts above can return to: a row written again carries a new marked_at,
// while no row and no row again read alike however many rows stood between them. That gap is a
// real run - no row, an append writes one, the settlement that covers it deletes it - and the
// head is what that append moved. The settlement's version is no token there, since it writes
// one only where the document now reads differently from the latest version while it clears the
// row whatever it writes. Comparing the head on every reading would instead make a close stand
// down behind its own flush, whose repairs append updates of their own and name no edit source,
// and a row left naming nobody is a worse answer than one naming the editor of an edit that is
// no longer the latest.
func (facts settlementRowFacts) unchangedSince(snapshot settlementRowFacts) bool {
	if facts.present != snapshot.present {
		return false
	}
	if !snapshot.present {
		return facts.head == snapshot.head
	}
	return facts.markedAt.Equal(snapshot.markedAt) && facts.lastActor == snapshot.lastActor
}

// readSettlementRowFacts reads room's pending-settlement row and the document's stored update
// head in one statement, so the two are one moment's reading of the document. marked_at is not
// null on a row that exists (migration 0063), so a null there is no row.
//
// It takes no lock and waits for none: a plain select reads the row as the reader's own snapshot
// of the database holds it, whatever writer has it locked meanwhile, so a caller that holds the
// room's close gate waits here on nothing that could want that gate (closeRefusedAppendRoom).
// The write reads it again through its own transaction, under the document's advisory lock, so
// no append can land between that reading and the statement (persistLastActor).
func readSettlementRowFacts(ctx context.Context, q Queryer, room string) (settlementRowFacts, error) {
	var (
		markedAt  *time.Time
		lastActor []byte
		head      int64
	)
	if err := q.QueryRow(ctx, `
		select pending.marked_at, pending.last_actor,
			coalesce((select max(version) from doc_updates where artifact_id = $1), 0)
		from (values (1)) as one_row(x)
		left join doc_settlements_pending as pending on pending.artifact_id = $1
	`, room).Scan(&markedAt, &lastActor, &head); err != nil {
		return settlementRowFacts{}, fmt.Errorf("read the document's pending settlement and stored head: %w", err)
	}
	facts := settlementRowFacts{head: head}
	if markedAt != nil {
		facts.present = true
		facts.markedAt = *markedAt
		facts.lastActor = string(lastActor)
	}
	return facts, nil
}

// persistLastActor records the latest edit source snapshot holds on room's pending-settlement
// row, in a transaction of its own holding the document's advisory lock, and answers whether it
// wrote the row: closing an issue keeps the latest edit source its settlement names once the room
// has gone. It leaves the latest person sender (last_person) as it is: that source is the room's
// memory, which can be older than the row, since a joined write sets it at commit after a browser
// edit its transaction saw (Ledger.creditRooms) while that edit's append lands later and records
// the browser's editor. The row is the order the edits landed in.
//
// The document can move between the close's snapshot and this write, and the row is then not this
// close's to write. Two things decide, both taken at that snapshot (closeSnapshot). The room's
// record must still read closed at the credit sequence the source was read at (recordClosedAt),
// the condition the record's own clearing takes (lastActorPersisted): a reopen this process
// heard, or an edit it observed since, moves one of the two. And the row must read as it did
// (settlementRowFacts.unchangedSince), because the record answers for neither a settlement nor
// another process's append: a settlement deletes the row and clears the record's source without
// moving the credit sequence, so the record alone lets this run through - a close snapshots
// (alice, N); the issue reopens, a room loads and its settlement deletes the row; the issue closes
// again with no edit, so that close snapshots no source at the same N and writes nothing; this
// write then finds the record closed at N and puts alice back on a row a settlement had taken
// (INFRA-1470).
//
// The two closes write at different points. The append gate's close snapshots the source, the row
// and the head under the room's close gate and writes after the close and outside that gate
// (closeRefusedAppendRoom), and the issue can reopen inside that window: ygo takes a room out of
// its map before CloseRoom waits for the flush, and the wait for a caller inside the room lets the
// gate go between tries (closeWhenFree), so a newer room can load, its edit's append can write
// that edit's own source to this row under the same advisory lock, and the issue can close a
// second time. Writing the snapshot then would put the older source back and the settlement would
// name the person whose edit is not the latest. The issue-open lane snapshots the source under the
// record's lock and reads the row immediately before this write, so its window is this
// transaction's first two round trips, and it writes while the room is still live
// (setRoomIssueOpen): a keystroke the room takes in between moves the credit sequence, and that
// keystroke's own append writes the row where it is stored.
//
// The row and the head are read here through tx, not the pool, so the advisory lock this
// transaction holds covers them. The record is read after that lock is taken, the order settlement
// takes the two in, and its lock is let go before the statement runs: the advisory lock holds off
// every other writer of this row for as long as this transaction lasts (append_writes.go), so
// nothing is gained by keeping the record locked across the statement, and every user of the
// record's lock - a keystroke's credit, a connect - would wait on a database round trip meanwhile.
func (s *Service) persistLastActor(ctx context.Context, room string, snapshot closeSnapshot) (bool, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the latest-edit-source transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return false, err
	}
	if !s.recordClosedAt(room, snapshot.creditSeq) {
		return false, nil
	}
	row, err := readSettlementRowFacts(ctx, tx, room)
	if err != nil {
		return false, err
	}
	if !row.unchangedSince(snapshot.row) {
		return false, nil
	}
	if err := markSettlementPending(ctx, tx, []string{room}, snapshot.lastActor, true, false, nil, false); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the document's latest edit source: %w", err)
	}
	return true, nil
}

// recordClosedAt is whether room's record is still the closed record a close snapshotted its
// latest edit source from: the service still holds a record for the room, it reads closed, and it
// stands at creditSeq, the credit sequence of that snapshot (closeRecordLocked). A reopen this
// process heard, or an edit it observed since, answers false. A settlement that took the source
// since, or an append another process made, moves neither the flag nor the sequence, so this
// check does not see them; the row's own facts, read beside it, do
// (settlementRowFacts.unchangedSince). Its caller holds the document's advisory lock and no
// record lock (persistLastActor).
func (s *Service) recordClosedAt(room string, creditSeq uint64) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	closedAt := state.closed && state.creditSeq.Load() == creditSeq
	s.unlockState(room, state)
	return closedAt
}
