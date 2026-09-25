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

func newTestProber(url, token string, bootTimeout time.Duration, log *bytes.Buffer) *Prober {
	return NewProber(ProberOptions{
		EnvoyURL: url, EnvoyToken: token, Project: "demo", BootTimeout: bootTimeout,
		Now: func() time.Time { return probeNow }, Log: slog.New(slog.NewTextHandler(log, nil)),
	})
}

// The window is two plugin heartbeats, never less than the boot timeout: 240 s at the default
// 120 s boot timeout (packages/daemon/src/daemon/runtime-kubernetes.ts:59-69).
func TestLivenessWindowIsTwoHeartbeatsAtLeastTheBootTimeout(t *testing.T) {
	for _, tc := range []struct{ boot, want time.Duration }{
		{120 * time.Second, 240 * time.Second},
		{60 * time.Second, 240 * time.Second},
		{300 * time.Second, 300 * time.Second},
	} {
		if got := LivenessWindow(tc.boot); got != tc.want {
			t.Errorf("LivenessWindow(%s) = %s, want %s", tc.boot, got, tc.want)
		}
	}
}

// Alive while the listener names the registered session as the controller role's holder and has
// seen it within the window; the lookup is the project's controller token, with the listener's
// bearer.
func TestProbeIsAliveWhileTheSessionHoldsTheRoleWithinTheWindow(t *testing.T) {
	var log bytes.Buffer
	l := &listener{status: http.StatusOK, body: holder("ses_ctl", probeNow.Add(-240*time.Second+time.Millisecond))}
	url := l.serve(t)
	if got := newTestProber(url, "envoy-bearer", 120*time.Second, &log).Probe(context.Background(), "ses_ctl"); got != Alive {
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
	if got := newTestProber(url, "", 120*time.Second, &log).Probe(context.Background(), "ses_ctl"); got != Alive {
		t.Fatalf("Probe = %s, want alive", got)
	}
	if l.authorization {
		t.Fatalf("the lookup carried Authorization %q with no token configured", l.bearer)
	}
}

// Gone: the role unheld or expired (404), held by another session, or last seen at or beyond the
// window.
func TestProbeIsGone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		boot     time.Duration
		wantLogs []string
	}{
		{name: "the role is unheld", status: http.StatusNotFound, body: `{"error":"no live holder","reason":"none"}`, boot: 120 * time.Second},
		{name: "another session holds the role", status: http.StatusOK, body: holder("ses_other", probeNow), boot: 120 * time.Second,
			wantLogs: []string{"ses_other", "ses_ctl"}},
		{name: "last seen at the window", status: http.StatusOK, body: holder("ses_ctl", probeNow.Add(-240*time.Second)), boot: 120 * time.Second,
			wantLogs: []string{"240"}},
		{name: "last seen beyond a boot timeout wider than two heartbeats", status: http.StatusOK, body: holder("ses_ctl", probeNow.Add(-301*time.Second)), boot: 300 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			l := &listener{status: tc.status, body: tc.body}
			url := l.serve(t)
			if got := newTestProber(url, "envoy-bearer", tc.boot, &log).Probe(context.Background(), "ses_ctl"); got != Gone {
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

// A boot timeout wider than two heartbeats widens the window.
func TestProbeWindowFollowsAWideBootTimeout(t *testing.T) {
	var log bytes.Buffer
	l := &listener{status: http.StatusOK, body: holder("ses_ctl", probeNow.Add(-299*time.Second))}
	url := l.serve(t)
	if got := newTestProber(url, "envoy-bearer", 300*time.Second, &log).Probe(context.Background(), "ses_ctl"); got != Alive {
		t.Fatalf("Probe = %s, want alive inside a 300 s window", got)
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
			if got := newTestProber(url, tc.token, 120*time.Second, &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
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
		if got := newTestProber(url, "envoy-bearer", 120*time.Second, &log).Probe(context.Background(), "ses_ctl"); got != Unknown {
			t.Fatalf("Probe = %s, want unknown", got)
		}
		if !strings.Contains(log.String(), "unreachable") {
			t.Errorf("log %q does not say the listener was unreachable", log.String())
		}
	})
}
