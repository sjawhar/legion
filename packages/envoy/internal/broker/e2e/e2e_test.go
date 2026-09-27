// packages/envoy/internal/broker/e2e/e2e_test.go
//
// TestSpikeContract replays the AGENTC-833 overview document's spike contract (Testing item 9)
// against the broker's own components booted in-process: a real Postgres-backed store, a real
// enroll.Service/requests.Machine/requests.Poller, api.Register on a real httptest.Server, and a
// fake Dispatch httptest.Server standing in for the real thing. Plans B and C repeat this against
// real boxes and pods (Testing item 10).
//
// The 14 cases run as one continuous scenario, not 14 independent fixtures: they share two box
// enrollments (A, B) for operator sjawhar and one pod enrollment (P), and later cases build on
// state earlier ones left behind (a live grant, an expired request, a torn-down server), exactly
// as the spike contract's own numbered steps do. Each case is its own t.Run subtest so `go test
// -v` prints one RAN line and one pass/fail line per case; they still execute in the fixed,
// sequential order Go always runs subtests in.
package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const testAudience = "broker"

func testDatabaseURL(t *testing.T) string {
	url := os.Getenv("BROKER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run Postgres e2e tests")
	}
	return url
}

func str(s string) *string { return &s }

// onceCloser wraps fn so it is safe to call both from a deliberate mid-test teardown (C09) and
// from t.Cleanup at the end: httptest.Server.Close is not itself safe to call twice.
func onceCloser(fn func()) func() {
	var once sync.Once
	return func() { once.Do(fn) }
}

type enrolledAgent struct {
	id  string
	key *ecdsa.PrivateKey
}

// signProof signs a proof binding agent to exactly this method and URL, the shape
// proof.Verifier.Verify checks (htm/htu).
func signProof(t *testing.T, agent enrolledAgent, method, url string) string {
	t.Helper()
	tok, err := proof.Sign(agent.key, agent.id, method, url, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return tok
}

// call sends one HTTP request and decodes its JSON body, if any, into a map. Exactly one of
// proofHeader or bearer should be set (or neither, for authNone routes); the broker's own error
// envelope ({"code":...,"error":...}) and every success body both decode the same way, so a
// caller checks status first and then keys off "code" for a refusal.
func call(t *testing.T, method, url, proofHeader, bearer string, body []byte) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	if proofHeader != "" {
		req.Header.Set("Proof", proofHeader)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("%s %s: response is not JSON: %v (%s)", method, url, err, data)
		}
	}
	return resp.StatusCode, out
}

func createRequest(t *testing.T, srv *httptest.Server, agent enrolledAgent, names []string, reason string) (int, map[string]any, time.Duration) {
	t.Helper()
	url := srv.URL + "/v1/requests"
	body, _ := json.Marshal(map[string]any{"secrets": names, "reason": reason, "issue": nil, "session_id": nil})
	proofTok := signProof(t, agent, http.MethodPost, url)
	start := time.Now()
	status, out := call(t, http.MethodPost, url, proofTok, "", body)
	return status, out, time.Since(start)
}

func getRequest(t *testing.T, srv *httptest.Server, agent enrolledAgent, id string) (int, map[string]any) {
	t.Helper()
	url := srv.URL + "/v1/requests/" + id
	return call(t, http.MethodGet, url, signProof(t, agent, http.MethodGet, url), "", nil)
}

func cancelRequest(t *testing.T, srv *httptest.Server, agent enrolledAgent, id string) (int, map[string]any) {
	t.Helper()
	url := srv.URL + "/v1/requests/" + id + "/cancel"
	return call(t, http.MethodPost, url, signProof(t, agent, http.MethodPost, url), "", nil)
}

func grantValues(t *testing.T, srv *httptest.Server, agent enrolledAgent, grantID string) (int, map[string]any) {
	t.Helper()
	url := srv.URL + "/v1/grants/" + grantID + "/values"
	return call(t, http.MethodPost, url, signProof(t, agent, http.MethodPost, url), "", nil)
}

func revokeGrantAsHuman(t *testing.T, srv *httptest.Server, grantID, login string) (int, map[string]any) {
	t.Helper()
	return call(t, http.MethodPost, srv.URL+"/v1/grants/"+grantID+"/revoke", "", login, nil)
}

func deleteEnrollment(t *testing.T, srv *httptest.Server, launcherToken, id string) int {
	t.Helper()
	status, _ := call(t, http.MethodDelete, srv.URL+"/v1/enrollments/"+id, "", launcherToken, nil)
	return status
}

func createEnrollmentHTTP(t *testing.T, srv *httptest.Server, launcherToken string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal enrollment body: %v", err)
	}
	return call(t, http.MethodPost, srv.URL+"/v1/enrollments", "", launcherToken, raw)
}

// askIDFromRef pulls the ask id out of a "dispatch://<issue>/ask/<id>" ref, POST /v1/requests's
// own wire shape for a pending request.
func askIDFromRef(ref string) string {
	i := strings.LastIndex(ref, "/ask/")
	return ref[i+len("/ask/"):]
}

// redactSecrets replaces every value this suite's fake secrets store hands out with a
// placeholder before it is used in a t.Fatalf diagnostic. Comparisons and control flow always
// use the real value; this project's rule against a secret ever reaching logs, audit, or error
// messages carries no test-only carve-out, so nothing this suite prints on failure may contain
// one either, even a fake, fixture-only value.
func redactSecrets(s string) string {
	return strings.NewReplacer("deel-v1", "[REDACTED]", "auto-v1", "[REDACTED]").Replace(s)
}

// mapKeys lists a decoded JSON response's top-level keys without its values, so a failure
// message can describe an unexpected response's shape without risking a raw secret value if a
// route ever regressed to include one in an error body.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// mintPodToken mints a projected service-account token bound to podUID, the shape
// enroll.K8sPodVerifier.Verify reads — the same pattern as enroll/enroll_test.go's helper of the
// same name, copied here because Go test helpers are not exported across packages.
func mintPodToken(t *testing.T, issuer *oidctest.Issuer, key *oidctest.Key, subject, podUID string) string {
	t.Helper()
	claims := issuer.Claims(subject, testAudience)
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	merged["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return issuer.Mint(t, key, merged)
}

// --- fake Dispatch ---------------------------------------------------------------------------

// fakeAsk is one ask's in-memory state: what dispatch.Client's real HTTP routes read and write,
// plus the fields the fake's own approve() test hook mutates directly.
type fakeAsk struct {
	id       string
	issue    string
	question string
	state    string // "open" or "answered"
	editedAt *string
	answer   *dispatch.Answer
}

// fakeDispatch is a real Dispatch httptest.Server double: it stores asks in memory and serves the
// three routes the broker's dispatch.Client actually calls (POST .../asks, GET /asks/{id}, GET
// /whoami), plus a fake-only POST /asks/{id}/answer that always refuses a bearer with 403
// HUMAN_ONLY — modeling the real constraint that answering an ask needs a human, never a bearer.
// approve() is a Go-level test hook standing in for a human clicking Approve in Dispatch's own
// UI: it mutates an ask's state directly, with no second HTTP round trip.
type fakeDispatch struct {
	mu    sync.Mutex
	asks  map[string]*fakeAsk
	order []string // insertion order, so lastAskID() can answer without a counter
}

func newFakeDispatch() *fakeDispatch { return &fakeDispatch{asks: map[string]*fakeAsk{}} }

func (f *fakeDispatch) server(token string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/issues/{key}/asks", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Question string `json:"question"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		// A uuid, not a small sequential id: this suite's Postgres is shared with every other
		// package's own tests, whose requests table rows also carry an "ask_id" column with no
		// per-test scoping at all — requests.Poller.RunOnce processes every pending row in the
		// whole table. A small sequential id can collide with another concurrently running
		// package's own fake ask ids, letting that package's Poller resolve and apply ITS OWN
		// unrelated answer to one of this suite's rows. A uuid can never collide with anything
		// another package mints.
		id := uuid.NewString()
		f.asks[id] = &fakeAsk{id: id, issue: r.PathValue("key"), question: body.Question, state: "open"}
		f.order = append(f.order, id)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(dispatch.Ask{ID: id, State: "open"})
	})
	mux.HandleFunc("GET /api/v1/asks/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		a, ok := f.asks[r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Dispatch's own GET /api/v1/asks/{id} shape: the ask nested under "ask" beside its
		// replies, edits and followers (internal/dispatch/api's getAsk).
		json.NewEncoder(w).Encode(map[string]any{
			"ask":       dispatch.Ask{ID: a.id, State: a.state, EditedAt: a.editedAt, Answer: a.answer},
			"replies":   []any{},
			"edits":     []any{},
			"followers": []any{},
		})
	})
	mux.HandleFunc("GET /api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		login := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if login == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dispatch.Identity{Kind: "user", Login: login})
	})
	mux.HandleFunc("POST /api/v1/asks/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		// Fake-only: real Dispatch answers an ask only through a human's cookie session, never a
		// bearer. No caller in this suite is meant to succeed here; approve() is the human stand-in.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"code": "HUMAN_ONLY", "error": "asks may only be answered by a human"})
	})
	return httptest.NewServer(mux)
}

// approve stands in for a human clicking Approve on the named ask in Dispatch's own UI.
func (f *fakeDispatch) approve(id, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.asks[id]
	if !ok {
		panic("approve: unknown ask " + id)
	}
	a.state = "answered"
	a.answer = &dispatch.Answer{User: user, Selected: []string{"Approve"}, At: time.Now().UTC()}
}

func (f *fakeDispatch) lastAskID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.order[len(f.order)-1]
}

func (f *fakeDispatch) askCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}

func (f *fakeDispatch) question(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.asks[id]
	if !ok {
		panic("question: unknown ask " + id)
	}
	return a.question
}

// --- broker boot -------------------------------------------------------------------------------

// brokerInstance is one in-process broker: api.Register on a real httptest.Server, backed by st.
// bootBroker is called twice in this suite — once for the initial server, once in C09 for the
// "new process, same Postgres" restart.
type brokerInstance struct {
	srv     *httptest.Server
	machine *requests.Machine
	poller  *requests.Poller
	enr     *enroll.Service
}

func bootBroker(t *testing.T, st *store.Store, dc *dispatch.Client, pod enroll.PodVerifier) brokerInstance {
	t.Helper()
	enr := &enroll.Service{Store: st, Lease: time.Hour, Pod: pod}

	cctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cur, err := rules.NewCurrent(cctx, rules.FileLoader{Path: "testdata/rules.yaml"}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	machine := &requests.Machine{
		Store:    st,
		Rules:    cur,
		Dispatch: dc,
		Secrets: secrets.Fake{
			"dev1/agent-secrets/DEEL_API_KEY": "deel-v1",
			"dev1/agent-secrets/AUTO_TOKEN":   "auto-v1",
		},
		MaxGrant:      time.Hour,
		PendingTTL:    12 * time.Hour,
		StandingIssue: func(context.Context, string) (string, error) { return "AGENTC-1", nil },
		IssueAssignee: func(context.Context, string) (string, error) { return "alice", nil },
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	api.Register(mux, api.Deps{
		PublicURL: srv.URL,
		Enroll:    enr,
		Machine:   machine,
		Proof:     &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, Replay: enr.Replay},
		Dispatch:  dc,
	})

	// Poller.RunOnce is driven explicitly by each case rather than a running goroutine, so every
	// state transition in this test is deterministic. Wake is nil for the whole suite: C11 proves
	// that a plain GET still recovers granted state with no push notification at all.
	poller := &requests.Poller{Machine: machine, Dispatch: dc, Interval: 50 * time.Millisecond, Wake: nil}

	return brokerInstance{srv: srv, machine: machine, poller: poller, enr: enr}
}

// --- agent-secrets subprocess helpers (mirrors cmd/agent-secrets/main_test.go's own pattern) ---

func buildAgentSecrets(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "agent-secrets")
	out, err := exec.Command("go", "build", "-o", binary, "github.com/sjawhar/envoy/cmd/agent-secrets").CombinedOutput()
	if err != nil {
		t.Fatalf("build agent-secrets: %v\n%s", err, out)
	}
	return binary
}

// writeKeyDir writes agent's real signing key and enrollment id into a fresh temp dir, the shape
// AGENT_SECRETS_KEY_DIR holds, so the real agent-secrets binary can sign proofs as that already-
// enrolled session.
func writeKeyDir(t *testing.T, agent enrolledAgent) string {
	t.Helper()
	dir := t.TempDir()
	der, err := x509.MarshalPKCS8PrivateKey(agent.key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pemBytes, 0o600); err != nil {
		t.Fatalf("write key.pem: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "enrollment"), []byte(agent.id+"\n"), 0o600); err != nil {
		t.Fatalf("write enrollment: %v", err)
	}
	return dir
}

func runAgentSecrets(t *testing.T, binary, broker, keyDir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_URL="+broker, "AGENT_SECRETS_KEY_DIR="+keyDir)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err == nil {
		return out.String(), errOut.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), errOut.String(), exitErr.ExitCode()
	}
	t.Fatalf("run agent-secrets %v: %v\nstdout: %s\nstderr: %s", args, err, out.String(), errOut.String())
	return "", "", -1
}

// --- the scenario --------------------------------------------------------------------------

func TestSpikeContract(t *testing.T) {
	outer := t // t.Run subtests below shadow this parameter; use outer.Cleanup for anything
	// that must outlive one subtest (env2's server, its store), never the subtest's own t.Cleanup.
	ctx := context.Background()
	dbURL := testDatabaseURL(t)

	// A fake Dispatch server that outlives both broker "boots" in C09, exactly as real Dispatch —
	// an external service — would.
	fd := newFakeDispatch()
	const dispatchToken = "broker-dispatch-token"
	dispatchSrv := fd.server(dispatchToken)
	t.Cleanup(dispatchSrv.Close)
	dc := dispatch.New(dispatchSrv.URL, dispatchToken, dispatchSrv.Client())

	// A pod verifier backed by a real local OIDC issuer, also shared across both boots.
	issuer := oidctest.New(t)
	oidcKey := issuer.PublishKey(t, "signing-key")
	verifier, err := oidc.New(ctx, issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	podVerifier := enroll.K8sPodVerifier{Verifier: verifier}

	st1, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st1.Pool.Close() })
	if err := st1.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	env1 := bootBroker(t, st1, dc, podVerifier)
	closeSrv1 := onceCloser(env1.srv.Close)
	t.Cleanup(closeSrv1)

	// One launcher credential mints both of operator sjawhar's box enrollments, A and B.
	_, launcherToken, err := env1.enr.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := env1.enr.AuthenticateLauncher(ctx, launcherToken)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	mkBox := func(name string) enrolledAgent {
		t.Helper()
		key, err := proof.NewKey()
		if err != nil {
			t.Fatalf("proof.NewKey: %v", err)
		}
		thumb, err := proof.Thumbprint(&key.PublicKey)
		if err != nil {
			t.Fatalf("proof.Thumbprint: %v", err)
		}
		e, err := env1.enr.Create(ctx, cred, enroll.Enrollment{
			Kind: "box", RuntimeID: "box-" + name + "-" + t.Name(), Operator: str("sjawhar"),
			ApproverKind: "operator", Thumbprint: thumb,
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		return enrolledAgent{id: e.ID.String(), key: key}
	}
	enrA := mkBox("a")
	enrB := mkBox("b")

	// A pod enrollment P, minted through the real HTTP route (unlike A/B) because the Pod case at
	// the end needs to attempt a SECOND, mismatched enrollment through that same route.
	_, serviceLauncherToken, err := env1.enr.MintLauncherCredential(ctx, nil, str("k8s"), "cluster", "")
	if err != nil {
		t.Fatalf("MintLauncherCredential (service): %v", err)
	}
	podKey, err := proof.NewKey()
	if err != nil {
		t.Fatalf("proof.NewKey: %v", err)
	}
	podThumb, err := proof.Thumbprint(&podKey.PublicKey)
	if err != nil {
		t.Fatalf("proof.Thumbprint: %v", err)
	}
	validPodToken := mintPodToken(t, issuer, oidcKey, "system:serviceaccount:default:agent-p", "pod-1")
	status, podOut := createEnrollmentHTTP(t, env1.srv, serviceLauncherToken, map[string]any{
		"kind": "pod", "runtime_id": "pod-1", "operator": nil,
		"approver":   map[string]any{"kind": "operator"},
		"thumbprint": podThumb, "session_id": nil, "pod_token": validPodToken,
	})
	if status != http.StatusCreated {
		t.Fatalf("enroll P: want 201, got %d %v", status, podOut)
	}
	podEnrollmentID, _ := podOut["enrollment_id"].(string)
	if podEnrollmentID == "" {
		t.Fatalf("enroll P: response carries no enrollment_id: %v", podOut)
	}

	agentSecretsBin := buildAgentSecrets(t)

	// Values threaded between subtests, matching the spike contract's own sequential narrative:
	// later cases build on state earlier ones left behind.
	var (
		dealReqID     string
		dealAskID     string
		dealGrantID   string
		combinedAskID string
		reqID6        string
		askID6        string
		env2          brokerInstance
	)

	t.Run("C01_AutomaticDenyAndPendingApprovalDecisions", func(t *testing.T) {
		t.Log("RAN: C01 - an automatic secret grants immediately, a denied secret opens no ask, and an approval secret opens exactly one ask naming the secret and the operator with no value")

		status, out, _ := createRequest(t, env1.srv, enrA, []string{"AUTO_TOKEN"}, "need it for automation")
		if status != http.StatusOK || out["state"] != "granted" || out["grant_id"] == nil {
			t.Fatalf("AUTO_TOKEN: want granted with a grant_id, got %d %v", status, out)
		}
		if fd.askCount() != 0 {
			t.Fatalf("an automatic grant must open no ask, got %d", fd.askCount())
		}

		status, out, _ = createRequest(t, env1.srv, enrA, []string{"DENIED_KEY"}, "need it for something denied")
		if status != http.StatusOK || out["state"] != "denied" || out["grant_id"] != nil {
			t.Fatalf("DENIED_KEY: want denied with no grant, got %d %v", status, out)
		}
		if fd.askCount() != 0 {
			t.Fatalf("a denial must open no ask, got %d", fd.askCount())
		}

		status, out, _ = createRequest(t, env1.srv, enrA, []string{"DEEL_API_KEY"}, "need it for testing")
		if status != http.StatusOK || out["state"] != "pending" || out["grant_id"] != nil || out["ask"] == nil {
			t.Fatalf("DEEL_API_KEY: want pending with an ask ref, got %d %v", status, out)
		}
		if fd.askCount() != 1 {
			t.Fatalf("want exactly one ask opened so far, got %d", fd.askCount())
		}
		dealReqID, _ = out["request_id"].(string)
		askRef, _ := out["ask"].(string)
		dealAskID = askIDFromRef(askRef)
		question := fd.question(dealAskID)
		if !strings.Contains(question, "DEEL_API_KEY") || !strings.Contains(question, "sjawhar") {
			t.Fatalf("ask question must name the secret and the operator, got %q", question)
		}
		if strings.Contains(question, "deel-v1") {
			t.Fatalf("ask question contained the secret's value")
		}
	})

	t.Run("C02_RequestRespondsWithinOneSecond", func(t *testing.T) {
		t.Log("RAN: C02 - POST /v1/requests answers within one second and always carries a request_id")
		status, out, elapsed := createRequest(t, env1.srv, enrA, []string{"AUTO_TOKEN"}, "timing check")
		if status != http.StatusOK {
			t.Fatalf("want 200, got %d %v", status, out)
		}
		if elapsed > time.Second {
			t.Fatalf("POST /v1/requests took %s, want under 1s", elapsed)
		}
		if id, _ := out["request_id"].(string); id == "" {
			t.Fatalf("response carries no request_id: %v", out)
		}
	})

	t.Run("C03_ApprovalGrantsAndValuesReadRepeatedly", func(t *testing.T) {
		t.Log("RAN: C03 - approving the ask and polling once grants the request, and reading the grant's values twice both return the value")
		fd.approve(dealAskID, "sjawhar")
		if err := env1.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce: %v", err)
		}
		status, out := getRequest(t, env1.srv, enrA, dealReqID)
		if status != http.StatusOK || out["state"] != "granted" || out["grant_id"] == nil {
			t.Fatalf("want granted with a grant_id, got %d %v", status, out)
		}
		dealGrantID, _ = out["grant_id"].(string)

		for i := 0; i < 2; i++ {
			status, values := grantValues(t, env1.srv, enrA, dealGrantID)
			if status != http.StatusOK {
				t.Fatalf("grant values read %d: want 200, got %d (response keys = %v)", i, status, mapKeys(values))
			}
			got, _ := values["values"].(map[string]any)
			if got["DEEL_API_KEY"] != "deel-v1" {
				t.Fatalf("grant values read %d: DEEL_API_KEY did not match the fake secrets store's value", i)
			}
		}
		if fd.askCount() != 1 {
			t.Fatalf("approving and reading a grant must not open another ask, got %d", fd.askCount())
		}
	})

	t.Run("C04_CrossSessionAndReplayRejections", func(t *testing.T) {
		t.Log("RAN: C04 - another session cannot read or spend A's request, cannot replay A's proof, and cannot forge A's identity with its own key")
		status, out := getRequest(t, env1.srv, enrB, dealReqID)
		if status != http.StatusForbidden || out["code"] != "NOT_YOURS" {
			t.Fatalf("B reading A's request: want 403 NOT_YOURS, got %d %v", status, out)
		}
		status, out = grantValues(t, env1.srv, enrB, dealGrantID)
		if status != http.StatusForbidden || out["code"] != "NOT_YOURS" {
			t.Fatalf("B spending A's grant: want 403 NOT_YOURS, got %d code=%v", status, out["code"])
		}

		readURL := env1.srv.URL + "/v1/requests/" + dealReqID
		proofTok := signProof(t, enrA, http.MethodGet, readURL)
		status, out = call(t, http.MethodGet, readURL, proofTok, "", nil)
		if status != http.StatusOK {
			t.Fatalf("first use of a fresh proof: want 200, got %d %v", status, out)
		}
		status, out = call(t, http.MethodGet, readURL, proofTok, "", nil)
		if status != http.StatusUnauthorized || out["code"] != "PROOF_INVALID" {
			t.Fatalf("replaying the same proof: want 401 PROOF_INVALID, got %d %v", status, out)
		}

		forgedTok, err := proof.Sign(enrB.key, enrA.id, http.MethodGet, readURL, time.Now())
		if err != nil {
			t.Fatalf("proof.Sign: %v", err)
		}
		status, out = call(t, http.MethodGet, readURL, forgedTok, "", nil)
		if status != http.StatusUnauthorized || out["code"] != "PROOF_INVALID" {
			t.Fatalf("B's key claiming to be A: want 401 PROOF_INVALID, got %d %v", status, out)
		}
	})

	t.Run("C05_NeverEnrolledKeyIsRejected", func(t *testing.T) {
		t.Log("RAN: C05 - a proof signed by a key with no enrollment row at all is rejected")
		strangerKey, err := proof.NewKey()
		if err != nil {
			t.Fatalf("proof.NewKey: %v", err)
		}
		url := env1.srv.URL + "/v1/requests"
		tok, err := proof.Sign(strangerKey, uuid.NewString(), http.MethodPost, url, time.Now())
		if err != nil {
			t.Fatalf("proof.Sign: %v", err)
		}
		body, _ := json.Marshal(map[string]any{"secrets": []string{"AUTO_TOKEN"}, "reason": "", "issue": nil, "session_id": nil})
		status, out := call(t, http.MethodPost, url, tok, "", body)
		if status != http.StatusUnauthorized || out["code"] != "PROOF_INVALID" {
			t.Fatalf("want 401 PROOF_INVALID, got %d %v", status, out)
		}
	})

	t.Run("C06_AgentSecretsExecFormPendingThenGranted", func(t *testing.T) {
		t.Log("RAN: C06 - the agent-secrets binary refuses to run its child while a requested secret is still pending, and runs it once approved")
		keyDir := writeKeyDir(t, enrA)

		stdout, _, exit := runAgentSecrets(t, agentSecretsBin, env1.srv.URL, keyDir,
			"AUTO_TOKEN", "DEEL_API_KEY", "--wait", "0s", "--", "sh", "-c", "echo ran")
		const exitPending = 75
		if exit != exitPending {
			t.Fatalf("with DEEL_API_KEY pending: want exit %d, got %d (stdout=%q)", exitPending, exit, stdout)
		}
		if strings.Contains(stdout, "ran") {
			t.Fatalf("the child must never run while a request is pending, got stdout=%q", stdout)
		}
		combinedAskID = fd.lastAskID()
		fd.approve(combinedAskID, "sjawhar")
		if err := env1.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce: %v", err)
		}

		stdout, stderr, exit := runAgentSecrets(t, agentSecretsBin, env1.srv.URL, keyDir,
			"AUTO_TOKEN", "DEEL_API_KEY", "--", "sh", "-c", "echo ran")
		if exit != 0 {
			t.Fatalf("after approval: want exit 0, got %d (stdout=%q stderr=%q)", exit, redactSecrets(stdout), redactSecrets(stderr))
		}
		if !strings.Contains(stdout, "ran") {
			t.Fatalf("after approval: child did not run, stdout=%q", redactSecrets(stdout))
		}
	})

	t.Run("C07_ExpiredPendingStaysExpiredEvenIfApprovedAfterward", func(t *testing.T) {
		t.Log("RAN: C07 - a pending request past its own deadline expires, and a later approval never grants it")
		_, out, _ := createRequest(t, env1.srv, enrB, []string{"DEEL_API_KEY"}, "will expire")
		reqID, _ := out["request_id"].(string)
		askID := askIDFromRef(out["ask"].(string))

		if _, err := env1.machine.Store.Pool.Exec(ctx,
			`update requests set pending_expires_at = now() - interval '1 hour' where id=$1`, reqID); err != nil {
			t.Fatalf("force expiry: %v", err)
		}
		if err := env1.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce: %v", err)
		}
		status, got := getRequest(t, env1.srv, enrB, reqID)
		if status != http.StatusOK || got["state"] != "expired" {
			t.Fatalf("want expired, got %d %v", status, got)
		}

		fd.approve(askID, "sjawhar")
		if err := env1.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce: %v", err)
		}
		status, got = getRequest(t, env1.srv, enrB, reqID)
		if status != http.StatusOK || got["state"] != "expired" || got["grant_id"] != nil {
			t.Fatalf("an approval after expiry must not grant, got %d %v", status, got)
		}
	})

	t.Run("C08_CancelRevokeAndDeleteEnrollment", func(t *testing.T) {
		t.Log("RAN: C08 - cancel-then-approve is a no-op, a human revoke ends a grant, and deleting the enrollment ends its remaining grant and refuses its proof")

		status, out := revokeGrantAsHuman(t, env1.srv, dealGrantID, "sjawhar")
		if status != http.StatusOK {
			t.Fatalf("revoke as sjawhar: want 200, got %d %v", status, out)
		}
		status, out = grantValues(t, env1.srv, enrA, dealGrantID)
		if status != http.StatusForbidden || out["code"] != "GRANT_NOT_LIVE" {
			t.Fatalf("values on a revoked grant: want 403 GRANT_NOT_LIVE, got %d code=%v", status, out["code"])
		}

		_, out, _ = createRequest(t, env1.srv, enrA, []string{"DEEL_API_KEY"}, "again, now that the old grant is gone")
		reqID3, _ := out["request_id"].(string)
		askID3 := askIDFromRef(out["ask"].(string))
		status, _ = cancelRequest(t, env1.srv, enrA, reqID3)
		if status != http.StatusOK {
			t.Fatalf("cancel: want 200, got %d", status)
		}
		fd.approve(askID3, "sjawhar")
		if err := env1.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce: %v", err)
		}
		status, got := getRequest(t, env1.srv, enrA, reqID3)
		if status != http.StatusOK || got["state"] != "cancelled" || got["grant_id"] != nil {
			t.Fatalf("approving a cancelled request must never grant it, got %d %v", status, got)
		}
		var grantCount int
		if err := env1.machine.Store.Pool.QueryRow(ctx, `select count(*) from grants where request_id=$1`, reqID3).Scan(&grantCount); err != nil {
			t.Fatalf("count grants: %v", err)
		}
		if grantCount != 0 {
			t.Fatalf("approving a cancelled request must create zero grant rows, got %d", grantCount)
		}

		status = deleteEnrollment(t, env1.srv, launcherToken, enrA.id)
		if status != http.StatusNoContent {
			t.Fatalf("delete enrollment A: want 204, got %d", status)
		}
		var combinedReqID, combinedGrantID string
		// Scoped by enrollment_id, not ask_id alone: enrA.id is a fresh uuid this run alone
		// minted, and combinedAskID is itself a globally unique uuid (fakeDispatch mints one via
		// uuid.NewString() per ask, never a small reused string), so either one alone already
		// identifies this run's own row in this shared, never-truncated Postgres instance; the
		// pair together makes that explicit rather than relying on just one of them.
		if err := env1.machine.Store.Pool.QueryRow(ctx, `select id from requests where ask_id=$1 and enrollment_id=$2`, combinedAskID, enrA.id).Scan(&combinedReqID); err != nil {
			t.Fatalf("look up C06's combined request: %v", err)
		}
		var revokedAt *time.Time
		if err := env1.machine.Store.Pool.QueryRow(ctx,
			`select id, revoked_at from grants where request_id=$1 and enrollment_id=$2`, combinedReqID, enrA.id).
			Scan(&combinedGrantID, &revokedAt); err != nil {
			t.Fatalf("look up A's combined grant: %v", err)
		}
		if revokedAt == nil {
			t.Fatalf("deleting A's enrollment must revoke its remaining live grant, but grant %s is still live", combinedGrantID)
		}
		status, _ = getRequest(t, env1.srv, enrA, dealReqID)
		if status != http.StatusUnauthorized {
			t.Fatalf("A's proof must be refused once its enrollment is deleted, got %d", status)
		}
	})

	t.Run("C09_RestartOnSamePostgresRecoversPendingState", func(t *testing.T) {
		t.Log("RAN: C09 - a fresh broker process on the same Postgres grants a request it never held in memory")
		_, out, _ := createRequest(t, env1.srv, enrB, []string{"DEEL_API_KEY"}, "restart test")
		reqID4, _ := out["request_id"].(string)
		askID4 := askIDFromRef(out["ask"].(string))

		closeSrv1()
		st2, err := store.Open(ctx, dbURL)
		if err != nil {
			t.Fatalf("open second store: %v", err)
		}
		outer.Cleanup(func() { st2.Pool.Close() })
		if err := st2.Migrate(ctx); err != nil {
			t.Fatalf("migrate second store: %v", err)
		}
		env2 = bootBroker(outer, st2, dc, podVerifier)
		outer.Cleanup(env2.srv.Close)

		fd.approve(askID4, "sjawhar")
		if err := env2.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce on the new process: %v", err)
		}
		status, got := getRequest(t, env2.srv, enrB, reqID4)
		if status != http.StatusOK || got["state"] != "granted" || got["grant_id"] == nil {
			t.Fatalf("after restart: want granted with a grant_id, got %d %v", status, got)
		}
	})

	t.Run("C10_WrongApproverDeniesAndAnswerRouteIsHumanOnly", func(t *testing.T) {
		t.Log("RAN: C10 - mallory cannot approve someone else's request, and the fake's answer route refuses a bearer exactly like Dispatch")
		_, out, _ := createRequest(t, env2.srv, enrB, []string{"AUTO_TOKEN", "DEEL_API_KEY"}, "combo for mallory test")
		reqID5, _ := out["request_id"].(string)
		askID5 := askIDFromRef(out["ask"].(string))
		fd.approve(askID5, "mallory")
		if err := env2.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce: %v", err)
		}
		full, err := env2.machine.Get(ctx, reqID5)
		if err != nil {
			t.Fatalf("machine.Get: %v", err)
		}
		if full.State != "denied" || full.Detail == nil || !strings.Contains(*full.Detail, "answered by mallory") {
			detail := "<nil>"
			if full.Detail != nil {
				detail = *full.Detail
			}
			t.Fatalf("want denied with detail containing 'answered by mallory', got state=%s detail=%q", full.State, detail)
		}

		_, out, _ = createRequest(t, env2.srv, enrB, []string{"AUTO_TOKEN", "DEEL_API_KEY"}, "combo again")
		reqID6, _ = out["request_id"].(string)
		askID6 = askIDFromRef(out["ask"].(string))
		answerStatus, answerOut := call(t, http.MethodPost, dispatchSrv.URL+"/api/v1/asks/"+askID6+"/answer", "", "agent-session-1", []byte(`{}`))
		if answerStatus != http.StatusForbidden || answerOut["code"] != "HUMAN_ONLY" {
			t.Fatalf("want 403 HUMAN_ONLY from the fake's answer route, got %d %v", answerStatus, answerOut)
		}
		if err := env2.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce: %v", err)
		}
		status, got := getRequest(t, env2.srv, enrB, reqID6)
		if status != http.StatusOK || got["state"] != "pending" {
			t.Fatalf("a bearer's answer attempt must not move the request; got %d %v", status, got)
		}
	})

	t.Run("C11_NilWakeStillRecoversStateThroughAPlainRead", func(t *testing.T) {
		t.Log("RAN: C11 - with Wake nil throughout, GET /v1/requests/{id} alone recovers the granted state")
		if env2.poller.Wake != nil {
			t.Fatalf("this suite wires every poller with Wake: nil; C11 depends on that")
		}
		fd.approve(askID6, "sjawhar")
		if err := env2.poller.RunOnce(ctx); err != nil {
			t.Fatalf("poller.RunOnce: %v", err)
		}
		status, got := getRequest(t, env2.srv, enrB, reqID6)
		if status != http.StatusOK || got["state"] != "granted" || got["grant_id"] == nil {
			t.Fatalf("want granted with a grant_id recovered by a plain read, got %d %v", status, got)
		}
	})

	t.Run("C12_ChildEnvironmentIsolationFromTheParentProcess", func(t *testing.T) {
		t.Log("RAN: C12 - the agent-secrets child sees the granted secret in its environment; the parent test process never does")
		for _, e := range os.Environ() {
			if strings.HasPrefix(e, "DEEL_API_KEY=") {
				t.Fatalf("the parent test process must never carry DEEL_API_KEY in its own environment")
			}
		}
		keyDir := writeKeyDir(t, enrB)
		stdout, stderr, exit := runAgentSecrets(t, agentSecretsBin, env2.srv.URL, keyDir,
			"DEEL_API_KEY", "--", "sh", "-c", `printf 'child-sees:%s\n' "$DEEL_API_KEY"`)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0: stdout=%q stderr=%q", exit, redactSecrets(stdout), redactSecrets(stderr))
		}
		if !strings.Contains(stdout, "child-sees:deel-v1") {
			t.Fatalf("child did not see the granted value: stdout=%q", redactSecrets(stdout))
		}
	})

	t.Run("Policy_AmbiguousRequesterRefusedAtParseAndLiveRulesUnchanged", func(t *testing.T) {
		t.Log("RAN: Policy - a second box/sjawhar entry for the same secret is refused by rules.Parse, and a bad reload keeps the live rules requiring approval")
		original, err := os.ReadFile("testdata/rules.yaml")
		if err != nil {
			t.Fatalf("read testdata/rules.yaml: %v", err)
		}
		ambiguous, err := os.ReadFile("testdata/rules_ambiguous.yaml")
		if err != nil {
			t.Fatalf("read testdata/rules_ambiguous.yaml: %v", err)
		}

		if _, err := rules.Parse(ambiguous); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("rules.Parse must refuse the ambiguous file with an 'ambiguous' error, got %v", err)
		}

		path := filepath.Join(t.TempDir(), "rules.yaml")
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatalf("write temp rules.yaml: %v", err)
		}
		alarmed := make(chan error, 1)
		cctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cur, err := rules.NewCurrent(cctx, rules.FileLoader{Path: path}, 10*time.Millisecond, func(e error) {
			select {
			case alarmed <- e:
			default:
			}
		})
		if err != nil {
			t.Fatalf("rules.NewCurrent: %v", err)
		}
		firstVersion := cur.Get().Version

		if err := os.WriteFile(path, ambiguous, 0o600); err != nil {
			t.Fatalf("write ambiguous rules.yaml: %v", err)
		}
		select {
		case <-alarmed:
		case <-time.After(2 * time.Second):
			t.Fatal("expected the loader alarm to fire on the ambiguous reload")
		}
		if cur.Get().Version != firstVersion {
			t.Fatalf("an invalid reload must not replace the live rules")
		}
		decision, err := cur.Get().Evaluate("DEEL_API_KEY", rules.Requester{Kind: "box", Operator: "sjawhar"})
		if err != nil || decision.Outcome != "approval" {
			t.Fatalf("live rules must still require approval after the refused reload, got %+v (%v)", decision, err)
		}
	})

	t.Run("Pod_MismatchedPodTokenIsRefused", func(t *testing.T) {
		t.Log("RAN: Pod - an enrollment attempt whose token names a different pod uid than runtime_id is refused")
		mismatchToken := mintPodToken(t, issuer, oidcKey, "system:serviceaccount:default:agent-p2", "pod-2")
		status, out := createEnrollmentHTTP(t, env2.srv, serviceLauncherToken, map[string]any{
			"kind": "pod", "runtime_id": "pod-1", "operator": nil,
			"approver":   map[string]any{"kind": "operator"},
			"thumbprint": podThumb, "session_id": nil, "pod_token": mismatchToken,
		})
		if status != http.StatusForbidden || out["code"] != "POD_IDENTITY_MISMATCH" {
			t.Fatalf("want 403 POD_IDENTITY_MISMATCH, got %d %v", status, out)
		}
		if _, err := env2.enr.Get(ctx, podEnrollmentID); err != nil {
			t.Fatalf("P's original enrollment must remain live and untouched, got %v", err)
		}
	})
}
