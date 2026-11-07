package workflow

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A member held at admitted is handed over whatever becomes, during the linger, of the ancestor
// that held it. The tree is the spec's own: a root R waiting for its children, a child C whose
// own reviewed head the daemon has accepted, and a grandchild G a person moved to todo under C,
// which the daemon recorded at admitted and told C's architect starts once C has closed
// (record.HeldUnderAccepted). A person closes R by hand, and the tree lingers.
//
// An ancestor below the root does not keep its phase through that linger: C's pull request still
// merges, a person still moves C to done or to backlog, and record.OpenBeneath leaves G out of
// the wait any of them read, so nothing stops them. Decided from the hold, the expiry then read
// G as held by nobody and left it todo at admitted in a closed tree, with its architect told it
// would start. Decided from G's own state — in the workflow and never started — the hand-over
// is the same in all four, which is what the architect's notice promised.
func TestTheHeldGrandchildIsHandedOverWhateverBecomesOfItsAcceptedParent(t *testing.T) {
	root, child, held := "LEGION-208", "LEGION-209", "LEGION-210"
	for _, during := range []struct {
		name  string
		close func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result)
	}{
		{
			name:  "the-parent-stands",
			close: func(*testing.T, *pgxpool.Pool, func(string, intake.Fact) intake.Result) {},
		},
		{
			name: "the-parents-pull-request-merges",
			close: func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result) {
				t.Helper()
				apply("child-merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 44, MergeSHA: "merge"})
				if at := issueAt(t, pool, child); at != phase.Done {
					t.Fatalf("%s stands at %s after its merge inside the linger, want done: a member of a lingering tree still closes at its merge", child, at)
				}
				// That close opens the child's production check, and a tree lingers past its
				// deadline while one is open: the check's own close is another rule's, so the
				// fixture takes it away rather than driving a look to its end.
				if _, err := pool.Exec(context.Background(), "delete from production_checks where issue = $1", child); err != nil {
					t.Fatalf("close the production check of %s: %v", child, err)
				}
			},
		},
		{
			name: "a-person-moves-the-parent-to-done",
			close: func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result) {
				t.Helper()
				apply("child-done", intake.DispatchIssue{Key: child, ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.closed",
					Status: "done", Title: "child", Rank: "V1"})
				recordTheStatusAMoveShows(t, pool, child, "done")
			},
		},
		{
			name: "a-person-moves-the-parent-to-backlog",
			close: func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result) {
				t.Helper()
				apply("child-parked", intake.DispatchIssue{Key: child, ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated",
					Status: "backlog", Title: "child", Rank: "V1"})
				recordTheStatusAMoveShows(t, pool, child, "backlog")
			},
		},
	} {
		t.Run(during.name, func(t *testing.T) {
			pool := migratedPool(t)
			now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
			engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
				Clock: func() time.Time { return now }}, nil)
			rootKey, childKey := root, child
			// The root's own first phase waits for the child beneath it, so it stands at admitted.
			seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: root, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 7,
				Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
			seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: root, Parent: &rootKey, Project: "LEGION", Title: "child", Phase: phase.AwaitingMerge, Generation: 7,
				Status: "retro", DispatchStatus: "retro", Rank: "V1", LastDispatchSeq: 1})
			seedIssue(t, pool, record.Issue{Key: held, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: root, Parent: &childKey, Project: "LEGION", Title: "held", Phase: phase.Admitted, Generation: 7,
				Status: "todo", DispatchStatus: "todo", Rank: "V2", LastDispatchSeq: 1})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: child, Repo: "sjawhar/legion", Number: 44,
				Branch: "legion/LEGION-209", HeadSHA: "head", Failing: []string{}})

			apply := applyFacts(t, pool, engine)
			apply("root-closed-by-hand", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed",
				Status: "done", Title: "root", Rank: "U"})
			var lingering bool
			if err := pool.QueryRow(t.Context(), "select linger_until is not null from issues where key = $1", root).Scan(&lingering); err != nil || !lingering {
				t.Fatalf("the hand-closed root lingers = %v, %v; want its tree lingering", lingering, err)
			}
			// Admission, which runs after the workflow handler, is what records the status a
			// person's move shows (its recordObservation); this test runs the engine alone.
			recordTheStatusAMoveShows(t, pool, root, "done")

			during.close(t, pool, apply)

			apply("linger", intake.LingerExpired{Issue: root, Generation: 7})

			assertHandedOver(t, pool, held, root, 7)
		})
	}
}

// A parent of the closing tree that is still waiting for its own Dispatch children when the
// linger expires is handed over as every other member that never started is. Nothing held it at
// admitted but the children-first wait (record.ChildWaits), which its new tree keeps: its
// children are open in Dispatch still, so its first phase waits for them there as it waited
// here. Left behind instead, it would stand todo at admitted in a closed tree, which is the one
// place no slot, no status and no person's move reaches.
func TestALingerHandsOverTheParentStillWaitingForItsChildren(t *testing.T) {
	pool := migratedPool(t)
	until := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	root, parent, openChild := "LEGION-208", "LEGION-209", "LEGION-300"
	rootKey := root
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "a finished root", Phase: phase.Done, Generation: 4, Status: "done",
		DispatchStatus: "done", Rank: "U", LingerUntil: &until})
	seedIssue(t, pool, record.Issue{Key: parent, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &rootKey, Project: "LEGION", Title: "a parent of its own", Phase: phase.Admitted, Generation: 4,
		Status: "todo", DispatchStatus: "todo", Rank: "V", LastDispatchSeq: 1})
	// What Dispatch shows: the member waits on nothing, and the child filed under it is open, so
	// its own first phase does its children's work first and never started.
	seedMirror(t, pool, parent, "todo")
	seedMirrorChild(t, pool, openChild, "todo", parent)

	if _, err := intake.ApplyFact(context.Background(), pool, "timer", "linger:LEGION-208:4",
		intake.LingerExpired{Issue: root, Generation: 4}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the linger's expiry: %v", err)
	}

	assertHandedOver(t, pool, parent, root, 4)
}

// What a closed tree keeps is what a person took out of the workflow, and nothing else. This
// root is closed by hand at admitted, before any phase of it ran, so no phase of it was ever
// accepted. The member's own state decides the hand-over, not the root's acceptance, so the
// member standing todo at admitted is handed over — its tree has closed and nothing would start
// it where it stands — while the sibling a person parked and the child its architect never
// released stay with the tree, their Dispatch status saying they wait on nobody
// (record.OutOfWorkflow).
func TestAClosedTreeKeepsOnlyTheMembersThatLeftTheWorkflow(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, member, parked, unreleased := "LEGION-208", "LEGION-210", "LEGION-211", "LEGION-212"
	rootKey := root
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 7,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
	for _, member := range []struct{ key, status string }{
		{key: member, status: "todo"},
		{key: parked, status: "backlog"},
		{key: unreleased, status: "triage"},
	} {
		seedIssue(t, pool, record.Issue{Key: member.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: root, Parent: &rootKey, Project: "LEGION", Title: member.key, Phase: phase.Admitted, Generation: 7,
			Status: member.status, DispatchStatus: member.status, Rank: "V", LastDispatchSeq: 1})
	}

	apply := applyFacts(t, pool, engine)
	apply("closed-by-hand", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed",
		Status: "done", Title: "root", Rank: "U"})
	recordTheStatusAMoveShows(t, pool, root, "done")

	apply("linger", intake.LingerExpired{Issue: root, Generation: 7})

	assertHandedOver(t, pool, member, root, 7)
	if got := handoverFences(t, pool); len(got) != 1 || got[0] != member {
		t.Fatalf("hand-over fences = %v, want %s's alone: a member out of the workflow stays with the tree that closed", got, member)
	}
}

// seedMirrorChild writes what Dispatch says of a child filed under parent: its status and the
// parent that makes it one of parent's open children (record.ChildWaits reads the pair).
func seedMirrorChild(t *testing.T, pool *pgxpool.Pool, key, status, parent string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchIssueRow{
			Key: key, Project: "LEGION", Status: status, Seq: 1, Parent: parent})
	})
}

// recordTheStatusAMoveShows records on the issue the status a person's move shows, which
// admission writes when it runs behind the workflow handler (its recordObservation): a test that
// drives the engine alone writes it itself.
func recordTheStatusAMoveShows(t *testing.T, pool *pgxpool.Pool, key, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "update issues set status = $2, dispatch_status = $2 where key = $1", key, status); err != nil {
		t.Fatalf("record the status %s's move shows: %v", key, err)
	}
}

// The tree both of the rule's fixtures are built from: a root at generation 7 with the three
// members under it, and one person's decision standing on each of the two members a close can
// hand over. The references are the asks those decisions were made on.
const (
	handsOverRoot             = "LEGION-208"
	handsOverMergeReady       = "LEGION-209"
	handsOverParkedAtTheMerge = "LEGION-210"
	handsOverWorking          = "LEGION-211"
	handsOverGeneration       = 7
	handsOverMergeReadyRef    = "dispatch://LEGION-209/ask/ask-1"
	handsOverWorkingRef       = "dispatch://LEGION-211/ask/ask-2"
)

// Which members the close of a lingering tree hands to Legion as roots of their own, under each
// of the two rules the root's own status picks between (unfinishedHandOvers).
//
// A root that stayed done hands over every member it left in the workflow, whatever phase that
// member stands in: its tree is finished for good, so nothing of it would ever run any of them
// again.
//
// A root a person or a controller parked hands over one member only: the one standing at
// phase.AwaitingMerge whose pull request is still open. Such a root can be re-admitted, and the
// re-admission resumes a member of any other phase where it stood (admit's startMidPhaseMembers),
// so waiting strands nothing else. The one thing no re-admission does is arm again the merge the
// park disarmed: the linger's start disarmed every member's open pull request (beginLinger), and
// only a new READY from a merger arms one. So the member this rule rescues is exactly the one
// holding an open, approved pull request with no live tree left to write that READY. Handed over,
// it keeps its run and its pull request, and the merge still closes it; left behind, it lost its
// claims and its workspace with the rest of the tree and stood at awaiting_merge for good, which
// is where INFRA-1333 stood until a person merged it by hand.
func TestLingerHandsOver(t *testing.T) {
	t.Run("a parked root hands over the member whose merge nothing can arm again", func(t *testing.T) {
		pool, logged, apply := handsOverFixture(t)
		apply("a-person-parks-the-root", intake.DispatchIssue{Key: handsOverRoot, ResolvedRepo: "sjawhar/legion", Seq: 2,
			Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"})
		recordTheStatusAMoveShows(t, pool, handsOverRoot, "backlog")
		assertLingersToItsClose(t, pool, handsOverRoot)

		apply("linger", intake.LingerExpired{Issue: handsOverRoot, Generation: handsOverGeneration})

		assertHandedOver(t, pool, handsOverMergeReady, handsOverRoot, handsOverGeneration)
		if got := handoverFences(t, pool); !slices.Equal(got, []string{handsOverMergeReady}) {
			t.Fatalf("hand-over fences = %v, want %s's alone: a root a person parked hands over the member awaiting its merge on an open pull request and nobody else",
				got, handsOverMergeReady)
		}
		// It goes as a root of its own carrying its run: the hand-over writes nothing over the
		// phase, so its new tree resumes where it stood (record.Issue.CarriesRun).
		carried := issueRecord(t, pool, handsOverMergeReady)
		if carried == nil || carried.Phase != phase.AwaitingMerge || !carried.CarriesRun() {
			t.Fatalf("the handed-over member = %+v, want it still at awaiting_merge carrying its run: its new tree resumes the phase it stood in", carried)
		}
		// The decisions parked on it go with it, and the close says so rather than warning that it
		// left them behind; the one on a member it keeps is the warning.
		want := []string{
			`level=INFO msg="workflow: the tree closed owing a person's decision; it goes to the architect of the child this close handed to Legion" ` +
				`ref=` + handsOverMergeReadyRef + ` root=` + handsOverRoot + ` issue=` + handsOverMergeReady + ` where="kept for its issue's tree"`,
			`level=WARN msg="workflow: the tree closed while it still owed a person's decision to an architect; the controller already has its parked-decision notice" ` +
				`ref=` + handsOverWorkingRef + ` root=` + handsOverRoot + ` issue=` + handsOverWorking + ` where="kept for its issue's tree"`,
		}
		if got := closeDecisionLines(t, logged); !slices.Equal(got, want) {
			t.Fatalf("the close wrote\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		// The two the close keeps lose their claims with the rest of the tree.
		retired := superviseRequests(t, pool, "tree_close")
		for _, kept := range []string{handsOverParkedAtTheMerge, handsOverWorking} {
			for _, role := range claim.Roles {
				if got := retired[kept+"/"+string(role)]; got != 1 {
					t.Fatalf("the close retired %s's %s claim %d times, want once: a member it keeps goes with the tree", kept, role, got)
				}
			}
		}
		// What the hand-over was for: the pull request it carried still merges, and that merge
		// closes the issue (closeMerged, in a tree that lingers).
		apply("the-handed-members-merge", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 44, MergeSHA: "merge-44"})
		if at := issueAt(t, pool, handsOverMergeReady); at != phase.Done {
			t.Fatalf("%s stands at %s after the merge of the pull request it carried, want done", handsOverMergeReady, at)
		}
	})

	// Today's rule, unchanged: under a root that stayed done the member's own status is the whole
	// of it, and the phase is not read at all. The member a person parked is the one the tree
	// keeps (record.OutOfWorkflow).
	t.Run("a done root hands over every member whose status is in the workflow", func(t *testing.T) {
		pool, logged, apply := handsOverFixture(t)
		apply("a-person-closes-the-root", intake.DispatchIssue{Key: handsOverRoot, ResolvedRepo: "sjawhar/legion", Seq: 2,
			Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
		recordTheStatusAMoveShows(t, pool, handsOverRoot, "done")
		assertLingersToItsClose(t, pool, handsOverRoot)

		apply("linger", intake.LingerExpired{Issue: handsOverRoot, Generation: handsOverGeneration})

		for _, handed := range []string{handsOverMergeReady, handsOverWorking} {
			assertHandedOver(t, pool, handed, handsOverRoot, handsOverGeneration)
		}
		if got := handoverFences(t, pool); !slices.Equal(got, []string{handsOverMergeReady, handsOverWorking}) {
			t.Fatalf("hand-over fences = %v, want both members in the workflow, whatever phase each stands in", got)
		}
		want := []string{
			`level=INFO msg="workflow: the tree closed owing a person's decision; it goes to the architect of the child this close handed to Legion" ` +
				`ref=` + handsOverMergeReadyRef + ` root=` + handsOverRoot + ` issue=` + handsOverMergeReady + ` where="kept for its issue's tree"`,
			`level=INFO msg="workflow: the tree closed owing a person's decision; it goes to the architect of the child this close handed to Legion" ` +
				`ref=` + handsOverWorkingRef + ` root=` + handsOverRoot + ` issue=` + handsOverWorking + ` where="kept for its issue's tree"`,
		}
		if got := closeDecisionLines(t, logged); !slices.Equal(got, want) {
			t.Fatalf("the close wrote\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})

	// The pull_requests row is the fact the narrow rule turns on, and the phase alone is not it.
	// Under a parked root, a member standing at awaiting_merge with no pull request recorded, one
	// whose pull request merged and one whose pull request was closed unmerged are all left with
	// the tree: none of them holds a merge a READY would ever arm, which is the only thing the
	// re-admission of this root could not do for them.
	t.Run("a parked root reads the pull request and not the phase alone", func(t *testing.T) {
		pool := migratedPool(t)
		now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
		engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
			Clock: func() time.Time { return now }}, nil)
		rootKey := handsOverRoot
		seedIssue(t, pool, record.Issue{Key: handsOverRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: handsOverRoot, Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: handsOverGeneration,
			Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
		for _, member := range []struct {
			key   string
			rank  string
			state record.PullRequestState
		}{
			{key: "LEGION-212", rank: "V", state: ""},
			{key: "LEGION-213", rank: "W", state: record.PullRequestMerged},
			{key: "LEGION-214", rank: "X", state: record.PullRequestClosed},
		} {
			seedIssue(t, pool, record.Issue{Key: member.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: handsOverRoot, Parent: &rootKey, Project: "LEGION", Title: member.key, Phase: phase.AwaitingMerge,
				Generation: handsOverGeneration, Status: "retro", DispatchStatus: "retro", Rank: member.rank, LastDispatchSeq: 1})
			if member.state == "" {
				continue
			}
			seedPR(t, pool, record.PullRequest{State: member.state, Issue: member.key, Repo: "sjawhar/legion",
				Number: 50 + len(member.rank), Branch: "legion/" + member.key, HeadSHA: "head-" + member.key, Failing: []string{}})
		}

		apply := applyFacts(t, pool, engine)
		apply("a-person-parks-the-root", intake.DispatchIssue{Key: handsOverRoot, ResolvedRepo: "sjawhar/legion", Seq: 2,
			Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"})
		recordTheStatusAMoveShows(t, pool, handsOverRoot, "backlog")

		apply("linger", intake.LingerExpired{Issue: handsOverRoot, Generation: handsOverGeneration})

		if got := handoverFences(t, pool); len(got) != 0 {
			t.Fatalf("hand-over fences = %v, want none: a parked root hands over a member at awaiting_merge only while its pull request is open", got)
		}
	})
}

// handsOverFixture records the tree check k5's two fixtures share and returns the pool, the
// engine's log and the way to apply a fact to it. The park that makes the tree linger is the
// person's own move, applied by the test, so the linger is the real one — every member's claim
// suspended and every open pull request disarmed (beginLinger) — rather than a deadline a test
// wrote onto the root.
func handsOverFixture(t *testing.T) (*pgxpool.Pool, *bytes.Buffer, func(id string, fact intake.Fact) intake.Result) {
	t.Helper()
	pool := migratedPool(t)
	now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	logged := &bytes.Buffer{}
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, slog.New(slog.NewTextHandler(logged, nil)))
	rootKey := handsOverRoot
	seedIssue(t, pool, record.Issue{Key: handsOverRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: handsOverRoot, Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: handsOverGeneration,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
	// The member one merge from landing, the member a person took out of the workflow where it
	// stood at the same phase, and the member still working.
	for _, member := range []struct {
		key    string
		at     phase.Phase
		status string
		rank   string
		number int
	}{
		{key: handsOverMergeReady, at: phase.AwaitingMerge, status: "retro", rank: "V", number: 44},
		{key: handsOverParkedAtTheMerge, at: phase.AwaitingMerge, status: "backlog", rank: "W", number: 45},
		{key: handsOverWorking, at: phase.Implementing, status: "in_progress", rank: "X"},
	} {
		seedIssue(t, pool, record.Issue{Key: member.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: handsOverRoot, Parent: &rootKey, Project: "LEGION", Title: member.key, Phase: member.at,
			Generation: handsOverGeneration, Status: member.status, DispatchStatus: member.status, Rank: member.rank,
			LastDispatchSeq: 1})
		if member.number == 0 {
			continue
		}
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: member.key, Repo: "sjawhar/legion",
			Number: member.number, Branch: "legion/" + member.key, HeadSHA: "head-" + member.key, Failing: []string{}})
	}
	// One decision on each member a close can hand over, the newer on the merge-ready one, so the
	// close's lines come in a fixed order (record.Store.ParkedDecisions is newest first).
	seedHandsOverDecision(t, pool, handsOverMergeReady, handsOverMergeReadyRef, now)
	seedHandsOverDecision(t, pool, handsOverWorking, handsOverWorkingRef, now.Add(-time.Minute))
	return pool, logged, applyFacts(t, pool, engine)
}

// seedHandsOverDecision keeps one person's decision made on an ask of issue, as
// record.RelayParkedDecision keeps one nobody in the issue's tree could hear.
func seedHandsOverDecision(t *testing.T, pool *pgxpool.Pool, issue, ref string, at time.Time) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().AddParkedDecision(context.Background(), tx, record.ParkedDecision{Issue: issue, Ref: ref,
			Decision: record.DecisionAskAnswered, Actor: "user:alice", At: at})
	})
}

// assertLingersToItsClose is the person's move having taken the tree into its linger, which is
// what the expiry below then ends.
func assertLingersToItsClose(t *testing.T, pool *pgxpool.Pool, root string) {
	t.Helper()
	var lingering bool
	if err := pool.QueryRow(context.Background(), "select linger_until is not null from issues where key = $1", root).Scan(&lingering); err != nil || !lingering {
		t.Fatalf("%s lingers = %v, %v; want its tree lingering to its close", root, lingering, err)
	}
}

// closeDecisionLines is every line the close wrote about a decision the closing tree owed, from
// `level=` on and in the order it wrote them: an INFO for one that goes with a member the close
// handed to Legion, and a WARN for one it leaves standing (keptDecisionOfAClosedTree).
func closeDecisionLines(t *testing.T, logged *bytes.Buffer) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if !strings.Contains(line, "a person's decision") {
			continue
		}
		lines = append(lines, line[strings.Index(line, "level="):])
	}
	return lines
}
