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

// A synchronize older than the newest applied (classify.LateLifecycle) moves no head, opens and
// reopens no head run, and wakes nobody. With no head run open, or with one none of whose arrivals
// it can be a skipped move of (classify.NameMove), it changes nothing at all: every table reads as
// before it. With an arrival that waits for it - a synchronize said the branch moved from a head
// the record never held - it records the move's push by its sender on the head run and replays the
// run, which can close it; no other table changes.
func TestALateSynchronizeOnlyNamesTheMoveAWaitingArrivalTook(t *testing.T) {
	const dev = "legion-implementer[bot]"
	skipped, arrived := lateApplied.Add(time.Minute), lateApplied.Add(2*time.Minute)
	for _, tc := range []struct {
		name    string
		facts   []intake.Fact
		late    intake.Fact
		changed []string
		closed  bool
	}{
		{name: "with no head run open",
			late: namedSync("head-b", "head-x", dev, lateApplied.Add(-time.Minute)), changed: nil},
		{name: "older than every arrival of the head run",
			facts: []intake.Fact{namedSync("head-x", "head-d", dev, arrived)},
			late:  namedSync("head-b", "head-x", dev, lateApplied.Add(-time.Minute)), changed: nil},
		{name: "named already",
			facts: []intake.Fact{namedSync("head-x", "head-d", dev, arrived), namedSync("head-c", "head-x", dev, skipped)},
			late:  namedSync("head-c", "head-x", dev, skipped), changed: nil},
		{name: "the move an arrival waits for, its pushes still to come",
			facts: []intake.Fact{namedSync("head-x", "head-d", dev, arrived)},
			late:  namedSync("head-c", "head-x", dev, skipped), changed: []string{"pull_requests"}},
		{name: "the move an arrival waits for, its pushes recorded",
			facts: []intake.Fact{namedSync("head-x", "head-d", dev, arrived), sentPush("head-c", "head-x", dev), sentPush("head-x", "head-d", dev)},
			late:  namedSync("head-c", "head-x", dev, skipped), changed: []string{"pull_requests"}, closed: true},
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
			if got := changedTables(before, after); !slices.Equal(got, tc.changed) {
				t.Fatalf("the late synchronize changed %v, want %v", got, tc.changed)
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
