package ghautomerge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// github is GitHub's GraphQL API for acme/widgets#42 as Disarm and Submit read and change it: what
// its read cannot find ("pullRequest", "repository", or empty when it finds the pull request), the
// pull request's state, head and merge state, whether its base requires a merge queue, whether the
// repository allows auto-merge, whether its auto-merge is enabled and whether it has a merge-queue
// entry, and every mutation sent. requeue is GitHub enqueueing again a pull request it dequeues
// while its auto-merge is still enabled; failDisable answers the disable with an error, stuckArm
// answers it as done while leaving auto-merge enabled, refuseSubmit answers an enable or merge with
// GitHub's refusal, refuseType is that GraphQL error's type, movedOnRefuse is the head a push moves
// the pull request to as GitHub refuses it, and submitStatus answers an enable or merge with that
// HTTP status instead. updated is the pull request's updatedAt as its JSON value, GitHub's last
// change to it: updatedAt by default, `null` for an answer that carries none. limitAfterRefusal
// answers every read after a refused enable or merge with GitHub's RATE_LIMITED and a Retry-After of
// 180 seconds.
type github struct {
	mu                             sync.Mutex
	notFound                       string
	state                          string
	head                           string
	mergeState                     string
	updated                        string
	queueRequired, autoMergeOK     bool
	armed, queued                  bool
	requeue, failDisable, stuckArm bool
	refuseSubmit, refuseType       string
	movedOnRefuse                  string
	submitStatus                   int
	limitAfterRefusal, refused     bool
	mutations                      []string
}

// updatedAt is the fake pull request's default last change on GitHub.
const updatedAt = "2026-10-16T05:00:00Z"

func (g *github) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.Query, "disablePullRequestAutoMerge"):
			g.mutations = append(g.mutations, fmt.Sprintf("disable %v", request.Variables["id"]))
			if g.failDisable {
				_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`))
				return
			}
			g.armed = g.armed && g.stuckArm
			_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
		case strings.Contains(request.Query, "dequeuePullRequest"):
			g.mutations = append(g.mutations, fmt.Sprintf("dequeue %v", request.Variables["id"]))
			g.queued = g.requeue && g.armed
			_, _ = w.Write([]byte(`{"data":{"dequeuePullRequest":{"clientMutationId":null}}}`))
		case strings.Contains(request.Query, "enablePullRequestAutoMerge"), strings.Contains(request.Query, "mergePullRequest"):
			verb := "merge"
			if strings.Contains(request.Query, "enablePullRequestAutoMerge") {
				verb = "enable"
			}
			g.mutations = append(g.mutations, fmt.Sprintf("%s %v %v %v", verb, request.Variables["id"], request.Variables["head"], request.Variables["method"]))
			if g.submitStatus != 0 {
				http.Error(w, `{"message":"Server Error"}`, g.submitStatus)
				return
			}
			if g.refuseSubmit != "" {
				if g.movedOnRefuse != "" {
					g.head = g.movedOnRefuse
				}
				kind := g.refuseType
				if kind == "" {
					kind = "UNPROCESSABLE"
				}
				g.refused = true
				_, _ = fmt.Fprintf(w, `{"data":null,"errors":[{"type":%q,"message":%q}]}`, kind, g.refuseSubmit)
				return
			}
			if verb == "enable" {
				g.armed = true
			} else {
				g.state = "MERGED"
			}
			_, _ = w.Write([]byte(`{"data":{}}`))
		case g.notFound == "pullRequest":
			// GitHub's own answer for a pull request number the repository does not have.
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 42."}]}`))
		case g.limitAfterRefusal && g.refused:
			w.Header().Set("Retry-After", "180")
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		case g.notFound == "repository":
			// GitHub's answer for a repository the token cannot see.
			_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name 'acme/widgets'."}]}`))
		default:
			if request.Variables["owner"] != "acme" || request.Variables["name"] != "widgets" || request.Variables["number"] != float64(42) {
				t.Errorf("read of %v, want acme/widgets#42", request.Variables)
			}
			autoMerge, entry := "null", "null"
			if g.armed {
				autoMerge = `{"enabledAt":"2026-10-11T12:00:00Z"}`
			}
			if g.queued {
				entry = `{"enqueuedAt":"2026-10-11T12:05:00Z"}`
			}
			state, mergeState, updated := g.state, g.mergeState, g.updated
			if state == "" {
				state = "OPEN"
			}
			if mergeState == "" {
				mergeState = "BLOCKED"
			}
			if updated == "" {
				updated = strconv.Quote(updatedAt)
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"autoMergeAllowed":%t,"pullRequest":{"id":"PR_42","state":%q,"headRefOid":%q,"mergeStateStatus":%q,"updatedAt":%s,"isMergeQueueEnabled":%t,"autoMergeRequest":%s,"mergeQueueEntry":%s}}}}`,
				g.autoMergeOK, state, g.head, mergeState, updated, g.queueRequired, autoMerge, entry)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// Disarm turns off what would merge the pull request without a person, by the pull request's state
// when it runs: while the pull request's head is the READY head it was told to keep, nothing is
// touched; otherwise auto-merge is disabled where GitHub has it enabled, and then the merge-queue
// entry is removed where it has one, however late GitHub made it. A pull request with nothing
// armed, or one GitHub answers NOT_FOUND for at the pull request, is left alone. A second Disarm
// changes nothing.
func TestDisarmTurnsOffEverythingArmedUnlessTheHeadIsTheKeptREADYHead(t *testing.T) {
	for _, tc := range []struct {
		name          string
		notFound      string
		armed, queued bool
		keep          string
		want          []string
		wantDisarmed  Disarmed
	}{
		{"auto-merge enabled, no READY to keep", "", true, false, "", []string{"disable PR_42"}, Disarmed{AutoMerge: true}},
		{"in the merge queue, no READY to keep", "", false, true, "", []string{"dequeue PR_42"}, Disarmed{Queue: true}},
		// In a merge-queue repository GitHub enqueues an armed pull request only once its checks
		// pass, so an arm's queue entry can be made after the disarm was decided: it goes too.
		{"armed and its queue entry made since", "", true, true, "", []string{"disable PR_42", "dequeue PR_42"}, Disarmed{AutoMerge: true, Queue: true}},
		{"armed for a READY the head moved past", "", true, true, "ready", []string{"disable PR_42", "dequeue PR_42"}, Disarmed{AutoMerge: true, Queue: true}},
		{"armed at the READY head it was told to keep", "", true, true, "head", nil, Disarmed{Kept: true}},
		{"nothing armed", "", false, false, "", nil, Disarmed{}},
		{"a pull request GitHub does not have", "pullRequest", false, false, "", nil, Disarmed{Missing: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{notFound: tc.notFound, head: "head", armed: tc.armed, queued: tc.queued}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			got, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping(tc.keep))
			if err != nil || got != tc.wantDisarmed {
				t.Fatalf("Disarm = %+v, %v; want %+v", got, err, tc.wantDisarmed)
			}
			if again, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping(tc.keep)); err != nil || again.AutoMerge || again.Queue {
				t.Fatalf("a second Disarm = %+v, %v; want nothing left to disarm", again, err)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if !slices.Equal(g.mutations, tc.want) {
				t.Fatalf("mutations %v, want %v", g.mutations, tc.want)
			}
		})
	}
}

// GitHub enqueues again a pull request it takes out of the merge queue while its auto-merge is
// still enabled, so Disarm disables auto-merge first: dequeued first, the pull request would be
// back in the queue, still armed, as Disarm reported it disarmed.
func TestDisarmDisablesAutoMergeBeforeItDequeues(t *testing.T) {
	g := &github{head: "head", armed: true, queued: true, requeue: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	if _, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping("")); err != nil {
		t.Fatalf("Disarm = %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armed || g.queued {
		t.Fatalf("after Disarm auto-merge enabled %t, queued %t; want neither (mutations %v)", g.armed, g.queued, g.mutations)
	}
}

// A disable GitHub refuses does not leave the queue entry standing: Disarm still takes the pull
// request out of the merge queue, then fails with the refused disable, so the row retries it.
func TestDisarmDequeuesEvenWhenTheDisableFails(t *testing.T) {
	g := &github{head: "head", armed: true, queued: true, failDisable: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	_, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping(""))
	if err == nil || !strings.Contains(err.Error(), "disable the auto-merge") {
		t.Fatalf("Disarm with the disable refused = %v, want the disable's failure", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if want := []string{"disable PR_42", "dequeue PR_42"}; !slices.Equal(g.mutations, want) || g.queued {
		t.Fatalf("mutations %v, queued %t; want %v and the entry gone", g.mutations, g.queued, want)
	}
}

// Disarm reads the pull request once more after its mutations, and fails, so the row retries,
// when anything it was to turn off is still armed: a mutation GitHub answered as done is not proof
// the merge is off.
func TestDisarmFailsWhenTheArmIsStillOnAfterItsMutations(t *testing.T) {
	g := &github{head: "head", armed: true, stuckArm: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	if _, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping("")); err == nil || !strings.Contains(err.Error(), "still armed") {
		t.Fatalf("Disarm with the arm still on afterwards = %v, want an error saying it is still armed", err)
	}
}

// A NOT_FOUND anywhere but at the pull request, such as a repository the implement App cannot see,
// is no answer about the pull request: Disarm fails, so its row retries and the failure is logged,
// rather than finishing as though there were nothing to disarm.
func TestDisarmFailsWhenGitHubCannotFindTheRepository(t *testing.T) {
	g := &github{notFound: "repository"}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	got, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, keeping(""))
	if err == nil || got.Missing {
		t.Fatalf("Disarm of a repository GitHub cannot find = %+v, %v; want an error, not Missing", got, err)
	}
}

// Submit asks GitHub to merge the READY head under the repository's own rules, and never another
// head. A merge queue gets the pull request enqueued and a repository that allows auto-merge gets it
// armed, each with GitHub's expected-head check; a pull request GitHub reports mergeable now is
// merged at that head. A repository that allows neither waits, without a mutation, until GitHub
// reports the pull request mergeable. An arm or a merge-queue entry at READY's head is not the
// end: Submit waits on it (Armed) until GitHub reports the pull request merged at that head, which
// alone ends the submission, as does the record leaving READY (Stale). While the record still
// awaits READY's head, a pull request GitHub shows closed, merged at another head, or at another
// head waits for the webhook that moves the record (Lagging).
func TestSubmitUsesGitHubsAnswerForTheREADYHead(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		state, head, mergeState, keep string
		queueRequired, autoMergeOK    bool
		armed, queued                 bool
		want                          []string
		outcome                       Outcome
		wait                          string
	}{
		{name: "a merge queue enqueues it and waits for the merge", head: "head", keep: "head", mergeState: "BLOCKED", queueRequired: true,
			want: []string{"enable PR_42 head SQUASH"}, wait: "armed"},
		{name: "auto-merge allowed arms it and waits for the merge", head: "head", keep: "head", mergeState: "BLOCKED", autoMergeOK: true,
			want: []string{"enable PR_42 head SQUASH"}, wait: "armed"},
		{name: "a clean pull request merges at once", head: "head", keep: "head", mergeState: "CLEAN",
			want: []string{"merge PR_42 head SQUASH"}, outcome: Merged},
		{name: "an unstable pull request merges at once", head: "head", keep: "head", mergeState: "UNSTABLE",
			want: []string{"merge PR_42 head SQUASH"}, outcome: Merged},
		{name: "auto-merge disallowed waits while blocked", head: "head", keep: "head", mergeState: "BLOCKED", wait: "unmergeable"},
		{name: "auto-merge disallowed waits while GitHub computes", head: "head", keep: "head", mergeState: "UNKNOWN", wait: "unmergeable"},
		{name: "auto-merge disallowed waits while behind", head: "head", keep: "head", mergeState: "BEHIND", wait: "unmergeable"},
		{name: "already armed waits for the merge", head: "head", keep: "head", mergeState: "BLOCKED", autoMergeOK: true, armed: true, wait: "armed"},
		{name: "already queued waits for the merge", head: "head", keep: "head", mergeState: "BLOCKED", queueRequired: true, queued: true, wait: "armed"},
		{name: "merged at READY's head", state: "MERGED", head: "head", keep: "head", mergeState: "MERGED", outcome: Already},
		{name: "merged at another head while the record awaits READY", state: "MERGED", head: "moved", keep: "head", mergeState: "MERGED", wait: "lagging"},
		{name: "closed while the record awaits READY", state: "CLOSED", head: "head", keep: "head", mergeState: "CLEAN", wait: "lagging"},
		{name: "closed once the record left READY", state: "CLOSED", head: "head", keep: "", mergeState: "CLEAN", outcome: Stale},
		{name: "moved past READY while the record awaits it", head: "moved", keep: "head", mergeState: "CLEAN", wait: "lagging"},
		{name: "moved past READY once the record left it", head: "moved", keep: "", mergeState: "CLEAN", outcome: Stale},
		{name: "armed once the record left READY", head: "head", keep: "", mergeState: "BLOCKED", autoMergeOK: true, armed: true, outcome: Stale},
		{name: "the record no longer covers READY", head: "head", keep: "", mergeState: "CLEAN", outcome: Stale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{state: tc.state, head: tc.head, mergeState: tc.mergeState, queueRequired: tc.queueRequired,
				autoMergeOK: tc.autoMergeOK, armed: tc.armed, queued: tc.queued}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			got, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", keeping(tc.keep))
			var lagging *Lagging
			var unmergeable *Unmergeable
			var armed *Armed
			waited := map[string]bool{"lagging": errors.As(err, &lagging), "unmergeable": errors.As(err, &unmergeable), "armed": errors.As(err, &armed)}
			if tc.wait != "" {
				if !errors.Is(err, wait.ErrWaiting) || !waited[tc.wait] {
					t.Fatalf("Submit = %q, %v; want a %s wait", got, err, tc.wait)
				}
			} else if err != nil || got != tc.outcome {
				t.Fatalf("Submit = %q, %v; want %q", got, err, tc.outcome)
			}
			if unmergeable != nil && unmergeable.State != tc.mergeState {
				t.Fatalf("Unmergeable wait in %s, want %s", unmergeable.State, tc.mergeState)
			}
			if armed != nil && armed.Head != "head" {
				t.Fatalf("Armed wait at %s, want READY's head", armed.Head)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if !slices.Equal(g.mutations, tc.want) {
				t.Fatalf("mutations %v, want %v", g.mutations, tc.want)
			}
		})
	}
}

// An arm or merge-queue entry that disappears while the record still awaits READY's head (a disarm
// sent before READY that GitHub applied late, or a merge queue dropping the pull request) leaves
// nothing to merge it, so Submit submits again by the same choice: it arms again where auto-merge is
// allowed, and merges where GitHub reports the pull request mergeable now. A wait on an arm is
// routine: only GitHub's own rules end it.
func TestSubmitSubmitsAgainWhenTheArmDisappears(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mergeState string
		want       string
	}{
		{"arms again where auto-merge is allowed", "BLOCKED", "enable PR_42 head SQUASH"},
		{"merges where GitHub reports it mergeable now", "CLEAN", "merge PR_42 head SQUASH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{head: "head", mergeState: "BLOCKED", autoMergeOK: true, armed: true}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", keeping("head"))
			var armed *Armed
			if !errors.As(err, &armed) || !armed.Routine() {
				t.Fatalf("Submit while armed = %v, want a routine Armed wait", err)
			}
			g.mu.Lock()
			g.armed, g.mergeState = false, tc.mergeState
			g.mu.Unlock()
			_, _ = Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", keeping("head"))
			g.mu.Lock()
			defer g.mu.Unlock()
			if !slices.Equal(g.mutations, []string{tc.want}) {
				t.Fatalf("mutations %v, want only %q once the arm is gone", g.mutations, tc.want)
			}
		})
	}
}

// A Lagging submission carries GitHub's last change to the pull request from the same read, and it
// is stalled only once that change has stood longer than LagBound, on GitHub's own clock: a
// disagreement GitHub made a moment ago is a webhook still on its way.
func TestALaggingSubmissionStallsOnlyOnGitHubsOwnClock(t *testing.T) {
	g := &github{state: "CLOSED", head: "head", mergeState: "UNKNOWN"}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", keeping("head"))
	var lagging *Lagging
	if !errors.As(err, &lagging) {
		t.Fatalf("Submit = %v, want Lagging", err)
	}
	changed, _ := time.Parse(time.RFC3339, updatedAt)
	if !lagging.Since.Equal(changed) {
		t.Fatalf("Lagging since %s, want GitHub's updatedAt %s", lagging.Since, changed)
	}
	if lagging.Stalled(changed.Add(LagBound)) || !lagging.Stalled(changed.Add(LagBound+time.Second)) {
		t.Fatalf("Lagging stalled at the bound or not past it (bound %s from %s)", LagBound, changed)
	}
}

// GitHub always answers a pull request's updatedAt. A read without one is not GitHub's answer, so
// Submit fails it loudly rather than judging a disagreement against no clock at all.
func TestSubmitFailsAReadWithoutUpdatedAt(t *testing.T) {
	g := &github{state: "CLOSED", head: "head", mergeState: "UNKNOWN", updated: "null"}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", keeping("head"))
	var lagging *Lagging
	if err == nil || errors.Is(err, wait.ErrWaiting) || errors.As(err, &lagging) || !strings.Contains(err.Error(), "updatedAt") {
		t.Fatalf("Submit without updatedAt = %v, want a failure naming updatedAt", err)
	}
}

// A wait on GitHub's merge state is routine only while the repository's own rules end it without
// anyone acting for Legion: a required check still running or a required review (BLOCKED), or
// GitHub still computing the state (UNKNOWN). A branch to update (BEHIND), a draft, or a conflict
// (DIRTY) waits on someone, so it is not.
func TestOnlyABlockedOrComputingMergeStateIsARoutineWait(t *testing.T) {
	for state, routine := range map[string]bool{"BLOCKED": true, "UNKNOWN": true, "BEHIND": false, "DRAFT": false, "DIRTY": false} {
		g := &github{head: "head", mergeState: state}
		client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
		_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", keeping("head"))
		var unmergeable *Unmergeable
		if !errors.As(err, &unmergeable) || !errors.Is(err, wait.ErrWaiting) || unmergeable.Routine() != routine {
			t.Fatalf("Submit while %s = %v; want an Unmergeable wait, routine %t", state, err, routine)
		}
	}
}

// GitHub's refusal of the merge or the arm is permanent when GitHub still stands where it refused:
// the same open pull request at READY's head, nothing armed, and the same submission chosen, so a
// retry would send the same request and meet the same refusal. Submit returns it as *Refused, with
// GitHub's message. A refusal after which the pull request moved, and an HTTP failure, are
// transient: the row retries and reads GitHub again.
func TestSubmitTellsAPermanentRefusalFromATransientOne(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	for _, tc := range []struct {
		name      string
		g         *github
		permanent bool
	}{
		{"GitHub refuses the merge and stands where it was", &github{head: "head", mergeState: "CLEAN", refuseSubmit: squash}, true},
		{"GitHub refuses the arm and stands where it was", &github{head: "head", mergeState: "BLOCKED", autoMergeOK: true, refuseSubmit: squash}, true},
		{"GitHub rejects the merge as invalid", &github{head: "head", mergeState: "CLEAN", refuseSubmit: squash, refuseType: "VALIDATION_FAILED"}, true},
		{"GitHub rate limits the merge", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "API rate limit exceeded", refuseType: "RATE_LIMITED"}, false},
		{"GitHub has an internal merge error", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "Service temporarily unavailable", refuseType: "INTERNAL"}, false},
		{"GitHub refuses the merge as a push moves the head", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "Head branch was modified. Review and try the merge again.", movedOnRefuse: "moved"}, false},
		{"GitHub answers the merge with a server error", &github{head: "head", mergeState: "CLEAN", submitStatus: http.StatusBadGateway}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(tc.g.serve(t))}
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", keeping("head"))
			var refused *Refused
			if err == nil || errors.Is(err, wait.ErrWaiting) {
				t.Fatalf("Submit = %v, want GitHub's failure", err)
			}
			if errors.As(err, &refused) != tc.permanent {
				t.Fatalf("Submit = %v (%T); want permanent %t", err, err, tc.permanent)
			}
			if tc.permanent && (!strings.Contains(refused.Message, squash) || !strings.Contains(err.Error(), squash)) {
				t.Fatalf("refusal = %+v, want GitHub's message to include %q", refused, squash)
			}
		})
	}
}

// GitHub refuses the merge, then rate-limits the read that would confirm the refusal. Submit
// returns the read's error, the refusal as its context, so the runner sees GitHub's rate limit, its
// Retry-After and its budget, and the row neither ends nor meets the limit again early.
func TestSubmitKeepsTheRateLimitOfItsConfirmationRead(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	g := &github{head: "head", mergeState: "CLEAN", refuseSubmit: squash, limitAfterRefusal: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", keeping("head"))
	var refused *Refused
	if err == nil || errors.As(err, &refused) || errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("Submit = %v, want a failure to retry", err)
	}
	if after, budget := githubrest.RetryAfter(err); after != 180*time.Second || budget != githubrest.BudgetSecondary {
		t.Fatalf("RetryAfter(%v) = %s, %q; want the confirmation read's 180s secondary limit", err, after, budget)
	}
	if !strings.Contains(err.Error(), squash) {
		t.Fatalf("Submit = %v, want the refusal kept as context", err)
	}
}

// keeping is a Keep that answers head.
func keeping(head string) Keep {
	return func(context.Context) (string, error) { return head, nil }
}
