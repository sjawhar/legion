package dispatch

// These tests run Client against Dispatch's own HTTP handler (internal/dispatch/api) on a
// migrated Postgres database, so the broker's reading of every route it calls is checked
// against the real wire shape rather than against a fake written from the same assumptions.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"

	dispatchapi "github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	dispatchstoretest "github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestMain runs the package under Dispatch's storetest, which gives each contract test a cloned,
// migrated Dispatch database. CI points BROKER_TEST_DATABASE_URL at the same server as
// DISPATCH_TEST_DATABASE_URL, so the broker's variable alone is enough to run them.
func TestMain(m *testing.M) {
	if os.Getenv(dispatchstoretest.DatabaseURLEnv) == "" {
		if url := os.Getenv("BROKER_TEST_DATABASE_URL"); url != "" {
			os.Setenv(dispatchstoretest.DatabaseURLEnv, url)
		}
	}
	os.Exit(dispatchstoretest.Main(m))
}

const (
	contractAgentToken = "contract-agent-token"
	contractProject    = "SECRETS"
)

// realDispatch serves Dispatch's own API over HTTP with alice and bob as the allowed humans (the
// X-Dispatch-User header names the caller) and contractAgentToken as the shared agent bearer.
func realDispatch(t *testing.T) *httptest.Server {
	t.Helper()
	database := dispatchstoretest.Open(t)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	allowed := map[string]struct{}{"alice": {}, "bob": {}}
	deps, err := dispatchapi.NewDeps(dispatchapi.DepsInput{
		Store:         database,
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed},
		AllowedLogins: allowed,
		AgentToken:    contractAgentToken,
		ServerURL:     "https://dispatch.example",
		Docs:          documents,
		Events:        broker,
	})
	if err != nil {
		t.Fatalf("dispatch api dependencies: %v", err)
	}
	mux := http.NewServeMux()
	dispatchapi.Register(mux, deps)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	human(t, srv, http.MethodPost, "/api/v1/projects", map[string]any{"key": contractProject, "name": "Secrets"}, "alice", http.StatusCreated, nil)
	return srv
}

// human sends one request as a signed-in Dispatch user and decodes the response into out.
func human(t *testing.T, srv *httptest.Server, method, path string, body any, login string, wantStatus int, out any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode %s %s: %v", method, path, err)
	}
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("X-Dispatch-User", login)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		var envelope map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&envelope)
		t.Fatalf("%s %s as %s: status %d, want %d (%v)", method, path, login, resp.StatusCode, wantStatus, envelope)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
}

var issueKeyShape = regexp.MustCompile(`^SECRETS-[0-9]+$`)

func TestClientAgainstRealDispatchIssuesAndAsks(t *testing.T) {
	srv := realDispatch(t)
	ctx := context.Background()
	c := New(srv.URL, contractAgentToken, srv.Client())

	key, err := c.CreateIssue(ctx, contractProject, "Secret requests: alice", new("alice"), []string{"agent-secrets"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if !issueKeyShape.MatchString(key) {
		t.Fatalf("CreateIssue key = %q, want a %s-<n> key", key, contractProject)
	}

	issues, err := c.ListIssues(ctx, contractProject, "agent-secrets")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Key != key || issues[0].Title != "Secret requests: alice" ||
		issues[0].Assignee == nil || *issues[0].Assignee != "alice" {
		t.Fatalf("ListIssues = %+v, want the one standing issue %s assigned to alice", issues, key)
	}

	assignee, err := c.IssueAssignee(ctx, key)
	if err != nil || assignee != "alice" {
		t.Fatalf("IssueAssignee(%s) = %q, %v, want alice", key, assignee, err)
	}

	opened, err := c.CreateAsk(ctx, key, "Release DEEL_API_KEY to sjawhar's box?", []Option{{Label: "Approve"}, {Label: "Deny"}}, "med")
	if err != nil {
		t.Fatalf("CreateAsk: %v", err)
	}
	if _, err := uuid.Parse(opened.ID); err != nil || opened.State != "open" || opened.Answer != nil {
		t.Fatalf("CreateAsk = %+v, want an open ask with a uuid id", opened)
	}

	read, err := c.GetAsk(ctx, opened.ID)
	if err != nil {
		t.Fatalf("GetAsk (open): %v", err)
	}
	if read.ID != opened.ID || read.State != "open" || read.Answer != nil || read.EditedAt != nil {
		t.Fatalf("GetAsk (open) = %+v, want the open ask %s with no answer and no edit", read, opened.ID)
	}

	// A human whose GitHub display login carries capitals answers; Dispatch records that casing.
	human(t, srv, http.MethodPost, "/api/v1/asks/"+opened.ID+"/answer",
		map[string]any{"selected": []string{"Approve"}, "expected_edited_at": nil}, "Alice", http.StatusOK, nil)
	answered, err := c.GetAsk(ctx, opened.ID)
	if err != nil {
		t.Fatalf("GetAsk (answered): %v", err)
	}
	if answered.ID != opened.ID || answered.State != "answered" || answered.Answer == nil ||
		answered.Answer.User != "Alice" || len(answered.Answer.Selected) != 1 || answered.Answer.Selected[0] != "Approve" ||
		answered.Answer.At.IsZero() {
		t.Fatalf("GetAsk (answered) = %+v (answer %+v), want answered by Alice selecting Approve", answered, answered.Answer)
	}
	// The approval checks accept that answer from the approver Dispatch stores in lowercase.
	if decision, reason, err := Verdict(answered, opened.ID, nil, "alice"); err != nil || decision != Approved {
		t.Fatalf("Verdict(answered by Alice, approver alice) = %v %q %v, want Approved", decision, reason, err)
	}
	if decision, _, err := Verdict(answered, opened.ID, nil, "bob"); err != nil || decision != Denied {
		t.Fatalf("Verdict(answered by Alice, approver bob) = %v %v, want Denied", decision, err)
	}
	// An answered ask cannot be retracted; Dispatch says so with a 409.
	if err := c.RetractAsk(ctx, opened.ID, "no longer needed"); err == nil {
		t.Fatal("RetractAsk(answered ask) succeeded, want Dispatch's refusal")
	} else if dispatchErr, ok := AsError(err); !ok || dispatchErr.Status != http.StatusConflict {
		t.Fatalf("RetractAsk(answered ask) = %v, want a 409 *Error", err)
	}

	// An open ask whose request ended is retracted, and reads back resolved.
	stale, err := c.CreateAsk(ctx, key, "Release AUTO_TOKEN?", []Option{{Label: "Approve"}, {Label: "Deny"}}, "med")
	if err != nil {
		t.Fatalf("CreateAsk(stale): %v", err)
	}
	if err := c.RetractAsk(ctx, stale.ID, "the request was cancelled"); err != nil {
		t.Fatalf("RetractAsk(open ask): %v", err)
	}
	if retracted, err := c.GetAsk(ctx, stale.ID); err != nil || retracted.State != "resolved" || retracted.Answer != nil {
		t.Fatalf("GetAsk(retracted) = %+v, %v, want resolved with no answer", retracted, err)
	}
	if decision, _, err := Verdict(mustGetAsk(t, c, stale.ID), stale.ID, nil, "alice"); err != nil || decision != Denied {
		t.Fatalf("Verdict(retracted ask) = %v %v, want Denied", decision, err)
	}

	// A closed standing issue is no candidate: ListIssues answers open issues only.
	human(t, srv, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"status": "done"}, "alice", http.StatusOK, nil)
	if open, err := c.ListIssues(ctx, contractProject, "agent-secrets"); err != nil || len(open) != 0 {
		t.Fatalf("ListIssues after closing %s = %+v, %v, want no open issue", key, open, err)
	}

	_, err = c.GetAsk(ctx, uuid.NewString())
	if dispatchErr, ok := AsError(err); !ok || dispatchErr.Status != http.StatusNotFound || dispatchErr.Unavailable() {
		t.Fatalf("GetAsk(unknown) error = %v, want a 404 *Error", err)
	}
}

func mustGetAsk(t *testing.T, c *Client, id string) Ask {
	t.Helper()
	ask, err := c.GetAsk(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAsk(%s): %v", id, err)
	}
	return ask
}

func TestClientAgainstRealDispatchWhoami(t *testing.T) {
	srv := realDispatch(t)
	ctx := context.Background()
	c := New(srv.URL, contractAgentToken, srv.Client())

	var minted struct {
		Token string `json:"token"`
	}
	human(t, srv, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "broker-contract"}, "alice", http.StatusCreated, &minted)
	if minted.Token == "" {
		t.Fatal("minting alice's personal agent token returned no token")
	}

	personal, err := c.Whoami(ctx, minted.Token)
	if err != nil {
		t.Fatalf("Whoami(alice's personal token): %v", err)
	}
	if personal.Kind != "agent" || personal.Owner == nil || *personal.Owner != "alice" {
		t.Fatalf("Whoami(alice's personal token) = %+v, want kind agent owned by alice", personal)
	}

	shared, err := c.Whoami(ctx, contractAgentToken)
	if err != nil {
		t.Fatalf("Whoami(shared token): %v", err)
	}
	if shared.Kind != "agent" || shared.Owner != nil {
		t.Fatalf("Whoami(shared token) = %+v, want kind agent with no owner", shared)
	}

	_, err = c.Whoami(ctx, "not-a-token")
	if dispatchErr, ok := AsError(err); !ok || dispatchErr.Status != http.StatusUnauthorized {
		t.Fatalf("Whoami(unknown bearer) error = %v, want a 401 *Error", err)
	}
}
