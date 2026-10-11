package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An auto_merge_disable row runs ghautomerge.Disarm as the implement App, which armed the pull
// request, on the repository's GraphQL route, and finishes once GitHub has disabled the auto-merge
// it had enabled before the row was queued. What Disarm decides is ghautomerge's own test.
func TestAnAutoMergeDisableRowDisarmsAsTheImplementApp(t *testing.T) {
	var mu sync.Mutex
	var bearers, mutations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/graphql" {
			http.NotFound(w, r)
			return
		}
		bearers = append(bearers, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "disablePullRequestAutoMerge") {
			mutations = append(mutations, "disable")
			_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"id":"PR_42","autoMergeRequest":{"enabledAt":"2026-10-11T00:00:00Z"},"mergeQueueEntry":null}}}}`))
	}))
	t.Cleanup(server.Close)
	runner := &outbox{log: quietLogger(), tokens: roleTokens{}, project: "legion", repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL}
	row := mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())
	row.CreatedAt = time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute the disarm: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mutations) != 1 {
		t.Fatalf("mutations %v, want the auto-merge disabled once", mutations)
	}
	for _, bearer := range bearers {
		if bearer != "Bearer implement-app-token-for-acme" {
			t.Fatalf("a GitHub call carried %q, want the implement App's token", bearer)
		}
	}
}
