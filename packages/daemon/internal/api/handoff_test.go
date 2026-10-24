package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// headSHA is the head of pull request #42, the commit a completion of LEGION-208 reports unless a
// test names another.
const headSHA = "c0de0000000000000000000000000000000000ff"

// handoffTokens leases each App a token of its own for each owner, so a test can tell which App's
// token, minted for which owner, reached GitHub.
type handoffTokens struct{}

func (handoffTokens) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return appauth.Lease{Token: string(role) + "-token-for-" + owner, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (t handoffTokens) ReadToken(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := t.Token(ctx, role, owner)
	lease.Token += "-read"
	return lease, err
}

// handoffFile is a handoff of phase word as a worker stamps it for issue: READY reads only whether
// the head still carries the directory, never what a file holds.
func handoffFile(word, issue string) string {
	return fmt.Sprintf(`{"schemaVersion":1,"phase":%q,"issue":%q,"completed":"2026-10-09T08:00:00Z"}`, word, issue)
}

// handoffGitHub is GitHub's REST API for one repository, acme/widgets unless a test names another
// (repo), as READY reads it: the files at each commit (whether a head still carries
// .legion/<issue>/), and what READY reads of pull request #42 on main. A test changes what it
// serves between calls; every call's bearer, every path it is asked for (served) and every
// contents read are recorded. Nothing but READY reads GitHub at a completion.
type handoffGitHub struct {
	url  string
	mu   sync.Mutex
	repo string
	// files is the content of each path at each commit, by sha then path; a directory is listed
	// when a path is under it.
	files map[string]map[string]string
	// fail is the status GitHub answers every route under the given path prefix with instead of
	// its answer: GitHub's trouble.
	fail map[string]int
	// pull request #42: its head, its mergeable_state, its state, whether it is merged, and its
	// updated_at. merge is the project's merge policy, and pullFiles the JSON array GitHub answers
	// for the pull request's changed files.
	pullHead       string
	mergeableState string
	pullState      string
	merged         bool
	pullUpdatedAt  time.Time
	merge          config.MergeRules
	pullFiles      string
	// What GitHub would answer for main and for the head, none of which READY reads: the rulesets'
	// rules (with the status the read answers when not 200), the branch's protection, and the
	// head's check runs and commit statuses. A READY that judged the head's checks itself would
	// read the last two, and served is what proves it does not.
	rules       string
	rulesStatus int
	mainBranch  string
	checkRuns   string
	statuses    string
	bearers     []string
	// served is each path the stand-in was asked for, relative to the repository, and contentsRead
	// each path read through the contents API, as path@ref.
	served       []string
	contentsRead []string
	// compare is GitHub's answer to the compare of approvedSHA with pullHead, the pull request's
	// head; compareAnswers holds the answer to any other basehead (<base>...<head>), and one in
	// neither is GitHub's 404 (fail makes the compare fail outright); compareRead is each basehead
	// READY asked it for.
	compare        string
	compareAnswers map[string]string
	compareRead    []string
}

func newHandoffGitHub(t *testing.T) *handoffGitHub {
	t.Helper()
	g := &handoffGitHub{
		files: map[string]map[string]string{}, fail: map[string]int{}, repo: "acme/widgets",
		pullFiles: `[{"filename":"src/main.go"}]`,
		pullHead:  headSHA, mergeableState: "clean", pullState: "open", pullUpdatedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		rules:      `[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`,
		mainBranch: `{"name":"main","protection":{"required_status_checks":{"contexts":["legacy"],"checks":[{"context":"legacy"}]}}}`,
		checkRuns:  `[{"id":1,"name":"ci","status":"completed","conclusion":"success"}]`,
		statuses:   `[{"context":"legacy","state":"success"}]`,
		compare:    retroCompare,
	}
	notFound := func(w http.ResponseWriter, message string) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"message":%q,"status":"404"}`, message)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.bearers = append(g.bearers, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		path, ok := strings.CutPrefix(r.URL.Path, "/repos/"+g.repo+"/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		g.served = append(g.served, path)
		for prefix, status := range g.fail {
			if strings.HasPrefix(path, prefix) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"message":"Server Error"}`)
				return
			}
		}
		ref := r.URL.Query().Get("ref")
		switch {
		case path == "branches/main":
			_, _ = io.WriteString(w, g.mainBranch)
		case path == "pulls/42":
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"base":{"ref":"main"},"mergeable_state":%q,"state":%q,"merged":%t,"updated_at":%q}`,
				g.pullHead, g.mergeableState, g.pullState, g.merged, g.pullUpdatedAt.Format(time.RFC3339))
		case path == "rules/branches/main":
			if g.rulesStatus != 0 {
				w.WriteHeader(g.rulesStatus)
			}
			_, _ = io.WriteString(w, g.rules)
		case strings.HasSuffix(path, "/check-runs"):
			_, _ = fmt.Fprintf(w, `{"total_count":%d,"check_runs":%s}`, strings.Count(g.checkRuns, `"name"`), g.checkRuns)
		case strings.HasSuffix(path, "/status"):
			_, _ = fmt.Fprintf(w, `{"statuses":%s}`, g.statuses)
		case path == "pulls/42/files":
			_, _ = io.WriteString(w, g.pullFiles)
		case strings.HasPrefix(path, "contents/"):
			file := strings.TrimPrefix(path, "contents/")
			g.contentsRead = append(g.contentsRead, file+"@"+ref)
			var listing []string
			for name := range g.files[ref] {
				if strings.HasPrefix(name, file+"/") {
					listing = append(listing, fmt.Sprintf(`{"name":%q,"path":%q,"type":"file"}`, strings.TrimPrefix(name, file+"/"), name))
				}
			}
			if len(listing) == 0 {
				notFound(w, "Not Found")
				return
			}
			_, _ = io.WriteString(w, "["+strings.Join(listing, ",")+"]")
		case strings.HasPrefix(path, "compare/"):
			basehead := strings.TrimPrefix(path, "compare/")
			g.compareRead = append(g.compareRead, basehead)
			answer, known := g.compareAnswers[basehead]
			if basehead == approvedSHA+"..."+g.pullHead {
				answer, known = g.compare, true
			}
			if !known {
				notFound(w, "Not Found")
				return
			}
			_, _ = io.WriteString(w, answer)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// put is content at path of commit sha.
func (g *handoffGitHub) put(sha, path, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.files[sha] == nil {
		g.files[sha] = map[string]string{}
	}
	g.files[sha][path] = content
}

// set changes what the fake serves, under its lock.
func (g *handoffGitHub) set(change func(g *handoffGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

// reads is each contents read made so far, as path@ref.
func (g *handoffGitHub) reads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.contentsRead...)
}

// serves is each path the stand-in was asked for so far, relative to the repository.
func (g *handoffGitHub) serves() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.served...)
}

// compares is each basehead READY's compare was asked for so far.
func (g *handoffGitHub) compares() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.compareRead...)
}

// approvedSHA is the head LEGION-208's review round approved, below headSHA: what READY compares
// retro's last push with unless a test names another.
const approvedSHA = "a99e0000000000000000000000000000000000ff"

// retroCompare is GitHub's compare of approvedSHA with headSHA as READY reads it: retro's learning
// added, and its last commit's removal of the issue's handoffs.
const retroCompare = `{"status":"ahead","files":[{"filename":".legion/LEGION-208/review.json","status":"removed"},{"filename":"docs/solutions/legion/retro-LEGION-208.md","status":"added"}]}`

// newHandoffHarness serves the daemon's routes over a real Postgres, reading GitHub at github as
// the implement App for READY, with facts recording every fact the routes apply, refused as
// refusal says. The issue's repository is acme/widgets, so a READY read minted for anyone but acme
// shows.
func newHandoffHarness(t *testing.T, github *handoffGitHub, grants *credential.Grants, refusal *intake.Refusal) (*harness, *factRecorder) {
	t.Helper()
	h := newHarness(t)
	if grants == nil {
		grants = credential.New(nil)
	}
	facts := &factRecorder{refusal: refusal}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: grants, Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: []intake.Handler{facts}, Dispatch: &statusRecorder{}, PluginRelaunchable: true,
		Tokens: handoffTokens{}, GitHubAPI: github.url,
		MergeRules: func(project string) config.MergeRules {
			if project != testProject {
				return config.MergeRules{}
			}
			return github.merge
		},
	}).Handler
	return h, facts
}

// seedIssueAt records key as a root at the given phase, on acme/widgets, which is where the handoff
// route reads the phase a completion finishes.
func seedIssueAt(t *testing.T, h *harness, key string, at phase.Phase) {
	t.Helper()
	seedIssueOn(t, h, key, at, "acme/widgets")
}

// seedIssueOn records key as a root at the given phase on repo, the repository its record names.
func seedIssueOn(t *testing.T, h *harness, key string, at phase.Phase, repo string) {
	t.Helper()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: key, Repo: repo, DefaultBranch: "main", GitHubRepo: repo, Tree: key, Project: testProject, Title: key, Phase: at, Generation: 1, Status: "in_progress",
		})
	})
	if err != nil {
		t.Fatalf("seed %s at %s: %v", key, at, err)
	}
}

// seedPullRequest records acme/widgets#42 as key's open pull request at head.
func seedPullRequest(t *testing.T, h *harness, key, head string) {
	t.Helper()
	seedPullRequestAt(t, h, key, head, time.Time{})
}

// seedPullRequestAt records acme/widgets#42 as key's open pull request at head, at the pull
// request's updated_at at.
func seedPullRequestAt(t *testing.T, h *harness, key, head string, at time.Time) {
	t.Helper()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: key, Repo: "acme/widgets", Number: 42, Branch: "legion/" + key, HeadSHA: head, HeadUpdatedAt: at,
		})
	})
	if err != nil {
		t.Fatalf("seed the pull request of %s: %v", key, err)
	}
}

// seedReviewDecision records decision as key's review round's decision, on its reviewer's row: an
// approval names the head READY compares the pull request's head with, and nil is a round that
// recorded none.
func seedReviewDecision(t *testing.T, h *harness, key string, decision *record.ReviewDecision) {
	t.Helper()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: key, Role: claim.RoleReviewer, Claim: "reviewer-claim", Decision: decision})
	})
	if err != nil {
		t.Fatalf("seed the review decision of %s: %v", key, err)
	}
}

// seedApproval records key's review round as approving head.
func seedApproval(t *testing.T, h *harness, key, head string) {
	t.Helper()
	seedReviewDecision(t, h, key, &record.ReviewDecision{State: "approved", Head: head})
}

// complete posts req to the completion route.
func (h *harness) complete(req HandoffCompleteRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/handoff/complete", req, nil)
}

// refused asserts recorder is the refusal status and code, and returns its sentence.
func refused(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != code {
		t.Fatalf("code = %q, want %q; error %q", failure.Code, code, failure.Error)
	}
	return failure.Error
}

// completion asserts recorder is an accepted completion and returns its answer.
func completion(t *testing.T, recorder *httptest.ResponseRecorder) HandoffCompleteResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("handoff = %d: %s", recorder.Code, recorder.Body)
	}
	var response HandoffCompleteResponse
	decodeInto(t, recorder, &response)
	return response
}

// lastCompletion is the newest completion facts recorded.
func lastCompletion(t *testing.T, facts *factRecorder) intake.HandoffComplete {
	t.Helper()
	recorded := facts.recorded()
	if len(recorded) == 0 {
		t.Fatal("no fact reached the intake")
	}
	fact, ok := recorded[len(recorded)-1].(intake.HandoffComplete)
	if !ok {
		t.Fatalf("the fact recorded is %T, want a completion", recorded[len(recorded)-1])
	}
	return fact
}

// The commit a completion records is the one the worker reports: for a file-backed phase the pushed
// commit carrying its handoff, which the `legion` tool's handoff_complete finds in the pane. The
// daemon reads no branch head and no handoff file on GitHub for it - nothing but READY reads GitHub
// at a completion - and a request without a commit is refused before anything is recorded.
func TestHandoffCompleteRecordsTheCommitTheWorkerReportsAndReadsNoGitHub(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented"}), http.StatusBadRequest, "MISSING_FIELD"); message != "commit is required" {
		t.Fatalf("a completion naming no commit was refused with %q; want the field named", message)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}

	answer := completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: headSHA}))
	if answer.Note != "" {
		t.Fatalf("answer = %+v, want no note outside READY", answer)
	}
	if fact := lastCompletion(t, facts); fact.Commit != headSHA || fact.Role != claim.RoleImplementer || fact.Summary != "implemented" {
		t.Fatalf("completion = %+v, want the implementer's at the commit it reported, %s", fact, headSHA)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if len(github.bearers) != 0 {
		t.Fatalf("GitHub was called %d times for a completion that is not READY, want never", len(github.bearers))
	}
}

func TestHandoffCompleteEnforcesRoleSpecificFieldsAndDeduplicatesTheCommit(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)

	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "ran the suite", Commit: "aabbcc"}), http.StatusBadRequest, "TESTER_VERDICT_REQUIRED")

	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Verdict: "pass", Commit: "bbccdd"}), http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN")

	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: worker.grant(t), Summary: "reviewed", Ready: true, Commit: "ccddee"}), http.StatusBadRequest, "READY_ROLE_FORBIDDEN")

	request := HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "ddeeff"}
	completion(t, h.complete(request))
	// A retried completion of the same phase at the same commit changes nothing, and says so.
	request.GrantID = tester.grant(t)
	if message := refused(t, h.complete(request), http.StatusConflict, "HANDOFF_ALREADY_RECORDED"); !strings.Contains(message, "at commit ddeeff") {
		t.Fatalf("refusal = %q, want it to name the commit", message)
	}
	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want one deduplicated fact", got)
	}
	fact, ok := got[0].(intake.HandoffComplete)
	if !ok || fact.Role != claim.RoleTester || fact.Verdict != "pass" || fact.Commit != "ddeeff" {
		t.Fatalf("handoff fact = %#v", got[0])
	}
}

// The implementer's retro writes no handoff, so it reports the commit the workspace stands on, the
// one its implementing round already reported: the completion is retro's own, not a repeat.
func TestHandoffCompleteRecordsRetroAtTheCommitItStandsOn(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	recorder := h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done", Commit: "c0ffee"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("retro handoff = %d: %s", recorder.Code, recorder.Body)
	}
	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want the retro completion", got)
	}
	if fact, ok := got[0].(intake.HandoffComplete); !ok || fact.Summary != "retro done" || fact.Commit != "c0ffee" {
		t.Fatalf("handoff fact = %#v, want summary %q at c0ffee", got[0], "retro done")
	}
	if reads := github.reads(); len(reads) != 0 {
		t.Fatalf("contents reads = %v, want none for phases that write no file", reads)
	}
}

// seedCheck records key's production check on acme/widgets#42 at look 2 — open, with its look
// open, unless closed — as the close and a pending first look leave it.
func seedCheck(t *testing.T, h *harness, key string, closed bool) {
	t.Helper()
	opened := time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	woke := opened.Add(time.Hour)
	check := record.ProductionCheck{
		Issue: key, Project: testProject, Tree: key, Repo: "acme/widgets", Number: 42, MergeSHA: "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0",
		OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour), Looks: 2, LookOpen: true, LookWakeAt: &woke,
		LastWakeKind: "issue.deployed", LastWakeDetail: "run 7 completed 2026-10-19T07:00:00Z", LastWakeAt: &woke,
	}
	if closed {
		check.LookOpen, check.ClosedAt, check.Outcome = false, &woke, record.CheckRecorded
	}
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(context.Background(), tx, check)
	})
	if err != nil {
		t.Fatalf("seed the production check of %s: %v", key, err)
	}
}

// lastCheckReport is the newest check report facts recorded.
func lastCheckReport(t *testing.T, facts *factRecorder) intake.CheckReport {
	t.Helper()
	recorded := facts.recorded()
	if len(recorded) == 0 {
		t.Fatal("no fact reached the intake")
	}
	fact, ok := recorded[len(recorded)-1].(intake.CheckReport)
	if !ok {
		t.Fatalf("the fact recorded is %T, want a check report", recorded[len(recorded)-1])
	}
	return fact
}

// The implementer's call on its closed issue, whose production check is open, is a report on the
// check's current look: it carries no commit (the look pushed nothing) and one of the three
// verdicts, and the fact is the check report of that look, not a phase completion. A claim that
// took no task reports all the same — the check fences itself on the look, not on a run — and
// nothing reads GitHub. A repeated report of the same look and verdict is answered as already
// received.
func TestHandoffCompleteRecordsACheckReportOfTheOpenLookWithoutACommit(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", false)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	registration := h.registered(boot, "ses_implementer_untasked")
	implementer := liveClaim{h: h, token: token, session: "ses_implementer_untasked", secret: registration.Secret, tree: "LEGION-208", issue: "LEGION-208"}

	for _, verdict := range []string{"pending", "fail", "pass"} {
		answer := completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked: " + verdict, Verdict: verdict}))
		if answer.Note != "" {
			t.Fatalf("answer = %+v, want no note on a check report", answer)
		}
		want := intake.CheckReport{Issue: "LEGION-208", Claim: token, Look: 2, Verdict: verdict, Summary: "looked: " + verdict}
		if got := lastCheckReport(t, facts); got != want {
			t.Fatalf("check report = %+v, want %+v", got, want)
		}
	}
	if got := facts.recorded(); len(got) != 3 {
		t.Fatalf("facts = %#v, want the three reports", got)
	}
	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked again", Verdict: "pass"}), http.StatusConflict, "HANDOFF_ALREADY_RECORDED"); !strings.Contains(message, "look 2") {
		t.Fatalf("refusal = %q, want it to name the look", message)
	}
	if got := facts.recorded(); len(got) != 3 {
		t.Fatalf("facts = %#v, want the repeated report deduplicated", got)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if len(github.bearers) != 0 {
		t.Fatalf("GitHub was called %d times for a check report, want never", len(github.bearers))
	}
}

// A check report names its verdict: one of pass, pending or fail, and nothing else, with nothing
// recorded otherwise. Ready stays the merger's alone, as on every other call.
func TestHandoffCompleteRefusesACheckReportWithoutOneOfTheThreeVerdicts(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", false)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	for _, verdict := range []string{"", "maybe"} {
		if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked", Verdict: verdict}), http.StatusBadRequest, "CHECK_VERDICT_REQUIRED"); message != "a production check report names a verdict of pass, pending or fail" {
			t.Fatalf("refusal of verdict %q = %q, want the three named", verdict, message)
		}
	}
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked", Verdict: "pass", Ready: true}), http.StatusBadRequest, "READY_ROLE_FORBIDDEN")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts = %#v, want none", got)
	}
}

// The implementer on a done issue with no open production check — none ever opened, or the check
// closed on an earlier look — reports on nothing: the call is refused by name, with nothing
// recorded, rather than read as a completion of a phase no role works.
func TestHandoffCompleteRefusesTheImplementerOnADoneIssueWithNoOpenCheck(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked", Verdict: "pass"}), http.StatusConflict, "CHECK_NOT_OPEN"); message != "LEGION-208 has no open production check; this completion changed nothing" {
		t.Fatalf("refusal with no check = %q", message)
	}
	seedCheck(t, h, "LEGION-208", true)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked", Verdict: "pass"}), http.StatusConflict, "CHECK_NOT_OPEN")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts = %#v, want none", got)
	}
}

// The engine's refusal of a check report — the look the route read ended before the report landed —
// is answered as the engine wrote it.
func TestHandoffCompleteAnswersTheEnginesRefusalOfACheckReport(t *testing.T) {
	h, _ := newHandoffHarness(t, newHandoffGitHub(t), nil, &intake.Refusal{Status: http.StatusConflict, Code: "CHECK_LOOK_NOT_OPEN", Message: "look 2 of LEGION-208's production check is not open"})
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", false)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked", Verdict: "pending"}), http.StatusConflict, "CHECK_LOOK_NOT_OPEN"); message != "look 2 of LEGION-208's production check is not open" {
		t.Fatalf("refusal = %q, want the engine's sentence", message)
	}
}

// A second production check of the same issue — a rerun's, after the first closed (a child set
// back to todo, a root reopened inside its linger) and its merge opened another — counts its looks
// from one again, so its implementer's report on look 1 is a new fact, not the first check's
// report of the same look and verdict: the event a report is recorded under must tell the two
// checks apart (workflow.openCheck replaces the record; nothing clears processed_events). The
// first check's first look was reported pending, as a first look is; the rerun's must be too.
func TestHandoffCompleteTakesAReportOnALaterChecksLookAsANewFact(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	put := func(check record.ProductionCheck) {
		t.Helper()
		if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
			return record.NewStore().PutProductionCheck(context.Background(), tx, check)
		}); err != nil {
			t.Fatalf("put the production check: %v", err)
		}
	}
	opened := time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	first := record.ProductionCheck{
		Issue: "LEGION-208", Project: testProject, Tree: "LEGION-208", Repo: "acme/widgets", Number: 42, MergeSHA: "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0",
		OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour), Looks: 1, LookOpen: true, LookWakeAt: &opened,
		LastWakeKind: "merge", LastWakeDetail: "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0", LastWakeAt: &opened,
	}
	put(first)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "no production carries the merge yet", Verdict: "pending"}))
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("facts after the first check's report = %#v, want one", got)
	}

	// The first check closed as not run when the child was set back to todo; the rerun merged a
	// day later, and the merge opened the issue's second check at its first look.
	closed := opened.Add(2 * time.Hour)
	first.LookOpen, first.ClosedAt, first.Outcome = false, &closed, record.CheckNotRun
	put(first)
	reopened := opened.Add(24 * time.Hour)
	second := first
	second.MergeSHA, second.OpenedAt, second.BoundAt = "f00dfeedf00dfeedf00dfeedf00dfeedf00dfeed", reopened, reopened.Add(336*time.Hour)
	second.Looks, second.LookOpen, second.LookWakeAt = 1, true, &reopened
	second.LastWakeDetail, second.LastWakeAt = second.MergeSHA, &reopened
	second.ClosedAt, second.Outcome = nil, ""
	put(second)

	answer := h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "no production carries the rerun's merge yet", Verdict: "pending"})
	if answer.Code != http.StatusOK {
		t.Fatalf("the rerun's first look reported pending = %d: %s; want it taken as the second check's own report, not the first check's", answer.Code, answer.Body)
	}
	want := intake.CheckReport{Issue: "LEGION-208", Claim: implementer.token, Look: 1, Verdict: "pending", Summary: "no production carries the rerun's merge yet"}
	if got := facts.recorded(); len(got) != 2 || got[1] != want {
		t.Fatalf("facts after the second check's report = %#v, want the first check's report and then %+v", got, want)
	}
}

// The commit is a phase completion's field, read once the grant is redeemed and the record says
// the call is one: an expired grant is refused before the missing commit is, a phase completion
// without a commit is still refused for it, and an implementer's verdict on a phase completion is
// still a tester's field.
func TestHandoffCompleteReadsTheCommitOnlyForAPhaseCompletionAfterTheGrant(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), credential.New(func() time.Time { return now }), nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	expired := implementer.grant(t)
	now = now.Add(time.Minute)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: expired, Summary: "implemented"}), http.StatusForbidden, "GRANT_EXPIRED")
	now = now.Add(-time.Minute)

	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented"}), http.StatusBadRequest, "MISSING_FIELD"); message != "commit is required" {
		t.Fatalf("a phase completion naming no commit was refused with %q; want the field named", message)
	}
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Verdict: "pending", Commit: headSHA}), http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts = %#v, want none", got)
	}
}

// READY is read against the issue's pull request in the repository the issue's record names, as
// the implement App minted for that repository's owner: an issue on a second owner's repository
// is read there, with that owner's token alone.
func TestHandoffCompleteReadyReadsTheIssuesRecordedRepositoryAsItsOwner(t *testing.T) {
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) { g.repo = "zeta/tools" })
	h, facts := newHandoffHarness(t, github, nil, nil)
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: "LEGION-208", Repo: "zeta/tools", DefaultBranch: "main", GitHubRepo: "zeta/tools", Tree: "LEGION-208",
			Project: testProject, Title: "LEGION-208", Phase: phase.Merging, Generation: 1, Status: "in_progress"}); err != nil {
			return err
		}
		return records.PutPullRequest(context.Background(), tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "zeta/tools", Number: 42, Branch: "legion/LEGION-208", HeadSHA: headSHA,
		})
	}); err != nil {
		t.Fatal(err)
	}
	seedApproval(t, h, "LEGION-208", approvedSHA)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "READY on zeta/tools", Ready: true, Commit: headSHA}))
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want the READY", got)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if len(github.bearers) == 0 {
		t.Fatal("READY read nothing of GitHub")
	}
	for _, bearer := range github.bearers {
		if bearer != "Bearer implement-token-for-zeta" {
			t.Fatalf("GitHub was read with %q, want the implement App's token for zeta, the recorded repository's owner, alone", bearer)
		}
	}
}

// GitHub failing to answer a READY read is GitHub's failure, not the head's: the completion is
// refused to complete again, nothing is recorded, and the same READY once GitHub answers is applied
// rather than answered already received. The reads are made as the implement App, minted for the
// repository's owner, whose token never leaves the daemon.
func TestHandoffCompleteReadyRefusesWhenGitHubFailsToAnswerAndAppliesTheRetry(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts, merger := newReadyHarness(t, github)
	github.set(func(g *handoffGitHub) { g.fail = map[string]int{"pulls/": http.StatusInternalServerError} })
	if code, body := ready(t, h, merger); code != http.StatusBadGateway || !strings.HasPrefix(body, "GITHUB_READ_FAILED: ") || !strings.Contains(body, "with 500") || !strings.HasSuffix(body, "GitHub's failure, not the head's: complete again") {
		t.Fatalf("READY while the pull request read fails = %d %q", code, body)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts after GitHub's failure = %#v, want none", got)
	}
	github.set(func(g *handoffGitHub) { g.fail = map[string]int{} })
	if code, body := ready(t, h, merger); code != http.StatusOK || body != "" {
		t.Fatalf("READY once GitHub answers = %d %q; want it published", code, body)
	}
	if fact := lastCompletion(t, facts); !fact.Ready || fact.Commit != headSHA {
		t.Fatalf("completion = %+v, want READY at %s", fact, headSHA)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	for _, bearer := range github.bearers {
		if bearer != "Bearer implement-token-for-acme" {
			t.Fatalf("GitHub was read with %q, want the implement App's token for acme, the repository's owner, alone", bearer)
		}
	}
}

func TestHandoffCompleteRefusesAnUnrecordedIssue(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "aabbcc"}), http.StatusNotFound, "ISSUE_NOT_FOUND")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none for an unrecorded issue", got)
	}
}

func TestHandoffCompleteRefusesExpiredAndRevokedGrants(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, credential.New(func() time.Time { return now }), nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	expired := implementer.grant(t)
	now = now.Add(time.Minute)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: expired, Summary: "implemented", Commit: "aabbcc"}), http.StatusForbidden, "GRANT_EXPIRED")

	now = now.Add(-time.Minute)
	grant := implementer.grant(t)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "implemented", Commit: "bbccdd"}))
	// The same command's grant still authenticates a second call; the phase's dedupe answers it.
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "implemented", Commit: "bbccdd"}), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")

	implementer.replaceRegistration(t)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "implemented again", Commit: "ccddee"}), http.StatusForbidden, "GRANT_REVOKED")
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want only the completion made before the claim was replaced", got)
	}
}

// A merger whose completion was refused READY_REQUIRED corrects it with ready: true at the same
// commit. The refusal is recorded as processed, so the corrected call must be a different fact: it
// reaches the workflow, and only a true retry of it is answered "already received". Otherwise the
// issue stays in merging with no way out but a new commit nothing asks for.
func TestHandoffCompleteAppliesTheMergersCorrectedReadyAtTheSameCommit(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, &intake.Refusal{Status: http.StatusConflict, Code: "READY_REQUIRED", Message: "call the legion tool's handoff_complete with ready: true"})
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	seedApproval(t, h, "LEGION-208", approvedSHA)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "merge-ready", Commit: headSHA}), http.StatusConflict, "READY_REQUIRED")

	facts.mu.Lock()
	facts.refusal = nil
	facts.mu.Unlock()
	corrected := HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "merge-ready", Ready: true, Commit: headSHA}
	completion(t, h.complete(corrected))
	corrected.GrantID = merger.grant(t)
	assertFailure(t, h.complete(corrected), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")
	got := facts.recorded()
	if len(got) != 2 {
		t.Fatalf("handoff facts = %#v, want the refused completion and the corrected READY", got)
	}
	if fact, ok := got[1].(intake.HandoffComplete); !ok || !fact.Ready || fact.Commit != headSHA {
		t.Fatalf("corrected fact = %#v, want READY at %s", got[1], headSHA)
	}
}

// The daemon posts the READY packet as one Dispatch message with the outbox's marker, so a packet
// over record.MessagePostLimit would be refused by Dispatch on every attempt. The route refuses it
// before the fact is applied, naming how far over it is, and records nothing: the merger's
// shortened packet at the same commit, the call the refusal asks for, is applied. The limit counts
// UTF-16 units, as Dispatch does, so a character outside the Basic Multilingual Plane counts twice.
func TestHandoffCompleteRefusesAREADYPacketTheDaemonCannotPostAndAppliesTheShortenedRetry(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	seedApproval(t, h, "LEGION-208", approvedSHA)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	const emoji = "\U0001F600"
	over := emoji + strings.Repeat("x", record.MessagePostLimit-1)
	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: over, Ready: true, Commit: headSHA})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("READY one unit over the limit = %d, want 400; body %s", recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if want := fmt.Sprintf("1 characters over the %d the daemon can post as one Dispatch message (%d/%d)", record.MessagePostLimit, record.MessagePostLimit+1, record.MessagePostLimit); failure.Code != "READY_PACKET_TOO_LONG" || !strings.Contains(failure.Error, want) {
		t.Fatalf("refusal = %+v, want READY_PACKET_TOO_LONG naming %q", failure, want)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts after the refusal = %#v, want none recorded", got)
	}

	shortened := emoji + strings.Repeat("x", record.MessagePostLimit-2)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: shortened, Ready: true, Commit: headSHA}))
	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want the shortened READY", got)
	}
	if fact, ok := got[0].(intake.HandoffComplete); !ok || !fact.Ready || fact.Commit != headSHA || fact.Summary != shortened {
		t.Fatalf("applied fact = %#v, want the shortened READY at %s", got[0], headSHA)
	}
}

// A completion the workflow refused while the child's tree lingered is recorded as processed under
// its key. Re-admission starts a new generation of the tree, bumping only the root's generation,
// and restarts the child's worker in the phase it stood in; the same completion there belongs to
// the new generation, so the key names the tree's generation and the completion is applied rather
// than answered already received.
func TestHandoffCompleteAppliesAfterReadmissionTheCompletionALingeringTreeRefused(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, &intake.Refusal{Status: http.StatusConflict, Code: "TREE_LINGERING", Message: "the tree LEGION-208 is lingering after it left the workflow"})
	seedTree(t, h, "LEGION-208", "LEGION-209")
	// A worker's task carries its issue's run, and a claim serving no run is refused before the
	// workflow sees the completion, so the tree is at a run of its own.
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		setIssue(t, h, key, func(issue *record.Issue) { issue.Generation = 1 })
	}
	planner := newLiveClaim(t, h, "LEGION-209", claim.RolePlanner)
	request := HandoffCompleteRequest{GrantID: planner.grant(t), Summary: "planned", Commit: "facade"}
	assertFailure(t, h.complete(request), http.StatusConflict, "TREE_LINGERING")

	facts.mu.Lock()
	facts.refusal = nil
	facts.mu.Unlock()
	setIssue(t, h, "LEGION-208", func(issue *record.Issue) { issue.Generation++ })
	request.GrantID = planner.grant(t)
	completion(t, h.complete(request))
	if got := facts.recorded(); len(got) != 2 {
		t.Fatalf("handoff facts = %#v, want the refused completion and the new generation's", got)
	}
}

// A worker told to wait replies WAITING and its turn ends, which retires the delivery. A notice
// then wakes it — Envoy starts that turn itself — and the completion it reports there is still the
// run's whose task it took: a tester waiting on CI, an implementer waiting on a review, a merger
// waiting on a person. So the claim keeps the run it is serving past the turn that ended, and
// only a claim that has taken no task at all is refused.
func TestHandoffCompleteAcceptsACompletionFromATurnNoDeliveryBacks(t *testing.T) {
	github := newHandoffGitHub(t)
	h, _ := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	// The WAITING turn ends; its delivery is retired with it.
	if err := machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the waiting turn: %v", err)
	}
	if pending := machine.Claim().Pending; pending != nil {
		t.Fatalf("pending after the turn = %+v, want it retired", pending)
	}

	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}); recorder.Code != http.StatusOK {
		t.Fatalf("completion from a notice-started turn = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
}

// A refusal is recorded as processed, under the key the route builds. That key has to name the
// run the completion is attributed to: a completion of the run that is over is refused, and
// without the run in the key the new run's completion of the same phase at the same commit takes
// that key, is answered ALREADY_RECORDED, never reaches the workflow, and leaves the phase
// stalled behind a worker that was told its report was received.
func TestAStaleCompletionDoesNotTakeTheNewRunsKey(t *testing.T) {
	h := newHarness(t)
	github := newHandoffGitHub(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, ReviewRoundCap: 3, MaxFixAttempts: 3}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	facts := &factRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine, facts}, Dispatch: &statusRecorder{},
		Tokens: handoffTokens{}, GitHubAPI: github.url,
	}).Handler
	// The tester takes run 1's task, and the child is then re-entered at run 2 while it works.
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 1, Status: "in_progress"})
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 2, Status: "in_progress"})
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	complete := func() int {
		t.Helper()
		return h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}).Code
	}

	// The worker is still on generation 1's task while the issue has moved to 2: refused.
	if code := complete(); code != http.StatusConflict {
		t.Fatalf("the stale completion = %d, want 409", code)
	}
	// Generation 2's task is delivered and its turn starts; the same report is now the new run's.
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the stale run's turn: %v", err)
	}
	id := "outbox:99"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: tester.token, Task: "the task", ID: id, Generation: 2}); err != nil {
		t.Fatalf("deliver generation 2's task: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester.token, DeliveryID: id}); err != nil {
		t.Fatalf("start generation 2's turn: %v", err)
	}
	if code := complete(); code != http.StatusOK {
		t.Fatalf("the new run's completion = %d, want 200", code)
	}

	var runs []uint64
	for _, fact := range facts.recorded() {
		if completion, ok := fact.(intake.HandoffComplete); ok {
			runs = append(runs, completion.Generation)
		}
	}
	if len(runs) != 2 || runs[0] != 1 || runs[1] != 2 {
		t.Fatalf("the workflow saw runs %v, want [1 2]", runs)
	}
}

// A pull request waiting to be merged that starts conflicting with its base is sent back to the
// implementer, which merges the base forward in the same run, with the same claim. The key the
// route builds tells one implementing pass from the last by the round, so that conflict round has
// to open a round of its own: otherwise a completion that pushed no new handoff reports the branch
// head of the round before at the same round, takes that completion's key and is answered
// HANDOFF_ALREADY_RECORDED, which tells the worker it was received when nothing moved. The workflow
// sees it instead and refuses it naming the handoff the round must write, and the completion that
// carries one moves the issue on to testing.
func TestAConflictRoundsCompletionIsTheNewRoundsAndNotTheLastOnes(t *testing.T) {
	github := newHandoffGitHub(t)
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, ReviewRoundCap: 3, MaxFixAttempts: 3}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: &statusRecorder{},
		Tokens: handoffTokens{}, GitHubAPI: github.url,
	}).Handler
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	h.recordIssue(issue)
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208",
			Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	complete := func(commit string) *httptest.ResponseRecorder {
		t.Helper()
		return h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: commit})
	}
	phaseNow := func() phase.Phase {
		t.Helper()
		var got phase.Phase
		if err := h.store.Pool().QueryRow(ctx, "select phase from issues where key = 'LEGION-208'").Scan(&got); err != nil {
			t.Fatalf("read the phase: %v", err)
		}
		return got
	}

	// The first round's handoff is the pushed commit carrying it, which the tool found in the pane.
	if recorder := complete("handoff-1"); recorder.Code != http.StatusOK || phaseNow() != phase.Testing {
		t.Fatalf("the first round's completion = %d (%s), phase %s; want 200 and testing", recorder.Code, recorder.Body, phaseNow())
	}
	// Testing, review, retro and the merger's READY pass; the issue waits on a human's merge.
	issue.Phase, issue.Status = phase.AwaitingMerge, "retro"
	h.recordIssue(issue)
	if result, err := intake.ApplyFact(ctx, h.store.Pool(), "github", "mergeability-conflicting", intake.PullRequestMergeability{
		Repo: "acme/widgets", Number: 42, Base: "main", Mergeable: record.MergeabilityConflicting,
	}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("apply the conflicting read = %+v, %v", result.Refusal, err)
	}
	if got := phaseNow(); got != phase.Implementing {
		t.Fatalf("after the conflicting read the issue is in %s, want implementing", got)
	}
	// The implementer takes the conflict round's task, a task of the run it already serves.
	machine, ok := h.supervisor.Machine(implementer.token)
	if !ok {
		t.Fatal("no machine for the implementer")
	}
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: implementer.token}); err != nil {
		t.Fatalf("end the first round's turn: %v", err)
	}
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: implementer.token, Task: "merge main forward", ID: "outbox:conflict", Generation: 1}); err != nil {
		t.Fatalf("deliver the conflict round's task: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: implementer.token, DeliveryID: "outbox:conflict"}); err != nil {
		t.Fatalf("start the conflict round's turn: %v", err)
	}

	// It merges main forward and completes without writing a new handoff: the carrying commit the
	// tool finds is still the first round's.
	stale := complete("handoff-1")
	var failure Failure
	decodeInto(t, stale, &failure)
	if stale.Code != http.StatusConflict || failure.Code != "HANDOFF_NOT_NEW" || !strings.Contains(failure.Error, "reported commit handoff-1 for its previous phase of LEGION-208; write and commit this phase's handoff before completing") {
		t.Fatalf("the completion with no new handoff = %d %+v, want 409 HANDOFF_NOT_NEW naming the handoff to write", stale.Code, failure)
	}
	if got := phaseNow(); got != phase.Implementing {
		t.Fatalf("after the refused completion the issue is in %s, want implementing", got)
	}
	// With the conflict round's handoff written and committed, its completion moves the issue on.
	if recorder := complete("handoff-2"); recorder.Code != http.StatusOK || phaseNow() != phase.Testing {
		t.Fatalf("the conflict round's completion = %d (%s), phase %s; want 200 and testing", recorder.Code, recorder.Body, phaseNow())
	}
}

// The wait that follows a task's turn can be hours long — a tester on CI, a merger on a person —
// and a daemon restarted inside it rebuilds the machine from the store. The run the claim serves
// has to be there, or the completion the worker reports when a notice finally wakes it is refused
// by a daemon that has forgotten which run it was working.
func TestTheRunAClaimServesSurvivesARestart(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	if err := machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the task's turn: %v", err)
	}

	if restarted := h.supervisor.restart(t, tester.token); restarted.Claim().ServingGeneration != 1 {
		t.Fatalf("the rebuilt claim serves generation %d, want 1", restarted.Claim().ServingGeneration)
	}
	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}); recorder.Code != http.StatusOK {
		t.Fatalf("completion after the restart = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if completion := lastCompletion(t, facts); completion.Generation != 1 {
		t.Fatalf("completion generation = %d, want the run's 1", completion.Generation)
	}
}

// Oh My Pi's own agent_start names no prompt, so a turn that starts after the daemon's
// five-second bound confirms nothing: the task is taken back and waits for the sweep to resend it
// while the worker is already doing it. A worker on its first task has no earlier run to fall
// back on, and its completion would be refused; the task it holds is the only run it can be
// working, so that is what the completion is attributed to.
func TestACompletionFromATurnThatStartedLateNamesTheTaskTheWorkerHolds(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	token, boot := h.launch("LEGION-208", claim.RoleTester)
	session := "ses_tester_late"
	registration := h.registered(boot, session)
	tester := liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: "LEGION-208", issue: "LEGION-208"}
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	if err := machine.Handle(ctx, supervise.RequestReady{Claim: token, Generation: machine.Claim().Generation, Session: session}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	id := "outbox:42"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: "the task", ID: id, Generation: 1}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	// The prompt is acknowledged and no turn starts within the bound, so the task is taken back —
	// keeping the mark, because the turn that has not started may still be this task's. The turn
	// the worker is in then starts late, naming no prompt, and confirms nothing. That leaves the
	// task delivered and unconfirmed, which is the state this completion arrives in.
	if err := machine.Handle(ctx, supervise.PromptAcked{Claim: token, Generation: machine.Claim().Generation, DeliveryID: id}); err != nil {
		t.Fatalf("acknowledge the prompt: %v", err)
	}
	// The bound's take-back is driven in internal/supervise, where the clock moves
	// (TestOnlyTheTurnBoundLeavesTheTaskMarkedAsRead); this route test starts from the state it
	// leaves.
	if p := machine.Claim().Pending; p == nil || p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the acknowledgement = %+v, want it delivered and unconfirmed", p)
	}

	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}); recorder.Code != http.StatusOK {
		t.Fatalf("completion from the late turn = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if completion := lastCompletion(t, facts); completion.Generation != 1 {
		t.Fatalf("completion generation = %d, want the task's 1", completion.Generation)
	}
}

// A task the agent refused is not a task it read, whichever way it refused. In a turn of its own
// ("Agent is already processing", the d9194522 sequence) it never saw this prompt; with no turn
// running (a provider answering "No API key found" before any turn began) the prompt did not run
// at all. Either way a completion arriving in whatever turn follows is not that task's run, and a
// claim that has confirmed nothing has no run to attribute it to.
func TestACompletionInAForeignTurnIsNotTheRefusedTasksRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refusal   string
		turnFirst bool
	}{
		{
			name:      "refused in a turn of the agent's own",
			refusal:   "Agent is already processing. Use steer() or followUp() to queue messages",
			turnFirst: true,
		},
		{name: "refused with no turn running", refusal: "No API key found for provider anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
				Phase: phase.Testing, Generation: 2, Status: "in_progress"})
			token, boot := h.launch("LEGION-208", claim.RoleTester)
			session := "ses_tester_foreign"
			registration := h.registered(boot, session)
			tester := liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: "LEGION-208", issue: "LEGION-208"}
			machine, ok := h.supervisor.Machine(token)
			if !ok {
				t.Fatal("no machine for the tester")
			}
			ctx := context.Background()
			if err := machine.Handle(ctx, supervise.RequestReady{Claim: token, Generation: machine.Claim().Generation, Session: session}); err != nil {
				t.Fatalf("ready: %v", err)
			}
			// The claim has confirmed no task: run 1's prompt was acknowledged and its turn was
			// never seen, and the re-entry's stop retired that task on its way through. The child
			// re-entered at run 2, whose task is delivered and acknowledged.
			second := "outbox:42"
			if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: "run 2's task", ID: second, Generation: 2}); err != nil {
				t.Fatalf("deliver run 2's task: %v", err)
			}
			if err := machine.Handle(ctx, supervise.PromptAcked{Claim: token, Generation: machine.Claim().Generation, DeliveryID: second}); err != nil {
				t.Fatalf("acknowledge run 2's prompt: %v", err)
			}
			// The busy refusal is the agent already being in a turn, so that turn starts first;
			// the provider's refusal arrives with no turn running at all, and the notice's turn —
			// the one the completion is reported from — starts after it.
			notice := func() {
				t.Helper()
				if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
					t.Fatalf("the notice's turn: %v", err)
				}
			}
			if tc.turnFirst {
				notice()
			}
			if err := machine.Handle(ctx, supervise.StreamLateRefusal{Claim: token, DeliveryID: second, Error: tc.refusal}); err != nil {
				t.Fatalf("refuse run 2's prompt: %v", err)
			}
			if !tc.turnFirst {
				notice()
			}

			assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}), http.StatusConflict, "HANDOFF_NO_RUN")
		})
	}
}

// An operator's task belongs to no run: it carries no generation, and the operator asked for it
// whatever phase the issue is in. Running one must not cost the worker the run it is serving —
// before, the operator's task became the claim's run and the phase stayed open behind a worker
// whose every completion was refused.
func TestAnOperatorsTaskLeavesTheRunTheClaimIsServing(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	// The run's task is worked and its turn ends; the run the claim serves is that one.
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the run's turn: %v", err)
	}
	// An operator's task runs and ends in between.
	operator := "outbox:operator"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: tester.token, Task: "say it again", ID: operator}); err != nil {
		t.Fatalf("deliver the operator's task: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester.token, DeliveryID: operator}); err != nil {
		t.Fatalf("start the operator's turn: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the operator's turn: %v", err)
	}

	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}); recorder.Code != http.StatusOK {
		t.Fatalf("completion after the operator's task = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if completion := lastCompletion(t, facts); completion.Generation != 1 {
		t.Fatalf("completion generation = %d, want the run's 1", completion.Generation)
	}
}

// A confirmation a busy refusal takes back names a turn the delivery never had: the pane was
// working something else, which is the sequence this branch saw live at `d9194522`. The next
// run's task has not been taken, so a completion arriving in that foreign turn is still the old
// run's — and the workflow refuses it, rather than crediting the new run with a completion of a
// task nobody has read.
func TestATakenBackConfirmationDoesNotMoveTheRunTheClaimIsServing(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	// The tester served run 1, and the child is re-entered at run 2 once that turn ends.
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 1, Status: "in_progress"})
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the first run's turn: %v", err)
	}
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 2, Status: "in_progress"})
	// The next run's task is sent, a foreign turn confirms it, and the agent refuses it as busy.
	id := "outbox:99"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: tester.token, Task: "the next run's task", ID: id, Generation: 2}); err != nil {
		t.Fatalf("deliver the next run's task: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester.token, DeliveryID: id}); err != nil {
		t.Fatalf("start the foreign turn: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamLateRefusal{Claim: tester.token, DeliveryID: id,
		Error: "Agent is already processing. Use steer() or followUp() to queue messages"}); err != nil {
		t.Fatalf("refuse the task: %v", err)
	}
	if p := machine.Claim().Pending; p == nil || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the refusal = %+v, want it waiting unconfirmed", p)
	}

	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}); recorder.Code != http.StatusOK {
		t.Fatalf("handoff = %d; body %s", recorder.Code, recorder.Body)
	}
	// The workflow refuses generation 1 against an issue on 2; crediting the new run would accept
	// a completion of a task the worker has not read.
	if completion := lastCompletion(t, facts); completion.Generation != 1 {
		t.Fatalf("completion generation = %d, want the old run's 1", completion.Generation)
	}
}

// A claim that has taken no task cannot say which run a completion belongs to: an operator
// running the command by hand, or a worker that never confirmed a delivery.
func TestHandoffCompleteRefusesACompletionFromAClaimThatTookNoTask(t *testing.T) {
	h, _ := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	token, boot := h.launch("LEGION-208", claim.RoleTester)
	session := "ses_tester_untasked"
	registration := h.registered(boot, session)
	untasked := liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: "LEGION-208", issue: "LEGION-208"}

	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: untasked.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}), http.StatusConflict, "HANDOFF_NO_RUN")
}

// The run a completion belongs to is the run of the task the worker is working, and the daemon
// reads it from the delivery that turn confirmed rather than from the pane: a live worker is
// handed the next run's task without being relaunched, so its own environment still names the run
// it was launched for. The engine refuses a completion of a run the issue has left
// (TestACompletionFromAnInterruptedRunIsRefused); what this route owes is the attribution.
func TestHandoffCompleteCarriesTheRunOfTheTaskBeingWorked(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delivered uint64
		relaunch  bool
	}{
		{name: "the task of the run the issue is on", delivered: 2},
		{name: "the task of the run that is over", delivered: 1},
		{name: "a worker relaunched inside one run", delivered: 2, relaunch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivered := tc.delivered
			github := newHandoffGitHub(t)
			h, facts := newHandoffHarness(t, github, nil, nil)
			h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
				Phase: phase.Testing, Generation: 2, Status: "in_progress"})
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			machine, ok := h.supervisor.Machine(tester.token)
			if !ok {
				t.Fatal("no machine for the tester")
			}
			// The pane keeps working; only the task it holds changes.
			if err := machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
				t.Fatalf("end the first turn: %v", err)
			}
			id := "outbox:99"
			if err := machine.Handle(context.Background(), supervise.RequestDeliver{Claim: tester.token, Task: "the task", ID: id, Generation: delivered}); err != nil {
				t.Fatalf("deliver: %v", err)
			}
			if err := machine.Handle(context.Background(), supervise.StreamTurnStart{Claim: tester.token, DeliveryID: id}); err != nil {
				t.Fatalf("start the turn: %v", err)
			}
			if tc.relaunch {
				// The claim's own launch counter moves — a crash relaunch, a resume — while the
				// run it is serving does not. A suspension is held for the agent's turn, so the turn
				// ends first.
				if err := machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
					t.Fatalf("end the turn: %v", err)
				}
				if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: tester.token, By: supervise.SuspendedByOperator}); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				if err := machine.Handle(context.Background(), supervise.RequestResume{Claim: tester.token}); err != nil {
					t.Fatalf("resume: %v", err)
				}
				// The relaunched pane registers its session again, which is what revokes the old
				// grants; the run the claim is serving is not a property of the process.
				tester.replaceRegistration(t)
			}

			if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA}); recorder.Code != http.StatusOK {
				t.Fatalf("handoff = %d; body %s", recorder.Code, recorder.Body)
			}
			if completion := lastCompletion(t, facts); completion.Generation != delivered {
				t.Fatalf("the completion names generation %d, want the delivery's %d", completion.Generation, delivered)
			}
		})
	}
}

// newReadyHarness is the merger of LEGION-208 at merging, with its pull request acme/widgets#42
// recorded at headSHA, retro's last push, and its review round's approval of approvedSHA: what a
// READY is read against.
func newReadyHarness(t *testing.T, github *handoffGitHub) (*harness, *factRecorder, liveClaim) {
	t.Helper()
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	seedApproval(t, h, "LEGION-208", approvedSHA)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	return h, facts, merger
}

func TestHandoffCompleteReadyRefusesAHeadThatMovedBeforeTheMergerCanArmIt(t *testing.T) {
	const movedHead = "f00d00000000000000000000000000000000ff"
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) { g.pullHead = movedHead })
	h, facts, merger := newReadyHarness(t, github)

	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "READY at the old head", Ready: true, Commit: headSHA})
	if message := refused(t, recorder, http.StatusConflict, "READY_HEAD_MOVED"); !strings.Contains(message, movedHead) {
		t.Fatalf("READY head-moved refusal = %q, want it to name %s", message, movedHead)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts after stale READY = %#v, want none", got)
	}

	recorder = h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "READY at the moved head", Ready: true, Commit: movedHead})
	completion(t, recorder)
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts after the re-posted READY = %#v, want one at %s", got, movedHead)
	}
	if got := lastCompletion(t, facts).Commit; got != movedHead {
		t.Fatalf("re-posted READY commit = %s, want %s", got, movedHead)
	}
}

// The route holds READY's commit to GitHub's head and passes the workflow the record as it read it,
// head and clock, which follows GitHub only by webhook and can lag, and GitHub's clock in that read.
// Here the record still holds an earlier push, at an earlier clock, while GitHub and the commit are
// at retro's last: the READY passes, and its fact carries the lagging record and GitHub's clock, so
// the workflow takes the record as lagging, not moved, and stamps the READY's head with that clock.
func TestHandoffCompleteReadyPassesTheRecordedHeadItRead(t *testing.T) {
	recordedAt, githubAt := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 12, 10, 5, 0, 0, time.UTC)
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) { g.pullUpdatedAt = githubAt })
	h, facts, merger := newReadyHarness(t, github)
	seedPullRequestAt(t, h, "LEGION-208", "stale", recordedAt)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "READY at retro's last push", Ready: true, Commit: headSHA}))
	got := lastCompletion(t, facts)
	if got.Commit != headSHA || got.RecordedHead != "stale" || !got.RecordedHeadAt.Equal(recordedAt) || !got.HeadAt.Equal(githubAt) {
		t.Fatalf("the READY fact = commit %q, recorded %q at %s, GitHub's clock %s; want %q, the record's %q at %s, and %s",
			got.Commit, got.RecordedHead, got.RecordedHeadAt, got.HeadAt, headSHA, "stale", recordedAt, githubAt)
	}
}

// GitHub can answer a read from a replica that has not seen the newest push, while the daemon's
// record already holds it from that push's webhook: the record is then ahead of GitHub's answer,
// and a READY at the head GitHub showed names a head the pull request has left. Both clocks are the
// pull request's updated_at, to the second, so a record at another head whose clock is not before
// GitHub's answer, a same-second tie included, may be ahead of it, and READY is refused, naming the
// recorded head, with nothing recorded. A tie costs one retry, until the head's own webhook lands.
func TestHandoffCompleteReadyRefusesWhenTheRecordIsAheadOfGitHubsAnswer(t *testing.T) {
	const newer = "beef00000000000000000000000000000000ff"
	githubAt := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		recordedAt time.Time
	}{
		{"a record later than GitHub's answer", githubAt.Add(5 * time.Minute)},
		{"a record in the same second as GitHub's answer", githubAt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) { g.pullUpdatedAt = githubAt })
			h, facts, merger := newReadyHarness(t, github)
			seedPullRequestAt(t, h, "LEGION-208", newer, tc.recordedAt)
			recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "READY at the head GitHub showed", Ready: true, Commit: headSHA})
			if message := refused(t, recorder, http.StatusConflict, "READY_HEAD_MOVED"); !strings.Contains(message, newer) {
				t.Fatalf("READY with the record ahead of GitHub = %q, want it to name the recorded head %s", message, newer)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("handoff facts = %#v, want none for a refused READY", got)
			}
		})
	}
}

// A pull request closed without merging has no merge left to arm, so READY is refused there by
// GitHub's own state, rather than accepted and left waiting on whether the close's webhook arrives
// in time to tell the architect: the READY's advance of the record can make that webhook look
// older than the record, and drop it.
func TestHandoffCompleteReadyRefusesAPullRequestClosedWithoutMerging(t *testing.T) {
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) { g.pullState = "closed" })
	h, facts, merger := newReadyHarness(t, github)
	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "READY on a closed pull request", Ready: true, Commit: headSHA})
	if message := refused(t, recorder, http.StatusConflict, "READY_PULL_REQUEST_CLOSED"); !strings.Contains(message, "#42") {
		t.Fatalf("READY on a closed pull request = %q, want it to name pull request #42", message)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none for a refused READY", got)
	}
}

// A pull request whose change touches a by_hand path is READY for a person to merge, never armed:
// the answer carries MANUAL_MERGE_REQUIRED, and a rename counts both of its paths, so moving a
// protected file out of a by_hand path is by hand too.
// The completion the workflow records carries the note too, so the packet the daemon tells carries
// it and nobody it reaches arms the pull request.
func TestHandoffCompleteReadyMarksAByHandPullRequestWithoutAutoArmingIt(t *testing.T) {
	for _, tc := range []struct {
		name, files string
	}{
		{"a by_hand path", `[{"filename":"migrations/versions/0001_init.py"}]`},
		{"a rename out of a by_hand path", `[{"filename":"src/legacy.py","previous_filename":"migrations/versions/0001_init.py","status":"renamed"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := byHandGitHub(t, tc.files)
			h, facts, merger := newReadyHarness(t, github)
			code, body := ready(t, h, merger)
			if code != http.StatusOK || !strings.HasPrefix(body, "MANUAL_MERGE_REQUIRED:") {
				t.Fatalf("READY for a by-hand pull request = %d %q", code, body)
			}
			if got := lastCompletion(t, facts).MergeNote; got != body {
				t.Fatalf("the recorded READY's by-hand note = %q, want the answer's %q", got, body)
			}
		})
	}
}

// A by-hand READY the design gate refuses keeps its note in the refusal, so the merger, which
// submits the merge on the architect's word once the gate opens, already knows a person merges it.
func TestHandoffCompleteReadyKeepsTheByHandNoteInADesignGateRefusal(t *testing.T) {
	github := byHandGitHub(t, `[{"filename":"migrations/versions/0001_init.py"}]`)
	h, facts := newHandoffHarness(t, github, nil, &intake.Refusal{Status: http.StatusConflict, Code: "DESIGN_GATE_CLOSED", Message: "READY refused: approve design version 4 before requesting READY."})
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	seedApproval(t, h, "LEGION-208", approvedSHA)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "gate facts hold", Ready: true, Commit: headSHA})
	if message := refused(t, recorder, http.StatusConflict, "DESIGN_GATE_CLOSED"); !strings.Contains(message, "MANUAL_MERGE_REQUIRED: pull request #42") {
		t.Fatalf("the design gate's refusal of a by-hand READY = %q, want it to carry the by-hand note", message)
	}
	if got := lastCompletion(t, facts).MergeNote; !strings.HasPrefix(got, "MANUAL_MERGE_REQUIRED:") {
		t.Fatalf("the refused READY's by-hand note = %q, want it recorded for the approval to tell", got)
	}
}

// The packet the daemon tells for a by-hand READY carries its note above the merger's packet, so it
// is the told packet that must fit one Dispatch message. A packet that fits alone but not with the
// note is refused, naming the note's share, and records nothing.
func TestHandoffCompleteReadyRefusesAByHandPacketTooLongWithItsNote(t *testing.T) {
	h, facts, merger := newReadyHarness(t, byHandGitHub(t, `[{"filename":"migrations/versions/0001_init.py"}]`))
	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: strings.Repeat("x", record.MessagePostLimit), Ready: true, Commit: headSHA})
	if message := refused(t, recorder, http.StatusBadRequest, "READY_PACKET_TOO_LONG"); !strings.Contains(message, "by-hand note") {
		t.Fatalf("a by-hand READY whose packet fits only alone = %q, want it refused naming the by-hand note", message)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none for a refused READY", got)
	}
}

// byHandGitHub is a GitHub whose pull request #42 changes files, under a by_hand rule for
// migrations/versions/.
func byHandGitHub(t *testing.T, files string) *handoffGitHub {
	t.Helper()
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) {
		g.merge.ByHand = []config.PathPattern{{Expression: "^migrations/versions/", Regexp: regexp.MustCompile("^migrations/versions/")}}
		g.pullFiles = files
	})
	return github
}

// GitHub lists at most 3,000 of a pull request's changed files, so a by_hand path past that limit
// is never read. With by_hand rules configured, READY refuses a pull request at the limit, naming
// why, rather than calling an unread path safe to arm.
func TestHandoffCompleteReadyRefusesAFileListGitHubTruncated(t *testing.T) {
	files := make([]string, 3000)
	for i := range files {
		files[i] = fmt.Sprintf(`{"filename":"src/file%04d.go"}`, i)
	}
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) {
		g.merge.ByHand = []config.PathPattern{{Expression: "^migrations/versions/", Regexp: regexp.MustCompile("^migrations/versions/")}}
		g.pullFiles = "[" + strings.Join(files, ",") + "]"
	})
	h, facts, merger := newReadyHarness(t, github)
	if code, body := ready(t, h, merger); code != http.StatusConflict || !strings.HasPrefix(body, "READY_FILES_TRUNCATED:") {
		t.Fatalf("READY of a pull request with 3,000 changed files = %d %q", code, body)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none for a refused READY", got)
	}
}

// A disarm row and the READY merge row a new READY queues each decide by GitHub and then the record
// when they run, in whichever order the outbox runs them: a disarm still unfinished when a new READY
// is accepted keeps any arm at the new READY head, and the READY merge submits nothing while an arm
// stands. READY therefore does not wait for it.
func TestHandoffCompleteReadyDoesNotWaitForAnUnfinishedDisarm(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts, merger := newReadyHarness(t, github)
	row, err := record.NewOutboxRow("LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error { return record.NewStore().Enqueue(context.Background(), tx, row) }); err != nil {
		t.Fatalf("queue the disarm: %v", err)
	}
	if code, body := ready(t, h, merger); code != http.StatusOK || body != "" {
		t.Fatalf("READY with the issue's disarm unfinished = %d %q, want it published", code, body)
	}
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want the one READY", got)
	}
}

// A by-hand READY never leaves an arm from an earlier ordinary READY standing. A push that starts
// touching a by_hand path queues its disarm before the merger reaches READY again; while that
// durable disarm has not finished, the by-hand READY waits rather than accepting a head GitHub
// could still merge automatically.
func TestHandoffCompleteReadyByHandWaitsForAnUnfinishedDisarm(t *testing.T) {
	github := byHandGitHub(t, `[{"filename":"migrations/versions/0002_by_hand.py"}]`)
	h, facts, merger := newReadyHarness(t, github)
	row, err := record.NewOutboxRow("LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error { return record.NewStore().Enqueue(context.Background(), tx, row) }); err != nil {
		t.Fatalf("queue the disarm: %v", err)
	}
	if code, body := ready(t, h, merger); code != http.StatusConflict || !strings.HasPrefix(body, "READY_DISARM_PENDING:") {
		t.Fatalf("by-hand READY with the issue's disarm unfinished = %d %q, want it held", code, body)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none until the disarm finishes", got)
	}
}

// ready posts the merger's READY and returns its answer: the note when it was published, or the
// refusal's code and sentence.
func ready(t *testing.T, h *harness, merger liveClaim) (code int, body string) {
	t.Helper()
	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "gate facts hold", Ready: true, Commit: headSHA})
	if recorder.Code == http.StatusOK {
		var response HandoffCompleteResponse
		decodeInto(t, recorder, &response)
		return recorder.Code, response.Note
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	return recorder.Code, failure.Code + ": " + failure.Error
}

// A merger's READY names the head the daemon will submit, and GitHub, under the base branch's own
// rules, decides whether that head merges: the daemon neither judges the head's checks nor reads
// them. READY is published at a head whose required check failed, at one whose required status
// failed, at one that reports no result at all (a push that skipped CI when it should not have),
// and at one still running; the submission of the merge waits on GitHub for whatever the base
// requires of the head, and the workflow withdraws the READY if the head's CI turns red. The
// proof is the two routes a judgment of READY's own would read and this one asks for neither of:
// the head's check runs, and its commit statuses.
func TestHandoffCompleteReadyAcceptsAHeadWithoutReadingItsChecks(t *testing.T) {
	const (
		ciGreen      = `{"id":1,"name":"ci","status":"completed","conclusion":"success"}`
		ciFailed     = `{"id":1,"name":"ci","status":"completed","conclusion":"failure"}`
		ciRunning    = `{"id":1,"name":"ci","status":"in_progress","conclusion":null}`
		legacyGreen  = `{"context":"legacy","state":"success"}`
		legacyFailed = `{"context":"legacy","state":"failure"}`
	)
	for _, tc := range []struct {
		name, checkRun, status string
	}{
		{"a required check that failed", ciFailed, legacyGreen},
		{"a required status that failed", ciGreen, legacyFailed},
		{"a head whose push skipped CI, with no result for either", "", ""},
		{"a required check still running", ciRunning, legacyGreen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) {
				g.checkRuns, g.statuses = "["+tc.checkRun+"]", "["+tc.status+"]"
			})
			h, facts, merger := newReadyHarness(t, github)
			if code, body := ready(t, h, merger); code != http.StatusOK || body != "" {
				t.Fatalf("READY = %d %q; want it published with no note", code, body)
			}
			if fact := lastCompletion(t, facts); !fact.Ready || fact.Commit != headSHA {
				t.Fatalf("completion = %+v, want READY at %s", fact, headSHA)
			}
			for _, path := range github.serves() {
				if strings.HasSuffix(path, "/check-runs") || strings.HasSuffix(path, "/status") {
					t.Fatalf("READY read %q while it judged the packet, want no read of the head's checks", path)
				}
			}
		})
	}
}

// A merger's READY names the head a human merges, and a squash merge commits that head merged into
// the default branch: the issue's own handoffs, .legion/<issue>/, must be gone from it (retro's last
// commit removes them; dispatch://LEGION-605). READY reads the head's tree on GitHub and is refused,
// naming the head and the directory, while it still holds .legion/<issue>/, and published once
// GitHub answers that the head has none. A read GitHub fails leaves the head unknown, and READY is
// refused as GitHub's failure to retry, never sending the issue back to retro. A pull request a
// person already merged is published unread, its answer saying so: its head can no longer change,
// and the workflow takes it on to the production check only on that READY.
func TestHandoffCompleteReadyRefusesAHeadThatStillCarriesTheIssuesHandoffs(t *testing.T) {
	const (
		stillCarries  = `READY_HEAD_CARRIES_HANDOFFS: head c0de00000000 of pull request #42 still carries .legion/LEGION-208/`
		readRefused   = `GITHUB_READ_FAILED: GitHub's read of .legion/LEGION-208/ at head c0de00000000 of pull request #42 failed, so whether the head still carries it is unknown: complete again`
		alreadyMerged = `pull request #42 is already merged, so READY was published without reading its head's checks or handoffs`
	)
	for _, tc := range []struct {
		name      string
		merged    bool
		carries   bool
		readFails bool
		want      string
	}{
		{"a head that still carries them", false, true, false, stillCarries},
		{"a head retro removed them from", false, false, false, ""},
		{"a head whose tree GitHub fails to read", false, false, true, readRefused},
		{"a pull request a person merged while its head still carried them", true, true, false, alreadyMerged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) {
				g.merged = tc.merged
				if tc.readFails {
					g.fail["contents/"] = http.StatusInternalServerError
				}
			})
			h, facts, merger := newReadyHarness(t, github)
			if tc.carries {
				github.put(headSHA, ".legion/LEGION-208/implement.json", handoffFile("implement", "LEGION-208"))
			}
			code, body := ready(t, h, merger)
			switch {
			case tc.merged:
				if code != http.StatusOK || body != alreadyMerged {
					t.Fatalf("READY of a merged pull request = %d %q; want it published saying %q", code, body, alreadyMerged)
				}
				if reads := github.reads(); len(reads) != 0 {
					t.Fatalf("READY read %v of a merged pull request's head, want nothing", reads)
				}
			case tc.want == "":
				if code != http.StatusOK || body != "" {
					t.Fatalf("READY = %d %q; want it published", code, body)
				}
				if reads := github.reads(); len(reads) != 1 || reads[0] != ".legion/LEGION-208@"+headSHA {
					t.Fatalf("READY read %v, want the handoffs directory at the pull request's head", reads)
				}
			case tc.readFails:
				if code != http.StatusBadGateway || !strings.HasPrefix(body, tc.want) {
					t.Fatalf("READY = %d %q; want %q", code, body, tc.want)
				}
			default:
				if code != http.StatusConflict || !strings.HasPrefix(body, tc.want) {
					t.Fatalf("READY = %d %q; want %q", code, body, tc.want)
				}
			}
			if recorded := len(facts.recorded()); (recorded == 1) != (code == http.StatusOK) {
				t.Fatalf("READY = %d with %d facts recorded; want one exactly when it was published", code, recorded)
			}
		})
	}
}

// What the base branch requires of a head is GitHub's business, and READY reads none of it: its
// refusals hold the same on a base that requires a check of every head and on one that requires
// nothing, and a rulesets read that would fail - a private repository on a plan without rulesets
// answers it 403, "make this repository public to enable this feature"
// (docs/solutions/legion/controller-gate-2-required-checks-live-reads.md) - refuses no READY,
// since READY makes no such read.
func TestHandoffCompleteReadyRefusesTheSameWhateverTheBaseRequires(t *testing.T) {
	const (
		unprotected  = `{"name":"main","protected":false}`
		protected    = `{"name":"main","protected":true,"protection":{"required_status_checks":{"contexts":["legacy"]}}}`
		requiresCI   = `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`
		stillCarries = `READY_HEAD_CARRIES_HANDOFFS: head c0de00000000 of pull request #42 still carries .legion/LEGION-208/`
		conflicts    = `READY_HEAD_CONFLICTS: head c0de00000000 of pull request #42 conflicts with its base main`
	)
	for _, base := range []struct {
		name, rules, branch string
		rulesStatus         int
	}{
		{"a base requiring a check of every head", requiresCI, protected, 0},
		{"a base requiring none", `[]`, unprotected, 0},
		{"a repository whose rulesets read fails", `{"message":"Server Error"}`, unprotected, http.StatusInternalServerError},
	} {
		t.Run(base.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, mergeableState, want string
				carries                    bool
			}{
				{"a head retro removed the handoffs from", "clean", "", false},
				{"a head that still carries them", "clean", stillCarries, true},
				{"a head that conflicts with its base", "dirty", conflicts, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					github := newHandoffGitHub(t)
					github.set(func(g *handoffGitHub) {
						g.rules, g.rulesStatus, g.mainBranch, g.mergeableState = base.rules, base.rulesStatus, base.branch, tc.mergeableState
					})
					h, facts, merger := newReadyHarness(t, github)
					if tc.carries {
						github.put(headSHA, ".legion/LEGION-208/implement.json", handoffFile("implement", "LEGION-208"))
					}
					code, body := ready(t, h, merger)
					switch {
					case tc.want == "":
						if code != http.StatusOK || body != "" {
							t.Fatalf("READY = %d %q; want it published", code, body)
						}
					case code != http.StatusConflict || !strings.HasPrefix(body, tc.want):
						t.Fatalf("READY = %d %q; want %q", code, body, tc.want)
					}
					if recorded := len(facts.recorded()); (recorded == 1) != (code == http.StatusOK) {
						t.Fatalf("READY = %d with %d facts recorded; want one exactly when it was published", code, recorded)
					}
				})
			}
		})
	}
}

// READY is read against the issue's recorded pull request: a merger whose issue has none recorded
// is refused before GitHub is asked about any pull request.
func TestHandoffCompleteReadyNeedsTheIssuesRecordedPullRequest(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	if code, body := ready(t, h, merger); code != http.StatusConflict || body != "NO_PULL_REQUEST: LEGION-208 has no pull request recorded" {
		t.Fatalf("READY without a pull request = %d %q", code, body)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}
}
