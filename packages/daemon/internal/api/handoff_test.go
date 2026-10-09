package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// implementerEmail and reviewerEmail are the emails the two Apps' commits carry, as GitHub
	// builds them from the App's id and login.
	implementerEmail = "271566630+legion-implementer[bot]@users.noreply.github.com"
	reviewerEmail    = "271566631+legion-reviewer[bot]@users.noreply.github.com"
	// headSHA is the head of legion/LEGION-208, and of pull request #42, unless a test moves it.
	headSHA = "c0de0000000000000000000000000000000000ff"
	// handoffProof is one production-like proof, every field of it non-blank.
	handoffProof = `{"criterion":"a handoff without proof is refused","surface":"the daemon's completion route",` +
		`"command":"handoff_complete after pushing implement.json without proof","observed":"422 HANDOFF_INVALID naming proof",` +
		`"headSha":"0123456789abcdef0123456789abcdef01234567","negativeControl":"the same file with its proof: 200"}`
)

// handoffTokens leases each App a token of its own and the bot identity its commits carry, so a
// test can tell which App's token reached GitHub and author a head as either App.
type handoffTokens struct{}

func (handoffTokens) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	identity := map[appauth.AppRole]appauth.GitIdentity{
		appauth.Implement: {Name: "legion-implementer[bot]", Email: implementerEmail},
		appauth.Review:    {Name: "legion-reviewer[bot]", Email: reviewerEmail},
	}[role]
	return appauth.Lease{Token: string(role) + "-token", ExpiresAt: time.Now().Add(time.Hour), Identity: identity}, nil
}

// appEmail is the email role's commits carry: its App's (appauth.AppRoleFor).
func appEmail(role claim.Role) string {
	if appauth.AppRoleFor(role) == appauth.Implement {
		return implementerEmail
	}
	return reviewerEmail
}

// validHandoff is a handoff of phase word its rules accept, stamped for issue as a worker writes it.
func validHandoff(word, issue string) string {
	body := map[string]string{
		"plan": `"requiredSkills":{"implement":["legion-worker"],"test":["legion-worker"],"review":["legion-worker"]},` +
			`"gapAnalysis":{"findings":[]},"planReview":{"verdict":"approved","rounds":1},"specDepartures":[]`,
		"implement": `"filesChanged":["x.go"],"proof":[` + handoffProof + `]`,
		"test":      `"passed":3,"failed":0,"implementerProof":{"verdict":"verified","how":"re-ran its command at its head"},"proof":[` + handoffProof + `]`,
		"review":    `"verdict":"approved","critical":0`,
	}[word]
	return fmt.Sprintf(`{"schemaVersion":1,"phase":%q,"issue":%q,"completed":"2026-10-09T08:00:00Z",%s}`, word, issue, body)
}

// handoffGitHub is GitHub's REST API for acme/widgets as a completion reads it: the head of each
// branch it has and the author of each commit it knows, the files at each commit, and what READY
// reads of pull request #42 on main. A test changes what it serves between calls; every call's
// bearer and every contents read are recorded.
type handoffGitHub struct {
	url string
	mu  sync.Mutex
	// heads is each branch GitHub has, by name, as its head's sha.
	heads map[string]string
	// authors is the email each commit GitHub knows was authored with, by sha.
	authors map[string]string
	// files is the content of each path at each commit, by sha then path; a directory is listed
	// when a path is under it.
	files map[string]map[string]string
	// fail is the status GitHub answers every route under the given path prefix with instead of
	// its answer: GitHub's trouble.
	fail map[string]int
	// pull request #42: its head, its base's repository id, its mergeable_state and whether it is
	// merged.
	pullHead       string
	baseRepo       int64
	mergeableState string
	merged         bool
	// What READY reads of main and of the head: the rulesets' rules (with the status the read
	// answers when not 200), the branch's protection, and the head's check runs, commit statuses
	// and workflow runs.
	rules        string
	rulesStatus  int
	mainBranch   string
	checkRuns    string
	statuses     string
	workflowRuns string
	bearers      []string
	// contentsRead is each path read through the contents API, as path@ref.
	contentsRead []string
}

func newHandoffGitHub(t *testing.T) *handoffGitHub {
	t.Helper()
	g := &handoffGitHub{
		heads: map[string]string{}, authors: map[string]string{}, files: map[string]map[string]string{}, fail: map[string]int{},
		pullHead: headSHA, baseRepo: 4242, mergeableState: "clean",
		rules:        `[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`,
		mainBranch:   `{"name":"main","protection":{"required_status_checks":{"contexts":["legacy"],"checks":[{"context":"legacy"}]}}}`,
		checkRuns:    `[{"id":1,"name":"ci","status":"completed","conclusion":"success"}]`,
		statuses:     `[{"context":"legacy","state":"success"}]`,
		workflowRuns: `[]`,
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
		path, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/")
		if !ok {
			http.NotFound(w, r)
			return
		}
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
		case strings.HasPrefix(path, "branches/"):
			name := strings.TrimPrefix(path, "branches/")
			sha, ok := g.heads[name]
			if !ok {
				notFound(w, "Branch not found")
				return
			}
			_, _ = fmt.Fprintf(w, `{"name":%q,"commit":%s}`, name, g.commit(sha))
		case path == "pulls/42":
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q},"base":{"ref":"main","repo":{"id":%d}},"mergeable_state":%q,"merged":%t}`, g.pullHead, g.baseRepo, g.mergeableState, g.merged)
		case path == "rules/branches/main":
			if g.rulesStatus != 0 {
				w.WriteHeader(g.rulesStatus)
			}
			_, _ = io.WriteString(w, g.rules)
		case path == "actions/runs":
			if r.URL.Query().Get("head_sha") != g.pullHead {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `{"total_count":%d,"workflow_runs":%s}`, strings.Count(g.workflowRuns, `"id"`), g.workflowRuns)
		case strings.HasSuffix(path, "/check-runs"):
			_, _ = fmt.Fprintf(w, `{"total_count":%d,"check_runs":%s}`, strings.Count(g.checkRuns, `"name"`), g.checkRuns)
		case strings.HasSuffix(path, "/status"):
			_, _ = fmt.Fprintf(w, `{"statuses":%s}`, g.statuses)
		case strings.HasPrefix(path, "commits/"):
			sha := strings.TrimPrefix(path, "commits/")
			if _, ok := g.authors[sha]; !ok {
				notFound(w, "No commit found for SHA: "+sha)
				return
			}
			_, _ = io.WriteString(w, g.commit(sha))
		case strings.HasPrefix(path, "contents/"):
			file := strings.TrimPrefix(path, "contents/")
			g.contentsRead = append(g.contentsRead, file+"@"+ref)
			if content, ok := g.files[ref][file]; ok {
				// GitHub wraps the base64 it answers at sixty columns.
				encoded := base64.StdEncoding.EncodeToString([]byte(content))
				if len(encoded) > 60 {
					encoded = encoded[:60] + "\n" + encoded[60:]
				}
				_, _ = fmt.Fprintf(w, `{"type":"file","path":%q,"encoding":"base64","size":%d,"content":%q}`, file, len(content), encoded)
				return
			}
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
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// commit is sha as GitHub answers a commit, under a branch's `commit` and as GET /commits/<sha>.
func (g *handoffGitHub) commit(sha string) string {
	return fmt.Sprintf(`{"sha":%q,"commit":{"author":{"name":"bot","email":%q}}}`, sha, g.authors[sha])
}

// pushed has role push sha as the head of issue's branch, authored by its App and carrying the
// handoff file phase at ends with, when it ends with one.
func (g *handoffGitHub) pushed(issue, sha string, role claim.Role, at phase.Phase) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.heads["legion/"+issue] = sha
	g.authors[sha] = appEmail(role)
	if word, ok := phase.HandoffFile(at); ok {
		g.putLocked(sha, ".legion/"+issue+"/"+word+".json", validHandoff(word, issue))
	}
}

// put is content at path of commit sha.
func (g *handoffGitHub) put(sha, path, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.putLocked(sha, path, content)
}

func (g *handoffGitHub) putLocked(sha, path, content string) {
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

// acmeWidgets is the repository every issue of the test project works in, as Options.Repository
// answers it: none for any other project.
func acmeWidgets(project string) (ghrepo.Repository, bool) {
	return ghrepo.MustParse("acme/widgets"), project == testProject
}

// newHandoffHarness serves the daemon's routes over a real Postgres, reading GitHub at github as
// the two Apps, with facts recording every fact the routes apply, refused as refusal says.
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
		Handlers: []intake.Handler{facts}, Dispatch: &statusRecorder{},
		Tokens: handoffTokens{}, GitHubOwner: "acme", GitHubAPI: github.url, Repository: acmeWidgets,
	}).Handler
	return h, facts
}

// seedIssueAt records key as a root at the given phase, which is where the handoff route reads the
// phase a completion finishes.
func seedIssueAt(t *testing.T, h *harness, key string, at phase.Phase) {
	t.Helper()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: key, Tree: key, Project: testProject, Title: key, Phase: at, Generation: 1, Status: "in_progress",
		})
	})
	if err != nil {
		t.Fatalf("seed %s at %s: %v", key, at, err)
	}
}

// seedPullRequest records acme/widgets#42 as key's open pull request at head.
func seedPullRequest(t *testing.T, h *harness, key, head string) {
	t.Helper()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: key, Repo: "acme/widgets", Number: 42, Branch: "legion/" + key, HeadSHA: head,
		})
	})
	if err != nil {
		t.Fatalf("seed the pull request of %s: %v", key, err)
	}
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

// The commit a completion records is the issue branch's head on GitHub, read by the daemon as the
// implement App: the worker names none, and a request that tries to is refused as a body the route
// does not take. The phase's handoff file is read at that head, so what the next role reads is
// what the completion was held to.
func TestHandoffCompleteRecordsTheHeadOfTheIssueBranchOnGitHub(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	github.pushed("LEGION-208", headSHA, claim.RoleImplementer, phase.Implementing)

	recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete",
		fmt.Sprintf(`{"grantId":%q,"summary":"implemented","commit":"aabbcc"}`, implementer.grant(t)), nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a completion naming its own commit = %d: %s; want 400 refusing the field", recorder.Code, recorder.Body)
	}
	var body struct {
		Error string `json:"error"`
	}
	decodeInto(t, recorder, &body)
	if !strings.Contains(body.Error, `unknown field "commit"`) {
		t.Fatalf("a completion naming its own commit was refused with %q; want the field named", body.Error)
	}

	answer := completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented"}))
	if answer.Note != "" {
		t.Fatalf("answer = %+v, want no note outside READY", answer)
	}
	if fact := lastCompletion(t, facts); fact.Commit != headSHA || fact.Role != claim.RoleImplementer || fact.Summary != "implemented" {
		t.Fatalf("completion = %+v, want the implementer's at the branch head %s", fact, headSHA)
	}
	if reads := github.reads(); len(reads) != 1 || reads[0] != ".legion/LEGION-208/implement.json@"+headSHA {
		t.Fatalf("contents reads = %v, want the implement handoff at the head", reads)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	for _, bearer := range github.bearers {
		if bearer != "Bearer implement-token" {
			t.Fatalf("GitHub was read with %q, want the implement App's token alone", bearer)
		}
	}
}

func TestHandoffCompleteEnforcesRoleSpecificFieldsAndDeduplicatesTheHead(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)

	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "ran the suite"}), http.StatusBadRequest, "TESTER_VERDICT_REQUIRED")

	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Verdict: "pass"}), http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN")

	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: worker.grant(t), Summary: "reviewed", Ready: true}), http.StatusBadRequest, "READY_ROLE_FORBIDDEN")

	github.pushed("LEGION-208", "ddeeff", claim.RoleTester, phase.Testing)
	request := HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}
	completion(t, h.complete(request))
	// A retried completion of the same phase at the same head changes nothing, and says so.
	request.GrantID = tester.grant(t)
	if message := refused(t, h.complete(request), http.StatusConflict, "HANDOFF_ALREADY_RECORDED"); !strings.Contains(message, "at head ddeeff") {
		t.Fatalf("refusal = %q, want it to name the head", message)
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

// The implementer runs retro and then, after the merge, the production check, and it may report
// both from the one commit: retro's last push is the branch head, and once the squash merge deleted
// the branch, the production check reports the pull request's recorded head, that same commit,
// read as a commit for its author. Each is its own phase's completion.
func TestHandoffCompleteRecordsRetroThenProductionCheckAtOneCommit(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	github.pushed("LEGION-208", "c0ffee", claim.RoleImplementer, phase.Retro)
	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	seedPullRequest(t, h, "LEGION-208", "c0ffee")
	for _, at := range []phase.Phase{phase.Retro, phase.ProductionCheck} {
		seedIssueAt(t, h, "LEGION-208", at)
		if at == phase.ProductionCheck {
			github.set(func(g *handoffGitHub) { delete(g.heads, "legion/LEGION-208") })
		}
		recorder := h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: string(at) + " done"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s handoff = %d: %s", at, recorder.Code, recorder.Body)
		}
	}
	got := facts.recorded()
	if len(got) != 2 {
		t.Fatalf("handoff facts = %#v, want the retro and the production-check completions", got)
	}
	for i, summary := range []string{"retro done", "production_check done"} {
		if fact, ok := got[i].(intake.HandoffComplete); !ok || fact.Summary != summary || fact.Commit != "c0ffee" {
			t.Fatalf("handoff fact %d = %#v, want summary %q at c0ffee", i, got[i], summary)
		}
	}
	if reads := github.reads(); len(reads) != 0 {
		t.Fatalf("contents reads = %v, want none for phases that write no file", reads)
	}
}

// Without the branch, a phase that writes no file falls back to the pull request's recorded head
// only when there is one, and the head's author is still checked, through the commit: a production
// check on a head another App authored is refused like any other. A phase that ends with a file has
// no fallback: its handoff is on the branch or nowhere.
func TestHandoffCompleteWithoutTheBranchFallsBackOnlyToTheRecordedPullRequestHead(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.ProductionCheck)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "checked"}), http.StatusConflict, "HANDOFF_BRANCH_MISSING"); !strings.Contains(message, "legion/LEGION-208 is not on GitHub: push your chain, then complete again") {
		t.Fatalf("refusal without a branch or a pull request = %q", message)
	}

	seedPullRequest(t, h, "LEGION-208", "c0ffee")
	github.set(func(g *handoffGitHub) { g.authors["c0ffee"] = reviewerEmail })
	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "checked"}), http.StatusConflict, "HANDOFF_AUTHOR_MISMATCH"); !strings.Contains(message, reviewerEmail) {
		t.Fatalf("refusal of a pull request head another App authored = %q", message)
	}

	github.set(func(g *handoffGitHub) { g.authors["c0ffee"] = implementerEmail })
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "checked"}))
	if fact := lastCompletion(t, facts); fact.Commit != "c0ffee" {
		t.Fatalf("completion = %+v, want the pull request's head c0ffee", fact)
	}

	// A file-backed phase with the branch gone: the pull request's head is no handoff.
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented"}), http.StatusConflict, "HANDOFF_BRANCH_MISSING")
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want only the production check", got)
	}
}

// An issue of a project the configuration names no repository for cannot have its branch read: the
// completion is refused before GitHub is called, and nothing is recorded. Configuration requires a
// repository, so this is a guard, not a state a running daemon reaches.
func TestHandoffCompleteRefusesAnIssueWhoseProjectHasNoRepository(t *testing.T) {
	github := newHandoffGitHub(t)
	h := newHarness(t)
	facts := &factRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, Controller: h.store, Grants: credential.New(nil),
		Pool: h.store.Pool(), Record: record.NewStore(), Handlers: []intake.Handler{facts}, Dispatch: &statusRecorder{},
		Tokens: handoffTokens{}, GitHubOwner: "acme", GitHubAPI: github.url,
		Repository: func(string) (ghrepo.Repository, bool) { return ghrepo.Repository{}, false },
	}).Handler
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	github.pushed("LEGION-208", headSHA, claim.RoleImplementer, phase.Implementing)

	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented"}), http.StatusConflict, "NO_REPOSITORY"); !strings.Contains(message, "the project legion of LEGION-208 has no repository configured") {
		t.Fatalf("refusal = %q", message)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if len(github.bearers) != 0 {
		t.Fatalf("GitHub was called %d times, want never", len(github.bearers))
	}
}

// A branch GitHub does not have is a chain never pushed, or a branch deleted: the completion is
// refused naming the branch and the push, and nothing is recorded.
func TestHandoffCompleteRefusesABranchGitHubDoesNotHave(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Planning)
	planner := newLiveClaim(t, h, "LEGION-208", claim.RolePlanner)
	if message := refused(t, h.complete(HandoffCompleteRequest{GrantID: planner.grant(t), Summary: "planned"}), http.StatusConflict, "HANDOFF_BRANCH_MISSING"); message != "legion/LEGION-208 is not on GitHub: push your chain, then complete again" {
		t.Fatalf("refusal = %q", message)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}
}

// GitHub failing to answer is GitHub's failure, not the head's: the completion is refused to
// complete again, nothing is recorded, and the same completion once GitHub answers is applied
// rather than answered already received.
func TestHandoffCompleteRefusesWhenGitHubFailsToAnswerAndAppliesTheRetry(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	github.pushed("LEGION-208", headSHA, claim.RoleTester, phase.Testing)
	for _, route := range []string{"branches/legion", "contents/"} {
		github.set(func(g *handoffGitHub) { g.fail = map[string]int{route: http.StatusInternalServerError} })
		message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}), http.StatusBadGateway, "GITHUB_READ_FAILED")
		if !strings.Contains(message, "with 500") || !strings.HasSuffix(message, "GitHub's failure, not the head's: complete again") {
			t.Fatalf("refusal while %s fails = %q", route, message)
		}
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts after GitHub's failures = %#v, want none", got)
	}
	github.set(func(g *handoffGitHub) { g.fail = map[string]int{} })
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}))
	if fact := lastCompletion(t, facts); fact.Commit != headSHA || fact.Verdict != "pass" {
		t.Fatalf("completion = %+v, want the tester's at %s", fact, headSHA)
	}
}

// Every role of an issue shares the branch, so a head another App authored - the previous role's
// commit, with this role's handoff written into it or without any - is not this role's completion:
// it is refused naming the author and the role's App, and told to start a commit of its own. The
// implementer and merger complete as the implement App, every other role as the review App.
func TestHandoffCompleteRefusesAHeadAnotherAppAuthored(t *testing.T) {
	for _, tc := range []struct {
		role   claim.Role
		at     phase.Phase
		author string
		app    string
	}{
		{claim.RoleTester, phase.Testing, implementerEmail, "legion-reviewer[bot]"},
		{claim.RoleImplementer, phase.Implementing, reviewerEmail, "legion-implementer[bot]"},
		{claim.RoleMerger, phase.Merging, reviewerEmail, "legion-implementer[bot]"},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			github := newHandoffGitHub(t)
			h, facts := newHandoffHarness(t, github, nil, nil)
			seedIssueAt(t, h, "LEGION-208", tc.at)
			seedPullRequest(t, h, "LEGION-208", headSHA)
			worker := newLiveClaim(t, h, "LEGION-208", tc.role)
			github.pushed("LEGION-208", headSHA, tc.role, tc.at)
			github.set(func(g *handoffGitHub) { g.authors[headSHA] = tc.author })
			request := HandoffCompleteRequest{GrantID: worker.grant(t), Summary: "done", Ready: tc.role == claim.RoleMerger}
			if tc.role == claim.RoleTester {
				request.Verdict = "pass"
			}
			message := refused(t, h.complete(request), http.StatusConflict, "HANDOFF_AUTHOR_MISMATCH")
			want := fmt.Sprintf("head c0de00000000 of legion/LEGION-208 was authored by %s, not by the %s's App %s (%s): run jj new, then write, commit and push this phase's handoff again", tc.author, tc.role, tc.app, appEmail(tc.role))
			if message != want {
				t.Fatalf("refusal = %q, want %q", message, want)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("handoff facts = %#v, want none", got)
			}
			if reads := github.reads(); len(reads) != 0 {
				t.Fatalf("contents reads = %v, want none before the author held", reads)
			}
		})
	}
}

// A phase that ends with a handoff file is completed only with that file at the head: a head without
// it is refused naming the path and the head, and nothing is recorded. A phase that writes none -
// retro, READY, the production check - reads no file.
func TestHandoffCompleteRefusesAHeadWithoutThePhasesHandoffFile(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Reviewing)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	github.pushed("LEGION-208", headSHA, claim.RoleReviewer, phase.Retro)
	// The previous round's file is not this round's: only the phase's own word is read.
	github.put(headSHA, ".legion/LEGION-208/test.json", validHandoff("test", "LEGION-208"))
	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: reviewer.grant(t), Summary: "reviewed"}), http.StatusConflict, "HANDOFF_FILE_MISSING")
	if message != "no .legion/LEGION-208/review.json at head c0de00000000 of legion/LEGION-208: write, commit and push this phase's handoff, then complete again" {
		t.Fatalf("refusal = %q", message)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}

	seedIssueAt(t, h, "LEGION-208", phase.Retro)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	github.pushed("LEGION-208", "feed00", claim.RoleImplementer, phase.Retro)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "retro done"}))
	if reads := github.reads(); len(reads) != 1 || reads[0] != ".legion/LEGION-208/review.json@"+headSHA {
		t.Fatalf("contents reads = %v, want the reviewer's one read and none for retro", reads)
	}
}

// The handoff at the head is held to its phase's shape (internal/handoff): a file its rules refuse
// is refused naming every field at fault, `<field>: <reason>`, with the stamps a worker copies wrong
// among them, and a file that is not a JSON object is refused as its shape. Nothing is recorded; the
// worker's fix is a new commit and push.
func TestHandoffCompleteRefusesAHandoffItsPhasesRulesRefuse(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	github.pushed("LEGION-208", headSHA, claim.RoleImplementer, phase.Retro)
	for _, tc := range []struct {
		name, file string
		problems   []string
	}{
		{"a proof left out, stamped for another tree", `{"schemaVersion":1,"phase":"implement","issue":"OTHER-2","filesChanged":["x.go"]}`,
			[]string{`issue: Invalid input: expected "LEGION-208", received "OTHER-2"`, `proof: Invalid input: expected array, received undefined`}},
		{"a file that is not JSON", "implemented it\n", []string{"the file is not a JSON object"}},
		{"a file that is a JSON list", `["done"]`, []string{"the file is not a JSON object"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github.put(headSHA, ".legion/LEGION-208/implement.json", tc.file)
			message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented"}), http.StatusUnprocessableEntity, "HANDOFF_INVALID")
			if !strings.HasPrefix(message, "invalid implement handoff at .legion/LEGION-208/implement.json on head c0de00000000 of legion/LEGION-208: ") {
				t.Fatalf("refusal = %q, want it to name the file and the head", message)
			}
			for _, problem := range tc.problems {
				if !strings.Contains(message, problem) {
					t.Fatalf("refusal = %q, want it to list %q", message, problem)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("handoff facts = %#v, want none", got)
			}
		})
	}
	github.put(headSHA, ".legion/LEGION-208/implement.json", validHandoff("implement", "LEGION-208"))
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented"}))
}

func TestHandoffCompleteRefusesAnUnrecordedIssue(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented"}), http.StatusNotFound, "ISSUE_NOT_FOUND")
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
	github.pushed("LEGION-208", "bbccdd", claim.RoleImplementer, phase.Implementing)
	expired := implementer.grant(t)
	now = now.Add(time.Minute)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: expired, Summary: "implemented"}), http.StatusForbidden, "GRANT_EXPIRED")

	now = now.Add(-time.Minute)
	grant := implementer.grant(t)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "implemented"}))
	// The same command's grant still authenticates a second call; the phase's dedupe answers it.
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "implemented"}), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")

	implementer.replaceRegistration(t)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: grant, Summary: "implemented again"}), http.StatusForbidden, "GRANT_REVOKED")
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want only the completion made before the claim was replaced", got)
	}
}

// A merger whose completion was refused READY_REQUIRED corrects it with ready: true at the same
// head. The refusal is recorded as processed, so the corrected call must be a different fact: it
// reaches the workflow, and only a true retry of it is answered "already received". Otherwise the
// issue stays in merging with no way out but a new commit nothing asks for.
func TestHandoffCompleteAppliesTheMergersCorrectedReadyAtTheSameHead(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, &intake.Refusal{Status: http.StatusConflict, Code: "READY_REQUIRED", Message: "call the legion tool's handoff_complete with ready: true"})
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	github.pushed("LEGION-208", headSHA, claim.RoleMerger, phase.Merging)
	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "merge-ready"}), http.StatusConflict, "READY_REQUIRED")

	facts.mu.Lock()
	facts.refusal = nil
	facts.mu.Unlock()
	corrected := HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "merge-ready", Ready: true}
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
// shortened packet at the same head, the call the refusal asks for, is applied. The limit counts
// UTF-16 units, as Dispatch does, so a character outside the Basic Multilingual Plane counts twice.
func TestHandoffCompleteRefusesAREADYPacketTheDaemonCannotPostAndAppliesTheShortenedRetry(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	github.pushed("LEGION-208", headSHA, claim.RoleMerger, phase.Merging)
	const emoji = "\U0001F600"
	over := emoji + strings.Repeat("x", record.MessagePostLimit-1)
	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: over, Ready: true})
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
	completion(t, h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: shortened, Ready: true}))
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
	github.pushed("LEGION-209", "facade", claim.RolePlanner, phase.Planning)
	request := HandoffCompleteRequest{GrantID: planner.grant(t), Summary: "planned"}
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
	github.pushed("LEGION-208", headSHA, claim.RoleTester, phase.Testing)
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

	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}); recorder.Code != http.StatusOK {
		t.Fatalf("completion from a notice-started turn = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
}

// A refusal is recorded as processed, under the key the route builds. That key has to name the
// run the completion is attributed to: a completion of the run that is over is refused, and
// without the run in the key the new run's completion of the same phase at the same head takes
// that key, is answered ALREADY_RECORDED, never reaches the workflow, and leaves the phase
// stalled behind a worker that was told its report was received.
func TestAStaleCompletionDoesNotTakeTheNewRunsKey(t *testing.T) {
	h := newHarness(t)
	github := newHandoffGitHub(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, ReviewRoundCap: 3, MaxFixAttempts: 3}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	facts := &factRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine, facts}, Dispatch: &statusRecorder{},
		Tokens: handoffTokens{}, GitHubOwner: "acme", GitHubAPI: github.url, Repository: acmeWidgets,
	}).Handler
	// The tester takes run 1's task, and the child is then re-entered at run 2 while it works.
	h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 1, Status: "in_progress"})
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	github.pushed("LEGION-208", headSHA, claim.RoleTester, phase.Testing)
	h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 2, Status: "in_progress"})
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	complete := func() int {
		t.Helper()
		return h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}).Code
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

// The wait that follows a task's turn can be hours long — a tester on CI, a merger on a person —
// and a daemon restarted inside it rebuilds the machine from the store. The run the claim serves
// has to be there, or the completion the worker reports when a notice finally wakes it is refused
// by a daemon that has forgotten which run it was working.
func TestTheRunAClaimServesSurvivesARestart(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	github.pushed("LEGION-208", headSHA, claim.RoleTester, phase.Testing)
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
	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}); recorder.Code != http.StatusOK {
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
	github.pushed("LEGION-208", headSHA, claim.RoleTester, phase.Testing)
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

	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}); recorder.Code != http.StatusOK {
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
			h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
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

			assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}), http.StatusConflict, "HANDOFF_NO_RUN")
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
	github.pushed("LEGION-208", headSHA, claim.RoleTester, phase.Testing)
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

	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}); recorder.Code != http.StatusOK {
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
	h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 1, Status: "in_progress"})
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	github.pushed("LEGION-208", headSHA, claim.RoleTester, phase.Testing)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the first run's turn: %v", err)
	}
	h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
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

	if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}); recorder.Code != http.StatusOK {
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

	assertFailure(t, h.complete(HandoffCompleteRequest{GrantID: untasked.grant(t), Summary: "tests pass", Verdict: "pass"}), http.StatusConflict, "HANDOFF_NO_RUN")
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
			h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
				Phase: phase.Testing, Generation: 2, Status: "in_progress"})
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			github.pushed("LEGION-208", headSHA, claim.RoleTester, phase.Testing)
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
				if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: tester.token}); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				if err := machine.Handle(context.Background(), supervise.RequestResume{Claim: tester.token}); err != nil {
					t.Fatalf("resume: %v", err)
				}
				// The relaunched pane registers its session again, which is what revokes the old
				// grants; the run the claim is serving is not a property of the process.
				tester.replaceRegistration(t)
			}

			if recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass"}); recorder.Code != http.StatusOK {
				t.Fatalf("handoff = %d; body %s", recorder.Code, recorder.Body)
			}
			if completion := lastCompletion(t, facts); completion.Generation != delivered {
				t.Fatalf("the completion names generation %d, want the delivery's %d", completion.Generation, delivered)
			}
		})
	}
}

// newReadyHarness is the merger of LEGION-208 at merging, with its pull request acme/widgets#42
// recorded at headSHA and the branch's head there, retro's last push: what a READY is read against.
func newReadyHarness(t *testing.T, github *handoffGitHub) (*harness, *factRecorder, liveClaim) {
	t.Helper()
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	seedPullRequest(t, h, "LEGION-208", headSHA)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	github.pushed("LEGION-208", headSHA, claim.RoleMerger, phase.Merging)
	return h, facts, merger
}

// ready posts the merger's READY and returns its answer: the note when it was published, or the
// refusal's code and sentence.
func ready(t *testing.T, h *harness, merger liveClaim) (code int, body string) {
	t.Helper()
	recorder := h.complete(HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "gate facts hold", Ready: true})
	if recorder.Code == http.StatusOK {
		var response HandoffCompleteResponse
		decodeInto(t, recorder, &response)
		return recorder.Code, response.Note
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	return recorder.Code, failure.Code + ": " + failure.Error
}

// A merger's READY names a head a human merges, which GitHub merges only once every check the base
// branch requires has succeeded there. A head whose push skipped CI when it should not have reports
// none of them, so the completion refuses READY naming the head and the check, and nothing reaches
// the workflow; a required check still running, or one that failed, is refused the same way.
//
// A pull request that conflicts with its base (mergeable_state "dirty") gets no pull_request run,
// so a required check with no result on its head is refused naming the conflict rather than a
// skipped push. The conflict changes only that text, never which heads are refused: every row is
// published or refused by its checks alone, whatever its mergeable_state, and a conflicting head
// whose required checks all succeeded is published.
func TestHandoffCompleteReadyRefusesAHeadWithoutItsRequiredChecksGreen(t *testing.T) {
	const (
		ciGreen      = `{"id":1,"name":"ci","status":"completed","conclusion":"success"}`
		ciSkipped    = `{"id":1,"name":"ci","status":"completed","conclusion":"skipped"}`
		ciRunning    = `{"id":1,"name":"ci","status":"in_progress","conclusion":null}`
		legacyGreen  = `{"context":"legacy","state":"success"}`
		legacyFailed = `{"context":"legacy","state":"failure"}`
		skippedPush  = `: its push may have skipped CI`
		conflict     = `: the pull request conflicts with main, and GitHub starts no pull_request CI`
	)
	for _, tc := range []struct {
		name, checkRun, status, mergeableState, refusal string
	}{
		{"every required check green", ciGreen, legacyGreen, "clean", ""},
		{"a required check that ended skipped counts", ciSkipped, legacyGreen, "clean", ""},
		{"every required check green on a conflicting pull request", ciGreen, legacyGreen, "dirty", ""},
		{"a head whose push skipped CI", "", "", "blocked", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + skippedPush},
		{"a head whose mergeability GitHub has not computed", "", "", "unknown", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + skippedPush},
		{"a head of a conflicting pull request", "", "", "dirty", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + conflict},
		{"a conflicting pull request missing one required check", ciGreen, "", "dirty", `head c0de00000000 of pull request #42 has no result for the required check "legacy"` + conflict},
		{"a required check still running", ciRunning, legacyGreen, "blocked", `the required check "ci" is still running on head c0de00000000`},
		{"a required check still running on a conflicting pull request", ciRunning, legacyGreen, "dirty", `the required check "ci" is still running on head c0de00000000`},
		{"a required status that failed", ciGreen, legacyFailed, "blocked", `the required check "legacy" ended failure on head c0de00000000`},
		{"a required status that failed on a conflicting pull request", ciGreen, legacyFailed, "dirty", `the required check "legacy" ended failure on head c0de00000000`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) {
				g.checkRuns, g.statuses, g.mergeableState = "["+tc.checkRun+"]", "["+tc.status+"]", tc.mergeableState
			})
			h, facts, merger := newReadyHarness(t, github)
			code, body := ready(t, h, merger)
			if tc.refusal == "" {
				if code != http.StatusOK || body != "" {
					t.Fatalf("READY = %d %q; want it published with no note", code, body)
				}
				if fact := lastCompletion(t, facts); !fact.Ready || fact.Commit != headSHA {
					t.Fatalf("completion = %+v, want READY at %s", fact, headSHA)
				}
				return
			}
			if code != http.StatusConflict || !strings.HasPrefix(body, "READY_CHECKS_NOT_GREEN: "+tc.refusal) {
				t.Fatalf("READY = %d %q; want READY_CHECKS_NOT_GREEN naming %q", code, body, tc.refusal)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("handoff facts = %#v, want none for a refused READY", got)
			}
		})
	}
}

// A READY refused for its checks records nothing, so the same READY once the checks are green is
// applied, not answered already received: the merger waits for CI and completes again.
func TestHandoffCompleteReadyRefusedForItsChecksIsAppliedOnceTheyAreGreen(t *testing.T) {
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) {
		g.checkRuns = `[{"id":1,"name":"ci","status":"in_progress","conclusion":null}]`
	})
	h, facts, merger := newReadyHarness(t, github)
	if code, body := ready(t, h, merger); code != http.StatusConflict || !strings.HasPrefix(body, "READY_CHECKS_NOT_GREEN: ") {
		t.Fatalf("READY while ci runs = %d %q", code, body)
	}
	github.set(func(g *handoffGitHub) {
		g.checkRuns = `[{"id":1,"name":"ci","status":"completed","conclusion":"success"}]`
	})
	if code, body := ready(t, h, merger); code != http.StatusOK || body != "" {
		t.Fatalf("READY once ci passed = %d %q; want it published", code, body)
	}
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want the one READY", got)
	}
}

// A merger's READY names the head a human merges, and a squash merge commits that head merged into
// the default branch: the issue's own handoffs, .legion/<issue>/, must be gone from it (retro's last
// commit removes them; dispatch://LEGION-605). READY reads the head's tree on GitHub and is refused,
// naming the head and the directory, while it still holds .legion/<issue>/, whether or not the base
// branch requires any check, and published once GitHub answers that the head has none. A read
// GitHub fails leaves the head unknown, and READY is refused as GitHub's failure to retry, never
// sending the issue back to retro. A pull request a person already merged is published unread, its
// answer saying so: its head can no longer change, and the workflow takes it on to the production
// check only on that READY.
func TestHandoffCompleteReadyRefusesAHeadThatStillCarriesTheIssuesHandoffs(t *testing.T) {
	const (
		requiresCI    = `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`
		stillCarries  = `READY_HEAD_CARRIES_HANDOFFS: head c0de00000000 of pull request #42 still carries .legion/LEGION-208/`
		unprotected   = `{"name":"main","protected":false}`
		readRefused   = `GITHUB_READ_FAILED: GitHub's read of .legion/LEGION-208/ at head c0de00000000 of pull request #42 failed, so whether the head still carries it is unknown: complete again`
		alreadyMerged = `pull request #42 is already merged, so READY was published without reading its head's checks or handoffs`
	)
	for _, tc := range []struct {
		name, rules string
		merged      bool
		carries     bool
		readFails   bool
		want        string
	}{
		{"a head that still carries them", requiresCI, false, true, false, stillCarries},
		{"a head that still carries them, on a base requiring no check", `[]`, false, true, false, stillCarries},
		{"a head retro removed them from", requiresCI, false, false, false, ""},
		{"a head whose tree GitHub fails to read", requiresCI, false, false, true, readRefused},
		{"a pull request a person merged while its head still carried them", requiresCI, true, true, false, alreadyMerged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) {
				g.rules, g.mainBranch, g.merged, g.statuses = tc.rules, unprotected, tc.merged, "[]"
				if tc.readFails {
					g.fail["contents/"] = http.StatusInternalServerError
				}
			})
			h, facts, merger := newReadyHarness(t, github)
			if tc.carries {
				github.put(headSHA, ".legion/LEGION-208/implement.json", validHandoff("implement", "LEGION-208"))
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

// A ruleset can also require a workflow to succeed (rule type workflows), which GitHub judges by
// that workflow's run for the pull request's head. The rule shapes are a live repository's: a
// workflows rule for its review workflow, a required_status_checks rule for one aggregator check,
// a pull_request rule requiring no approval, and a branch protection requiring nothing. With the
// required check green, READY waits on the review workflow's latest pull_request run on the head:
// refused while it failed, still runs, or never ran, and published once it succeeded. A workflow
// another repository defines (an organization ruleset can require one) never matches a run here,
// whatever the head ran, so READY is refused naming that repository rather than a skipped push.
func TestHandoffCompleteReadyJudgesARequiredWorkflowByItsRunOnTheHead(t *testing.T) {
	const (
		review = `{"id":%d,"name":"Review PR #42","path":".github/workflows/claude-pr-review.yml","event":"pull_request","status":"%s","conclusion":%s,"repository":{"id":%d}}`
		rules  = `[{"type":"workflows","parameters":{"do_not_enforce_on_create":true,"workflows":[{"repository_id":4242,"path":".github/workflows/claude-pr-review.yml","ref":"refs/heads/main"}]}},` +
			`{"type":"deletion","parameters":null},` +
			`{"type":"pull_request","parameters":{"required_approving_review_count":0,"dismiss_stale_reviews_on_push":true,"require_code_owner_review":false}},` +
			`{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"pr-checks-result","integration_id":15368}]}}]`
	)
	reviewRun := func(id int, status, conclusion string) string {
		return fmt.Sprintf(review, id, status, conclusion, 4242)
	}
	for _, tc := range []struct {
		name string
		runs []string
		// base is the id of the pull request's base repository.
		base    int64
		refusal string
	}{
		{"while the required workflow failed", []string{reviewRun(7, "completed", `"failure"`)}, 4242, `the required workflow ".github/workflows/claude-pr-review.yml" ended failure on head c0de00000000`},
		{"while the required workflow still runs", []string{reviewRun(7, "in_progress", "null")}, 4242, `the required workflow ".github/workflows/claude-pr-review.yml" is still running on head c0de00000000`},
		{"while the head has no run of the required workflow", nil, 4242, `head c0de00000000 of pull request #42 has no run of the required workflow ".github/workflows/claude-pr-review.yml": its push may have skipped CI`},
		{"while another repository defines the required workflow", []string{fmt.Sprintf(review, 7, "completed", `"success"`, 5151)}, 5151,
			`the required workflow ".github/workflows/claude-pr-review.yml" is defined in repository 4242, not in pull request #42's own (5151)`},
		{"once the required workflow succeeded", []string{reviewRun(7, "completed", `"success"`)}, 4242, ""},
		{"once a later run of the required workflow succeeded", []string{reviewRun(7, "completed", `"failure"`), reviewRun(9, "completed", `"success"`)}, 4242, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) {
				g.rules, g.mainBranch, g.baseRepo, g.mergeableState = rules, `{"name":"main","protected":false}`, tc.base, "blocked"
				g.checkRuns, g.statuses = `[{"id":1,"name":"pr-checks-result","status":"completed","conclusion":"success"}]`, "[]"
				g.workflowRuns = "[" + strings.Join(tc.runs, ",") + "]"
			})
			h, _, merger := newReadyHarness(t, github)
			code, body := ready(t, h, merger)
			if tc.refusal == "" {
				if code != http.StatusOK || body != "" {
					t.Fatalf("READY = %d %q; want it published", code, body)
				}
				return
			}
			if code != http.StatusConflict || !strings.HasPrefix(body, "READY_CHECKS_NOT_GREEN: "+tc.refusal) {
				t.Fatalf("READY = %d %q; want READY_CHECKS_NOT_GREEN naming %q", code, body, tc.refusal)
			}
		})
	}
}

// A private repository whose plan has no rulesets answers the rulesets read 403, "make this
// repository public to enable this feature" (docs/solutions/legion/controller-gate-2-required-checks-live-reads.md).
// It can define no ruleset, so none requires a check there, and READY rests on the branch's
// protection alone: published when that requires nothing, its answer saying it read no checks,
// refused when it requires a check the head lacks. Only that answer means no rulesets: a rulesets
// read that fails otherwise (another 403, such as a token that lost access, or a server error)
// leaves the required checks unknown, and READY is refused naming the read.
func TestHandoffCompleteReadyOnARepositoryWhosePlanHasNoRulesets(t *testing.T) {
	const planAnswer = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","status":"403"}`
	unprotected := `{"name":"main","protected":false}`
	for _, tc := range []struct {
		name, branch string
		rulesStatus  int
		rulesBody    string
		code         int
		want         string
	}{
		{"and no branch protection", unprotected, http.StatusForbidden, planAnswer, http.StatusOK, `no check is required on "main" of acme/widgets, so READY was published without reading the head's checks`},
		{"and branch protection requiring a check the head lacks", `{"name":"main","protected":true,"protection":{"required_status_checks":{"contexts":["legacy"]}}}`, http.StatusForbidden, planAnswer, http.StatusConflict, `READY_CHECKS_NOT_GREEN: head c0de00000000 of pull request #42 has no result for the required check "legacy"`},
		{"but the read is refused for another reason", unprotected, http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`, http.StatusBadGateway, "GITHUB_READ_FAILED: GitHub answered GET /rules/branches/main with 403"},
		{"but the read fails", unprotected, http.StatusInternalServerError, `{"message":"Server Error"}`, http.StatusBadGateway, "GITHUB_READ_FAILED: GitHub answered GET /rules/branches/main with 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) {
				g.rules, g.rulesStatus, g.mainBranch, g.checkRuns, g.statuses = tc.rulesBody, tc.rulesStatus, tc.branch, "[]", "[]"
			})
			h, facts, merger := newReadyHarness(t, github)
			code, body := ready(t, h, merger)
			if code != tc.code || !strings.HasPrefix(body, tc.want) {
				t.Fatalf("READY = %d %q; want %d %q", code, body, tc.code, tc.want)
			}
			if recorded := len(facts.recorded()); (recorded == 1) != (code == http.StatusOK) {
				t.Fatalf("READY = %d with %d facts recorded; want one exactly when it was published", code, recorded)
			}
		})
	}
}

// A pull_request concurrency group cancels an earlier, still-queued run once a newer push's run
// for the same head's workflow starts (a `pull_request` `edited`/`synchronize` re-trigger after a
// concurrency-group cancellation, LEGION's own pr-title.yaml among them): GitHub's check-runs API
// documents no ordering for the list, unlike the combined-status endpoint, so the cancelled run
// can list after the later run that actually succeeded. READY must still see the later run's
// success rather than the cancelled run a naive "last one wins" read would keep.
func TestHandoffCompleteReadyKeepsTheNewestCheckRunWhenGitHubListsAnOlderCancelledDuplicateLast(t *testing.T) {
	github := newHandoffGitHub(t)
	// id 5 (the later run, success) listed before id 2 (the earlier run a concurrency-group
	// cancellation superseded) - the one ordering a "keep whichever is last" read gets wrong.
	github.set(func(g *handoffGitHub) {
		g.checkRuns = `[{"id":5,"name":"ci","status":"completed","conclusion":"success"},` +
			`{"id":2,"name":"ci","status":"completed","conclusion":"cancelled"}]`
	})
	h, facts, merger := newReadyHarness(t, github)
	if code, body := ready(t, h, merger); code != http.StatusOK || body != "" {
		t.Fatalf("READY = %d %q; want it published since the newest \"ci\" run (id 5) succeeded", code, body)
	}
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want the one READY", got)
	}
}

// READY is read against the issue's recorded pull request: a merger whose issue has none recorded
// is refused before GitHub is asked about any pull request.
func TestHandoffCompleteReadyNeedsTheIssuesRecordedPullRequest(t *testing.T) {
	github := newHandoffGitHub(t)
	h, facts := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	github.pushed("LEGION-208", headSHA, claim.RoleMerger, phase.Merging)
	if code, body := ready(t, h, merger); code != http.StatusConflict || body != "NO_PULL_REQUEST: LEGION-208 has no pull request recorded" {
		t.Fatalf("READY without a pull request = %d %q", code, body)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}
}
