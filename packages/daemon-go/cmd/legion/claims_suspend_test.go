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

// seenClaim is one answer the list gives about architectClaim: its state, and whether the daemon
// holds its suspension.
type seenClaim struct {
	state string
	held  bool
}

var (
	heldWorking = seenClaim{"working", true}
	suspended   = seenClaim{"suspended", false}
)

// heldSuspendDaemon answers the suspend of architectClaim 202 with the claim working and its
// suspension held, and each later list with the next of answers, the last repeated.
func heldSuspendDaemon(t *testing.T, answers ...seenClaim) (port string, lists func() int) {
	t.Helper()
	var mu sync.Mutex
	served := 0
	claimAs := func(seen seenClaim) api.OperatorClaim {
		return api.OperatorClaim{Token: architectClaim, Tree: "LEGION-208", Issue: "LEGION-208", Role: "architect",
			State: seen.state, Generation: 1, SuspensionHeld: seen.held}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == claimsRoute+"/"+string(architectClaim)+"/suspend":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(claimAs(heldWorking))
		case r.Method == http.MethodGet && r.URL.Path == claimsRoute:
			mu.Lock()
			seen := answers[min(served, len(answers)-1)]
			served++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.OperatorClaims{Claims: []api.OperatorClaim{claimAs(seen)}})
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

// suspendAgainst runs `legion claims suspend` of architectClaim against port, waiting up to wait,
// with any extra flags.
func suspendAgainst(t *testing.T, port, wait string, extra ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	args := append([]string{"legion", "claims", "suspend", "--port", port,
		"--operator-token-file", operatorTokenFile(t), "--claim", string(architectClaim), "--wait", wait}, extra...)
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// A suspend the daemon holds for the agent's turn (202) is waited out: the command polls the
// claims until the claim is suspended and prints it so, exiting 0.
func TestClaimsSuspendWaitsForASuspensionHeldForTheTurn(t *testing.T) {
	port, lists := heldSuspendDaemon(t, heldWorking, heldWorking, suspended)

	code, out, errb := suspendAgainst(t, port, "10s")

	if code != 0 || errb != "" {
		t.Fatalf("suspend exited %d; stderr %q", code, errb)
	}
	if want := "legion-legion-legion-208-architect architect LEGION-208 suspended 1\n"; out != want {
		t.Fatalf("suspend printed %q, want %q", out, want)
	}
	if got := lists(); got != 3 {
		t.Fatalf("the command listed the claims %d times, want until the third answer said suspended", got)
	}
}

// Under --json the claim a held suspension ended with prints as one JSON line, as every other
// --json answer does.
func TestClaimsSuspendJSONPrintsTheSuspendedClaimAsOneLine(t *testing.T) {
	port, _ := heldSuspendDaemon(t, heldWorking, suspended)

	code, out, errb := suspendAgainst(t, port, "10s", "--json")

	if code != 0 || errb != "" {
		t.Fatalf("suspend exited %d; stderr %q", code, errb)
	}
	var got api.OperatorClaim
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.State != "suspended" || !strings.HasSuffix(out, "}\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("suspend --json printed %q (%v), want the suspended claim as one JSON line", out, err)
	}
}

// Every other end stops the poll at once, exiting 1 and saying what happened: the claim failed or
// retired, or the daemon holds its suspension no longer — a start dropped it — rather than waiting
// out --wait.
func TestClaimsSuspendStopsAtEveryOtherEnd(t *testing.T) {
	for _, tc := range []struct {
		seen seenClaim
		want string
	}{
		{seenClaim{"failed", false}, "is failed, so it will not be suspended"},
		{seenClaim{"retired", false}, "is retired, so it will not be suspended"},
		{seenClaim{"idle", false}, "no longer holds the suspension of legion-legion-legion-208-architect, which is idle: a start run against the claim dropped it"},
	} {
		t.Run(tc.seen.state, func(t *testing.T) {
			port, lists := heldSuspendDaemon(t, heldWorking, tc.seen)

			code, out, errb := suspendAgainst(t, port, "1h")

			if code != 1 || out != "" {
				t.Fatalf("suspend exited %d printing %q, want 1 and nothing printed", code, out)
			}
			if !strings.Contains(errb, tc.want) {
				t.Fatalf("suspend said %q, want %q", errb, tc.want)
			}
			if got := lists(); got != 2 {
				t.Fatalf("the command listed the claims %d times, want it to stop at the second answer", got)
			}
		})
	}
}

// A suspension still held when --wait runs out fails the command, naming the claim's state and why
// a held suspension can outlast the stop timeout.
func TestClaimsSuspendFailsWhenTheHeldSuspensionOutlastsTheWait(t *testing.T) {
	port, _ := heldSuspendDaemon(t, heldWorking)

	code, out, errb := suspendAgainst(t, port, "1s")

	if code != 1 || out != "" {
		t.Fatalf("suspend exited %d printing %q, want 1 and nothing printed", code, out)
	}
	if want := "is still working after 1s, its suspension held"; !strings.Contains(errb, want) {
		t.Fatalf("suspend said %q, want %q", errb, want)
	}
}
