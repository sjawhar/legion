package api

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
)

// An API transaction holds one pooled connection for its lifetime and can need a second one
// while it holds it: anchoring a comment or an ask stamps its mark in the live document, and a
// document whose room is cold loads from its own connection, independent of the writer's
// transaction by design — the room outlives the request and its updates must not roll back with
// it. Transactions that fill the pool therefore wait on connections only their peers can free by
// committing, and none of them can. The document settler avoids this by warming its room before
// it opens its transaction; an API writer cannot, because it resolves the document it will touch
// from rows it locks inside that transaction.
//
// So the pool, not the writers, is the budget: admit fewer concurrent write transactions than
// there are connections, leaving one for the admitted transaction that needs a second and one
// for the room loading and settlement that share the pool. Waiters here hold no connection.
const spareWriteConnections = 2

type writeSlots struct {
	slots chan struct{}
}

// admitWrite holds one write slot for the caller's transaction. The budget comes from the pool
// the server actually has, so the size is read on first use rather than at registration: a
// caller can construct a server around any store.
func (s *server) admitWrite(ctx context.Context) (func(), error) {
	s.sizeWriteSlots.Do(func() {
		capacity := int(s.deps.Store.Pool.Config().MaxConns) - spareWriteConnections
		if capacity < 1 {
			capacity = 1
		}
		s.writes = &writeSlots{slots: make(chan struct{}, capacity)}
	})
	return s.writes.admit(ctx)
}

// admit returns the release for one write slot, or the caller's cancellation while it waits.
func (w *writeSlots) admit(ctx context.Context) (func(), error) {
	select {
	case w.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-w.slots }) }, nil
}

// admittedTx returns its write slot when the transaction ends, whichever way it ends. Handlers
// commit and then run a deferred rollback, so the release is idempotent.
type admittedTx struct {
	pgx.Tx
	release func()
}

func (t *admittedTx) Commit(ctx context.Context) error {
	defer t.release()
	return t.Tx.Commit(ctx)
}

func (t *admittedTx) Rollback(ctx context.Context) error {
	defer t.release()
	return t.Tx.Rollback(ctx)
}
