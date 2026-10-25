// Package accesslog writes Dispatch's one line per served request.
//
// Dispatch kept no record of who called it: when its database saturated on 2026-10-25 nobody
// could name the callers behind the requests it was serving (dispatch://INFRA-324,
// dispatch://INFRA-351). Handler wraps one route and logs, when the request ends, the method, the
// route pattern (never a path's ids alone), the caller the authentication layer resolved, the
// status, the time it took and the bytes of body it wrote.
//
// The line never carries a token, a cookie, a request body or a response body: the only
// caller-written text in it is the request path, which is clipped to pathBytes.
package accesslog

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The caller kinds the line names, one per way Dispatch authenticates a request.
const (
	// KindUser is a signed-in person, named by their lowercase email.
	KindUser = "user"
	// KindSession is an agent session a bearer named in its request's actor.
	KindSession = "session"
	// KindToken is a personal agent token with no session named, by its owner's email.
	KindToken = "token"
	// KindService is a verified Kubernetes service-account token, by its subject.
	KindService = "service"
	// KindSharedToken is the deployment's shared agent token with no session named: it proves
	// no owner, so the line names none.
	KindSharedToken = "shared-token"
	// KindAnonymous is a request no credential was resolved for: a public route, or one
	// refused before its handler ran.
	KindAnonymous = "anonymous"
)

// Message is the line every served request writes, so a log reader greps one string.
const Message = "dispatch: request"

// pathBytes bounds the request path the line keeps, as the previous-token log bounds the caller
// fields it writes (auth.previousTokenCallerBytes): the path is the caller's own text.
const pathBytes = 256

// callerKey carries the request's recorder, which the authentication layer names the caller on.
type callerKey struct{}

// recorder is one request's response writer and its line's facts. It wraps the server's writer
// rather than replacing it, and carries through the three interfaces Dispatch's handlers take it
// for: Flusher for the event streams, Hijacker for the websocket upgrade, and ReaderFrom so an
// artifact download still streams through the server's own copy.
type recorder struct {
	http.ResponseWriter
	// mu guards kind and id alone: a stream's handler runs beside the goroutine that resolves
	// its caller again on every heartbeat (api.whileCallerResolves), and that goroutine can
	// name the caller while the handler is still writing.
	mu      sync.Mutex
	kind    string
	id      string
	status  int
	written int64
}

func (rec *recorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(body []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	written, err := rec.ResponseWriter.Write(body)
	rec.written += int64(written)
	return written, err
}

// ReadFrom keeps the server's own copy (sendfile, and its buffer reuse) for a handler that
// streams a body with io.Copy, such as an artifact download.
func (rec *recorder) ReadFrom(body io.Reader) (int64, error) {
	from, ok := rec.ResponseWriter.(io.ReaderFrom)
	if !ok {
		return io.Copy(rec.ResponseWriter, body)
	}
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	written, err := from.ReadFrom(body)
	rec.written += written
	return written, err
}

func (rec *recorder) Flush() {
	if flusher, ok := rec.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack serves the websocket upgrade, which writes its own answer on the connection it takes.
// The line records the upgrade as the 101 it is; the frames that follow are not its bytes.
func (rec *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rec.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, buffered, err := hijacker.Hijack()
	if err == nil && rec.status == 0 {
		rec.status = http.StatusSwitchingProtocols
	}
	return conn, buffered, err
}

// Unwrap gives http.ResponseController the server's own writer, so a handler that moves its
// connection's deadlines reaches it through this wrapper.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// Handler serves one route and logs the request when it ends. route is the route's own pattern
// (`/api/v1/issues/{key}`), mounted once, so the line groups a route's requests together whatever
// ids their paths carry.
func Handler(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, kind: KindAnonymous}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), callerKey{}, rec)))
		rec.write(route, r, time.Since(start))
	})
}

// HandlerFunc is Handler for a handler function, which is how every Dispatch route is written.
func HandlerFunc(route string, next http.HandlerFunc) http.HandlerFunc {
	handler := Handler(route, next)
	return handler.ServeHTTP
}

// Note records who the server took this request's sender for. The authentication layer calls it
// wherever it resolves a caller, and the last resolution wins: a bearer is noted by what its
// token proved and then by the session it named, which is the caller a reader wants.
func Note(ctx context.Context, kind, id string) {
	rec, ok := ctx.Value(callerKey{}).(*recorder)
	if !ok {
		return
	}
	rec.mu.Lock()
	rec.kind, rec.id = kind, id
	rec.mu.Unlock()
}

func (rec *recorder) write(route string, r *http.Request, elapsed time.Duration) {
	rec.mu.Lock()
	kind, id := rec.kind, rec.id
	rec.mu.Unlock()
	status := rec.status
	if status == 0 {
		// A handler that wrote nothing at all answered 200 with an empty body.
		status = http.StatusOK
	}
	slog.Info(Message,
		"method", r.Method,
		"route", route,
		"path", clipPath(r.URL.Path),
		"caller_kind", kind,
		"caller", id,
		"status", status,
		"elapsed_ms", elapsed.Milliseconds(),
		"bytes", rec.written,
	)
}

// clipPath cuts a path to pathBytes, dropping a rune the cut splits.
func clipPath(path string) string {
	if len(path) <= pathBytes {
		return path
	}
	return strings.ToValidUTF8(path[:pathBytes], "")
}
