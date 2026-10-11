package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An auto_merge_disable row decides by the record when it runs, as the implement App: while the
// issue awaits its merge and the pull request's head on GitHub is the merger's READY commit, the
// arm is that READY's and is kept; otherwise auto-merge is disabled. A row queued long before, by
// a withdrawal the issue has since come back from, so never undoes the READY that followed it.
func TestAnAutoMergeDisableRowKeepsOnlyTheAwaitedREADYHeadsArm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		ready string
		want  int
	}{
		{"awaiting its merge at the READY head", phase.AwaitingMerge, "ready-head", 0},
		{"awaiting its merge, the head past READY", phase.AwaitingMerge, "older-head", 1},
		{"back in implementing", phase.Implementing, "ready-head", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bearers []string
			disables := 0
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
					disables++
					_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
					return
				}
				_, _ = fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"id":"PR_42","headRefOid":"ready-head","autoMergeRequest":{"enabledAt":"2026-10-11T00:00:00Z"},"mergeQueueEntry":null}}}}`)
			}))
			t.Cleanup(server.Close)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: tc.at,
				Generation: 1, Status: "retro", Rank: "U"})
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: tc.ready})
			}); err != nil {
				t.Fatalf("seed the merger's READY: %v", err)
			}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
				repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL}
			if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())); err != nil {
				t.Fatalf("execute the disarm: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if disables != tc.want {
				t.Fatalf("auto-merge disables %d, want %d", disables, tc.want)
			}
			for _, bearer := range bearers {
				if bearer != "Bearer implement-app-token-for-acme" {
					t.Fatalf("a GitHub call carried %q, want the implement App's token", bearer)
				}
			}
		})
	}
}
