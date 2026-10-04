package docs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	// rebuilds are the documents this transaction rebuilds (RebuildDocument), whose rooms refuse
	// loads until it ends, committed or not.
	rebuilds []string
}

type ledgerVersion struct {
	artifactID string
	version    model.Version
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
func (l *Ledger) commit(ctx context.Context) error {
	err := l.tx.Commit(ctx)
	// The transaction has ended whichever way the commit went, so a rebuild's room reads the
	// history the commit left from here.
	l.endRebuilds()
	if err != nil {
		l.fail(err)
		return err
	}
	l.credit()
	for _, written := range l.versions {
		l.service.commitVersion(written.artifactID, written.version)
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
// (commitVersion). The version holds every change the transaction's write to the document has
// made so far and credits their authors, so the commit does not credit them again (credit) unless
// the write changes the document after it (creditLiveWrite).
func (l *Ledger) recordVersion(artifactID string, version model.Version) {
	l.versions = append(l.versions, ledgerVersion{artifactID: artifactID, version: version})
	if write := l.liveWriteFor(artifactID); write != nil {
		write.versioned = true
	}
}

// WroteVersion records version, of artifactID, which the caller wrote itself in this transaction,
// outside the document service, over the transaction's own write to the document - an upload,
// whose version credits its uploader alone. Once this transaction commits, it clears every author
// credited no later than the write's last read of the room (liveWrite.forkSeq), whether its
// replacement removed that edit or kept it; an edit credited after that read stays pending for
// the next version (commitVersion). An upload that changed nothing has no write to hold and
// nothing to clear.
func (l *Ledger) WroteVersion(artifactID string, version model.Version) {
	write := l.liveWriteFor(artifactID)
	if write == nil || len(write.updates) == 0 {
		return
	}
	l.service.rememberPendingVersion(artifactID, version, versionPending{through: write.forkSeq})
	l.recordVersion(artifactID, version)
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

// credit credits each content change of a committed transaction to its room, for the room's
// next version. It also registers the ask ids the write introduced to its actor
// (registerAskAuthors) and the author carried forward for each one a stamp of the write's own
// renamed (registerCarriedAskAuthors), before this commit's own publish can reach any observer,
// so attribution does not depend on whichever update's observer ends up rendering the publish
// first (LEGION-503). It runs before the transaction's own versions are released, which clears
// the authors those versions already name.
func (l *Ledger) credit() {
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if len(write.credits) == 0 {
			continue
		}
		state := l.service.room(artifactID)
		state.mu.Lock()
		if !write.versioned {
			for key, actor := range write.credits {
				state.creditSeq++
				state.pending[key] = pendingAuthor{actor: actor, seq: state.creditSeq}
			}
		}
		state.lastActor = write.actor
		state.registerAskAuthors(write.addedAskBlockIDs, *write.actor)
		state.registerCarriedAskAuthors(write.carriedAskAuthors)
		state.mu.Unlock()
	}
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
