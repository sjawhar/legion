package workflow

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// envoyDeploy is the deploy the deploy-run tests' project names: a deployment repository's shape,
// its Apply step in a top-level envoy.yaml, on the check rigs' repository.
var envoyDeploy = config.Deploy{Repo: ghrepo.MustParse("sjawhar/legion"), Workflow: ".github/workflows/envoy.yaml", Step: "Apply"}

// deployRunDetail is a deploy run's wake detail as the check_compare executor writes it.
const deployRunDetail = "sjawhar/legion .github/workflows/envoy.yaml run 40203304600 attempt 1, job apply: Apply succeeded at 2026-10-19T05:00:00Z"

// withDeploys gives rig an engine whose project names deploys, on the rig's clock and journal: the
// facts applied so far stay recorded, and every later one goes through the new engine.
func withDeploys(t *testing.T, rig checkRig, deploys ...config.Deploy) checkRig {
	t.Helper()
	rig.engine = New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: checkBound, Deploys: deploys, Clock: rig.clock.Now},
		slog.New(slog.NewTextHandler(rig.logged, nil)))
	rig.apply = applyFacts(t, rig.pool, rig.engine)
	return rig
}

// openRootCheck records root issue key on repo, whose default branch is branch, with its pull
// request number merged, so its production check is open at look 1.
func openRootCheck(t *testing.T, rig checkRig, key, repo, branch string, number int) {
	t.Helper()
	seedIssue(t, rig.pool, record.Issue{Key: key, Repo: repo, DefaultBranch: branch, GitHubRepo: repo, Tree: key, Project: "LEGION", Title: key, Phase: phase.AwaitingMerge, Generation: 2, Status: "retro", Rank: "W"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: repo, Number: number, Branch: "legion/" + key, HeadSHA: "head-" + key, Failing: []string{}})
	rig.apply(key+"-merged", intake.PullRequestMerged{Repo: repo, Number: number, MergeSHA: "merge-" + key})
	if check := productionCheck(t, rig.pool, key); check == nil || !check.Open() {
		t.Fatalf("%s's check after its merge = %+v, want it open", key, check)
	}
}

// A completed run of a deploy workflow queues one check_compare row, carrying the run and the
// deploy's step, for each open check of the deploy's repository — named in any case, as GitHub
// matches names — whose issue's default branch is the run's branch, and logs the run once. A run
// of another branch queues rows only for the checks on that branch.
func TestADeployRunQueuesACompareForEachOpenCheckOnItsBranch(t *testing.T) {
	rig := withDeploys(t, childCheckRig(t, ""), envoyDeploy)
	openRootCheck(t, rig, "LEGION-300", "sjawhar/legion", "main", 7)
	openRootCheck(t, rig, "LEGION-301", "acme/widgets", "main", 8)
	openRootCheck(t, rig, "LEGION-302", "sjawhar/legion", "release", 9)

	rig.apply("run", intake.WorkflowRunCompleted{Repo: "SJAWHAR/Legion", Path: ".github/workflows/envoy.yaml", RunID: 40203304600, Attempt: 2, HeadBranch: "main"})
	run := record.DeployRun{Run: 40203304600, Attempt: 2, Workflow: ".github/workflows/envoy.yaml", Step: "Apply", Branch: "main"}
	for _, issue := range []string{"LEGION-209", "LEGION-300"} {
		want := []record.CheckCompare{{Look: 1, Cause: "deploy run", Deploy: &run, OpenedAt: checkOpened}}
		if got := checkCompares(t, rig.pool, issue); !reflect.DeepEqual(got, want) {
			t.Fatalf("check_compare rows of %s = %+v, want one for look 1 carrying the run", issue, got)
		}
	}
	for _, issue := range []string{"LEGION-301", "LEGION-302"} {
		if got := checkCompares(t, rig.pool, issue); len(got) != 0 {
			t.Fatalf("check_compare rows of %s = %+v, want none: another repository or another branch", issue, got)
		}
	}
	if logged := rig.logged.String(); strings.Count(logged, "a deploy run is compared against the open production checks") != 1 ||
		!strings.Contains(logged, `repo=SJAWHAR/Legion workflow=.github/workflows/envoy.yaml run=40203304600 attempt=2 branch=main checks=2`) {
		t.Fatalf("the journal holds no one line naming the run and its two checks:\n%s", logged)
	}

	rig.apply("release-run", intake.WorkflowRunCompleted{Repo: "sjawhar/legion", Path: ".github/workflows/envoy.yaml", RunID: 40203304601, Attempt: 1, HeadBranch: "release"})
	want := []record.CheckCompare{{Look: 1, Cause: "deploy run", Deploy: &record.DeployRun{Run: 40203304601, Attempt: 1, Workflow: ".github/workflows/envoy.yaml", Step: "Apply", Branch: "release"}, OpenedAt: checkOpened}}
	if got := checkCompares(t, rig.pool, "LEGION-302"); !reflect.DeepEqual(got, want) {
		t.Fatalf("check_compare rows of the release check after a release run = %+v, want %+v", got, want)
	}
	for _, issue := range []string{"LEGION-209", "LEGION-300"} {
		if got := checkCompares(t, rig.pool, issue); len(got) != 1 {
			t.Fatalf("check_compare rows of %s after a release run = %+v, want its one", issue, got)
		}
	}
}

// A run of a workflow the project names no deploy, a run in a repository it names none in, and any
// run in a project with no deploys queue nothing and log nothing.
func TestARunOfNoDeployQueuesNothing(t *testing.T) {
	widgets := config.Deploy{Repo: ghrepo.MustParse("acme/widgets"), Workflow: ".github/workflows/deploy.yml", Step: "Apply"}
	for _, tc := range []struct {
		name    string
		deploys []config.Deploy
		run     intake.WorkflowRunCompleted
	}{
		{name: "another workflow", deploys: []config.Deploy{envoyDeploy, widgets},
			run: intake.WorkflowRunCompleted{Repo: "sjawhar/legion", Path: ".github/workflows/ci.yaml", RunID: 1, Attempt: 1, HeadBranch: "main"}},
		{name: "another repository's workflow of the same file", deploys: []config.Deploy{envoyDeploy, widgets},
			run: intake.WorkflowRunCompleted{Repo: "sjawhar/legion", Path: ".github/workflows/deploy.yml", RunID: 1, Attempt: 1, HeadBranch: "main"}},
		{name: "another repository", deploys: []config.Deploy{envoyDeploy},
			run: intake.WorkflowRunCompleted{Repo: "acme/widgets", Path: ".github/workflows/envoy.yaml", RunID: 1, Attempt: 1, HeadBranch: "main"}},
		{name: "no deploys",
			run: intake.WorkflowRunCompleted{Repo: "sjawhar/legion", Path: ".github/workflows/envoy.yaml", RunID: 1, Attempt: 1, HeadBranch: "main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := withDeploys(t, childCheckRig(t, ""), tc.deploys...)
			openRootCheck(t, rig, "LEGION-301", "acme/widgets", "main", 8)
			rig.apply("run", tc.run)
			for _, issue := range []string{"LEGION-209", "LEGION-301"} {
				if got := checkCompares(t, rig.pool, issue); len(got) != 0 {
					t.Fatalf("check_compare rows of %s = %+v, want none", issue, got)
				}
			}
			if strings.Contains(rig.logged.String(), "a deploy run is compared") {
				t.Fatalf("the journal logs a compare of a run of no deploy:\n%s", rig.logged.String())
			}
		})
	}
}

// A deploy run queues nothing for a check whose last look has started, since no look follows, nor
// for a closed check.
func TestADeployRunQueuesNothingForACheckInItsLastLookOrClosed(t *testing.T) {
	run := intake.WorkflowRunCompleted{Repo: "sjawhar/legion", Path: ".github/workflows/envoy.yaml", RunID: 40203304600, Attempt: 1, HeadBranch: "main"}
	t.Run("last look", func(t *testing.T) {
		rig := withDeploys(t, childCheckRig(t, ""), envoyDeploy)
		rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending"})
		rig.clock.now = checkOpened.Add(checkBound)
		rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
		if check := productionCheck(t, rig.pool, "LEGION-209"); !check.Open() || !check.Last {
			t.Fatalf("the check at its bound = %+v, want its last look started", check)
		}
		rig.apply("run", run)
		if got := checkCompares(t, rig.pool, "LEGION-209"); len(got) != 0 {
			t.Fatalf("check_compare rows for a check in its last look = %+v, want none", got)
		}
	})
	t.Run("closed", func(t *testing.T) {
		rig := withDeploys(t, childCheckRig(t, ""), envoyDeploy)
		rig.apply("pass", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pass", Summary: "seen"})
		if check := productionCheck(t, rig.pool, "LEGION-209"); check.Open() {
			t.Fatalf("the check after a pass = %+v, want it closed", check)
		}
		rig.apply("run", run)
		if got := checkCompares(t, rig.pool, "LEGION-209"); len(got) != 0 {
			t.Fatalf("check_compare rows for a closed check = %+v, want none", got)
		}
	})
}

// A deploy run's wake with no look open starts the next look, whose task names the run. One that
// arrives while a look is open is kept, and the look's pending report starts the next look with it
// at once.
func TestADeployRunWakeStartsTheNextLookOrIsKeptForIt(t *testing.T) {
	rig := childCheckRig(t, "")
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending"})
	rig.clock.now = checkOpened.Add(time.Hour)
	rig.apply("deploy-1", intake.CheckWake{Issue: "LEGION-209", Look: 1, Cause: "deploy run", Detail: deployRunDetail, OpenedAt: checkOpened})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Looks != 2 || !check.LookOpen || check.LastWakeKind != "deploy run" || check.LastWakeDetail != deployRunDetail || check.PendingCauseKind != "" {
		t.Fatalf("the check after a deploy run's wake = %+v, want look 2 open, woken by the run", check)
	}
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if len(starts) != 2 || starts[1].CheckLook != 2 || !strings.Contains(starts[1].Task, "Look 2, woken by the deploy workflow's run: "+deployRunDetail+".") {
		t.Fatalf("start rows = %+v, want look 2's naming the run", starts)
	}
	if !strings.Contains(rig.logged.String(), `production check woken" issue=LEGION-209 mergeCommit=merge-43 kind="deploy run"`) {
		t.Fatalf("the journal holds no wake naming the deploy run:\n%s", rig.logged.String())
	}

	second := strings.Replace(deployRunDetail, "run 40203304600 attempt 1", "run 40203304700 attempt 1", 1)
	rig.apply("deploy-2", intake.CheckWake{Issue: "LEGION-209", Look: 2, Cause: "deploy run", Detail: second, OpenedAt: checkOpened})
	check = productionCheck(t, rig.pool, "LEGION-209")
	if check.Looks != 2 || !check.LookOpen || check.LastWakeDetail != deployRunDetail || check.PendingCauseKind != "deploy run" || check.PendingCauseDetail != second {
		t.Fatalf("the check after a deploy run during look 2 = %+v, want look 2 open with the second run kept", check)
	}
	suspends := len(superviseRequests(t, rig.pool, "suspend"))
	rig.apply("pending-2", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pending"})
	check = productionCheck(t, rig.pool, "LEGION-209")
	if check.Looks != 3 || !check.LookOpen || check.LastWakeKind != "deploy run" || check.LastWakeDetail != second || check.PendingCauseKind != "" {
		t.Fatalf("the check after look 2's pending = %+v, want look 3 open, woken by the kept run", check)
	}
	if starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start"); len(starts) != 3 || starts[2].CheckLook != 3 ||
		!strings.Contains(starts[2].Task, "Look 3, woken by the deploy workflow's run: "+second+".") {
		t.Fatalf("start rows = %+v, want look 3's naming the kept run", starts)
	}
	if got := len(superviseRequests(t, rig.pool, "suspend")); got != suspends {
		t.Fatalf("suspend rows = %d after look 2's pending, want %d: the next look started at once", got, suspends)
	}
}

// One cause is kept for the next look: a deploy run's wake behind a kept build or a kept
// issue.deployed is ignored, and so is a build's behind a kept deploy run.
func TestADeployRunWakeAndACauseKeptBeforeIt(t *testing.T) {
	deployRun := intake.CheckWake{Issue: "LEGION-209", Look: 1, Cause: "deploy run", Detail: deployRunDetail, OpenedAt: checkOpened}
	build := intake.CheckWake{Issue: "LEGION-209", Look: 1, Cause: "daemon boot", Detail: "b00f", OpenedAt: checkOpened}
	deployed := intake.IssueDeployed{Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, RunID: 77, CompletedAt: checkOpened}
	for _, tc := range []struct {
		name                 string
		kept, later          intake.Fact
		keptKind, keptDetail string
		laterKind            string
	}{
		{name: "a deploy run behind a build", kept: build, later: deployRun, keptKind: "daemon boot", keptDetail: "b00f", laterKind: "deploy run"},
		{name: "a deploy run behind issue.deployed", kept: deployed, later: deployRun, keptKind: "issue.deployed", keptDetail: "run 77 completed 2026-10-19T04:00:00Z", laterKind: "deploy run"},
		{name: "a build behind a deploy run", kept: deployRun, later: build, keptKind: "deploy run", keptDetail: deployRunDetail, laterKind: "daemon boot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t, "")
			rig.apply("kept", tc.kept)
			rig.apply("later", tc.later)
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check.Looks != 1 || !check.LookOpen || check.PendingCauseKind != tc.keptKind || check.PendingCauseDetail != tc.keptDetail {
				t.Fatalf("the check = %+v, want look 1 open with %s %q kept", check, tc.keptKind, tc.keptDetail)
			}
			if !strings.Contains(rig.logged.String(), `wake ignored" issue=LEGION-209 mergeCommit=merge-43 kind="`+tc.laterKind+`"`) {
				t.Fatalf("the journal holds no ignored %s:\n%s", tc.laterKind, rig.logged.String())
			}
			rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending"})
			if check := productionCheck(t, rig.pool, "LEGION-209"); check.Looks != 2 || check.LastWakeKind != tc.keptKind || check.PendingCauseKind != "" {
				t.Fatalf("the check after look 1's pending = %+v, want look 2 woken by the kept %s alone", check, tc.keptKind)
			}
		})
	}
}
