package main

import (
	"context"
	"net"
	"net/http"
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
// closes the connection once that response is written. A request carrying an Upgrade header (a
// websocket handshake) keeps its connection, since the connection is what it upgrades, and a
// request whose context holds no accept time (connAcceptTime not wired) passes through untouched.
func maxConnAgeHandler(next http.Handler, maxAge time.Duration, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accepted, ok := r.Context().Value(connAcceptTimeKey{}).(time.Time); ok &&
			r.Header.Get("Upgrade") == "" && now().Sub(accepted) >= maxAge {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}

// newHTTPServer is the server main runs: handler behind the connection age limit, with every
// connection stamped with its accept time as now reads it.
func newHTTPServer(handler http.Handler, now func() time.Time) *http.Server {
	return &http.Server{
		Handler:           maxConnAgeHandler(handler, maxConnAge, now),
		ConnContext:       connAcceptTime(now),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// WriteTimeout remains zero because the event stream is long-lived.
	}
}
