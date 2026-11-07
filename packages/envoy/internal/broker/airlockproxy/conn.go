// packages/envoy/internal/broker/airlockproxy/conn.go
// The four connection pieces every CONNECT on the proxy port goes through: the listener that
// hands a decrypted connection to the inner server, the bytes a client pipelined behind its
// CONNECT, the idle limit, and the registry of hijacked connections the drain closes.
package airlockproxy

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// chanListener is how a connection the outer server hijacked reaches the inner one: a listener
// with no socket of its own, whose Accept answers whatever serveConnect hands it. The inner
// http.Server is an ordinary server on it, so every request on a bound host is parsed, bounded
// and answered by net/http rather than by anything written here.
type chanListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close ends the inner server's Accept loop. It is idempotent: the drain closes it, and a
// hand-over racing that close reads the same channel and closes its own connection instead.
func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Addr is what the inner server logs and nothing else reads: these connections are hijacked
// from the outer listener and this one owns no address.
func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{} }

// hand gives c to the inner server, and answers false when the drain has closed the listener,
// in which case the caller owns c and closes it.
func (l *chanListener) hand(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.closed:
		return false
	}
}

// prefixConn reads rest before the connection under it. A client that wrote its first bytes -
// a TLS ClientHello, or a tunnel's first record - without waiting for the proxy's "200
// Connection established" leaves them in the bufio.Reader net/http hands back with the hijacked
// connection, where neither tls.Server nor a copy loop would ever see them.
type prefixConn struct {
	net.Conn
	rest *bytes.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if c.rest.Len() > 0 {
		return c.rest.Read(b)
	}
	return c.Conn.Read(b)
}

// idleConn closes the connection under it once no byte has crossed it in either direction for
// idle. It wraps the client's own connection, through which every byte of a tunnel and every
// byte of a decrypted connection passes in both directions, so one wrapper bounds both: a read
// is a byte from the command and a write is a byte on its way back to it.
//
// The clock is an atomic store per read and per write - no syscall, no lock, no deadline reset -
// read by one timer that re-arms itself for what is left of the window. So a connection moving
// bytes costs two atomic stores per buffer and at most two timer wakeups per idle window.
type idleConn struct {
	net.Conn
	idle time.Duration
	last atomic.Int64 // the last byte's time, UnixNano

	// The timer and the closed flag are the one piece of shared state the timer's own
	// goroutine and the connection's touch. The lock is held over the timer's first arming
	// too, so the goroutine that fires it cannot read the field before it is written.
	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

func newIdleConn(c net.Conn, idle time.Duration) net.Conn {
	if idle <= 0 {
		return c
	}
	ic := &idleConn{Conn: c, idle: idle}
	ic.last.Store(time.Now().UnixNano())
	ic.mu.Lock()
	ic.timer = time.AfterFunc(idle, ic.check)
	ic.mu.Unlock()
	return ic
}

// check ends the connection when the window has run out, and otherwise waits out what is left of
// it. Reading the clock rather than resetting the timer on every byte is what keeps the cost of
// a busy connection to an atomic store.
func (c *idleConn) check() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if since := time.Since(time.Unix(0, c.last.Load())); since < c.idle {
		c.timer.Reset(c.idle - since)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.Close()
}

func (c *idleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *idleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *idleConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.timer.Stop()
	c.mu.Unlock()
	return c.Conn.Close()
}

// bothEnds closes a tunnel's two connections as one, so the registry holds one entry per tunnel
// and the drain closes the command's side and the service's side together.
type bothEnds struct{ client, upstream io.Closer }

func (b bothEnds) Close() error {
	err := b.client.Close()
	if upstreamErr := b.upstream.Close(); err == nil {
		err = upstreamErr
	}
	return err
}

// track registers c as a connection the drain must close. http.Server.Shutdown neither closes a
// hijacked connection nor waits for one, and every tunnel and every bound connection of this
// proxy is hijacked out of the outer server, so without this registry a drain would return with
// the command's connections still open and the pod still holding its grant's values.
func (s *Server) track(c io.Closer) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	s.open[c] = struct{}{}
}

// forget drops a connection that has closed on its own.
func (s *Server) forget(c io.Closer) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	delete(s.open, c)
}

// closeOpen closes every connection the registry holds and empties it.
func (s *Server) closeOpen() {
	s.openMu.Lock()
	open := make([]io.Closer, 0, len(s.open))
	for c := range s.open {
		open = append(open, c)
	}
	clear(s.open)
	s.openMu.Unlock()
	for _, c := range open {
		c.Close()
	}
}
