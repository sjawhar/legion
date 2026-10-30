package docs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The conditional-edit queue: how many conditional edits of one document may be admitted and not
// yet released at once across every envoy-dispatch process serving the database
// (doc_conditional_edit_queue, which the admission reads under the document's queue lock), and the
// gate each document's edits take in turn in this process (AcquireConditionalEdit).

// maxQueuedConditionalEdits is how many conditional edits of one document may be admitted and not
// yet released at once, across every envoy-dispatch process serving the database: the one running
// on each process and those waiting behind it.
const maxQueuedConditionalEdits = 32

// conditionalEditQueueLockNamespace is the first key of the lock a conditional edit's admission
// takes ("QUEU"), its second the artifact id's hashtext. It is the two-key form, which Postgres
// keeps apart from the single-key locks (a document's room, lockDocumentRoom, among them), so an
// admission never waits on a document's writers; within the two-key space the other namespaces
// are the copy locks' (copiedAsksLockNamespace), the loop leases' (duty.leaseNamespace) and the
// issue dependencies' (api's lockProjectIssueDependencies), none of them this one.
const conditionalEditQueueLockNamespace int32 = 0x51554555

// conditionalEditReleaseTimeout bounds the write of a process's count when one of its conditional
// edits leaves the queue. It runs on a context of its own, as the request's may be cancelled by
// then, and a count that does not land only overstates this process's queue until its next write.
const conditionalEditReleaseTimeout = 5 * time.Second

// The statements of the document's queue across processes (doc_conditional_edit_queue, migration
// 0150). The admission's sum counts the members' rows other than this process's own, whose count
// it holds in memory, and deletes the rows of processes that are no members: one that died, or
// whose room-sync connection is down, counts for nothing until it is a member again, and writes
// its row back with its next admission.
const (
	conditionalEditQueueLock   = `select pg_advisory_xact_lock($1, hashtext($2))`
	conditionalEditQueueOthers = `
		with gone as (
			delete from doc_conditional_edit_queue
			where artifact_id = $1 and not (process_id::text = any($2::text[]))
		)
		select coalesce(sum(queued), 0)::int from doc_conditional_edit_queue
		where artifact_id = $1 and process_id::text = any($2::text[]) and process_id <> $3::uuid`
	conditionalEditQueueWrite = `
		insert into doc_conditional_edit_queue (artifact_id, process_id, queued) values ($1, $2, $3)
		on conflict (artifact_id, process_id) do update set queued = excluded.queued`
	conditionalEditQueueClear = `
		delete from doc_conditional_edit_queue where artifact_id = $1 and process_id = $2`
)

// conditionalEditGate is one document's conditional-edit queue in this process: the admission
// every edit takes in turn, the turn every admitted edit waits for, first come first served, and
// how many it has admitted.
type conditionalEditGate struct {
	// admit is held, by taking its one token, through one admission's whole transaction, so at
	// most one connection of this process waits on the document's queue lock however many
	// requests queue for it; the rest wait here, in memory, and leave when their context ends.
	admit chan struct{}
	turn  chan struct{}
	mu    sync.Mutex
	// users is the callers holding the gate - admitting, admitted, or writing the count their
	// release left - so the gate leaves conditionalGates only once none does.
	users int
	// queued is the edits admitted and not yet released, the one holding the turn included: this
	// process's row of doc_conditional_edit_queue.
	queued int
	// write orders this process's writes of queued to its row: each writer reads queued after
	// taking it, so the last write to land holds the count as it stands.
	write sync.Mutex
}

// full is whether this process's own queue of the document holds maxQueuedConditionalEdits,
// whatever the others hold.
func (g *conditionalEditGate) full() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.queued >= maxQueuedConditionalEdits
}

// AcquireConditionalEdit queues one conditional edit before its request acquires a database
// connection for its own transaction, keeping stale retries from exhausting pgx. The edits of one
// document are admitted one at a time in this process, each waiting in memory for the one before
// it, and take turns in the order they were admitted; at most maxQueuedConditionalEdits are
// admitted across every process at once, and one past that is refused with ErrPreconditionBusy.
// The admission is a short transaction of its own on the shared pool, so the caller must hold
// none of its connections (its request's transaction begins once this returns): under the
// document's queue lock it sums the counts of the other member processes (roomSyncMembers) and
// writes this process's own. A retry storm on one document therefore holds at most one shared
// connection per process while it waits for that lock. The returned release gives the turn to
// the next edit and writes this process's count again.
func (s *Service) AcquireConditionalEdit(ctx context.Context, artifactID string) (func(), error) {
	gate := s.joinConditionalGate(artifactID)
	if err := s.admitConditionalEdit(ctx, artifactID, gate); err != nil {
		s.leaveConditionalGate(artifactID, gate)
		return nil, err
	}
	select {
	case <-gate.turn:
		return func() { s.releaseConditionalEdit(ctx, artifactID, gate, true) }, nil
	case <-ctx.Done():
		s.releaseConditionalEdit(ctx, artifactID, gate, false)
		return nil, ctx.Err()
	}
}

// joinConditionalGate is artifactID's gate, counted as one more user.
func (s *Service) joinConditionalGate(artifactID string) *conditionalEditGate {
	for {
		created := &conditionalEditGate{admit: make(chan struct{}, 1), turn: make(chan struct{}, 1)}
		created.admit <- struct{}{}
		created.turn <- struct{}{}
		value, _ := s.conditionalGates.LoadOrStore(artifactID, created)
		gate := value.(*conditionalEditGate)
		gate.mu.Lock()
		if current, present := s.conditionalGates.Load(artifactID); present && current == gate {
			gate.users++
			gate.mu.Unlock()
			return gate
		}
		gate.mu.Unlock()
	}
}

// leaveConditionalGate ends one user of gate, and forgets the gate once it has none.
func (s *Service) leaveConditionalGate(artifactID string, gate *conditionalEditGate) {
	gate.mu.Lock()
	gate.users--
	if gate.users == 0 {
		s.conditionalGates.CompareAndDelete(artifactID, gate)
	}
	gate.mu.Unlock()
}

// admitConditionalEdit admits one more conditional edit of artifactID into gate's queue, or
// refuses it with ErrPreconditionBusy when the processes' queues hold maxQueuedConditionalEdits
// already. It first waits for gate's admission, in memory and until ctx ends, and holds it until
// its transaction ends, so this process's admissions of the document begin their transactions one
// at a time. Admissions of one document, on every process, then take its queue lock in turn, so
// each reads the counts the earlier ones wrote; a release writes without it and only ever lowers
// a count, so a sum read beside one can only overstate the queue.
func (s *Service) admitConditionalEdit(ctx context.Context, artifactID string, gate *conditionalEditGate) error {
	// Refused at once, without queuing for the admission, when this process's own queue is full.
	if gate.full() {
		return ErrPreconditionBusy
	}
	select {
	case <-gate.admit:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Deferred first, so it runs after the transaction's rollback has returned its connection.
	defer func() { gate.admit <- struct{}{} }()
	// The admissions this one waited behind may have filled this process's queue meanwhile.
	if gate.full() {
		return ErrPreconditionBusy
	}
	// Outside the commit step a keyed request's context carries (store.WithoutBeforeCommit): this
	// transaction writes the queue's bookkeeping and commits before the request's own write, so a
	// mark taken here would answer a repeat after a crash KEY_COMMITTED for a write nobody made.
	tx, err := s.store.Pool.Begin(store.WithoutBeforeCommit(ctx))
	if err != nil {
		return fmt.Errorf("begin the conditional edit queue's admission: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, conditionalEditQueueLock, conditionalEditQueueLockNamespace, artifactID); err != nil {
		return fmt.Errorf("lock the document's conditional edit queue: %w", err)
	}
	// write is taken before the sum, whose deletion takes this process's own row when it is no
	// member: a release writes that row holding write, so taking write after the row would leave
	// each waiting on the other until the release's timeout.
	gate.write.Lock()
	defer gate.write.Unlock()
	members, err := roomSyncMembers(ctx, tx)
	if err != nil {
		return err
	}
	var others int
	if err := tx.QueryRow(ctx, conditionalEditQueueOthers, artifactID, members, s.processID).Scan(&others); err != nil {
		return fmt.Errorf("count the other processes' conditional edits: %w", err)
	}
	gate.mu.Lock()
	queued := gate.queued
	gate.mu.Unlock()
	if others+queued >= maxQueuedConditionalEdits {
		// The deletion of the rows no member owns stands all the same.
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit the conditional edit queue's admission: %w", err)
		}
		return ErrPreconditionBusy
	}
	if _, err := tx.Exec(ctx, conditionalEditQueueWrite, artifactID, s.processID, queued+1); err != nil {
		return fmt.Errorf("write this process's conditional edits: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the conditional edit queue's admission: %w", err)
	}
	// Counted only once the count is durable, still holding write: a release that lowered queued
	// meanwhile writes after this, from queued as it then stands.
	gate.mu.Lock()
	gate.queued++
	gate.mu.Unlock()
	return nil
}

// releaseConditionalEdit takes one admitted edit out of gate's queue, gives the turn to the next
// edit when the leaving one held it, and writes this process's count again: the absolute number,
// without the queue lock, and the row's deletion once it is zero.
func (s *Service) releaseConditionalEdit(ctx context.Context, artifactID string, gate *conditionalEditGate, heldTurn bool) {
	gate.mu.Lock()
	gate.queued--
	gate.mu.Unlock()
	if heldTurn {
		gate.turn <- struct{}{}
	}
	defer s.leaveConditionalGate(artifactID, gate)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conditionalEditReleaseTimeout)
	defer cancel()
	gate.write.Lock()
	defer gate.write.Unlock()
	gate.mu.Lock()
	queued := gate.queued
	gate.mu.Unlock()
	var err error
	if queued == 0 {
		_, err = s.store.Pool.Exec(ctx, conditionalEditQueueClear, artifactID, s.processID)
	} else {
		_, err = s.store.Pool.Exec(ctx, conditionalEditQueueWrite, artifactID, s.processID, queued)
	}
	if err != nil {
		slog.Warn("dispatch: write this process's queued conditional edits of a document; the other processes count the last one written until the next",
			"artifact_id", artifactID, "queued", queued, "error", err)
	}
}
