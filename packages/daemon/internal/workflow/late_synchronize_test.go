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
	"github.com/sjawhar/legion/daemon/internal/phase"
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

// A red that settles at a head whose arrival waits for a late synchronize is held; the late
// synchronize then names the skipped move, the head run closes, and the red is decided at once, as
// the push that closed a run would decide it (decideChecks): the issue in reviewing goes back to
// implementing with checks-red, whichever of the late synchronize and the red comes first, as it
// does with no move named. A synchronize never delivered leaves the red held until the hold lapses,
// when the required-checks pass decides it.
func TestARedHeldForALateSynchronizeIsDecidedWhenItArrives(t *testing.T) {
	const dev = "legion-implementer[bot]"
	skipped, arrived := time.Date(2026, 9, 22, 23, 50, 0, 0, time.UTC), time.Date(2026, 9, 22, 23, 51, 0, 0, time.UTC)
	pushes := []intake.Fact{sentPush("head", "h1", dev), sentPush("h1", "h2", dev), namedSync("h1", "h2", dev, arrived)}
	red := settledAt("h2", "ci", 2, "ci")
	late := namedSync("head", "h1", dev, skipped)
	for _, tc := range []struct {
		name  string
		facts []intake.Fact
		pass  bool
	}{
		{"the late synchronize after the red", []intake.Fact{red, late}, false},
		{"the late synchronize before the red", []intake.Fact{late, red}, false},
		{"the synchronize never delivered", []intake.Fact{red}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "head", "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			applyRefusingNothing(t, pool, engine, pushes...)
			applyRefusingNothing(t, pool, engine, tc.facts...)
			if tc.pass {
				if got := issuePhase(t, pool); got != phase.Reviewing {
					t.Fatalf("the red held while its arrival waits: phase %s, want reviewing", got)
				}
				later := readyEngineAt("", time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC).Add(redHold+time.Minute))
				applyRefusingNothing(t, pool, later, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{"ci"}})
			}
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("phase %s, want implementing", got)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); !containsNotice(got, "checks-red") {
				t.Fatalf("notices %v, want checks-red", got)
			}
		})
	}
}

// The push handler records each push's sender, and the synchronize's sender picks its move's push
// by it: two pushes of head..h1 on their way, the review App's first and the implementer's second,
// and h1's synchronize naming the implementer. The implementer's code onto the red counts a fix
// attempt; the review App's push, which a push recorded with no sender would let stand for any,
// would spare it.
func TestASynchronizesSenderPicksItsPushInTheWorkflow(t *testing.T) {
	const dev = "legion-implementer[bot]"
	pool := migratedPool(t)
	seedReviewOf(t, pool, "head", "red")
	engine := testEngine(config.DesignGateRootIssues, nil)
	before := probePullRequest(t, pool)
	applyRefusingNothing(t, pool, engine, sentPush("head", "h1", testReviewApp), sentPush("head", "h1", dev),
		namedSync("head", "h1", dev, time.Date(2026, 9, 22, 23, 51, 0, 0, time.UTC)))
	after := probePullRequest(t, pool)
	if after.HeadSHA != "h1" || after.Run != nil || after.FixAttempts != before.FixAttempts+1 || after.PlannedRed {
		t.Fatalf("at %s, run open %v, fix=%d planned=%v; want h1, closed, fix=%d and unplanned: the implementer's push", after.HeadSHA, after.Run != nil, after.FixAttempts, after.PlannedRed, before.FixAttempts+1)
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
