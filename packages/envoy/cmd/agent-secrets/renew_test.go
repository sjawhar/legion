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

// renewTestBound is how long a renewal test waits for any one step before it fails, so a
// regression that turns a terminal refusal into a retry (or the reverse) fails the test rather
// than hanging it until go test's own timeout.
const renewTestBound = 10 * time.Second

// syncWriter is a stderr a renew loop running on its own goroutine writes and a test reads.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// receive is the next value from ch, failing the test when none arrives within renewTestBound.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(renewTestBound):
		t.Fatalf("no %s within %s", what, renewTestBound)
	}
	var zero T
	return zero
}

// awaitRetry waits for renewLoop to reach its retry wait and returns that wait's channel. It
// fails the test at once when the loop exits instead (the refusal was treated as terminal), and
// when it does neither within renewTestBound.
func awaitRetry(t *testing.T, waits <-chan chan time.Time, result <-chan int, what string) chan time.Time {
	t.Helper()
	select {
	case wait := <-waits:
		return wait
	case code := <-result:
		t.Fatalf("renew loop exited %d on %s, want it to wait and retry", code, what)
	case <-time.After(renewTestBound):
		t.Fatalf("renew loop neither retried nor exited on %s within %s", what, renewTestBound)
	}
	return nil
}

// steppedWaits is an injected renewLoop wait: each call hands the test a channel through waits,
// which the loop then blocks on until the test sends on it, so every step is the test's.
func steppedWaits(capacity int) (chan chan time.Time, func(time.Duration) <-chan time.Time) {
	waits := make(chan chan time.Time, capacity)
	return waits, func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		waits <- ch
		return ch
	}
}

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
// request. The test advances the loop through a channel, so the rotation and the terminal broker
// refusal are steps, not wall-clock delays. ENROLLMENT_NOT_LIVE exits exitRenewRefused so the
// worker shim can stop instead of restarting a renewer whose enrollment has ended.
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
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "ENROLLMENT_NOT_LIVE", "error": "enrollment is not live"})
	}))
	defer srv.Close()
	client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
	waits, after := steppedWaits(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- renewLoop(ctx, client, signer, enrollmentID, func() (string, error) { return readPodToken(tokenFile) }, &stderr, after)
	}()
	receive(t, first, "first renewal")
	receive(t, waits, "wait for the next renewal") <- time.Now()
	if code := receive(t, result, "renew loop exit"); code != exitRenewRefused {
		t.Fatalf("renew loop exit = %d, want terminal-refusal code %d", code, exitRenewRefused)
	}
	if got := stderr.String(); !strings.Contains(got, "enrollment "+enrollmentID) || !strings.Contains(got, "HTTP 401 ENROLLMENT_NOT_LIVE") {
		t.Fatalf("terminal refusal stderr %q, want enrollment, HTTP status and broker code", got)
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

// TestRenewWithoutAPodTokenFileSendsNoBody: a box renewal posts no body, and a refusal outside the
// terminal set (here an iat-skewed PROOF_INVALID) reaches the retry wait rather than exit 76.
func TestRenewWithoutAPodTokenFileSendsNoBody(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read renew body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, raw)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "PROOF_INVALID", "error": "proof invalid: iat outside the allowed skew"})
	}))
	defer srv.Close()
	client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
	waits, after := steppedWaits(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan int, 1)
	go func() {
		result <- renewLoop(ctx, client, signer, enrollmentID, func() (string, error) { return "", nil }, io.Discard, after)
	}()
	awaitRetry(t, waits, result, "an iat-skewed PROOF_INVALID")
	mu.Lock()
	got := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("renew bodies %q, want one request with no body", got)
	}
	cancel()
	if code := receive(t, result, "renew loop exit after cancellation"); code != 0 {
		t.Fatalf("renew loop exit after retry cancellation = %d, want 0", code)
	}
}

// TestRenewStopsOnlyForTerminalBrokerCodes: POD_TOKEN_REQUIRED and ENROLLMENT_NOT_LIVE exit 76,
// and so does an older broker's PROOF_INVALID that says the enrollment is not live (its renew
// handler's and its proof check's wording), since that broker has no ENROLLMENT_NOT_LIVE to send.
// The injected wait fails the test, so a terminal code that regresses to a retry fails at once.
func TestRenewStopsOnlyForTerminalBrokerCodes(t *testing.T) {
	for _, tc := range []struct {
		code, message string
		status        int
	}{
		{"POD_TOKEN_REQUIRED", "a pod enrollment's renewal must carry its projected token", http.StatusForbidden},
		{"ENROLLMENT_NOT_LIVE", "enrollment is not live", http.StatusUnauthorized},
		{"PROOF_INVALID", "enrollment is not live", http.StatusUnauthorized},
		{"PROOF_INVALID", "proof invalid: not live", http.StatusUnauthorized},
	} {
		t.Run(tc.code+" "+tc.message, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "error": tc.message})
			}))
			defer srv.Close()
			client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			after := func(time.Duration) <-chan time.Time {
				t.Errorf("renew loop waited to retry %s %q, want it to exit %d at once", tc.code, tc.message, exitRenewRefused)
				cancel()
				return make(chan time.Time)
			}
			if got := renewLoop(ctx, client, signer, enrollmentID, func() (string, error) { return "", nil }, io.Discard, after); got != exitRenewRefused {
				t.Fatalf("renew loop exit = %d, want %d", got, exitRenewRefused)
			}
		})
	}
}

// TestRenewRetriesEveryOtherRefusal: a refusal outside the terminal set keeps the retry backoff.
// POD_IDENTITY_MISMATCH is among them because the broker also answers it for a token it could not
// verify while its issuer or key set was unreachable; a real mismatch is bounded by the lease,
// after which the next renewal is ENROLLMENT_NOT_LIVE. POD_TOKEN_EXPIRED is retried with the token
// the file holds next, and PROOF_INVALID that does not say "not live" (clock skew, key, URL) can
// clear on its own.
func TestRenewRetriesEveryOtherRefusal(t *testing.T) {
	for _, tc := range []struct {
		code, message string
		status        int
	}{
		{"POD_IDENTITY_MISMATCH", "the projected token does not identify this pod", http.StatusForbidden},
		{"POD_TOKEN_EXPIRED", "the projected token has expired (oidc: token expired at 2026-10-11T07:41:46Z)", http.StatusForbidden},
		{"PROOF_INVALID", "proof invalid: iat outside the allowed skew", http.StatusUnauthorized},
		{"PROOF_INVALID", "proof invalid: htm/htu do not match the request", http.StatusUnauthorized},
	} {
		t.Run(tc.code+" "+tc.message, func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "error": tc.message})
			}))
			defer srv.Close()
			client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
			waits, after := steppedWaits(1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan int, 1)
			go func() {
				result <- renewLoop(ctx, client, signer, enrollmentID, func() (string, error) { return "a-pod-token", nil }, io.Discard, after)
			}()
			awaitRetry(t, waits, result, tc.code) <- time.Now()
			awaitRetry(t, waits, result, tc.code+" again")
			mu.Lock()
			got := requests
			mu.Unlock()
			if got != 2 {
				t.Fatalf("broker requests after two %s refusals = %d, want 2", tc.code, got)
			}
			cancel()
			if code := receive(t, result, "renew loop exit after cancellation"); code != 0 {
				t.Fatalf("renew loop exit after cancellation = %d, want 0", code)
			}
		})
	}
}

// TestRenewRefusesAnUnreadableOrBlankPodTokenFile: a configured token file must be nonblank before
// cmdRenew sends anything. A blank projected file is never silently turned into the legacy
// bodyless renewal, at startup or after a successful renewal, and the refusal is an ERROR naming
// the enrollment, since the broker hears nothing and the lease lapses.
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
			if want := "ERROR: enrollment " + testEnrollmentID + " cannot read its pod token"; !strings.Contains(stderr.String(), want) {
				t.Fatalf("renew --pod-token-file %s: stderr %q, want %q", name, stderr.String(), want)
			}
			if requests != 0 {
				t.Fatalf("blank/unreadable token sent %d broker requests, want none", requests)
			}
		})
	}
}

// TestRenewNeverSendsABlankLaterToken: after one successful renewal, a token file that turns blank
// is a local transient error and starts the retry wait, printed as an ERROR naming the enrollment;
// it never makes a second, bodyless request.
func TestRenewNeverSendsABlankLaterToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := make(chan struct{}, 1)
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n != 1 {
			t.Errorf("unexpected broker request %d", n)
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "ENROLLMENT_NOT_LIVE", "error": "enrollment is not live"})
			return
		}
		if err := os.WriteFile(tokenFile, []byte(" \n"), 0o600); err != nil {
			t.Errorf("blank the token: %v", err)
		}
		first <- struct{}{}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(time.Hour)})
	}))
	defer srv.Close()
	client, signer, enrollmentID, _ := renewalSession(t, srv.URL)
	waits, after := steppedWaits(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan int, 1)
	var stderr syncWriter
	go func() {
		result <- renewLoop(ctx, client, signer, enrollmentID, func() (string, error) { return readPodToken(tokenFile) }, &stderr, after)
	}()
	receive(t, first, "first renewal")
	awaitRetry(t, waits, result, "the wait after the first renewal") <- time.Now()
	awaitRetry(t, waits, result, "a blank later token")
	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 1 {
		t.Fatalf("blank later token sent %d broker requests, want one", got)
	}
	if want := "ERROR: enrollment " + enrollmentID + " cannot read its pod token"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("blank later token stderr %q, want %q", stderr.String(), want)
	}
	cancel()
	if code := receive(t, result, "renew loop exit after cancellation"); code != 0 {
		t.Fatalf("renew loop exit after cancellation = %d, want 0", code)
	}
}

func TestUsageDocumentsPermanentRenewalRefusalExit(t *testing.T) {
	if !strings.Contains(usage(), "76 permanent renewal refusal") {
		t.Fatalf("usage does not document exit 76: %q", usage())
	}
}

// TestRenewHelpNamesTheTerminalSet: renew's help names every member of terminalRenewal, so the
// help and the set Permanent reads cannot drift apart.
func TestRenewHelpNamesTheTerminalSet(t *testing.T) {
	summary := strings.ReplaceAll(lookupCommand("renew").summary, "\n", " ")
	for _, want := range append(append([]string(nil), terminalRenewal.Codes...), terminalRenewal.LegacyCode, `"`+terminalRenewal.LegacyMessage+`"`) {
		if !strings.Contains(summary, want) {
			t.Errorf("renew help %q does not name %s", summary, want)
		}
	}
	if strings.Contains(summary, "POD_IDENTITY_MISMATCH") {
		t.Errorf("renew help %q names POD_IDENTITY_MISMATCH, which retries", summary)
	}
}
