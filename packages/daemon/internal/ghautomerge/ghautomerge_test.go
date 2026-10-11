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
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// github is GitHub's GraphQL API for acme/widgets#42 as Disarm reads and changes it: whether the
// pull request exists, when its auto-merge was enabled and when it entered a merge queue (empty
// for neither), and every mutation Disarm sent.
type github struct {
	mu         sync.Mutex
	missing    bool
	enabledAt  string
	enqueuedAt string
	mutations  []string
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
			g.enabledAt = ""
			_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
		case strings.Contains(request.Query, "dequeuePullRequest"):
			g.mutations = append(g.mutations, fmt.Sprintf("dequeue %v", request.Variables["id"]))
			g.enqueuedAt = ""
			_, _ = w.Write([]byte(`{"data":{"dequeuePullRequest":{"clientMutationId":null}}}`))
		default:
			if request.Variables["owner"] != "acme" || request.Variables["name"] != "widgets" || request.Variables["number"] != float64(42) {
				t.Errorf("read of %v, want acme/widgets#42", request.Variables)
			}
			if g.missing {
				// GitHub's own answer for a pull request number the repository does not have.
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 42."}]}`))
				return
			}
			autoMerge, entry := "null", "null"
			if g.enabledAt != "" {
				autoMerge = fmt.Sprintf(`{"enabledAt":%q}`, g.enabledAt)
			}
			if g.enqueuedAt != "" {
				entry = fmt.Sprintf(`{"enqueuedAt":%q}`, g.enqueuedAt)
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"id":"PR_42","autoMergeRequest":%s,"mergeQueueEntry":%s}}}}`, autoMerge, entry)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// Disarm turns off what would merge the pull request without a person: auto-merge where GitHub
// has it enabled, and the pull request's merge-queue entry where it has one. It leaves alone a pull
// request with neither, one GitHub answers NOT_FOUND for, and an auto-merge or queue entry made
// after the disarm was decided (since), which a later READY armed and this disarm must not undo. A
// second Disarm of the same pull request changes nothing.
func TestDisarmTurnsOffOnlyWhatWasArmedBeforeIt(t *testing.T) {
	since := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	const before, after = "2026-10-11T11:00:00Z", "2026-10-11T13:00:00Z"
	for _, tc := range []struct {
		name                  string
		missing               bool
		enabledAt, enqueuedAt string
		want                  []string
		wantDisarmed          Disarmed
	}{
		{"auto-merge enabled before", false, before, "", []string{"disable PR_42"}, Disarmed{AutoMerge: true}},
		{"in the merge queue since before", false, "", before, []string{"dequeue PR_42"}, Disarmed{Queue: true}},
		{"neither", false, "", "", nil, Disarmed{}},
		{"a pull request GitHub does not have", true, "", "", nil, Disarmed{Missing: true}},
		{"auto-merge enabled after", false, after, "", nil, Disarmed{}},
		{"in the merge queue since after", false, "", after, nil, Disarmed{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &github{missing: tc.missing, enabledAt: tc.enabledAt, enqueuedAt: tc.enqueuedAt}
			client := githubrest.Client{Token: "token", API: githubrest.GraphQLAPI(g.serve(t))}
			got, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, since)
			if err != nil || got != tc.wantDisarmed {
				t.Fatalf("Disarm = %+v, %v; want %+v", got, err, tc.wantDisarmed)
			}
			if again, err := Disarm(context.Background(), client, ghrepo.MustParse("acme/widgets"), 42, since); err != nil || again.AutoMerge || again.Queue {
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
