package main

import (
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

// TestRenewSendsThePodsCurrentToken: `agent-secrets renew --pod-token-file <file>` reads the file
// again before every renewal and sends what it holds, trimmed, as the body's pod_token, so the
// projected token the kubelet rotates reaches the broker as it is now. A refusal (here 403
// POD_IDENTITY_MISMATCH) ends the renewer with exit 1, naming the broker's code.
func TestRenewSendsThePodsCurrentToken(t *testing.T) {
	binary := buildAgentSecrets(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/enrollments/"+testEnrollmentID+"/renew" {
			http.NotFound(w, r)
			return
		}
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
			// The kubelet rotates the projected token before the next renewal; a lease already
			// past makes the renewer come back after its one-second floor.
			if err := os.WriteFile(tokenFile, []byte("rotated-token\n"), 0o600); err != nil {
				t.Errorf("rotate token: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(-time.Second)})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "POD_IDENTITY_MISMATCH", "error": "the projected token does not identify this pod"})
	}))
	defer srv.Close()

	_, stderr, exit := runAgentSecrets(t, binary, srv.URL, newKeyDir(t), nil, "renew", "--pod-token-file", tokenFile)
	if exit != 1 || !strings.Contains(stderr, "POD_IDENTITY_MISMATCH") {
		t.Fatalf("renew --pod-token-file: exit %d, stderr %q; want 1 naming POD_IDENTITY_MISMATCH", exit, stderr)
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

// TestRenewWithoutAPodTokenFileSendsNoBody: a box's renewer posts no body, which every broker
// release accepts for a box.
func TestRenewWithoutAPodTokenFileSendsNoBody(t *testing.T) {
	binary := buildAgentSecrets(t)
	var mu sync.Mutex
	var lengths []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lengths = append(lengths, r.ContentLength)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "PROOF_INVALID", "error": "enrollment is not live"})
	}))
	defer srv.Close()
	if _, stderr, exit := runAgentSecrets(t, binary, srv.URL, newKeyDir(t), nil, "renew"); exit != 1 || !strings.Contains(stderr, "PROOF_INVALID") {
		t.Fatalf("renew: exit %d, stderr %q; want 1 naming PROOF_INVALID", exit, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lengths) != 1 || lengths[0] != 0 {
		t.Fatalf("renew request content lengths %v, want one request with no body", lengths)
	}
}

// TestRenewRefusesAnUnreadablePodTokenFile: a --pod-token-file the renewer cannot read is a usage
// error before any renewal, naming the file, so the renewer never renews without the token it was
// told to send.
func TestRenewRefusesAnUnreadablePodTokenFile(t *testing.T) {
	binary := buildAgentSecrets(t)
	missing := filepath.Join(t.TempDir(), "no-token")
	_, stderr, exit := runAgentSecrets(t, binary, "http://127.0.0.1:1", newKeyDir(t), nil, "renew", "--pod-token-file", missing)
	if exit != exitUsageError || !strings.Contains(stderr, missing) {
		t.Fatalf("renew --pod-token-file <missing>: exit %d, stderr %q; want %d naming the file", exit, stderr, exitUsageError)
	}
}
