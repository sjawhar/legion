package workflow

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// databaseRows is every table's rows, each table's as one sorted JSON text: what a fact changed,
// and that it sent or queued nothing, read off the whole database.
func databaseRows(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := context.Background()
	tables, err := pool.Query(ctx, `select table_name from information_schema.tables where table_schema = current_schema() and table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	names, err := pgx.CollectRows(tables, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	rows := map[string]string{}
	for _, name := range names {
		var text string
		query := fmt.Sprintf(`select coalesce(jsonb_agg(to_jsonb(x) order by to_jsonb(x)::text), '[]'::jsonb)::text from %s x`, pgx.Identifier{name}.Sanitize())
		if err := pool.QueryRow(ctx, query).Scan(&text); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		rows[name] = text
	}
	return rows
}

// changedTables is the tables whose rows differ between before and after.
func changedTables(before, after map[string]string) []string {
	var changed []string
	for _, name := range slices.Sorted(maps.Keys(after)) {
		if before[name] != after[name] {
			changed = append(changed, name)
		}
	}
	return changed
}

// namedSync is a synchronize from a listener that sends its before and its sender.
func namedSync(before, head, sender string, at time.Time) intake.Fact {
	return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Before: before, Sender: sender, HeadSHA: head, UpdatedAt: at}
}

// sentPush is a push of code by sender, the account the push's webhook names.
func sentPush(before, after, sender string) intake.Fact {
	push := pushedFromTo(before, after, "src/widget.go", sender).(intake.Push)
	push.Sender = sender
	return push
}

// A synchronize older than the newest applied (classify.LateLifecycle) moves no head and opens and
// reopens no head run. With no head run open, or with one none of whose arrivals it can be a
// skipped move of (classify.NameMove), it changes nothing at all: every table reads as before it.
// With an arrival that waits for it - a synchronize said the branch moved from a head the record
// never held - it records the move's push by its sender on the head run and replays the run, which
// can close it; the head stays where it was.
func TestALateSynchronizeOnlyNamesTheMoveAWaitingArrivalTook(t *testing.T) {
	const dev = "legion-implementer[bot]"
	skipped, arrived := lateApplied.Add(time.Minute), lateApplied.Add(2*time.Minute)
	for _, tc := range []struct {
		name    string
		facts   []intake.Fact
		late    intake.Fact
		changes bool
		closed  bool
	}{
		{name: "with no head run open",
			late: namedSync("head-b", "head-x", dev, lateApplied.Add(-time.Minute))},
		{name: "older than every arrival of the head run",
			facts: []intake.Fact{namedSync("head-x", "head-d", dev, arrived)},
			late:  namedSync("head-b", "head-x", dev, lateApplied.Add(-time.Minute))},
		{name: "named already",
			facts: []intake.Fact{namedSync("head-x", "head-d", dev, arrived), namedSync("head-c", "head-x", dev, skipped)},
			late:  namedSync("head-c", "head-x", dev, skipped)},
		{name: "the move an arrival waits for, its pushes still to come",
			facts: []intake.Fact{namedSync("head-x", "head-d", dev, arrived)},
			late:  namedSync("head-c", "head-x", dev, skipped), changes: true},
		{name: "the move an arrival waits for, its pushes recorded",
			facts: []intake.Fact{namedSync("head-x", "head-d", dev, arrived), sentPush("head-c", "head-x", dev), sentPush("head-x", "head-d", dev)},
			late:  namedSync("head-c", "head-x", dev, skipped), changes: true, closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := appliedEvents(t, record.PullRequestOpen, tc.facts...)
			before := databaseRows(t, pool)
			if _, err := intake.ApplyFact(context.Background(), pool, "github", "late", tc.late, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("apply the late synchronize: %v", err)
			}
			after := databaseRows(t, pool)
			delete(before, "processed_events")
			delete(after, "processed_events")
			changed := changedTables(before, after)
			if !tc.changes && len(changed) != 0 {
				t.Fatalf("the late synchronize changed %v, want nothing", changed)
			}
			if tc.changes && !slices.Contains(changed, "pull_requests") {
				t.Fatalf("the late synchronize changed %v, want the pull request's head run", changed)
			}
			var pr *record.PullRequest
			seedRecord(t, pool, func(tx pgx.Tx) error {
				var err error
				pr, err = record.NewStore().PullRequest(t.Context(), tx, "LEGION-208")
				return err
			})
			want := "head-c"
			if len(tc.facts) > 0 {
				want = "head-d"
			}
			if pr.HeadSHA != want || (pr.Run == nil) != (tc.closed || len(tc.facts) == 0) {
				t.Fatalf("pull request at %s with a head run open %v, want %s and open %v", pr.HeadSHA, pr.Run != nil, want, !tc.closed && len(tc.facts) > 0)
			}
		})
	}
}

// Two moves in one second of GitHub's clock, head..h1 and h1..h2, their synchronizes delivered in
// the reverse of their order: the older synchronize is not late by the clock (LateLifecycle), so it
// moves the record's head back to h1, as on main - the floor of two moves in one GitHub second
// delivered reversed. The pull request is judged from there: at h1, with h1's synchronize naming the
// move from head the record held, nothing waits.
func TestTwoMovesInOneSecondDeliveredReversedMoveTheHeadBackAsOnMain(t *testing.T) {
	const dev = "legion-implementer[bot]"
	second := time.Date(2026, 9, 22, 23, 51, 0, 0, time.UTC)
	pool := migratedPool(t)
	seedReviewOf(t, pool, "head", "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	applyRefusingNothing(t, pool, engine, sentPush("head", "h1", dev), sentPush("h1", "h2", dev), namedSync("h1", "h2", dev, second),
		namedSync("head", "h1", dev, second))
	if pr := probePullRequest(t, pool); pr.HeadSHA != "h1" {
		t.Fatalf("pull request at %s, want h1: the synchronize of one second delivered last moves the head, as on main", pr.HeadSHA)
	}
}

// No synchronize main would apply is dropped (classify.LateLifecycle decides lateness by GitHub's
// clock alone): a stale push to a head h2's waiting run awaits, then the branch's real move b..y in
// h2's second; and h1 awaited after a missed head..h1 synchronize, then a reset h2..head with both
// its webhooks missed and a real head..h1 in h2's second. The pull request moves to the head, as on
// main.
func TestNoSynchronizeMainWouldApplyIsDroppedInTheWorkflow(t *testing.T) {
	const dev = "legion-implementer[bot]"
	second := time.Date(2026, 9, 22, 23, 51, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		facts []intake.Fact
		head  string
	}{
		{"a stale push to an awaited head", []intake.Fact{namedSync("x", "h2", dev, second), sentPush("y", "x", dev), sentPush("b", "y", dev),
			namedSync("b", "y", dev, second)}, "y"},
		{"an awaited head reached again after a missed reset", []intake.Fact{sentPush("head", "h1", dev), sentPush("h1", "h2", dev), namedSync("h1", "h2", dev, second),
			sentPush("head", "h1", dev), namedSync("head", "h1", dev, second)}, "h1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "head", "green")
			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), tc.facts...)
			if pr := probePullRequest(t, pool); pr.HeadSHA != tc.head {
				t.Fatalf("pull request at %s, want %s: the synchronize moves the head, as on main", pr.HeadSHA, tc.head)
			}
		})
	}
}

// A move after a reset whose synchronize was missed moves the head though GitHub's clock puts its
// synchronize in the second of the newest applied and its head was held before: head..h1 and
// h1..h2 arrive, the branch is reset h2..head with its synchronize missed - its push delivered, or
// missed too - and moves head..h1 again in the second of h2's synchronize. The pull request is at h1,
// as on main, not left at h2.
func TestAMoveAfterAMissedResetInTheSameSecondMovesTheHead(t *testing.T) {
	const dev = "legion-implementer[bot]"
	first, second := time.Date(2026, 9, 22, 23, 50, 0, 0, time.UTC), time.Date(2026, 9, 22, 23, 51, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		reset []intake.Fact
	}{
		{"the reset's push delivered", []intake.Fact{sentPush("h2", "head", dev)}},
		{"the reset's push missed too", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "head", "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			applyRefusingNothing(t, pool, engine, sentPush("head", "h1", dev), namedSync("head", "h1", dev, first), sentPush("h1", "h2", dev), namedSync("h1", "h2", dev, second))
			applyRefusingNothing(t, pool, engine, tc.reset...)
			applyRefusingNothing(t, pool, engine, sentPush("head", "h1", dev), namedSync("head", "h1", dev, second))
			if pr := probePullRequest(t, pool); pr.HeadSHA != "h1" {
				t.Fatalf("pull request at %s, want h1: the move after the reset is a move", pr.HeadSHA)
			}
		})
	}
}

// The push handler records each push's sender, and the synchronize's sender picks its move's push
// by it: two pushes of head..h1 on their way, the review App's first and the implementer's
// second, and h1's synchronize naming the implementer. The implementer's push is the one the move
// takes, so the head run closes on it, and neither push is left recorded without its sender.
func TestASynchronizesSenderPicksItsPushInTheWorkflow(t *testing.T) {
	const dev = "legion-implementer[bot]"
	pool := migratedPool(t)
	seedReviewOf(t, pool, "head", "red")
	engine := testEngine(config.DesignGateRootIssues, nil)
	applyRefusingNothing(t, pool, engine, sentPush("head", "h1", testReviewApp), sentPush("head", "h1", dev),
		namedSync("head", "h1", dev, time.Date(2026, 9, 22, 23, 51, 0, 0, time.UTC)))
	after := probePullRequest(t, pool)
	if after.HeadSHA != "h1" || after.Run != nil {
		t.Fatalf("at %s, run open %v; want h1 with the run closed: the implementer's push", after.HeadSHA, after.Run != nil)
	}
	for _, p := range append(append([]record.ClassifiedPush{}, after.Pushes...), after.Moves[len(after.Moves)-1].Repeats...) {
		if p.Before == "head" && p.SHA == "h1" && p.Sender == "" {
			t.Fatalf("a push of head..h1 recorded with no sender: %+v", p)
		}
	}
}

// probePullRequest reads the pull request back.
func probePullRequest(t *testing.T, pool *pgxpool.Pool) record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		pr, err = record.NewStore().PullRequest(t.Context(), tx, "LEGION-208")
		return err
	})
	if pr == nil {
		t.Fatal("no pull request")
	}
	return *pr
}
