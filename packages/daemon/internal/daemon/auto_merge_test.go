package daemon

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
	"github.com/sjawhar/legion/daemon/internal/record"
)

// autoMergeGitHub is GitHub's GraphQL API for acme/widgets#42 as the disarm reads and changes it:
// whether auto-merge is enabled, whether the pull request sits in a merge queue, and every
// mutation the disarm sent, with the bearer it sent it under.
type autoMergeGitHub struct {
	url       string
	mu        sync.Mutex
	armed     bool
	queued    bool
	mutations []string
	bearers   []string
}

func newAutoMergeGitHub(t *testing.T, armed, queued bool) *autoMergeGitHub {
	t.Helper()
	g := &autoMergeGitHub{armed: armed, queued: queued}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.URL.Path != "/graphql" {
			http.NotFound(w, r)
			return
		}
		g.bearers = append(g.bearers, r.Header.Get("Authorization"))
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.Query, "disablePullRequestAutoMerge"):
			g.mutations = append(g.mutations, fmt.Sprintf("disable %v", request.Variables["id"]))
			g.armed = false
			_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
		case strings.Contains(request.Query, "dequeuePullRequest"):
			g.mutations = append(g.mutations, fmt.Sprintf("dequeue %v", request.Variables["id"]))
			g.queued = false
			_, _ = w.Write([]byte(`{"data":{"dequeuePullRequest":{"clientMutationId":null}}}`))
		default:
			if request.Variables["owner"] != "acme" || request.Variables["name"] != "widgets" || request.Variables["number"] != float64(42) {
				http.Error(w, "wrong pull request", http.StatusBadRequest)
				return
			}
			autoMerge, entry := "null", "null"
			if g.armed {
				autoMerge = `{"enabledAt":"2026-10-11T00:00:00Z"}`
			}
			if g.queued {
				entry = `{"id":"MQE_42"}`
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"id":"PR_42","autoMergeRequest":%s,"mergeQueueEntry":%s}}}}`, autoMerge, entry)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// An auto_merge_disable row turns off whatever would merge the pull request without a person: it
// disables auto-merge where GitHub has it enabled and takes the pull request out of the merge
// queue where it sits there, as the implement App, which armed it. A pull request with neither is
// left alone, so a retried row, or one for a pull request never armed, changes nothing.
func TestAnAutoMergeDisableRowDisarmsThePullRequest(t *testing.T) {
	for _, tc := range []struct {
		name          string
		armed, queued bool
		want          []string
	}{
		{"auto-merge enabled", true, false, []string{"disable PR_42"}},
		{"in the merge queue", false, true, []string{"dequeue PR_42"}},
		{"neither", false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newAutoMergeGitHub(t, tc.armed, tc.queued)
			runner := &outbox{log: quietLogger(), tokens: roleTokens{}, project: "legion", repo: ghrepo.MustParse("acme/widgets"), githubAPI: github.url}
			row := mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute the disarm: %v", err)
			}
			// The retry of the same row finds nothing left to disarm.
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute the disarm again: %v", err)
			}
			github.mu.Lock()
			defer github.mu.Unlock()
			if !slices.Equal(github.mutations, tc.want) {
				t.Fatalf("mutations %v, want %v", github.mutations, tc.want)
			}
			for _, bearer := range github.bearers {
				if bearer != "Bearer implement-app-token-for-acme" {
					t.Fatalf("a GitHub call carried %q, want the implement App's token", bearer)
				}
			}
		})
	}
}
