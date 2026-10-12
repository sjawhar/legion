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
// a withdrawal the issue has since come back from, so never undoes the READY that followed it. A
// READY a person merges (its by-hand note) covers no arm at all, so whoever armed it is undone, and
// neither does one in a tree that lingers after its root left the workflow, which beginLinger
// disarms.
func TestAnAutoMergeDisableRowKeepsOnlyTheAwaitedREADYHeadsArm(t *testing.T) {
	const byHand = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	for _, tc := range []struct {
		name      string
		at        phase.Phase
		ready     string
		mergeNote string
		lingers   bool
		want      int
	}{
		{"awaiting its merge at the READY head", phase.AwaitingMerge, "ready-head", "", false, 0},
		{"awaiting its merge, the head past READY", phase.AwaitingMerge, "older-head", "", false, 1},
		{"back in implementing", phase.Implementing, "ready-head", "", false, 1},
		{"awaiting a person's merge at the READY head", phase.AwaitingMerge, "ready-head", byHand, false, 1},
		{"awaiting its merge at the READY head in a lingering tree", phase.AwaitingMerge, "ready-head", "", true, 1},
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
				autoMerge := `{"enabledAt":"2026-10-11T00:00:00Z"}`
				if disables > 0 {
					autoMerge = "null"
				}
				_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"id":"PR_42","headRefOid":"ready-head","autoMergeRequest":%s,"mergeQueueEntry":null}}}}`, autoMerge)
			}))
			t.Cleanup(server.Close)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			child := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: tc.at, Generation: 1, Status: "retro", Rank: "U"}
			if tc.lingers {
				until, parent := time.Now().Add(time.Hour), "LEGION-207"
				putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-207", Project: "LEGION", Tree: "LEGION-207", Title: "Root", Phase: phase.Done,
					Generation: 1, Status: "done", Rank: "T", LingerUntil: &until})
				child.Tree, child.Parent = "LEGION-207", &parent
			}
			putOutboxIssue(t, pool, records, child)
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: tc.ready, MergeNote: tc.mergeNote})
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

// The row reads GitHub before the record. The merger arms only after its READY commits, so an arm
// the row's GitHub read finds is already covered by the record it reads next. Read the other way
// round, a READY committing and its merger arming between the two reads would leave the row
// keeping nothing and disabling that legitimate arm. Here the READY commits while GitHub answers
// the row's read, and the arm it reports is kept.
func TestAnAutoMergeDisableRowReadsGitHubBeforeTheRecord(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Merging,
		Generation: 1, Status: "retro", Rank: "U"})
	var mu sync.Mutex
	disables := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "disablePullRequestAutoMerge") {
			disables++
			_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
			return
		}
		// The READY commits, and its merger arms, as GitHub answers this read.
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), "update issues set phase = 'awaiting_merge' where key = 'LEGION-208'"); err != nil {
				return err
			}
			return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"})
		}); err != nil {
			t.Errorf("commit the READY: %v", err)
		}
		_, _ = fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"id":"PR_42","headRefOid":"ready-head","autoMergeRequest":{"enabledAt":"2026-10-11T00:00:00Z"},"mergeQueueEntry":null}}}}`)
	}))
	t.Cleanup(server.Close)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
		repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL}
	if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())); err != nil {
		t.Fatalf("execute the disarm: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if disables != 0 {
		t.Fatalf("auto-merge disables %d, want the READY's arm kept", disables)
	}
}
