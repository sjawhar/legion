package routes

import (
	"bytes"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
)

// logBuffer is a log sink the router's goroutines may write while the test reads it.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// routeLines are the access-log lines logged holds for route, in order.
func routeLines(logged *logBuffer, route string) []string {
	logged.mu.Lock()
	defer logged.mu.Unlock()
	var lines []string
	for _, line := range strings.Split(logged.b.String(), "\n") {
		if strings.Contains(line, `msg="`+accesslog.Message+`"`) && strings.Contains(line, " route="+route+" ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// setDefaultLogger makes handler the default logger's for the test's lifetime. slog.SetDefault also
// points the log package's output at handler and clears its flags, and setting the previous logger
// back undoes neither, so the cleanup puts the log package's output and flags back as well: without
// that, every log.Print after the test writes into its finished buffer instead of stderr.
func setDefaultLogger(t *testing.T, handler slog.Handler) {
	t.Helper()
	previous, output, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(output)
		log.SetFlags(flags)
	})
}

func captureAccessLog(t *testing.T) *logBuffer {
	t.Helper()
	logged := &logBuffer{}
	setDefaultLogger(t, slog.NewTextHandler(logged, nil))
	return logged
}

// A request for an API path no route answers writes its line too, so a client calling a route
// that does not exist, or a route with a method it does not serve, shows among the callers.
func TestAnUnknownAPIPathWritesOneAccessLogLine(t *testing.T) {
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/does-not-exist"},
		{http.MethodPost, "/auth/whoami"},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			handler, _ := newTestRouter(t)
			logged := captureAccessLog(t)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d", request.method, request.path, response.Code)
			}
			lines := routeLines(logged, unmatchedRoute)
			if len(lines) != 1 {
				t.Fatalf("an unknown API path wrote %d access-log lines", len(lines))
			}
			for _, want := range []string{`method=` + request.method, `path=` + request.path, `status=404`, `caller_kind=anonymous`} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("access-log line %q does not carry %q", lines[0], want)
				}
			}
		})
	}
}

// A sign-in that succeeds names the person it signed in on its callback's line.
func TestASignInCallbackNamesThePersonItSignedIn(t *testing.T) {
	rig := newSignInRig(t, "")
	logged := captureAccessLog(t)
	if response := rig.signIn(t, personClaims("other-group", rigGroup)); response.Code != http.StatusFound {
		t.Fatalf("callback: status %d body=%s", response.Code, response.Body.String())
	}
	lines := routeLines(logged, "/auth/callback")
	if len(lines) != 1 {
		t.Fatalf("one callback wrote %d access-log lines", len(lines))
	}
	for _, want := range []string{`caller_kind=user`, `caller=` + rigEmail, `status=302`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
}
