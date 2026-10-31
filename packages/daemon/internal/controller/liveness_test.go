package controller

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var probeNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// listener is the Envoy listener's role lookup as the probe reaches it: it answers every request
// with status and body, and records the path and bearer it was asked with.
type listener struct {
	status        int
	body          string
	path, bearer  string
	authorization bool
}

func (l *listener) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.path = r.URL.Path
		l.bearer = r.Header.Get("Authorization")
		_, l.authorization = r.Header["Authorization"]
		w.WriteHeader(l.status)
		_, _ = io.WriteString(w, l.body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// holder is the listener's live-holder answer (roleGetHandler, packages/envoy/cmd/listener/api.go):
// last_seen in unix milliseconds.
func holder(session string, lastSeen time.Time) string {
	return `{"role":"legion-demo-controller","holder":"` + session +
		`","title":"controller","dir":"/x","machine_id":"m","capabilities":[],"last_seen":` + strconv.FormatInt(lastSeen.UnixMilli(), 10) + `}`
}

// lapsed is the listener's 404 for a role whose holder is past the live window, in state
// (writeRoleHolderError, packages/envoy/cmd/listener/api.go).
func lapsed(state string) string {
	return `{"error":"role legion-demo-controller holder ses_ctl lapsed; claim retained","reason":"holder_lapsed","holder":"ses_ctl","holder_state":"` +
		state + `","last_seen":` + strconv.FormatInt(probeNow.Add(-10*time.Minute).UnixMilli(), 10) + `,"claim_released":false}`
}

func newTestProber(url, token string, log *bytes.Buffer) *Prober {
	return NewProber(ProberOptions{
		EnvoyURL: url, EnvoyToken: token, Project: "demo", Log: slog.New(slog.NewTextHandler(log, nil)),
	})
}

// Alive while the listener names the registered session as the controller role's holder, which it
// does only inside its own live window; the lookup is the project's controller token, with the
// listener's bearer.
func TestProbeIsAliveWhileTheSessionHoldsTheRole(t *testing.T) {
	var log bytes.Buffer
	l := &listener{status: http.StatusOK, body: holder("ses_ctl", probeNow.Add(-4*time.Minute))}
	url := l.serve(t)
	if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Alive {
		t.Fatalf("Probe = %s, want alive; log %s", got, log.String())
	}
	if l.path != "/v1/roles/legion-demo-controller" || l.bearer != "Bearer envoy-bearer" {
		t.Fatalf("lookup = %s with %q, want the controller token with the listener's bearer", l.path, l.bearer)
	}
}

func TestProbeSendsNoBearerWithoutAToken(t *testing.T) {
	var log bytes.Buffer
	l := &listener{status: http.StatusOK, body: holder("ses_ctl", probeNow)}
	url := l.serve(t)
	if got := newTestProber(url, "", &log).Probe(context.Background(), "ses_ctl"); got != Alive {
		t.Fatalf("Probe = %s, want alive", got)
	}
	if l.authorization {
		t.Fatalf("the lookup carried Authorization %q with no token configured", l.bearer)
	}
}

// Gone: nobody claimed the role, its holder reached the gone horizon, another session holds it, or
// a listener of an earlier release named no state with its 404 — that listener's lapsed holder is
// no holder at all.
func TestProbeIsGone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantLogs []string
	}{
		{name: "nobody claimed the role", status: http.StatusNotFound, body: `{"error":"no holder for role legion-demo-controller","reason":"unclaimed"}`,
			wantLogs: []string{"unclaimed"}},
		{name: "the holder is gone", status: http.StatusNotFound, body: lapsed("gone"), wantLogs: []string{"holder_lapsed", "gone"}},
		{name: "an older listener names no state", status: http.StatusNotFound, body: `{"error":"no live holder for role legion-demo-controller"}`},
		{name: "a 404 whose body is not JSON", status: http.StatusNotFound, body: "<html>"},
		{name: "another session holds the role", status: http.StatusOK, body: holder("ses_other", probeNow),
			wantLogs: []string{"ses_other", "ses_ctl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			l := &listener{status: tc.status, body: tc.body}
			url := l.serve(t)
			if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Gone {
				t.Fatalf("Probe = %s, want gone; log %s", got, log.String())
			}
			for _, want := range tc.wantLogs {
				if !strings.Contains(log.String(), want) {
					t.Errorf("log %q does not name %q", log.String(), want)
				}
			}
		})
	}
}

// A holder the listener calls unresponsive is past its live window and inside its gone horizon: it
// still holds the role and answers for itself once its heartbeats land, so the probe says Unknown
// and the keeper leaves the controller alone rather than replacing a session that is working
// (dispatch://INFRA-392).
func TestProbeIsUnknownForAnUnresponsiveHolder(t *testing.T) {
	var log bytes.Buffer
	l := &listener{status: http.StatusNotFound, body: lapsed("unresponsive")}
	url := l.serve(t)
	if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
		t.Fatalf("Probe = %s, want unknown; log %s", got, log.String())
	}
	if !strings.Contains(log.String(), "unresponsive, not gone") {
		t.Errorf("log %q does not say the holder is unresponsive rather than gone", log.String())
	}
}

// Unknown, never a death verdict: the listener unreachable, a refusal other than 404, or a body
// the probe cannot read.
func TestProbeIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		status      int
		body        string
		wantLog     string
	}{
		{name: "a server error", token: "envoy-bearer", status: http.StatusInternalServerError, body: `{"error":"kv"}`, wantLog: "500"},
		{name: "an unauthorized lookup with no bearer", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, wantLog: "no bearer token sent"},
		{name: "a body that is not JSON", token: "envoy-bearer", status: http.StatusOK, body: "<html>", wantLog: "unreadable"},
		{name: "a body with no holder", token: "envoy-bearer", status: http.StatusOK, body: `{"last_seen":1}`, wantLog: "unreadable"},
		{name: "a last_seen that is not an integer", token: "envoy-bearer", status: http.StatusOK, body: `{"holder":"ses_ctl","last_seen":"now"}`, wantLog: "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			l := &listener{status: tc.status, body: tc.body}
			url := l.serve(t)
			if got := newTestProber(url, tc.token, &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
				t.Fatalf("Probe = %s, want unknown", got)
			}
			if !strings.Contains(log.String(), tc.wantLog) {
				t.Errorf("log %q does not say %q", log.String(), tc.wantLog)
			}
		})
	}
	t.Run("the listener is unreachable", func(t *testing.T) {
		var log bytes.Buffer
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		if got := newTestProber(url, "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
			t.Fatalf("Probe = %s, want unknown", got)
		}
		if !strings.Contains(log.String(), "unreachable") {
			t.Errorf("log %q does not say the listener was unreachable", log.String())
		}
	})
}
