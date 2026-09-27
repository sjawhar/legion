package launcher

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// createCall records one CreateAsk invocation for a test to assert against.
type createCall struct {
	issue, question, urgency string
	options                  []dispatch.Option
	askID                    string
}

// fakeDispatch is launcher.Service's dispatchClient: it opens and answers asks entirely
// in-memory (askID -> *dispatch.Ask, mutated by answer between Request and Reconcile) and keeps a
// project's issues in a slice, following the same fake-over-httptest-server precedent as
// requests.fakeOpener and requests.fakeAskReader. mu guards every field. listIssuesDelay widens
// the find-or-create race window; listGate, when set, holds every ListIssues call until it is
// closed, announcing each call on listEntered first.
type fakeDispatch struct {
	mu              sync.Mutex
	nextAsk         int
	asks            map[string]*dispatch.Ask
	createCalls     []createCall
	getAskCalls     []string
	issues          []dispatch.IssueSummary
	createIssueN    int
	listIssuesDelay time.Duration
	listGate        chan struct{}
	listEntered     chan struct{}
	retracted       []string
}

func (f *fakeDispatch) CreateAsk(_ context.Context, issue, question string, options []dispatch.Option, urgency string) (dispatch.Ask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextAsk++
	id := fmt.Sprintf("ask-%d", f.nextAsk)
	ask := dispatch.Ask{ID: id, State: "open"}
	if f.asks == nil {
		f.asks = map[string]*dispatch.Ask{}
	}
	f.asks[id] = &ask
	f.createCalls = append(f.createCalls, createCall{issue: issue, question: question, urgency: urgency, options: options, askID: id})
	return ask, nil
}

func (f *fakeDispatch) GetAsk(_ context.Context, id string) (dispatch.Ask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getAskCalls = append(f.getAskCalls, id)
	a, ok := f.asks[id]
	if !ok {
		return dispatch.Ask{}, fmt.Errorf("fakeDispatch: no such ask %q", id)
	}
	return *a, nil
}

func (f *fakeDispatch) ListIssues(_ context.Context, _, _ string) ([]dispatch.IssueSummary, error) {
	f.mu.Lock()
	delay, gate, entered := f.listIssuesDelay, f.listGate, f.listEntered
	out := make([]dispatch.IssueSummary, len(f.issues))
	copy(out, f.issues)
	f.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		<-gate
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return out, nil
}

func (f *fakeDispatch) CreateIssue(_ context.Context, _, title string, assignee *string, labels []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createIssueN++
	key := fmt.Sprintf("PROJ-%d", f.createIssueN)
	f.issues = append(f.issues, dispatch.IssueSummary{Key: key, Title: title, Assignee: assignee, Labels: labels})
	return key, nil
}

func (f *fakeDispatch) RetractAsk(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retracted = append(f.retracted, id)
	return nil
}

// createIssueCount safely reads createIssueN after concurrent Standing calls have settled.
func (f *fakeDispatch) createIssueCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createIssueN
}

// answer sets an ask's state to answered with the given approver and selection so the next
// Reconcile sees it as resolved.
func (f *fakeDispatch) answer(askID, user string, selected ...string) {
	a := f.asks[askID]
	a.State = "answered"
	a.Answer = &dispatch.Answer{User: user, Selected: selected, At: time.Now()}
}

// newTestService opens a store on a fresh schema (params are extra connection parameters) and
// returns a Service wired to a fresh fakeDispatch and a real enroll.Service against the same
// store, so AuthenticateLauncher exercises the genuine credential Reconcile minted.
func newTestService(t *testing.T, params ...string) (*Service, *fakeDispatch) {
	t.Helper()
	st := storetest.Open(t, params...)
	fake := &fakeDispatch{}
	svc := &Service{
		Store:    st,
		Dispatch: fake,
		Enroll:   &enroll.Service{Store: st, Lease: time.Hour},
		Project:  "PROJ",
	}
	return svc, fake
}

var confirmationCodeShape = regexp.MustCompile(`^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$`)

// TestLauncherRequestApprovalAndDenial pins Task 12's Step 1 scenario end to end: Request opens
// one ask on the operator's standing issue naming the host; Read reports pending; once the fake
// answers Approve as the operator, Reconcile mints a real launcher credential and Read reports it
// issued with the raw token exactly once, then issued with no token on the next read; that token
// authenticates as the requesting operator; and a second, independent request answered by someone
// other than the operator is denied rather than issued.
func TestLauncherRequestApprovalAndDenial(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t)

	pending, err := svc.Request(ctx, "sjawhar", "sami-agents", nil)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(fake.createCalls) != 1 {
		t.Fatalf("CreateAsk calls = %d, want 1", len(fake.createCalls))
	}
	call := fake.createCalls[0]
	if !strings.Contains(call.question, "sami-agents") {
		t.Fatalf("question = %q, want it to name the host", call.question)
	}
	if !confirmationCodeShape.MatchString(pending.ConfirmationCode) || !strings.Contains(call.question, pending.ConfirmationCode) {
		t.Fatalf("confirmation code %q, question %q: want an XXXX-XXXX code the question names", pending.ConfirmationCode, call.question)
	}
	if len(fake.issues) != 1 || fake.issues[0].Title != "Secret requests: sjawhar" {
		t.Fatalf("standing issue = %+v, want one titled %q", fake.issues, "Secret requests: sjawhar")
	}
	if call.issue != fake.issues[0].Key {
		t.Fatalf("ask opened on issue %q, want the standing issue %q", call.issue, fake.issues[0].Key)
	}

	state, token, _, err := svc.Read(ctx, pending.ID)
	if err != nil || state != "pending" || token != "" {
		t.Fatalf("Read (pending) = state=%q token_present=%v err=%v, want pending/empty/nil", state, token != "", err)
	}

	fake.answer(call.askID, "sjawhar", "Approve")
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	state, token, _, err = svc.Read(ctx, pending.ID)
	if err != nil || state != "issued" || token == "" {
		t.Fatalf("Read (issued, first) = state=%q token_present=%v err=%v, want issued with a token", state, token != "", err)
	}
	firstToken := token

	state, token, credentialID, err := svc.Read(ctx, pending.ID)
	if err != nil || state != "issued" || token != "" {
		t.Fatalf("Read (issued, second) = state=%q token_present=%v err=%v, want issued with no token", state, token != "", err)
	}
	if credentialID == "" {
		t.Fatal("Read (issued, second) credentialID = \"\", want the credential id an operator could revoke")
	}

	cred, err := svc.Enroll.AuthenticateLauncher(ctx, firstToken)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	if cred.Operator == nil || *cred.Operator != "sjawhar" {
		t.Fatalf("credential operator = %v, want sjawhar", cred.Operator)
	}

	// A second, independent request on the same standing issue, answered by someone other than
	// the operator, must be denied rather than issued.
	pending2, err := svc.Request(ctx, "sjawhar", "another-host", nil)
	if err != nil {
		t.Fatalf("Request (2nd): %v", err)
	}
	call2 := fake.createCalls[1]
	fake.answer(call2.askID, "mallory", "Approve")
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile (2nd): %v", err)
	}
	state, token, _, err = svc.Read(ctx, pending2.ID)
	if err != nil || state != "denied" || token != "" {
		t.Fatalf("Read (2nd, denied) = state=%q token_present=%v err=%v, want denied/empty", state, token != "", err)
	}
}

// TestRequestFloodHoldsNoPooledConnectionAcrossDispatch pins that no Postgres connection is held
// while Request waits on Dispatch. Eight requests for eight operators, against a two-connection
// pool, all reach Dispatch's ListIssues and park there; while they wait, nothing is checked out
// of the pool and a ping still gets a connection at once. Holding a connection (or the
// transaction an advisory lock needs) across the call stops the third request at the pool and
// starves every other caller.
func TestRequestFloodHoldsNoPooledConnectionAcrossDispatch(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t, "pool_max_conns=2")
	const n = 8
	fake.listGate = make(chan struct{})
	fake.listEntered = make(chan struct{}, n)
	errs := make(chan error, n)
	for i := range n {
		go func() {
			_, err := svc.Request(ctx, fmt.Sprintf("flood-operator-%d", i), "flood-host", nil)
			errs <- err
		}()
	}
	released := false
	defer func() {
		if !released {
			close(fake.listGate)
		}
	}()
	for i := range n {
		select {
		case <-fake.listEntered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d requests reached Dispatch; the rest are stuck waiting for a pooled connection", i, n)
		}
	}
	if acquired := svc.Store.Pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("%d pooled connections checked out while every request waits on Dispatch, want 0", acquired)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := svc.Store.Pool.Ping(pingCtx); err != nil {
		t.Fatalf("the pool refused a ping while requests wait on Dispatch: %v", err)
	}
	close(fake.listGate)
	released = true
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("Request: %v", err)
		}
	}
}

// TestLauncherRequestServiceCredentialHasNilOperator pins the service-credential branch: when
// Request is called with a non-nil service, the ask still opens on the named operator's own
// standing issue (they are who approves it), but the credential Reconcile mints on approval
// carries that service with a nil operator rather than the reverse.
func TestLauncherRequestServiceCredentialHasNilOperator(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t)

	pending, err := svc.Request(ctx, "sjawhar", "cluster", new("legion-daemon"))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	call := fake.createCalls[0]
	if !strings.Contains(call.question, "legion-daemon") {
		t.Fatalf("question = %q, want it to name the service", call.question)
	}
	if call.issue != fake.issues[0].Key {
		t.Fatalf("ask opened on issue %q, want the standing issue %q", call.issue, fake.issues[0].Key)
	}

	fake.answer(call.askID, "sjawhar", "Approve")
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	_, token, _, err := svc.Read(ctx, pending.ID)
	if err != nil || token == "" {
		t.Fatalf("Read (issued): token_present=%v err=%v", token != "", err)
	}
	cred, err := svc.Enroll.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	if cred.Operator != nil {
		t.Fatalf("credential operator = %v, want nil for a service credential", *cred.Operator)
	}
	if cred.Service == nil || *cred.Service != "legion-daemon" {
		t.Fatalf("credential service = %v, want legion-daemon", cred.Service)
	}
}

// TestStandingReusesExistingIssue pins Standing's find-or-create rule: two lookups for the same
// operator return the same issue key and only the first one creates it.
func TestStandingReusesExistingIssue(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t)

	first, err := svc.Standing(ctx, "sjawhar")
	if err != nil {
		t.Fatalf("Standing (1st): %v", err)
	}
	second, err := svc.Standing(ctx, "sjawhar")
	if err != nil {
		t.Fatalf("Standing (2nd): %v", err)
	}
	if first != second {
		t.Fatalf("Standing = %q then %q, want the same issue reused", first, second)
	}
	if fake.createIssueN != 1 {
		t.Fatalf("CreateIssue calls = %d, want 1", fake.createIssueN)
	}

	other, err := svc.Standing(ctx, "bob")
	if err != nil {
		t.Fatalf("Standing (bob): %v", err)
	}
	if other == first {
		t.Fatalf("Standing(bob) = %q, want a different issue than sjawhar's", other)
	}
}

// TestLauncherRequestExpiresAfterTTL pins Reconcile's expiry pass: a request whose expires_at is
// backdated is expired before Reconcile ever reads its pending rows, so it never reaches
// Dispatch.GetAsk (the fake would otherwise fail the test on being asked about the already-decided
// ask), Read reports it expired, and its now-pointless ask is retracted exactly once.
func TestLauncherRequestExpiresAfterTTL(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t)

	pending, err := svc.Request(ctx, "sjawhar", "stale-host", nil)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credential_requests set expires_at = now() - interval '1 minute' where pending_id_hash=$1`, hashPendingID(pending.ID)); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(fake.getAskCalls) != 0 {
		t.Fatalf("GetAsk calls = %v, want none for an already-expired row", fake.getAskCalls)
	}
	state, token, _, err := svc.Read(ctx, pending.ID)
	if err != nil || state != "expired" || token != "" {
		t.Fatalf("Read (expired) = state=%q token_present=%v err=%v, want expired/empty", state, token != "", err)
	}
	if len(fake.retracted) != 1 || fake.retracted[0] != fake.createCalls[0].askID {
		t.Fatalf("retracted asks = %v, want the expired request's ask %s", fake.retracted, fake.createCalls[0].askID)
	}
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile (again): %v", err)
	}
	if len(fake.retracted) != 1 {
		t.Fatalf("retracted asks after a second pass = %v, want the one retraction only", fake.retracted)
	}
}

// TestReadUnknownPendingID pins Read's ErrNotFound for a pending id no request ever minted.
func TestReadUnknownPendingID(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	if _, _, _, err := svc.Read(ctx, "not-a-real-pending-id"); err != ErrNotFound {
		t.Fatalf("Read(unknown) error = %v, want ErrNotFound", err)
	}
}

// TestReconcileLeavesRequestPendingOnUnrecognizedAskState pins that an ask read applyAsk cannot
// interpret leaves the launcher request pending for the next Reconcile instead of denying it.
func TestReconcileLeavesRequestPendingOnUnrecognizedAskState(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t)

	pending, err := svc.Request(ctx, "sjawhar", "odd-state-host", nil)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	askID := fake.createCalls[0].askID
	fake.asks[askID].State = ""
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	state, token, _, err := svc.Read(ctx, pending.ID)
	if err != nil || state != "pending" || token != "" {
		t.Fatalf("Read = state=%q token_present=%v err=%v, want still pending", state, token != "", err)
	}
}

// TestStandingSerializesConcurrentCreateForNewOperator pins the find-or-create race: Dispatch's
// own issue creation has no unique constraint on (project, title, label), so two concurrent
// Standing calls for the same never-before-seen operator could otherwise each observe zero
// matching issues and each create one. listIssuesDelay widens the window between ListIssues and
// CreateIssue so this test would reliably fail (several created issues) if concurrent callers for
// one operator did not share one lookup; with sharing, exactly one issue is created.
func TestStandingSerializesConcurrentCreateForNewOperator(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t)
	fake.listIssuesDelay = 100 * time.Millisecond

	const n = 5
	var wg sync.WaitGroup
	keys := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = svc.Standing(ctx, "race-operator")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Standing(%d): %v", i, err)
		}
	}
	for i, key := range keys {
		if key != keys[0] {
			t.Fatalf("Standing(%d) = %q, want the same issue as Standing(0) = %q", i, key, keys[0])
		}
	}
	if n := fake.createIssueCount(); n != 1 {
		t.Fatalf("CreateIssue calls = %d, want exactly 1 despite %d concurrent callers", n, len(keys))
	}
}

// TestLauncherRequestEmptyServiceStringNormalizedToOperatorCredential is the regression for the
// second-round finding: a non-nil pointer to an empty string ({"service": ""}, exactly what
// decoding a JSON body with a present-but-empty "service" field produces) must behave identically
// to a nil service — an ordinary operator credential, an ask that never mentions "service", and
// no distinct service-scoped question wording that would let a differently-scoped credential get
// approved behind text a human never saw naming it.
func TestLauncherRequestEmptyServiceStringNormalizedToOperatorCredential(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t)

	emptyService := new(string)
	pending, err := svc.Request(ctx, "sjawhar", "empty-service-host", emptyService)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	call := fake.createCalls[0]
	wantQuestion := "Issue a launcher credential for host empty-service-host? Approve only if you started this login yourself and your terminal shows confirmation code " +
		pending.ConfirmationCode + ": anyone can open a request that reads like this one."
	if call.question != wantQuestion {
		t.Fatalf("question = %q, want %q (empty service must read exactly like no service)", call.question, wantQuestion)
	}

	fake.answer(call.askID, "sjawhar", "Approve")
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	_, token, _, err := svc.Read(ctx, pending.ID)
	if err != nil || token == "" {
		t.Fatalf("Read (issued): token_present=%v err=%v", token != "", err)
	}
	cred, err := svc.Enroll.AuthenticateLauncher(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateLauncher: %v", err)
	}
	if cred.Operator == nil || *cred.Operator != "sjawhar" {
		t.Fatalf("credential operator = %v, want sjawhar (an empty service string must mint an ordinary operator credential)", cred.Operator)
	}
	if cred.Service != nil {
		t.Fatalf("credential service = %v, want nil for an empty service string", *cred.Service)
	}
}

// TestApprovalMatchesTheOperatorCaseInsensitively pins that the operator approving from an
// account whose GitHub display login carries capitals still issues the credential.
func TestApprovalMatchesTheOperatorCaseInsensitively(t *testing.T) {
	ctx := context.Background()
	svc, fake := newTestService(t)
	pending, err := svc.Request(ctx, "Xodarap", "mixed-case-host", nil)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	fake.answer(fake.createCalls[0].askID, "XodaRap", "Approve")
	if err := svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	state, token, _, err := svc.Read(ctx, pending.ID)
	if err != nil || state != "issued" || token == "" {
		t.Fatalf("Read = state=%q token_present=%v err=%v, want issued", state, token != "", err)
	}
	cred, err := svc.Enroll.AuthenticateLauncher(ctx, token)
	if err != nil || cred.Operator == nil || *cred.Operator != "xodarap" {
		t.Fatalf("credential operator = %v (err %v), want xodarap", cred.Operator, err)
	}
}
