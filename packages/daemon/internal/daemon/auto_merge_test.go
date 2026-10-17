package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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
		name string
		seed readyMergeSeed
		want int
	}{
		{"awaiting its merge at the READY head", awaitingREADY, 0},
		{"awaiting its merge, the head past READY", awaitingREADY.with(func(s *readyMergeSeed) { s.ready = "older-head" }), 1},
		{"back in implementing", awaitingREADY.with(func(s *readyMergeSeed) { s.at = phase.Implementing }), 1},
		{"awaiting a person's merge at the READY head", awaitingREADY.with(func(s *readyMergeSeed) { s.mergeNote = byHand }), 1},
		{"awaiting its merge at the READY head in a lingering tree", awaitingREADY.with(func(s *readyMergeSeed) { s.lingers = true }), 1},
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
				_, _ = w.Write([]byte(armedPullJSON("ready-head", disables == 0)))
			}))
			t.Cleanup(server.Close)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", tc.seed)
			runner := mergeRunner(pool, records, server.URL, quietLogger(), time.Now)
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
		_, _ = w.Write([]byte(armedPullJSON("ready-head", true)))
	}))
	t.Cleanup(server.Close)
	runner := mergeRunner(pool, records, server.URL, quietLogger(), time.Now)
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
// exact READY head on an open pull request. A READY a person merges, a READY the issue has left, a
// tree that lingers, and a pull request the record holds closed are stale; GitHub's expected-head
// check guards the head between the row's read and its mutation.
func TestAREADYMergeRowSubmitsOnlyTheAwaitedREADYHead(t *testing.T) {
	const byHand = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	for _, tc := range []struct {
		name   string
		seed   readyMergeSeed
		merges int
	}{
		{"awaiting its merge at the READY head", awaitingREADY, 1},
		{"awaiting another READY head", awaitingREADY.with(func(s *readyMergeSeed) { s.ready = "older-head" }), 0},
		{"back in implementing", awaitingREADY.with(func(s *readyMergeSeed) { s.at = phase.Implementing }), 0},
		{"awaiting a person's merge", awaitingREADY.with(func(s *readyMergeSeed) { s.mergeNote = byHand }), 0},
		{"awaiting its merge in a lingering tree", awaitingREADY.with(func(s *readyMergeSeed) { s.lingers = true }), 0},
		{"awaiting its merge on a pull request the record holds closed", awaitingREADY.with(func(s *readyMergeSeed) { s.pr = record.PullRequestClosed }), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, mergedAnswer)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", tc.seed)
			runner := mergeRunner(pool, records, github.url, quietLogger(), time.Now)
			if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())); err != nil {
				t.Fatalf("execute the READY merge: %v", err)
			}
			if merges, bearers := github.merged(); merges != tc.merges {
				t.Fatalf("merges %d, want %d", merges, tc.merges)
			} else {
				for _, bearer := range bearers {
					if bearer != "Bearer implement-app-token-for-acme" {
						t.Fatalf("a GitHub call carried %q, want the implement App's token", bearer)
					}
				}
			}
		})
	}
}

// GitHub refusing a ready_merge row's submission where a retry would meet the same refusal (squash
// merges disabled, a permission the App lacks) fails loudly: the issue's architect is told once,
// with GitHub's message, and the row ends rather than retrying forever. A transient failure
// (GitHub's server error) tells nobody and keeps the row on its backoff.
func TestAPermanentREADYMergeRefusalTellsTheArchitectOnce(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	for _, tc := range []struct {
		name           string
		answer         func(http.ResponseWriter)
		ready, notices int
		reasonMentions []string
	}{
		{"refused for good", refusedAnswer(squash), 0, 1, []string{squash, "#42"}},
		{"a server error", func(w http.ResponseWriter) { http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway) }, 1, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, tc.answer)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now()))
			if err := mergeRunner(pool, records, github.url, quietLogger(), time.Now).RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			reasons := assertMergeRows(t, pool, tc.ready, "merge-refused", tc.notices)
			for _, mention := range tc.reasonMentions {
				if !strings.Contains(reasons[0], mention) {
					t.Fatalf("merge-refused reason %q, want it to name %q", reasons[0], mention)
				}
			}
		})
	}
}

// A permanent ready_merge refusal writes its architect notice and finishes the row in one
// transaction. When FinishLeasedOutbox loses its connection, the notice rolls back with it and the
// row stays leased; once its lease expires, the row runs again and leaves exactly one notice and no
// ready_merge row. Writing the notice first and finishing later would send it twice.
func TestARetriedPermanentREADYMergeRefusalPostsOneNotice(t *testing.T) {
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, refusedAnswer("Squash merges are not allowed on this repository."))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	flaky := &flakyOutboxStore{Store: records, finishFailures: 1}
	clock := now
	runner := mergeRunner(pool, flaky, github.url, quietLogger(), func() time.Time { return clock })

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	if got := flaky.failedFinishes(); got != 1 {
		t.Fatalf("failed finishes = %d, want the injected one", got)
	}

	clock = clock.Add(outboxLease + time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("retried RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 0, "merge-refused", 1)
}

// When a slow worker's lease expires, another worker owns the ready_merge row. The stale worker's
// permanent refusal rolls its notice back with its finish, which no longer holds the lease; only
// the worker holding the lease leaves the one architect notice.
func TestOnlyTheOwningREADYMergeLeasePostsAPermanentRefusal(t *testing.T) {
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, refusedAnswer("Squash merges are not allowed on this repository."))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))

	if err := mergeRunner(pool, leaseStealer{records}, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
		t.Fatalf("stale RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)

	later := now.Add(3 * outboxLease)
	if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return later }).RunOnce(context.Background()); err != nil {
		t.Fatalf("owning RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 0, "merge-refused", 1)
}

// A ready_merge row only GitHub's rules hold back (a required check still running or a required
// review, BLOCKED; GitHub still computing, UNKNOWN; an arm or merge-queue entry at READY's head)
// waits routinely, logged at debug however long it waits, so a person supplying the review GitHub
// requires sees no warning flood. One waiting on a branch update (BEHIND) or a draft (DRAFT) waits
// on someone, so it warns once it has waited long.
func TestAWaitingREADYMergeWarnsOnlyWhenItWaitsOnSomeone(t *testing.T) {
	for state, level := range map[string]string{"BLOCKED": "DEBUG", "UNKNOWN": "DEBUG", "BEHIND": "WARN", "DRAFT": "WARN", "armed": "DEBUG"} {
		t.Run(state, func(t *testing.T) {
			url := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", state) }, mergedAnswer).url
			if state == "armed" {
				url = newArmableGitHub(t).url
			}
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())
			row.Attempts = waitWarnAttempts
			enqueueOutbox(t, pool, records, row)
			var logged bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
			if err := mergeRunner(pool, records, url, log, time.Now).RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			assertMergeRows(t, pool, 1, "merge-refused", 0)
			var levels []string
			for line := range strings.Lines(logged.String()) {
				var entry struct{ Level, Kind string }
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("decode log line %q: %v", line, err)
				}
				if entry.Kind == string(record.OutboxKindReadyMerge) {
					levels = append(levels, entry.Level)
				}
			}
			if len(levels) != 1 || levels[0] != level {
				t.Fatalf("the waiting row logged at %v, want one line at %s", levels, level)
			}
		})
	}
}

// A ready_merge row ends as stale only on the record's word. While the record still awaits READY's
// head on an open pull request, GitHub showing the pull request closed or at another head is a
// webhook the record has not applied yet, or a GitHub read behind the record: the row waits and
// decides again. A reopen at READY's head takes the row back from any worker that read the record
// before it, so it is never lost: one landing while the row reads the close, however long the row
// waited before, or between the row's record read and its finish, leaves the merge submitted on the
// next run. A close the record has applied ends the row quietly, the architect already told of the
// close. The bound is GitHub's own clock, never the row's retries: a disagreement ends the row only
// once GitHub's last change to the pull request has stood longer than ghautomerge.LagBound, far
// longer than a webhook takes, and then tells the architect once (merge-stalled). So one
// disagreement after a long routine wait never ends it.
func TestAREADYMergeRowWaitsForGitHubAndTheRecordToAgree(t *testing.T) {
	for _, waited := range []int{0, waitWarnAttempts} {
		t.Run(fmt.Sprintf("a reopen lands while a row that waited %d times reads the close", waited), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY.with(func(s *readyMergeSeed) { s.pr = record.PullRequestClosed }))
			github := newMergeGitHub(t, func(read int) string {
				if read > 1 {
					return pullJSON("OPEN", "ready-head", "CLEAN")
				}
				// The reopen's transaction commits as GitHub answers the row's read of the close.
				reopen(t, pool, records)
				return pullJSON("CLOSED", "ready-head", "UNKNOWN")
			}, mergedAnswer)
			now := time.Now().UTC()
			row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
			row.Attempts = waited
			enqueueOutbox(t, pool, records, row)
			clock := now
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("first RunOnce: %v", err)
			}
			assertMergeRows(t, pool, 1, "merge-stalled", 0)
			clock = clock.Add(time.Minute)
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("second RunOnce: %v", err)
			}
			assertMergeRows(t, pool, 0, "merge-stalled", 0)
			if merges, _ := github.merged(); merges != 1 {
				t.Fatalf("merges %d, want READY's merge submitted once the reopen reached both", merges)
			}
		})
	}
	t.Run("a reopen lands between the row's record read and its finish", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY.with(func(s *readyMergeSeed) { s.pr = record.PullRequestClosed }))
		github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, mergedAnswer)
		now := time.Now().UTC()
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
		clock := now
		reopening := &reopensBeforeFinish{Store: records, t: t, pool: pool}
		if err := mergeRunner(pool, reopening, github.url, quietLogger(), func() time.Time { return clock }).RunOnce(context.Background()); err != nil {
			t.Fatalf("first RunOnce: %v", err)
		}
		if !reopening.reopened {
			t.Fatal("the row finished without the reopen landing first")
		}
		assertMergeRows(t, pool, 1, "merge-stalled", 0)
		clock = clock.Add(time.Minute)
		if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock }).RunOnce(context.Background()); err != nil {
			t.Fatalf("second RunOnce: %v", err)
		}
		assertMergeRows(t, pool, 0, "merge-stalled", 0)
		if merges, _ := github.merged(); merges != 1 {
			t.Fatalf("merges %d, want READY's merge submitted after the reopen", merges)
		}
	})
	t.Run("one disagreement after a long routine wait", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		now := time.Now().UTC()
		clock := now
		github := newMergeGitHub(t, func(read int) string {
			if read <= waitWarnAttempts {
				return pullJSON("OPEN", "ready-head", "BLOCKED")
			}
			// A close GitHub made just now, whose webhook has not reached the record.
			return pullJSONAt("CLOSED", "ready-head", "UNKNOWN", clock)
		}, mergedAnswer)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
		runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
		for run := 0; run <= waitWarnAttempts; run++ {
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce %d: %v", run, err)
			}
			clock = clock.Add(2 * time.Minute)
		}
		assertMergeRows(t, pool, 1, "merge-stalled", 0)
	})
	t.Run("GitHub and the record both hold the pull request closed", func(t *testing.T) {
		github := newMergeGitHub(t, func(int) string { return pullJSON("CLOSED", "ready-head", "UNKNOWN") }, mergedAnswer)
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY.with(func(s *readyMergeSeed) { s.pr = record.PullRequestClosed }))
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now()))
		if err := mergeRunner(pool, records, github.url, quietLogger(), time.Now).RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		assertMergeRows(t, pool, 0, "merge-stalled", 0)
	})
	t.Run("a disagreement that has stood on GitHub past the bound", func(t *testing.T) {
		now := time.Now().UTC()
		closed := now.Add(-9 * time.Minute)
		github := newMergeGitHub(t, func(int) string { return pullJSONAt("CLOSED", "ready-head", "UNKNOWN", closed) }, mergedAnswer)
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		// However many times the row tried before, only GitHub's clock bounds the disagreement.
		row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
		row.Attempts = 100
		enqueueOutbox(t, pool, records, row)
		clock := now
		runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("first RunOnce: %v", err)
		}
		assertMergeRows(t, pool, 1, "merge-stalled", 0)
		clock = clock.Add(2 * time.Minute)
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("second RunOnce: %v", err)
		}
		reasons := assertMergeRows(t, pool, 0, "merge-stalled", 1)
		for _, want := range []string{"#42 CLOSED", "ready-head", closed.Format(time.RFC3339)} {
			if !strings.Contains(reasons[0], want) {
				t.Fatalf("merge-stalled reason %q, want it to name %q", reasons[0], want)
			}
		}
	})
}

// The delayed disarm: an auto_merge_disable row sent its disable while the issue was still merging,
// and its runner stopped before GitHub answered. READY has been accepted at that head since, so the
// replacement runner's disarm keeps the arm it finds, and the READY merge row finds the pull request
// armed. The outstanding disable then lands. An arm is not the merge: the row waits on it, and once
// the arm is gone while the record still awaits READY it arms READY's head again, ending only once
// GitHub reports the pull request merged at that head.
func TestADelayedDisarmNeverLosesTheREADYMerge(t *testing.T) {
	github := newArmableGitHub(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	run := func(step string) {
		t.Helper()
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %s: %v", step, err)
		}
		clock = clock.Add(time.Minute)
	}
	run("with READY's arm standing")
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	if disables, arms := github.counts(); disables != 0 || arms != 0 {
		t.Fatalf("disables %d and arms %d while READY's arm stands, want the arm kept and none sent", disables, arms)
	}
	github.remove(manuallyDisabledEvent(implementApp))
	run("after the earlier disable lands")
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("arms %d after the arm disappeared, want READY's head armed again", arms)
	}
	github.set(func(g *armableGitHub) { g.state = "MERGED" })
	run("after GitHub merges READY's head")
	assertMergeRows(t, pool, 0, "merge-refused", 0)
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("arms %d after GitHub merged READY's head, want no other arm", arms)
	}
}

// A merge the merge queue fails is the transition back to implementing, never a second submission
// (LEGION-800): once the row has seen READY's head in the queue, GitHub taking it out for
// failed_checks, its merge_group run red on the commit the queue built, sends the issue from
// awaiting_merge to implementing as a red head does. The implementer's task and the architect's
// checks-red name the failing run, the READY's withdrawal is posted with its disarm, and the row
// ends without enqueueing the head again, which would rebuild every entry queued behind it and fail
// the same way.
func TestAMergeTheQueueFailsSendsTheIssueBackToImplementing(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	clock := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION"}, quietLogger())}
	run := func(step string) {
		t.Helper()
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %s: %v", step, err)
		}
		clock = clock.Add(time.Minute)
	}
	run("READY's submission")
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("enables %d, want READY's head enqueued once", arms)
	}
	github.remove(dequeuedEvent("failed_checks", "github-merge-queue", failedMergeGroup))
	run("the retry after the merge queue failed the merge")
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("enables %d after the merge queue failed READY's merge, want no second submission", arms)
	}
	assertMergeRows(t, pool, 0, "merge-removed", 0)
	var at string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(phase.Implementing) {
		t.Fatalf("LEGION-208 in %q (%v), want implementing", at, err)
	}
	var task string
	if err := pool.QueryRow(context.Background(), `select payload->>'task' from outbox where kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'implementer'`).Scan(&task); err != nil {
		t.Fatalf("read the implementer's start: %v", err)
	}
	for _, want := range []string{failedMergeGroupRun, "integration", "failed_checks", "#42"} {
		if !strings.Contains(task, want) {
			t.Fatalf("the implementer's task %q, want it to name %q", task, want)
		}
	}
	if strings.Contains(task, "lint") {
		t.Fatalf("the implementer's task %q names the merge_group job that passed", task)
	}
	var reason string
	if err := pool.QueryRow(context.Background(), `select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'`).Scan(&reason); err != nil || !strings.Contains(reason, failedMergeGroupRun) {
		t.Fatalf("the architect's checks-red reason %q (%v), want it to name the failing run", reason, err)
	}
	var disarms int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'auto_merge_disable'`).Scan(&disarms); err != nil || disarms != 1 {
		t.Fatalf("auto_merge_disable rows %d (%v), want the withdrawal's one", disarms, err)
	}
}

// A merge failure whose fact the workflow already applied (intake.ApplyFact's Duplicate) changes
// nothing, and the row's log says so rather than that the issue goes back to implementing.
func TestAMergeFailureAlreadyAppliedIsNotLoggedAsSendingTheIssueBack(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	clock := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
	var logged bytes.Buffer
	runner := mergeRunner(pool, records, github.url, slog.New(slog.NewJSONHandler(&logged, nil)), func() time.Time { return clock })
	runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION"}, quietLogger())}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce READY's submission: %v", err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		_, err := records.MarkProcessed(context.Background(), tx, "outbox", "merge-failed:LEGION-208:ready-head:RFMQE_1")
		return err
	}); err != nil {
		t.Fatalf("record the merge failure's fact as applied: %v", err)
	}
	github.remove(dequeuedEvent("failed_checks", "github-merge-queue", failedMergeGroup))
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce after the merge queue failed the merge: %v", err)
	}
	var at string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(phase.AwaitingMerge) {
		t.Fatalf("LEGION-208 in %q (%v), want awaiting_merge: the fact was already applied", at, err)
	}
	told := false
	for line := range strings.Lines(logged.String()) {
		var entry struct{ Msg string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if strings.Contains(entry.Msg, "goes back to implementing") {
			t.Fatalf("the row logged %q for a merge failure already applied", entry.Msg)
		}
		told = told || strings.Contains(entry.Msg, "already applied")
	}
	if !told {
		t.Fatalf("the row's log %s, want a line saying the merge failure was already applied", logged.String())
	}
}

// Any other removal of an arm or queue entry the row saw at READY's head (a person dequeuing the
// pull request, a person turning its auto-merge off) is not the row's to undo (LEGION-800): the row
// ends, submitting nothing more, and tells the architect once (merge-removed), naming what was
// removed, GitHub's reason and who removed it. The issue stays in awaiting_merge.
func TestARemovalByAPersonEndsTheREADYMergeAndTellsTheArchitect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		github  func(*testing.T) *armableGitHub
		removal string
		names   []string
	}{
		{"a person dequeues it", func(t *testing.T) *armableGitHub { return newQueueGitHub(t, "CLEAN") },
			dequeuedEvent("manual", "a-maintainer", "null"), []string{"#42", "merge queue", "manual", "a-maintainer", "ready-head"}},
		{"a person turns its auto-merge off", func(t *testing.T) *armableGitHub {
			g := newArmableGitHub(t)
			g.set(func(g *armableGitHub) { g.armed, g.timeline = false, nil })
			return g
		}, manuallyDisabledEvent("a-maintainer"), []string{"#42", "auto-merge", "manually_disabled", "a-maintainer", "ready-head"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := tc.github(t)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			clock := time.Now().UTC()
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			run := func(step string) {
				t.Helper()
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("RunOnce %s: %v", step, err)
				}
				clock = clock.Add(time.Minute)
			}
			run("READY's submission")
			github.remove(tc.removal)
			run("the retry after the removal")
			run("a later drain")
			if _, arms := github.counts(); arms != 1 {
				t.Fatalf("enables %d after a person's removal, want READY's one submission and no other", arms)
			}
			reasons := assertMergeRows(t, pool, 0, "merge-removed", 1)
			for _, want := range tc.names {
				if !strings.Contains(reasons[0], want) {
					t.Fatalf("merge-removed reason %q, want it to name %q", reasons[0], want)
				}
			}
			var at string
			if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(phase.AwaitingMerge) {
				t.Fatalf("LEGION-208 in %q (%v), want awaiting_merge", at, err)
			}
		})
	}
}

// On a base with a merge queue, enabling auto-merge before GitHub reports the pull request mergeable
// arms it rather than enqueueing it, and an armed pull request fails a required check that judges
// the arm (LEGION-800). The row waits instead, quietly at debug however long, while a required check
// runs (BLOCKED), and enters the queue once GitHub reports the pull request mergeable: auto-merge is
// never armed.
func TestAREADYMergeOnAMergeQueueBaseNeverArmsAutoMerge(t *testing.T) {
	github := newQueueGitHub(t, "BLOCKED")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	clock := time.Now().UTC()
	row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock)
	row.Attempts = waitWarnAttempts
	enqueueOutbox(t, pool, records, row)
	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runner := mergeRunner(pool, records, github.url, log, func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce with a required check running: %v", err)
	}
	if _, arms := github.counts(); arms != 0 {
		t.Fatalf("enables %d while a required check runs on a merge-queue base, want none: GitHub would arm auto-merge", arms)
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	var levels []string
	for line := range strings.Lines(logged.String()) {
		var entry struct{ Level, Kind string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry.Kind == string(record.OutboxKindReadyMerge) {
			levels = append(levels, entry.Level)
		}
	}
	if len(levels) != 1 || levels[0] != "DEBUG" {
		t.Fatalf("the waiting row logged at %v, want one line at DEBUG", levels)
	}
	github.set(func(g *armableGitHub) { g.mergeState = "CLEAN" })
	clock = clock.Add(time.Hour)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce once the required checks passed: %v", err)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if github.arms != 1 || !github.queued || github.armed {
		t.Fatalf("enables %d, queued %t, armed %t once GitHub reports the pull request mergeable; want one enable that enqueues it and never an arm", github.arms, github.queued, github.armed)
	}
}

// A READY merge row waiting on an arm at READY's head ends once the merged pull request's webhook
// moves the record past awaiting its merge, even while GitHub's read still shows the arm.
func TestAREADYMergeRowWaitingOnAnArmEndsOnceTheMergeMovesTheRecord(t *testing.T) {
	github := newArmableGitHub(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	if _, err := pool.Exec(context.Background(), "update issues set phase = 'production_check' where key = 'LEGION-208'"); err != nil {
		t.Fatalf("apply the merge to the record: %v", err)
	}
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 0, "merge-refused", 0)
	if _, arms := github.counts(); arms != 0 {
		t.Fatalf("arms %d, want none sent for an arm that stood", arms)
	}
}

// armableGitHub is GitHub's GraphQL API for acme/widgets#42 at ready-head as a ready_merge row and a
// disarm read and change it: the pull request's state and merge state, whether its base requires a
// merge queue (its repository allowing auto-merge either way), whether it is armed for auto-merge or
// sits in the merge queue, and its timeline of arms and removals. An enablePullRequestAutoMerge at
// ready-head enqueues it on a merge-queue base GitHub reports mergeable now and arms auto-merge
// otherwise, as GitHub does, and a disablePullRequestAutoMerge turns auto-merge off; each lands on
// the timeline as the implement App's (implementApp), and both are counted (arms counts every
// enable). A removal outside the daemon is remove's.
type armableGitHub struct {
	url               string
	mu                sync.Mutex
	state, mergeState string
	queueRequired     bool
	armed, queued     bool
	timeline          []string
	disables, arms    int
}

// implementApp is the implement App's login as GitHub's timeline names an actor: its bot login
// without the [bot] suffix that GraphQL's viewer gives it.
const implementApp = "legion-implementer"

// newArmableGitHub is armableGitHub for a repository that allows auto-merge and has no merge queue,
// the pull request open, armed and BLOCKED on its required checks.
func newArmableGitHub(t *testing.T) *armableGitHub {
	t.Helper()
	return serveArmable(t, &armableGitHub{state: "OPEN", mergeState: "BLOCKED", armed: true, timeline: []string{timelineEvent("AutoMergeEnabledEvent")}})
}

// newQueueGitHub is armableGitHub for a base that requires a merge queue, the pull request open in
// mergeState with nothing armed.
func newQueueGitHub(t *testing.T, mergeState string) *armableGitHub {
	t.Helper()
	return serveArmable(t, &armableGitHub{state: "OPEN", mergeState: mergeState, queueRequired: true})
}

func serveArmable(t *testing.T, g *armableGitHub) *armableGitHub {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(string(body), "disablePullRequestAutoMerge"):
			g.disables++
			g.armed = false
			g.timeline = append(g.timeline, manuallyDisabledEvent(implementApp))
			_, _ = io.WriteString(w, `{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`)
		case strings.Contains(string(body), "enablePullRequestAutoMerge"):
			if !strings.Contains(string(body), `"head":"ready-head"`) {
				t.Errorf("arm request %s, want READY's expected head", body)
			}
			g.arms++
			if g.queueRequired && (g.mergeState == "CLEAN" || g.mergeState == "HAS_HOOKS" || g.mergeState == "UNSTABLE") {
				g.queued = true
				g.timeline = append(g.timeline, timelineEvent("AddedToMergeQueueEvent"))
			} else {
				g.armed = true
				g.timeline = append(g.timeline, timelineEvent("AutoMergeEnabledEvent"))
			}
			_, _ = io.WriteString(w, `{"data":{}}`)
		case strings.Contains(string(body), "mergePullRequest"), strings.Contains(string(body), "dequeuePullRequest"):
			t.Errorf("request %s, want only an enable: GitHub merges an armed or queued pull request itself", body)
		case strings.Contains(string(body), "timelineItems"):
			_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":%q},"repository":{"pullRequest":{"timelineItems":{"nodes":[%s]}}}}}`,
				implementApp+"[bot]", strings.Join(g.timeline, ","))
		default:
			autoMerge, entry := "null", "null"
			if g.armed {
				autoMerge = `{"enabledAt":"2026-10-11T00:00:00Z"}`
			}
			if g.queued {
				entry = `{"enqueuedAt":"2026-10-11T00:05:00Z"}`
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"autoMergeAllowed":true,"pullRequest":{"id":"PR_42","state":%q,"headRefOid":"ready-head","mergeStateStatus":%q,"updatedAt":%q,"isMergeQueueEnabled":%t,"autoMergeRequest":%s,"mergeQueueEntry":%s}}}}`,
				g.state, g.mergeState, time.Now().UTC().Format(time.RFC3339), g.queueRequired, autoMerge, entry)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// set changes GitHub's state, as a mutation landing outside the daemon or GitHub's own merge does.
func (g *armableGitHub) set(change func(*armableGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

// remove takes the pull request out of the merge queue and turns its auto-merge off without a merge,
// as someone other than the daemon's submission does, leaving event, GitHub's record of it, on the
// timeline.
func (g *armableGitHub) remove(event string) {
	g.set(func(g *armableGitHub) {
		g.armed, g.queued = false, false
		g.timeline = append(g.timeline, event)
	})
}

func (g *armableGitHub) counts() (disables, arms int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.disables, g.arms
}

// timelineEvent is a timeline item of kind (AddedToMergeQueueEvent or AutoMergeEnabledEvent) as
// GitHub lists it.
func timelineEvent(kind string) string {
	return fmt.Sprintf(`{"__typename":%q,"createdAt":"2026-10-16T05:00:00Z"}`, kind)
}

// manuallyDisabledEvent is GitHub's record of the pull request's auto-merge turned off by actor, a
// person or the implement App's disarm: reason code manually_disabled, with GitHub's sentence for it.
func manuallyDisabledEvent(actor string) string {
	return fmt.Sprintf(`{"__typename":"AutoMergeDisabledEvent","id":"AMDE_1","createdAt":"2026-10-16T05:10:00Z","reason":"Manually disabled by user","reasonCode":"manually_disabled","actor":{"login":%q}}`,
		actor)
}

// dequeuedEvent is GitHub's record of the pull request taken out of the merge queue by actor for
// reason, the merge group commit the queue built for it (failedMergeGroup) or null.
func dequeuedEvent(reason, actor, commit string) string {
	return fmt.Sprintf(`{"__typename":"RemovedFromMergeQueueEvent","id":"RFMQE_1","createdAt":"2026-10-16T05:10:00Z","reason":%q,"actor":{"login":%q},"beforeCommit":%s}`,
		reason, actor, commit)
}

// failedMergeGroupRun is the URL of the merge_group run that failed on the commit the merge queue
// built for #42 (failedMergeGroup).
const failedMergeGroupRun = "https://github.com/acme/widgets/actions/runs/7"

// failedMergeGroup is the commit the merge queue built for #42, on which its merge_group run's
// integration job failed and its lint job passed.
const failedMergeGroup = `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
	`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedMergeGroupRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedMergeGroupRun + `"}}},` +
	`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedMergeGroupRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedMergeGroupRun + `"}}}]}}}`

// A reopen at READY's head that lands while a worker is recording GitHub's permanent refusal takes
// the row from that worker: the worker's notice rolls back with its finish, and the run that holds
// the row afterwards tells the architect, so the refusal is told once. The worker whose finish
// rolled back logs the refusal it could not record beside its finish's failure.
func TestAReopenDuringAPermanentRefusalTellsTheArchitectOnce(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, refusedAnswer(squash))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	clock := now
	reopening := &reopensBeforeFinish{Store: records, t: t, pool: pool}
	var logged bytes.Buffer
	if err := mergeRunner(pool, reopening, github.url, slog.New(slog.NewJSONHandler(&logged, nil)), func() time.Time { return clock }).RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if !reopening.reopened {
		t.Fatal("the refusal finished without the reopen landing first")
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	logsCause := false
	for line := range strings.Lines(logged.String()) {
		var entry struct{ Level, Msg, Cause string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		logsCause = logsCause || entry.Level == "ERROR" && strings.Contains(entry.Msg, "finish not recorded") && strings.Contains(entry.Cause, squash)
	}
	if !logsCause {
		t.Fatalf("the worker whose finish rolled back logged %s, want its failed finish logged at ERROR with the refusal as its cause", logged.String())
	}
	clock = clock.Add(time.Minute)
	if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock }).RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 0, "merge-refused", 1)
}

// reopen applies a reopen of LEGION-208's pull request at READY's head as the workflow does: the
// record holds it open, and READY's merge is queued again (queueReadyMerge).
func reopen(t *testing.T, pool *pgxpool.Pool, records record.Store) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutPullRequest(context.Background(), tx, readyPullRequest("LEGION-208", record.PullRequestOpen)); err != nil {
			return err
		}
		row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())
		if err != nil {
			return err
		}
		return records.Enqueue(context.Background(), tx, row)
	}); err != nil {
		t.Errorf("apply the reopen: %v", err)
	}
}

// reopensBeforeFinish is a record store whose first finish of a row, plain or lease-owned, lets a
// reopen at READY's head (reopen) commit first, as one landing after the row's worker read the
// record and before that worker finishes the row.
type reopensBeforeFinish struct {
	record.Store
	t        *testing.T
	pool     *pgxpool.Pool
	reopened bool
}

func (s *reopensBeforeFinish) FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	s.reopenOnce()
	return s.Store.FinishOutbox(ctx, tx, id, leaseToken)
}

func (s *reopensBeforeFinish) FinishLeasedOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	s.reopenOnce()
	return s.Store.FinishLeasedOutbox(ctx, tx, id, leaseToken)
}

func (s *reopensBeforeFinish) reopenOnce() {
	if !s.reopened {
		s.reopened = true
		reopen(s.t, s.pool, s.Store)
	}
}

// A rate-limited ready_merge waits as long as GitHub asks, longer than the outbox's own backoff,
// whether GitHub answers the GraphQL mutation with RATE_LIMITED and 200 or with a 429.
func TestARateLimitedREADYMergeWaitsGitHubsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter)
		wait   time.Duration
	}{
		{"GraphQL's RATE_LIMITED", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "180")
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		}, 180 * time.Second},
		{"a 429", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "120")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusTooManyRequests)
		}, 120 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, tc.answer)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			now := time.Date(2026, 10, 16, 5, 0, 0, 0, time.UTC)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
			if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if nextAt, _ := outboxRowState(t, pool, "LEGION-208"); !nextAt.Equal(now.Add(tc.wait)) {
				t.Fatalf("rate-limited READY merge next_at = %s, want GitHub's retry-after %s", nextAt, now.Add(tc.wait))
			}
		})
	}
}

// A GitHub rate limit defers the rows after it only where they would spend the same budget.
// The implement App's REST core budget (an issue branch's create) and its GraphQL budget (a disarm,
// a READY merge) are separate, so a REST core limit never holds back a disarm, the effect that keeps
// an unreviewed head from merging, nor a GraphQL limit an issue branch. GitHub's secondary limit
// (a Retry-After) covers every call the token makes. A deferred row is not run in the batch at all,
// and its last error names the row whose limit deferred it.
func TestARateLimitDefersOnlyTheRowsThatSpendItsBudget(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	primary := func(resource string) map[string]string {
		return map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset, "X-RateLimit-Resource": resource}
	}
	for _, tc := range []struct {
		name          string
		first, second record.OutboxPayload
		restLimit     map[string]string
		mergeLimit    map[string]string
		deferred      bool
		wait          time.Duration
	}{
		{name: "a REST core limit leaves a disarm behind it to run", first: record.IssueBranch{Generation: 1}, second: record.AutoMergeDisable{Number: 43},
			restLimit: primary("core")},
		{name: "a GraphQL limit defers a disarm behind it", first: record.ReadyMerge{Number: 42, Head: "ready-head"}, second: record.AutoMergeDisable{Number: 43},
			mergeLimit: primary("graphql"), deferred: true, wait: 9 * time.Minute},
		{name: "a GraphQL limit leaves an issue branch behind it to run", first: record.ReadyMerge{Number: 42, Head: "ready-head"}, second: record.IssueBranch{Generation: 1},
			mergeLimit: primary("graphql")},
		{name: "a secondary limit defers an issue branch behind it", first: record.ReadyMerge{Number: 42, Head: "ready-head"}, second: record.IssueBranch{Generation: 1},
			mergeLimit: map[string]string{"Retry-After": "180"}, deferred: true, wait: 180 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newBudgetGitHub(t, tc.restLimit, tc.mergeLimit)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: "LEGION-209", Title: "Disarmed", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
			putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-210", Project: "LEGION", Tree: "LEGION-210", Title: "Branch", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W"})
			issueOf := func(payload record.OutboxPayload) string {
				switch payload.(type) {
				case record.ReadyMerge:
					return "LEGION-208"
				case record.AutoMergeDisable:
					return "LEGION-209"
				default:
					return "LEGION-210"
				}
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issueOf(tc.first), tc.first, now))
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issueOf(tc.second), tc.second, now))
			var firstID int64
			if err := pool.QueryRow(context.Background(), "select id from outbox where issue = $1", issueOf(tc.first)).Scan(&firstID); err != nil {
				t.Fatalf("read the first row's id: %v", err)
			}
			if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			second := issueOf(tc.second)
			ran := github.calledFor(second)
			if ran == tc.deferred {
				t.Fatalf("GitHub calls for %s's row: %t, want deferred %t", second, ran, tc.deferred)
			}
			if !tc.deferred {
				return
			}
			nextAt, lastError := outboxRowState(t, pool, second)
			if nextAt.Before(now.Add(tc.wait)) || !strings.Contains(lastError, fmt.Sprintf("row %d", firstID)) {
				t.Fatalf("the deferred row waits until %s with last error %q; want no earlier than %s, naming row %d", nextAt, lastError, now.Add(tc.wait), firstID)
			}
		})
	}
}

// The REST core and GraphQL budgets are exhausted independently, so the runner remembers each limit
// its rows met: a REST core limit met after a GraphQL one leaves the GraphQL limit standing, and a
// disarm behind both is deferred rather than sent against an exhausted budget.
func TestARateLimitedBatchRemembersEveryBudgetItExhausted(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	primary := func(resource string) map[string]string {
		return map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset, "X-RateLimit-Resource": resource}
	}
	github := newBudgetGitHub(t, primary("core"), primary("graphql"))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: "LEGION-209", Title: "Disarmed", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-210", Project: "LEGION", Tree: "LEGION-210", Title: "Branch", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W"})
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-210", record.IssueBranch{Generation: 1}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-209", record.AutoMergeDisable{Number: 43}, now))
	if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !github.calledFor("LEGION-208") || !github.calledFor("LEGION-210") {
		t.Fatal("the READY merge and the issue branch, each the first of its budget, did not run")
	}
	if github.calledFor("LEGION-209") {
		t.Fatal("the disarm called GraphQL in the batch whose first row met GraphQL's limit")
	}
}

// A GitHub rate limit stands until its deadline, not until its batch ends, and a row that spends
// several limited budgets waits for the latest of their deadlines. A disarm queued after the batch
// that met GitHub's secondary limit is deferred to that limit's end, then runs once it has passed;
// and an issue branch behind a 30-second REST core limit and a 180-second secondary limit waits the
// 180 seconds.
func TestARateLimitStandsUntilTheLatestDeadlineOfTheBudgetsARowSpends(t *testing.T) {
	secondary := map[string]string{"Retry-After": "180"}
	t.Run("across batches", func(t *testing.T) {
		github := newBudgetGitHub(t, nil, secondary)
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: "LEGION-209", Title: "Disarmed", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
		start := time.Now().UTC().Truncate(time.Microsecond)
		clock := start
		runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("the batch that meets the limit: %v", err)
		}
		clock = start.Add(time.Minute)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-209", record.AutoMergeDisable{Number: 43}, clock))
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("the next batch: %v", err)
		}
		if github.calledFor("LEGION-209") {
			t.Fatal("the disarm called GitHub inside the secondary limit a batch before it met")
		}
		if nextAt, _ := outboxRowState(t, pool, "LEGION-209"); !nextAt.Equal(start.Add(180 * time.Second)) {
			t.Fatalf("the deferred disarm is due at %s, want the limit's end %s", nextAt, start.Add(180*time.Second))
		}
		// The READY merge no longer has a READY to submit, so its next run calls no mutation.
		if _, err := pool.Exec(context.Background(), "update issues set phase = 'implementing' where key = 'LEGION-208'"); err != nil {
			t.Fatalf("withdraw the READY: %v", err)
		}
		clock = start.Add(181 * time.Second)
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("the batch after the limit: %v", err)
		}
		if !github.calledFor("LEGION-209") {
			t.Fatal("the disarm did not run once the limit had passed")
		}
	})
	t.Run("overlapping REST core and secondary limits", func(t *testing.T) {
		core := map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10), "X-RateLimit-Resource": "core"}
		github := newBudgetGitHub(t, core, secondary)
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		for _, key := range []string{"LEGION-210", "LEGION-211"} {
			putOutboxIssue(t, pool, records, record.Issue{Key: key, Project: "LEGION", Tree: key, Title: "Branch", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W"})
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-210", record.IssueBranch{Generation: 1}, now))
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-211", record.IssueBranch{Generation: 1}, now))
		if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if nextAt, _ := outboxRowState(t, pool, "LEGION-211"); !nextAt.Equal(now.Add(180 * time.Second)) {
			t.Fatalf("the issue branch behind both limits is due at %s, want the later one's end %s", nextAt, now.Add(180*time.Second))
		}
	})
}

// budgetGitHub is GitHub for TestARateLimitDefersOnlyTheRowsThatSpendItsBudget: acme/widgets' REST
// API, which answers an issue branch's read of main with restLimit's headers and a 403 when it is
// set, and GraphQL, where #42 is a READY merge's pull request, mergeable now, whose merge GitHub
// rate-limits with mergeLimit's headers when it is set, and #43 a pull request armed at another
// head, whose disarm GitHub accepts. It records which issue's row called it.
type budgetGitHub struct {
	url    string
	mu     sync.Mutex
	called map[string]bool
}

func newBudgetGitHub(t *testing.T, restLimit, mergeLimit map[string]string) *budgetGitHub {
	t.Helper()
	g := &budgetGitHub{called: map[string]bool{}}
	disabled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/git/") {
			g.called["LEGION-210"] = true
			switch {
			case restLimit != nil:
				for name, value := range restLimit {
					w.Header().Set(name, value)
				}
				http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			case r.Method == http.MethodGet:
				_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+branchBaseCommit+`"}}`)
			default:
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{}`)
			}
			return
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode GraphQL request: %v", err)
		}
		switch {
		case strings.Contains(request.Query, "disablePullRequestAutoMerge"):
			g.called["LEGION-209"] = true
			disabled = true
			_, _ = io.WriteString(w, `{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`)
		case strings.Contains(request.Query, "mergePullRequest"):
			g.called["LEGION-208"] = true
			for name, value := range mergeLimit {
				w.Header().Set(name, value)
			}
			if mergeLimit == nil {
				_, _ = io.WriteString(w, `{"data":{}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
		case request.Variables["number"] == float64(43):
			g.called["LEGION-209"] = true
			_, _ = io.WriteString(w, armedPullJSON("other-head", !disabled))
		default:
			g.called["LEGION-208"] = true
			_, _ = io.WriteString(w, pullJSON("OPEN", "ready-head", "CLEAN"))
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

func (g *budgetGitHub) calledFor(issue string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.called[issue]
}

// mergeGitHub is GitHub's GraphQL API as a ready_merge row calls it: read answers the row's read of
// pull request #42 (given how many reads came before it), and mutation answers its merge or arm. It
// counts the merges sent and records every call's bearer, and fails the test on a merge that names
// another expected head than ready-head.
type mergeGitHub struct {
	url     string
	mu      sync.Mutex
	merges  int
	bearers []string
}

func newMergeGitHub(t *testing.T, read func(int) string, mutation func(http.ResponseWriter)) *mergeGitHub {
	t.Helper()
	g := &mergeGitHub{}
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.bearers = append(g.bearers, r.Header.Get("Authorization"))
		isMerge := strings.Contains(string(body), "mergePullRequest") || strings.Contains(string(body), "enablePullRequestAutoMerge")
		if isMerge {
			g.merges++
			if !strings.Contains(string(body), `"head":"ready-head"`) {
				t.Errorf("merge request %s, want READY's expected head", body)
			}
		} else {
			reads++
		}
		count := reads
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if isMerge {
			mutation(w)
			return
		}
		_, _ = io.WriteString(w, read(count))
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

func (g *mergeGitHub) merged() (int, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.merges, append([]string(nil), g.bearers...)
}

// mergedAnswer is GitHub accepting a merge; refusedAnswer is GitHub refusing it with message, an
// UNPROCESSABLE a retry meets again.
func mergedAnswer(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"data":{}}`) }

func refusedAnswer(message string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_, _ = fmt.Fprintf(w, `{"data":null,"errors":[{"type":"UNPROCESSABLE","message":%q}]}`, message)
	}
}

// pullJSON is ghautomerge's read of acme/widgets#42 as GitHub answers it in a repository that allows
// no auto-merge: the pull request in state at head, in mergeState, with nothing armed, GitHub's last
// change to it made just now (pullJSONAt).
func pullJSON(state, head, mergeState string) string {
	return pullJSONAt(state, head, mergeState, time.Now())
}

// pullJSONAt is pullJSON with GitHub's last change to the pull request (its updatedAt) made at
// updated.
func pullJSONAt(state, head, mergeState string, updated time.Time) string {
	return fmt.Sprintf(`{"data":{"repository":{"autoMergeAllowed":false,"pullRequest":{"id":"PR_42","state":%q,"headRefOid":%q,"mergeStateStatus":%q,"updatedAt":%q,"isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null}}}}`,
		state, head, mergeState, updated.UTC().Format(time.RFC3339))
}

// armedPullJSON is a disarm's read of an open pull request at head, its auto-merge enabled while
// armed.
func armedPullJSON(head string, armed bool) string {
	autoMerge := "null"
	if armed {
		autoMerge = `{"enabledAt":"2026-10-11T00:00:00Z"}`
	}
	return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"id":"PR","state":"OPEN","headRefOid":%q,"autoMergeRequest":%s,"mergeQueueEntry":null}}}}`, head, autoMerge)
}

// readyMergeSeed is the record a ready_merge row of an issue's pull request #42 at ready-head runs
// against: the issue's phase, the merger's READY head and by-hand note, whether the issue's tree
// lingers, and the pull request's recorded state.
type readyMergeSeed struct {
	at        phase.Phase
	ready     string
	mergeNote string
	lingers   bool
	pr        record.PullRequestState
}

// awaitingREADY is the issue awaiting its merge at ready-head, its pull request open.
var awaitingREADY = readyMergeSeed{at: phase.AwaitingMerge, ready: "ready-head", pr: record.PullRequestOpen}

// with is the seed changed by change.
func (s readyMergeSeed) with(change func(*readyMergeSeed)) readyMergeSeed {
	change(&s)
	return s
}

// seedReadyMerge records issue as seed has it.
func seedReadyMerge(t *testing.T, pool *pgxpool.Pool, records record.Store, issue string, seed readyMergeSeed) {
	t.Helper()
	child := record.Issue{Key: issue, Project: "LEGION", Tree: issue, Title: "Workflow", Phase: seed.at, Generation: 1, Status: "retro", Rank: "U"}
	if seed.lingers {
		until, parent := time.Now().Add(time.Hour), "LEGION-207"
		putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-207", Project: "LEGION", Tree: "LEGION-207", Title: "Root", Phase: phase.Done,
			Generation: 1, Status: "done", Rank: "T", LingerUntil: &until})
		child.Tree, child.Parent = "LEGION-207", &parent
	}
	putOutboxIssue(t, pool, records, child)
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: issue, Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: seed.ready, MergeNote: seed.mergeNote}); err != nil {
			return err
		}
		return records.PutPullRequest(context.Background(), tx, readyPullRequest(issue, seed.pr))
	}); err != nil {
		t.Fatalf("seed %s's READY: %v", issue, err)
	}
}

// readyPullRequest is issue's pull request #42 at ready-head, recorded in state.
func readyPullRequest(issue string, state record.PullRequestState) record.PullRequest {
	return record.PullRequest{State: state, Issue: issue, Repo: "acme/widgets", Number: 42, Branch: "legion/" + issue, HeadSHA: "ready-head", Failing: []string{}}
}

// mergeRunner is the outbox the merge tests drive: as the implement App (roleTokens), for
// acme/widgets, whose default branch is main (mainBranch), against GitHub at api, logging to log,
// on the clock now.
func mergeRunner(pool *pgxpool.Pool, records record.Store, api string, log *slog.Logger, now func() time.Time) *outbox {
	return &outbox{log: log, pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
		repo: ghrepo.MustParse("acme/widgets"), githubAPI: api, defaultBranch: mainBranch, now: now}
}

// leaseStealer is a record store whose ClaimRow lets another worker take every row it hands out, as
// a worker too slow for its lease meets: it claims the row again in the same transaction, as of the
// moment its lease expires, so the row it returns holds a stale lease.
type leaseStealer struct{ record.Store }

func (s leaseStealer) ClaimRow(ctx context.Context, tx pgx.Tx, id int64, now time.Time, lease time.Duration) (record.OutboxRow, bool, error) {
	row, ok, err := s.Store.ClaimRow(ctx, tx, id, now, lease)
	if err != nil || !ok {
		return row, ok, err
	}
	if _, _, err := s.Store.ClaimRow(ctx, tx, id, now.Add(lease), lease); err != nil {
		return record.OutboxRow{}, false, err
	}
	return row, true, nil
}

// assertMergeRows checks the outbox holds ready ready_merge rows and notices notices of noticeKind,
// and returns those notices' reasons, oldest first.
func assertMergeRows(t *testing.T, pool *pgxpool.Pool, ready int, noticeKind string, notices int) []string {
	t.Helper()
	var readyRows int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'ready_merge'`).Scan(&readyRows); err != nil {
		t.Fatalf("count ready_merge rows: %v", err)
	}
	rows, err := pool.Query(context.Background(), `select payload->>'role', payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = $1 order by id`, noticeKind)
	if err != nil {
		t.Fatalf("read %s notices: %v", noticeKind, err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var role, reason string
		if err := rows.Scan(&role, &reason); err != nil {
			t.Fatalf("scan a %s notice: %v", noticeKind, err)
		}
		if role != string(claim.RoleArchitect) {
			t.Fatalf("a %s notice for %q, want the architect", noticeKind, role)
		}
		reasons = append(reasons, reason)
	}
	if readyRows != ready || len(reasons) != notices {
		t.Fatalf("ready_merge rows %d and %s notices %q; want %d and %d", readyRows, noticeKind, reasons, ready, notices)
	}
	return reasons
}

// outboxRowState is when issue's one outbox row runs next and the error it last failed with.
func outboxRowState(t *testing.T, pool *pgxpool.Pool, issue string) (time.Time, string) {
	t.Helper()
	var nextAt time.Time
	var lastError string
	if err := pool.QueryRow(context.Background(), `select next_at, last_error from outbox where issue = $1`, issue).Scan(&nextAt, &lastError); err != nil {
		t.Fatalf("read %s's outbox row: %v", issue, err)
	}
	return nextAt, lastError
}
