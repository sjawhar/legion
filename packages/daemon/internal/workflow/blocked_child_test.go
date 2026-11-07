package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// seedMirror writes what Dispatch says of key — its status and, in order, the keys it waits on —
// as the blockers.Mirror handler writes one from an event. Its sequence is 1, so an event a test
// then applies through the mirror (sequence 2 or more) wins the row's fence.
func seedMirror(t *testing.T, pool *pgxpool.Pool, key, status string, waits ...string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchIssueRow{
			Key: key, Project: "LEGION", Status: status, Blockers: waits, Seq: 1})
	})
}

// applyMirrored applies fact through the daemon's own handler order (daemon/workflow.go): the
// mirror first, so the engine behind it reads the row this very event writes.
func applyMirrored(t *testing.T, pool *pgxpool.Pool, engine *Engine, eventID string, fact intake.Fact) {
	t.Helper()
	mirror := blockers.NewMirror(record.NewStore(), "LEGION")
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, fact, mirror, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

// startUnblockedChildren is admission's own pass over the waiting line (admit's promoteHolds):
// the engine no longer chooses which issues start — record.Waiting does, from the tree
// lifecycles, the design gates and the slots — so this reads the line as admission reads it and
// gives each issue whose first phase can start what its slot would give it.
func startUnblockedChildren(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	scoped, committed := intake.WithCommitHooks(context.Background())
	tx, err := pool.Begin(scoped)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(scoped)
	store := record.NewStore()
	issues, err := store.Issues(scoped, tx)
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	slots, err := store.Slots(scoped, tx)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	facts, err := record.ReadLineFacts(scoped, tx, store, "LEGION", issues)
	if err != nil {
		t.Fatalf("read the line facts: %v", err)
	}
	mirrored, err := store.DispatchIssues(scoped, tx, "LEGION")
	if err != nil {
		t.Fatalf("list the mirror: %v", err)
	}
	rows := blockers.Rows(mirrored)
	for _, entry := range record.Waiting(issues, slots, facts) {
		if entry.Kind != record.LineFirstPhase || len(blockers.Holds(rows, entry.Issue)) > 0 {
			continue
		}
		if err := engine.StartFirstPhase(scoped, tx, entry.Issue, rows[entry.Issue.Key].Blockers); err != nil {
			t.Fatalf("StartFirstPhase %s: %v", entry.Issue.Key, err)
		}
	}
	if err := tx.Commit(scoped); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed()
}

// seedTreeLifecycle opens key's tree lifecycle under the workflow's authority, as promotion's own
// admission of the root does: that lifecycle, not a slot row, is what says the tree runs.
func seedTreeLifecycle(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			return err
		}
		_, err = record.NewStore().OpenTreeLifecycle(t.Context(), tx, project, key, treelifecycle.AuthorityWorkflow)
		return err
	})
}

// seedHeldTree records a running root whose design gate is gate, admitted at the engine's own
// clock: the tree the held children of these tests belong to.
func seedHeldTree(t *testing.T, pool *pgxpool.Pool, gate record.DesignGate) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, gate)
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
}

// seedAdmittedChild records a child of that tree where enterChild leaves one it does not start.
func seedAdmittedChild(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: key, Parent: new("LEGION-208"), Phase: phase.Admitted,
		Generation: 4, Status: "todo", Rank: "V", LastDispatchSeq: 1})
}

// A child released into a tree whose gate is open, but which waits on an issue Dispatch has not
// finished, is recorded and left at phase.Admitted with nothing queued for it — exactly where a
// child whose tree's gate is still closed is left. A sibling that waits on nothing is released as
// it always was.
func TestAChildReleasedWhileABlockerIsUnfinishedStaysAdmitted(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedMirror(t, pool, "LEGION-300", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "child-blocked", intake.DispatchIssue{
		Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "a blocked child", Parent: "LEGION-208", Rank: "V", Blockers: []string{"LEGION-300"}})

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the blocked child = %s, want it left at admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})

	// Neither child starts at its release now: each is an issue the cap counts, so each waits at
	// admitted until the line reaches it. The pass below is the line's.
	applyMirrored(t, pool, engine, "child-free", intake.DispatchIssue{
		Key: "LEGION-210", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: "a child that waits on nothing", Parent: "LEGION-208", Rank: "W"})
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Admitted {
		t.Fatalf("the unblocked child = %s at its release, want it left at admitted until it has a slot", got)
	}
	assertOutboxKinds(t, pool, []string{})

	startUnblockedChildren(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the blocked child = %s after the line's pass, want it left at admitted", got)
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Planning {
		t.Fatalf("the unblocked child = %s once the line reached it, want planning", got)
	}
	// The second notice is the architect's: every child the line starts tells it so
	// (ChildStartedNotice), whether a blocker held it or its tree's gate did.
	assertOutboxKinds(t, pool, []string{"issue_branch", "dispatch_status", "supervise", "notice", "notice"})
}

// The close of the last issue a held child waited on starts it: its branch row, the in_progress
// its entry into planning writes to Dispatch, its planner's start behind the branch row, and one
// child-status notice to the tree's architect naming the issues it waited on.
func TestAHeldChildStartsWhenItsLastBlockerCloses(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300", "LEGION-301")
	seedMirror(t, pool, "LEGION-300", "done")
	seedMirror(t, pool, "LEGION-301", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)

	startUnblockedChildren(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s while one issue it waits on is unfinished, want admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})

	applyMirrored(t, pool, engine, "blocker-done", intake.DispatchIssue{
		Key: "LEGION-301", ResolvedRepo: "sjawhar/legion", Seq: 4, Type: "issue.updated", Status: "done", Title: "the last blocker"})
	startUnblockedChildren(t, pool, engine)

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
		t.Fatalf("the child = %s once every issue it waited on is done, want planning", got)
	}
	assertOutboxKinds(t, pool, []string{"issue_branch", "dispatch_status", "supervise", "notice", "notice"})
	assertBranchRow(t, pool, "LEGION-209", 4)
	assertPlannerStart(t, pool, "LEGION-209")
	want := "LEGION-209 starts planning under LEGION-208: every issue it waited on (LEGION-300, LEGION-301) is done"
	if got := childStatusNotices(t, pool); len(got) != 1 || got[0] != want {
		t.Fatalf("child-status notices = %#v, want one reading %q", got, want)
	}
}

// A held child whose links a person takes off while it waits is held by nothing any longer, and
// starts at the next look: left admitted, it would stand unstarted with nothing naming it, since
// the state's `blocked` names only a child with an unfinished blocker and the edit, which changes
// no status, is not one the engine's handling of the event acts on. It starts as a child its tree's
// gate opened onto does, and its tree's architect is told so by the same child-status notice a
// blocker's close sends, its reason saying the links were taken off: no issue it waited on closed.
func TestAHeldChildWhoseLinksAreTakenOffStarts(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)

	startUnblockedChildren(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s while the issue it waits on is unfinished, want admitted", got)
	}

	applyMirrored(t, pool, engine, "links-taken-off", intake.DispatchIssue{
		Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "LEGION-209", Parent: "LEGION-208", Rank: "V"})
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s on the edit itself, want it still admitted: the event changes no status", got)
	}
	if got := blockedNow(t, pool); len(got) != 0 {
		t.Fatalf("blocked = %#v once the child names no blocker, want nothing", got)
	}
	startUnblockedChildren(t, pool, engine)

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
		t.Fatalf("the child = %s once it names no blocker, want planning", got)
	}
	assertBranchRow(t, pool, "LEGION-209", 4)
	assertPlannerStart(t, pool, "LEGION-209")
	want := "LEGION-209 starts planning under LEGION-208: it reached the front of the waiting line and took an admission slot"
	if got := childStatusNotices(t, pool); len(got) != 1 || got[0] != want {
		t.Fatalf("child-status notices = %#v, want one reading %q", got, want)
	}
}

// blockedNow is the state's `blocked` as the project stands: every recorded issue and the whole
// mirror, judged as the projection judges them, with no root waiting for a slot.
func blockedNow(t *testing.T, pool *pgxpool.Pool) []record.BlockedIssue {
	t.Helper()
	var blocked []record.BlockedIssue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		issues, err := store.Issues(t.Context(), tx)
		if err != nil {
			return err
		}
		mirrored, err := store.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			return err
		}
		blocked = blockers.Blocked(blockers.Rows(mirrored), issues, nil, nil)
		return nil
	})
	return blocked
}

// The opening of a tree's design gate releases every admitted member it was holding, and the
// waiting line then starts each of them at its own slot — except one that waits on an unfinished
// issue of its own, whose own blockers hold it where the gate left it.
func TestTheGateOpeningDoesNotStartAHeldChild(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedAdmittedChild(t, pool, "LEGION-210")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "backlog")
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyMirrored(t, pool, engine, "approved", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})
	startUnblockedChildren(t, pool, engine)

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the blocked child = %s when the tree's gate opened, want it left at admitted", got)
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Planning {
		t.Fatalf("the child that waits on nothing = %s once the line reached it, want planning", got)
	}
	assertOutboxKinds(t, pool, []string{"notice", "issue_branch", "dispatch_status", "supervise", "notice", "notice"})
}

// An unblocked child whose tree would not start it now is in no line either: the tree's gate is
// what holds it, and the line reads that gate (record.Waiting's LineFirstPhase).
func TestAnUnblockedChildWaitsForItsTreesGate(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "done")

	startUnblockedChildren(t, pool, testEngine(config.DesignGateRootIssues, nil))

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the child = %s while its tree's gate is closed, want admitted", got)
	}
	assertOutboxKinds(t, pool, []string{})
}

// A root's own blockers are judged once, when promotion opens its tree; its tree is running from
// then on, and running work is never re-checked. So its first phase starts whatever its links
// say now: a blocker a person added after the tree was opened, or one a tree Legion took back
// still names (record.Issue.TakenBack, which promotion admitted whatever its links say). The
// line carries the root as an issue of its own (record.Waiting's LineFirstPhase) and
// blockers.LineWaitsOn asks blockers.Holds of it, which answers nothing for a root, so no blocker
// keeps it out.
func TestTheGateOpeningStartsAnAdmittedRootWhateverItsLinksSay(t *testing.T) {
	for _, tc := range []struct {
		name      string
		takenBack bool
	}{
		{name: "a root that took its slot before a blocker was added"},
		{name: "a tree taken back after it finished and lingered", takenBack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 4, Status: "in_progress", Rank: "U",
				HandedOver: true, TakenBack: tc.takenBack, LastDispatchSeq: 1})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
			seedTreeLifecycle(t, pool, "LEGION-208")
			seedMirror(t, pool, "LEGION-208", "in_progress", "LEGION-300")
			seedMirror(t, pool, "LEGION-300", "backlog")

			engine := testEngine(config.DesignGateRootIssues, nil)
			applyMirrored(t, pool, engine, "approved", intake.DispatchArtifact{
				Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})
			startUnblockedChildren(t, pool, engine)

			if got := phaseOf(t, pool, "LEGION-208"); got != phase.Planning {
				t.Fatalf("the admitted root = %s when its tree's gate opened, want planning: its tree already runs", got)
			}
			assertBranchRow(t, pool, "LEGION-208", 4)
			assertPlannerStart(t, pool, "LEGION-208")
		})
	}
}

// A child still waiting on an unfinished issue when its tree finishes is handed to Legion as a
// root of its own, though it never started under its tree's design: its blockers outlive the
// tree, and leaving it admitted inside a closed tree would park it for good. Its sibling, also
// admitted but waiting on nothing, goes with it: the expiry decides by the member's own state —
// in the workflow and never started — and not by what held it there (unfinishedHandOvers),
// because nothing would start either of them where they stand. The sibling was waiting for a
// slot its tree will never offer again, which the cap leaves an ordinary member doing because
// it counts issues; left behind it would stand todo at admitted in a closed tree for good.
func TestALingerHandsOverTheAdmittedChildThatStillWaits(t *testing.T) {
	pool := migratedPool(t)
	until := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "a finished root", Phase: phase.Done, Generation: 4, Status: "done", Rank: "U", LingerUntil: &until})
	seedAdmittedChild(t, pool, "LEGION-209")
	seedAdmittedChild(t, pool, "LEGION-210")
	seedMirror(t, pool, "LEGION-209", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-210", "todo", "LEGION-301")
	seedMirror(t, pool, "LEGION-300", "in_progress")
	seedMirror(t, pool, "LEGION-301", "done")

	if _, err := intake.ApplyFact(context.Background(), pool, "timer", "linger:LEGION-208:4",
		intake.LingerExpired{Issue: "LEGION-208", Generation: 4}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the linger's expiry: %v", err)
	}

	if got := handoverFences(t, pool); len(got) != 2 || got[0] != "LEGION-209" || got[1] != "LEGION-210" {
		t.Fatalf("hand-over fences = %v, want both admitted children's: neither would ever start where it stands", got)
	}
	// Each hand-over's one Dispatch write puts the child todo under the fence of this linger, as
	// the outbox's drop guard and intake.Handover read it.
	for _, child := range []string{"LEGION-209", "LEGION-210"} {
		var writes int
		if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = 'dispatch_status' and issue = $1
			and payload->>'status' = 'todo' and payload->>'observedStatus' = 'todo' and payload->>'handOver' = 'true'
			and payload->>'handOverTree' = 'LEGION-208' and payload->>'handOverLinger' = '4'`, child).Scan(&writes); err != nil || writes != 1 {
			t.Fatalf("%s's hand-over writes = %d, %v; want one fenced on LEGION-208's linger 4", child, writes, err)
		}
	}
	var others int
	if err := pool.QueryRow(t.Context(),
		`select count(*) from outbox where kind = 'dispatch_status' and issue not in ('LEGION-209', 'LEGION-210')`).Scan(&others); err != nil || others != 0 {
		t.Fatalf("status writes for another member = %d, %v; want none: the tree holds nobody else in the workflow", others, err)
	}
}

// An issue's run earns its mark (record.Issue.TakenBack) as it moves into a phase of its own, and
// the mark belongs to that run: a child's re-entry into its next generation in its own tree opens a
// new run, which starts without it — held there by a blocker, it is checked like any child that
// never ran — while the root's leave into a linger opens no run, and its record keeps it. So the record
// a tree that ran leaves for its taken-back root is already one promotion exempts
// (blockers.Exempt), whatever re-admission later writes of its phase and generation, which keeps
// the mark of a root whose own tree comes back — while a member that never started carries none.
func TestTheRunMarkIsWrittenAsAPhaseStartsAndBelongsToThatRun(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 4, Status: "in_progress", Rank: "U",
		HandedOver: true, LastDispatchSeq: 1})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 2})
	seedTreeLifecycle(t, pool, "LEGION-208")
	seedAdmittedChild(t, pool, "LEGION-209")
	seedAdmittedChild(t, pool, "LEGION-210")
	seedMirror(t, pool, "LEGION-210", "todo", "LEGION-300")
	seedMirror(t, pool, "LEGION-300", "in_progress")
	engine := testEngine(config.DesignGateRootIssues, nil)
	marked := func(key string) bool {
		t.Helper()
		var got bool
		if err := pool.QueryRow(t.Context(), "select taken_back from issues where key = $1", key).Scan(&got); err != nil {
			t.Fatalf("read the mark of %s: %v", key, err)
		}
		return got
	}
	for _, key := range []string{"LEGION-208", "LEGION-209", "LEGION-210"} {
		if marked(key) {
			t.Fatalf("%s is marked before any phase of it started", key)
		}
	}

	applyMirrored(t, pool, engine, "approved", intake.DispatchArtifact{
		Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2})
	startUnblockedChildren(t, pool, engine)
	for key, want := range map[string]struct {
		at     phase.Phase
		marked bool
	}{
		"LEGION-208": {phase.Planning, true}, "LEGION-209": {phase.Planning, true}, "LEGION-210": {phase.Admitted, false},
	} {
		if got := phaseOf(t, pool, key); got != want.at || marked(key) != want.marked {
			t.Fatalf("%s = %s marked %t once the gate opened, want %s marked %t", key, got, marked(key), want.at, want.marked)
		}
	}

	// The child that ran, in_progress as its run's status writes leave it, is set back to todo
	// while an issue it now waits on is unfinished: its next generation is a new run, held at
	// admitted and carrying no mark, since that run has stood in no phase.
	if _, err := pool.Exec(t.Context(), "update issues set status = 'in_progress', dispatch_status = 'in_progress' where key = 'LEGION-209'"); err != nil {
		t.Fatalf("record the running child in_progress: %v", err)
	}
	seedMirror(t, pool, "LEGION-301", "in_progress")
	applyMirrored(t, pool, engine, "child-set-back", intake.DispatchIssue{
		Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "LEGION-209", Parent: "LEGION-208", Rank: "V", Blockers: []string{"LEGION-301"}})
	var generation int
	if err := pool.QueryRow(t.Context(), "select generation from issues where key = 'LEGION-209'").Scan(&generation); err != nil {
		t.Fatalf("read the re-entered child's generation: %v", err)
	}
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted || generation != 5 || marked("LEGION-209") {
		t.Fatalf("the re-entered child = %s at generation %d marked %t, want held at admitted in generation 5 and unmarked: its new run has not run",
			got, generation, marked("LEGION-209"))
	}

	// The root leaves the workflow: its tree lingers, its run over, and its record keeps the mark.
	applyMirrored(t, pool, engine, "root-backlog", intake.DispatchIssue{
		Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated", Status: "backlog",
		Title: "root", Rank: "U", HandedOver: true})
	var root *record.Issue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		root, err = record.NewStore().Issue(t.Context(), tx, "LEGION-208")
		return err
	})
	if root.Phase != phase.Done || !root.Lingers() || !root.TakenBack {
		t.Fatalf("the root that left = %#v, want it done, lingering and still marked", *root)
	}
	// What re-admission writes of a root taken back — its next generation at admitted, no linger —
	// leaves the mark as it found it, and the record is exempt by that alone: it carries no run.
	taken := *root
	taken.Phase, taken.Generation, taken.LingerUntil = phase.Admitted, root.Generation+1, nil
	if taken.CarriesRun() || !blockers.Exempt(taken) {
		t.Fatalf("the root taken back = %#v, want it exempt by its mark alone", taken)
	}
	if waits := blockers.WaitsOn(map[string]blockers.Row{"LEGION-208": {Status: "todo", Blockers: []string{"LEGION-300"}}}, taken); len(waits) != 0 {
		t.Fatalf("the root taken back waits on %v, want nothing: it ran", waits)
	}
}

// handoverFences is every issue the linger fenced for hand-over, in key order.
func handoverFences(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select issue from handovers order by issue")
	if err != nil {
		t.Fatalf("read the hand-over fences: %v", err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			t.Fatalf("scan a hand-over fence: %v", err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the hand-over fences: %v", err)
	}
	return issues
}

func phaseOf(t *testing.T, pool *pgxpool.Pool, key string) phase.Phase {
	t.Helper()
	var got string
	if err := pool.QueryRow(t.Context(), "select phase from issues where key = $1", key).Scan(&got); err != nil {
		t.Fatalf("read the phase of %s: %v", key, err)
	}
	return phase.Phase(got)
}

func assertBranchRow(t *testing.T, pool *pgxpool.Pool, issue string, generation int) {
	t.Helper()
	var gotIssue string
	var gotGeneration int
	if err := pool.QueryRow(t.Context(), "select issue, (payload->>'generation')::int from outbox where kind = 'issue_branch'").Scan(&gotIssue, &gotGeneration); err != nil {
		t.Fatalf("read the branch row: %v", err)
	}
	if gotIssue != issue || gotGeneration != generation {
		t.Fatalf("branch row = %s at generation %d, want %s at %d", gotIssue, gotGeneration, issue, generation)
	}
}

func assertPlannerStart(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	var gotIssue, op, role string
	if err := pool.QueryRow(t.Context(), "select issue, payload->>'op', payload->>'role' from outbox where kind = 'supervise' and coalesce(payload->>'task', '') <> ''").Scan(&gotIssue, &op, &role); err != nil {
		t.Fatalf("read the supervise row: %v", err)
	}
	if gotIssue != issue || op != "start" || role != "planner" {
		t.Fatalf("supervise row = %s %s %s, want a planner start for %s", gotIssue, op, role, issue)
	}
}

func childStatusNotices(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'child-status' order by id")
	if err != nil {
		t.Fatalf("read the child-status notices: %v", err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("scan a child-status notice: %v", err)
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the child-status notices: %v", err)
	}
	return reasons
}
