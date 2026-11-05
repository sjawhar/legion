package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const (
	// runningTrees is the slotted roots the project runs, each with a live child.
	runningTrees = 350
	// finishedTrees is the done roots of the larger project, each with finishedChildren done
	// children: finishedTrees * (1 + finishedChildren) done records, every one with its mirror row.
	finishedTrees    = 2000
	finishedChildren = 4
	// stillWaiting is the waiting roots, and the held children, an unfinished Dispatch issue holds.
	stillWaiting = 5
	waitedOn     = "LEGION-BLOCKER"
)

func runningChild(i int) string { return fmt.Sprintf("LEGION-%d", 2000+i) }

// seedRunningProject records runningTrees slotted roots, each implementing with a live child
// implementing beside it, stillWaiting labelled todo roots waiting for a slot and stillWaiting
// children held at admitted in the first trees, the roots and the children all waiting on
// waitedOn, which Dispatch shows unfinished; and, when finished, finishedTrees done roots, one in
// four still lingering, each with its done children, every one with a mirror row.
func seedRunningProject(t *testing.T, pool *pgxpool.Pool, finished bool) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	inTx(t, pool, func(tx pgx.Tx) {
		put := func(issue record.Issue, waits ...string) {
			issue.Repo, issue.DefaultBranch, issue.GitHubRepo, issue.Project, issue.Title = "acme/widgets", "main", "acme/widgets", testProject, issue.Key
			issue.Generation, issue.Rank, issue.HandedOver = 1, "M", true
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				t.Fatalf("record %s: %v", issue.Key, err)
			}
			if err := records.PutDispatchIssue(ctx, tx, record.DispatchIssueRow{Key: issue.Key, Project: testProject, Status: issue.Status, Blockers: waits, Seq: 1}); err != nil {
				t.Fatalf("mirror %s: %v", issue.Key, err)
			}
		}
		if err := records.PutDispatchIssue(ctx, tx, record.DispatchIssueRow{Key: waitedOn, Project: testProject, Status: "todo", Seq: 1}); err != nil {
			t.Fatalf("mirror %s: %v", waitedOn, err)
		}
		for i := range runningTrees {
			root := slottedKey(i)
			put(record.Issue{Key: root, Tree: root, Phase: phase.Implementing, Status: "in_progress"})
			put(record.Issue{Key: runningChild(i), Tree: root, Parent: record.ParentOf(root), Phase: phase.Implementing, Status: "in_progress"})
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: i, AdmittedAt: fixedNow}); err != nil {
				t.Fatalf("slot %s: %v", root, err)
			}
		}
		for i := range stillWaiting {
			waiting := fmt.Sprintf("LEGION-%d", 3000+i)
			put(record.Issue{Key: waiting, Tree: waiting, Phase: phase.Admitted, Status: "todo"}, waitedOn)
			held := fmt.Sprintf("LEGION-%d", 3100+i)
			put(record.Issue{Key: held, Tree: slottedKey(i), Parent: record.ParentOf(slottedKey(i)), Phase: phase.Admitted, Status: "todo"}, waitedOn)
		}
		if !finished {
			return
		}
		lingering := fixedNow.Add(time.Hour)
		for i := range finishedTrees {
			root := fmt.Sprintf("LEGION-%d", 10000+i*(1+finishedChildren))
			done := record.Issue{Key: root, Tree: root, Phase: phase.Done, Status: "done"}
			if i%4 == 0 {
				done.LingerUntil = &lingering
			}
			put(done)
			for c := 1; c <= finishedChildren; c++ {
				child := fmt.Sprintf("LEGION-%d", 10000+i*(1+finishedChildren)+c)
				put(record.Issue{Key: child, Tree: root, Parent: record.ParentOf(root), Phase: phase.Done, Status: "done"})
			}
		}
	})
}

// A project keeps the record of every issue it ever finished, and its mirror row, for good, while
// what it runs at once is bounded by admission's cap. Every fact runs admission under the one fact
// lock, so a fact that read every record would cost more with each tree the project finishes, until
// the consumers fall behind Dispatch and GitHub. Each kind of fact the daemon applies all day — a
// pull request opened, pushed to, settled and reviewed, an observation of a running root, one of an
// issue nobody handed to Legion (which asks whether a slot stands free), and the controller tick —
// reads the same statements and the same rows with 10,000 finished records as with none.
func TestAFactReadsAsManyRowsWhateverNumberOfFinishedRecordsTheProjectKeeps(t *testing.T) {
	branch := "legion/" + runningChild(0)
	facts := []struct {
		name, source string
		fact         intake.Fact
	}{
		{"a pull request opened", "github", intake.PullRequestOpened{Repo: "acme/widgets", Number: 7, Branch: branch, HeadSHA: "a1", Body: "Dispatch: " + runningChild(0),
			URL: "https://github.com/acme/widgets/pull/7", UpdatedAt: fixedNow}},
		{"a push to it", "github", intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 7, Branch: branch, HeadSHA: "a2", UpdatedAt: fixedNow.Add(time.Minute)}},
		{"its checks settled", "github", intake.PullRequestChecks{Repo: "acme/widgets", Number: 7, HeadSHA: "a2", CheckRuns: []intake.CheckRun{{Name: "unit", ID: 70}},
			Snapshot: "settled-a2", Failing: []string{}, Cancelled: []string{}}},
		{"a review of it", "github", intake.PullRequestReview{Repo: "acme/widgets", Number: 7, ID: 9, SubmittedAt: fixedNow.Add(2 * time.Minute), State: "commented",
			CommitID: "a2", HeadSHA: "a2", Author: "someone", Body: "a comment"}},
		{"an observation of a running root", "dispatch", intake.DispatchIssue{Key: slottedKey(0), Seq: 5, Type: "issue.updated", Status: "in_progress",
			Title: slottedKey(0), Rank: "M", HandedOver: handed, ResolvedRepo: "acme/widgets"}},
		{"an issue nobody handed to Legion moved to todo", "dispatch", intake.DispatchIssue{Key: "LEGION-9", Seq: 5, Type: "issue.updated", Status: "todo",
			Title: "someone else's", Rank: "Z", ResolvedRepo: "acme/widgets"}},
		{"the controller tick", "controller", intake.ControllerTick{}},
	}
	type cost struct{ statements, rows int64 }
	run := func(t *testing.T, finished bool) ([]cost, *pgxpool.Pool) {
		t.Helper()
		pool := migratedPool(t)
		seedRunningProject(t, pool, finished)
		registerController(t, pool)
		counter := &statementCounter{}
		counted := countedPool(t, pool, counter)
		admission := newAdmission(t, runningTrees+10, slog.New(slog.NewTextHandler(io.Discard, nil)))
		handlers := []intake.Handler{blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission}
		costs := make([]cost, len(facts))
		for i, tc := range facts {
			counter.sent.Store(0)
			counter.rows.Store(0)
			if _, err := intake.ApplyFact(context.Background(), counted, tc.source, fmt.Sprintf("fact-%d", i), tc.fact, handlers...); err != nil {
				t.Fatalf("apply %s: %v", tc.name, err)
			}
			costs[i] = cost{counter.sent.Load(), counter.rows.Load()}
		}
		return costs, pool
	}

	none, _ := run(t, false)
	many, pool := run(t, true)
	for i, tc := range facts {
		if many[i] != none[i] {
			t.Errorf("%s: %d statements reading %d rows with %d finished records, %d reading %d with none; want as many",
				tc.name, many[i].statements, many[i].rows, finishedTrees*(1+finishedChildren), none[i].statements, none[i].rows)
		}
	}

	// The facts did their work on the larger project: the pull request is recorded at its new head,
	// and the controller was woken for the free slot and by the tick, the tick naming every root and
	// child the unfinished issue holds.
	inTx(t, pool, func(tx pgx.Tx) {
		pr, err := record.NewStore().PullRequestByNumber(context.Background(), tx, "acme/widgets", 7)
		if err != nil || pr == nil || pr.HeadSHA != "a2" {
			t.Fatalf("the pull request = %#v, %v; want it recorded at a2", pr, err)
		}
	})
	var kinds []record.NoticeKind
	var blocked []string
	for _, got := range effects(t, pool) {
		if notice, ok := got.payload.(record.ControllerNotice); ok {
			kinds = append(kinds, notice.Kind)
			for _, issue := range notice.Blocked {
				blocked = append(blocked, issue.Issue)
			}
		}
	}
	if !slices.Equal(kinds, []record.NoticeKind{record.TodoNotice, record.TickNotice}) {
		t.Fatalf("controller notices = %v, want the free slot's todo wake, then the tick", kinds)
	}
	var want []string
	for i := range stillWaiting {
		want = append(want, fmt.Sprintf("LEGION-%d", 3000+i), fmt.Sprintf("LEGION-%d", 3100+i))
	}
	slices.Sort(want)
	if !slices.Equal(blocked, want) {
		t.Fatalf("the tick names %v blocked, want %v", blocked, want)
	}
}
