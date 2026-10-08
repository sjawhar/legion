package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Ledger is what the document operations joined to one database transaction produce until it
// ends: the events they generate, their writes to live documents (see liveWrite) and the versions
// they write. Join is the only way to join a transaction, so an operation that has one always has
// its ledger. Commit ends the transaction the way every caller must: it records the writes'
// authors and takes out the authors its versions listed in the transaction itself, commits, marks
// the in-flight credits its versions listed consumed, records the writes' latest edit source in
// their rooms, and publishes the writes to their rooms, in that order. Discard, which callers
// defer right after Join, drops whatever a transaction that did not commit left behind.
//
// Settlement's operations run in a transaction of its own, which no caller joined, so its ledger
// carries no `tx` and marks itself `settling` instead. Those are the only two ledgers there are:
// every operation that writes a document either joins its caller's transaction or is
// settlement's own.
type Ledger struct {
	service *Service
	tx      pgx.Tx
	// settling marks settlement's ledger, whose operations run inside settlement's own
	// transaction although no caller joined it.
	settling bool
	events   []model.Event
	// live holds, per document, the writes this transaction made to it; order is the order it
	// first wrote them in.
	live     map[string]*liveWrite
	order    []string
	versions []ledgerVersion
	// seeds holds, per document this transaction seeded (SeedText), the actor that wrote its
	// first text and the tree it wrote, which credit records and registers as that actor's ask
	// blocks once the transaction has committed.
	seeds map[string]ledgerSeed
	// rebuilds are the documents this transaction rebuilds (RebuildDocument), whose rooms refuse
	// loads until it ends, committed or not.
	rebuilds []string
}

type ledgerVersion struct {
	artifactID string
	version    model.Version
	// capture is whom the version listed (authorCapture): the pending authors its commit deletes
	// and the in-flight credits it marks consumed.
	capture authorCapture
}

// ledgerSeed is a document SeedText wrote in this transaction: the actor who wrote its first
// text, and the tree so credit can register that actor against every ask block the seed
// introduced (registerAskAuthors) once the transaction has committed.
type ledgerSeed struct {
	actor model.Actor
	tree  *pmdoc.Node
}

type ledgerContextKey struct{}

// errUnjoined refuses a transactional operation called outside Join: its writes would reach no
// ledger, so their authors would never be credited or released.
var errUnjoined = errors.New("docs: the operation needs a transaction joined with Service.Join")

// Join joins the document operations run with the returned context to tx, recording what they
// produce in the returned ledger.
func (s *Service) Join(ctx context.Context, tx pgx.Tx) (context.Context, *Ledger) {
	ledger := &Ledger{service: s, tx: tx}
	return withLedger(ctx, ledger), ledger
}

// inTransaction reports whether the operations recording into l run inside an open database
// transaction that this ledger knows about: one a caller joined, or settlement's own. Such an
// operation fails on a failed room rather than waiting for its recovery (awaitRoomRecovery). An
// operation that runs inside a transaction without joining it is invisible here and still
// waits, which is the hang this rule exists to stop, so a handler joins (Docs.Join) and passes
// the context Join returned.
func (l *Ledger) inTransaction() bool {
	return l != nil && (l.tx != nil || l.settling)
}

func withLedger(ctx context.Context, ledger *Ledger) context.Context {
	return context.WithValue(ctx, ledgerContextKey{}, ledger)
}

func ledgerFrom(ctx context.Context) *Ledger {
	ledger, _ := ctx.Value(ledgerContextKey{}).(*Ledger)
	return ledger
}

func txFromContext(ctx context.Context) (pgx.Tx, bool) {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.tx == nil {
		return nil, false
	}
	return ledger.tx, true
}

func collectEvent(ctx context.Context, event model.Event) {
	if ledger := ledgerFrom(ctx); ledger != nil {
		ledger.events = append(ledger.events, event)
	}
}

// Commit commits the transaction, then credits, releases and publishes what its document
// operations recorded (see Ledger), and last publishes the events they appended, so a caller's
// own events, published after Commit returns, follow them. When the commit returns an error its
// outcome is unknown, so the rooms the transaction wrote are failed and reload the durable
// document.
func (l *Ledger) Commit(ctx context.Context) error {
	if err := l.commit(ctx); err != nil {
		return err
	}
	l.publish()
	l.publishEvents()
	return nil
}

// publishEvents publishes the events this ledger's document operations appended, in the order
// they appended them.
func (l *Ledger) publishEvents() {
	for _, event := range l.events {
		l.service.events.Publish(event)
	}
}

// commit is Commit up to the publish. Another transaction can run between the two, and tests
// call them apart to hold that window open.
//
// Each version's capture's state is locked (state.mu) from before the commit until the in-flight
// credits the version listed are marked consumed: an append queued behind the document's advisory
// lock, which this commit holds until it returns, takes that lock the instant this commit drops
// it, and must find its credit consumed when it reads it (UpdateCredit.take), or it writes an
// author this version listed to the pending authors. The states are locked in their artifacts'
// sorted order, so two transactions versioning the same two documents cannot deadlock.
func (l *Ledger) commit(ctx context.Context) error {
	if err := l.recordSettlementCredit(ctx); err != nil {
		_ = l.tx.Rollback(context.Background())
		l.endRebuilds()
		l.fail(err)
		return err
	}
	versions := slices.Clone(l.versions)
	slices.SortStableFunc(versions, func(a, b ledgerVersion) int { return strings.Compare(a.artifactID, b.artifactID) })
	type lockedState struct {
		artifactID string
		state      *roomState
	}
	locked := make([]lockedState, 0, len(versions))
	for _, written := range versions {
		state := written.capture.state
		if !slices.ContainsFunc(locked, func(held lockedState) bool { return held.state == state }) {
			state.mu.Lock()
			locked = append(locked, lockedState{artifactID: written.artifactID, state: state})
		}
	}
	err := l.tx.Commit(ctx)
	// The transaction has ended whichever way the commit went, so a rebuild's room reads the
	// history the commit left from here.
	l.endRebuilds()
	if err == nil {
		for _, written := range l.versions {
			written.capture.consumeLocked()
		}
	}
	for _, held := range locked {
		l.service.unlockState(held.artifactID, held.state)
	}
	l.versions = nil
	if err != nil {
		l.fail(err)
		return err
	}
	l.creditRooms()
	return nil
}

// Discard drops what a transaction that did not commit left behind: its live writes, which no
// room ever saw, and the rooms its rebuilds held. A version it wrote marked nothing it read, so
// its authors stay where they were. After Commit it does nothing.
func (l *Ledger) Discard() {
	for _, artifactID := range l.order {
		l.service.finishLiveWrite(l.live[artifactID])
	}
	l.versions = nil
	l.endRebuilds()
}

func (l *Ledger) holdRebuild(artifactID string) {
	l.rebuilds = append(l.rebuilds, artifactID)
}

func (l *Ledger) endRebuilds() {
	for _, artifactID := range l.rebuilds {
		l.service.rebuilding.Delete(artifactID)
	}
	l.rebuilds = nil
}

// recordVersion records a version this transaction wrote, with whom it listed (capture), which
// its commit takes out of the pending authors (recordSettlementCredit, Ledger.commit). The version
// holds every change the transaction's write to the document has made so far and lists their
// authors, so the commit does not record them as pending again unless the write changes the
// document after it (creditLiveWrite).
func (l *Ledger) recordVersion(artifactID string, version model.Version, capture authorCapture) {
	l.versions = append(l.versions, ledgerVersion{artifactID: artifactID, version: version, capture: capture})
	if write := l.liveWriteFor(artifactID); write != nil {
		write.versioned = true
	}
}

// WroteVersion records version, of artifactID, which the caller wrote itself in this transaction,
// outside the document service, over the transaction's own write to the document - an upload,
// whose version lists its uploader alone. Once this transaction commits, it has cleared every
// author whose change the write's last read of the room held (liveWrite.forkSeq), whether its
// replacement removed that edit or kept it: every pending author row, since the write held the
// document's advisory lock from before that read (ReplaceText), and each in-flight credit observed
// by then. A credit observed after that read stays pending for the next version. An upload that
// changed nothing has no write to hold and nothing to clear.
func (l *Ledger) WroteVersion(artifactID string, version model.Version) {
	write := l.liveWriteFor(artifactID)
	if write == nil || len(write.updates) == 0 {
		return
	}
	state := write.state
	state.mu.Lock()
	inflight := state.unconsumedInflightLocked(write.forkSeq)
	state.mu.Unlock()
	l.recordVersion(artifactID, version, authorCapture{state: state, inflight: inflight, full: true})
}

func (l *Ledger) liveWriteFor(artifactID string) *liveWrite {
	if l == nil {
		return nil
	}
	return l.live[artifactID]
}

// LostOps reports which operations of this transaction's write to artifactID the live document
// no longer held once the write was published, which the caller reads after Commit. The second
// return is false when no verdict was reached - the transaction wrote nothing to that document,
// its publish failed and the room is reloading, or the room holds a tree past the schema's depth
// bound, which no read can serve - which the caller reports as undetermined rather than as
// survival (LEGION-269).
func (l *Ledger) LostOps(artifactID string) ([]int, bool) {
	write := l.liveWriteFor(artifactID)
	if write == nil || !write.lostVerdict {
		return nil, false
	}
	return write.lost, true
}

func (l *Ledger) addLiveWrite(write *liveWrite) {
	if l.live == nil {
		l.live = make(map[string]*liveWrite)
	}
	l.live[write.artifactID] = write
	l.order = append(l.order, write.artifactID)
}

// recordSettlementCredit records, in the transaction that wrote the documents, who owes what once
// it commits. It first deletes the pending authors each version this transaction wrote read (all
// of them for an upload's), then records each write's own authors, less those its own versions
// list, and each write's and seed's latest edit source on the document's pending-settlement row.
// Deleting first means the write's own authors are recorded after the delete that would otherwise
// take them out. Both commit or roll back with the content, before the request context can be
// canceled after commit.
func (l *Ledger) recordSettlementCredit(ctx context.Context) error {
	for _, written := range l.versions {
		if written.capture.full {
			if err := deleteAllPendingAuthors(ctx, l.tx, written.artifactID); err != nil {
				return err
			}
			continue
		}
		if err := deletePendingAuthors(ctx, l.tx, written.artifactID, written.capture.rKeys); err != nil {
			return err
		}
	}
	released := l.releasedAuthors()
	for artifactID, seed := range l.seeds {
		if _, listed := released[artifactID][actorKey(seed.actor)]; listed {
			continue
		}
		if err := markSettlementPending(ctx, l.tx, artifactID, &seed.actor, true, false); err != nil {
			return err
		}
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if len(write.credits) == 0 {
			continue
		}
		listed := released[artifactID]
		pending := write.credits
		if len(listed) > 0 {
			pending = make(map[string]model.Actor, len(write.credits))
			for key, credited := range write.credits {
				if _, done := listed[key]; !done {
					pending[key] = credited
				}
			}
		}
		if err := upsertPendingAuthors(ctx, l.tx, artifactID, pending); err != nil {
			return err
		}
		if err := markSettlementPending(ctx, l.tx, artifactID, write.actor, true, false); err != nil {
			return err
		}
	}
	return nil
}

// releasedAuthors is, per artifact, the keys of the authors this transaction's own versions
// listed, which the same transaction's own write does not record as pending again.
func (l *Ledger) releasedAuthors() map[string]map[string]struct{} {
	released := make(map[string]map[string]struct{}, len(l.versions))
	for _, written := range l.versions {
		keys := released[written.artifactID]
		if keys == nil {
			keys = make(map[string]struct{}, len(written.version.Authors))
			released[written.artifactID] = keys
		}
		for _, actor := range written.version.Authors {
			keys[actorKey(actor)] = struct{}{}
		}
	}
	return released
}

// creditRooms records, once the transaction has committed and before its live writes publish,
// each seed's and write's latest edit source in its room, which the settlement it arms names on
// its events, and registers the ask blocks each introduced to its author. The authors themselves
// are already recorded in the database (recordSettlementCredit).
func (l *Ledger) creditRooms() {
	for artifactID, seed := range l.seeds {
		state := l.service.lockState(artifactID)
		state.creditSeq.Add(1)
		state.lastActor = new(seed.actor)
		state.unsettled = true
		state.registerAskAuthors(askBlockIDs(seed.tree), seed.actor)
		l.service.unlockState(artifactID, state)
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if len(write.credits) == 0 {
			continue
		}
		state := l.service.lockState(artifactID)
		state.creditSeq.Add(1)
		state.lastActor = write.actor
		state.unsettled = true
		if write.actor != nil {
			// The carried-forward author of a renamed id is registered before the ids this
			// write's own before/after diff adds, so a rename's more specific registration is
			// never overwritten by the less specific one applyLive's diff would otherwise give
			// it (registerAskAuthors, registerCarriedAskAuthors).
			state.registerCarriedAskAuthors(write.carriedAskAuthors)
			state.registerAskAuthors(write.addedAskBlockIDs, *write.actor)
		}
		l.service.unlockState(artifactID, state)
	}
}

// seeded records that this transaction seeded artifactID's first text as actor (SeedText), with
// the tree it wrote, whose ask blocks credit registers to actor once the transaction commits.
func (l *Ledger) seeded(artifactID string, tree *pmdoc.Node, actor model.Actor) {
	if l.seeds == nil {
		l.seeds = make(map[string]ledgerSeed)
	}
	l.seeds[artifactID] = ledgerSeed{actor: actor, tree: tree}
}

// publish applies the committed transaction's live writes to their rooms and broadcasts them.
// Each update is already durable, so the room's own persistence of it is suppressed. A room that
// cannot take its update is failed, and reloads the durable document on its next access.
func (l *Ledger) publish() {
	for _, artifactID := range l.order {
		if write := l.live[artifactID]; !write.finished {
			l.service.publishLiveWrite(write)
		}
	}
}

// fail fails the rooms a transaction wrote when its commit returned an error. The commit may
// have gone through, so the rooms cannot tell whether they should hold the writes; failed, they
// reload the durable document, whichever way the commit went.
func (l *Ledger) fail(cause error) {
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if write.finished {
			continue
		}
		if len(write.updates) > 0 {
			l.service.failRoom(artifactID, fmt.Errorf("commit live document write: %w", cause))
		}
		l.service.finishLiveWrite(write)
	}
}
