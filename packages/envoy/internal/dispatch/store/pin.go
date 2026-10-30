package store

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PinConnection returns a context on which every call to the request pool runs on one connection:
// the first call takes it, each later one runs on it instead of taking another, and the returned
// end gives it back to the pool. The API pins each ordinary read (api.serveRoute), which reads
// through a dozen statements one after another - an issue read through twelve, a page of its
// comments through four - and made each of them wait for a connection of its own: under load
// every statement queued behind every other request's, so a read waited a dozen times for
// connections it never held two of at once. Pinned, a read waits once.
//
// It holds no connection admission does not already count. Admission takes a read's unit for the
// read's whole length and counts each unit as one request-pool connection (admission's read
// limit), so a read holding its connection from its first statement to its end is the
// connection admission set aside for it; and a pinned connection between statements holds no
// lock and no transaction, so nothing waits on it but its own request. A route whose reads run
// side by side (weight two) or whose request runs on without the database for minutes (a file
// download, an event stream) is not pinned, and neither is a write: a write waits for a
// document's writer gate between its statements, which a caller holding a connection must never
// do (ConnectionHeld).
//
// The nested-acquire guard holds on the pinned connection as on any other. A pinned connection
// runs one call at a time: a call that arrives while another is still running on it - a statement
// under the request's own open cursor or transaction, or one from a goroutine sharing the
// request's context while another statement runs - is refused with ErrNestedAcquire, as a
// second connection would be, rather than waiting on its own connection or interleaving two
// statements on it. A read that must run its statements side by side gives each its own context
// with ForConcurrentRead before the first one runs; each of those takes a connection of its own.
//
// Only the request pool pins, and it is the only pool a pinned request may take a connection of
// while it holds its own: a call on any other guarded pool (Background, Live, Settlements,
// Outbox) under a held pin is refused with ErrNestedAcquire, since it would be a second
// connection of these pools held at once. A call after end, and one made before the pin's first
// call, takes a connection for itself as without a pin, and a context already holding a
// connection is returned as it is.
func PinConnection(ctx context.Context) (context.Context, func()) {
	if ConnectionHeld(ctx) {
		return ctx, func() {}
	}
	pin := &pinnedConnection{}
	return context.WithValue(ctx, holdingKey{}, &holding{pin: pin}), pin.end
}

// pinnedConnection is the connection a pinned context's calls run on (PinConnection).
type pinnedConnection struct {
	mu sync.Mutex
	// pool is the pool the connection is taken from, set by the first call: a call on another
	// pool sharing the name - a second store's - is not served from it.
	pool *Pool
	// conn is the pinned connection, nil before the first call, once Acquire has handed it over,
	// and once it has gone back to the pool.
	conn *pgxpool.Conn
	// inUse is whether a call is running on the connection; ended whether the request has ended,
	// after which the connection goes back to the pool as soon as no call runs on it.
	inUse bool
	ended bool
}

// pinned is the pinned connection of state, a context's hold mark, when p is the request pool and
// the context pins one, else nil.
func (p *Pool) pinned(state *holding) *pinnedConnection {
	if p.name != RequestsPool || state == nil {
		return nil
	}
	return state.pin
}

// take hands the pinned connection to one call on pool, taking it from the pool first when the
// request holds none yet or the one it holds has closed - a statement cancelled by its context
// closes its connection - and returns the done that gives it back once the call is over. It
// answers ErrNestedAcquire while another call runs on it, an acquire error as ErrPoolBusy where it
// is one (Pool.busy), and a nil connection when the pin does not serve pool - it has ended, or
// it pinned another pool - so the call takes a connection for itself.
func (c *pinnedConnection) take(ctx context.Context, pool *Pool) (*pgxpool.Conn, func(), error) {
	c.mu.Lock()
	if c.ended || (c.pool != nil && c.pool != pool) {
		c.mu.Unlock()
		return nil, nil, nil
	}
	if c.inUse {
		c.mu.Unlock()
		return nil, nil, ErrNestedAcquire
	}
	c.inUse = true
	conn := c.conn
	c.mu.Unlock()
	// While inUse is set nothing else reads or replaces conn, so the acquire runs unlocked: a
	// call arriving meanwhile is refused at once rather than waiting the acquire out.
	if conn == nil || conn.Conn().IsClosed() {
		if conn != nil {
			conn.Release()
		}
		acquired, err := pool.pool.Acquire(ctx)
		c.mu.Lock()
		c.conn = acquired
		if err == nil {
			c.pool = pool
		}
		c.mu.Unlock()
		if err != nil {
			c.giveBack()
			return nil, nil, pool.busy(ctx, err)
		}
		conn = acquired
	}
	return conn, sync.OnceFunc(c.giveBack), nil
}

// giveBack ends the call running on the connection, and returns the connection to the pool if the
// request has ended meanwhile.
func (c *pinnedConnection) giveBack() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inUse = false
	if c.ended {
		c.releaseLocked()
	}
}

// handOver gives the pinned connection to Acquire's caller, who releases it to the pool; the
// request's next call takes another. It answers nil when the request holds none (or holds a closed
// one, which it releases), and ErrNestedAcquire while a call runs on it.
func (c *pinnedConnection) handOver(pool *Pool) (*pgxpool.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended || (c.pool != nil && c.pool != pool) {
		return nil, nil
	}
	if c.inUse {
		return nil, ErrNestedAcquire
	}
	conn := c.conn
	c.conn = nil
	if conn != nil && conn.Conn().IsClosed() {
		conn.Release()
		return nil, nil
	}
	return conn, nil
}

// end is the request's end: the connection goes back to the pool now, or when the call running on
// it gives it back, and every later call takes a connection for itself.
func (c *pinnedConnection) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = true
	if !c.inUse {
		c.releaseLocked()
	}
}

func (c *pinnedConnection) releaseLocked() {
	if c.conn != nil {
		c.conn.Release()
		c.conn = nil
	}
}

// held reports whether the request holds its pinned connection, idle or running a call. A nil
// pin holds none.
func (c *pinnedConnection) held() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.ended && (c.inUse || c.conn != nil)
}
