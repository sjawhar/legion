package requests

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

// fakeOpener is the machine's askOpener: it always opens successfully, returning the same open
// ask, and records every call so a test can assert how many asks were opened and what they said.
type fakeOpener struct {
	calls []openCall
}

func (f *fakeOpener) CreateAsk(_ context.Context, issue, question string, options []dispatch.Option, urgency string) (dispatch.Ask, error) {
	f.calls = append(f.calls, openCall{issue: issue, question: question, urgency: urgency, options: options})
	return dispatch.Ask{ID: "ask-1", State: "open"}, nil
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
		t.Fatalf("values = %v, want %v", values, want)
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
		t.Fatalf("values = %v, want DEEL_API_KEY=deel-v1", values)
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
