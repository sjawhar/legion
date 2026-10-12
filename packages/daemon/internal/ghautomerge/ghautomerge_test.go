package ghautomerge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// github is GitHub's GraphQL API for acme/widgets#42 as Disarm reads and changes it: what its read
// cannot find ("pullRequest", "repository", or empty when it finds the pull request), the pull
// request's head, whether its auto-merge is enabled and whether it has a merge-queue entry, and
// every mutation Disarm sent. requeue is GitHub enqueueing again a pull request it dequeues while
// its auto-merge is still enabled; failDisable answers the disable with an error, and stuckArm
// answers it as done while leaving auto-merge enabled.
type github struct {
	mu                             sync.Mutex
	notFound                       string
	head                           string
	armed, queued                  bool
	requeue, failDisable, stuckArm bool
	mutations                      []string
}

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
		case g.notFound == "pullRequest":
			// GitHub's own answer for a pull request number the repository does not have.
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 42."}]}`))
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
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"id":"PR_42","headRefOid":%q,"autoMergeRequest":%s,"mergeQueueEntry":%s}}}}`, g.head, autoMerge, entry)
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

// keeping is a Keep that answers head.
func keeping(head string) Keep {
	return func(context.Context) (string, error) { return head, nil }
}
