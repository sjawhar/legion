package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/rules"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const testAudience = "broker"

func str(s string) *string { return &s }

func testDatabaseURL(t *testing.T) string {
	url := os.Getenv("BROKER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run Postgres requests tests")
	}
	return url
}

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

// newFixture opens a migrated store, builds an enroll.Service and two live box/sjawhar
// enrollments (enrA, enrB) through it, and wires a Machine against rules.Current loaded from
// testdata/rules.yaml (DEEL_API_KEY needs approval for box/sjawhar and pod/issue_assignee,
// AUTO_TOKEN is automatic for box/sjawhar, DENIED_KEY matches no requester and always denies), a
// secrets.Fake with the inject-mode values, and the fakeOpener.
func newFixture(t *testing.T) (m *Machine, opener *fakeOpener, svc *enroll.Service, enrA, enrB enroll.Enrollment) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Pool.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

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
	registerFixtureEnrollment(enrA.ID.String(), enrB.ID.String())

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
	registerFixtureEnrollment(podA.ID.String(), podB.ID.String())

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
	if err := m.RevokeGrant(ctx, *req.GrantID, "sjawhar", nil); err != nil {
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

	if err := m.RevokeGrant(ctx, *fresh.GrantID, "test", nil); err != nil {
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
