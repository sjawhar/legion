// Package accesslog writes Dispatch's one line per served request.
//
// Dispatch kept no record of who called it: when its database saturated nobody could name the
// callers behind the requests it was serving. Handler wraps one route and logs, when the request
// ends, the method, the route pattern (never a path's ids alone), the caller the authentication
// layer resolved and the credential behind it, the address and user agent it came from, the
// status, the time it took and the bytes of body it wrote.
//
// The line never carries a token, a cookie, a query string, a request body or a response body.
// The text in it a caller wrote - the method, the path, the session id a bearer names, the
// address a proxy forwarded and the user agent - is each clipped to fieldBytes.
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

	"github.com/sjawhar/envoy/internal/dispatch/auth"
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

// Message is the line every served request writes. Match it whole (msg="dispatch: request"):
// the previous-shared-token warning's message starts with the same words.
const Message = "dispatch: request"

// fieldBytes bounds the caller-written text the line keeps (the request path and a named
// session's id), as the previous-token log bounds the caller fields it writes
// (auth.previousTokenCallerBytes).
const fieldBytes = 256

// callerKey carries the request's recorder, which the authentication layer names the caller on.
// A nil *recorder under it masks the request's recorder (Detached).
type callerKey struct{}

// recorder is one request's response writer and its line's facts. It wraps the server's writer
// rather than replacing it, and carries through the three interfaces Dispatch's handlers take it
// for: Flusher for the body drain's flush of an answer (api.drainingWriter), Hijacker for the
// websocket upgrade, and ReaderFrom so an artifact download still streams through the server's own
// copy.
type recorder struct {
	http.ResponseWriter
	// mu guards kind, id and credential alone: Note may run on another goroutine than the
	// handler's while the handler is still writing.
	mu   sync.Mutex
	kind string
	id   string
	// credential is the owner or subject a bearer's credential proved, kept when the bearer then
	// names a session (which is text the caller wrote), so the line still says whose credential
	// acted. Empty when the caller is that credential itself, a person, or the shared token.
	credential string
	status     int
	written    int64
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
		// Copy through Write, which counts, without handing io.Copy this ReadFrom again.
		return io.Copy(writerOnly{rec}, body)
	}
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	written, err := from.ReadFrom(body)
	rec.written += written
	return written, err
}

// writerOnly hides every method of its writer but Write.
type writerOnly struct{ io.Writer }

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
// ids their paths carry. A handler that panics (an artifact download whose store fails mid-stream
// aborts with http.ErrAbortHandler) still writes its line, marked aborted=true, as the panic
// passes on to net/http.
func Handler(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, kind: KindAnonymous}
		completed := false
		defer func() { rec.write(route, r, time.Since(start), !completed) }()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), callerKey{}, rec)))
		completed = true
	})
}

// HandlerFunc is Handler for a handler function, which is how every Dispatch route is written.
func HandlerFunc(route string, next http.HandlerFunc) http.HandlerFunc {
	handler := Handler(route, next)
	return handler.ServeHTTP
}

// Note records who the server took this request's sender for. The authentication layer calls it
// wherever it resolves a caller, and the last resolution wins: a bearer is noted by what its
// token proved and then by the session it named, which is the caller a reader wants. Since that
// session is text the caller wrote, a session named after a bearer credential keeps the
// credential as the line's `credential`: `token:<owner>`, `service:<subject>` or `shared-token`.
func Note(ctx context.Context, kind, id string) {
	rec, _ := ctx.Value(callerKey{}).(*recorder)
	if rec == nil {
		return
	}
	rec.mu.Lock()
	switch {
	case kind == KindSession && rec.kind == KindSharedToken:
		rec.credential = KindSharedToken
	case kind == KindSession && (rec.kind == KindToken || rec.kind == KindService):
		rec.credential = rec.kind + ":" + rec.id
	case kind != KindSession:
		rec.credential = ""
	}
	rec.kind, rec.id = kind, id
	rec.mu.Unlock()
}

// Detached returns ctx with its request's recorder masked, so a caller resolved on it names no
// one on the line. A connection that resolves its caller again on every heartbeat
// (api.whileCallerResolves: the document socket and the live socket) does so on it: the line
// names the caller the request was admitted as, and a heartbeat that resolves only the bearer's
// credential would otherwise replace the session the request named.
func Detached(ctx context.Context) context.Context {
	return context.WithValue(ctx, callerKey{}, (*recorder)(nil))
}

func (rec *recorder) write(route string, r *http.Request, elapsed time.Duration, aborted bool) {
	rec.mu.Lock()
	kind, id, credential := rec.kind, rec.id, rec.credential
	rec.mu.Unlock()
	status := rec.status
	if status == 0 {
		// A handler that wrote nothing answered 200 with an empty body. One that panicked is
		// logged as 500, though net/http writes no status: it closes the connection (HTTP/1)
		// or resets the stream (HTTP/2).
		status = http.StatusOK
		if aborted {
			status = http.StatusInternalServerError
		}
	}
	attrs := []any{
		"method", clip(r.Method),
		"route", route,
		"path", clip(r.URL.EscapedPath()),
		"caller_kind", kind,
		"caller", clip(id),
		"address", clip(auth.ForwardedAddress(r)),
		"peer", peerAddress(r),
		"user_agent", clip(r.UserAgent()),
		"status", status,
		"elapsed_ms", elapsed.Milliseconds(),
		"bytes", rec.written,
	}
	if credential != "" {
		attrs = append(attrs, "credential", credential)
	}
	if aborted {
		attrs = append(attrs, "aborted", true)
	}
	slog.Info(Message, attrs...)
}

// peerAddress is the connection's own address without its port: the hop that reached Dispatch,
// which a caller cannot forge as it can an X-Forwarded-For header on a direct path.
func peerAddress(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return clip(r.RemoteAddr)
}

// clip cuts caller-written text to fieldBytes, dropping a rune the cut splits.
func clip(text string) string {
	if len(text) <= fieldBytes {
		return text
	}
	return strings.ToValidUTF8(text[:fieldBytes], "")
}
