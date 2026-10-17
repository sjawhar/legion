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
// entry, its timeline of arms and removals as GraphQL's viewer (viewer, the implement App's bot
// when empty) reads it, and every mutation sent. requeue is GitHub enqueueing again a pull request
// it dequeues while its auto-merge is still enabled; failDisable answers the disable with an error,
// stuckArm answers it as done while leaving auto-merge enabled, refuseSubmit answers an enable or
// merge with GitHub's refusal, refuseType is that GraphQL error's type, movedOnRefuse is the head a
// push moves the pull request to as GitHub refuses it, and submitStatus answers an enable or merge
// with that HTTP status instead. updated is the pull request's updatedAt as its JSON value, GitHub's
// last change to it: updatedAt by default, `null` for an answer that carries none.
// limitAfterRefusal answers every read after a refused enable or merge with GitHub's RATE_LIMITED
// and a Retry-After of 180 seconds. timelineReads counts the timeline's reads. contextPages is each
// further page of the merge queue commit's checks by the cursor it follows, pageReads counts their
// reads, and failPages answers every one with GitHub's server error.
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
	viewer                         string
	timeline                       []string
	timelineReads, pageReads       int
	contextPages                   map[string]string
	failPages                      bool
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
		case strings.Contains(request.Query, "MergeGroupContexts"):
			g.pageReads++
			if g.failPages {
				http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
				return
			}
			page, ok := g.contextPages[fmt.Sprint(request.Variables["after"])]
			if !ok || request.Variables["oid"] != "group-sha" {
				t.Errorf("checks page read of %v, want a page this fake lists", request.Variables)
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"object":{"oid":"group-sha","statusCheckRollup":{"contexts":%s}}}}}`, page)
		case strings.Contains(request.Query, "timelineItems"):
			g.timelineReads++
			if request.Variables["owner"] != "acme" || request.Variables["name"] != "widgets" || request.Variables["number"] != float64(42) {
				t.Errorf("timeline read of %v, want acme/widgets#42", request.Variables)
			}
			viewer := g.viewer
			if viewer == "" {
				viewer = "legion-implementer[bot]"
			}
			_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":%q},"repository":{"pullRequest":{"timelineItems":{"nodes":[%s]}}}}}`, viewer, strings.Join(g.timeline, ","))
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
// head. A merge queue gets the pull request enqueued once GitHub reports it mergeable now, and a
// repository that allows auto-merge gets it armed, each with GitHub's expected-head check; a pull
// request GitHub reports mergeable now is merged at that head. A merge-queue base waits, without a
// mutation, while GitHub reports the pull request anything else (a required check running, BLOCKED;
// GitHub computing, UNKNOWN), since an enable there would arm auto-merge rather than enqueue it; a
// repository that allows neither waits the same way. An arm or a merge-queue entry at READY's head
// is not the end: Submit waits on it (Armed) until GitHub reports the pull request merged at that
// head, which alone ends the submission, as does the record leaving READY (Stale). While the record
// still awaits READY's head, a pull request GitHub shows closed, merged at another head, or at
// another head waits for the webhook that moves the record (Lagging).
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
		{name: "a merge queue enqueues a pull request GitHub reports mergeable and waits for the merge", head: "head", keep: "head", mergeState: "CLEAN", queueRequired: true,
			want: []string{"enable PR_42 head SQUASH"}, wait: "armed"},
		{name: "a merge queue waits while a required check runs", head: "head", keep: "head", mergeState: "BLOCKED", queueRequired: true, autoMergeOK: true, wait: "unmergeable"},
		{name: "a merge queue waits while GitHub computes", head: "head", keep: "head", mergeState: "UNKNOWN", queueRequired: true, autoMergeOK: true, wait: "unmergeable"},
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
			got, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping(tc.keep))
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
			if unmergeable != nil && (unmergeable.State != tc.mergeState || unmergeable.Queue != tc.queueRequired) {
				t.Fatalf("Unmergeable wait in %s on a merge-queue base %t, want %s and %t", unmergeable.State, unmergeable.Queue, tc.mergeState, tc.queueRequired)
			}
			if armed != nil && armed.Head != "head" {
				t.Fatalf("Armed wait at %s, want READY's head", armed.Head)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if !slices.Equal(g.mutations, tc.want) {
				t.Fatalf("mutations %v, want %v", g.mutations, tc.want)
			}
			if g.timelineReads != 0 {
				t.Fatalf("timeline reads %d by a row that never saw READY's head armed, want none", g.timelineReads)
			}
		})
	}
}

// failedRun is the merge_group run whose checks failed on the commit the merge queue built for #42.
const failedRun = "https://github.com/acme/widgets/actions/runs/7"

// unexplained is a hypothetical queue removal reason, one GitHub gives no other meaning: GitHub's
// own are merged, failed_checks, manual and merge_conflict, and this stands for any it adds.
const unexplained = "timed_out"

// enqueued and enabled are GitHub's timeline records of the pull request entering the merge queue
// and of its auto-merge being enabled.
const (
	enqueued = `{"__typename":"AddedToMergeQueueEvent","createdAt":"2026-10-16T05:00:00Z"}`
	enabled  = `{"__typename":"AutoMergeEnabledEvent","createdAt":"2026-10-16T05:00:00Z"}`
)

// pagedChecks is a merge queue commit whose checks GitHub lists over two pages: the first, cut at
// 100, all passed, and its integration failure is on the second, secondChecksPage, which moreChecks
// reads after the cursor after-100.
var pagedChecks = func() string {
	var passing []string
	for i := range 100 {
		passing = append(passing, fmt.Sprintf(`{"__typename":"CheckRun","name":"check-%d","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"%s/job/%d","checkSuite":{"workflowRun":{"url":"%s"}}}`, i, failedRun, i, failedRun))
	}
	return `{"oid":"group-sha","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":true,"endCursor":"after-100"},"nodes":[` + strings.Join(passing, ",") + `]}}}`
}()

const secondChecksPage = `{"pageInfo":{"hasNextPage":false,"endCursor":"after-101"},"nodes":[` +
	`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedRun + `/job/200","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}`

// dequeued is GitHub's record of the pull request taken out of the merge queue by actor for reason,
// with commit, the commit the queue built for it, or null.
func dequeued(reason, actor, commit string) string {
	return fmt.Sprintf(`{"__typename":"RemovedFromMergeQueueEvent","id":"RFMQE_9","createdAt":"2026-10-16T05:10:00Z","reason":%q,"actor":{"login":%q},"beforeCommit":%s}`, reason, actor, commit)
}

// disableReasons is GitHub's sentence for each auto-merge reason code these tests use, as its
// timeline answers them.
var disableReasons = map[string]string{
	"manually_disabled":    "Manually disabled by user",
	"merge_commit_blocked": "Merge commits are not allowed on this repository",
	"closed":               "Pull request was closed",
}

// disabled is GitHub's record of the pull request's auto-merge turned off for reasonCode, with its
// sentence, by actor, or naming no actor when actor is empty.
func disabled(reasonCode, actor string) string {
	reason, ok := disableReasons[reasonCode]
	if !ok {
		panic("no sentence of GitHub's for auto-merge reason code " + reasonCode)
	}
	login := "null"
	if actor != "" {
		login = fmt.Sprintf(`{"login":%q}`, actor)
	}
	return fmt.Sprintf(`{"__typename":"AutoMergeDisabledEvent","id":"AMDE_9","createdAt":"2026-10-16T05:10:00Z","reason":%q,"reasonCode":%q,"actor":%s}`, reason, reasonCode, login)
}

// Once the row has seen READY's head armed or queued (armed), an open pull request at that head with
// nothing armed is a removal someone made, which Submit reads from the pull request's timeline and
// never answers with a second submission (LEGION-800). The merge queue taking it out because the
// merge failed - GitHub's reason failed_checks, or a reason GitHub gives no other meaning with a run
// that failed on the commit the queue built - is MergeFailed, naming each failing check and its run
// and not one that passed. Anyone else's removal since the last arm (a person's dequeue, a person
// turning auto-merge off, the queue finding a conflict) is Removed, naming who, GitHub's reason and
// whether it was the queue. The queue's removal for the merge itself waits for GitHub to show the
// merge, routinely, and a timeline with no removal since the arm waits on GitHub too, but not
// routinely. Only removals that restart the submission submit again: the App submitting's own
// (GraphQL's viewer: the daemon's disarm landing late), and auto-merge a close turned off, which the
// reopen at READY's head undid. Only a queue removal for failed_checks, or for a reason GitHub gives
// no other meaning, reads its commit's checks past the first page: the merge, a person's dequeue and
// a conflict mean what they say, so they read none, and a page GitHub fails to answer never turns
// the merge's wait into a failure.
func TestSubmitReadsARemovalItDidNotMakeInsteadOfSubmittingAgain(t *testing.T) {
	red := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"e2e","status":"IN_PROGRESS","conclusion":null,"detailsUrl":"` + failedRun + `/job/3","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"TIMED_OUT","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"StatusContext","context":"freeze","state":"SUCCESS","targetUrl":"https://example.com/freeze"}]}}}`
	green := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	// cancelled is the queue's commit after a person dequeues it while its merge_group runs are in
	// flight: they end cancelled, which is no failed merge.
	cancelled := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"CANCELLED","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"e2e","status":"COMPLETED","conclusion":"SKIPPED","detailsUrl":"` + failedRun + `/job/3","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	// needsAction is the queue's commit with a run that ended needing action, which Legion's rule for
	// a check run counts as failed (classify.RunResult).
	needsAction := `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}},` +
		`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"ACTION_REQUIRED","detailsUrl":"` + failedRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedRun + `"}}}]}}}`
	for _, tc := range []struct {
		name     string
		queue    bool
		timeline []string
		// failPages fails every read of a further page of the queue commit's checks, and pages is how
		// many such reads Submit makes.
		failPages bool
		pages     int
		// failed names what MergeFailed must name; removed what Removed must; wait the wait, merging
		// (GitHub's own merge, routine) or disappeared (not routine); resubmit is a second submission.
		failed, removed []string
		wait            string
		resubmit        bool
	}{
		{name: "a person dequeues it while its merge group runs", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", cancelled)},
			removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "the merge queue removes it for an unexplained reason on a merge group that only cancelled", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", cancelled)},
			removed: []string{"github-merge-queue", "merge queue", unexplained}},
		{name: "the merge queue removes it for an unexplained reason on a merge group whose run needs action", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", needsAction)},
			failed: []string{unexplained, "integration (action_required, " + failedRun + ")"}},
		{name: "the merge queue removes it for failed_checks, its failure past the first 100 checks", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", pagedChecks)},
			pages: 1, failed: []string{"failed_checks", "integration (failure, " + failedRun + ")"}},
		{name: "the merge queue removes it for an unexplained reason, its failure past the first 100 checks", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", pagedChecks)},
			pages: 1, failed: []string{unexplained, "integration (failure, " + failedRun + ")"}},
		{name: "a person dequeues it from a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", pagedChecks)},
			failPages: true, removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "the merge queue finds a conflict in a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", pagedChecks)},
			failPages: true, removed: []string{"github-merge-queue", "merge queue", "merge_conflict"}},
		{name: "the merge queue takes it out to merge a merge group of more than 100 checks", queue: true, timeline: []string{enqueued, dequeued("merged", "github-merge-queue", pagedChecks)},
			failPages: true, wait: "merging"},
		{name: "the merge queue removes it for failed_checks", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", red)},
			failed: []string{"#42", "head", "failed_checks", "group-sha", "integration (timed_out, " + failedRun + ")"}},
		{name: "the merge queue removes it for an unexplained reason on a red merge group", queue: true, timeline: []string{enqueued, dequeued(unexplained, "github-merge-queue", red)},
			failed: []string{unexplained, "integration (timed_out, " + failedRun + ")"}},
		{name: "the merge queue removes it for failed_checks naming no commit", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", "null")},
			failed: []string{"failed_checks", "named no commit"}},
		{name: "the merge queue removes it for failed_checks with no failing run read", queue: true, timeline: []string{enqueued, dequeued("failed_checks", "github-merge-queue", green)},
			failed: []string{"failed_checks", "no failing run", "group-sha"}},
		{name: "a person dequeues it", queue: true, timeline: []string{enqueued, dequeued("manual", "a-maintainer", "null")},
			removed: []string{"a-maintainer", "merge queue", "manual", "#42", "head"}},
		{name: "the merge queue finds a conflict", queue: true, timeline: []string{enqueued, dequeued("merge_conflict", "github-merge-queue", "null")},
			removed: []string{"github-merge-queue", "merge queue", "merge_conflict"}},
		{name: "a person turns its auto-merge off", timeline: []string{enabled, disabled("manually_disabled", "a-maintainer")},
			removed: []string{"a-maintainer turned off the auto-merge", "manually_disabled: Manually disabled by user"}},
		{name: "auto-merge is turned off with GitHub naming no actor", timeline: []string{enabled, disabled("manually_disabled", "")},
			removed: []string{"someone turned off the auto-merge", "manually_disabled: Manually disabled by user"}},
		{name: "GitHub turns its auto-merge off, naming the App that enabled it", timeline: []string{enabled, disabled("merge_commit_blocked", "legion-implementer")},
			removed: []string{"GitHub turned off the auto-merge", "naming legion-implementer, the App that submitted it", "merge_commit_blocked: Merge commits are not allowed on this repository"}},
		{name: "the daemon's own disarm lands late", timeline: []string{enabled, disabled("manually_disabled", "legion-implementer")}, resubmit: true},
		{name: "the daemon's own disarm, then a person's dequeue", queue: true, timeline: []string{enqueued, disabled("manually_disabled", "legion-implementer"), dequeued("manual", "a-maintainer", "null")},
			removed: []string{"a-maintainer", "merge queue", "manual"}},
		{name: "a person's removal before the last arm, then the daemon's own", timeline: []string{enabled, disabled("manually_disabled", "a-maintainer"), enabled, disabled("manually_disabled", "legion-implementer")}, resubmit: true},
		{name: "a close a reopen at READY's head undid", timeline: []string{enabled, disabled("closed", "a-maintainer")}, resubmit: true},
		{name: "the merge queue takes it out to merge it", queue: true, timeline: []string{enqueued, dequeued("merged", "github-merge-queue", green)}, wait: "merging"},
		{name: "the timeline names no removal since the arm", queue: true, timeline: []string{dequeued("failed_checks", "github-merge-queue", red), enqueued}, wait: "disappeared"},
		{name: "the timeline names no arm or removal at all", wait: "disappeared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A merge-queue base GitHub reports mergeable, or a repository that allows auto-merge
			// whose required checks still run: a second submission would be an enable either way.
			mergeState := "BLOCKED"
			if tc.queue {
				mergeState = "CLEAN"
			}
			g := &github{head: "head", mergeState: mergeState, queueRequired: tc.queue, autoMergeOK: true, timeline: tc.timeline,
				contextPages: map[string]string{"after-100": secondChecksPage}, failPages: tc.failPages}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var failed *MergeFailed
			var removed *Removed
			var merging *Merging
			var disappeared *Disappeared
			var routine interface{ Routine() bool }
			var armed *Armed
			switch {
			case tc.failed != nil:
				if !errors.As(err, &failed) || errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("Submit = %v (%T), want MergeFailed", err, err)
				}
				if failed.Event != "RFMQE_9" {
					t.Fatalf("MergeFailed event %q, want GitHub's id for the removal", failed.Event)
				}
				for _, want := range tc.failed {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("MergeFailed %q, want it to name %q", err, want)
					}
				}
				for _, passed := range []string{"lint", "e2e", "freeze"} {
					if strings.Contains(err.Error(), passed) {
						t.Fatalf("MergeFailed %q names %s, which did not fail", err, passed)
					}
				}
			case tc.removed != nil:
				if !errors.As(err, &removed) || errors.Is(err, wait.ErrWaiting) || removed.Queue != tc.queue {
					t.Fatalf("Submit = %v (%T), want Removed from the merge queue %t", err, err, tc.queue)
				}
				for _, want := range tc.removed {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("Removed %q, want it to name %q", err, want)
					}
				}
				if (removed.Detail == "") != removed.Queue {
					t.Fatalf("Removed %q with Detail %q, want GitHub's sentence in Detail for an auto-merge disable and none for a queue removal", err, removed.Detail)
				}
			case tc.wait == "merging":
				if !errors.As(err, &merging) || !errors.Is(err, wait.ErrWaiting) || !merging.Routine() {
					t.Fatalf("Submit = %v (%T), want a routine Merging wait", err, err)
				}
			case tc.wait == "disappeared":
				if !errors.As(err, &disappeared) || !errors.Is(err, wait.ErrWaiting) || errors.As(err, &routine) && routine.Routine() {
					t.Fatalf("Submit = %v (%T), want a Disappeared wait that is not routine", err, err)
				}
			case tc.resubmit:
				if !errors.As(err, &armed) {
					t.Fatalf("Submit = %v (%T), want READY's head submitted again and an Armed wait", err, err)
				}
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			var want []string
			if tc.resubmit {
				want = []string{"enable PR_42 head SQUASH"}
			}
			if !slices.Equal(g.mutations, want) {
				t.Fatalf("mutations %v, want %v", g.mutations, want)
			}
			if g.pageReads != tc.pages {
				t.Fatalf("reads of the queue commit's further checks %d, want %d", g.pageReads, tc.pages)
			}
		})
	}
}

// A queue removal whose checks GitHub fails to answer past their first page is a failure the row
// retries, never a judgment of the page it read: failed_checks judged on that page alone would say
// GitHub reported no failing run, and an unexplained reason would read as a removal by someone. The
// retry, with the page answered, finds the failure on it.
func TestSubmitRetriesAQueueRemovalWhoseChecksGitHubFailsToRead(t *testing.T) {
	for _, reason := range []string{"failed_checks", unexplained} {
		t.Run(reason, func(t *testing.T) {
			g := &github{head: "head", mergeState: "CLEAN", queueRequired: true, autoMergeOK: true,
				timeline:     []string{enqueued, dequeued(reason, "github-merge-queue", pagedChecks)},
				contextPages: map[string]string{"after-100": secondChecksPage}, failPages: true}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			var failed *MergeFailed
			var removed *Removed
			var refused *Refused
			if err == nil || errors.Is(err, wait.ErrWaiting) || errors.As(err, &failed) || errors.As(err, &removed) || errors.As(err, &refused) ||
				strings.Contains(err.Error(), "no failing run") || !strings.Contains(err.Error(), "after-100") {
				t.Fatalf("Submit with the queue commit's second page of checks unanswered = %v (%T), want a failure to retry naming the page", err, err)
			}
			g.mu.Lock()
			g.failPages = false
			g.mu.Unlock()
			_, err = Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
			if !errors.As(err, &failed) || !strings.Contains(err.Error(), "integration (failure, "+failedRun+")") {
				t.Fatalf("the retry once GitHub answers the page = %v (%T), want MergeFailed naming the failure on it", err, err)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if len(g.mutations) != 0 || g.pageReads != 2 {
				t.Fatalf("mutations %v and page reads %d, want none and one each run", g.mutations, g.pageReads)
			}
		})
	}
}

// A row that has seen READY's head armed reads no timeline while the arm or entry stands: it waits
// on it (Armed) as before.
func TestSubmitWaitsOnAStandingArmWithoutReadingTheTimeline(t *testing.T) {
	g := &github{head: "head", mergeState: "CLEAN", queueRequired: true, queued: true}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", true, keeping("head"))
	var armed *Armed
	if !errors.As(err, &armed) || !armed.Routine() {
		t.Fatalf("Submit while queued = %v, want a routine Armed wait", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timelineReads != 0 || len(g.mutations) != 0 {
		t.Fatalf("timeline reads %d and mutations %v while READY's head stands queued, want none", g.timelineReads, g.mutations)
	}
}

// A Lagging submission carries GitHub's last change to the pull request from the same read, and it
// is stalled only once that change has stood longer than LagBound, on GitHub's own clock: a
// disagreement GitHub made a moment ago is a webhook still on its way.
func TestALaggingSubmissionStallsOnlyOnGitHubsOwnClock(t *testing.T) {
	g := &github{state: "CLOSED", head: "head", mergeState: "UNKNOWN"}
	client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
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
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
	var lagging *Lagging
	if err == nil || errors.Is(err, wait.ErrWaiting) || errors.As(err, &lagging) || !strings.Contains(err.Error(), "updatedAt") {
		t.Fatalf("Submit without updatedAt = %v, want a failure naming updatedAt", err)
	}
}

// A wait on GitHub's merge state is routine only while the repository's own rules end it without
// anyone acting for Legion: a required check still running or a required review (BLOCKED), or
// GitHub still computing the state (UNKNOWN). A branch to update (BEHIND), a draft, or a conflict
// (DIRTY) waits on someone, so it is not. The same holds in a repository that allows no auto-merge
// and on a base that requires a merge queue.
func TestOnlyABlockedOrComputingMergeStateIsARoutineWait(t *testing.T) {
	for state, routine := range map[string]bool{"BLOCKED": true, "UNKNOWN": true, "BEHIND": false, "DRAFT": false, "DIRTY": false} {
		for _, queue := range []bool{false, true} {
			g := &github{head: "head", mergeState: state, queueRequired: queue, autoMergeOK: queue}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
			var unmergeable *Unmergeable
			if !errors.As(err, &unmergeable) || !errors.Is(err, wait.ErrWaiting) || unmergeable.Routine() != routine || unmergeable.Queue != queue {
				t.Fatalf("Submit while %s on a merge-queue base %t = %v; want an Unmergeable wait, routine %t", state, queue, err, routine)
			}
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
		{"GitHub refuses the enqueue and stands where it was", &github{head: "head", mergeState: "CLEAN", queueRequired: true, refuseSubmit: squash}, true},
		{"GitHub rejects the merge as invalid", &github{head: "head", mergeState: "CLEAN", refuseSubmit: squash, refuseType: "VALIDATION_FAILED"}, true},
		{"GitHub rate limits the merge", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "API rate limit exceeded", refuseType: "RATE_LIMITED"}, false},
		{"GitHub has an internal merge error", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "Service temporarily unavailable", refuseType: "INTERNAL"}, false},
		{"GitHub refuses the merge as a push moves the head", &github{head: "head", mergeState: "CLEAN", refuseSubmit: "Head branch was modified. Review and try the merge again.", movedOnRefuse: "moved"}, false},
		{"GitHub answers the merge with a server error", &github{head: "head", mergeState: "CLEAN", submitStatus: http.StatusBadGateway}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(tc.g.serve(t))}
			_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
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
	_, err := Submit(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, "head", false, keeping("head"))
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
