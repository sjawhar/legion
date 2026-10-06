package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Ledger is what the document operations joined to one database transaction produce until it
// ends: the events they generate, their writes to live documents (see liveWrite) and the versions
// they write. Join is the only way to join a transaction, so an operation that has one always has
// its ledger. Commit ends the transaction the way every caller must: it commits, credits the
// writes' authors to their rooms, releases the authors a version the transaction wrote named,
// and publishes the writes to their rooms, in that order. Discard, which callers defer right
// after Join, drops whatever a transaction that did not commit left behind.
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
	// capture is the whole capture rememberPendingVersion also stored for the room's own release
	// (commitVersionLocked), rather than copies of its creditSeq and fullRelease fields: recordVersion
	// and WroteVersion each already hold one capture of their own by the time they call
	// recordSettlementCredit's release, and this is that same value, not a second copy of two of
	// its fields.
	capture versionPending
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
// Every artifact a version in this transaction names is locked (state.mu) before the commit and
// held through that version's own in-memory release (commitVersionLocked): an append queued
// behind the document's advisory lock, which this commit also holds until it returns, can take
// that lock the instant this commit drops it. Without this, such an append could run
// creditContentChange between the durable release (inside this transaction, before commit) and
// commitVersionLocked (today, after it) and read the room's pending as it stood before the
// release, bundling an author this version already credited into its own snapshot - which the
// durable release's sequence watermark cannot catch, since that snapshot's own sequence is
// genuinely newer. Locking state.mu for the same span closes that window: no creditContentChange
// or captureAuthors call for this artifact can run until this whole commit, release included,
// has finished.
//
// l.versions names at most one artifact per transaction in every production and test path today
// (every writeVersionTx caller passes one artifactID; confirmed by reading each), so this loop's
// one entry never contends with itself. Artifact ids are still locked in sorted order, not
// insertion order, so a future caller that does join two artifacts' versions into one
// transaction cannot deadlock against another such transaction locking the same two artifacts in
// the opposite order.
func (l *Ledger) commit(ctx context.Context) error {
	if err := l.recordSettlementCredit(ctx); err != nil {
		_ = l.tx.Rollback(context.Background())
		l.endRebuilds()
		l.fail(err)
		return err
	}
	artifactIDs := make([]string, 0, len(l.versions))
	for _, written := range l.versions {
		artifactIDs = append(artifactIDs, written.artifactID)
	}
	slices.Sort(artifactIDs)
	artifactIDs = slices.Compact(artifactIDs)
	locked := make(map[string]*roomState, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		locked[artifactID] = l.service.lockState(artifactID)
	}
	err := l.tx.Commit(ctx)
	// The transaction has ended whichever way the commit went, so a rebuild's room reads the
	// history the commit left from here.
	l.endRebuilds()
	if err != nil {
		for artifactID, state := range locked {
			l.service.unlockState(artifactID, state)
		}
		l.fail(err)
		return err
	}
	l.creditLocked(locked)
	for _, written := range l.versions {
		commitVersionLocked(locked[written.artifactID], written.version)
	}
	for artifactID, state := range locked {
		l.service.unlockState(artifactID, state)
	}
	l.versions = nil
	return nil
}

// Discard drops what a transaction that did not commit left behind: its live writes, which no
// room ever saw, the author captures of the versions it wrote, and the rooms its rebuilds held.
// After Commit it does nothing.
func (l *Ledger) Discard() {
	for _, artifactID := range l.order {
		l.service.finishLiveWrite(l.live[artifactID])
	}
	for _, written := range l.versions {
		l.service.discardPendingVersion(written.artifactID, written.version)
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

// recordVersion records a version this transaction wrote, which its commit releases
// (commitVersionLocked). The version holds every change the transaction's write to the document
// has made so far and credits their authors, so the commit does not credit them again
// (creditLocked) unless the write changes the document after it (creditLiveWrite). capture's
// fullRelease carries through to the durable release (recordSettlementCredit) the same way it
// does to the room's (commitVersionLocked): an upload's version credits its uploader alone, but
// clears every pending author, not just the ones it names.
func (l *Ledger) recordVersion(artifactID string, version model.Version, capture versionPending) {
	l.versions = append(l.versions, ledgerVersion{artifactID: artifactID, version: version, capture: capture})
	if write := l.liveWriteFor(artifactID); write != nil {
		write.versioned = true
	}
}

// WroteVersion records version, of artifactID, which the caller wrote itself in this transaction,
// outside the document service, over the transaction's own write to the document - an upload,
// whose version credits its uploader alone. Once this transaction commits, it clears every author
// credited no later than the write's last read of the room (liveWrite.forkSeq), whether its
// replacement removed that edit or kept it; an edit credited after that read stays pending for
// the next version (commitVersionLocked, recordSettlementCredit). An upload that changed nothing
// has no write to hold and nothing to clear.
func (l *Ledger) WroteVersion(artifactID string, version model.Version) {
	write := l.liveWriteFor(artifactID)
	if write == nil || len(write.updates) == 0 {
		return
	}
	capture := versionPending{creditSeq: write.forkSeq, creditGeneration: write.forkGeneration, fullRelease: true}
	l.service.rememberPendingVersion(artifactID, version, capture)
	l.recordVersion(artifactID, version, capture)
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

// recordSettlementCredit releases, in the transaction that wrote the document, the authors each
// version this transaction wrote credited, as commitVersionLocked releases them from the room
// once the transaction commits, then adds each committed transaction's own credit to the
// pending-settlement row. Releasing first, before this transaction's own credit upserts run,
// means a full release's blanket per-entry sweep (releaseSettlementCredit) can never read an
// entry this same statement sequence is about to add below: it does not exist in the row yet.
// Each such entry's own persisted pending_seq is zero regardless - the sentinel
// settlementCreditFor's callers below already pass, which a release's own per-entry filter
// treats as exempt rather than literally "earliest" - so the order is a second guard here, not
// the only one: it is what protects a different, unrelated, later transaction's own release
// against an entry this transaction adds, after this transaction has already committed and
// released. The durable row therefore commits or rolls back with its content, before the request
// context can be canceled after commit, and owes no author a version already credited. An author
// the same transaction's own version immediately releases is never upserted at all: the version
// already records it durably, so paying an upsert the release above already undid is wasted
// work. Every upsert here leaves CreditSeq (settlementCredit's own aggregate field, distinct from
// each entry's own PendingSeq) at its zero value: this transaction's own credit and its own
// version's release are already ordered by the same transaction, so neither needs the watermark
// gate a concurrent reader's stale credit does (upsertSettlementCredit).
func (l *Ledger) recordSettlementCredit(ctx context.Context) error {
	for _, written := range l.versions {
		var authors []model.Actor
		if !written.capture.fullRelease {
			authors = written.version.Authors
		}
		if err := releaseSettlementCredit(ctx, l.tx, written.artifactID, authors, written.capture.creditSeq, written.capture.creditGeneration, written.capture.fullRelease); err != nil {
			return err
		}
	}
	released := l.releasedAuthors()
	for artifactID, seed := range l.seeds {
		if _, consumed := released[artifactID][actorKey(seed.actor)]; consumed {
			continue
		}
		// A seed's credit names only its lastActor, no pending authors (settlementCreditFor's
		// nil pending loop tags nothing), so it needs no generation of its own.
		if err := upsertSettlementCredit(ctx, l.tx, artifactID, settlementCreditFor(nil, &seed.actor, 0, 0), false); err != nil {
			return err
		}
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if len(write.credits) == 0 {
			continue
		}
		consumed := released[artifactID]
		pending := write.credits
		if len(consumed) > 0 {
			pending = make(map[string]model.Actor, len(write.credits))
			for key, credited := range write.credits {
				if _, done := consumed[key]; !done {
					pending[key] = credited
				}
			}
		}
		lastActor := write.actor
		if lastActor != nil {
			if _, done := consumed[actorKey(*lastActor)]; done {
				lastActor = nil
			}
		}
		if len(pending) == 0 && lastActor == nil {
			continue
		}
		if err := upsertSettlementCredit(ctx, l.tx, artifactID, settlementCreditFor(pending, lastActor, 0, write.state.creditGeneration.Load()), false); err != nil {
			return err
		}
	}
	return nil
}

// withState runs fn against artifactID's state: the one commit already locked, if artifactID is
// among locked, or a freshly locked-and-released one otherwise. creditLocked's two loops both
// need this branching, since only the artifacts this transaction's own versions name are
// pre-locked (Ledger.commit) - an artifact with credit but no version in this transaction has no
// race to guard and is locked only for the duration of fn.
func (l *Ledger) withState(locked map[string]*roomState, artifactID string, fn func(*roomState)) {
	state, preLocked := locked[artifactID]
	if !preLocked {
		state = l.service.lockState(artifactID)
	}
	fn(state)
	if !preLocked {
		l.service.unlockState(artifactID, state)
	}
}

// releasedAuthors is, per artifact, the keys of the authors this transaction's own versions
// credited: what their release takes out of the room and the pending-settlement row, so neither
// the row nor the room is credited them again by the same transaction's own write.
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

// creditLocked mirrors committed settlement credit into each room before the transaction's
// versions release their captured authors and before its live writes publish. locked holds the
// states commit already locked for this transaction's own versions, keyed by artifact id; an
// artifact this loop touches that is not among them is locked and unlocked here as before
// (withState). An author this transaction's own version credits is not made owed, as
// recordSettlementCredit does not upsert it: the version holds that credit, and a credit made here
// would carry a creditSeq past the version's own capture, which its release then leaves owed.
func (l *Ledger) creditLocked(locked map[string]*roomState) {
	for artifactID, seed := range l.seeds {
		l.withState(locked, artifactID, func(state *roomState) {
			state.lastActor = new(seed.actor)
			state.unsettled = true
			state.creditSeq.Add(1)
			state.registerAskAuthors(askBlockIDs(seed.tree), seed.actor)
		})
	}
	released := l.releasedAuthors()
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if len(write.credits) == 0 {
			continue
		}
		l.withState(locked, artifactID, func(state *roomState) {
			state.creditSeq.Add(1)
			creditSeq := state.creditSeq.Load()
			for key, actor := range write.credits {
				if _, consumed := released[artifactID][key]; !consumed {
					state.creditPendingLocked(key, actor, creditSeq)
				}
			}
			state.lastActor = write.actor
			state.unsettled = true
			if write.actor != nil {
				// The carried-forward author of a renamed id is registered before the ids this
				// write's own before/after diff adds, so a rename's more specific registration
				// is never overwritten by the less specific one applyLive's diff would otherwise
				// give it (registerAskAuthors, registerCarriedAskAuthors).
				state.registerCarriedAskAuthors(write.carriedAskAuthors)
				state.registerAskAuthors(write.addedAskBlockIDs, *write.actor)
			}
		})
	}
}

// commitVersionLocked takes a version's captured authors out of its artifact's room, given a state
// the caller already holds locked (state.mu): Ledger.commit locks every version's artifact before
// it commits and calls this, still holding it, right after, so the room's pending map loses a
// version's authors no later than the transaction that released them from the durable row - the
// two critical sections, this lock and the document's advisory lock the same commit held, start
// and end together.
func commitVersionLocked(state *roomState, version model.Version) {
	capture, ok := state.pendingVersions[version.Number]
	if !ok {
		return
	}
	delete(state.pendingVersions, version.Number)
	if state.roomGeneration != capture.roomGeneration {
		return
	}
	if capture.fullRelease {
		state.releaseAllPendingLocked(capture.creditSeq)
	} else {
		state.releasePendingLocked(capture.authors, capture.creditSeq)
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
