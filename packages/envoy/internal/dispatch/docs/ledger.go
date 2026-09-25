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
// carries no `tx` and marks itself `settling` instead; a named version written outside any
// transaction collects its events in a ledger with neither.
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
	if err := l.tx.Commit(ctx); err != nil {
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
// room ever saw, and the author captures of the versions it wrote. After Commit it does nothing.
func (l *Ledger) Discard() {
	for _, artifactID := range l.order {
		l.service.finishLiveWrite(l.live[artifactID])
	}
	for _, written := range l.versions {
		l.service.discardPendingVersion(written.artifactID, written.version)
	}
	l.versions = nil
}

func (l *Ledger) recordVersion(artifactID string, version model.Version) {
	l.versions = append(l.versions, ledgerVersion{artifactID: artifactID, version: version})
}

func (l *Ledger) liveWriteFor(artifactID string) *liveWrite {
	if l == nil {
		return nil
	}
	return l.live[artifactID]
}

func (l *Ledger) addLiveWrite(write *liveWrite) {
	if l.live == nil {
		l.live = make(map[string]*liveWrite)
	}
	l.live[write.artifactID] = write
	l.order = append(l.order, write.artifactID)
}

// credit credits each content change of a committed transaction to its room, for the room's
// next version. It runs before the transaction's own versions are released, which clears the
// authors those versions already name.
func (l *Ledger) credit() {
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if len(write.credits) == 0 {
			continue
		}
		state := l.service.room(artifactID)
		state.mu.Lock()
		for key, actor := range write.credits {
			state.pending[key] = actor
		}
		state.lastActor = write.actor
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
