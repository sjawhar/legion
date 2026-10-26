package accesslog

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer is a log sink a handler's goroutines may write while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// capture sends the default logger's lines to a buffer for the test's lifetime.
func capture(t *testing.T) *lockedBuffer {
	t.Helper()
	logged := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logged
}

// line is the one access-log line logged holds.
func line(t *testing.T, logged *lockedBuffer) string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, `msg="`+Message+`"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want one access-log line, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	return lines[0]
}

func carries(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("access-log line %q does not carry %q", got, w)
		}
	}
}

// A handler that panics, as an artifact download aborts with http.ErrAbortHandler when its store
// fails mid-stream, still writes its line, marked aborted, and the panic still reaches net/http.
func TestAPanickingHandlerStillWritesItsLine(t *testing.T) {
	logged := capture(t)
	handler := Handler("/api/v1/artifacts/{id}/files", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), KindUser, "alice")
		_, _ = w.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("the panic reaching net/http = %v, want http.ErrAbortHandler", recovered)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/x/files", nil))
	}()
	carries(t, line(t, logged), `caller_kind=user`, `caller=alice`, `status=200`, `bytes=7`, `aborted=true`)
}

// A handler that panics before it writes anything is logged as 500, though the client gets a
// closed connection rather than a status.
func TestAHandlerThatPanicsBeforeWritingIsLoggedAsA500(t *testing.T) {
	logged := capture(t)
	handler := Handler("/r", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	}()
	carries(t, line(t, logged), `status=500`, `aborted=true`)
}

// A request that completes carries no aborted field.
func TestACompletedRequestIsNotMarkedAborted(t *testing.T) {
	logged := capture(t)
	Handler("/r", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	if got := line(t, logged); strings.Contains(got, "aborted") {
		t.Fatalf("access-log line %q is marked aborted", got)
	}
}

// writerWithoutReadFrom is a response writer with no ReadFrom of its own.
type writerWithoutReadFrom struct {
	header http.Header
	body   bytes.Buffer
}

func (w *writerWithoutReadFrom) Header() http.Header         { return w.header }
func (w *writerWithoutReadFrom) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *writerWithoutReadFrom) WriteHeader(int)             {}

// A body streamed with io.Copy counts its bytes even when the server's writer has no ReadFrom.
func TestACopiedBodyIsCountedWithoutTheServersReadFrom(t *testing.T) {
	logged := capture(t)
	w := &writerWithoutReadFrom{header: http.Header{}}
	Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, strings.NewReader("twelve bytes"))
	})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/r", nil))
	if w.body.String() != "twelve bytes" {
		t.Fatalf("the response body = %q", w.body.String())
	}
	carries(t, line(t, logged), `bytes=12`, `status=200`)
}

// A caller resolved on a detached context names no one, so a heartbeat that resolves only the
// bearer's credential leaves the session the request named on the line.
func TestADetachedResolutionLeavesTheLinesCaller(t *testing.T) {
	logged := capture(t)
	Handler("/ws/doc/{room}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), KindSharedToken, "")
		Note(r.Context(), KindSession, "session-0123456789abcdef")
		Note(Detached(r.Context()), KindSharedToken, "")
		Note(Detached(context.Background()), KindUser, "nobody")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ws/doc/x", nil))
	carries(t, line(t, logged), `caller_kind=session`, `caller=session-0123456789abcdef`)
}

// A session named after a bearer credential keeps that credential on the line, since the session
// id is text the caller wrote; a request naming no session carries no credential field.
func TestASessionKeepsTheCredentialThatNamedIt(t *testing.T) {
	for name, test := range map[string]struct {
		first, firstID string
		session        bool
		want           []string
		absent         string
	}{
		"personal token":    {first: KindToken, firstID: "ada@example.com", session: true, want: []string{`caller_kind=session`, `credential=token:ada@example.com`}},
		"service":           {first: KindService, firstID: "system:serviceaccount:ns:sa", session: true, want: []string{`caller_kind=session`, `credential=service:system:serviceaccount:ns:sa`}},
		"shared token":      {first: KindSharedToken, session: true, want: []string{`caller_kind=session`, `credential=shared-token`}},
		"token, no session": {first: KindToken, firstID: "ada@example.com", want: []string{`caller_kind=token`, `caller=ada@example.com`}, absent: "credential="},
	} {
		t.Run(name, func(t *testing.T) {
			logged := capture(t)
			Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Note(r.Context(), test.first, test.firstID)
				if test.session {
					Note(r.Context(), KindSession, "session-0123456789abcdef")
				}
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
			got := line(t, logged)
			carries(t, got, test.want...)
			if test.absent != "" && strings.Contains(got, test.absent) {
				t.Fatalf("access-log line %q carries %q", got, test.absent)
			}
		})
	}
}

// The line names the address the load balancer saw and the user agent, so callers sharing one
// credential can still be told apart; it keeps the path escaped and never its query string.
func TestTheLineNamesTheAddressAndUserAgentAndKeepsThePathEscaped(t *testing.T) {
	logged := capture(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/a%2Fb%3Ftoken%3Dsecret?q=hidden-query", nil)
	request.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	request.Header.Set("User-Agent", "dispatch-cli/1.0")
	Handler("/api/v1/issues/{key}", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), request)
	got := line(t, logged)
	carries(t, got, `path=/api/v1/issues/a%2Fb%3Ftoken%3Dsecret `, `address=203.0.113.9`, `peer=192.0.2.1 `, `user_agent=dispatch-cli/1.0`)
	for _, forbidden := range []string{"hidden-query", "token=secret"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("access-log line %q carries %q", got, forbidden)
		}
	}
}

// A method is the caller's own text on a route mounted for every method, so it is clipped too.
func TestAnUnknownMethodIsClipped(t *testing.T) {
	logged := capture(t)
	request := httptest.NewRequest(http.MethodGet, "/r", nil)
	request.Method = strings.Repeat("M", 4*fieldBytes)
	Handler("/r", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), request)
	if got := line(t, logged); strings.Contains(got, strings.Repeat("M", fieldBytes+1)) {
		t.Fatalf("access-log line keeps more than %d bytes of the method", fieldBytes)
	}
}

// The session id a bearer names is the caller's own text, so the line keeps at most fieldBytes of
// it, as it keeps of the path.
func TestANamedSessionIsClipped(t *testing.T) {
	logged := capture(t)
	long := strings.Repeat("s", 4*fieldBytes)
	Handler("/r", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Note(r.Context(), KindSession, long)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
	got := line(t, logged)
	carries(t, got, `caller=`+strings.Repeat("s", fieldBytes)+" ")
	if strings.Contains(got, strings.Repeat("s", fieldBytes+1)) {
		t.Fatalf("access-log line keeps more than %d bytes of the named session", fieldBytes)
	}
}
