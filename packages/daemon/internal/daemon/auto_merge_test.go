package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghautomerge"
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

// The row reads GitHub before the record. The daemon arms only after the READY commits, so an arm
// the row's GitHub read finds is already covered by the record it reads next. Read the other way
// round, a READY committing and its merge being armed between the two reads would leave the row
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
		// The READY commits, and its merge is armed, as GitHub answers this read.
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

// A ready_merge row submits the merge as the implement App only while the record still awaits that
// exact READY head. A READY a person merges, a READY the issue has left, and a tree that lingers are
// stale; GitHub's expected-head check guards the head between the row's read and its mutation.
func TestAREADYMergeRowSubmitsOnlyTheAwaitedREADYHead(t *testing.T) {
	const byHand = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	for _, tc := range []struct {
		name      string
		at        phase.Phase
		ready     string
		mergeNote string
		lingers   bool
		merges    int
	}{
		{"awaiting its merge at the READY head", phase.AwaitingMerge, "ready-head", "", false, 1},
		{"awaiting another READY head", phase.AwaitingMerge, "older-head", "", false, 0},
		{"back in implementing", phase.Implementing, "ready-head", "", false, 0},
		{"awaiting a person's merge", phase.AwaitingMerge, "ready-head", byHand, false, 0},
		{"awaiting its merge in a lingering tree", phase.AwaitingMerge, "ready-head", "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bearers []string
			merges := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				bearers = append(bearers, r.Header.Get("Authorization"))
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(string(body), "mergePullRequest") {
					merges++
					if !strings.Contains(string(body), `"head":"ready-head"`) {
						t.Errorf("merge request %s, want READY's expected head", body)
					}
					_, _ = w.Write([]byte(`{"data":{}}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":{"repository":{"autoMergeAllowed":false,"pullRequest":{"id":"PR_42","state":"OPEN","headRefOid":"ready-head","mergeStateStatus":"CLEAN","isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null}}}}`))
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
			if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())); err != nil {
				t.Fatalf("execute the READY merge: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if merges != tc.merges {
				t.Fatalf("merges %d, want %d", merges, tc.merges)
			}
			for _, bearer := range bearers {
				if bearer != "Bearer implement-app-token-for-acme" {
					t.Fatalf("a GitHub call carried %q, want the implement App's token", bearer)
				}
			}
		})
	}
}

// GitHub refusing a ready_merge row's submission where a retry would meet the same refusal (squash
// merges disabled, a permission the App lacks) fails loudly: the issue's architect is told once,
// with GitHub's message, and the row finishes rather than retrying forever. A transient failure
// (GitHub's server error) tells nobody and keeps the row on its backoff.
func TestAREADYMergeRowTellsTheArchitectOnceWhenGitHubRefusesItForGood(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	for _, tc := range []struct {
		name      string
		answer    func(http.ResponseWriter)
		permanent bool
	}{
		{"refused for good", func(w http.ResponseWriter) {
			_, _ = fmt.Fprintf(w, `{"data":null,"errors":[{"type":"UNPROCESSABLE","message":%q}]}`, squash)
		}, true},
		{"a server error", func(w http.ResponseWriter) { http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(string(body), "mergePullRequest") {
					tc.answer(w)
					return
				}
				_, _ = w.Write([]byte(`{"data":{"repository":{"autoMergeAllowed":false,"pullRequest":{"id":"PR_42","state":"OPEN","headRefOid":"ready-head","mergeStateStatus":"CLEAN","isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null}}}}`))
			}))
			t.Cleanup(server.Close)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"})
			}); err != nil {
				t.Fatalf("seed the merger's READY: %v", err)
			}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
				repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL, now: time.Now}
			err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now()))
			var notices []string
			rows, qerr := pool.Query(context.Background(), "select payload->>'kind', payload->>'role', payload->>'reason' from outbox where kind = 'notice' and issue = 'LEGION-208'")
			if qerr != nil {
				t.Fatalf("read notices: %v", qerr)
			}
			for rows.Next() {
				var kind, role, reason string
				if err := rows.Scan(&kind, &role, &reason); err != nil {
					t.Fatalf("scan notice: %v", err)
				}
				notices = append(notices, kind+" "+role+" "+reason)
			}
			rows.Close()
			if !tc.permanent {
				if err == nil || len(notices) != 0 {
					t.Fatalf("execute = %v with notices %q; want the failure, to retry, and nobody told", err, notices)
				}
				return
			}
			if !errors.Is(err, errReadyMergeRefusedFinished) {
				t.Fatalf("execute = %v; want the permanent-refusal finish signal", err)
			}
			if len(notices) != 1 || !strings.HasPrefix(notices[0], "merge-refused architect ") || !strings.Contains(notices[0], squash) || !strings.Contains(notices[0], "#42") {
				t.Fatalf("notices %q; want one merge-refused to the architect naming #42 and GitHub's message", notices)
			}
		})
	}
}

// A permanent ready_merge refusal writes its architect notice and finishes the row in one
// transaction. If FinishOutbox loses its first connection, the transaction rolls the notice back
// too; after the lease expires, the one retried row leaves exactly one notice and no ready_merge
// row. Writing the notice first and finishing later would send it twice.
func TestARetriedPermanentREADYMergeRefusalPostsOneNotice(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "mergePullRequest") {
			_, _ = fmt.Fprintf(w, `{"data":null,"errors":[{"type":"UNPROCESSABLE","message":%q}]}`, squash)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"autoMergeAllowed":false,"pullRequest":{"id":"PR_42","state":"OPEN","headRefOid":"ready-head","mergeStateStatus":"CLEAN","isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null}}}}`))
	}))
	t.Cleanup(server.Close)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"})
	}); err != nil {
		t.Fatalf("seed the merger's READY: %v", err)
	}
	now := time.Now().UTC()
	row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.Enqueue(context.Background(), tx, row); err != nil {
			return err
		}
		return records.Enqueue(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("enqueue duplicate READY merge: %v", err)
	}
	flaky := &flakyOutboxStore{Store: records, finishFailures: 1}
	clock := now
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: flaky, tokens: roleTokens{}, project: "legion",
		repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL, now: func() time.Time { return clock }}

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	assertReadyMergeRefusalRows(t, pool, 1, 0)
	if got := flaky.failedFinishes(); got != 1 {
		t.Fatalf("failed finishes = %d, want the injected one", got)
	}

	clock = clock.Add(outboxLease + time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("retried RunOnce: %v", err)
	}
	assertReadyMergeRefusalRows(t, pool, 0, 1)
}

func assertReadyMergeRefusalRows(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, wantReady, wantNotice int) {
	t.Helper()
	var ready, notices int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'ready_merge'`).Scan(&ready); err != nil {
		t.Fatalf("count READY merge rows: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'merge-refused'`).Scan(&notices); err != nil {
		t.Fatalf("count merge-refused notices: %v", err)
	}
	if ready != wantReady || notices != wantNotice {
		t.Fatalf("READY merge rows = %d and notices = %d, want %d and %d", ready, notices, wantReady, wantNotice)
	}
}

// A ready_merge row waiting on a required review or check is routine, even after its retry backoff
// reaches the cap. It must not flood warnings while a person supplies the review GitHub requires.
func TestAWaitingREADYMergeStaysDebugAfterItsBackoffCaps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"autoMergeAllowed":false,"pullRequest":{"id":"PR_42","state":"OPEN","headRefOid":"ready-head","mergeStateStatus":"BLOCKED","isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null}}}}`))
	}))
	t.Cleanup(server.Close)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"})
	}); err != nil {
		t.Fatalf("seed the merger's READY: %v", err)
	}
	row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	row.Attempts = waitWarnAttempts
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.Enqueue(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("enqueue READY merge: %v", err)
	}
	var logged bytes.Buffer
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})), pool: pool, dispatchProject: "LEGION",
		records: records, tokens: roleTokens{}, project: "legion", repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL, now: time.Now}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if text := logged.String(); !strings.Contains(text, `level=DEBUG msg="outbox row waits"`) || strings.Contains(text, "level=WARN") {
		t.Fatalf("waiting READY merge logged %q, want one debug line and no warning", text)
	}
}

// A GitHub GraphQL RATE_LIMITED response is transient, and its Retry-After delay outranks the
// outbox's ordinary backoff so the implement App does not immediately exhaust its token again.
func TestARateLimitedREADYMergeHonorsGitHubsRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "mergePullRequest") {
			w.Header().Set("Retry-After", "180")
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"autoMergeAllowed":false,"pullRequest":{"id":"PR_42","state":"OPEN","headRefOid":"ready-head","mergeStateStatus":"CLEAN","isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null}}}}`))
	}))
	t.Cleanup(server.Close)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"})
	}); err != nil {
		t.Fatalf("seed the merger's READY: %v", err)
	}
	now := time.Date(2026, 10, 16, 5, 0, 0, 0, time.UTC)
	row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.Enqueue(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("enqueue READY merge: %v", err)
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
		repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL, now: func() time.Time { return now }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var nextAt time.Time
	if err := pool.QueryRow(context.Background(), `select next_at from outbox where kind = 'ready_merge'`).Scan(&nextAt); err != nil {
		t.Fatalf("read rate-limited row: %v", err)
	}
	if want := now.Add(180 * time.Second); !nextAt.Equal(want) {
		t.Fatalf("rate-limited READY merge next_at = %s, want GitHub's retry-after %s", nextAt, want)
	}
}

// When a slow worker's lease expires, a second worker owns the ready_merge row. The stale worker's
// permanent refusal must roll its notice back with its failed finish; only the replacement lease
// can leave the one architect notice.
func TestOnlyTheOwningREADYMergeLeasePostsAPermanentRefusal(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
	enqueueOutbox(t, pool, records, row)
	claim := func(at time.Time) record.OutboxRow {
		var claimed record.OutboxRow
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			rows, err := records.ClaimDue(context.Background(), tx, "LEGION", at, 1, outboxLease)
			if err != nil {
				return err
			}
			if len(rows) != 1 {
				return fmt.Errorf("claimed %d rows", len(rows))
			}
			claimed = rows[0]
			return nil
		}); err != nil {
			t.Fatalf("claim READY merge: %v", err)
		}
		return claimed
	}
	first := claim(now)
	second := claim(first.LeaseUntil.Add(time.Nanosecond))
	runner := &outbox{log: quietLogger(), pool: pool, records: records, now: time.Now}
	refused := &ghautomerge.Refused{Number: 42, Head: "ready-head", Action: "merge", Message: "GitHub GraphQL: Squash merges are disabled"}
	if err := runner.finishReadyMergeRefused(context.Background(), first, refused); err == nil {
		t.Fatal("stale lease recorded a permanent refusal, want its transaction rolled back")
	}
	assertReadyMergeRefusalRows(t, pool, 1, 0)
	if err := runner.finishReadyMergeRefused(context.Background(), second, refused); err != nil {
		t.Fatalf("replacement lease records refusal: %v", err)
	}
	assertReadyMergeRefusalRows(t, pool, 0, 1)
}

// One App token shares GitHub's rate limit across every row a RunOnce already leased. After the
// first ready_merge receives RATE_LIMITED, the runner must defer its remaining GitHub rows without
// sending them another GraphQL request.
func TestARateLimitedREADYMergeDefersTheRestOfItsBatch(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "mergePullRequest") {
			w.Header().Set("Retry-After", "180")
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"autoMergeAllowed":false,"pullRequest":{"id":"PR","state":"OPEN","headRefOid":"ready-head","mergeStateStatus":"CLEAN","isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null}}}}`))
	}))
	t.Cleanup(server.Close)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Date(2026, 10, 16, 5, 0, 0, 0, time.UTC)
	for _, issue := range []string{"LEGION-208", "LEGION-209"} {
		putOutboxIssue(t, pool, records, record.Issue{Key: issue, Project: "LEGION", Tree: issue, Title: "Workflow", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: issue, Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"})
		}); err != nil {
			t.Fatalf("seed %s merger: %v", issue, err)
		}
		row, err := record.NewOutboxRow(issue, record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
		if err != nil {
			t.Fatal(err)
		}
		enqueueOutbox(t, pool, records, row)
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
		repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL, now: func() time.Time { return now }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 2 {
		t.Fatalf("GraphQL requests = %d, want only the first row's read and rate-limited mutation", got)
	}
	var rows int
	var earliest time.Time
	var detail string
	if err := pool.QueryRow(context.Background(), `select count(*), min(next_at), string_agg(issue || ':' || attempts || ':' || next_at::text || ':' || last_error, ', ' order by issue) from outbox where kind = 'ready_merge'`).Scan(&rows, &earliest, &detail); err != nil {
		t.Fatalf("read deferred READY merges: %v", err)
	}
	if rows != 2 || earliest.Before(now.Add(180*time.Second)) {
		t.Fatalf("deferred READY merges = %d earliest %s (%s), want two rows no earlier than %s", rows, earliest, detail, now.Add(180*time.Second))
	}
}

// GitHub can rate-limit the GraphQL endpoint with a non-2xx REST answer. That path retains the
// Retry-After header too, rather than falling back to the ordinary one-second retry.
func TestAHTTPRateLimitedREADYMergeHonorsGitHubsRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"})
	}); err != nil {
		t.Fatalf("seed the merger's READY: %v", err)
	}
	now := time.Date(2026, 10, 16, 5, 0, 0, 0, time.UTC)
	row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
	if err != nil {
		t.Fatal(err)
	}
	enqueueOutbox(t, pool, records, row)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
		repo: ghrepo.MustParse("acme/widgets"), githubAPI: server.URL, now: func() time.Time { return now }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var nextAt time.Time
	if err := pool.QueryRow(context.Background(), `select next_at from outbox where kind = 'ready_merge'`).Scan(&nextAt); err != nil {
		t.Fatalf("read rate-limited row: %v", err)
	}
	if want := now.Add(120 * time.Second); !nextAt.Equal(want) {
		t.Fatalf("HTTP rate-limited READY merge next_at = %s, want GitHub's retry-after %s", nextAt, want)
	}
}
