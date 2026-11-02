package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/notify"
)

// deleteRecorder is the listener's session route: it keeps what the clean stop asked of it and
// answers as the real route does. answer, when a test gives it one, is the body the route writes
// for a request that named a condition; with none it answers 200 with no body, as the route
// answers a request that names no condition at all.
type deleteRecorder struct {
	method, path, query, bearer string
	answer                      string
	requests                    int
	arrived                     chan struct{}
}

func (d *deleteRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.method, d.path, d.query, d.bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		d.requests++
		if d.arrived != nil {
			close(d.arrived)
		}
		if d.answer == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(d.answer))
	}))
	t.Cleanup(server.Close)
	return server
}

func bufferedLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// The stop is one DELETE of the dead session's key, with the listener's bearer, and it names the
// moment the probe judged the process gone: the listener deletes the registration only while
// nothing has been written under that session id since.
func TestTheCleanStopDeletesTheRegistrationUnderTheMomentItsProbeJudged(t *testing.T) {
	listener := &deleteRecorder{}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}
	goneAt := time.Now()

	stop.send(context.Background(), "ses_dead", goneAt)

	if listener.method != http.MethodDelete || listener.path != "/v1/sessions/ses_dead" || listener.bearer != "Bearer envoy-token" {
		t.Fatalf("the listener was asked %s %s with %q, want one DELETE of the session's key with the bearer",
			listener.method, listener.path, listener.bearer)
	}
	if want := fmt.Sprintf("if_not_registered_after=%d", goneAt.UnixMilli()); listener.query != want {
		t.Fatalf("the listener was asked %q, want the stop's own moment %q", listener.query, want)
	}
	if !strings.Contains(logged.String(), "the Envoy session of a role whose process is gone was stopped") {
		t.Fatalf("logged %q, want the stop reported done", logged.String())
	}
}

// The race the condition exists for, as the daemon sees it: the claim relaunched under the same
// session id and the new process registered before the stop was judged, so the listener kept that
// registration. The stop is still sent — only the listener can tell what is stored when it lands
// — and what it reports is that the live registration stayed, not that the session was stopped.
func TestTheCleanStopReportsARegistrationTheListenerKept(t *testing.T) {
	listener := &deleteRecorder{answer: `{"deleted":false,"reason":"the registration was written after the moment the delete belongs to"}`}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}

	stop.send(context.Background(), "ses_relaunched", time.Now())

	if listener.requests != 1 {
		t.Fatalf("the listener was asked %d times, want the one stop, judged there", listener.requests)
	}
	if !strings.Contains(logged.String(), "the Envoy listener kept the registration of a dead role's session") {
		t.Fatalf("logged %q, want the kept registration reported", logged.String())
	}
	if strings.Contains(logged.String(), "the Envoy session of a role whose process is gone was stopped") {
		t.Fatalf("logged %q, want no claim that a registration the listener kept was stopped", logged.String())
	}
}

// A listener of an earlier release judges no condition and answers 200 with no body at all: it
// deleted the registration, and the stop says so.
func TestTheCleanStopReadsABodilessAnswerAsTheRegistrationDeleted(t *testing.T) {
	listener := &deleteRecorder{}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, ""), log: bufferedLogger(&logged)}

	stop.send(context.Background(), "ses_older_listener", time.Now())

	if !strings.Contains(logged.String(), "the Envoy session of a role whose process is gone was stopped") {
		t.Fatalf("logged %q, want the stop reported done", logged.String())
	}
}

// The machine calls the stop inside its own decision, under its lock, so what cleanSessionStop
// returns reads the moment there — where no lock is waited on — and leaves the delete to a
// goroutine.
func TestTheCleanStopRunsOffTheMachinesDecision(t *testing.T) {
	arrived := make(chan struct{})
	listener := &deleteRecorder{arrived: arrived}
	server := listener.server(t)
	stop := cleanSessionStop(context.Background(), server.URL, "envoy-token", bufferedLogger(&bytes.Buffer{}))

	returned := make(chan struct{})
	go func() {
		stop("ses_dead")
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean stop held the machine's decision until the listener answered")
	}
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean stop never reached the listener")
	}
}

// A listener that never answers does not leave the stop in flight for as long as it likes: the
// request is bounded, by cleanSessionStopBudget and by supervision's own end, and what it then
// costs the session is only the wait it already had, so it is reported as the refusal it is.
func TestTheCleanStopIsAbandonedWhenSupervisionEnds(t *testing.T) {
	arrived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	}))
	defer server.Close()
	supervising, endSupervision := context.WithCancel(context.Background())
	defer endSupervision()
	go func() {
		<-arrived
		endSupervision()
	}()
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, ""), log: bufferedLogger(&logged)}

	stop.send(supervising, "ses_hung", time.Now())

	if !strings.Contains(logged.String(), "the Envoy listener did not take the clean stop") {
		t.Fatalf("logged %q, want the abandoned stop reported", logged.String())
	}
}

// With no listener configured there is nothing to tell, and the machines are given no SessionGone
// at all.
func TestNoCleanStopWithoutAListener(t *testing.T) {
	if stop := cleanSessionStop(context.Background(), "", "", quietLogger()); stop != nil {
		t.Fatal("a daemon with no Envoy listener was given a clean stop to call")
	}
}
