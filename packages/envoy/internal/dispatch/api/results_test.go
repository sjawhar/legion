package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// twoChecks is a Verification document with one pre-merge check and one after-merge check.
const twoChecks = ":::check{#k1 surface=\"cli\" run=\"go test ./...\" negative_control=\"Break it and watch it fail.\"}\n" +
	"The server records a result against a check.\n" +
	":::\n\n" +
	":::check{#k2 when=\"after_merge\"}\n" +
	"Production answers at the merged commit.\n" +
	":::\n"

// siblingChecks is two pre-merge checks, so a rewording of one can be seen to leave the other's
// results alone; siblingChecksReworded changes k1's words and leaves k3's.
const siblingChecks = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\n" +
	"The server records a result against a check.\n" +
	":::\n\n" +
	":::check{#k3 surface=\"cli\" run=\"go test ./...\"}\n" +
	"A sibling check nobody reworded.\n" +
	":::\n"

const siblingChecksReworded = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\n" +
	"The server records a result against a check, named by its block id.\n" +
	":::\n\n" +
	":::check{#k3 surface=\"cli\" run=\"go test ./...\"}\n" +
	"A sibling check nobody reworded.\n" +
	":::\n"

const (
	headCommit    = "1111111111111111111111111111111111111111"
	newHeadCommit = "2222222222222222222222222222222222222222"
	mergeCommit   = "3333333333333333333333333333333333333333"
)

type recordedResult struct {
	Result model.CheckResult `json:"result"`
	Line   string            `json:"line"`
}

type issueResults struct {
	Issue                  string             `json:"issue"`
	VerificationArtifactID string             `json:"verification_artifact_id"`
	Checks                 []model.CheckBlock `json:"checks"`
}

// resultBody is a well-formed record body with the given check and verdict.
func resultBody(check, verdict, commit string, fields map[string]any) map[string]any {
	body := map[string]any{"check": check, "role": "implementer", "verdict": verdict, "commit": commit}
	for name, value := range fields {
		body[name] = value
	}
	return body
}

func recordResult(t *testing.T, handler http.Handler, issue string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/results", body, "alice")
}

func readResults(t *testing.T, handler http.Handler, issue string) issueResults {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	assertVerificationShadow(t, handler)
	return decodeBody[issueResults](t, response)
}

// checkOf is the check of the read with that block id.
func checkOf(t *testing.T, read issueResults, blockID string) model.CheckBlock {
	t.Helper()
	for _, check := range read.Checks {
		if check.BlockID == blockID {
			return check
		}
	}
	t.Fatalf("the read holds no check %s: %+v", blockID, read.Checks)
	return model.CheckBlock{}
}

// writeHead stores one pull request head row directly, as the intake's write of a
// `pull_request` envelope would leave it. An empty issueKey is a pull request the attribution
// named no issue for.
func writeHead(t *testing.T, database *store.Store, repo string, number int, sha, state, issueKey string) {
	t.Helper()
	var attributed *string
	if issueKey != "" {
		attributed = &issueKey
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, event_at, checked_at)
		values ($1, $2, $3, 'legion/work', 'main', $4, $5, now(), now())
		on conflict (repo, number) do update set head_sha = excluded.head_sha, state = excluded.state, issue_key = excluded.issue_key
	`, repo, number, sha, state, attributed); err != nil {
		t.Fatalf("store the head of %s#%d: %v", repo, number, err)
	}
}

// A result is recorded against a check of the issue's own Verification document and against a
// check of an ancestor's; the event goes on the check's issue either way, and the answer carries
// the pull-request body line. An id neither holds is refused, and one two of them share is
// refused until the caller names which document it means.
func TestResultRecordsAgainstAChecksOfTheIssueAndOfAnAncestor(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	parent := createInteractionIssue(t, handler, "RES", "Parent", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", twoChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RES", "title": "Child", "parent": parent.Key, "force": true}, "alice"))

	own := recordResult(t, handler, parent.Key, resultBody("k1", "pass", headCommit, map[string]any{
		"surface": "cli", "run": "go test ./...", "observed": "ok", "negative_control": "broke it",
	}))
	if own.Code != http.StatusCreated {
		t.Fatalf("record a result on the check's own issue: status=%d body=%s", own.Code, own.Body.String())
	}
	recorded := decodeBody[recordedResult](t, own)
	if recorded.Result.CheckIssueKey != parent.Key || recorded.Result.IssueKey != parent.Key {
		t.Fatalf("result = recorded on %s against %s, want both %s", recorded.Result.IssueKey, recorded.Result.CheckIssueKey, parent.Key)
	}
	wantLine := "- Check k1 (before_merge): pass at 111111111111, recorded by implementer. The server records a result against a check."
	if recorded.Line != wantLine {
		t.Fatalf("line = %q, want %q", recorded.Line, wantLine)
	}
	if strings.Contains(recorded.Line, "dispatch://") || strings.Contains(recorded.Line, testServerURL) {
		t.Fatalf("the pull-request line carries a Dispatch reference: %q", recorded.Line)
	}

	// The child has no Verification document of its own, so its result names its parent's check
	// and is recorded against the parent, whose event log carries it.
	fromChild := recordResult(t, handler, child.Key, resultBody("k1", "fail", newHeadCommit, map[string]any{"failed": 2}))
	if fromChild.Code != http.StatusCreated {
		t.Fatalf("record a result against an ancestor's check: status=%d body=%s", fromChild.Code, fromChild.Body.String())
	}
	childResult := decodeBody[recordedResult](t, fromChild)
	if childResult.Result.IssueKey != child.Key || childResult.Result.CheckIssueKey != parent.Key {
		t.Fatalf("child's result = recorded on %s against %s, want %s against %s",
			childResult.Result.IssueKey, childResult.Result.CheckIssueKey, child.Key, parent.Key)
	}
	event := lastIssueEventPayload(t, database, parent.Key)
	if event["result_id"] != childResult.Result.ID || event["issue"] != child.Key || event["check_issue"] != parent.Key {
		t.Fatalf("result.recorded payload = %+v, want the child's result on the parent's check", event)
	}
	// Both results stand under the parent's check, newest first.
	read := readResults(t, handler, parent.Key)
	if read.VerificationArtifactID == "" || len(read.Checks) != 2 {
		t.Fatalf("the parent's results read = %+v, want its Verification document and two checks", read)
	}
	if results := checkOf(t, read, "k1").Results; len(results) != 2 || results[0].ID != childResult.Result.ID {
		t.Fatalf("k1's results = %+v, want the child's newest of two", results)
	}

	// An id no Verification document in reach holds.
	unknown := recordResult(t, handler, child.Key, resultBody("k9", "pass", headCommit, nil))
	if code, _ := refusalOf(t, unknown); unknown.Code != http.StatusBadRequest || code != "UNKNOWN_CHECK" {
		t.Fatalf("an unknown check: status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	// The child writes a Verification document of its own that repeats k1: the bare id now names
	// two checks, and only the canonical reference decides between them.
	childVerification := uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "verification.md", twoChecks)
	ambiguous := recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil))
	if code, _ := refusalOf(t, ambiguous); ambiguous.Code != http.StatusConflict || code != "CHECK_AMBIGUOUS" {
		t.Fatalf("an ambiguous check: status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}
	named := recordResult(t, handler, child.Key, resultBody(
		"dispatch://"+child.Key+"/artifact/"+childVerification.Artifact.Slug+"#b-k1", "pass", headCommit, nil))
	if named.Code != http.StatusCreated {
		t.Fatalf("record against the named document: status=%d body=%s", named.Code, named.Body.String())
	}
	if against := decodeBody[recordedResult](t, named).Result.CheckIssueKey; against != child.Key {
		t.Fatalf("the named result was recorded against %s, want the child's own document", against)
	}
}

// A pass that counts a skipped case contradicts itself.
func TestResultRefusesAPassThatCountsASkippedCase(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESC", "Contradiction", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	refused := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, map[string]any{"skipped": 1}))
	code, message := refusalOf(t, refused)
	if refused.Code != http.StatusBadRequest || code != "RESULT_CONTRADICTS" || !strings.Contains(message, "1 skipped") {
		t.Fatalf("a pass counting a skipped case: status=%d body=%s", refused.Code, refused.Body.String())
	}
	// The same counts with a failing verdict are no contradiction.
	if accepted := recordResult(t, handler, issue.Key, resultBody("k1", "fail", headCommit, map[string]any{"skipped": 1})); accepted.Code != http.StatusCreated {
		t.Fatalf("a fail counting a skipped case: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}

// A closed issue takes a result for an after-merge check — the production check runs only once
// the merge has closed the issue — and refuses a pre-merge one, while every other write on it
// stays refused.
func TestClosedIssueTakesAnAfterMergeResultAndRefusesEveryOtherWrite(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESX", "Closed", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", twoChecks)
	closed := dispatchRequest(t, handler, http.MethodPatch, owner,
		map[string]any{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closed.Code, closed.Body.String())
	}

	after := recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil))
	if after.Code != http.StatusCreated {
		t.Fatalf("record an after-merge result on a closed issue: status=%d body=%s", after.Code, after.Body.String())
	}
	before := recordResult(t, handler, issue.Key, resultBody("k1", "pass", mergeCommit, nil))
	if code, _ := refusalOf(t, before); before.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
		t.Fatalf("a pre-merge result on a closed issue: status=%d body=%s", before.Code, before.Body.String())
	}
	// The exemption lives in this route alone.
	write := dispatchRequest(t, handler, http.MethodPost, owner+"/artifacts",
		map[string]string{"name": "notes.md", "content": "# Notes\n"}, "alice")
	if code, _ := refusalOf(t, write); write.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
		t.Fatalf("a document write on a closed issue: status=%d body=%s", write.Code, write.Body.String())
	}
	comment := dispatchRequest(t, handler, http.MethodPost, owner+"/comments", map[string]any{"body": "A note."}, "alice")
	if code, _ := refusalOf(t, comment); comment.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
		t.Fatalf("a comment on a closed issue: status=%d body=%s", comment.Code, comment.Body.String())
	}
}

// A pre-merge result is current at the head of an open pull request of its check's issue. A push
// that moves that head makes it older with no write of Dispatch's, and a result at the new head
// is current. Rewording one check makes only its own results older, and restoring the words makes
// them current again.
//
// The issue resolves to a repository the App can read, which is what makes "this commit heads no
// pull request I know" the older answer rather than the unknown one.
func TestBeforeMergeResultCurrencyFollowsTheHeadAndTheChecksWords(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	issue := issueInRepository(t, handler, "RESH", "acme/widgets", "Heads")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", siblingChecks)
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)

	first := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", first.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result at the open head = %q, want current", got)
	}

	// A push moves the head: the same result, with no Dispatch write of its own, is older, and a
	// result at the new head is current.
	writeHead(t, database, "acme/widgets", 7, newHeadCommit, "open", issue.Key)
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", first.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("a result at the old head = %q, want older", got)
	}
	moved := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", newHeadCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", moved.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result at the new head = %q, want current", got)
	}

	// A pull request attributed to another issue never answers for this one.
	writeHead(t, database, "acme/widgets", 8, mergeCommit, "open", "")
	stranger := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", mergeCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", stranger.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("a result at another issue's head = %q, want older", got)
	}

	// Rewording k1 makes its results older and leaves its sibling's alone; restoring the words
	// makes them current again. Both checks run before the merge, so the only thing that moves
	// between them is their own words.
	sibling := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k3", "pass", newHeadCommit, nil)))
	uploadDocument(t, handler, owner, "verification.md", siblingChecksReworded)
	read := readResults(t, handler, issue.Key)
	if got := currencyOf(t, read, "k1", moved.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("a result of a reworded check = %q, want older", got)
	}
	if got := currencyOf(t, read, "k3", sibling.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("the sibling check's result = %q, want current", got)
	}
	uploadDocument(t, handler, owner, "verification.md", siblingChecks)
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", moved.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result of a check whose words were restored = %q, want current", got)
	}
}

// currencyOf is the currency the read gives one result of one check.
func currencyOf(t *testing.T, read issueResults, blockID, resultID string) string {
	t.Helper()
	for _, result := range checkOf(t, read, blockID).Results {
		if result.ID == resultID {
			return result.Currency
		}
	}
	t.Fatalf("check %s holds no result %s", blockID, resultID)
	return ""
}

// issueInRepository seeds a project whose default repository is repo and one issue of it, so
// the check's issue resolves to that repository: the pair every currency answer turns on.
func issueInRepository(t *testing.T, handler http.Handler, project, repo, title string) model.Issue {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": project, "name": title}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo,
		map[string]any{"project": project, "default": true}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map %s to %s: status=%d body=%s", repo, project, response.Code, response.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": project, "title": title, "spec": "A spec\n"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue of %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// resultsGitHub is a Dispatch holding an App key whose GitHub answers `handler`, with the
// installation lookup and token mint every read needs already served.
func resultsGitHub(t *testing.T, repos map[string]bool, compare func(base, head string) string) (http.Handler, *store.Store) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !repos[r.PathValue("owner")+"/"+r.PathValue("repo")] {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) {
		base, head, _ := strings.Cut(r.PathValue("basehead"), "...")
		fmt.Fprintf(w, `{"status":%q}`, compare(base, head))
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, database, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})
	return handler, database
}

// An after-merge result is current at a commit on the repository's default branch, older at a
// commit of an unmerged branch, and unknown where Dispatch's App cannot read the repository.
func TestAfterMergeResultCurrencyFollowsTheDefaultBranch(t *testing.T) {
	calls := 0
	handler, _ := resultsGitHub(t, map[string]bool{"acme/widgets": true}, func(base, head string) string {
		calls++
		if base != "main" {
			return "diverged"
		}
		if head == mergeCommit {
			return "behind"
		}
		return "diverged"
	})
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "RESD", "name": "Default"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": "RESD", "default": true}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESD", "title": "Default branch", "spec": "A spec\n"}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	merged := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))
	unmerged := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", newHeadCommit, nil)))
	read := readResults(t, handler, issue.Key)
	if got := currencyOf(t, read, "k2", merged.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result at a default-branch commit = %q, want current", got)
	}
	if got := currencyOf(t, read, "k2", unmerged.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("a result at an unmerged branch's commit = %q, want older", got)
	}
	// Neither commit is asked about again: a yes is kept for good, and a no stands for five
	// minutes, so a second read inside them makes no GitHub call at all.
	before := calls
	readResults(t, handler, issue.Key)
	if calls != before {
		t.Fatalf("a second read made %d compare calls, want none (both answers are stored)", calls-before)
	}
}

// Where the App cannot read the check's issue's repository, the result's commit is shown with
// neither mark.
func TestAfterMergeResultCurrencyIsUnknownWhenTheAppCannotReadTheRepository(t *testing.T) {
	handler, _ := resultsGitHub(t, map[string]bool{}, func(string, string) string { return "behind" })
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "RESU", "name": "Unknown"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/hidden",
		map[string]any{"project": "RESU", "default": true}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESU", "title": "Unreadable", "spec": "A spec\n"}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k2", "pass", mergeCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k2", recorded.Result.ID); got != model.CurrencyUnknown {
		t.Fatalf("a result of a repository the App cannot read = %q, want unknown", got)
	}

	// An issue that resolves to no repository at all is unknown too.
	// createInteractionIssue creates the project itself.
	loose := createInteractionIssue(t, handler, "RESN", "No repository", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+loose.Key, "verification.md", twoChecks)
	none := decodeBody[recordedResult](t, recordResult(t, handler, loose.Key, resultBody("k2", "pass", mergeCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, loose.Key), "k2", none.Result.ID); got != model.CurrencyUnknown {
		t.Fatalf("a result of an issue with no repository = %q, want unknown", got)
	}
}

// A `before_merge` result of a repository Dispatch cannot see is marked neither current nor
// older: nothing can teach Dispatch that repository's pull request heads, so "this commit heads
// none I know" says nothing about the work, and the reviewer reads an unmarked pass rather than
// a forced refusal. Where Dispatch can read the repository the same answer is older, which is
// what makes the mark worth reading.
func TestBeforeMergeResultCurrencyIsUnknownWhenDispatchCannotSeeTheRepository(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })

	hidden := issueInRepository(t, handler, "RESB", "acme/hidden", "Unreadable")
	uploadDocument(t, handler, "/api/v1/issues/"+hidden.Key, "verification.md", twoChecks)
	unreadable := decodeBody[recordedResult](t, recordResult(t, handler, hidden.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, hidden.Key), "k1", unreadable.Result.ID); got != model.CurrencyUnknown {
		t.Fatalf("a pre-merge result of a repository the App cannot read = %q, want unknown", got)
	}

	// An issue that resolves to no repository at all is unknown too: createInteractionIssue
	// creates the project itself, and nothing maps a repository to it.
	loose := createInteractionIssue(t, handler, "RESL", "No repository", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+loose.Key, "verification.md", twoChecks)
	none := decodeBody[recordedResult](t, recordResult(t, handler, loose.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, loose.Key), "k1", none.Result.ID); got != model.CurrencyUnknown {
		t.Fatalf("a pre-merge result of an issue with no repository = %q, want unknown", got)
	}

	// The contrast, in a repository the App reads: a commit heading no pull request Dispatch
	// knows is older, and one heading an open pull request of the issue is current.
	seen := issueInRepository(t, handler, "RESV", "acme/widgets", "Readable")
	uploadDocument(t, handler, "/api/v1/issues/"+seen.Key, "verification.md", twoChecks)
	adrift := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, seen.Key), "k1", adrift.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("a pre-merge result heading no pull request of a readable repository = %q, want older", got)
	}
	writeHead(t, database, "acme/widgets", 31, newHeadCommit, "open", seen.Key)
	atHead := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", newHeadCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, seen.Key), "k1", atHead.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a pre-merge result at the open head = %q, want current", got)
	}
}

// Recording a result at a commit that is the head of no pull request Dispatch has an event for
// reads GitHub through the App and stores every pull request it names, so the result is current
// without a webhook ever arriving.
func TestRecordingAResultReadsThroughToAPullRequestDispatchHasNoEventFor(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	var issueKey string
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"number":12,"title":%q,"body":"","html_url":"https://github.com/acme/widgets/pull/12",
			"state":"open","merged":false,"updated_at":"2026-01-01T00:00:00Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}}]`, issueKey+" work", headCommit)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, database, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "REST", "name": "Through"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": "REST", "default": true}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "REST", "title": "Read through", "spec": "A spec\n"}, "alice"))
	issueKey = issue.Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	var storedIssue *string
	var state string
	if err := database.Pool.QueryRow(context.Background(), `
		select issue_key, state from pull_request_heads where repo = 'acme/widgets' and number = 12
	`).Scan(&storedIssue, &state); err != nil {
		t.Fatalf("read the pull request the read-through stored: %v", err)
	}
	if storedIssue == nil || *storedIssue != issue.Key || state != "open" {
		t.Fatalf("the read-through stored issue=%v state=%q, want %s open", storedIssue, state, issue.Key)
	}
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result at a head the read-through learned = %q, want current", got)
	}
}

// The answer to a record carries the moment the row was written, not the zero time: created_at
// is a declared field of the contract, and a reader takes it for when the run happened.
func TestRecordedResultAnswersTheTimeItWasStored(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESA", "Stored at", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	if recorded.Result.CreatedAt.IsZero() {
		t.Fatalf("the record's answer carries created_at %s, want the moment the row was written", recorded.Result.CreatedAt)
	}
	var stored time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select created_at from check_results where id = $1::uuid`, recorded.Result.ID).Scan(&stored); err != nil {
		t.Fatalf("read the stored result: %v", err)
	}
	if !recorded.Result.CreatedAt.Equal(stored) {
		t.Fatalf("the record's answer carries created_at %s, want the row's %s", recorded.Result.CreatedAt, stored)
	}
	if read := checkOf(t, readResults(t, handler, issue.Key), "k1").Results; len(read) != 1 || !read[0].CreatedAt.Equal(stored) {
		t.Fatalf("the results read carries %+v, want the one result at %s", read, stored)
	}
}

// R4: a child's pull request never makes a root check's result current. A child proving its
// parent's check records the result on the child, and the head of the child's own pull request
// answers for nothing on the parent's tab. What reads current there is the parent's own result
// at a head of the parent's own pull request.
func TestChildsResultAgainstAnAncestorsCheckIsOlderAtTheChildsHead(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	parent := issueInRepository(t, handler, "RESX", "acme/widgets", "Parent")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESX", "title": "Child", "parent": parent.Key, "force": true}, "alice"))
	// Two open pull requests in the one repository: the child's, and the parent's own.
	writeHead(t, database, "acme/widgets", 21, headCommit, "open", child.Key)
	writeHead(t, database, "acme/widgets", 22, newHeadCommit, "open", parent.Key)

	fromChild := decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil)))
	fromParent := decodeBody[recordedResult](t, recordResult(t, handler, parent.Key, resultBody("k1", "pass", newHeadCommit, nil)))

	read := readResults(t, handler, parent.Key)
	if got := currencyOf(t, read, "k1", fromChild.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("the child's result at the child's own head = %q, want older", got)
	}
	if got := currencyOf(t, read, "k1", fromParent.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("the parent's own result at the parent's own head = %q, want current", got)
	}
}

// The same rule where the child has a repository of its own. The read-through asks GitHub about
// the repository the check's issue resolves to and no other, so the child's own pull request is
// never learned, and the child's result reads older on the parent's tab although GitHub would
// name a pull request for that commit in the child's repository.
func TestChildsResultInItsOwnRepositoryIsOlderOnTheParentsTab(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	// Both repositories read, so a commit heading no pull request Dispatch knows is older there
	// rather than unknown.
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	var childKey string
	var askedParent, askedChild bool
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("repo") != "gadgets" {
			askedParent = true
			fmt.Fprint(w, `[]`)
			return
		}
		askedChild = true
		fmt.Fprintf(w, `[{"number":31,"title":%q,"body":"","html_url":"https://github.com/acme/gadgets/pull/31",
			"state":"open","merged":false,"updated_at":"2026-01-01T00:00:00Z",
			"head":{"ref":"legion/child","sha":%q},"base":{"ref":"main"}}]`, childKey+" work", headCommit)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, database, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "RESR", "name": "Two repos"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": "RESR", "default": true}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets",
		map[string]any{"project": "RESR"}, "alice")
	parent := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESR", "title": "Parent", "spec": "A spec\n"}, "alice"))
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESR", "title": "Child", "parent": parent.Key, "force": true}, "alice"))
	childKey = child.Key
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key,
		map[string]any{"repo": "acme/gadgets"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("give the child its own repository: status=%d body=%s", response.Code, response.Body.String())
	}

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil)))

	if !askedParent {
		t.Fatalf("the read-through never asked the repository the check's issue resolves to")
	}
	if askedChild {
		t.Fatalf("the read-through asked the recording issue's own repository, which R4 rules out")
	}
	var stored int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from pull_request_heads where repo = 'acme/gadgets'
	`).Scan(&stored); err != nil {
		t.Fatalf("count the pull requests stored for the child's repository: %v", err)
	}
	if stored != 0 {
		t.Fatalf("the read-through stored %d pull requests of the child's repository, want none", stored)
	}
	if got := currencyOf(t, readResults(t, handler, parent.Key), "k1", recorded.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("the child's result at its own repository's head = %q, want older", got)
	}
}
