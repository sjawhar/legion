package legion

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// stateServer is a daemon stand-in answering GET /legion/v1/state with body, and anything else
// with 404.
func stateServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != StatePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestTheStateReadIsBoundedByTheRequestTimeout: the window is the plan's D14, twenty seconds, and
// New gives it to every client it builds, so a daemon that accepts a connection and never answers
// ends one pass rather than holding the minute's loop for good.
func TestTheStateReadIsBoundedByTheRequestTimeout(t *testing.T) {
	if RequestTimeout != 20*time.Second {
		t.Fatalf("RequestTimeout = %s, want the 20s of the plan's D14", RequestTimeout)
	}
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer silent.Close()

	// New reads RequestTimeout as it builds, so shrinking it here proves the window the client
	// takes is that one, without waiting the production twenty seconds out.
	production := RequestTimeout
	RequestTimeout = 150 * time.Millisecond
	client := New(silent.URL, "")
	RequestTimeout = production

	started := time.Now()
	_, bytes, err := client.State(t.Context())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a read of a daemon that never answers: err = %v, want one wrapping ErrUnavailable", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("the read waited %s, want the client's own window", waited)
	}
	if bytes != 0 {
		t.Errorf("bytes = %d, want none: nothing was read", bytes)
	}
}

// TestABodyOverTheCapIsAFailedRead: a daemon answering more than ResponseLimit is a failed read,
// never a parse of the first 8 MiB. A body exactly at the cap is read, so the bound refuses only
// what is over it.
func TestABodyOverTheCapIsAFailedRead(t *testing.T) {
	// Valid JSON throughout, so what refuses it is the cap and not the decode.
	envelope := `{"issues":{},"pad":"` + `"}`
	padding := strings.Repeat("a", ResponseLimit+1-len(envelope))
	over := stateServer(t, `{"issues":{},"pad":"`+padding+`"}`)

	_, bytes, err := New(over.URL, "").State(t.Context())
	if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a read of a body over the cap: err = %v, want one wrapping both ErrResponseTooLarge and ErrUnavailable", err)
	}
	if bytes != ResponseLimit+1 {
		t.Errorf("bytes = %d, want the point the read stopped (%d)", bytes, ResponseLimit+1)
	}

	atTheCap := stateServer(t, `{"issues":{},"pad":"`+padding[:len(padding)-1]+`"}`)
	state, bytes, err := New(atTheCap.URL, "").State(t.Context())
	if err != nil {
		t.Fatalf("a read of a body exactly at the cap: %v", err)
	}
	if bytes != ResponseLimit {
		t.Errorf("bytes = %d, want the cap (%d)", bytes, ResponseLimit)
	}
	if len(state.Issues) != 0 {
		t.Errorf("issues = %v, want none", state.Issues)
	}
}

// newFieldsState is a state document as a daemon on dispatch://INFRA-980 serves it: every issue
// holding a slot in admission.active, children included; waiting ordered by tree root; a blocked
// parent waiting for its children; a new admission.trees; a new per-issue tree; and a higher
// contract version. Every one of those is a field this package names nowhere, except tree, which
// it keeps optional.
const newFieldsState = `{
  "daemon": {"project": "INFRA", "contract": 47, "schema": 212},
  "admission": {
    "cap": 4,
    "active": ["INFRA-1", "INFRA-2"],
    "waiting": ["INFRA-3"],
    "trees": ["INFRA-1"],
    "blocked": [{"issue": "INFRA-1", "waitingOn": ["INFRA-2"], "kind": "children"}]
  },
  "issues": {
    "INFRA-1": {
      "key": "INFRA-1", "generation": 3, "phase": "implementing", "status": "doing",
      "tree": "INFRA-1",
      "slot": {"index": 0, "admittedAt": "2026-11-03T10:00:00Z"},
      "architect": {"session": "s-1", "state": "working", "pluginContract": 47},
      "workers": {"implementer": {"claim": {"session": "s-2", "state": "working", "pluginContract": 47}, "rounds": 1}},
      "pullRequest": {"repo": "acme/widgets", "number": 7, "head": "abc", "state": "open",
                      "checksVerdict": "green", "requiresChecks": ["lint", "test"], "fixAttempts": 0}
    },
    "INFRA-2": {
      "key": "INFRA-2", "generation": 1, "phase": "planning", "status": "doing",
      "tree": "INFRA-1",
      "slot": {"index": 1, "admittedAt": "2026-11-03T10:01:00Z"},
      "workers": {"planner": {"claim": {"session": "s-3", "state": "ready", "pluginContract": 47}, "rounds": 1}}
    }
  },
  "pendingStatusWrites": [], "heldOutboxRows": [], "strandedClaims": [],
  "capabilities": [], "consumers": [], "repositories": {"owners": [], "mapped": []},
  "settings": {"concurrency": 4}
}`

// TestAStateWithTheNewAdmissionFieldsParses: the decode is loose, so a daemon that grows the route
// under dispatch://INFRA-980 is still read. Nothing here assumes a slot holder is a root: INFRA-2
// is a child of INFRA-1 and holds slot 1, and its slot is read from its own record.
func TestAStateWithTheNewAdmissionFieldsParses(t *testing.T) {
	server := stateServer(t, newFieldsState)

	state, bytes, err := New(server.URL, "").State(t.Context())
	if err != nil {
		t.Fatalf("read a state carrying dispatch://INFRA-980's new fields: %v", err)
	}
	if bytes != len(newFieldsState) {
		t.Errorf("bytes = %d, want the %d the daemon sent", bytes, len(newFieldsState))
	}
	if len(state.Issues) != 2 {
		t.Fatalf("issues = %v, want both", state.Issues)
	}
	child := state.Issues["INFRA-2"]
	if child.Tree == nil || *child.Tree != "INFRA-1" {
		t.Errorf("INFRA-2's tree = %v, want the root INFRA-1 the new field names", child.Tree)
	}
	if child.Slot == nil || child.Slot.Index != 1 {
		t.Errorf("INFRA-2's slot = %v, want slot 1 read from the issue's own record", child.Slot)
	}
	if child.Workers["planner"].Claim.State != "ready" {
		t.Errorf("INFRA-2's planner = %v, want its claim state", child.Workers["planner"])
	}
	root := state.Issues["INFRA-1"]
	if root.PullRequest == nil || root.PullRequest.State != "open" || root.PullRequest.ChecksVerdict != "green" {
		t.Errorf("INFRA-1's pull request = %+v, want its state and verdict", root.PullRequest)
	}
	if string(root.PullRequest.RequiresChecks) != `["lint", "test"]` {
		t.Errorf("requiresChecks = %s, want the bytes the daemon sent", root.PullRequest.RequiresChecks)
	}
}

// TestAStateFromTodaysDaemonParses: the same decode reads a daemon that carries none of those
// fields — no tree, no admission.trees, no requiresChecks — so the two shapes are read by one
// reader.
func TestAStateFromTodaysDaemonParses(t *testing.T) {
	const today = `{"daemon":{"project":"INFRA","contract":46},
	  "admission":{"cap":4,"active":["INFRA-1"],"waiting":[],"blocked":[]},
	  "issues":{"INFRA-1":{"key":"INFRA-1","phase":"reviewing","status":"doing",
	    "slot":{"index":0,"admittedAt":"2026-11-03T10:00:00Z"},
	    "workers":{"reviewer":{"claim":{"session":"s-9","state":"working"},"rounds":2}},
	    "pullRequest":{"repo":"acme/widgets","number":7,"head":"abc","state":"open","fixAttempts":0}}}}`
	server := stateServer(t, today)

	state, _, err := New(server.URL, "").State(t.Context())
	if err != nil {
		t.Fatalf("read today's state: %v", err)
	}
	issue := state.Issues["INFRA-1"]
	if issue.Tree != nil {
		t.Errorf("tree = %v, want none: this daemon sends no tree field", *issue.Tree)
	}
	if issue.PullRequest == nil || issue.PullRequest.ChecksVerdict != "" || len(issue.PullRequest.RequiresChecks) != 0 {
		t.Errorf("pull request = %+v, want the two absent fields absent", issue.PullRequest)
	}
}

// TestAMalformedOrRefusedAnswerIsAFailedRead: a body that is not the route's JSON, and a status
// that is not 200, are each a failed read, so the sampler records the failure and leaves the last
// good observations standing.
func TestAMalformedOrRefusedAnswerIsAFailedRead(t *testing.T) {
	malformed := stateServer(t, `{"issues": {`)
	if _, bytes, err := New(malformed.URL, "").State(t.Context()); !errors.Is(err, ErrUnavailable) || bytes == 0 {
		t.Fatalf("a malformed body: err = %v bytes = %d, want a failed read of what it sent", err, bytes)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer refusing.Close()
	_, _, err := New(refusing.URL, "").State(t.Context())
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a 503: err = %v, want a failed read naming the status", err)
	}
}

// relayServer records each call to the operator settings route and answers with status and body.
type relayServer struct {
	*httptest.Server
	authorization string
	body          []byte
}

func newRelayServer(t *testing.T, status int, answer string) *relayServer {
	t.Helper()
	relay := &relayServer{}
	relay.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != OperatorSettingsPath {
			http.NotFound(w, r)
			return
		}
		relay.authorization = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the relayed body: %v", err)
		}
		relay.body = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(relay.Close)
	return relay
}

// TestTheRelaySendsTheChangesTheAuthorAndTheBearer: the body carries the operator's changes and
// the person's login as `by`, the call presents the token file's contents as its bearer, and a
// rewritten file is picked up without a restart.
func TestTheRelaySendsTheChangesTheAuthorAndTheBearer(t *testing.T) {
	relay := newRelayServer(t, http.StatusOK, `{"concurrency":{"applied":"at-once"}}`)
	tokenFile := filepath.Join(t.TempDir(), "daemon-token")
	if err := os.WriteFile(tokenFile, []byte(" first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := New(relay.URL, tokenFile)

	answer, err := client.PutOperatorSettings(t.Context(), SettingsChange{
		Changes: map[string]json.RawMessage{"concurrency": json.RawMessage("4")},
		By:      "alice@example.com",
	})
	if err != nil {
		t.Fatalf("relay a settings change: %v", err)
	}
	if answer.Status != http.StatusOK || string(answer.Body) != `{"concurrency":{"applied":"at-once"}}` {
		t.Errorf("answer = %d %s, want the daemon's own", answer.Status, answer.Body)
	}
	if relay.authorization != "Bearer first-token" {
		t.Errorf("Authorization = %q, want the token file's trimmed contents", relay.authorization)
	}
	var sent struct {
		Changes map[string]json.RawMessage `json:"changes"`
		By      string                     `json:"by"`
	}
	if err := json.Unmarshal(relay.body, &sent); err != nil {
		t.Fatalf("decode the relayed body %s: %v", relay.body, err)
	}
	if sent.By != "alice@example.com" || string(sent.Changes["concurrency"]) != "4" {
		t.Errorf("relayed body = %s, want the changes and the author", relay.body)
	}

	// A projected token rotates in place, and the reader opens the file again once its cache has
	// lapsed, so the next call presents what the kubelet wrote.
	if err := os.WriteFile(tokenFile, []byte("second-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.account.TTL = 0
	if _, err := client.PutOperatorSettings(t.Context(), SettingsChange{Changes: map[string]json.RawMessage{"concurrency": json.RawMessage("5")}, By: "alice@example.com"}); err != nil {
		t.Fatalf("relay after the rotation: %v", err)
	}
	if relay.authorization != "Bearer second-token" {
		t.Errorf("Authorization after the rotation = %q, want the rewritten token", relay.authorization)
	}
}

// TestWithNoTokenFileTheRelaySendsNoBearer: with DISPATCH_LEGION_DAEMON_TOKEN_FILE unset, the
// relay presents nothing and whatever the daemon answers — here a refusal — is what the caller
// sees, status and body unchanged.
func TestWithNoTokenFileTheRelaySendsNoBearer(t *testing.T) {
	relay := newRelayServer(t, http.StatusForbidden, `{"code":"UNAUTHENTICATED","error":"no bearer"}`)

	answer, err := New(relay.URL, "").PutOperatorSettings(t.Context(), SettingsChange{
		Changes: map[string]json.RawMessage{"concurrency": json.RawMessage("4")}, By: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("relay without a token file: %v", err)
	}
	if relay.authorization != "" {
		t.Errorf("Authorization = %q, want none", relay.authorization)
	}
	if answer.Status != http.StatusForbidden || string(answer.Body) != `{"code":"UNAUTHENTICATED","error":"no bearer"}` {
		t.Errorf("answer = %d %s, want the daemon's refusal unchanged", answer.Status, answer.Body)
	}
}

// TestTheStateReadPresentsNoBearerEvenWithATokenFile: GET /legion/v1/state authenticates nobody,
// so the minute's read never sends Dispatch's credential, whatever the deployment configured.
func TestTheStateReadPresentsNoBearerEvenWithATokenFile(t *testing.T) {
	var presented string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"issues":{}}`))
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "daemon-token")
	if err := os.WriteFile(tokenFile, []byte("a-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := New(server.URL, tokenFile).State(t.Context()); err != nil {
		t.Fatalf("read the state: %v", err)
	}
	if presented != "" {
		t.Errorf("the state read presented %q, want no credential", presented)
	}
}

// TestARelayWhoseTokenFileCannotBeReadNeverCallsTheDaemon: what is wrong is Dispatch's own
// deployment, so the call stops at the token with a *brokertoken.AccountTokenError naming the
// file, and the daemon is never asked.
func TestARelayWhoseTokenFileCannotBeReadNeverCallsTheDaemon(t *testing.T) {
	relay := newRelayServer(t, http.StatusOK, `{}`)
	missing := filepath.Join(t.TempDir(), "never-written")

	_, err := New(relay.URL, missing).PutOperatorSettings(t.Context(), SettingsChange{
		Changes: map[string]json.RawMessage{"concurrency": json.RawMessage("4")}, By: "alice@example.com",
	})
	var tokenErr *brokertoken.AccountTokenError
	if !errors.As(err, &tokenErr) {
		t.Fatalf("err = %v, want a *brokertoken.AccountTokenError", err)
	}
	if tokenErr.Path != missing {
		t.Errorf("the error names %q, want the token file", tokenErr.Path)
	}
	if relay.body != nil {
		t.Errorf("the daemon was called with %s, want no call at all", relay.body)
	}
}

// TestADaemonAnswerThatIsNotJSONIsAFailure: the relay passes a daemon's body through, so a body
// that is not JSON is refused here rather than handed to a browser as if the daemon had spoken.
func TestADaemonAnswerThatIsNotJSONIsAFailure(t *testing.T) {
	relay := newRelayServer(t, http.StatusBadGateway, "<html>a proxy wrote this</html>")

	_, err := New(relay.URL, "").PutOperatorSettings(t.Context(), SettingsChange{
		Changes: map[string]json.RawMessage{"concurrency": json.RawMessage("4")}, By: "alice@example.com",
	})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("err = %v, want a failure naming the body that is not JSON", err)
	}
}
