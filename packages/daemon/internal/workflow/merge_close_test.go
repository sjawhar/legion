package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The merge of a child's pull request while it awaits the merge closes the child and opens its
// production check in the implementer's live session: the child is done on the record and on
// Dispatch, its architect is told child-closed, five of its six claims are closed, and the
// implementer's is handed the check's first look — a start with no phase, fenced on the check's
// look 1, whose task opens with the line a resumed session tells the check by — with no
// IssueSuspend at the close, since the check's close writes the release. The check records the
// repository, the pull request, the merge commit and its one open look; its look is bounded two
// hours out and the whole check at the project's bound.
func TestAChildsMergeClosesItAndHandsItsImplementerTheFirstLook(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, Clock: func() time.Time { return now }}, nil)

	if result, err := intake.ApplyFact(context.Background(), pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"}, engine, admissionStub{}); err != nil || result.Refusal != nil {
		t.Fatalf("ApplyFact merge = %+v, %v", result.Refusal, err)
	}

	var childPhase phase.Phase
	var childStatus string
	if err := pool.QueryRow(context.Background(), "select phase, status from issues where key = 'LEGION-209'").Scan(&childPhase, &childStatus); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if childPhase != phase.Done || childStatus != "done" {
		t.Fatalf("the merged child = %s/%s, want done/done", childPhase, childStatus)
	}
	var statusWrites int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'dispatch_status' and issue = 'LEGION-209'
		and payload->>'status' = 'done' and payload->>'observedStatus' = 'retro'`).Scan(&statusWrites); err != nil || statusWrites != 1 {
		t.Fatalf("done status writes = %d, %v; want one over retro", statusWrites, err)
	}
	if got := noticeKinds(t, pool, "LEGION-209"); !containsNotice(got, "child-closed") {
		t.Fatalf("child notices = %v, want child-closed", got)
	}
	closed := superviseRequests(t, pool, "issue_close")
	for _, role := range claim.Roles {
		want := 1
		if role == claim.RoleImplementer {
			want = 0
		}
		if closed["LEGION-209/"+string(role)] != want {
			t.Fatalf("issue_close rows = %v, want one for every role but the implementer", closed)
		}
	}
	starts := superviseRowsOf(t, pool, "LEGION-209", "start")
	if len(starts) != 1 {
		t.Fatalf("start rows = %+v, want the implementer's check look alone", starts)
	}
	start := starts[0]
	if start.Role != claim.RoleImplementer || start.CheckLook != 1 || start.Phase != "" || start.Generation != 7 || start.Tree != "LEGION-208" {
		t.Fatalf("the check's start row = %+v, want the implementer's, check look 1, no phase, generation 7", start)
	}
	if lead, _, _ := strings.Cut(start.Task, "\n"); lead != "Production check of LEGION-209: child." {
		t.Fatalf("the check task opens %q, want the line a resumed session tells the check by", lead)
	}
	for _, want := range []string{"sjawhar/legion", "#43", "merge-43", "Look 1, woken by the merge.", "handoff_complete", "pass", "pending", "fail", "the daemon wakes the check",
		// A defect with a clear fix goes to the controller topic, which files it for Legion; one that
		// needs a decision is a finding ask on the report issue, whose reference the implementer
		// publishes to the controller, which files only on a person's answer.
		"a defect with a clear fix goes to the controller topic", "dispatch ask", "skill://legion-worker/references/findings.md", "report issue", "controller topic"} {
		if !strings.Contains(start.Task, want) {
			t.Fatalf("the check task lacks %q:\n%s", want, start.Task)
		}
	}
	// The implementer never files an issue for a defect the check finds: the controller does.
	for _, never := range []string{"dispatch issue", "issue of its own"} {
		if strings.Contains(start.Task, never) {
			t.Fatalf("the check task still says %q:\n%s", never, start.Task)
		}
	}
	if strings.Contains(start.Task, "last look") {
		t.Fatalf("the first look's task says it is the last:\n%s", start.Task)
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("issue suspensions at the close = %+v, want none until the check closes", got)
	}
	check := productionCheck(t, pool, "LEGION-209")
	if check == nil || !check.Open() || check.Repo != "sjawhar/legion" || check.Number != 43 || check.MergeSHA != "merge-43" || check.Tree != "LEGION-208" || check.Project != "LEGION" ||
		check.Looks != 1 || !check.LookOpen || !check.OpenedAt.Equal(now) || !check.BoundAt.Equal(now.Add(48*time.Hour)) || check.LastWakeKind != "merge" || check.Last {
		t.Fatalf("the production check = %+v, want it open on sjawhar/legion#43 at merge-43, look 1 open, woken by the merge, bound 48h out", check)
	}
	if look := issueOutboxOf(t, pool, "LEGION-209", "check_look"); len(look) != 1 || !look[0].nextAt.Equal(now.Add(CheckLookBound)) || decodeLook(t, look[0]) != 1 {
		t.Fatalf("check_look rows = %+v, want one for look 1 due two hours out", look)
	}
	if bound := issueOutboxOf(t, pool, "LEGION-209", "check_bound"); len(bound) != 1 || !bound[0].nextAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("check_bound rows = %+v, want one due at the bound", bound)
	}
	if got := issueSuspensions(t, pool, "LEGION-208"); len(got) != 0 || superviseRequests(t, pool, "suspend")["LEGION-208/implementer"] != 0 {
		t.Fatalf("the child's close touched its root: suspensions %+v, suspends %v", got, superviseRequests(t, pool, "suspend"))
	}
}

// A root's merge waits for the rest of its tree, so the close that lingers the tree is the one its
// last child's departure brings: every member's claims are suspended and each member's Sandbox
// kept. The root's production check opens behind the linger's rows — its start row is newer than
// every suspend and every IssueSuspend the linger wrote — so the root's own suspension finishes
// without acting on the check's start, and the start resumes the implementer in the still-live pod.
func TestARootsCloseLingersItsTreeAndOpensItsCheckBehindTheLingersRows(t *testing.T) {
	rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
	pool := rig.pool
	assertPhase(t, pool, phase.AwaitingMerge)

	rig.apply("child-done", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.closed", Status: "done", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V1"})
	assertPhase(t, pool, phase.Done)
	everyClaim := map[string]int{}
	for _, issue := range []string{"LEGION-208", "LEGION-209"} {
		for _, role := range claim.Roles {
			everyClaim[issue+"/"+string(role)]++
		}
	}
	if suspended := superviseRequests(t, pool, "suspend"); !sameCounts(suspended, everyClaim) {
		t.Fatalf("the linger suspended %v, want each tree claim once: %v", suspended, everyClaim)
	}
	if got := superviseRequests(t, pool, "issue_close"); got["LEGION-208/implementer"] != 0 {
		t.Fatalf("issue_close rows at a root's close = %v, want none of the root's: the tree's close retires its claims", got)
	}
	var start outboxRow
	for _, row := range issueOutboxOf(t, pool, "LEGION-208", "supervise") {
		if request := decodeSupervise(t, row); request.Op == "start" && request.CheckLook == 1 {
			start = row
		}
	}
	if start.id == 0 {
		t.Fatalf("the root's supervise rows hold no check look 1 start")
	}
	for _, kind := range []string{"issue_suspend", "linger_close"} {
		for _, issue := range []string{"LEGION-208", "LEGION-209"} {
			for _, row := range issueOutboxOf(t, pool, issue, kind) {
				if row.id > start.id {
					t.Fatalf("%s row %d of %s is newer than the check's start row %d; the start must follow the linger's rows", kind, row.id, issue, start.id)
				}
			}
		}
	}
	for _, row := range issueOutboxOf(t, pool, "LEGION-208", "supervise") {
		if row.id > start.id {
			t.Fatalf("supervise row %d of the root is newer than the check's start row %d: %s", row.id, start.id, row.payload)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-208"); len(got) != 1 || got[0].Release {
		t.Fatalf("the root's issue suspensions = %+v, want the linger's one keeping row", got)
	}
	if check := productionCheck(t, pool, "LEGION-208"); check == nil || !check.Open() || check.Looks != 1 || !check.LookOpen || check.MergeSHA != "merge" {
		t.Fatalf("the root's production check = %+v, want it open at look 1 on merge", check)
	}
}

// A merge that lands while the tree lingers still closes the member: its phase and status are done,
// its architect is told child-closed, and its production check opens with look 1 as a wake start
// row, since the linger already suspended its implementer. It retires nobody and suspends nothing:
// no issue_close and no IssueSuspend, which the linger's own rows already wrote and the tree's
// close completes.
func TestALingeringMembersMergeClosesItAndWakesItsCheck(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	seedMergingChild(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 7, Status: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, Clock: func() time.Time { return now }}, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact merge: %v", err)
	}
	var childPhase phase.Phase
	var childStatus string
	if err := pool.QueryRow(context.Background(), "select phase, status from issues where key = 'LEGION-209'").Scan(&childPhase, &childStatus); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if childPhase != phase.Done || childStatus != "done" {
		t.Fatalf("the lingering member = %s/%s, want done/done", childPhase, childStatus)
	}
	var statusWrites int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'dispatch_status' and issue = 'LEGION-209' and payload->>'status' = 'done'`).Scan(&statusWrites); err != nil || statusWrites != 1 {
		t.Fatalf("done status writes = %d, %v; want one", statusWrites, err)
	}
	if got := noticeKinds(t, pool, "LEGION-209"); !containsNotice(got, "child-closed") {
		t.Fatalf("child notices = %v, want child-closed", got)
	}
	starts := superviseRowsOf(t, pool, "LEGION-209", "start")
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].CheckLook != 1 || starts[0].Phase != "" {
		t.Fatalf("start rows = %+v, want the check's look 1 as the implementer's wake", starts)
	}
	for _, op := range []string{"issue_close", "suspend"} {
		if got := superviseRequests(t, pool, op); len(got) != 0 {
			t.Fatalf("%s rows = %v, want none: the linger's rows already stopped the member", op, got)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("issue suspensions = %+v, want none", got)
	}
	if check := productionCheck(t, pool, "LEGION-209"); check == nil || !check.Open() || check.Looks != 1 || !check.LookOpen || check.MergeSHA != "merge-43" {
		t.Fatalf("the production check = %+v, want it open at look 1 on merge-43", check)
	}
}

// A child closed as done with no pull request — a human's done on an issue short of its merge —
// has nothing to check in production: today's close, every claim closed and the Sandbox released
// with its volume, and no check, no look and no bound.
func TestAChildClosedAsDoneWithNoPullRequestOpensNoCheck(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, Clock: func() time.Time { return time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC) }}, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "child-done", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.closed", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact done: %v", err)
	}
	closed := superviseRequests(t, pool, "issue_close")
	for _, role := range claim.Roles {
		if closed["LEGION-209/"+string(role)] != 1 {
			t.Fatalf("issue_close rows = %v, want one for every role", closed)
		}
	}
	if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 1 || !got[0].Release {
		t.Fatalf("issue suspensions = %+v, want one releasing row", got)
	}
	if check := productionCheck(t, pool, "LEGION-209"); check != nil {
		t.Fatalf("production check = %+v, want none for a child with no pull request", check)
	}
	for _, kind := range []string{"check_look", "check_bound"} {
		assertOutboxCount(t, pool, kind, 0)
	}
	if got := superviseRequests(t, pool, "start"); len(got) != 0 {
		t.Fatalf("start rows = %v, want none", got)
	}
}

// A root's merge while a child runs records the merge and waits: the root stays in awaiting_merge
// with nothing written to Dispatch, no linger deadline, no member suspended and its slot still
// held, and its architect is told once, by name, which issue the close waits for.
func TestAMergedRootWaitsWhileItsChildRuns(t *testing.T) {
	rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
	assertPhase(t, rig.pool, phase.AwaitingMerge)
	var lingering bool
	if err := rig.pool.QueryRow(context.Background(), "select linger_until is not null from issues where key = 'LEGION-208'").Scan(&lingering); err != nil || lingering {
		t.Fatalf("the waiting root lingers = %t, %v; want no linger deadline", lingering, err)
	}
	var state record.PullRequestState
	var mergeSHA string
	if err := rig.pool.QueryRow(context.Background(), "select state, merge_sha from pull_requests where issue = 'LEGION-208'").Scan(&state, &mergeSHA); err != nil {
		t.Fatalf("read the pull request: %v", err)
	}
	if state != record.PullRequestMerged || mergeSHA != "merge" {
		t.Fatalf("the pull request = %s at %q, want it recorded merged at merge", state, mergeSHA)
	}
	assertOutboxCount(t, rig.pool, "dispatch_status", 0)
	assertOutboxCount(t, rig.pool, "linger_close", 0)
	for _, op := range []string{"suspend", "issue_close"} {
		if got := superviseRequests(t, rig.pool, op); len(got) != 0 {
			t.Fatalf("%s rows = %v, want none while the close waits", op, got)
		}
	}
	if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("the child's issue suspensions = %+v, want none: nothing of the tree is suspended", got)
	}
	if got := slottedIssues(t, rig.pool); len(got) != 1 || got[0] != "LEGION-208" {
		t.Fatalf("slots = %v, want the waiting tree's kept", got)
	}
	if check := productionCheck(t, rig.pool, "LEGION-208"); check != nil {
		t.Fatalf("the waiting root's production check = %+v, want none until it closes", check)
	}
	assertWaitNotice(t, rig.pool, "LEGION-209")
}

// While its root's merged close waits, a child's tree is not lingering, so the child's implementer
// completes its phase and the child moves on. Before the wait the root's close lingered the tree
// mid-turn and that completion was refused 409 TREE_LINGERING.
func TestAWaitingRootsChildCompletesItsPhase(t *testing.T) {
	rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
	seedPhase(t, rig.pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "child-implementer"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})

	result := rig.apply("child-implemented", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "child-implementer", Generation: 3, Commit: "head-43", Summary: "implemented"})
	if result.Refusal != nil {
		t.Fatalf("the child's completion = %+v, want it accepted", result.Refusal)
	}
	if got := issueAt(t, rig.pool, "LEGION-209"); got != phase.Testing {
		t.Fatalf("the child after its completion = %s, want testing", got)
	}
	assertPhase(t, rig.pool, phase.AwaitingMerge)
}

// The architect is told once, when the wait begins. A second child leaving while a third still
// runs re-enters the wait and tells it nothing more, and neither does a daemon boot that finds the
// root still waiting: notices never de-duplicate, and a root can wait for days across many boots.
func TestAWaitingRootTellsItsArchitectOnlyWhenTheWaitBegins(t *testing.T) {
	rig := seedMergedRoot(t,
		record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"},
		record.Issue{Key: "LEGION-210", Phase: phase.Testing, Status: "in_progress"},
		record.Issue{Key: "LEGION-211", Phase: phase.Reviewing, Status: "needs_review"})
	assertWaitNotice(t, rig.pool, "LEGION-209, LEGION-210, LEGION-211")

	rig.apply("child-parked", intake.DispatchIssue{Key: "LEGION-210", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "LEGION-210", Parent: "LEGION-208", Rank: "V2"})
	assertPhase(t, rig.pool, phase.AwaitingMerge)
	assertWaitNotice(t, rig.pool, "LEGION-209, LEGION-210, LEGION-211")

	rig.apply("boot", intake.DaemonBooted{Boot: 1})
	assertPhase(t, rig.pool, phase.AwaitingMerge)
	assertWaitNotice(t, rig.pool, "LEGION-209, LEGION-210, LEGION-211")
}

// A merged root closes the moment its last open child leaves the workflow, however it leaves: that
// child's own merge, or the architect parking it to backlog (a Dispatch status event no session
// wrote). The close is the one the merge would have made: done written to Dispatch, the linger
// armed, every claim of the tree suspended and the root's production check opened on its merge.
func TestAMergedRootClosesWhenItsLastOpenChildLeaves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child record.Issue
		leave func(t *testing.T, rig mergedRootRig)
	}{
		{name: "the child's own merge", child: record.Issue{Key: "LEGION-209", Phase: phase.AwaitingMerge, Status: "retro"}, leave: func(t *testing.T, rig mergedRootRig) {
			seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
			rig.apply("child-merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
		}},
		{name: "the architect parks the child", child: record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"}, leave: func(t *testing.T, rig mergedRootRig) {
			rig.apply("child-parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V1"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedMergedRoot(t, tc.child)
			tc.leave(t, rig)
			assertPhase(t, rig.pool, phase.Done)
			var lingering bool
			if err := rig.pool.QueryRow(context.Background(), "select linger_until is not null from issues where key = 'LEGION-208'").Scan(&lingering); err != nil || !lingering {
				t.Fatalf("the closed root lingers = %t, %v; want its linger armed", lingering, err)
			}
			var statusWrites int
			if err := rig.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'dispatch_status' and issue = 'LEGION-208'
				and payload->>'status' = 'done' and payload->>'observedStatus' = 'retro'`).Scan(&statusWrites); err != nil || statusWrites != 1 {
				t.Fatalf("the root's done status writes = %d, %v; want one over retro", statusWrites, err)
			}
			assertOutboxCount(t, rig.pool, "linger_close", 1)
			// The linger suspends every claim of the tree; a child the architect parked was suspended
			// by its own departure first, so only the root's count is exactly one.
			suspended := superviseRequests(t, rig.pool, "suspend")
			for _, issue := range []string{"LEGION-208", "LEGION-209"} {
				for _, role := range claim.Roles {
					key := issue + "/" + string(role)
					if suspended[key] < 1 || issue == "LEGION-208" && suspended[key] != 1 {
						t.Fatalf("the close suspended %v, want every tree claim and the root's once", suspended)
					}
				}
			}
			if check := productionCheck(t, rig.pool, "LEGION-208"); check == nil || !check.Open() || check.Looks != 1 || check.MergeSHA != "merge" {
				t.Fatalf("the root's production check = %+v, want it open at look 1 on merge", check)
			}
			assertWaitNotice(t, rig.pool, "LEGION-209")
		})
	}
}

// An issue that reaches awaiting_merge on an already-merged pull request waits the same way while
// a child runs, and its architect is told there: the notice it got when the pull request merged
// could not say which issues the close waits for, since the issue was not awaiting the merge then.
func TestAnIssueReachingAwaitingMergeOnAMergedPullRequestWaitsForItsTree(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
		MergeSHA: "merge", Failing: []string{}, CheckRuns: []record.AttemptRun{}})

	result := applyFacts(t, pool, readyEngine(""))("ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"})
	if result.Refusal != nil {
		t.Fatalf("the merger's READY = %+v, want it accepted", result.Refusal)
	}
	assertPhase(t, pool, phase.AwaitingMerge)
	assertOutboxCount(t, pool, "linger_close", 0)
	if got := superviseRequests(t, pool, "suspend"); len(got) != 0 {
		t.Fatalf("suspend rows = %v, want none while the close waits", got)
	}
	assertWaitNotice(t, pool, "LEGION-209")
}

// Which members hold a merged root's close up: an issue is in the workflow when its status is one
// the workflow runs in and its phase is short of done, so a child in backlog and a child already
// done hold it not at all, while a released child that has not started a phase and one held
// because its worker ran out of launches both do.
func TestWhichChildrenHoldAMergedRootsCloseUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child record.Issue
		waits bool
	}{
		{name: "parked in backlog", child: record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "backlog"}},
		{name: "already done", child: record.Issue{Key: "LEGION-209", Phase: phase.Done, Status: "done"}},
		{name: "released, no phase started", child: record.Issue{Key: "LEGION-209", Phase: phase.Admitted, Status: "todo"}, waits: true},
		{name: "held out of launches", child: record.Issue{Key: "LEGION-209", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Status: "in_progress"}, waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedMergedRoot(t, tc.child)
			want := phase.Done
			notices := 0
			if tc.waits {
				want, notices = phase.AwaitingMerge, 1
			}
			assertPhase(t, rig.pool, want)
			if got := architectNotices(t, rig.pool); len(got) != notices {
				t.Fatalf("architect notices = %+v, want %d: one exactly when the close waits", got, notices)
			}
		})
	}
}

// A root whose tree holds no other open issue closes at its merge exactly as it did before the
// wait: the close is the merge's, with nothing to wait for and nobody told it waits.
func TestARootWithNoOpenTreeClosesAtItsMerge(t *testing.T) {
	rig := seedMergedRoot(t)
	assertPhase(t, rig.pool, phase.Done)
	assertOutboxCount(t, rig.pool, "linger_close", 1)
	if got := architectNotices(t, rig.pool); len(got) != 0 {
		t.Fatalf("architect notices = %+v, want none: the close waited for nothing", got)
	}
	if check := productionCheck(t, rig.pool, "LEGION-208"); check == nil || !check.Open() || check.MergeSHA != "merge" {
		t.Fatalf("the root's production check = %+v, want it open on merge", check)
	}
}

// A merged root stands in awaiting_merge for as long as its tree runs, so the two facts that would
// take an awaiting pull request back to implementing can land on a merge GitHub already made. A
// push past the head READY named takes neither move (withdrawMovedReady), and a red required check
// withdraws nothing there, as it never has (classify.RedWithdrawsReady): there is nothing left to
// merge or to re-implement.
func TestNothingWithdrawsTheREADYOfAWaitingMergedRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact intake.Fact
	}{
		{name: "a push past the READY head", fact: intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "pushed"}},
		{name: "a red required check", fact: intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", Generation: 7, Snapshot: "settled",
			CheckRuns: []record.AttemptRun{{Name: "pr-checks-result", ID: 1}}, Failing: []string{"pr-checks-result"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
			seedPhase(t, rig.pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: readyPacket})
			rig.apply("moved", tc.fact)
			assertPhase(t, rig.pool, phase.AwaitingMerge)
			if got := superviseRequests(t, rig.pool, "start"); len(got) != 0 {
				t.Fatalf("start rows = %v, want none: no READY was withdrawn", got)
			}
			if got := messageBodies(t, rig.pool); len(got) != 0 {
				t.Fatalf("Dispatch messages = %q, want none: no READY was withdrawn", got)
			}
			for _, notice := range architectNotices(t, rig.pool) {
				if notice.Kind != "pr-merged" {
					t.Fatalf("architect notices hold %s, want the wait's pr-merged alone", notice.Kind)
				}
			}
		})
	}
}

// A merged root can stand in awaiting_merge for days, and a pull request opened from its branch in
// that time is new work, not the change the root waits to close on: GitHub has already merged that
// one and never sends the merge again. The root still closes on that merge when its last child
// leaves, its production check reading pull request #42 at merge. The tree's slot is admission's,
// freed at phase done, and is asserted in admit's
// TestAMergedRootWaitsForItsChildrenAndClosesWhenTheLastLeaves.
func TestANewPullRequestFromAWaitingRootsBranchLeavesItsMergeStanding(t *testing.T) {
	rig := seedMergedRoot(t, record.Issue{Key: "LEGION-209", Phase: phase.Implementing, Status: "in_progress"})
	rig.apply("follow-up-opened", intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 99, Branch: "legion/LEGION-208", HeadSHA: "follow-up",
		UpdatedAt: time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)})
	assertPhase(t, rig.pool, phase.AwaitingMerge)

	rig.apply("child-parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V1"})
	assertPhase(t, rig.pool, phase.Done)
	if check := productionCheck(t, rig.pool, "LEGION-208"); check == nil || !check.Open() || check.Number != 42 || check.MergeSHA != "merge" {
		t.Fatalf("the root's production check = %+v, want it open on pull request #42 at merge", check)
	}
	assertOutboxCount(t, rig.pool, "linger_close", 1)
}

// mergedRootRig is a root whose pull request merged while its tree still ran: the pool it was
// recorded in, and apply, which applies a fact and fails the test on an error.
type mergedRootRig struct {
	pool  *pgxpool.Pool
	apply func(id string, fact intake.Fact) intake.Result
}

// seedMergedRoot seeds the root LEGION-208 awaiting the merge of its open sjawhar/legion#42, the
// slot its tree holds, and one child of it per entry of children — each seeded with the key, phase,
// status and hold its entry names — then merges the root's pull request at merge.
func seedMergedRoot(t *testing.T, children ...record.Issue) mergedRootRig {
	t.Helper()
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour, Clock: func() time.Time { return now }}, nil)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.AwaitingMerge, Generation: 7, Status: "retro", Rank: "U", LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
	parent := "LEGION-208"
	for i, child := range children {
		child.Repo, child.DefaultBranch, child.GitHubRepo = "sjawhar/legion", "main", "sjawhar/legion"
		child.Tree, child.Project, child.Title, child.Parent = "LEGION-208", "LEGION", child.Key, &parent
		child.Generation, child.Rank, child.LastDispatchSeq = 3, fmt.Sprintf("V%d", i+1), 1
		seedIssue(t, pool, child)
	}
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
	rig := mergedRootRig{pool: pool, apply: applyFacts(t, pool, engine)}
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"})
	return rig
}

// assertWaitNotice checks that the architect was told the close waits exactly once: one pr-merged
// notice, naming the pull request and waiting, the keys the close is waiting for. The architect's
// other notices — a child's close or park among them — are not this one's business.
func assertWaitNotice(t *testing.T, pool *pgxpool.Pool, waiting string) {
	t.Helper()
	var got []record.Notice
	for _, notice := range architectNotices(t, pool) {
		if notice.Kind == "pr-merged" {
			got = append(got, notice)
		}
	}
	want := fmt.Sprintf("pull request #42 merged, and LEGION-208's close is waiting for %s, still in the workflow; LEGION-208 closes once the last of them leaves, or take one out of the workflow with park_child", waiting)
	if len(got) != 1 || got[0].Reason != want {
		t.Fatalf("pr-merged notices = %+v, want one reading %q", got, want)
	}
}

// issueAt is the phase issue stands in.
func issueAt(t *testing.T, pool *pgxpool.Pool, issue string) phase.Phase {
	t.Helper()
	var got phase.Phase
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = $1", issue).Scan(&got); err != nil {
		t.Fatalf("read %s's phase: %v", issue, err)
	}
	return got
}

// slottedIssues is the issues holding an admission slot, in slot order.
func slottedIssues(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select issue from slots order by index")
	if err != nil {
		t.Fatalf("read the slots: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect the slots: %v", err)
	}
	return got
}

// seedMergingChild seeds root, its child LEGION-209 awaiting its merge at generation 7 with status
// retro, the child's merger row and its open pull request sjawhar/legion#43.
func seedMergingChild(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	seedIssue(t, pool, root)
	parent := root.Key
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root.Key, Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.AwaitingMerge, Generation: 7, Status: "retro", Rank: "V", LastDispatchSeq: 1})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleMerger, Claim: "child-merger", HandoffCommit: "head-43"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
}

// productionCheck reads issue's production check, nil for none.
func productionCheck(t *testing.T, pool *pgxpool.Pool, issue string) *record.ProductionCheck {
	t.Helper()
	var check *record.ProductionCheck
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		check, err = record.NewStore().ProductionCheck(t.Context(), tx, issue)
		return err
	})
	return check
}

// outboxRow is one outbox row of an issue as the tests read it: its id, when it is due and its
// payload.
type outboxRow struct {
	id      int64
	nextAt  time.Time
	payload []byte
}

// issueOutboxOf is issue's outbox rows of kind, oldest first (outboxOf, repository_test.go, is
// every issue's of a kind).
func issueOutboxOf(t *testing.T, pool *pgxpool.Pool, issue, kind string) []outboxRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select id, next_at, payload from outbox where issue = $1 and kind = $2 order by id", issue, kind)
	if err != nil {
		t.Fatalf("read %s rows of %s: %v", kind, issue, err)
	}
	defer rows.Close()
	var got []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.id, &row.nextAt, &row.payload); err != nil {
			t.Fatalf("scan %s row: %v", kind, err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s rows: %v", kind, err)
	}
	return got
}

// superviseRowsOf decodes issue's supervise rows of op, oldest first.
func superviseRowsOf(t *testing.T, pool *pgxpool.Pool, issue, op string) []record.SuperviseRequest {
	t.Helper()
	var requests []record.SuperviseRequest
	for _, row := range issueOutboxOf(t, pool, issue, "supervise") {
		if request := decodeSupervise(t, row); string(request.Op) == op {
			requests = append(requests, request)
		}
	}
	return requests
}

// decodeSupervise is the supervise request a supervise row carries.
func decodeSupervise(t *testing.T, row outboxRow) record.SuperviseRequest {
	t.Helper()
	var request record.SuperviseRequest
	if err := json.Unmarshal(row.payload, &request); err != nil {
		t.Fatalf("decode supervise row %d: %v", row.id, err)
	}
	return request
}

// decodeLook is the look a check_look row names.
func decodeLook(t *testing.T, row outboxRow) int {
	t.Helper()
	var look record.CheckLook
	if err := json.Unmarshal(row.payload, &look); err != nil {
		t.Fatalf("decode check_look row %d: %v", row.id, err)
	}
	return look.Look
}
