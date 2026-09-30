package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// heldSuspendDaemon answers the suspend of architectClaim 202 with the claim still working — a
// suspension held for the agent's turn — and each later list with the next of states, the last
// repeated.
func heldSuspendDaemon(t *testing.T, states ...string) (port string, lists func() int) {
	t.Helper()
	var mu sync.Mutex
	served := 0
	claimIn := func(state string) api.OperatorClaim {
		return api.OperatorClaim{Token: architectClaim, Tree: "LEGION-208", Issue: "LEGION-208", Role: "architect", State: state, Generation: 1}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == claimsRoute+"/"+string(architectClaim)+"/suspend":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(claimIn("working"))
		case r.Method == http.MethodGet && r.URL.Path == claimsRoute:
			mu.Lock()
			state := states[min(served, len(states)-1)]
			served++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.OperatorClaims{Claims: []api.OperatorClaim{claimIn(state)}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port(), func() int { mu.Lock(); defer mu.Unlock(); return served }
}

func operatorTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "operator-token")
	if err := os.WriteFile(path, []byte(claimsOperatorToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A suspend the daemon holds for the agent's turn (202) is waited out: the command polls the
// claims until the claim is suspended and prints it so, exiting 0.
func TestClaimsSuspendWaitsForASuspensionHeldForTheTurn(t *testing.T) {
	port, lists := heldSuspendDaemon(t, "working", "working", "suspended")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "claims", "suspend", "--port", port,
		"--operator-token-file", operatorTokenFile(t), "--claim", string(architectClaim), "--wait", "10s"}, &out, &errb)

	if code != 0 || errb.Len() != 0 {
		t.Fatalf("suspend exited %d; stderr %q", code, errb.String())
	}
	if want := "legion-legion-legion-208-architect architect LEGION-208 suspended 1\n"; out.String() != want {
		t.Fatalf("suspend printed %q, want %q", out.String(), want)
	}
	if got := lists(); got != 3 {
		t.Fatalf("the command listed the claims %d times, want until the third answer said suspended", got)
	}
}

// A suspension that never lands within --wait fails the command, naming the state the claim was
// last seen in.
func TestClaimsSuspendFailsWhenTheHeldSuspensionNeverLands(t *testing.T) {
	port, _ := heldSuspendDaemon(t, "working")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "claims", "suspend", "--port", port,
		"--operator-token-file", operatorTokenFile(t), "--claim", string(architectClaim), "--wait", "1s"}, &out, &errb)

	if code != 1 || out.Len() != 0 {
		t.Fatalf("suspend exited %d printing %q, want 1 and nothing printed", code, out.String())
	}
	if msg := errb.String(); !strings.Contains(msg, "still working after 1s") {
		t.Fatalf("suspend said %q, want it to name the last state it saw and the wait", msg)
	}
}
