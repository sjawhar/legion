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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
	pending := &PendingPush{SHA: "b1c2d3", HandoffOnly: true, Unknown: "paths_truncated"}
	pr := PullRequest{
		Issue: issue.Key, Repo: "sjawhar/legion", Number: 1243, Branch: "legion/LEGION-208",
		HeadSHA: "b1c2d3", HeadUpdatedAt: updatedAt, HeadUpdatedAtSource: "pull_request.synchronize",
		Verdict: "failing", Failing: []string{"unit"}, FailingStatuses: []string{"unit / test"},
		ReviewDecision: "changes_requested", FixAttempts: 2, BlockedAttempts: 1,
		CheckRuns: []AttemptRun{{Name: "unit", ID: 91}, {Name: "lint", ID: 92}}, Generation: 4,
		Snapshot: "snapshot-4", Reconciled: true, PendingPush: pending, HeadCounted: "b1c2d3", State: PullRequestMerged,
	}
	phase := PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "legion-208-implementer", HandoffCommit: "aabbcc", Rounds: 2, Verdict: "pass"}
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
		must(t, records.DeletePullRequest(ctx, tx, issue.Key))
		must(t, records.ReleaseSlot(ctx, tx, issue.Key))
		pr, err := records.PullRequest(ctx, tx, issue.Key)
		must(t, err)
		if pr != nil {
			t.Fatalf("pull request after delete = %#v, want nil", pr)
		}
		slots, err := records.Slots(ctx, tx)
		must(t, err)
		if len(slots) != 0 {
			t.Fatalf("slots after release = %#v, want no rows", slots)
		}
	})
}

func TestStoreRoundTripsLingerStateAndRefusesHeldFromHeld(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	until := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	heldFrom := phase.Implementing
	lingering := issueFixture("LEGION-210")
	lingering.LingerUntil = &until
	lingering.HeldFrom = &heldFrom

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, lingering))
		got, err := records.Issue(ctx, tx, lingering.Key)
		must(t, err)
		if got == nil || got.LingerUntil == nil || !got.LingerUntil.Equal(until) || got.HeldFrom == nil || *got.HeldFrom != heldFrom {
			t.Fatalf("lingering issue state = %#v, want linger until %s from %s", got, until, heldFrom)
		}
	})

	cleared := lingering
	cleared.LingerUntil = nil
	cleared.HeldFrom = nil
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, cleared))
		got, err := records.Issue(ctx, tx, cleared.Key)
		must(t, err)
		if !reflect.DeepEqual(got, &cleared) {
			t.Fatalf("cleared issue = %#v, want %#v", got, cleared)
		}
	})

	invalid := issueFixture("LEGION-211")
	held := phase.Held
	invalid.HeldFrom = &held
	err := st.Tx(ctx, func(tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, invalid)
	})
	if err == nil {
		t.Fatal("PutIssue accepted held_from=held")
	}
}

func samePullRequest(got, want PullRequest) bool {
	return got.Issue == want.Issue && got.Repo == want.Repo && got.Number == want.Number &&
		got.Branch == want.Branch && got.HeadSHA == want.HeadSHA && got.HeadUpdatedAt.Equal(want.HeadUpdatedAt) &&
		got.HeadUpdatedAtSource == want.HeadUpdatedAtSource && got.Verdict == want.Verdict &&
		reflect.DeepEqual(got.Failing, want.Failing) && reflect.DeepEqual(got.FailingStatuses, want.FailingStatuses) &&
		got.ReviewDecision == want.ReviewDecision && got.FixAttempts == want.FixAttempts &&
		got.BlockedAttempts == want.BlockedAttempts && reflect.DeepEqual(got.CheckRuns, want.CheckRuns) &&
		got.Generation == want.Generation && got.Snapshot == want.Snapshot && got.Reconciled == want.Reconciled &&
		reflect.DeepEqual(got.PendingPush, want.PendingPush) && got.HeadCounted == want.HeadCounted
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

func TestOutboxLeaseFencesConcurrentWorkersAndExpires(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"status":"testing"}`)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.Enqueue(ctx, tx, OutboxRow{Kind: OutboxKindDispatchStatus, Issue: "LEGION-208", Payload: payload, NextAt: now}))
	})

	tx1, err := st.BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	rows1, err := records.ClaimDue(ctx, tx1, now, 1, time.Minute)
	must(t, err)
	if len(rows1) != 1 || rows1[0].LeaseToken == "" || rows1[0].LeaseUntil == nil {
		t.Fatalf("first claim = %#v, want one row with a lease", rows1)
	}

	tx2, err := st.BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	rows2, err := records.ClaimDue(ctx, tx2, now, 1, time.Minute)
	must(t, err)
	if len(rows2) != 0 {
		t.Fatalf("second concurrent claim = %#v, want no locked row", rows2)
	}
	must(t, tx2.Commit(ctx))
	must(t, tx1.Commit(ctx))

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.FinishOutbox(ctx, tx, rows1[0].ID, "stale-lease"))
		must(t, records.RetryOutbox(ctx, tx, rows1[0].ID, "stale-lease", now.Add(24*time.Hour), "stale"))
	})

	tx3, err := st.BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	rows3, err := records.ClaimDue(ctx, tx3, now.Add(2*time.Minute), 1, time.Minute)
	must(t, err)
	if len(rows3) != 1 || rows3[0].ID != rows1[0].ID {
		t.Fatalf("expired lease claim = %#v, want the original outbox row", rows3)
	}
	must(t, records.FinishOutbox(ctx, tx3, rows3[0].ID, rows3[0].LeaseToken))
	must(t, tx3.Commit(ctx))

	inTx(t, st, func(tx pgx.Tx) {
		rows, err := records.ClaimDue(ctx, tx, now.Add(3*time.Minute), 1, time.Minute)
		must(t, err)
		if len(rows) != 0 {
			t.Fatalf("finished row still due: %#v", rows)
		}
	})
}

// A status write the outbox failed and pushed back is still unwritten, so it stays listed through its
// backoff; a finished one is deleted and gone, and a row of another kind is never listed.
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
		rows, err := records.ClaimDue(ctx, tx, now, 10, time.Minute)
		must(t, err)
		if len(rows) != 3 {
			t.Fatalf("claimed %#v, want all three rows", rows)
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
	})

	var pending []OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		pending, err = records.PendingStatusWrites(ctx, tx)
		must(t, err)
	})
	if len(pending) != 2 {
		t.Fatalf("pending status writes = %#v, want the due LEGION-210 row then the backing-off LEGION-208 row", pending)
	}
	if due := pending[0]; due.Issue != "LEGION-210" || due.Attempts != 0 || !due.NextAt.Equal(now) {
		t.Fatalf("first pending status write = %#v, want LEGION-210 due now and never attempted", due)
	}
	if backingOff := pending[1]; backingOff.Issue != "LEGION-208" || backingOff.Kind != OutboxKindDispatchStatus ||
		backingOff.Attempts != 1 || backingOff.LastError != "Dispatch unavailable" || !backingOff.NextAt.Equal(backoffUntil) {
		t.Fatalf("second pending status write = %#v, want LEGION-208 after one failed attempt, next at %s", backingOff, backoffUntil)
	}
}

func TestWaitingIncludesOnlySlotlessTodoRootsAndOrphansInRankOrder(t *testing.T) {
	issues := []Issue{
		{Key: "LEGION-210", Tree: "LEGION-210", Status: "done", Rank: "00000"},
		{Key: "LEGION-211", Tree: "LEGION-211", Status: "backlog", Rank: "00001"},
		{Key: "LEGION-212", Tree: "LEGION-212", Status: "todo", Rank: "00004", LastDispatchSeq: 1},
		{Key: "LEGION-213", Tree: "LEGION-213", Status: "todo", Rank: "00003", LastDispatchSeq: 10},
		{Key: "LEGION-214", Tree: "LEGION-212", Status: "todo", Rank: "00002"},
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
	if len(all) != 6 {
		t.Fatalf("migrations = %d, want three Stage 2 migrations and 0004 through 0006", len(all))
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
	stageTwo := supervise.Claim{Token: "legion-208-architect", Project: "LEGION", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect, State: supervise.StateSuspended}
	must(t, st.PutClaim(ctx, stageTwo))

	applied, err := st.Migrate(ctx)
	must(t, err)
	if applied != 3 {
		t.Fatalf("migrations applied = %d, want only 0004 through 0006", applied)
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
		"issues":           {"key", "tree", "project", "title", "parent", "phase", "generation", "status", "rank", "linger_until", "held_from", "last_dispatch_seq", "ready_pending_version"},
		"phases":           {"issue", "role", "claim", "handoff_commit", "rounds", "verdict", "last_handoff"},
		"pull_requests":    {"issue", "repo", "number", "branch", "head_sha", "head_updated_at", "head_updated_at_source", "verdict", "failing", "failing_statuses", "review_decision", "fix_attempts", "blocked_attempts", "check_runs", "generation", "snapshot", "reconciled", "pending_push", "head_counted", "state"},
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
