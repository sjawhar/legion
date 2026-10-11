package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func renewalSession(t *testing.T, broker string) (*client, Signer, string, string) {
	t.Helper()
	dir := newKeyDir(t)
	t.Setenv("AGENT_SECRETS_URL", broker)
	t.Setenv("AGENT_SECRETS_KEY_DIR", dir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	base, enrollmentID, signer, err := sessionContext()
	if err != nil {
		t.Fatal(err)
	}
	return newClient(base), signer, enrollmentID, dir
}

// TestRenewSendsThePodsCurrentToken: renewLoop reads the projected token again before every
// request. The test advances the loop through a channel, so the rotation and the permanent broker
// refusal are steps, not wall-clock delays. A 403 exits exitRenewRefused so the worker shim can
// stop instead of restarting an identity-rejected pod forever.
func TestRenewSendsThePodsCurrentToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies []string
	first := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read renew body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			if err := os.WriteFile(tokenFile, []byte("rotated-token\n"), 0o600); err != nil {
				t.Errorf("rotate token: %v", err)
			}
			first <- struct{}{}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(time.Hour)})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "POD_IDENTITY_MISMATCH", "error": "the projected token does not identify this pod"})
	}))
	defer srv.Close()
	client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
	wait := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- renewLoop(ctx, client, signer, enrollmentID, func() (string, error) { return readPodToken(tokenFile) }, &stderr, func(time.Duration) <-chan time.Time { return wait })
	}()
	<-first
	wait <- time.Now()
	if code := <-result; code != exitRenewRefused {
		t.Fatalf("renew loop exit = %d, want permanent-refusal code %d", code, exitRenewRefused)
	}
	if got := stderr.String(); !strings.Contains(got, "enrollment "+enrollmentID) || !strings.Contains(got, "HTTP 403 POD_IDENTITY_MISMATCH") {
		t.Fatalf("permanent refusal stderr %q, want enrollment, HTTP status and broker code", got)
	}
	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	want := []string{`{"pod_token":"first-token"}`, `{"pod_token":"rotated-token"}`}
	if len(got) != len(want) {
		t.Fatalf("renew bodies %q, want %q", got, want)
	}
	for i := range want {
		if strings.TrimSpace(got[i]) != want[i] {
			t.Fatalf("renew body %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRenewWithoutAPodTokenFileSendsNoBody: a box renewal posts no body. PROOF_INVALID is not a
// terminal enrollment verdict: an iat-skew correction can make the next proof valid, so the loop
// waits to retry instead of returning exit 76.
func TestRenewWithoutAPodTokenFileSendsNoBody(t *testing.T) {
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read renew body: %v", err)
		}
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "PROOF_INVALID", "error": "iat outside 60s"})
	}))
	defer srv.Close()
	client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
	waits := make(chan chan time.Time, 1)
	after := func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time)
		waits <- ch
		return ch
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)
	go func() {
		result <- renewLoop(ctx, client, signer, enrollmentID, func() (string, error) { return "", nil }, io.Discard, after)
	}()
	<-waits // PROOF_INVALID reached the transient retry wait
	if len(bodies) != 1 || len(bodies[0]) != 0 {
		t.Fatalf("renew bodies %q, want one request with no body", bodies)
	}
	cancel()
	if code := <-result; code != 0 {
		t.Fatalf("renew loop exit after retry cancellation = %d, want 0", code)
	}
}

func TestRenewStopsOnlyForTerminalBrokerCodes(t *testing.T) {
	for code, status := range map[string]int{
		terminalRenewalCodePodIdentity: http.StatusForbidden,
		terminalRenewalCodePodToken:    http.StatusForbidden,
		terminalRenewalCodeNotLive:     http.StatusUnauthorized,
	} {
		t.Run(code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": "terminal"})
			}))
			defer srv.Close()
			client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
			if got := renewLoop(context.Background(), client, signer, enrollmentID, func() (string, error) { return "", nil }, io.Discard, time.After); got != exitRenewRefused {
				t.Fatalf("renew loop exit = %d, want %d", got, exitRenewRefused)
			}
		})
	}
}

// TestRenewRefusesAnUnreadableOrBlankPodTokenFile: a configured token file must be nonblank before
// cmdRenew sends anything. A blank projected file is never silently turned into the legacy
// bodyless renewal, at startup or after a successful renewal.
func TestRenewRefusesAnUnreadableOrBlankPodTokenFile(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "PROOF_INVALID", "error": "enrollment is not live"})
	}))
	defer srv.Close()
	keyDir := newKeyDir(t)
	t.Setenv("AGENT_SECRETS_URL", srv.URL)
	t.Setenv("AGENT_SECRETS_KEY_DIR", keyDir)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	for name, content := range map[string]*string{
		"unreadable": nil,
		"blank":      new(" \n\t "),
	} {
		t.Run(name, func(t *testing.T) {
			requests = 0
			path := filepath.Join(t.TempDir(), "token")
			if content != nil {
				if err := os.WriteFile(path, []byte(*content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			if code := cmdRenew([]string{"--pod-token-file", path}, io.Discard, &stderr); code != exitUsageError || !strings.Contains(stderr.String(), path) {
				t.Fatalf("renew --pod-token-file %s: exit %d stderr %q, want usage naming the file", name, code, stderr.String())
			}
			if requests != 0 {
				t.Fatalf("blank/unreadable token sent %d broker requests, want none", requests)
			}
		})
	}
}

// TestRenewNeverSendsABlankLaterToken: after one successful renewal, a token file that turns blank
// is a local transient error and starts the retry wait; it never makes a second, bodyless request.
func TestRenewNeverSendsABlankLaterToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := make(chan struct{}, 1)
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests != 1 {
			t.Fatalf("unexpected broker request %d", requests)
		}
		if err := os.WriteFile(tokenFile, []byte(" \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		first <- struct{}{}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
	waits := make(chan chan time.Time, 2)
	after := func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time)
		waits <- ch
		return ch
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan int, 1)
	go func() {
		result <- renewLoop(ctx, client, signer, enrollmentID, func() (string, error) { return readPodToken(tokenFile) }, io.Discard, after)
	}()
	<-first
	firstWait := <-waits
	firstWait <- time.Now()
	<-waits // the blank read reached the transient retry wait without a second HTTP request
	if requests != 1 {
		t.Fatalf("blank later token sent %d broker requests, want one", requests)
	}
	cancel()
	if code := <-result; code != 0 {
		t.Fatalf("renew loop exit after cancellation = %d, want 0", code)
	}
}

func TestUsageDocumentsPermanentRenewalRefusalExit(t *testing.T) {
	if !strings.Contains(usage(), "76 permanent renewal refusal") {
		t.Fatalf("usage does not document exit 76: %q", usage())
	}
}
