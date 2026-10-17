package record

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

var _ Store = NewStore()

// testDSN is the Postgres connection CI and the devbox provide for this package. The record has
// no in-memory implementation: its transaction and locking semantics are PostgreSQL's.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test")
	}
	return dsn
}

func emptyStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	t.Cleanup(admin.Close)

	name := "legion_record_test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	testURL := *base
	testURL.Path = "/" + name
	st, err := store.Open(ctx, testURL.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(st.Close)
	return st
}

func migratedStore(t *testing.T) *store.Store {
	t.Helper()
	st := emptyStore(t)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func inTx(t *testing.T, st *store.Store, fn func(pgx.Tx)) {
	t.Helper()
	if err := st.Tx(context.Background(), func(tx pgx.Tx) error {
		fn(tx)
		return nil
	}); err != nil {
		t.Fatalf("transaction: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func issueFixture(key string) Issue {
	parent := "LEGION-200"
	ready := 9
	return Issue{
		Key:                 key,
		Tree:                key,
		Project:             "LEGION",
		Title:               "Persist every workflow fact",
		Parent:              &parent,
		Phase:               phase.Implementing,
		Generation:          3,
		Status:              "in_progress",
		Rank:                "00042U",
		HandedOver:          true,
		LastDispatchSeq:     41,
		ReadyPendingVersion: &ready,
	}
}

func TestStoreRoundTripsEveryRecord(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-208")
	issue.Tree = "LEGION-200"
	updatedAt := time.Date(2026, 9, 22, 14, 12, 13, 456000000, time.UTC)
	approved := 7
	pushes := []ClassifiedPush{
		{SHA: "b1c2d3", Before: "a0b1c2", HandoffOnly: true, Unknown: "paths_truncated", ByReviewApp: true},
		{SHA: "c2d3e4", Before: "b1c2d3", Forced: true},
	}
	pr := PullRequest{
		Issue: issue.Key, Repo: "sjawhar/legion", Number: 1243, Branch: "legion/LEGION-208",
		HeadSHA: "b1c2d3", HeadUpdatedAt: updatedAt,
		Failing: []string{"unit"}, Cancelled: []string{"e2e"}, CheckedHead: "b1c2d3",
		FixAttempts: 2, BlockedAttempts: 1,
		CheckRuns: []AttemptRun{{Name: "unit", ID: 91}, {Name: "lint", ID: 92}}, Generation: 4,
		Snapshot: "snapshot-4", Pushes: pushes, HeadCounted: "b1c2d3", HeadCountedBefore: "a0b1c2", PlannedRed: true, State: PullRequestMerged,
		// A base that requires no check reads back as such, never as a set that was not read.
		Required:  []string{},
		Workflows: []RequiredWorkflow{{Path: ".github/workflows/review.yml", Result: "failure"}}, WorkflowsHead: "b1c2d3",
	}
	phase := PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "legion-208-implementer", HandoffCommit: "aabbcc", Rounds: 2, Verdict: "pass",
		MergeNote: "MANUAL_MERGE_REQUIRED: pull request #1243 matches a by_hand merge rule; do not auto-arm it"}
	gate := DesignGate{Issue: issue.Key, ArtifactID: "artifact-208", LatestVersion: 7, ApprovedVersion: &approved}
	slot := Slot{Issue: issue.Key, Index: 0, AdmittedAt: updatedAt}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPhase(ctx, tx, phase))
		must(t, records.PutPullRequest(ctx, tx, pr))
		must(t, records.PutGate(ctx, tx, gate))
		must(t, records.PutSlot(ctx, tx, slot))
	})

	inTx(t, st, func(tx pgx.Tx) {
		gotIssue, err := records.Issue(ctx, tx, issue.Key)
		must(t, err)
		if !reflect.DeepEqual(gotIssue, &issue) {
			t.Fatalf("issue = %#v, want %#v", gotIssue, issue)
		}
		issues, err := records.Issues(ctx, tx)
		must(t, err)
		if !reflect.DeepEqual(issues, []Issue{issue}) {
			t.Fatalf("issues = %#v, want %#v", issues, []Issue{issue})
		}
		phases, err := records.Phases(ctx, tx, issue.Key)
		must(t, err)
		if !reflect.DeepEqual(phases, []PhaseRow{phase}) {
			t.Fatalf("phases = %#v, want %#v", phases, []PhaseRow{phase})
		}
		gotPR, err := records.PullRequest(ctx, tx, issue.Key)
		must(t, err)
		if !samePullRequest(*gotPR, pr) {
			t.Fatalf("pull request = %#v, want %#v", gotPR, pr)
		}
		byBranch, err := records.PullRequestByBranch(ctx, tx, pr.Repo, pr.Branch)
		must(t, err)
		if !samePullRequest(*byBranch, pr) {
			t.Fatalf("pull request by branch = %#v, want %#v", byBranch, pr)
		}
		gotGate, err := records.Gate(ctx, tx, issue.Key)
		must(t, err)
		if !reflect.DeepEqual(gotGate, &gate) {
			t.Fatalf("gate = %#v, want %#v", gotGate, gate)
		}
		slots, err := records.Slots(ctx, tx)
		must(t, err)
		if len(slots) != 1 || slots[0].Issue != slot.Issue || slots[0].Index != slot.Index || !slots[0].AdmittedAt.Equal(slot.AdmittedAt) {
			t.Fatalf("slots = %#v, want %#v", slots, slot)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ReleaseSlot(ctx, tx, issue.Key))
		slots, err := records.Slots(ctx, tx)
		must(t, err)
		if len(slots) != 0 {
			t.Fatalf("slots after release = %#v, want no rows", slots)
		}
	})
}

// ClaimSession is the session claims records for the token (the workflow reads it for the
// reviewer's claim to tell the reviewer's own review-App session from another's by the Legion
// footer each carries). An unknown token answers "" and no error.
func TestClaimSessionAnswersTheSeededSessionOrEmpty(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, $3, $4, $5, 0, $6, '', $7, 0, 0, 0, 0)`,
			"review-claim", "LEGION", "LEGION-208", "LEGION-208", string(claim.RoleReviewer), "ses-reviewer", string(supervise.StateWorking))
		must(t, err)
	})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := records.ClaimSession(ctx, tx, "review-claim")
		must(t, err)
		if got != "ses-reviewer" {
			t.Fatalf("ClaimSession(review-claim) = %q, want ses-reviewer", got)
		}
		got, err = records.ClaimSession(ctx, tx, "no-such-claim")
		must(t, err)
		if got != "" {
			t.Fatalf("ClaimSession(no-such-claim) = %q, want \"\"", got)
		}
	})
}

func TestStoreRoundTripsLingerStateAndRefusesHeldFromHeld(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	until := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	hold := Hold{From: phase.Implementing, Reason: HoldEscalated}
	lingering := issueFixture("LEGION-210")
	lingering.LingerUntil = &until
	lingering.Hold = &hold

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, lingering))
		got, err := records.Issue(ctx, tx, lingering.Key)
		must(t, err)
		if got == nil || got.LingerUntil == nil || !got.LingerUntil.Equal(until) || got.Hold == nil || *got.Hold != hold {
			t.Fatalf("lingering issue state = %#v, want linger until %s and hold %+v", got, until, hold)
		}
	})

	cleared := lingering
	cleared.LingerUntil = nil
	cleared.Hold = nil
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, cleared))
		got, err := records.Issue(ctx, tx, cleared.Key)
		must(t, err)
		if !reflect.DeepEqual(got, &cleared) {
			t.Fatalf("cleared issue = %#v, want %#v", got, cleared)
		}
	})

	invalid := issueFixture("LEGION-211")
	invalid.Hold = &Hold{From: phase.Held}
	err := st.Tx(ctx, func(tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, invalid)
	})
	if err == nil {
		t.Fatal("PutIssue accepted held_from=held")
	}
}

// A hold's reason ends with the hold whichever build writes the row: an earlier build's upsert
// names no hold_reason, so ending a hold and holding the issue again there would otherwise leave
// the old escalation on a hold nobody escalated.
func TestAWriteThatEndsAHoldEndsItsReasonWhateverWritesIt(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-212")
	issue.Phase, issue.Hold = phase.Held, &Hold{From: phase.Implementing, Reason: HoldEscalated}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		for _, write := range []string{
			"update issues set phase = 'implementing', held_from = null where key = $1",
			"update issues set phase = 'held', held_from = 'implementing' where key = $1",
		} {
			if _, err := tx.Exec(ctx, write, issue.Key); err != nil {
				t.Fatalf("%s: %v", write, err)
			}
		}
		got, err := records.Issue(ctx, tx, issue.Key)
		must(t, err)
		if want := (Hold{From: phase.Implementing}); got == nil || got.Hold == nil || *got.Hold != want {
			t.Fatalf("hold after it ended and began again without a reason = %+v, want %+v", got.Hold, want)
		}
	})
}

func samePullRequest(got, want PullRequest) bool {
	return got.Issue == want.Issue && got.Repo == want.Repo && got.Number == want.Number &&
		got.Branch == want.Branch && got.HeadSHA == want.HeadSHA && got.HeadUpdatedAt.Equal(want.HeadUpdatedAt) &&
		got.CheckedHead == want.CheckedHead && reflect.DeepEqual(got.Failing, want.Failing) &&
		reflect.DeepEqual(append([]string{}, got.Cancelled...), append([]string{}, want.Cancelled...)) &&
		got.FixAttempts == want.FixAttempts &&
		got.BlockedAttempts == want.BlockedAttempts && reflect.DeepEqual(got.CheckRuns, want.CheckRuns) &&
		got.Generation == want.Generation && got.Snapshot == want.Snapshot &&
		reflect.DeepEqual(got.Pushes, want.Pushes) && got.HeadCounted == want.HeadCounted && got.HeadCountedBefore == want.HeadCountedBefore &&
		got.PlannedRed == want.PlannedRed && reflect.DeepEqual(got.Required, want.Required) &&
		reflect.DeepEqual(got.Workflows, want.Workflows) && got.WorkflowsHead == want.WorkflowsHead
}

// A new generation keeps an open pull request but none of the old generation's fix-attempt
// accounting: its counts, its counted head and the mark that count replaced, and its planned mark,
// which would otherwise exempt the new generation's first red from counting.
func TestClearGenerationResetsAnOpenPullRequestsFixAttemptAccounting(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-285")
	pr := PullRequest{
		Issue: issue.Key, Repo: "sjawhar/legion", Number: 1359, Branch: "legion/LEGION-285",
		HeadSHA: "red-tests", HeadUpdatedAt: time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC),
		CheckedHead: "red-tests", Failing: []string{"test"}, CheckRuns: []AttemptRun{},
		FixAttempts: 2, BlockedAttempts: 2, HeadCounted: "red-tests", HeadCountedBefore: "fix", PlannedRed: true, State: PullRequestOpen,
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPullRequest(ctx, tx, pr))
		must(t, records.ClearGeneration(ctx, tx, issue.Key))
	})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := records.PullRequest(ctx, tx, issue.Key)
		must(t, err)
		if got == nil {
			t.Fatal("the open pull request is gone, want it kept")
		}
		if got.FixAttempts != 0 || got.BlockedAttempts != 0 || got.HeadCounted != "" || got.HeadCountedBefore != "" || got.PlannedRed {
			t.Fatalf("after ClearGeneration: fixAttempts=%d blockedAttempts=%d headCounted=%q headCountedBefore=%q plannedRed=%v, want 0, 0, \"\", \"\" and false",
				got.FixAttempts, got.BlockedAttempts, got.HeadCounted, got.HeadCountedBefore, got.PlannedRed)
		}
	})
}

func TestMarkProcessedIsIdempotent(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()

	inTx(t, st, func(tx pgx.Tx) {
		fresh, err := records.MarkProcessed(context.Background(), tx, "dispatch", "event-208")
		must(t, err)
		if !fresh {
			t.Fatal("first event was not fresh")
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		fresh, err := records.MarkProcessed(context.Background(), tx, "dispatch", "event-208")
		must(t, err)
		if fresh {
			t.Fatal("second event was fresh, want deduplicated")
		}
	})
}

// outboxKeys names rows for comparison: `kind:issue`, with a supervise row's op or a status
// write's status after it.
func outboxKeys(rows []OutboxRow) []string {
	keys := []string{}
	for _, row := range rows {
		key := string(row.Kind) + ":" + row.Issue
		var payload struct {
			Op     string `json:"op"`
			Status string `json:"status"`
		}
		if json.Unmarshal(row.Payload, &payload) == nil {
			switch row.Kind {
			case OutboxKindSupervise:
				key += ":" + payload.Op
			case OutboxKindDispatchStatus:
				key += ":" + payload.Status
			}
		}
		keys = append(keys, key)
	}
	return keys
}

// claimEachDue is what a lane does with project's outbox at now: lists the rows it may run and
// claims each by id. It fails the test when a listed row is not claimable, since nothing else ran
// between the list and the claim. It leases every row it lists, so it suits a list with at most one
// lifecycle row per issue: a lifecycle row's lease holds its issue's other lifecycle rows back until
// it settles (outboxLaneFree), which a lane never meets, since it settles each row before it claims
// the next.
func claimEachDue(t *testing.T, records *Postgres, tx pgx.Tx, project string, now time.Time) []OutboxRow {
	t.Helper()
	ctx := context.Background()
	due, err := records.DueRows(ctx, tx, project, "", nil, now)
	must(t, err)
	claimed := []OutboxRow{}
	for _, row := range due {
		leased, ok, err := records.ClaimRow(ctx, tx, row.ID, now, time.Minute)
		must(t, err)
		if !ok {
			t.Fatalf("row %d was listed due but not claimed", row.ID)
		}
		claimed = append(claimed, leased)
	}
	return claimed
}

// A lane claims a due row by one conditional update, so two lanes that both listed it lease it
// exactly once: the first claim takes the lease, and a second begun while the first is uncommitted
// waits on the row and then finds the lease taken. A leased row is listed due to nobody, and a stale
// token neither finishes nor retries it. Once the lease lapses the row is due again and claimable
// under a new token; once finished it is claimable by nobody.
func TestClaimRowLeasesADueRowExactlyOnceUntilItsLeaseLapses(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	var row OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindDispatchStatus, Issue: "LEGION-208", Payload: json.RawMessage(`{"status":"testing"}`), NextAt: now}))
		due, err := records.DueRows(ctx, tx, "LEGION", "", nil, now)
		must(t, err)
		if len(due) != 1 || due[0].LeaseToken != "" || due[0].LeaseUntil != nil {
			t.Fatalf("due rows = %#v, want the one row, unleased", due)
		}
		row = due[0]
	})

	tx1, err := st.BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	first, ok, err := records.ClaimRow(ctx, tx1, row.ID, now, time.Minute)
	must(t, err)
	if !ok || first.ID != row.ID || first.LeaseToken == "" || first.LeaseUntil == nil || !first.LeaseUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("first claim = %#v, %v, want the row leased for a minute", first, ok)
	}
	type attempt struct {
		ok  bool
		err error
	}
	second := make(chan attempt, 1)
	go func() {
		tx2, err := st.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			second <- attempt{err: err}
			return
		}
		defer tx2.Rollback(ctx)
		_, ok, err := records.ClaimRow(ctx, tx2, row.ID, now, time.Minute)
		if err == nil {
			err = tx2.Commit(ctx)
		}
		second <- attempt{ok: ok, err: err}
	}()
	must(t, tx1.Commit(ctx))
	if got := <-second; got.err != nil || got.ok {
		t.Fatalf("second claim of the leased row = %+v, want nothing claimed", got)
	}

	inTx(t, st, func(tx pgx.Tx) {
		due, err := records.DueRows(ctx, tx, "LEGION", "", nil, now)
		must(t, err)
		if len(due) != 0 {
			t.Fatalf("due rows while leased = %#v, want none", due)
		}
		must(t, records.FinishOutbox(ctx, tx, row.ID, "stale-lease"))
		must(t, records.RetryOutbox(ctx, tx, row.ID, "stale-lease", now.Add(24*time.Hour), "stale"))
	})

	lapsed := now.Add(2 * time.Minute)
	inTx(t, st, func(tx pgx.Tx) {
		due, err := records.DueRows(ctx, tx, "LEGION", "", nil, lapsed)
		must(t, err)
		if len(due) != 1 || due[0].ID != row.ID || due[0].Attempts != 0 {
			t.Fatalf("due rows once the lease lapsed = %#v, want the original row, untouched by the stale retry", due)
		}
		again, ok, err := records.ClaimRow(ctx, tx, row.ID, lapsed, time.Minute)
		must(t, err)
		if !ok || again.LeaseToken == first.LeaseToken || again.LeaseUntil == nil || !again.LeaseUntil.Equal(lapsed.Add(time.Minute)) {
			t.Fatalf("claim once the lease lapsed = %#v, %v, want the row under a new lease", again, ok)
		}
		must(t, records.FinishOutbox(ctx, tx, again.ID, again.LeaseToken))
	})

	inTx(t, st, func(tx pgx.Tx) {
		due, err := records.DueRows(ctx, tx, "LEGION", "", nil, now.Add(3*time.Minute))
		must(t, err)
		if len(due) != 0 {
			t.Fatalf("finished row still due: %#v", due)
		}
		if _, ok, err := records.ClaimRow(ctx, tx, row.ID, now.Add(3*time.Minute), time.Minute); err != nil || ok {
			t.Fatalf("claim of the finished row = %v, %v, want nothing claimed", ok, err)
		}
	})
}

// ClaimRow decides by the row as it is at the claim, not as the lane listed it: a start the branch
// rule holds behind an older issue_branch row of its issue is refused though due and unleased, and
// claimable once the branch row finishes.
func TestClaimRowReEvaluatesTheOrderingRulesAtTheClaim(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var branch, start OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		row, err := NewOutboxRow("LEGION-208", IssueBranch{Generation: 1}, now)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
		row, err = NewOutboxRow("LEGION-208", SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1}, now)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
		rows, err := records.DueRows(ctx, tx, "LEGION", "", nil, now.Add(time.Hour))
		must(t, err)
		if len(rows) != 1 || rows[0].Kind != OutboxKindIssueBranch {
			t.Fatalf("due rows = %v, want the branch row alone", outboxKeys(rows))
		}
		branch = rows[0]
		must(t, tx.QueryRow(ctx, "select id from outbox where kind = $1", string(OutboxKindSupervise)).Scan(&start.ID))
	})
	inTx(t, st, func(tx pgx.Tx) {
		if _, ok, err := records.ClaimRow(ctx, tx, start.ID, now, time.Minute); err != nil || ok {
			t.Fatalf("claim of the start behind its branch row = %v, %v, want refused", ok, err)
		}
		leased, ok, err := records.ClaimRow(ctx, tx, branch.ID, now, time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("the branch row was not claimed")
		}
		must(t, records.FinishOutbox(ctx, tx, leased.ID, leased.LeaseToken))
	})
	inTx(t, st, func(tx pgx.Tx) {
		if _, ok, err := records.ClaimRow(ctx, tx, start.ID, now, time.Minute); err != nil || !ok {
			t.Fatalf("claim of the start once its branch row finished = %v, %v, want claimed", ok, err)
		}
	})
}

// A lease is renewed only by its holder: the right token moves lease_until, a wrong one moves
// nothing, and a finished row has no lease to renew.
func TestRenewOutboxLeaseRenewsOnlyUnderItsToken(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	var leased OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindSupervise, Issue: "LEGION-208", Payload: json.RawMessage(`{"op":"suspend"}`), NextAt: now}))
		claimed := claimEachDue(t, records, tx, "LEGION", now)
		if len(claimed) != 1 {
			t.Fatalf("claimed %v, want the one row", outboxKeys(claimed))
		}
		leased = claimed[0]
	})
	// dueAt is whether the row is due at at: a lease renewed to five minutes keeps it from the two-minute mark.
	dueAt := func(tx pgx.Tx, at time.Time) bool {
		due, err := records.DueRows(ctx, tx, "LEGION", "", nil, at)
		must(t, err)
		return len(due) == 1
	}
	inTx(t, st, func(tx pgx.Tx) {
		renewed, err := records.RenewOutboxLease(ctx, tx, leased.ID, "another-lane", now.Add(5*time.Minute))
		must(t, err)
		if renewed || !dueAt(tx, now.Add(2*time.Minute)) {
			t.Fatalf("renewal under a wrong token = %v and the row due at two minutes = %v, want false and true", renewed, dueAt(tx, now.Add(2*time.Minute)))
		}
		renewed, err = records.RenewOutboxLease(ctx, tx, leased.ID, leased.LeaseToken, now.Add(5*time.Minute))
		must(t, err)
		if !renewed || dueAt(tx, now.Add(2*time.Minute)) || !dueAt(tx, now.Add(6*time.Minute)) {
			t.Fatalf("renewal under the lease's token = %v, want true and the row due only after five minutes", renewed)
		}
		must(t, records.FinishOutbox(ctx, tx, leased.ID, leased.LeaseToken))
		renewed, err = records.RenewOutboxLease(ctx, tx, leased.ID, leased.LeaseToken, now.Add(9*time.Minute))
		must(t, err)
		if renewed {
			t.Fatal("a finished row's lease was renewed")
		}
	})
}

// `legion state` shows a lifecycle row held behind its issue's running one. A due, unleased
// lifecycle row is held when another lifecycle row of its issue is under a live lease, on that
// row; a start the branch rule holds behind an issue_branch row is held on the branch row while it
// runs, and listed by nothing while the branch row waits unleased for its retry. A due lifecycle
// row no sibling is running is not held, a board row is never held, another project's rows are
// never listed, the held rows come by next_at then id with their own next_at, and an issue given
// narrows them to its own.
func TestHeldRowsListsLifecycleRowsBehindTheirIssuesRunningOne(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueue := func(tx pgx.Tx, issue string, payload OutboxPayload, at time.Time) int64 {
		row, err := NewOutboxRow(issue, payload, at)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
		var id int64
		must(t, tx.QueryRow(ctx, "select max(id) from outbox").Scan(&id))
		return id
	}
	lease := func(tx pgx.Tx, id int64) {
		if _, ok, err := records.ClaimRow(ctx, tx, id, now, time.Minute); err != nil || !ok {
			t.Fatalf("lease of row %d = %v, %v, want leased", id, ok, err)
		}
	}
	suspend := SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RolePlanner, Generation: 1}
	start := SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleArchitect, Generation: 1}
	var runningSuspend, removal, runningBranch, fencedStart int64
	inTx(t, st, func(tx pgx.Tx) {
		// (a) LEGION-208: a workspace removal and a status write due behind a running suspend.
		runningSuspend = enqueue(tx, "LEGION-208", suspend, now)
		removal = enqueue(tx, "LEGION-208", WorkspaceRemove{Linger: 1}, now)
		enqueue(tx, "LEGION-208", StatusWrite{Status: "done"}, now)
		// (b) LEGION-209: a start due behind its branch row, which is running; due a minute before 208's.
		runningBranch = enqueue(tx, "LEGION-209", IssueBranch{Generation: 1}, now)
		fencedStart = enqueue(tx, "LEGION-209", start, now.Add(-time.Minute))
		// (c) LEGION-210: a start behind a branch row on its backoff, which nobody runs.
		enqueue(tx, "LEGION-210", IssueBranch{Generation: 1}, now.Add(time.Minute))
		enqueue(tx, "LEGION-210", SuperviseRequest{Op: "start", Tree: "LEGION-210", Role: claim.RoleArchitect, Generation: 1}, now)
		// (d) LEGION-211: a due linger close with nothing of its issue running.
		enqueue(tx, "LEGION-211", LingerClose{Generation: 1}, now)
		// OTHER-7: shape (a) in another project.
		otherSuspend := enqueue(tx, "OTHER-7", SuperviseRequest{Op: "suspend", Tree: "OTHER-7", Role: claim.RolePlanner, Generation: 1}, now)
		enqueue(tx, "OTHER-7", WorkspaceRemove{Linger: 1}, now)
		lease(tx, runningSuspend)
		lease(tx, runningBranch)
		lease(tx, otherSuspend)
	})
	inTx(t, st, func(tx pgx.Tx) {
		held, err := records.HeldRows(ctx, tx, "LEGION", "", now)
		must(t, err)
		if len(held) != 2 {
			t.Fatalf("held rows = %+v, want LEGION-209's start then LEGION-208's workspace removal", held)
		}
		if held[0].ID != fencedStart || held[0].WaitsOn != runningBranch || !held[0].NextAt.Equal(now.Add(-time.Minute)) {
			t.Errorf("first held row = %+v, want the start %d, due at %s, waiting on its branch row %d", held[0], fencedStart, now.Add(-time.Minute), runningBranch)
		}
		if held[1].ID != removal || held[1].WaitsOn != runningSuspend || !held[1].NextAt.Equal(now) {
			t.Errorf("second held row = %+v, want the workspace removal %d, due at %s, waiting on the suspend %d", held[1], removal, now, runningSuspend)
		}
		other, err := records.HeldRows(ctx, tx, "OTHER", "", now)
		must(t, err)
		if len(other) != 1 || other[0].Issue != "OTHER-7" || other[0].Kind != OutboxKindWorkspaceRemove {
			t.Errorf("OTHER's held rows = %+v, want its workspace removal alone", other)
		}
		ofIssue, err := records.HeldRows(ctx, tx, "LEGION", "LEGION-208", now)
		must(t, err)
		if len(ofIssue) != 1 || ofIssue[0].ID != removal || ofIssue[0].WaitsOn != runningSuspend {
			t.Errorf("LEGION-208's held rows = %+v, want its workspace removal %d alone, waiting on the suspend %d", ofIssue, removal, runningSuspend)
		}
	})
	// Once the running rows finish, nothing of LEGION is held: 208's removal and 211's close are
	// due to run, 209's start is due behind no running row, and 210's start waits on its backoff.
	inTx(t, st, func(tx pgx.Tx) {
		var token string
		for _, id := range []int64{runningSuspend, runningBranch} {
			must(t, tx.QueryRow(ctx, "select lease_token from outbox where id = $1", id).Scan(&token))
			must(t, records.FinishOutbox(ctx, tx, id, token))
		}
		held, err := records.HeldRows(ctx, tx, "LEGION", "", now)
		must(t, err)
		if len(held) != 0 {
			t.Fatalf("held rows with nothing running = %+v, want none", held)
		}
	})
}

// UnfinishedLanes names each lane with a row left, once, by issue then lane, for the project alone:
// an issue with a status write and a start has both lanes, one with two board rows has the board
// lane once, and a project with no rows has none.
func TestUnfinishedLanesNamesEachLaneWithARowLeft(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	inTx(t, st, func(tx pgx.Tx) {
		for _, row := range []OutboxRow{
			{Kind: OutboxKindDispatchStatus, Issue: "LEGION-208", Payload: json.RawMessage(`{"status":"testing"}`)},
			{Kind: OutboxKindSupervise, Issue: "LEGION-208", Payload: json.RawMessage(`{"op":"start"}`)},
			{Kind: OutboxKindNotice, Issue: "LEGION-209", Payload: json.RawMessage(`{"kind":"phase-finished"}`)},
			{Kind: OutboxKindDispatchMessage, Issue: "LEGION-209", Payload: json.RawMessage(`{"body":"Done."}`)},
			{Kind: OutboxKindSupervise, Issue: "OTHER-7", Payload: json.RawMessage(`{"op":"start"}`)},
		} {
			row.NextAt = now
			must(t, records.Enqueue(ctx, tx, row))
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		lanes, err := records.UnfinishedLanes(ctx, tx, "LEGION")
		must(t, err)
		want := []LaneKey{{Issue: "LEGION-208", Lane: LaneBoard}, {Issue: "LEGION-208", Lane: LaneLifecycle}, {Issue: "LEGION-209", Lane: LaneBoard}}
		if !reflect.DeepEqual(lanes, want) {
			t.Errorf("LEGION's unfinished lanes = %v, want %v", lanes, want)
		}
		none, err := records.UnfinishedLanes(ctx, tx, "NOBODY")
		must(t, err)
		if none == nil || len(none) != 0 {
			t.Errorf("a project with no rows has lanes %#v, want an empty list", none)
		}
	})
}

// NextDue is when a lane with nothing to run next has something: the earliest next_at after now
// among the issue's rows of its kinds that no live lease holds. A row due already is not it, nor one
// under a live lease, nor another issue's or another lane's row; a lapsed lease does not hide a row.
func TestNextDueIsTheEarliestLaterMomentAnUnleasedRowComesDue(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := json.RawMessage(`{"op":"start"}`)
	live, lapsed := now.Add(10*time.Minute), now.Add(-time.Minute)
	inTx(t, st, func(tx pgx.Tx) {
		for _, row := range []OutboxRow{
			{Kind: OutboxKindSupervise, Issue: "LEGION-208", Payload: start, NextAt: now},
			{Kind: OutboxKindSupervise, Issue: "LEGION-208", Payload: start, NextAt: now.Add(2 * time.Minute), LeaseToken: "running", LeaseUntil: &live},
			{Kind: OutboxKindSupervise, Issue: "LEGION-208", Payload: start, NextAt: now.Add(4 * time.Minute), LeaseToken: "gone", LeaseUntil: &lapsed},
			{Kind: OutboxKindSupervise, Issue: "LEGION-208", Payload: start, NextAt: now.Add(5 * time.Minute)},
			{Kind: OutboxKindNotice, Issue: "LEGION-208", Payload: json.RawMessage(`{"kind":"phase-finished"}`), NextAt: now.Add(3 * time.Minute)},
			{Kind: OutboxKindSupervise, Issue: "LEGION-209", Payload: start, NextAt: now.Add(time.Minute)},
		} {
			must(t, records.Enqueue(ctx, tx, row))
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		next, ok, err := records.NextDue(ctx, tx, "LEGION-208", LaneKinds(LaneLifecycle), now)
		must(t, err)
		if !ok || !next.Equal(now.Add(4*time.Minute)) {
			t.Errorf("LEGION-208's lifecycle lane is next due at %s, %v, want %s: the row whose lease lapsed", next, ok, now.Add(4*time.Minute))
		}
		next, ok, err = records.NextDue(ctx, tx, "LEGION-208", nil, now)
		must(t, err)
		if !ok || !next.Equal(now.Add(3*time.Minute)) {
			t.Errorf("LEGION-208 is next due at %s, %v, want %s: its notice", next, ok, now.Add(3*time.Minute))
		}
		if next, ok, err := records.NextDue(ctx, tx, "LEGION-208", LaneKinds(LaneLifecycle), now.Add(5*time.Minute)); err != nil || ok {
			t.Errorf("LEGION-208's lifecycle lane once every row is due = %s, %v, %v, want nothing later", next, ok, err)
		}
		if next, ok, err := records.NextDue(ctx, tx, "LEGION-210", nil, now); err != nil || ok {
			t.Errorf("an issue with no rows is next due at %s, %v, %v, want never", next, ok, err)
		}
	})
}

// Migration 0046 indexes the outbox by issue, then id, for the reads each issue's lanes make.
func TestRecordMigrationCreatesTheOutboxIssueIndex(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		var definition string
		must(t, tx.QueryRow(context.Background(), "select indexdef from pg_indexes where tablename = 'outbox' and indexname = 'outbox_issue'").Scan(&definition))
		if !strings.HasSuffix(definition, "(issue, id)") {
			t.Fatalf("outbox_issue = %q, want an index on (issue, id)", definition)
		}
	})
}

// Daemons of different projects share one database, and each runs only its own project's outbox:
// a daemon lists and leases no row of another project's issue, which stays due for its own daemon,
// and the pending status writes list none of them. A project whose key another begins with is
// another.
func TestTheOutboxIsEachProjectsOwn(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	status := json.RawMessage(`{"status":"in_progress"}`)
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []string{"LEGION-208", "OTHER-7", "LEGION2-5"} {
			must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindDispatchStatus, Issue: issue, Payload: status, NextAt: now}))
		}
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindNotice, Issue: "OTHER-7", Payload: json.RawMessage(`{"kind":"phase-finished"}`), NextAt: now}))
	})
	inTx(t, st, func(tx pgx.Tx) {
		claimed := claimEachDue(t, records, tx, "LEGION", now)
		if got := outboxKeys(claimed); !reflect.DeepEqual(got, []string{"dispatch_status:LEGION-208:in_progress"}) {
			t.Errorf("LEGION's claim = %v, want its own status row alone", got)
		}
		pending, err := records.PendingStatusWrites(ctx, tx, "LEGION")
		must(t, err)
		if got := outboxKeys(pending); !reflect.DeepEqual(got, []string{"dispatch_status:LEGION-208:in_progress"}) {
			t.Errorf("LEGION's pending status writes = %v, want its own alone", got)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		claimed := claimEachDue(t, records, tx, "OTHER", now)
		if got := outboxKeys(claimed); !reflect.DeepEqual(got, []string{"dispatch_status:OTHER-7:in_progress", "notice:OTHER-7"}) {
			t.Errorf("OTHER's claim after LEGION's = %v, want both of its rows, due and unleased", got)
		}
	})
}

// DueRows lists what a lane may run now and nothing it may not: a newer status write of an issue
// waits unlisted while an older one is unfinished and is listed once it is, a start waits unlisted
// while an older issue_branch row of its issue exists, and a kinds or issue filter narrows the list
// to those kinds or that issue.
func TestDueRowsListsOnlyTheRowsALaneMayRunNow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueue := func(tx pgx.Tx, issue string, payload OutboxPayload) {
		row, err := NewOutboxRow(issue, payload, now)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
	}
	inTx(t, st, func(tx pgx.Tx) {
		enqueue(tx, "LEGION-208", StatusWrite{Status: "testing"})
		enqueue(tx, "LEGION-208", StatusWrite{Status: "retro"})
		enqueue(tx, "LEGION-208", IssueBranch{Generation: 1})
		enqueue(tx, "LEGION-208", SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1})
		enqueue(tx, "LEGION-209", Notice{Kind: "phase-finished", Role: claim.RoleTester, Summary: "Tests passed"})
	})
	due := func(tx pgx.Tx, issue string, kinds []OutboxKind) []string {
		rows, err := records.DueRows(ctx, tx, "LEGION", issue, kinds, now)
		must(t, err)
		return outboxKeys(rows)
	}
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := due(tx, "", nil), []string{"dispatch_status:LEGION-208:testing", "issue_branch:LEGION-208", "notice:LEGION-209"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("due rows = %v, want %v: the newer status and the start behind their older rows", got, want)
		}
		if got, want := due(tx, "LEGION-209", nil), []string{"notice:LEGION-209"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("due rows of LEGION-209 = %v, want %v", got, want)
		}
		if got, want := due(tx, "", []OutboxKind{OutboxKindIssueBranch, OutboxKindNotice}), []string{"issue_branch:LEGION-208", "notice:LEGION-209"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("due branch and notice rows = %v, want %v", got, want)
		}
		if got, want := due(tx, "LEGION-208", LaneKinds(LaneBoard)), []string{"dispatch_status:LEGION-208:testing"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("due board rows of LEGION-208 = %v, want %v", got, want)
		}
		for _, row := range claimEachDue(t, records, tx, "LEGION", now) {
			must(t, records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken))
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := due(tx, "", nil), []string{"dispatch_status:LEGION-208:retro", "supervise:LEGION-208:start"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("due rows once the older ones finished = %v, want %v", got, want)
		}
	})
}

// An issue's lifecycle rows run one at a time, and the store says so, not the lane alone: while one
// lifecycle row of an issue is under a live lease — a lane of this daemon's, or of another's in a
// rolling restart — a due lifecycle row of the issue is listed by nobody and ClaimRow refuses it,
// while a due board row of the same issue and another issue's lifecycle row are listed and run.
// Once the leased row finishes, the row is listed and claimable.
func TestADueLifecycleRowWaitsWhileItsIssuesLifecycleRowRuns(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueue := func(tx pgx.Tx, issue string, payload OutboxPayload) int64 {
		row, err := NewOutboxRow(issue, payload, now)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
		var id int64
		must(t, tx.QueryRow(ctx, "select max(id) from outbox").Scan(&id))
		return id
	}
	due := func(tx pgx.Tx, issue string, kinds []OutboxKind) []string {
		rows, err := records.DueRows(ctx, tx, "LEGION", issue, kinds, now)
		must(t, err)
		return outboxKeys(rows)
	}
	var suspend, removal OutboxRow
	var removalID int64
	inTx(t, st, func(tx pgx.Tx) {
		enqueue(tx, "LEGION-208", SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RolePlanner, Generation: 1})
		removalID = enqueue(tx, "LEGION-208", WorkspaceRemove{Linger: 1})
		enqueue(tx, "LEGION-208", StatusWrite{Status: "done"})
		enqueue(tx, "LEGION-209", LingerClose{Generation: 1})
		all := []string{"supervise:LEGION-208:suspend", "workspace_remove:LEGION-208", "dispatch_status:LEGION-208:done", "linger_close:LEGION-209"}
		if got := due(tx, "", nil); !reflect.DeepEqual(got, all) {
			t.Fatalf("due rows with nothing leased = %v, want %v", got, all)
		}
		rows, err := records.DueRows(ctx, tx, "LEGION", "LEGION-208", LaneKinds(LaneLifecycle), now)
		must(t, err)
		leased, ok, err := records.ClaimRow(ctx, tx, rows[0].ID, now, time.Minute)
		must(t, err)
		if !ok || leased.Kind != OutboxKindSupervise {
			t.Fatalf("claim of the suspend = %v, %v, want it leased", outboxKeys([]OutboxRow{leased}), ok)
		}
		suspend = leased
	})
	// Another daemon's lane holds the suspend: this one lists and claims nothing of 208's lifecycle.
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := due(tx, "", nil), []string{"dispatch_status:LEGION-208:done", "linger_close:LEGION-209"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("due rows while the suspend runs = %v, want %v: 208's board row and 209's close, not 208's removal", got, want)
		}
		if got := due(tx, "LEGION-208", LaneKinds(LaneLifecycle)); len(got) != 0 {
			t.Fatalf("due lifecycle rows of LEGION-208 while its suspend runs = %v, want none", got)
		}
		if got, want := due(tx, "LEGION-208", LaneKinds(LaneBoard)), []string{"dispatch_status:LEGION-208:done"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("due board rows of LEGION-208 while its suspend runs = %v, want %v", got, want)
		}
		if _, ok, err := records.ClaimRow(ctx, tx, removalID, now, time.Minute); err != nil || ok {
			t.Fatalf("claim of the removal while the suspend runs = %v, %v, want refused", ok, err)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.FinishOutbox(ctx, tx, suspend.ID, suspend.LeaseToken))
		if got, want := due(tx, "", nil), []string{"workspace_remove:LEGION-208", "dispatch_status:LEGION-208:done", "linger_close:LEGION-209"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("due rows once the suspend finished = %v, want %v", got, want)
		}
		var ok bool
		var err error
		removal, ok, err = records.ClaimRow(ctx, tx, removalID, now, time.Minute)
		must(t, err)
		if !ok || removal.ID != removalID || removal.LeaseToken == "" {
			t.Fatalf("claim of the removal once the suspend finished = %#v, %v, want it leased", removal, ok)
		}
	})
}

// Two daemons' lanes claim different lifecycle rows of one issue at the same moment: the first
// daemon's claim of the suspend is still uncommitted when the second daemon's claim of the removal
// runs. The rule outboxLaneFree states — no lifecycle row of an issue runs while another of its
// lifecycle rows is under a live lease, across daemons — holds only if at most one of the two is
// leased once both have committed: the second claim must see the first's lease, or wait for it,
// rather than read a snapshot from before it and lease a second row beside it.
func TestTwoDaemonsClaimingDifferentLifecycleRowsOfOneIssueAtOnceLeaseOnlyOne(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var suspendID, removalID int64
	inTx(t, st, func(tx pgx.Tx) {
		for _, payload := range []OutboxPayload{
			SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RolePlanner, Generation: 1},
			WorkspaceRemove{Linger: 1},
		} {
			row, err := NewOutboxRow("LEGION-208", payload, now)
			must(t, err)
			must(t, records.Enqueue(ctx, tx, row))
		}
		must(t, tx.QueryRow(ctx, "select min(id), max(id) from outbox where issue = 'LEGION-208'").Scan(&suspendID, &removalID))
	})

	// Daemon A's lane claims the suspend and is a moment from committing.
	txA, err := st.BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	_, okA, err := records.ClaimRow(ctx, txA, suspendID, now, time.Minute)
	must(t, err)
	if !okA {
		t.Fatal("the first daemon's claim of the suspend was refused with nothing leased")
	}
	// Daemon B's lane, whose list also showed both rows, claims the removal in that moment. Its
	// claim either returns — before A commits, it must then have leased nothing — or waits on A's
	// transaction, which commits once B is seen waiting. A claim refused by an error of its own
	// (a serialization failure at commit) leased nothing either; the test checks what is leased.
	type attempt struct {
		ok  bool
		err error
	}
	second := make(chan attempt, 1)
	go func() {
		txB, err := st.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			second <- attempt{err: err}
			return
		}
		defer txB.Rollback(ctx)
		_, ok, err := records.ClaimRow(ctx, txB, removalID, now, time.Minute)
		if err == nil {
			err = txB.Commit(ctx)
		}
		second <- attempt{ok: ok && err == nil, err: err}
	}()
	var got attempt
	returned := false
	testwait.Eventually(t, "the second daemon's claim to return or to wait on the first's transaction", func() bool {
		select {
		case got = <-second:
			returned = true
			return true
		default:
		}
		var waiting int
		err := st.Pool().QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting)
		return err == nil && waiting > 0
	})
	must(t, txA.Commit(ctx))
	if !returned {
		got = <-second
	}
	if got.err != nil {
		t.Logf("the second daemon's claim was refused by an error: %v", got.err)
	}

	var leased []string
	inTx(t, st, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, "select kind from outbox where issue = 'LEGION-208' and lease_until > $1 order by id", now)
		must(t, err)
		defer rows.Close()
		for rows.Next() {
			var kind string
			must(t, rows.Scan(&kind))
			leased = append(leased, kind)
		}
		must(t, rows.Err())
	})
	if got.ok || len(leased) != 1 {
		t.Fatalf("second daemon's claim of the removal = %v and the issue's lifecycle rows under a live lease = %v; want the claim refused and the suspend leased alone", got.ok, leased)
	}
}

// The lock a lifecycle claim takes on its issue is the lifecycle lane's alone: a board row of the
// issue, claimed while the lifecycle claim is still uncommitted, is leased at once rather than
// waiting on it. Board rows are not under the lane rule, and an issue's board lane never waits on
// its lifecycle claim.
func TestABoardRowClaimedWhileItsIssuesLifecycleClaimIsUncommittedIsLeasedAtOnce(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var suspendID, statusID int64
	inTx(t, st, func(tx pgx.Tx) {
		for _, payload := range []OutboxPayload{
			SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RolePlanner, Generation: 1},
			StatusWrite{Status: "done"},
		} {
			row, err := NewOutboxRow("LEGION-208", payload, now)
			must(t, err)
			must(t, records.Enqueue(ctx, tx, row))
		}
		must(t, tx.QueryRow(ctx, "select min(id), max(id) from outbox where issue = 'LEGION-208'").Scan(&suspendID, &statusID))
	})

	// Daemon A's lifecycle lane claims the suspend and is a moment from committing.
	txA, err := st.BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	_, okA, err := records.ClaimRow(ctx, txA, suspendID, now, time.Minute)
	must(t, err)
	if !okA {
		t.Fatal("the lifecycle lane's claim of the suspend was refused with nothing leased")
	}
	// The board lane claims the status row in that moment. The claim runs apart so a claim that
	// waits on A's transaction is reported as the failure it is, rather than hanging the test: the
	// wait shows in pg_stat_activity, and A commits nothing until the claim has returned.
	type attempt struct {
		ok  bool
		err error
	}
	second := make(chan attempt, 1)
	go func() {
		txB, err := st.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			second <- attempt{err: err}
			return
		}
		defer txB.Rollback(ctx)
		_, ok, err := records.ClaimRow(ctx, txB, statusID, now, time.Minute)
		if err == nil {
			err = txB.Commit(ctx)
		}
		second <- attempt{ok: ok && err == nil, err: err}
	}()
	var got attempt
	returned := false
	testwait.Eventually(t, "the board lane's claim to return or to wait on the lifecycle claim's transaction", func() bool {
		select {
		case got = <-second:
			returned = true
			return true
		default:
		}
		var waiting int
		err := st.Pool().QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting)
		return err == nil && waiting > 0
	})
	if !returned {
		must(t, txA.Rollback(ctx))
		t.Fatal("the board lane's claim of the status row waited on a lock while the lifecycle claim was uncommitted; want it leased at once")
	}
	if got.err != nil || !got.ok {
		t.Fatalf("the board lane's claim of the status row = %v, %v; want it leased while the lifecycle claim is uncommitted", got.ok, got.err)
	}
	must(t, txA.Commit(ctx))

	var leased []string
	inTx(t, st, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, "select kind from outbox where issue = 'LEGION-208' and lease_until > $1 order by id", now)
		must(t, err)
		defer rows.Close()
		for rows.Next() {
			var kind string
			must(t, rows.Scan(&kind))
			leased = append(leased, kind)
		}
		must(t, rows.Err())
	})
	if want := []string{"supervise", "dispatch_status"}; !reflect.DeepEqual(leased, want) {
		t.Fatalf("the issue's rows under a live lease = %v, want %v: the lifecycle lane's suspend and the board lane's status write both leased", leased, want)
	}
}

// A start of an issue's role waits while an older issue_branch row of the issue is unfinished, so no
// role starts before its branch exists on GitHub. Nothing else waits on it: a suspend of the same
// issue, a start queued before the row, and another issue's start are each due.
func TestAStartWaitsForItsIssuesBranch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueue := func(tx pgx.Tx, issue string, payload OutboxPayload) {
		row, err := NewOutboxRow(issue, payload, now)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
	}
	start := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1}
	inTx(t, st, func(tx pgx.Tx) {
		enqueue(tx, "LEGION-208", start)
		enqueue(tx, "LEGION-208", IssueBranch{Generation: 1})
		enqueue(tx, "LEGION-208", start)
		enqueue(tx, "LEGION-208", SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RolePlanner, Generation: 1})
		enqueue(tx, "LEGION-209", SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleArchitect, Generation: 1})
	})
	// claimed is every row due at at, in claim order, each claimed and finished before the next is
	// claimed, as a lane runs its rows: a lifecycle row's lease holds its issue's other lifecycle rows
	// back until it settles.
	claimed := func(at time.Time) []string {
		var keys []string
		inTx(t, st, func(tx pgx.Tx) {
			rows, err := records.DueRows(ctx, tx, "LEGION", "", nil, at)
			must(t, err)
			keys = outboxKeys(rows)
			for _, row := range rows {
				leased, ok, err := records.ClaimRow(ctx, tx, row.ID, at, time.Minute)
				must(t, err)
				if !ok {
					t.Fatalf("row %d was listed due but not claimed", row.ID)
				}
				must(t, records.FinishOutbox(ctx, tx, leased.ID, leased.LeaseToken))
			}
		})
		return keys
	}

	if got, want := claimed(now), []string{"supervise:LEGION-208:start", "issue_branch:LEGION-208", "supervise:LEGION-208:suspend", "supervise:LEGION-209:start"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claimed with the branch unfinished = %v, want %v", got, want)
	}
	if got, want := claimed(now.Add(2*time.Minute)), []string{"supervise:LEGION-208:start"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claimed once the branch finished = %v, want the start that waited for it alone", got)
	}
}

// A status write the outbox failed and pushed back is still unwritten, so it stays listed through its
// backoff; a finished one is deleted and gone, and a row of another kind is never listed. The list
// runs oldest first, the order an issue's statuses run in, so a newer write queued behind a
// backing-off one is listed after it rather than ahead of it as a due write.
func TestPendingStatusWritesListsEveryUnfinishedStatusEffect(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindDispatchStatus, Issue: "LEGION-208", Payload: json.RawMessage(`{"status":"testing"}`), NextAt: now}))
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindDispatchStatus, Issue: "LEGION-209", Payload: json.RawMessage(`{"status":"retro"}`), NextAt: now}))
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindNotice, Issue: "LEGION-208", Payload: json.RawMessage(`{"kind":"phase-finished"}`), NextAt: now}))
	})

	// LEGION-208's write fails and backs off a minute, as outbox.retry records it; LEGION-209's lands.
	backoffUntil := now.Add(time.Minute)
	inTx(t, st, func(tx pgx.Tx) {
		rows := claimEachDue(t, records, tx, "LEGION", now)
		if len(rows) != 3 {
			t.Fatalf("claimed %#v, want all three rows", rows)
		}
		// Leased for an attempt in flight, both status writes are still unfinished.
		leased, err := records.PendingStatusWrites(ctx, tx, "LEGION")
		must(t, err)
		if len(leased) != 2 || leased[0].Issue != "LEGION-208" || leased[1].Issue != "LEGION-209" {
			t.Fatalf("pending status writes while leased = %#v, want the LEGION-208 and LEGION-209 status rows", leased)
		}
		for _, row := range rows {
			switch {
			case row.Kind != OutboxKindDispatchStatus:
			case row.Issue == "LEGION-208":
				must(t, records.RetryOutbox(ctx, tx, row.ID, row.LeaseToken, backoffUntil, "Dispatch unavailable"))
			default:
				must(t, records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken))
			}
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindDispatchStatus, Issue: "LEGION-210", Payload: json.RawMessage(`{"status":"in_progress"}`), NextAt: now}))
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindDispatchStatus, Issue: "LEGION-208", Payload: json.RawMessage(`{"status":"needs_review"}`), NextAt: now}))
	})

	var pending []OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		pending, err = records.PendingStatusWrites(ctx, tx, "LEGION")
		must(t, err)
	})
	if len(pending) != 3 {
		t.Fatalf("pending status writes = %#v, want LEGION-208's backing-off write, LEGION-210's, then LEGION-208's queued write", pending)
	}
	if backingOff := pending[0]; backingOff.Issue != "LEGION-208" || backingOff.Kind != OutboxKindDispatchStatus ||
		backingOff.Attempts != 1 || backingOff.LastError != "Dispatch unavailable" || !backingOff.NextAt.Equal(backoffUntil) {
		t.Fatalf("first pending status write = %#v, want LEGION-208 after one failed attempt, next at %s", backingOff, backoffUntil)
	}
	if due := pending[1]; due.Issue != "LEGION-210" || due.Attempts != 0 || !due.NextAt.Equal(now) {
		t.Fatalf("second pending status write = %#v, want LEGION-210 due now and never attempted", due)
	}
	queued := pending[2]
	var queuedWrite StatusWrite
	must(t, json.Unmarshal(queued.Payload, &queuedWrite))
	if queued.Issue != "LEGION-208" || queued.Attempts != 0 || queuedWrite.Status != "needs_review" {
		t.Fatalf("third pending status write = %#v, want LEGION-208's needs_review write, queued behind its testing write", queued)
	}
}

// The waiting line is the slotless todo roots and orphans handed to Legion, in rank order: a root
// in todo without the label waits for nothing, however high it ranks.
func TestWaitingIncludesOnlySlotlessHandedOverTodoRootsAndOrphansInRankOrder(t *testing.T) {
	issues := []Issue{
		{Key: "LEGION-210", Tree: "LEGION-210", Status: "done", Rank: "00000", HandedOver: true},
		{Key: "LEGION-211", Tree: "LEGION-211", Status: "backlog", Rank: "00001", HandedOver: true},
		{Key: "LEGION-212", Tree: "LEGION-212", Status: "todo", Rank: "00004", LastDispatchSeq: 1, HandedOver: true},
		{Key: "LEGION-213", Tree: "LEGION-213", Status: "todo", Rank: "00003", LastDispatchSeq: 10, HandedOver: true},
		{Key: "LEGION-214", Tree: "LEGION-212", Status: "todo", Rank: "00002"},
		{Key: "LEGION-215", Tree: "LEGION-215", Status: "todo", Rank: "00000"},
	}
	got := Waiting(issues, nil)
	want := []Issue{issues[3], issues[2]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Waiting = %#v, want %#v", got, want)
	}
}

func TestRecordMigrationAppliesOverAPopulatedStageTwoDatabase(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	all, err := migrations.All()
	must(t, err)
	if len(all) < 6 {
		t.Fatalf("migrations = %d, want at least the three Stage 2 migrations and 0004 through 0006", len(all))
	}
	for _, migration := range all[:3] {
		inTx(t, st, func(tx pgx.Tx) {
			must(t, func() error {
				if _, err := tx.Exec(ctx, migration.SQL); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "insert into schema_version (version) values ($1)", migration.Version)
				return err
			}())
		})
	}
	// The claim is written in the Stage 2 schema's own columns: the store's PutClaim writes the
	// latest schema's, which a Stage 2 database does not have yet.
	stageTwo := supervise.Claim{Token: "legion-208-architect", Project: "LEGION", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect, State: supervise.StateSuspended}
	inTx(t, st, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, $3, $4, $5, 0, '', '', $6, 0, 0, 0, 0)`,
			string(stageTwo.Token), stageTwo.Project, stageTwo.Tree, stageTwo.Issue, string(stageTwo.Role), string(stageTwo.State))
		must(t, err)
	})

	applied, err := st.Migrate(ctx)
	must(t, err)
	if applied != len(all)-3 {
		t.Fatalf("migrations applied = %d, want every one after the Stage 2 three (%d)", applied, len(all)-3)
	}
	claims, err := st.Claims(ctx)
	must(t, err)
	if len(claims) != 1 || claims[0].Token != stageTwo.Token {
		t.Fatalf("Stage 2 claim after record migration = %#v, want %#v", claims, stageTwo)
	}
}

func TestRecordMigrationCreatesTheRequiredColumns(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	want := map[string][]string{
		"issues":           {"key", "tree", "project", "title", "parent", "phase", "generation", "status", "rank", "handed_over", "linger_until", "held_from", "last_dispatch_seq", "ready_pending_version", "hold_reason", "dispatch_status"},
		"phases":           {"issue", "role", "claim", "handoff_commit", "rounds", "verdict", "summary", "last_handoff", "decision", "completed_at", "merge_note"},
		"pull_requests":    {"issue", "repo", "number", "branch", "head_sha", "head_updated_at", "failing", "cancelled", "fix_attempts", "blocked_attempts", "check_runs", "generation", "snapshot", "pushes", "head_counted", "head_counted_before", "planned_red", "review_seen", "review_seen_at", "state", "checked_head", "required", "required_workflows", "required_workflows_head", "mergeability"},
		"design_gates":     {"issue", "artifact_id", "latest_version", "approved_version"},
		"slots":            {"issue", "index", "admitted_at"},
		"processed_events": {"source", "event_id", "processed_at"},
		"outbox":           {"id", "kind", "issue", "payload", "attempts", "next_at", "last_error", "created_at", "lease_token", "lease_until"},
	}
	inTx(t, st, func(tx pgx.Tx) {
		for table, expected := range want {
			rows, err := tx.Query(ctx, `select column_name from information_schema.columns where table_schema = 'public' and table_name = $1`, table)
			must(t, err)
			var got []string
			for rows.Next() {
				var name string
				must(t, rows.Scan(&name))
				got = append(got, name)
			}
			must(t, rows.Err())
			rows.Close()
			sort.Strings(got)
			sort.Strings(expected)
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("%s columns = %v, want %v", table, got, expected)
			}
		}
	})
}

// Migration 0019 moves the newest deciding review off the reviewer's phase row onto the pull
// request, so a database the daemon ran on before it keeps ordering the reviews it has seen, and
// the round's decision keeps what it decided without the id it no longer carries.
func TestReviewOrderMigrationCarriesTheReviewersMarkToThePullRequest(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	all, err := migrations.All()
	must(t, err)
	for _, migration := range all {
		if migration.Version >= 19 {
			break
		}
		inTx(t, st, func(tx pgx.Tx) {
			must(t, func() error {
				if _, err := tx.Exec(ctx, migration.SQL); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "insert into schema_version (version) values ($1)", migration.Version)
				return err
			}())
		})
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, statement := range []string{
			`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ('LEGION-208', 'LEGION-208', 'LEGION', 'the issue', 'reviewing', 1, 'needs_review', 'U', 0)`,
			`insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, head_updated_at_source,
				verdict, failing, failing_statuses, fix_attempts, blocked_attempts, check_runs, generation, snapshot,
				reconciled, pushes, head_counted, planned_red, state)
				values ('LEGION-208', 'sjawhar/legion', 42, 'legion/LEGION-208', 'head', now(), 'webhook', '', '[]', '[]',
				0, 0, '[]', 0, '', false, '[]', '', false, 'open')`,
			`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, last_handoff, decision, review_seen)
				values ('LEGION-208', 'reviewer', 'review-claim', '', 0, '', '',
				'{"state": "approved", "body": "ship it", "head": "head", "id": 12}', 12)`,
		} {
			_, err := tx.Exec(ctx, statement)
			must(t, err)
		}
	})

	_, err = st.Migrate(ctx)
	must(t, err)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		pr, err := records.PullRequest(ctx, tx, "LEGION-208")
		must(t, err)
		if pr == nil || pr.ReviewSeen.ID != 12 || !pr.ReviewSeen.SubmittedAt.IsZero() {
			t.Fatalf("pull request after 0019 = %+v, want the reviewer's mark 12 with no submission time", pr)
		}
		phases, err := records.Phases(ctx, tx, "LEGION-208")
		must(t, err)
		want := &ReviewDecision{State: "approved", Body: "ship it", Head: "head"}
		if len(phases) != 1 || !reflect.DeepEqual(phases[0].Decision, want) {
			t.Fatalf("reviewer phase after 0019 = %+v, want the decision %+v", phases, want)
		}
		var keepsID bool
		must(t, tx.QueryRow(ctx, `select decision ? 'id' from phases where issue = 'LEGION-208'`).Scan(&keepsID))
		if keepsID {
			t.Fatal("the stored decision still carries its review id")
		}
	})
}

// Migration 0024 records whose settlement a pull request's verdict is. A row recorded before it
// held its own head's settlement, since a new head cleared the verdict, so its checked head is its
// head: a settlement recorded before the upgrade keeps standing for the head it settled. Migration
// 0028 then keeps a checked head only where a verdict was recorded, since a checked head now means
// a settlement stands: a row 0024 gave its own head with nothing settled has none.
func TestCheckedHeadMigrationKeepsARecordedVerdictStandingForItsHead(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	all, err := migrations.All()
	must(t, err)
	for _, migration := range all {
		if migration.Version >= 24 {
			break
		}
		inTx(t, st, func(tx pgx.Tx) {
			must(t, func() error {
				if _, err := tx.Exec(ctx, migration.SQL); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "insert into schema_version (version) values ($1)", migration.Version)
				return err
			}())
		})
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, statement := range []string{
			`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ('LEGION-208', 'LEGION-208', 'LEGION', 'the issue', 'reviewing', 1, 'needs_review', 'U', 0)`,
			`insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, head_updated_at_source,
				verdict, failing, failing_statuses, fix_attempts, blocked_attempts, check_runs, generation, snapshot,
				reconciled, pushes, head_counted, planned_red, state)
				values ('LEGION-208', 'sjawhar/legion', 42, 'legion/LEGION-208', 'head', now(), 'webhook', 'green', '[]', '[]',
				0, 0, '[{"name": "ci", "id": 7}]', 1, 'green-head', false, '[]', '', false, 'open')`,
			`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ('LEGION-209', 'LEGION-209', 'LEGION', 'unsettled', 'testing', 1, 'testing', 'U', 0)`,
			`insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at, head_updated_at_source,
				verdict, failing, failing_statuses, fix_attempts, blocked_attempts, check_runs, generation, snapshot,
				reconciled, pushes, head_counted, planned_red, state)
				values ('LEGION-209', 'sjawhar/legion', 43, 'legion/LEGION-209', 'fresh', now(), 'webhook', '', '[]', '[]',
				0, 0, '[]', 0, '', false, '[]', '', false, 'open')`,
		} {
			_, err := tx.Exec(ctx, statement)
			must(t, err)
		}
	})

	_, err = st.Migrate(ctx)
	must(t, err)
	inTx(t, st, func(tx pgx.Tx) {
		pr, err := NewStore().PullRequest(ctx, tx, "LEGION-208")
		must(t, err)
		if pr == nil || pr.CheckedHead != "head" || len(pr.CheckRuns) != 1 || pr.Required != nil {
			t.Fatalf("pull request after 0028 = %+v, want checked head \"head\" with its settlement, and no required set read", pr)
		}
		unsettled, err := NewStore().PullRequest(ctx, tx, "LEGION-209")
		must(t, err)
		if unsettled == nil || unsettled.CheckedHead != "" {
			t.Fatalf("never-settled pull request after 0028 = %+v, want no checked head", unsettled)
		}
	})
}

// Migration 0035 ends phase-end suspension. A transition's suspend queued before it named the phase
// it ended ("leaves"); the field is gone, and the outbox decodes rows strictly, so each such row is
// deleted rather than left to fail on every attempt or, read without the field, stop a role its
// issue still needs. Every other supervise row stays queued and decodes: a close's suspend, a start
// and a tree close.
func TestResidentRolesMigrationDropsOnlyTheQueuedPhaseEndSuspends(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	all, err := migrations.All()
	must(t, err)
	for _, migration := range all {
		if migration.Version >= 35 {
			break
		}
		inTx(t, st, func(tx pgx.Tx) {
			must(t, func() error {
				if _, err := tx.Exec(ctx, migration.SQL); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "insert into schema_version (version) values ($1)", migration.Version)
				return err
			}())
		})
	}
	queued := map[string]string{
		"phase-end suspend": `{"op": "suspend", "tree": "LEGION-208", "role": "planner", "generation": 1, "leaves": "planning", "reason": "LEGION-208 left planning"}`,
		"close suspend":     `{"op": "suspend", "tree": "LEGION-208", "role": "implementer", "generation": 1, "reason": "the tree of LEGION-208 lingers"}`,
		"start":             `{"op": "start", "tree": "LEGION-208", "role": "implementer", "generation": 1, "phase": "implementing", "task": "Continue Workflow."}`,
		"tree close":        `{"op": "tree_close", "tree": "LEGION-208", "role": "tester", "generation": 1, "linger": 1}`,
	}
	inTx(t, st, func(tx pgx.Tx) {
		for name, payload := range queued {
			_, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ('supervise', 'LEGION-208', $1, 1, now(), $2)`, payload, name)
			must(t, err)
		}
	})

	_, err = st.Migrate(ctx)
	must(t, err)
	inTx(t, st, func(tx pgx.Tx) {
		rows, err := NewStore().DueRows(ctx, tx, "LEGION", "", nil, time.Now().Add(time.Hour))
		must(t, err)
		kept := map[string]SuperviseRequest{}
		for _, row := range rows {
			payload, err := DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("the %s row after 0035 does not decode: %v", row.LastError, err)
			}
			kept[row.LastError] = payload.(SuperviseRequest)
		}
		want := map[string]SuperviseRequest{
			"close suspend": {Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, Reason: "the tree of LEGION-208 lingers"},
			"start":         {Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, Task: "Continue Workflow."},
			"tree close":    {Op: "tree_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 1, Linger: 1},
		}
		if !reflect.DeepEqual(kept, want) {
			t.Fatalf("supervise rows after 0035 = %+v, want %+v: the phase-end suspend deleted and every other row kept", kept, want)
		}
	})
}

// A queued supervise row that does not decode belongs to one role's run. Reading another role's run
// of the same issue — what the state route's report does for every candidate — skips it before
// decoding, so one bad row fails only its own role, never the state of the whole issue.
func TestRoleRunsSkipsAnotherRolesUndecodableRow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	start := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RolePlanner, Generation: 1, Phase: phase.Planning}
	inTx(t, st, func(tx pgx.Tx) {
		row, err := NewOutboxRow("LEGION-208", start, time.Now())
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
		_, err = tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('supervise', 'LEGION-208', '{"op": "bogus", "tree": "LEGION-208", "role": "tester", "generation": 1}', 0, now(), '')`)
		must(t, err)
	})
	planner := RoleRunKey{Token: "legion-legion-legion-208-planner", Issue: "LEGION-208", Role: claim.RolePlanner, Generation: 1}
	tester := RoleRunKey{Token: "legion-legion-legion-208-tester", Issue: "LEGION-208", Role: claim.RoleTester, Generation: 1}
	inTx(t, st, func(tx pgx.Tx) {
		runs, err := records.RoleRuns(ctx, tx, []RoleRunKey{planner})
		if err != nil {
			t.Fatalf("the planner's run beside the tester's undecodable row = %v, want it read", err)
		}
		if queued := runs[planner.Token].Queued; len(queued) != 1 || queued[0].Request != start {
			t.Fatalf("the planner's queued rows = %+v, want its one start", queued)
		}
		if _, err := records.RoleRuns(ctx, tx, []RoleRunKey{tester}); err == nil {
			t.Fatal("the tester's run read its own undecodable row without an error")
		}
	})
}
