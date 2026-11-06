package main

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxConnAge is how old a client connection may grow before the server closes it after its next
// response. kube-proxy and the front door spread load per connection, not per request, so while
// callers keep reusing their keep-alive connections, a process that has just rolled in gets
// little of the traffic. Closing a connection once it is this old makes the client open a new
// one, which kube-proxy spreads across every ready process.
const maxConnAge = 60 * time.Second

// connAcceptTimeKey is the context key under which connAcceptTime stores a connection's accept
// time.
type connAcceptTimeKey struct{}

// connAcceptTime is an http.Server ConnContext that stamps each accepted connection's context with
// the time now reads when the connection is accepted, which every request on it inherits.
func connAcceptTime(now func() time.Time) func(ctx context.Context, c net.Conn) context.Context {
	return func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, connAcceptTimeKey{}, now())
	}
}

// maxConnAgeHandler sets "Connection: close" on the response to a request whose connection has
// been open maxAge or longer (age >= maxAge), before next writes anything; net/http's server then
// closes the connection once that response is written. A websocket handler hijacks the connection
// before it writes its own 101 response, so this header is never sent on that connection. A request
// whose context holds no accept time (connAcceptTime not wired) passes through untouched.
func maxConnAgeHandler(next http.Handler, maxAge time.Duration, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accepted, ok := r.Context().Value(connAcceptTimeKey{}).(time.Time); ok &&
			now().Sub(accepted) >= maxAge {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}

// newHTTPServer is the server main runs: handler behind the connection age limit and the
// connection watch, with every connection stamped with its accept time as now reads it and
// counted by conns, and net/http's own error lines in the process's log.
//
// ReadTimeout and WriteTimeout remain zero: both are per connection, and this server's longest
// connections are the websockets (/ws/doc, /ws/live), which a server timeout would cut mid-stream.
// What they would bound is bounded per request instead, where a handler can tell a stream from a
// request: a request's body arrives, and a read's answer goes out, under api's own transfer
// bounds - no silence longer than 20 s and 32 KiB/s after a minute's grace - as a read deadline
// the API pushes out as the body arrives (api.drainedBody) and a write deadline it pushes out as
// the answer goes out (api.boundedAnswer).
func newHTTPServer(handler http.Handler, now func() time.Time, conns *conns) *http.Server {
	accepted := connAcceptTime(now)
	server := &http.Server{
		Handler:     conns.watch(maxConnAgeHandler(handler, maxConnAge, now)),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context { return conns.opened(accepted(ctx, c), c) },
		ConnState:   conns.state,
		// net/http writes its own errors - a request head it could not read, a response it
		// could not write, a handler that panicked - to this logger; with none set they went
		// to the standard logger's stderr, outside the process's log stream and counted
		// nowhere, which is why the silent closes of INFRA-592 left no line of any kind.
		ErrorLog:          log.New(serverErrorWriter{}, "", 0),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	// Every connection closes after its answer once shutdown begins, net/http saying so itself
	// rather than any handler of ours, so a drain is no cause to count anything (conns.drain).
	server.RegisterOnShutdown(conns.drain)
	return server
}

// serverErrorWriter is where net/http's own error lines go: one WARN record a line, in the
// process's log with everything else.
type serverErrorWriter struct{}

func (serverErrorWriter) Write(line []byte) (int, error) {
	slog.Warn("dispatch: the HTTP server reported an error", "error", strings.TrimRight(string(line), "\n"))
	return len(line), nil
}

// A connection the server closes after reading a request on it, without the client having been
// told the connection was going, costs that client the request it writes onto the connection
// next: it takes the connection from its idle pool, writes the request in full and reads an EOF
// with no status, while the server never dispatched that request to a handler and so wrote no
// access line for it. Four writes of the round-22 load test ended that way with nothing on
// either side to say which connection they rode or why it went (INFRA-592). These counts name
// the causes the server can tell apart, so such a close is visible in a metric and in a line
// rather than only in the client's EOF.
const (
	// connRequestUnanswered is a connection closed with a request read on it that no handler
	// of ours answered: net/http never dispatched it - a request head it could not read, the
	// header read deadline, a connection that broke while the head arrived - or refused it
	// itself with a 400 or a 431 of its own.
	connRequestUnanswered connCloseReason = "request_unanswered"
	// connHandlerAborted is a connection closed because the handler of the request on it
	// aborted rather than returned: an answer whose write failed or fell short, which
	// api.servedAsRead ends with http.ErrAbortHandler because its header is already on the
	// wire and cannot be made to say the connection is closing.
	connHandlerAborted connCloseReason = "handler_aborted"
	// connAnswerNotClosing is a connection closed after an answer that did not say so: the
	// answer went out whole and carried no Connection: close, and net/http dropped the
	// connection anyway (its shouldReuseConnection: a write that failed, a body left on the
	// connection, a Content-Length it did not fill). This is the cause a client reads as an
	// EOF on its next request.
	connAnswerNotClosing connCloseReason = "answer_not_closing"
)

// connCloseReason names why a connection closed with its client told nothing.
type connCloseReason string

// connCloseReasons are the causes, in the order the metric lists them.
var connCloseReasons = []connCloseReason{connRequestUnanswered, connHandlerAborted, connAnswerNotClosing}

// connRecordKey is the context key under which conns.opened puts a connection's record, which
// every request on that connection inherits.
type connRecordKey struct{}

// connRecord is what the server knows about one open connection: when it was accepted, how many
// requests have run on it, and what the request running on it now has done. Everything in it is
// written by that connection's own goroutine - net/http reports a connection's states and runs
// its handler there - and read by it, under conns' lock, which the metrics scrape and other
// connections also take.
type connRecord struct {
	accepted time.Time
	requests int
	// active is set while net/http has read a request on the connection and not yet put it
	// back in its idle state (StateActive to StateIdle), which is the window a close leaves a
	// client with no answer to read.
	active bool
	// ran is set once a handler has run on the request now active, returned once that handler
	// returned rather than aborted, and closing when the answer told the client the connection
	// is closing.
	ran      bool
	returned bool
	closing  bool
}

// conns watches the server's connections: it counts the ones that close with their client told
// nothing, by cause, and logs each at WARN. Its map holds one record per open connection, put
// there when the connection is accepted (opened) and taken out when it closes or is hijacked
// (state), so it holds no more than the connections the server has open.
type conns struct {
	mu   sync.Mutex
	open map[net.Conn]*connRecord
	// draining is set once shutdown begins, after which no close is counted: net/http answers
	// every request with Connection: close while it drains and closes the connection after it,
	// which is the shedding a rolling deploy asks for and not a client left with nothing.
	draining bool
	closed   map[connCloseReason]*atomic.Int64
}

func newConns() *conns {
	watched := &conns{open: map[net.Conn]*connRecord{}, closed: map[connCloseReason]*atomic.Int64{}}
	for _, reason := range connCloseReasons {
		watched.closed[reason] = &atomic.Int64{}
	}
	return watched
}

// ClosedUnanswered is how many connections have closed for each cause, in connCloseReasons'
// order, which the metrics endpoint serves as dispatch_http_conns_closed_unanswered_total.
func (c *conns) ClosedUnanswered() []ConnCloseCount {
	counts := make([]ConnCloseCount, 0, len(connCloseReasons))
	for _, reason := range connCloseReasons {
		counts = append(counts, ConnCloseCount{Reason: string(reason), Count: c.closed[reason].Load()})
	}
	return counts
}

// ConnCloseCount is one cause and how many connections have closed for it.
type ConnCloseCount struct {
	Reason string
	Count  int64
}

// opened records the connection and puts its record on the context every request on it
// inherits, which is how the handler reaches the record net/http's state hooks name by
// connection (watch).
func (c *conns) opened(ctx context.Context, conn net.Conn) context.Context {
	record := &connRecord{}
	if accepted, ok := ctx.Value(connAcceptTimeKey{}).(time.Time); ok {
		record.accepted = accepted
	}
	c.mu.Lock()
	c.open[conn] = record
	c.mu.Unlock()
	return context.WithValue(ctx, connRecordKey{}, record)
}

// drain stops the counting, which shutdown does before it closes anything.
func (c *conns) drain() {
	c.mu.Lock()
	c.draining = true
	c.mu.Unlock()
}

// state takes net/http's report of a connection's state. StateActive opens the window a close
// leaves a client with nothing - net/http sets it once it has read bytes of a request, whether
// or not that request parses - and StateIdle closes it, the answer written and the connection
// kept. StateClosed judges the window the connection died in, and StateHijacked drops the
// record: a websocket's connection is its handler's from there, and net/http reports no state
// of it again.
func (c *conns) state(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	record := c.open[conn]
	if record == nil {
		c.mu.Unlock()
		return
	}
	var closed connCloseReason
	var requests int
	var age time.Duration
	switch state {
	case http.StateActive:
		record.active, record.ran, record.returned, record.closing = true, false, false, false
	case http.StateIdle:
		record.active = false
	case http.StateHijacked:
		delete(c.open, conn)
	case http.StateClosed:
		delete(c.open, conn)
		closed = record.closeUntold(c.draining)
		requests, age = record.requests, time.Since(record.accepted)
	}
	c.mu.Unlock()
	if closed == "" {
		return
	}
	c.closed[closed].Add(1)
	slog.Warn("dispatch: closed a connection whose client was told nothing",
		"reason", string(closed), "requests", requests, "age", age.Round(time.Millisecond))
}

// closeUntold names the cause of this close where the client was told nothing of it, and "" where
// the client has nothing to be told: a connection closed while idle or before its first request
// (its client may reuse nothing it has not been answered on), one whose answer said the
// connection was closing, and every close during a drain.
func (r *connRecord) closeUntold(draining bool) connCloseReason {
	switch {
	case draining || !r.active:
		return ""
	case !r.ran:
		return connRequestUnanswered
	case !r.returned:
		return connHandlerAborted
	case !r.closing:
		return connAnswerNotClosing
	}
	return ""
}

// watch records what each request does to its connection: that a handler ran on it, and, once
// that handler has returned, whether its answer told the client the connection is closing. A
// handler that aborts - api.servedAsRead's http.ErrAbortHandler for an answer it could not write
// whole - never reaches the second, which is how a cut answer is told from one that went out.
//
// It wraps the connection age limit rather than sitting inside it, so the header it reads is the
// one every layer has had its say on: the age limit's own Connection: close, the drain's on a
// body still on the connection, and what a refusal that replaced an answer kept of them
// (api.drainingWriter.replaceAnswer).
func (c *conns) watch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record, _ := r.Context().Value(connRecordKey{}).(*connRecord)
		c.began(record)
		next.ServeHTTP(w, r)
		c.answered(record, w.Header(), r)
	})
}

func (c *conns) began(record *connRecord) {
	if record == nil {
		return
	}
	c.mu.Lock()
	record.ran = true
	record.requests++
	c.mu.Unlock()
}

func (c *conns) answered(record *connRecord, header http.Header, r *http.Request) {
	if record == nil {
		return
	}
	c.mu.Lock()
	record.returned = true
	record.closing = closesConnection(header, r)
	c.mu.Unlock()
}

// closesConnection reports whether the client has been told this connection carries no further
// request: the answer says Connection: close, the request itself asked for the close, or the
// request is older than HTTP/1.1, which reuses no connection it was not told to.
func closesConnection(header http.Header, r *http.Request) bool {
	if r.Close || !r.ProtoAtLeast(1, 1) {
		return true
	}
	for _, value := range header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "close") {
				return true
			}
		}
	}
	return false
}
