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
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
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

// The same unknown, with a second repository in play: Dispatch reads one of the two, and the
// commit a result of the unreadable repository's issue names is the head of an open pull request
// of the readable one, attributed to the readable repository's own issue. That head says nothing
// about the unreadable repository — Dispatch sees none of its pull requests, so it cannot know
// what that commit is there — so the result stays unknown and never reads older, while the
// readable issue's own result at the same commit reads current.
func TestBeforeMergeResultCurrencyStaysUnknownAtAnotherRepositorysHead(t *testing.T) {
	handler, database := resultsGitHub(t, map[string]bool{"acme/widgets": true},
		func(string, string) string { return "diverged" })
	seen := issueInRepository(t, handler, "RESY", "acme/widgets", "Readable")
	hidden := issueInRepository(t, handler, "RESZ", "acme/hidden", "Unreadable")
	uploadDocument(t, handler, "/api/v1/issues/"+seen.Key, "verification.md", twoChecks)
	uploadDocument(t, handler, "/api/v1/issues/"+hidden.Key, "verification.md", twoChecks)

	// The one pull request Dispatch knows at that commit: open, in the repository it reads, and
	// attributed to that repository's own issue.
	writeHead(t, database, "acme/widgets", 41, headCommit, "open", seen.Key)

	elsewhere := decodeBody[recordedResult](t, recordResult(t, handler, hidden.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, hidden.Key), "k1", elsewhere.Result.ID); got != model.CurrencyUnknown {
		t.Fatalf("a pre-merge result of acme/hidden's issue at acme/widgets' head = %q, want unknown: Dispatch cannot see acme/hidden's pull requests at all", got)
	}
	atHead := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, seen.Key), "k1", atHead.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("acme/widgets' own result at that head = %q, want current", got)
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

// The same case at the surface a reviewer reads: a pull request of acme/gadgets whose title
// names acme/widgets' issue before its own. The attribution walks every candidate the five
// sources name (delivery's attributionCandidates) and takes the first one held to the pull
// request's own repository (attributionHeld), so the head the read-through learns is attributed
// to the check's own issue and the result at it reads current, although the title names another
// repository's issue first. Judged through the route, which is where a reviewer's approval
// condition is actually read.
func TestAResultAtTheHeadOfAPullRequestTitledAfterAnotherRepositorysIssueReadsCurrent(t *testing.T) {
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
	// The repository is readable, so a result at no attributed head is older rather than unknown.
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	var title string
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		repo := r.PathValue("owner") + "/" + r.PathValue("repo")
		fmt.Fprintf(w, `[{"number":33,"title":%q,"body":"","html_url":"https://github.com/%s/pull/33",
			"state":"open","merged":false,"updated_at":"2026-01-02T00:00:00Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`,
			title, repo, headCommit, repo)
	})
	// The title names a candidate held to acme/gadgets, so the read-through keeps it without
	// paging the commit messages, the fifth source; the handler answers an empty page for a read
	// that asks anyway.
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, _, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})

	elsewhere := issueInRepository(t, handler, "RESW", "acme/widgets", "Another repository's issue")
	own := issueInRepository(t, handler, "RESG", "acme/gadgets", "This repository's issue")
	title = elsewhere.Key + ": the other repository's issue, and " + own.Key + " (this one)"
	uploadDocument(t, handler, "/api/v1/issues/"+own.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, own.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, own.Key), "k1", recorded.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result at the head of %s's own pull request = %q, want current: its title names %s first, but %s is the issue of acme/gadgets",
			own.Key, got, elsewhere.Key, own.Key)
	}
}

// The same surface for a repository under two names. An issue is recorded under the name its
// repository was renamed away from, and its pull request is on the name GitHub knows. Dispatch
// holds both — repo_projects maps each, and the old name's row names what it was renamed to —
// so the results route asks the App about the canonical name (refs.CanonicalRepo) and the
// attribution holds the issue to it. Before that pair was read, the App was asked about the old
// name, whose installation GitHub answers 404 for: the read-through learned no pull request and
// Dispatch could see no repository at all, so the result read `unknown` rather than `older`.
func TestAResultOfAnIssueRecordedUnderARepositorysOldNameReadsCurrent(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	// GitHub knows acme/gadgets and nothing of the name it was renamed away from, which is why
	// Dispatch cannot resolve a rename by asking and is told it instead.
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner")+"/"+r.PathValue("repo") != "acme/gadgets" {
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
	var title string
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"number":44,"title":%q,"body":"","html_url":"https://github.com/acme/gadgets/pull/44",
			"state":"open","merged":false,"updated_at":"2026-03-01T00:00:00Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":"acme/gadgets"}},"base":{"ref":"main"}}]`, title, headCommit)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, _, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": "RENM", "name": "Renamed"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create RENM: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, mapping := range []struct {
		repo string
		body map[string]any
	}{
		{"acme/gadgets", map[string]any{"project": "RENM", "default": true}},
		{"oldco/gadgets", map[string]any{"project": "RENM", "renamed_to": "acme/gadgets"}},
	} {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+mapping.repo,
			mapping.body, "alice"); response.Code != http.StatusOK {
			t.Fatalf("map %s: status=%d body=%s", mapping.repo, response.Code, response.Body.String())
		}
	}
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RENM", "title": "Recorded under the old name", "spec": "A spec\n", "repo": "oldco/gadgets"}, "alice"))
	title = issue.Key + ": the work, on the new name"
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result of %s, recorded under oldco/gadgets, at the head of its pull request on acme/gadgets = %q, want current", issue.Key, got)
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

// commitPullsFake is the GitHub a read-time read-through reads (readThroughGitHub): it answers
// GET /repos/{owner}/{repo}/commits/{sha}/pulls from the body its test set last, so a test can
// open a pull request between two reads, and counts every such ask per repository, so a test can
// show what one read of an issue's results spent.
type commitPullsFake struct {
	mu     sync.Mutex
	asked  map[string]int
	answer func(repo, sha string) string
}

// answers sets what GET /repos/{owner}/{repo}/commits/{sha}/pulls answers from now on: the JSON
// array of pull requests GitHub names for that commit. Until a test sets one, GitHub names none.
func (f *commitPullsFake) answers(body func(repo, sha string) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = body
}

// asks is how often GitHub has been asked for repo's pull requests, at any commit.
func (f *commitPullsFake) asks(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[repo]
}

func (f *commitPullsFake) serveCommitPulls(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	f.mu.Lock()
	f.asked[repo]++
	answer := f.answer
	f.mu.Unlock()
	if answer == nil {
		fmt.Fprint(w, `[]`)
		return
	}
	fmt.Fprint(w, answer(repo, r.PathValue("sha")))
}

// openPullAt is GitHub's answer for a commit that is the head of one open pull request titled
// after issueKey, in whichever repository was asked about.
func openPullAt(number int, issueKey string) func(repo, sha string) string {
	return func(repo, sha string) string {
		return fmt.Sprintf(`[{"number":%d,"title":%q,"body":"","html_url":"https://github.com/%s/pull/%d",
			"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`,
			number, issueKey+" the work", repo, number, sha, repo)
	}
}

// readThroughGitHub is resultsGitHub's twin for the read-time read-through: a Dispatch holding
// an App key, the server its handler mounts — whose commitPulls cache a test moves — and the
// GitHub it reads. The App is installed on repos and on nothing else, every repository it can
// read answers `main` as its default branch (so a commit heading no pull request is `older`
// rather than `unknown`), each pull request's commit messages are an empty page, and one commit's
// pull requests are the fake's, which counts the asks.
func readThroughGitHub(t *testing.T, repos ...string) (http.Handler, *store.Store, *server, *commitPullsFake) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	installed := map[string]bool{}
	for _, repo := range repos {
		installed[repo] = true
	}
	fake := &commitPullsFake{asked: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !installed[r.PathValue("owner")+"/"+r.PathValue("repo")] {
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
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", fake.serveCommitPulls)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream.URL,
	})
	return handler, database, served, fake
}

// INFRA-1240 at the surface a reviewer reads. A `before_merge` result is recorded at a commit no
// pull request carries yet: GitHub names none, so the record's own read-through stores no head
// row and the result reads `older`. The pull request is opened at that head a moment later and
// nothing tells Dispatch — a deployment holding no delivery settings consumes no `pull_request`
// event at all, the reconcile's head pass only re-reads rows that already exist, and the record's
// read-through has run once and will not run again — so the read of the results asks GitHub
// itself, and the result reads `current`.
func TestAResultReadsCurrentOnceAPullRequestOpensAtItsCommit(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESO", "acme/widgets", "Opened after the record")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	var heads int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from pull_request_heads where repo = 'acme/widgets'`).Scan(&heads); err != nil {
		t.Fatalf("count the stored pull request heads: %v", err)
	}
	if heads != 0 {
		t.Fatalf("recording at a commit GitHub names no pull request for stored %d head rows, want none", heads)
	}
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("a result at a commit no pull request carries = %q, want older", got)
	}

	// The pull request opens at that head, and no event reaches Dispatch.
	fake.answers(openPullAt(91, issue.Key))
	// The read-through the first read spent holds the next one off for
	// delivery.CommitPullReadStaleAfter, so the cache is rewound to its zero value, an empty one:
	// what the clock does on its own between a reviewer's refetches, without the test waiting for
	// it.
	served.commitPulls = delivery.CommitPullReads{}

	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result at the head of a pull request opened after it was recorded = %q, want current: the read of the results reads GitHub itself", got)
	}
}

// The bound on that read. Two reads in a row, while GitHub still names no pull request for the
// commit, spend one read-through between them: the Verification tab refetches an issue's results
// on every result.recorded, and a tab left open would otherwise spend a GitHub call per result
// per refetch for ever, since nothing about such a commit is ever stored
// (delivery.CommitPullReadStaleAfter).
func TestTwoResultReadsSpendOneReadThroughOfACommit(t *testing.T) {
	handler, _, _, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESC", "acme/widgets", "Bounded")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	// The record's own read-through, which the read-time bound neither counts nor is bound by.
	spent := fake.asks("acme/widgets")

	for _, read := range []string{"first", "second"} {
		if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyOlder {
			t.Fatalf("the %s read of a result at a commit no pull request carries = %q, want older", read, got)
		}
	}
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Fatalf("two reads in a row asked GitHub for the commit's pull requests %d times, want 1", asked)
	}
}

// What the read never spends a GitHub call on. A repository Dispatch cannot see is the unknown
// answer whatever GitHub would say about the commit, so it is not read through at all; and a
// commit that already heads a pull request attributed to the check's own issue is answered from
// that row, so neither the record nor the read asks GitHub anything.
func TestAResultsReadReadsThroughNeitherAnUnseenRepositoryNorAKnownHead(t *testing.T) {
	handler, database, _, fake := readThroughGitHub(t, "acme/widgets")
	hidden := issueInRepository(t, handler, "RESJ", "acme/hidden", "Unreadable")
	seen := issueInRepository(t, handler, "RESK", "acme/widgets", "Readable")
	uploadDocument(t, handler, "/api/v1/issues/"+hidden.Key, "verification.md", twoChecks)
	uploadDocument(t, handler, "/api/v1/issues/"+seen.Key, "verification.md", twoChecks)
	// GitHub would name an open pull request of the issue for the commit in either repository, so
	// a read that asked would read `current` and the counts below are what decides the test.
	fake.answers(openPullAt(92, hidden.Key))

	unreadable := decodeBody[recordedResult](t, recordResult(t, handler, hidden.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, hidden.Key), "k1", unreadable.Result.ID); got != model.CurrencyUnknown {
		t.Fatalf("a result of a repository Dispatch cannot see = %q, want unknown", got)
	}
	if asked := fake.asks("acme/hidden"); asked != 0 {
		t.Fatalf("acme/hidden's pull requests were asked for %d times, want none: Dispatch sees none of that repository", asked)
	}

	writeHead(t, database, "acme/widgets", 71, newHeadCommit, "open", seen.Key)
	known := decodeBody[recordedResult](t, recordResult(t, handler, seen.Key, resultBody("k1", "pass", newHeadCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, seen.Key), "k1", known.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result at a head Dispatch already holds = %q, want current", got)
	}
	if asked := fake.asks("acme/widgets"); asked != 0 {
		t.Fatalf("acme/widgets' pull requests were asked for %d times, want none: the stored head row answers", asked)
	}
}

// What a superseded result costs: one GitHub call, once, and not again while the answer stands.
// A check with two results — the newer at the head of a pull request Dispatch holds, the older
// at a commit it holds none for — is the state a push past a recorded head leaves behind. The
// superseded commit heads no row Dispatch keeps, so the first read asks GitHub about it, and
// GitHub answers by naming the pull request at the head it has now: an answer about that commit
// that stands until something opens a pull request at it. So the pair is held off for
// delivery.CommitPullSettledFor and no read inside that span asks again
// (delivery.CommitPullReads.Settle, api/results_currency.go's readThroughHead); without that, a
// tab left open would spend the call again every delivery.CommitPullReadStaleAfter for as long
// as the result is read.
func TestASupersededResultsCommitIsAskedOfGitHubOnceAndNotAgainWithinTheSpan(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESP", "acme/widgets", "Pushed past")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	// The older result, recorded while GitHub named no pull request for its commit, so nothing is
	// stored for that commit.
	superseded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	// The push: the pull request's head moves, Dispatch holds that row, and the next result of the
	// same check is recorded there.
	writeHead(t, database, "acme/widgets", 97, newHeadCommit, "open", issue.Key)
	newest := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", newHeadCommit, nil)))

	// GitHub names that pull request for the superseded commit too, carrying the head it has now,
	// which is what it answers for a commit a later push replaced. The cache, rewound to its zero
	// value, holds no read off, so what keeps the second read from asking is the settling and not
	// the span.
	fake.answers(func(repo, _ string) string {
		return fmt.Sprintf(`[{"number":97,"title":%q,"body":"","html_url":"https://github.com/%s/pull/97",
			"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}]`,
			issue.Key+" the work", repo, newHeadCommit, repo)
	})
	served.commitPulls = delivery.CommitPullReads{}

	for _, read := range []struct {
		name  string
		asked int
	}{{"first", 1}, {"second", 0}} {
		spent := fake.asks("acme/widgets")
		answered := readResults(t, handler, issue.Key)
		if got := currencyOf(t, answered, "k1", superseded.Result.ID); got != model.CurrencyOlder {
			t.Fatalf("the %s read of the result a later push left behind = %q, want older", read.name, got)
		}
		if got := currencyOf(t, answered, "k1", newest.Result.ID); got != model.CurrencyCurrent {
			t.Fatalf("the %s read of the newest result, at the head Dispatch holds, = %q, want current", read.name, got)
		}
		if asked := fake.asks("acme/widgets") - spent; asked != read.asked {
			t.Fatalf("the %s read asked GitHub for the superseded commit's pull requests %d times, want %d",
				read.name, asked, read.asked)
		}
	}
	// Settled, not merely bounded: the pair is refused a claim far past
	// delivery.CommitPullReadStaleAfter, which is what keeps the call off the refetches of the
	// hour that follows. The settling's own moment is the read's, a second or so before this
	// one, so a claim a minute short of the span is inside it.
	if served.commitPulls.Claim("acme/widgets", headCommit, time.Now().Add(delivery.CommitPullSettledFor-time.Minute)) {
		t.Fatal("a claim of the superseded commit inside the settled span was granted, want it refused")
	}
}

// The settling is believed for a span and no longer. GitHub has answered about the commit —
// naming a pull request whose head is another commit — and a pull request opened later can
// still have its head at that commit, which no event tells Dispatch about: so the answer is
// held for delivery.CommitPullSettledFor and then asked again, and a settling kept for the life
// of the process would leave such a result `older` for ever (dispatch://INFRA-1240, the round-3
// review of pull request #2798).
//
// Four steps: the first read settles the pair; the second pull request opens at the commit; a
// read inside the span answers `older` and asks GitHub nothing, which is what bounds the cost;
// and a read past the span asks once and answers `current`.
func TestAPullRequestOpenedAtASettledCommitIsSeenOnceTheSpanEnds(t *testing.T) {
	handler, _, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESS", "acme/widgets", "Opened at a settled commit")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	// The result is recorded while GitHub names no pull request for its commit at all, so its
	// own read-through stores nothing and the commit heads no row Dispatch keeps.
	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	// The pull request the commit is behind, and the one that opens at it later: both titled
	// after the issue, so each is attributed to it.
	pushedPast := func(repo string) string {
		return fmt.Sprintf(`{"number":97,"title":%q,"body":"","html_url":"https://github.com/%s/pull/97",
			"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
			issue.Key+" the work", repo, newHeadCommit, repo)
	}
	openedAtTheCommit := func(repo, sha string) string {
		return fmt.Sprintf(`{"number":98,"title":%q,"body":"","html_url":"https://github.com/%s/pull/98",
			"state":"open","merged":false,"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/y","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`,
			issue.Key+" the rest", repo, sha, repo)
	}

	// 1. GitHub names the one pull request, whose head is another commit, and the read settles
	// the pair on that answer.
	fake.answers(func(repo, _ string) string { return "[" + pushedPast(repo) + "]" })
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("the read that settles the commit = %q, want older", got)
	}

	// 2. The second pull request opens with its head at that commit, and nothing tells Dispatch:
	// a deployment holding no delivery settings consumes no `pull_request` event, and the
	// reconcile's head pass only re-reads rows that already exist.
	fake.answers(func(repo, sha string) string {
		return "[" + pushedPast(repo) + "," + openedAtTheCommit(repo, sha) + "]"
	})

	// 3. Inside the span the settling still refuses the read, which is what bounds its cost: a
	// tab left open spends no call an hour on a commit GitHub has answered about.
	spent := fake.asks("acme/widgets")
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("the read inside the settled span = %q, want older: the settling stands", got)
	}
	if asked := fake.asks("acme/widgets") - spent; asked != 0 {
		t.Fatalf("the read inside the settled span asked GitHub %d times, want none", asked)
	}

	// 4. Past the span. The cache's moments are its only clock, and the public way to move one
	// is to stamp the pair again from an earlier moment: a settling made
	// delivery.CommitPullSettledFor ago is one whose span has just run out, which is what the
	// clock does on its own between a reviewer's refetches, without the test waiting an hour for
	// it — as the sibling tests rewind the whole cache to its zero value for the shorter span.
	served.commitPulls.Settle("acme/widgets", headCommit, time.Now().Add(-delivery.CommitPullSettledFor))
	spent = fake.asks("acme/widgets")
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("the read past the settled span = %q, want current: a pull request has opened at the commit", got)
	}
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Fatalf("the read past the settled span asked GitHub %d times, want 1", asked)
	}
}

// What a child's result on its parent's tab costs: nothing. Its commit heads the child's own pull
// request, attributed to the child, so Dispatch has been told that commit's pull request and
// asking GitHub again teaches it nothing — the result reads `older` there by the currency rule
// itself, whatever GitHub answers (R4, api/results_currency.go, readThroughHead).
func TestAChildsResultOnTheParentsTabAsksGitHubNothing(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	parent := issueInRepository(t, handler, "RESN", "acme/widgets", "Parent")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESN", "title": "Child", "parent": parent.Key, "force": true}, "alice"))
	// The child's own pull request: open at the commit and attributed to the child.
	writeHead(t, database, "acme/widgets", 98, headCommit, "open", child.Key)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", headCommit, nil)))
	// GitHub would name that pull request again, so a call that was spent would show as an ask;
	// and the cache, rewound to its zero value, holds no read off.
	fake.answers(openPullAt(98, child.Key))
	served.commitPulls = delivery.CommitPullReads{}
	spent := fake.asks("acme/widgets")

	if got := currencyOf(t, readResults(t, handler, parent.Key), "k1", recorded.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("the child's result at the child's own head = %q, want older", got)
	}
	if asked := fake.asks("acme/widgets") - spent; asked != 0 {
		t.Fatalf("the read asked GitHub for the child's commit's pull requests %d times, want none", asked)
	}
}

// Eligibility is the (issue, commit) pair's, and never one result's. A check holds a result at
// the head of a pull request nothing told Dispatch of and a later result at a commit no pull
// request carries, and a sibling check holds one result, at that same head. Every result of the
// issue is answered from one memo per issue and commit (api/results_currency.go, headOf), so a
// read the eligibility test skipped for one result must not stand as the answer for the rest:
// both results at the head read `current`, whichever check recorded them and whenever.
func TestEveryResultOfAnIssueAtAPullRequestsHeadReadsCurrent(t *testing.T) {
	handler, _, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESZ", "acme/widgets", "Two checks at one head")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)

	atHead := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	sibling := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k3", "pass", headCommit, nil)))
	// A later result of the first check, at a commit no pull request carries: a run against a
	// working copy, or the commit of a branch nobody has opened a pull request from.
	decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "fail", newHeadCommit, nil)))

	// The pull request opens at the first commit, and nothing tells Dispatch.
	open := openPullAt(91, issue.Key)
	fake.answers(func(repo, sha string) string {
		if sha == headCommit {
			return open(repo, sha)
		}
		return `[]`
	})
	served.commitPulls = delivery.CommitPullReads{}

	read := readResults(t, handler, issue.Key)
	if got := currencyOf(t, read, "k3", sibling.Result.ID); got != model.CurrencyCurrent {
		t.Errorf("the sibling check's only result, at the pull request's head, = %q, want current", got)
	}
	if got := currencyOf(t, read, "k1", atHead.Result.ID); got != model.CurrencyCurrent {
		t.Errorf("the earlier result of the check that has a later one elsewhere = %q, want current", got)
	}
}

// The same, where the later result is a child's. A child records against a check of its parent at
// the head of the child's own pull request, which Dispatch holds; the parent's own results sit at
// the parent's head, which it holds nothing for. The child's commit is eligible for no read —
// Dispatch keeps that row and asking GitHub teaches nothing — and the parent's commit is eligible
// for one, so the parent's results read `current` and the child's commit costs no call.
func TestAParentsResultsReadCurrentThoughAChildRecordedOnTheSameCheck(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	parent := issueInRepository(t, handler, "RESY", "acme/widgets", "Parent")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", siblingChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RESY", "title": "Child", "parent": parent.Key, "force": true}, "alice"))

	atHead := decodeBody[recordedResult](t, recordResult(t, handler, parent.Key, resultBody("k1", "pass", headCommit, nil)))
	sibling := decodeBody[recordedResult](t, recordResult(t, handler, parent.Key, resultBody("k3", "pass", headCommit, nil)))
	// The child's own pull request, open at its own head, and the child's result against the
	// parent's check there — recorded after the parent's two.
	writeHead(t, database, "acme/widgets", 98, newHeadCommit, "open", child.Key)
	decodeBody[recordedResult](t, recordResult(t, handler, child.Key, resultBody("k1", "pass", newHeadCommit, nil)))

	// The parent's pull request opens at the parent's head, and nothing tells Dispatch.
	open := openPullAt(91, parent.Key)
	fake.answers(func(repo, sha string) string {
		if sha == headCommit {
			return open(repo, sha)
		}
		return `[]`
	})
	served.commitPulls = delivery.CommitPullReads{}
	spent := fake.asks("acme/widgets")

	read := readResults(t, handler, parent.Key)
	if got := currencyOf(t, read, "k3", sibling.Result.ID); got != model.CurrencyCurrent {
		t.Errorf("the sibling check's only result, at the parent's head, = %q, want current", got)
	}
	if got := currencyOf(t, read, "k1", atHead.Result.ID); got != model.CurrencyCurrent {
		t.Errorf("the parent's result at the parent's head, beside the child's = %q, want current", got)
	}
	if asked := fake.asks("acme/widgets") - spent; asked != 1 {
		t.Errorf("the read asked GitHub %d times, want 1: the parent's head alone, since Dispatch keeps the child's row", asked)
	}
}

// The control the two above are read against: one check, one result, at the head of a pull
// request nothing told Dispatch of, and nothing else recorded anywhere. It reads `current`, so a
// failure of either test above is the eligibility test refusing the call and not the read-through
// failing to repair the result (api/results_currency.go, readThroughHead).
func TestACheckWithOneResultAtAPullRequestsHeadReadsCurrent(t *testing.T) {
	handler, _, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESX", "acme/widgets", "One result")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k3", "pass", headCommit, nil)))
	fake.answers(openPullAt(91, issue.Key))
	served.commitPulls = delivery.CommitPullReads{}

	if got := currencyOf(t, readResults(t, handler, issue.Key), "k3", recorded.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("one result at the head of a pull request opened after it was recorded = %q, want current", got)
	}
}

// A closed pull request at the commit holds no read off. Dispatch holds a row for a pull request
// closed without merging at the result's commit, and a new pull request is then opened from the
// same head while no event reaches Dispatch. The closed row makes no result current and no pass
// of Dispatch's ever reads it again (delivery.ListStalePullRequestHeads), so counting it as a
// pull request Dispatch holds would leave the result `older` for good; the read asks GitHub and
// the result reads `current`.
func TestAClosedPullRequestsHeadDoesNotHoldOffTheReadThrough(t *testing.T) {
	handler, database, served, fake := readThroughGitHub(t, "acme/widgets")
	issue := issueInRepository(t, handler, "RESW", "acme/widgets", "Opened again from the same head")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", siblingChecks)
	writeHead(t, database, "acme/widgets", 90, headCommit, "closed", issue.Key)

	recorded := decodeBody[recordedResult](t, recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)))
	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyOlder {
		t.Fatalf("a result at the head of a closed pull request = %q, want older", got)
	}

	// The new pull request, opened from that same head.
	fake.answers(openPullAt(91, issue.Key))
	served.commitPulls = delivery.CommitPullReads{}

	if got := currencyOf(t, readResults(t, handler, issue.Key), "k1", recorded.Result.ID); got != model.CurrencyCurrent {
		t.Fatalf("a result at the head of a pull request opened after a closed one = %q, want current", got)
	}
}
