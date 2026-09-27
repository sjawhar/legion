package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const testAudience = "broker"

func str(s string) *string { return &s }

// openCall records one askOpener.CreateAsk invocation for a test to assert against.
type openCall struct {
	issue, question, urgency string
	options                  []dispatch.Option
}

// fakeOpener is the machine's askOpener: it always opens successfully, returning a fresh open ask
// with a unique id per call (ask-1, ask-2, ...), and records every call so a test can assert how
// many asks were opened, what they said, and which request each one belongs to.
type fakeOpener struct {
	calls []openCall
	next  int
}

func (f *fakeOpener) CreateAsk(_ context.Context, issue, question string, options []dispatch.Option, urgency string) (dispatch.Ask, error) {
	f.next++
	f.calls = append(f.calls, openCall{issue: issue, question: question, urgency: urgency, options: options})
	return dispatch.Ask{ID: fmt.Sprintf("ask-%d", f.next), State: "open"}, nil
}

// gatedOpener is an askOpener for concurrency tests: every CreateAsk announces itself on entered,
// then waits for gate to close before answering with err, or with a fresh open ask when err is nil.
type gatedOpener struct {
	entered chan struct{}
	gate    chan struct{}
	err     error
	mu      sync.Mutex
	calls   int
}

func (g *gatedOpener) CreateAsk(context.Context, string, string, []dispatch.Option, string) (dispatch.Ask, error) {
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	g.entered <- struct{}{}
	<-g.gate
	if g.err != nil {
		return dispatch.Ask{}, g.err
	}
	return dispatch.Ask{ID: fmt.Sprintf("gated-ask-%d", n), State: "open"}, nil
}

// gatedReader is a secrets.Reader that announces each Read on entered and answers from its Fake
// only once gate is closed.
type gatedReader struct {
	secrets.Fake
	entered chan struct{}
	gate    chan struct{}
}

func (g gatedReader) Read(ctx context.Context, source string) (string, error) {
	g.entered <- struct{}{}
	<-g.gate
	return g.Fake.Read(ctx, source)
}

// awaitEntered waits for one announcement on entered, failing t after five seconds.
func awaitEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never started", what)
	}
}

// mintPodToken mints a projected service-account token bound to podUID, the shape
// enroll.K8sPodVerifier.Verify reads.
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

// newFixture opens a store on a fresh schema (params are extra connection parameters), builds an
// enroll.Service and two live box/sjawhar enrollments (enrA, enrB) through it, and wires a
// Machine against rules.Current loaded from testdata/rules.yaml (DEEL_API_KEY needs approval for
// box/sjawhar and pod/issue_assignee, AUTO_TOKEN is automatic for box/sjawhar, DENIED_KEY matches
// no requester and always denies), a secrets.Fake with the inject-mode values, and the fakeOpener.
func newFixture(t *testing.T, params ...string) (m *Machine, opener *fakeOpener, svc *enroll.Service, enrA, enrB enroll.Enrollment) {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t, params...)

	svc = &enroll.Service{Store: st, Lease: time.Hour}
	_, token, err := svc.MintLauncherCredential(ctx, str("sjawhar"), nil, "devbox", "ask-enroll")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	enrA, err = svc.Create(ctx, cred, enroll.Enrollment{
		Kind: "box", RuntimeID: "box-a-" + t.Name(), Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-a-" + t.Name(),
	})
	if err != nil {
		t.Fatalf("Create(enrA): %v", err)
	}
	enrB, err = svc.Create(ctx, cred, enroll.Enrollment{
		Kind: "box", RuntimeID: "box-b-" + t.Name(), Operator: str("sjawhar"),
		ApproverKind: "operator", Thumbprint: "tp-b-" + t.Name(),
	})
	if err != nil {
		t.Fatalf("Create(enrB): %v", err)
	}

	cctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cur, err := rules.NewCurrent(cctx, rules.FileLoader{Path: "testdata/rules.yaml"}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}

	opener = &fakeOpener{}
	m = &Machine{
		Store:    st,
		Rules:    cur,
		Dispatch: opener,
		Secrets: secrets.Fake{
			"dev1/agent-secrets/DEEL_API_KEY": "deel-v1",
			"dev1/agent-secrets/AUTO_TOKEN":   "auto-v1",
		},
		MaxGrant:      time.Hour,
		PendingTTL:    12 * time.Hour,
		StandingIssue: func(context.Context, string) (string, error) { return "AGENTC-1", nil },
		IssueAssignee: func(context.Context, string) (string, error) { return "alice", nil },
	}
	return m, opener, svc, enrA, enrB
}

func TestAutomaticGrantsAtOnce(t *testing.T) {
	m, opener, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if req.State != "granted" || req.GrantID == nil {
		t.Fatalf("req = %+v, want state granted with a grant id", req)
	}
	if len(opener.calls) != 0 {
		t.Fatalf("opener called %d times, want 0", len(opener.calls))
	}

	values, proxyOnly, _, err := m.Values(ctx, *req.GrantID, enrA.ID.String())
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if len(proxyOnly) != 0 {
		t.Fatalf("proxyOnly = %v, want none", proxyOnly)
	}
	if want := map[string]string{"AUTO_TOKEN": "auto-v1"}; values["AUTO_TOKEN"] != want["AUTO_TOKEN"] || len(values) != 1 {
		t.Fatalf("values has %d entries (want 1) and AUTO_TOKEN matches the expected granted value = %v (want true)", len(values), values["AUTO_TOKEN"] == want["AUTO_TOKEN"])
	}
}

func TestDenyOpensNoAsk(t *testing.T) {
	m, opener, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY", "DENIED_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if req.State != "denied" {
		t.Fatalf("state = %q, want denied", req.State)
	}
	if len(opener.calls) != 0 {
		t.Fatalf("opener called %d times, want 0", len(opener.calls))
	}
}

func TestApprovalFlow(t *testing.T) {
	m, opener, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if req.State != "pending" {
		t.Fatalf("state = %q, want pending", req.State)
	}
	if len(opener.calls) != 1 {
		t.Fatalf("opener called %d times, want 1", len(opener.calls))
	}
	if q := opener.calls[0].question; !strings.Contains(q, "DEEL_API_KEY") || !strings.Contains(q, "sjawhar") {
		t.Fatalf("question = %q, want it to name DEEL_API_KEY and sjawhar", q)
	}

	approve := dispatch.Ask{
		ID: "ask-1", State: "answered",
		Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()},
	}
	changed, err := m.ApplyAnswer(ctx, req.ID, approve)
	if err != nil || !changed {
		t.Fatalf("ApplyAnswer(approve) = changed=%v err=%v, want changed", changed, err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "granted" || got.GrantID == nil {
		t.Fatalf("got = %+v, want state granted with a grant id", got)
	}

	values, _, _, err := m.Values(ctx, *got.GrantID, enrA.ID.String())
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if values["DEEL_API_KEY"] != "deel-v1" {
		_, present := values["DEEL_API_KEY"]
		t.Fatalf("values has %d entries; DEEL_API_KEY present=%v but did not match the expected granted value", len(values), present)
	}

	changed, err = m.ApplyAnswer(ctx, req.ID, approve)
	if err != nil || changed {
		t.Fatalf("ApplyAnswer(repeat) = changed=%v err=%v, want unchanged", changed, err)
	}
	var grantCount int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from grants where request_id=$1`, req.ID).Scan(&grantCount); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grantCount != 1 {
		t.Fatalf("grant count = %d, want 1", grantCount)
	}
}

func TestWrongApproverDenies(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	changed, err := m.ApplyAnswer(ctx, req.ID, dispatch.Ask{
		ID: "ask-1", State: "answered",
		Answer: &dispatch.Answer{User: "mallory", Selected: []string{"Approve"}, At: time.Now()},
	})
	if err != nil || !changed {
		t.Fatalf("ApplyAnswer = changed=%v err=%v, want changed", changed, err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "denied" {
		t.Fatalf("state = %q, want denied", got.State)
	}
	if got.Detail == nil || !strings.Contains(*got.Detail, "mallory") {
		t.Fatalf("detail = %v, want it to name mallory", got.Detail)
	}
}

func TestEditedAskDenies(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	edited := "2026-01-02T00:00:00Z"
	changed, err := m.ApplyAnswer(ctx, req.ID, dispatch.Ask{
		ID: "ask-1", State: "answered", EditedAt: &edited,
		Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()},
	})
	if err != nil || !changed {
		t.Fatalf("ApplyAnswer = changed=%v err=%v, want changed", changed, err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "denied" {
		t.Fatalf("state = %q, want denied", got.State)
	}
}

// TestApplyAnswerLeavesRequestPendingOnUnrecognizedAskState pins that an ask read this package
// cannot interpret — the zero Ask a mis-decoded Dispatch response produces, or a state Dispatch
// adds later — is an error to retry on the next poll, never a denial of a request no human
// refused.
func TestApplyAnswerLeavesRequestPendingOnUnrecognizedAskState(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, ask := range []dispatch.Ask{{}, {ID: "ask-1", State: "snoozed"}} {
		changed, err := m.ApplyAnswer(ctx, req.ID, ask)
		if err == nil || changed {
			t.Fatalf("ApplyAnswer(%+v) = changed=%v err=%v, want an error and no change", ask, changed, err)
		}
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "pending" {
		t.Fatalf("state = %q, want still pending", got.State)
	}
}

func TestLateApprovalAfterCancelChangesNothing(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Cancel(ctx, req.ID, enrA.ID.String()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "cancelled" {
		t.Fatalf("state = %q, want cancelled", got.State)
	}

	changed, err := m.ApplyAnswer(ctx, req.ID, dispatch.Ask{
		ID: "ask-1", State: "answered",
		Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()},
	})
	if err != nil || changed {
		t.Fatalf("ApplyAnswer(after cancel) = changed=%v err=%v, want unchanged", changed, err)
	}
	got2, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got2.State != "cancelled" {
		t.Fatalf("state after late approval = %q, want still cancelled", got2.State)
	}
	var grantCount int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from grants where request_id=$1`, req.ID).Scan(&grantCount); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grantCount != 0 {
		t.Fatalf("grant count = %d, want 0", grantCount)
	}
}

func TestOtherEnrollmentCannotUseGrant(t *testing.T) {
	m, _, _, enrA, enrB := newFixture(t)
	ctx := context.Background()

	granted, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(automatic): %v", err)
	}
	pending, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(pending): %v", err)
	}

	if _, _, _, err := m.Values(ctx, *granted.GrantID, enrB.ID.String()); !errors.Is(err, ErrNotYours) {
		t.Fatalf("Values(other enrollment) = %v, want ErrNotYours", err)
	}
	if err := m.Cancel(ctx, pending.ID, enrB.ID.String()); !errors.Is(err, ErrNotYours) {
		t.Fatalf("Cancel(other enrollment) = %v, want ErrNotYours", err)
	}
}

func TestCoalescesIdenticalPending(t *testing.T) {
	m, opener, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	first, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	second, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it again", "", "")
	if err != nil {
		t.Fatalf("Create(second): %v", err)
	}
	if second.ID != first.ID || !second.Coalesced {
		t.Fatalf("second = %+v, want coalesced onto %s", second, first.ID)
	}
	if len(opener.calls) != 1 {
		t.Fatalf("opener called %d times, want 1", len(opener.calls))
	}
}

func TestExpirePending(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update requests set pending_expires_at = now() - interval '1 hour' where id=$1`, req.ID); err != nil {
		t.Fatalf("backdate pending_expires_at: %v", err)
	}

	n, err := m.ExpirePending(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpirePending: %v", err)
	}
	if n != 1 {
		t.Fatalf("ExpirePending = %d, want 1", n)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "expired" {
		t.Fatalf("state = %q, want expired", got.State)
	}

	changed, err := m.ApplyAnswer(ctx, req.ID, dispatch.Ask{
		ID: "ask-1", State: "answered",
		Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()},
	})
	if err != nil || changed {
		t.Fatalf("ApplyAnswer(after expiry) = changed=%v err=%v, want unchanged", changed, err)
	}
}

func TestPodRequestIssueComesFromEnrollment(t *testing.T) {
	m, opener, svc, _, _ := newFixture(t)
	ctx := context.Background()

	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "signing-key")
	verifier, err := oidc.New(ctx, issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	svc.Pod = enroll.K8sPodVerifier{Verifier: verifier}

	_, token, err := svc.MintLauncherCredential(ctx, nil, str("legion-daemon"), "cluster", "ask-pod")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}

	podA, err := svc.Create(ctx, cred, enroll.Enrollment{
		Kind: "pod", RuntimeID: "pod-legion-9-a", ApproverKind: "issue_assignee",
		ApproverIssue: str("LEGION-9"), Thumbprint: "tp-pod-a",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-legion-9-a"),
	})
	if err != nil {
		t.Fatalf("Create(pod enrollment a): %v", err)
	}
	podB, err := svc.Create(ctx, cred, enroll.Enrollment{
		Kind: "pod", RuntimeID: "pod-legion-9-b", ApproverKind: "issue_assignee",
		ApproverIssue: str("LEGION-9"), Thumbprint: "tp-pod-b",
		PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", "pod-legion-9-b"),
	})
	if err != nil {
		t.Fatalf("Create(pod enrollment b): %v", err)
	}

	req, err := m.Create(ctx, podA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "LEGION-9", "")
	if err != nil {
		t.Fatalf("Create(explicit matching issue): %v", err)
	}
	if req.IssueKey != "LEGION-9" || req.State != "pending" {
		t.Fatalf("req = %+v, want issue LEGION-9 pending", req)
	}

	req2, err := m.Create(ctx, podB.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(empty issue): %v", err)
	}
	if req2.IssueKey != "LEGION-9" || req2.State != "pending" {
		t.Fatalf("req2 = %+v, want issue LEGION-9 pending", req2)
	}

	if len(opener.calls) != 2 {
		t.Fatalf("opener called %d times, want 2", len(opener.calls))
	}
	for _, c := range opener.calls {
		if c.issue != "LEGION-9" {
			t.Fatalf("ask opened on issue %q, want LEGION-9", c.issue)
		}
	}

	if _, err := m.Create(ctx, podA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "AGENTC-1", ""); !errors.Is(err, ErrIssueMismatch) {
		t.Fatalf("Create(mismatched issue) = %v, want ErrIssueMismatch", err)
	}
	if len(opener.calls) != 2 {
		t.Fatalf("opener called %d times after mismatch, want still 2 (no ask opened)", len(opener.calls))
	}
}

func TestRevokeStopsValues(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if req.GrantID == nil {
		t.Fatal("want a grant id")
	}
	if err := m.RevokeGrant(ctx, *req.GrantID, Revoker{Login: "sjawhar"}); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if _, _, _, err := m.Values(ctx, *req.GrantID, enrA.ID.String()); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values(after revoke) = %v, want ErrGrantNotLive", err)
	}
}

// TestApplyAnswerRejectsAskFromAnotherRequest pins the ask_id binding ApplyAnswer enforces: an
// otherwise-valid approval (same approver, matching nil edited_at) answered against a different
// request's ask must never grant the request it's mistakenly applied to.
func TestApplyAnswerRejectsAskFromAnotherRequest(t *testing.T) {
	m, opener, _, enrA, enrB := newFixture(t)
	ctx := context.Background()

	reqA, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(A): %v", err)
	}
	reqB, err := m.Create(ctx, enrB.ID.String(), []string{"DEEL_API_KEY"}, "need it too", "", "")
	if err != nil {
		t.Fatalf("Create(B): %v", err)
	}
	if len(opener.calls) != 2 {
		t.Fatalf("opener called %d times, want 2 (one ask per request)", len(opener.calls))
	}
	askForB := "ask-2"

	// An answer that would validly approve B (same approver, B's own nil edited_at) but is
	// applied to A's request id: A must not be granted by B's ask.
	changed, err := m.ApplyAnswer(ctx, reqA.ID, dispatch.Ask{
		ID: askForB, State: "answered",
		Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()},
	})
	if err != nil {
		t.Fatalf("ApplyAnswer(A, B's ask): %v", err)
	}
	if !changed {
		t.Fatal("ApplyAnswer(A, B's ask) = unchanged, want it to resolve A (denied), not leave it pending")
	}
	gotA, err := m.Get(ctx, reqA.ID)
	if err != nil {
		t.Fatalf("Get(A): %v", err)
	}
	if gotA.State == "granted" || gotA.GrantID != nil {
		t.Fatalf("A = %+v, must never be granted by an ask belonging to another request", gotA)
	}

	// B is untouched: still pending, its own ask still valid to approve.
	gotB, err := m.Get(ctx, reqB.ID)
	if err != nil {
		t.Fatalf("Get(B): %v", err)
	}
	if gotB.State != "pending" {
		t.Fatalf("B state = %q, want still pending (ApplyAnswer(A, ...) must not touch B)", gotB.State)
	}
}

// TestExpirePendingAuditsEveryExpiredRequest pins that ExpirePending's state transition and its
// audit row are atomic: after expiring several pending requests in one call, every one of them
// has exactly one matching request.expired audit row, never zero (a lost event) or more than one.
func TestExpirePendingAuditsEveryExpiredRequest(t *testing.T) {
	m, _, _, enrA, enrB := newFixture(t)
	ctx := context.Background()

	reqA, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(A): %v", err)
	}
	reqB, err := m.Create(ctx, enrB.ID.String(), []string{"DEEL_API_KEY"}, "need it too", "", "")
	if err != nil {
		t.Fatalf("Create(B): %v", err)
	}
	for _, id := range []string{reqA.ID, reqB.ID} {
		if _, err := m.Store.Pool.Exec(ctx, `update requests set pending_expires_at = now() - interval '1 hour' where id=$1`, id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	n, err := m.ExpirePending(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpirePending: %v", err)
	}
	if n != 2 {
		t.Fatalf("ExpirePending = %d, want 2", n)
	}
	for _, id := range []string{reqA.ID, reqB.ID} {
		got, err := m.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.State != "expired" {
			t.Fatalf("state(%s) = %q, want expired", id, got.State)
		}
		var auditCount int
		if err := m.Store.Pool.QueryRow(ctx, `select count(*) from audit where kind='request.expired' and request_id=$1`, id).Scan(&auditCount); err != nil {
			t.Fatalf("count audit(%s): %v", id, err)
		}
		if auditCount != 1 {
			t.Fatalf("audit rows for %s = %d, want exactly 1 (state transition and audit must commit together)", id, auditCount)
		}
	}
}

// TestMachineSessionID covers Machine.SessionID's three answers: a request created with a
// session_id override, one created without, and a request id that does not exist — every case
// answers ("", nil) except the first.
func TestMachineSessionID(t *testing.T) {
	m, _, _, enrA, enrB := newFixture(t)
	ctx := context.Background()

	withSession, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "session-abc")
	if err != nil {
		t.Fatalf("Create(withSession): %v", err)
	}
	if got, err := m.SessionID(ctx, withSession.ID); err != nil || got != "session-abc" {
		t.Fatalf("SessionID(withSession) = %q, %v, want %q, nil", got, err, "session-abc")
	}

	// enrB, not enrA: reuseLiveGrant matches on (enrollment, exact name set), and the first
	// Create above already left a live AUTO_TOKEN grant on enrA — a same-enrollment repeat would
	// now legitimately return that same live grant (and its session id) instead of a fresh row.
	noSession, err := m.Create(ctx, enrB.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(noSession): %v", err)
	}
	if got, err := m.SessionID(ctx, noSession.ID); err != nil || got != "" {
		t.Fatalf("SessionID(noSession) = %q, %v, want empty, nil", got, err)
	}

	if got, err := m.SessionID(ctx, uuid.NewString()); err != nil || got != "" {
		t.Fatalf("SessionID(nonexistent) = %q, %v, want empty, nil", got, err)
	}
}

// TestCreateReusesLiveGrantForIdenticalNameSetWithoutNewAsk is the regression for the review's
// Critical finding: Create's own "request (or reuse the live grant)" contract must answer a
// repeat request for the exact same names with the already-live grant, not a fresh ask.
func TestCreateReusesLiveGrantForIdenticalNameSetWithoutNewAsk(t *testing.T) {
	m, opener, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	approve := dispatch.Ask{
		ID: "ask-1", State: "answered",
		Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()},
	}
	if _, err := m.ApplyAnswer(ctx, req.ID, approve); err != nil {
		t.Fatalf("ApplyAnswer: %v", err)
	}
	granted, err := m.Get(ctx, req.ID)
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Get(after approve) = %+v, %v, want a grant id", granted, err)
	}
	if len(opener.calls) != 1 {
		t.Fatalf("opener called %d times, want 1", len(opener.calls))
	}

	reused, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it again", "", "")
	if err != nil {
		t.Fatalf("Create(again): %v", err)
	}
	if reused.ID != req.ID || reused.GrantID == nil || *reused.GrantID != *granted.GrantID {
		t.Fatalf("reused = %+v, want the same request %q and grant %q", reused, req.ID, *granted.GrantID)
	}
	if len(opener.calls) != 1 {
		t.Fatalf("opener called %d times after reuse, want still 1 (no new ask)", len(opener.calls))
	}
}

// TestCreateDoesNotReuseLiveGrantForADifferentNameSet checks reuseLiveGrant's exact-match-only
// rule: a superset of an already-granted name set must open its own fresh ask, never silently
// widen the reused grant to cover names it never approved.
func TestCreateDoesNotReuseLiveGrantForADifferentNameSet(t *testing.T) {
	m, opener, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	first, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	approve := dispatch.Ask{
		ID: "ask-1", State: "answered",
		Answer: &dispatch.Answer{User: "sjawhar", Selected: []string{"Approve"}, At: time.Now()},
	}
	if _, err := m.ApplyAnswer(ctx, first.ID, approve); err != nil {
		t.Fatalf("ApplyAnswer: %v", err)
	}

	second, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY", "AUTO_TOKEN"}, "need more", "", "")
	if err != nil {
		t.Fatalf("Create(superset): %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("Create(superset) reused the first request, want a fresh one")
	}
	if len(opener.calls) != 2 {
		t.Fatalf("opener called %d times, want 2 (a fresh ask for the superset)", len(opener.calls))
	}
}

// TestCreateDoesNotReuseAnExpiredOrRevokedGrant checks reuseLiveGrant's liveness rule: a grant
// that has expired or been revoked must not be handed back, even though its request row is still
// state='granted' — Create must fall through to a brand-new request and grant.
func TestCreateDoesNotReuseAnExpiredOrRevokedGrant(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()

	granted, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if granted.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, want a grant id", granted)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update grants set expires_at = now() - interval '1 hour' where id=$1`, *granted.GrantID); err != nil {
		t.Fatalf("backdate grant: %v", err)
	}

	fresh, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it again", "", "")
	if err != nil {
		t.Fatalf("Create(after expiry): %v", err)
	}
	if fresh.ID == granted.ID || fresh.GrantID == nil || *fresh.GrantID == *granted.GrantID {
		t.Fatalf("fresh = %+v, want a brand-new request+grant, not the expired one %+v", fresh, granted)
	}

	if err := m.RevokeGrant(ctx, *fresh.GrantID, Revoker{EnrollmentID: enrA.ID.String()}); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	again, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it a third time", "", "")
	if err != nil {
		t.Fatalf("Create(after revoke): %v", err)
	}
	if again.ID == fresh.ID || again.GrantID == nil || *again.GrantID == *fresh.GrantID {
		t.Fatalf("again = %+v, want a brand-new request+grant, not the revoked one %+v", again, fresh)
	}
}

// podEnrollment enrolls a pod running as the service account subject through svc, whose requests
// are approved by LEGION-9's assignee (the fixture's IssueAssignee answers "alice").
func podEnrollment(t *testing.T, svc *enroll.Service, runtimeID, subject string) enroll.Enrollment {
	t.Helper()
	ctx := context.Background()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "signing-key")
	verifier, err := oidc.New(ctx, issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	svc.Pod = enroll.K8sPodVerifier{Verifier: verifier}
	_, token, err := svc.MintLauncherCredential(ctx, nil, str("legion-daemon"), "cluster", "ask-pod")
	if err != nil {
		t.Fatalf("MintLauncherCredential: %v", err)
	}
	cred, err := svc.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	pod, err := svc.Create(ctx, cred, enroll.Enrollment{
		Kind: "pod", RuntimeID: runtimeID, ApproverKind: "issue_assignee", ApproverIssue: str("LEGION-9"),
		Thumbprint: "tp-" + runtimeID, PodToken: mintPodToken(t, issuer, key, subject, runtimeID),
	})
	if err != nil {
		t.Fatalf("Create(pod enrollment): %v", err)
	}
	return pod
}

// TestCreateHoldsNoPooledConnectionWhileOpeningAsk pins that Create holds no transaction, and so
// no pooled connection, while Dispatch opens the ask.
func TestCreateHoldsNoPooledConnectionWhileOpeningAsk(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	gated := &gatedOpener{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	m.Dispatch = gated
	done := make(chan error, 1)
	var req Request
	go func() {
		var err error
		req, err = m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
		done <- err
	}()
	awaitEntered(t, gated.entered, "CreateAsk")
	acquired := m.Store.Pool.Stat().AcquiredConns()
	close(gated.gate)
	if err := <-done; err != nil {
		t.Fatalf("Create: %v", err)
	}
	if acquired != 0 {
		t.Fatalf("%d pooled connections checked out while Dispatch opened the ask, want 0", acquired)
	}
	if req.State != "pending" || req.AskRef == nil || !strings.HasSuffix(*req.AskRef, "/ask/gated-ask-1") {
		t.Fatalf("req = %+v, want pending with the opened ask recorded", req)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.AskRef == nil || *got.AskRef != *req.AskRef {
		t.Fatalf("Get = %+v, %v, want the ask recorded on the row", got, err)
	}
}

// TestCreateCancelsRequestWhoseAskCannotBeOpened pins that a Dispatch failure while opening the
// ask leaves no half-made pending row: Create reports the failure and the row is cancelled by the
// broker with its own audit row.
func TestCreateCancelsRequestWhoseAskCannotBeOpened(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	gated := &gatedOpener{entered: make(chan struct{}, 1), gate: make(chan struct{}), err: errors.New("dispatch is down")}
	close(gated.gate)
	m.Dispatch = gated
	if _, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", ""); err == nil || !strings.Contains(err.Error(), "dispatch is down") {
		t.Fatalf("Create = %v, want the Dispatch failure", err)
	}
	var state, decidedBy string
	var askID *string
	if err := m.Store.Pool.QueryRow(ctx, `select state, decided_by, ask_id from requests where enrollment_id=$1`, enrA.ID).Scan(&state, &decidedBy, &askID); err != nil {
		t.Fatalf("read the request row: %v", err)
	}
	if state != "cancelled" || decidedBy != "broker" || askID != nil {
		t.Fatalf("row state=%s decided_by=%s ask_id=%v, want cancelled by broker with no ask", state, decidedBy, askID)
	}
	var audits int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from audit where kind='request.cancelled' and actor='broker' and enrollment_id=$1`, enrA.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("request.cancelled audit rows = %d, %v, want 1", audits, err)
	}
}

// TestCancelUnopenedCancelsOnlyStrandedPendingRequests pins the sweep for a pending row whose ask
// was never recorded: past the grace period it is cancelled and audited; a row still inside the
// grace period (an ask being opened right now) and a row with an ask are left alone.
func TestCancelUnopenedCancelsOnlyStrandedPendingRequests(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	stranded, fresh := uuid.NewString(), uuid.NewString()
	for id, age := range map[string]string{stranded: "10 minutes", fresh: "0 seconds"} {
		if _, err := m.Store.Pool.Exec(ctx, `insert into requests (id, enrollment_id, issue_key, reason, state, rules_version, lifetime_seconds, pending_expires_at, created_at)
			values ($1,$2,'AGENTC-1','r','pending','v',60, now() + interval '1 hour', now() - $3::interval)`, id, enrA.ID, age); err != nil {
			t.Fatalf("insert pending row: %v", err)
		}
	}
	withAsk, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update requests set created_at = now() - interval '10 minutes' where id=$1`, withAsk.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	n, err := m.CancelUnopened(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("CancelUnopened = %d, %v, want 1", n, err)
	}
	for id, want := range map[string]string{stranded: "cancelled", fresh: "pending", withAsk.ID: "pending"} {
		got, err := m.Get(ctx, id)
		if err != nil || got.State != want {
			t.Fatalf("request %s = %+v, %v, want %s", id, got, err, want)
		}
	}
}

// TestConcurrentIdenticalRequestsOpenOneAsk pins that two identical requests from one enrollment
// racing each other open one Dispatch ask between them: the second waits on the first's row,
// sees it, and coalesces onto it instead of opening its own.
func TestConcurrentIdenticalRequestsOpenOneAsk(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	gated := &gatedOpener{entered: make(chan struct{}, 2), gate: make(chan struct{})}
	m.Dispatch = gated
	first := make(chan Request, 1)
	go func() {
		req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "first", "", "")
		if err != nil {
			t.Errorf("Create(first): %v", err)
		}
		first <- req
	}()
	awaitEntered(t, gated.entered, "the first request's CreateAsk")
	second := make(chan Request, 1)
	go func() {
		req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "second", "", "")
		if err != nil {
			t.Errorf("Create(second): %v", err)
		}
		second <- req
	}()
	var coalesced Request
	select {
	case coalesced = <-second:
	case <-gated.entered:
		close(gated.gate)
		t.Fatal("the second identical request opened a Dispatch ask of its own")
	case <-time.After(5 * time.Second):
		close(gated.gate)
		t.Fatal("the second identical request did not coalesce while the first was opening its ask")
	}
	close(gated.gate)
	opened := <-first
	if !coalesced.Coalesced || coalesced.ID != opened.ID {
		t.Fatalf("second = %+v, want coalesced onto %s", coalesced, opened.ID)
	}
	if gated.calls != 1 {
		t.Fatalf("CreateAsk calls = %d, want 1", gated.calls)
	}
}

// TestValuesHoldsNoPooledConnectionWhileReadingSecrets pins that Values reads a grant's names into
// memory before fetching the first value, so no cursor (and its pooled connection) stays open
// across a Secrets Manager read.
func TestValuesHoldsNoPooledConnectionWhileReadingSecrets(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil || req.GrantID == nil {
		t.Fatalf("Create = %+v, %v, want an automatic grant", req, err)
	}
	reader := gatedReader{Fake: m.Secrets.(secrets.Fake), entered: make(chan struct{}, 1), gate: make(chan struct{})}
	m.Secrets = reader
	done := make(chan error, 1)
	go func() {
		_, _, _, err := m.Values(ctx, *req.GrantID, enrA.ID.String())
		done <- err
	}()
	awaitEntered(t, reader.entered, "the secret read")
	acquired := m.Store.Pool.Stat().AcquiredConns()
	close(reader.gate)
	if err := <-done; err != nil {
		t.Fatalf("Values: %v", err)
	}
	if acquired != 0 {
		t.Fatalf("%d pooled connections checked out during the secret read, want 0", acquired)
	}
}

// TestHumanRevokeIsLimitedToTheApproverOrOperator pins that a human may end only a grant they
// approved or one whose enrollment they operate: any other signed-in human is refused and the
// grant stays live, and login case never matters.
func TestHumanRevokeIsLimitedToTheApproverOrOperator(t *testing.T) {
	m, _, svc, enrA, _ := newFixture(t)
	ctx := context.Background()

	automatic, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil || automatic.GrantID == nil {
		t.Fatalf("Create(automatic) = %+v, %v", automatic, err)
	}
	if err := m.RevokeGrant(ctx, *automatic.GrantID, Revoker{Login: "mallory"}); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("RevokeGrant(mallory) = %v, want ErrNotApprover", err)
	}
	if _, _, _, err := m.Values(ctx, *automatic.GrantID, enrA.ID.String()); err != nil {
		t.Fatalf("Values after a refused revoke = %v, want the grant still live", err)
	}
	if err := m.RevokeGrant(ctx, *automatic.GrantID, Revoker{Login: "SJawhar"}); err != nil {
		t.Fatalf("RevokeGrant(the operator, other casing) = %v", err)
	}

	pod := podEnrollment(t, svc, "pod-revoke-approver", "system:serviceaccount:legion:worker")
	pending, err := m.Create(ctx, pod.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create(pod) = %v", err)
	}
	if _, err := m.ApplyAnswer(ctx, pending.ID, dispatch.Ask{ID: "ask-1", State: "answered",
		Answer: &dispatch.Answer{User: "alice", Selected: []string{"Approve"}, At: time.Now()}}); err != nil {
		t.Fatalf("ApplyAnswer: %v", err)
	}
	approved, err := m.Get(ctx, pending.ID)
	if err != nil || approved.GrantID == nil {
		t.Fatalf("Get = %+v, %v, want a grant", approved, err)
	}
	if err := m.RevokeGrant(ctx, *approved.GrantID, Revoker{Login: "sjawhar"}); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("RevokeGrant(a human who neither approved nor operates it) = %v, want ErrNotApprover", err)
	}
	if err := m.RevokeGrant(ctx, *approved.GrantID, Revoker{Login: "Alice"}); err != nil {
		t.Fatalf("RevokeGrant(the approver) = %v", err)
	}
	var actor string
	if err := m.Store.Pool.QueryRow(ctx, `select actor from audit where kind='grant.revoked' and grant_id=$1`, *approved.GrantID).Scan(&actor); err != nil || actor != "human:alice" {
		t.Fatalf("grant.revoked actor = %q, %v, want human:alice", actor, err)
	}
}

// withRules points m at a rules file holding yaml.
func withRules(t *testing.T, m *Machine, yaml string) {
	t.Helper()
	path := t.TempDir() + "/rules.yaml"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cur, err := rules.NewCurrent(ctx, rules.FileLoader{Path: path}, time.Hour, func(error) {})
	if err != nil {
		t.Fatalf("rules.NewCurrent: %v", err)
	}
	m.Rules = cur
}

// TestCoalescingComparesWholeNames pins that a request for the one name "PAIR_A,PAIR_B" is never
// mistaken for a pending request for the two names PAIR_A and PAIR_B.
func TestCoalescingComparesWholeNames(t *testing.T) {
	m, opener, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	rule := `
    source: dev1/agent-secrets/%s
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: sjawhar, decision: approval, approver: operator}
`
	withRules(t, m, "version: 1\nsecrets:\n  PAIR_A:"+fmt.Sprintf(rule, "a")+"  PAIR_B:"+fmt.Sprintf(rule, "b")+"  \"PAIR_A,PAIR_B\":"+fmt.Sprintf(rule, "ab"))
	pair, err := m.Create(ctx, enrA.ID.String(), []string{"PAIR_A", "PAIR_B"}, "need both", "", "")
	if err != nil || pair.State != "pending" {
		t.Fatalf("Create(pair) = %+v, %v, want pending", pair, err)
	}
	joined, err := m.Create(ctx, enrA.ID.String(), []string{"PAIR_A,PAIR_B"}, "need the joined one", "", "")
	if err != nil || joined.State != "pending" {
		t.Fatalf("Create(joined) = %+v, %v, want pending", joined, err)
	}
	if joined.Coalesced || joined.ID == pair.ID || len(opener.calls) != 2 {
		t.Fatalf("joined = %+v (asks opened %d), want its own request and ask, never the pair's", joined, len(opener.calls))
	}
}

// TestAuditSurvivesControlCharactersInSecretNames pins that a caller-supplied secret name holding a
// control character is written to the audit trail as a JSON string like any other: the request is
// decided (denied, the name matching no rule) and audited, never failed.
func TestAuditSurvivesControlCharactersInSecretNames(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	name := "BELL\aNAME\vTAB"
	req, err := m.Create(ctx, enrA.ID.String(), []string{name}, "odd name", "", "")
	if err != nil || req.State != "denied" {
		t.Fatalf("Create = %+v, %v, want denied", req, err)
	}
	var recorded []string
	if err := m.Store.Pool.QueryRow(ctx, `select array(select jsonb_array_elements_text(detail->'secrets')) from audit where kind='request.created' and request_id=$1`, req.ID).Scan(&recorded); err != nil {
		t.Fatalf("read request.created audit row: %v", err)
	}
	if len(recorded) != 1 || recorded[0] != name {
		t.Fatalf("audited secrets = %q, want [%q]", recorded, name)
	}
}

func approveAs(user, askID string) dispatch.Ask {
	return dispatch.Ask{ID: askID, State: "answered", Answer: &dispatch.Answer{User: user, Selected: []string{"Approve"}, At: time.Now()}}
}

// TestApprovalMatchesTheApproverCaseInsensitively pins that Dispatch recording the answering
// human's login with GitHub's display casing never turns their genuine approval into a denial.
func TestApprovalMatchesTheApproverCaseInsensitively(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.ApplyAnswer(ctx, req.ID, approveAs("SJawhar", "ask-1")); err != nil {
		t.Fatalf("ApplyAnswer: %v", err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.State != "granted" {
		t.Fatalf("Get = %+v (detail %v), %v, want granted", got, got.Detail, err)
	}
}

// TestApprovalAfterTheEnrollmentLapsedDenies pins that an approval landing after the requesting
// session's lease lapsed denies the request as withdrawn instead of granting to a dead session.
func TestApprovalAfterTheEnrollmentLapsedDenies(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update enrollments set lease_expires_at = now() - interval '1 minute' where id=$1`, enrA.ID); err != nil {
		t.Fatalf("lapse lease: %v", err)
	}
	changed, err := m.ApplyAnswer(ctx, req.ID, approveAs("sjawhar", "ask-1"))
	if err != nil || !changed {
		t.Fatalf("ApplyAnswer = %v, %v, want a decision", changed, err)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.State != "denied" || got.GrantID != nil || got.Detail == nil || *got.Detail != "enrollment no longer live" {
		t.Fatalf("Get = %+v, %v, want denied with no grant because the enrollment is no longer live", got, err)
	}
}

const tightenedRules = `version: 1
secrets:
  AUTO_TOKEN:
    source: dev1/agent-secrets/AUTO_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 43200
    requesters: %s
`

// TestTightenedRulesStopLiveGrants pins that a live grant is re-checked against rules that changed
// after it was decided: once the rules deny the requester, or want an approval the automatic
// grant never had, values are refused and the grant is no longer handed back by reuse.
func TestTightenedRulesStopLiveGrants(t *testing.T) {
	for name, requesters := range map[string]string{
		"denied":            "[]",
		"approval-now":      "[{kind: box, operator: sjawhar, decision: approval, approver: operator}]",
		"other-operator":    "[{kind: box, operator: mallory, decision: automatic}]",
		"other-kind":        "[{kind: host, operator: sjawhar, decision: automatic}]",
		"explicitly-denied": "[{kind: box, operator: sjawhar, decision: deny}]",
	} {
		t.Run(name, func(t *testing.T) {
			m, _, _, enrA, _ := newFixture(t)
			ctx := context.Background()
			granted, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
			if err != nil || granted.GrantID == nil {
				t.Fatalf("Create = %+v, %v, want an automatic grant", granted, err)
			}
			withRules(t, m, fmt.Sprintf(tightenedRules, requesters))
			if _, _, _, err := m.Values(ctx, *granted.GrantID, enrA.ID.String()); !errors.Is(err, ErrGrantNotLive) {
				t.Fatalf("Values under tightened rules = %v, want ErrGrantNotLive", err)
			}
			again, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it again", "", "")
			if err != nil {
				t.Fatalf("Create(again): %v", err)
			}
			if again.ID == granted.ID {
				t.Fatalf("Create(again) reused the grant the tightened rules no longer allow")
			}
		})
	}
}

// TestUnchangedPermissionSurvivesARulesChange pins the other side: a rules change that still
// allows the grant (here, only the lifetime changes) keeps it usable and reusable.
func TestUnchangedPermissionSurvivesARulesChange(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	granted, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create = %+v, %v", granted, err)
	}
	withRules(t, m, strings.Replace(fmt.Sprintf(tightenedRules, "[{kind: box, operator: sjawhar, decision: automatic}]"), "43200", "600", 1))
	if _, _, _, err := m.Values(ctx, *granted.GrantID, enrA.ID.String()); err != nil {
		t.Fatalf("Values = %v, want the grant still usable", err)
	}
	if again, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "again", "", ""); err != nil || again.ID != granted.ID {
		t.Fatalf("Create(again) = %+v, %v, want the live grant reused", again, err)
	}
}

// TestProxyGrantNeverWidensToInject pins that the delivery frozen at grant time is a ceiling: a
// name granted for proxy delivery stays proxy-only even after the rules switch it to inject.
func TestProxyGrantNeverWidensToInject(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	proxied := `version: 1
secrets:
  AUTO_TOKEN:
    source: dev1/agent-secrets/AUTO_TOKEN
    owner: sjawhar
    delivery: %s
    max_lifetime_seconds: 43200
    requesters: [{kind: box, operator: sjawhar, decision: automatic}]
%s`
	withRules(t, m, fmt.Sprintf(proxied, "proxy", "    proxy: {scheme: https, host: example.com, port: 443, path_prefix: /, methods: [GET], header: Authorization, header_format: 'Bearer {value}'}\n"))
	granted, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create = %+v, %v", granted, err)
	}
	withRules(t, m, fmt.Sprintf(proxied, "inject", ""))
	values, proxyOnly, _, err := m.Values(ctx, *granted.GrantID, enrA.ID.String())
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if len(values) != 0 || len(proxyOnly) != 1 || proxyOnly[0] != "AUTO_TOKEN" {
		t.Fatalf("Values released %d values, proxy_only %v; want none released and AUTO_TOKEN proxy-only", len(values), proxyOnly)
	}
}

// TestAutomaticAccessNeedsNoDispatch pins the spec's Dispatch-outage row: with every Dispatch
// lookup failing, an automatic request is still granted and a denied one still denied; only a
// request that needs an approval fails, and it fails with the Dispatch error.
func TestAutomaticAccessNeedsNoDispatch(t *testing.T) {
	m, opener, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	down := &dispatch.Error{Method: "GET", Path: "/api/v1/issues", Err: errors.New("connection refused")}
	m.StandingIssue = func(context.Context, string) (string, error) { return "", down }
	m.IssueAssignee = func(context.Context, string) (string, error) { return "", down }

	auto, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil || auto.State != "granted" {
		t.Fatalf("Create(automatic) with Dispatch down = %+v, %v, want granted", auto, err)
	}
	denied, err := m.Create(ctx, enrA.ID.String(), []string{"DENIED_KEY", "DEEL_API_KEY"}, "need it", "", "")
	if err != nil || denied.State != "denied" {
		t.Fatalf("Create(denied) with Dispatch down = %+v, %v, want denied", denied, err)
	}
	_, err = m.Create(ctx, enrA.ID.String(), []string{"DEEL_API_KEY"}, "need it", "", "")
	if dispatchErr, ok := dispatch.AsError(err); !ok || !dispatchErr.Unavailable() {
		t.Fatalf("Create(approval) with Dispatch down = %v, want the Dispatch outage", err)
	}
	if len(opener.calls) != 0 {
		t.Fatalf("asks opened = %d, want 0", len(opener.calls))
	}
}

// TestValuesNamesASecretMissingFromTheStore pins that a rule whose source the secrets store does
// not hold is refused as ErrSecretNotInStore naming the secret, not an opaque failure.
func TestValuesNamesASecretMissingFromTheStore(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	withRules(t, m, `version: 1
secrets:
  GHOST_KEY:
    source: dev1/agent-secrets/GHOST_KEY
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters: [{kind: box, operator: sjawhar, decision: automatic}]
`)
	granted, err := m.Create(ctx, enrA.ID.String(), []string{"GHOST_KEY"}, "need it", "", "")
	if err != nil || granted.GrantID == nil {
		t.Fatalf("Create = %+v, %v", granted, err)
	}
	_, _, _, err = m.Values(ctx, *granted.GrantID, enrA.ID.String())
	if !errors.Is(err, ErrSecretNotInStore) || !strings.Contains(err.Error(), "GHOST_KEY") {
		t.Fatalf("Values = %v, want ErrSecretNotInStore naming GHOST_KEY", err)
	}
}

// TestRevokingARevokedGrantWritesNoSecondAuditRow pins that a repeated revoke succeeds without
// recording a revocation that never happened: one grant.revoked row, naming the first revoker.
func TestRevokingARevokedGrantWritesNoSecondAuditRow(t *testing.T) {
	m, _, _, enrA, _ := newFixture(t)
	ctx := context.Background()
	req, err := m.Create(ctx, enrA.ID.String(), []string{"AUTO_TOKEN"}, "need it", "", "")
	if err != nil || req.GrantID == nil {
		t.Fatalf("Create = %+v, %v", req, err)
	}
	if err := m.RevokeGrant(ctx, *req.GrantID, Revoker{EnrollmentID: enrA.ID.String()}); err != nil {
		t.Fatalf("RevokeGrant (first): %v", err)
	}
	if err := m.RevokeGrant(ctx, *req.GrantID, Revoker{Login: "sjawhar"}); err != nil {
		t.Fatalf("RevokeGrant (again) = %v, want success", err)
	}
	var rows int
	var actor, revokedBy string
	if err := m.Store.Pool.QueryRow(ctx, `select count(*), min(a.actor), min(g.revoked_by) from audit a join grants g on g.id=a.grant_id where a.kind='grant.revoked' and a.grant_id=$1`, *req.GrantID).Scan(&rows, &actor, &revokedBy); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	want := "session:" + enrA.ID.String()
	if rows != 1 || actor != want || revokedBy != want {
		t.Fatalf("grant.revoked rows=%d actor=%s revoked_by=%s, want one row naming %s", rows, actor, revokedBy, want)
	}
}

// TestPodRulesMatchTheVerifiedServiceAccount pins that a pod entry naming a service account
// matches only pods whose projected token proved that account at enrollment.
func TestPodRulesMatchTheVerifiedServiceAccount(t *testing.T) {
	m, _, svc, _, _ := newFixture(t)
	ctx := context.Background()
	withRules(t, m, `version: 1
secrets:
  WORKER_KEY:
    source: dev1/agent-secrets/AUTO_TOKEN
    owner: sjawhar
    delivery: inject
    max_lifetime_seconds: 3600
    requesters: [{kind: pod, service_account: 'system:serviceaccount:legion:worker', decision: automatic}]
`)
	worker := podEnrollment(t, svc, "pod-worker", "system:serviceaccount:legion:worker")
	if req, err := m.Create(ctx, worker.ID.String(), []string{"WORKER_KEY"}, "need it", "", ""); err != nil || req.State != "granted" {
		t.Fatalf("Create(pod running as legion:worker) = %+v, %v, want granted", req, err)
	}
	stranger := podEnrollment(t, svc, "pod-stranger", "system:serviceaccount:default:stranger")
	if req, err := m.Create(ctx, stranger.ID.String(), []string{"WORKER_KEY"}, "need it", "", ""); err != nil || req.State != "denied" {
		t.Fatalf("Create(pod running as default:stranger) = %+v, %v, want denied", req, err)
	}
}
