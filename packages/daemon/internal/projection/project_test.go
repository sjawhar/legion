package projection

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

type projectionStore struct {
	record.Store
	issues       []record.Issue
	phases       map[string][]record.PhaseRow
	slots        []record.Slot
	pullRequests map[string]*record.PullRequest
	checks       []record.ProductionCheck
	held         []record.HeldRow
	// mirror is what Dispatch says each issue of the project waits on, as the store lists it: by
	// key, which is the order the state's `blocked` is judged from. DispatchIssuesOf serves the rows
	// of the keys asked for and of what they wait on, as the real store does.
	mirror []record.DispatchIssueRow
	// lifecycles names the trees whose lifecycle stands open under the workflow, which is what
	// says a tree runs (record.TreeLive); gates is the design gate of each root that has one;
	// owed is the start each issue owes a worker of it, which is a returning issue's place in
	// the waiting line. The three are what record.ReadLineFacts reads for the line.
	lifecycles []string
	gates      map[string]record.DesignGate
	owed       []record.OwedStart
}

func (s projectionStore) TreeLifecycles(_ context.Context, _ pgx.Tx, project string, trees []string) (map[string]treelifecycle.Lifecycle, error) {
	open := make(map[string]treelifecycle.Lifecycle, len(s.lifecycles))
	for _, tree := range s.lifecycles {
		if slices.Contains(trees, tree) {
			open[tree] = treelifecycle.Lifecycle{Project: project, Tree: tree, Epoch: 1, Authority: treelifecycle.AuthorityWorkflow}
		}
	}
	return open, nil
}

func (s projectionStore) Gates(_ context.Context, _ pgx.Tx, trees []string) (map[string]record.DesignGate, error) {
	gates := make(map[string]record.DesignGate, len(s.gates))
	for _, tree := range trees {
		if gate, held := s.gates[tree]; held {
			gates[tree] = gate
		}
	}
	return gates, nil
}

func (s projectionStore) OwedStarts(context.Context, pgx.Tx, string) ([]record.OwedStart, error) {
	return s.owed, nil
}

func (s projectionStore) Issues(context.Context, pgx.Tx) ([]record.Issue, error) {
	return s.issues, nil
}
func (s projectionStore) Slots(context.Context, pgx.Tx) ([]record.Slot, error) { return s.slots, nil }
func (projectionStore) PendingStatusWrites(context.Context, pgx.Tx, string) ([]record.OutboxRow, error) {
	return nil, nil
}
func (s projectionStore) HeldRows(context.Context, pgx.Tx, string, string, time.Time) ([]record.HeldRow, error) {
	return s.held, nil
}
func (s projectionStore) Phases(_ context.Context, _ pgx.Tx, issue string) ([]record.PhaseRow, error) {
	return s.phases[issue], nil
}
func (s projectionStore) PullRequest(_ context.Context, _ pgx.Tx, issue string) (*record.PullRequest, error) {
	return s.pullRequests[issue], nil
}
func (projectionStore) Gate(context.Context, pgx.Tx, string) (*record.DesignGate, error) {
	return nil, nil
}
func (s projectionStore) OpenProductionChecks(context.Context, pgx.Tx) ([]record.ProductionCheck, error) {
	return s.checks, nil
}
func (s projectionStore) DispatchIssuesOf(_ context.Context, _ pgx.Tx, _ string, keys []string) ([]record.DispatchIssueRow, error) {
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[key] = true
	}
	for _, row := range s.mirror {
		if wanted[row.Key] {
			for _, blocker := range row.Blockers {
				wanted[blocker] = true
			}
		}
	}
	var rows []record.DispatchIssueRow
	for _, row := range s.mirror {
		if wanted[row.Key] {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// DispatchChildren groups the mirror's rows under the parent each names, as the real store does:
// the children-first judgement reads it for every issue whose own first phase has not started
// (record.ChildWaitsOf).
func (s projectionStore) DispatchChildren(_ context.Context, _ pgx.Tx, _ string, parents []string) (map[string][]record.DispatchIssueRow, error) {
	children := make(map[string][]record.DispatchIssueRow, len(parents))
	for _, row := range s.mirror {
		if row.Parent != "" && slices.Contains(parents, row.Parent) {
			children[row.Parent] = append(children[row.Parent], row)
		}
	}
	return children, nil
}

// IssuesOf is the record of each of keys this store holds, as the real store reads them by
// primary key: the children-first judgement reads the record of every child it judges, which
// may be a member that has left the workflow and so is not among the live issues.
func (s projectionStore) IssuesOf(_ context.Context, _ pgx.Tx, _ string, keys []string) ([]record.Issue, error) {
	var issues []record.Issue
	for _, issue := range s.issues {
		if slices.Contains(keys, issue.Key) {
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

// projectedAt is the fixed instant every test here projects at.
var projectedAt = time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)

func TestProjectShowsOnlySlotlessTodoIssuesInDispatchRankOrder(t *testing.T) {
	now := projectedAt
	store := projectionStore{
		issues: []record.Issue{
			{Key: "LEGION-210", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-210", Phase: phase.Done, Status: "done", Rank: "00000"},
			{Key: "LEGION-211", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-211", Phase: phase.Admitted, Status: "backlog", Rank: "00001"},
			{Key: "LEGION-212", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-212", Phase: phase.Admitted, Status: "todo", Rank: "00004", LastDispatchSeq: 1, HandedOver: true},
			// A root another tree handed over carrying the phase it ran there waits for its tree
			// to be opened like any other: its promotion opens it and starts that phase's role.
			{Key: "LEGION-213", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-213", Status: "todo", Rank: "00003", LastDispatchSeq: 10, Phase: phase.Implementing, HandedOver: true, HandedOverFrom: "LEGION-200"},
			{Key: "LEGION-214", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-214", Phase: phase.Admitted, Status: "todo", Rank: "00002", HandedOver: true},
		},
		slots: []record.Slot{{Issue: "LEGION-214", Index: 0, AdmittedAt: now}},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !reflect.DeepEqual(got.Admission.Active, []string{"LEGION-214"}) {
		t.Fatalf("active = %#v, want the slotted issue", got.Admission.Active)
	}
	if !reflect.DeepEqual(got.Admission.Waiting, []string{"LEGION-213", "LEGION-212"}) {
		t.Fatalf("waiting = %#v, want slotless todo issues in rank order", got.Admission.Waiting)
	}
	if got.Issues["LEGION-213"].Phase != phase.Implementing {
		t.Fatalf("projected phase = %q, want %q", got.Issues["LEGION-213"].Phase, phase.Implementing)
	}
	var _ api.State = got
}

// A root in awaiting_person shows what it waits on: the question, or the spec version of the gate
// shape, when the wait began, and the answer only once a person gave one. A root in any other
// phase shows no wait, even with a stale Awaiting on its record.
func TestProjectShowsWhatAnAwaitingRootWaitsOn(t *testing.T) {
	since := projectedAt.Add(-time.Hour)
	answeredAt := projectedAt.Add(-time.Minute)
	store := projectionStore{issues: []record.Issue{
		{
			Key: "LEGION-ASK", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-ASK",
			Phase: phase.AwaitingPerson, Status: "in_progress", Rank: "A", HandedOver: true,
			Awaiting: &record.Awaiting{
				Ask: "ask-1", Since: record.EventMark{Seq: 9, At: since},
				AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByReply,
			},
		},
		{
			Key: "LEGION-GATE", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-GATE",
			Phase: phase.AwaitingPerson, Status: "in_progress", Rank: "B", HandedOver: true,
			Awaiting: &record.Awaiting{GateVersion: 4, Artifact: "artifact-gate"},
		},
		{
			Key: "LEGION-RUN", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-RUN",
			Phase: phase.Admitted, Status: "in_progress", Rank: "C", HandedOver: true,
			Awaiting: &record.Awaiting{Ask: "ask-2"},
		},
	}}

	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	ask := got.Issues["LEGION-ASK"].WaitingOn
	if ask == nil || ask.Kind != "ask" || ask.Ask != "ask-1" || ask.Since == nil || !ask.Since.Equal(since) ||
		ask.AnsweredAt == nil || !ask.AnsweredAt.Equal(answeredAt) || ask.AnsweredBy != record.AnsweredByReply {
		t.Fatalf("ask waitingOn = %#v, want ask-1 since %s answered by reply at %s", ask, since, answeredAt)
	}
	gate := got.Issues["LEGION-GATE"].WaitingOn
	if gate == nil || gate.Kind != "gate" || gate.GateVersion != 4 || gate.ArtifactID != "artifact-gate" ||
		gate.Since != nil || gate.AnsweredAt != nil || gate.AnsweredBy != "" {
		t.Fatalf("gate waitingOn = %#v, want unanswered gate version 4 of artifact-gate with no start time", gate)
	}
	if run := got.Issues["LEGION-RUN"].WaitingOn; run != nil {
		t.Fatalf("admitted root waitingOn = %#v, want none", run)
	}
}

func TestProjectShowsOperatorSpawnedClaimsWithoutRecordIssue(t *testing.T) {
	const issue = "S2-1"
	architect := claim.Token("legion-s2-1-architect")
	implementer := claim.Token("legion-s2-1-implementer")
	got, err := Project(context.Background(), nil, projectionStore{}, "LEGION", []supervise.Claim{
		{Token: architect, Issue: issue, Role: claim.RoleArchitect, State: supervise.StateReady, Session: "session-1"},
		{Token: implementer, Issue: issue, Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "session-2"},
	}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view, ok := got.Issues[issue]
	if !ok {
		t.Fatalf("operator-spawned issue %q is absent from the projected state", issue)
	}
	if view.Key != issue {
		t.Fatalf("operator-spawned issue key = %q, want %q", view.Key, issue)
	}
	architectView := api.ClaimView{Session: "session-1", State: string(supervise.StateReady)}
	if !reflect.DeepEqual(view.Architect, &architectView) {
		t.Fatalf("operator-spawned architect = %#v, want %#v", view.Architect, &architectView)
	}
	implementerView := api.ClaimView{Session: "session-2", State: string(supervise.StateIdle)}
	if got, want := view.Workers[claim.RoleImplementer], (api.PhaseView{Claim: implementerView}); !reflect.DeepEqual(got, want) {
		t.Fatalf("operator-spawned implementer = %#v, want %#v", got, want)
	}
	// The row is listed for the claim's sake and says the workflow does not record its issue.
	// Empty strings here refused the whole state document at every strict client — the contract's
	// own parse of that document is packages/contracts' state-operator-claim fixture.
	if view.Phase != api.Unrecorded || view.Status != string(api.Unrecorded) {
		t.Fatalf("operator-spawned issue = phase %q status %q, want both %q", view.Phase, view.Status, api.Unrecorded)
	}
}

// A claim's recorded plugin contract passes through to its view, 0 when nothing was recorded.
func TestProjectShowsEachClaimsPluginContract(t *testing.T) {
	const issue = "S2-2"
	architect := claim.Token("legion-s2-2-architect")
	implementer := claim.Token("legion-s2-2-implementer")
	got, err := Project(context.Background(), nil, projectionStore{}, "LEGION", []supervise.Claim{
		{Token: architect, Issue: issue, Role: claim.RoleArchitect, State: supervise.StateReady, Session: "session-1", PluginContract: 24},
		{Token: implementer, Issue: issue, Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "session-2"},
	}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view := got.Issues[issue]
	if view.Architect.PluginContract != 24 {
		t.Fatalf("architect pluginContract = %d, want 24", view.Architect.PluginContract)
	}
	if got := view.Workers[claim.RoleImplementer].Claim.PluginContract; got != 0 {
		t.Fatalf("unregistered implementer pluginContract = %d, want 0", got)
	}
}

func TestProjectShowsLaunchUncertainClaimsWithoutALocator(t *testing.T) {
	token := claim.Token("legion-208-architect")
	store := projectionStore{
		issues: []record.Issue{{Key: "LEGION-208", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Status: "todo", Rank: "00042U"}},
		phases: map[string][]record.PhaseRow{
			"LEGION-208": {{Issue: "LEGION-208", Role: claim.RoleArchitect, Claim: token}},
		},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{{
		Token: token, Issue: "LEGION-208", Role: claim.RoleArchitect, State: supervise.StateLaunchUncertain,
	}}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view := got.Issues["LEGION-208"].Architect
	if view == nil {
		t.Fatal("launch_uncertain architect is absent from the projected state")
	}
	if view.State != string(supervise.StateLaunchUncertain) || view.Locator != nil {
		t.Fatalf("launch_uncertain architect = %#v, want its state with no locator", view)
	}
}

// A worker's view shows the handoff of its current phase as its completion left it: the Dispatch
// handoff record a file-backed phase's completion named, with its version, and the commit a
// completion reported; a worker whose phase has not completed shows neither.
func TestProjectShowsTheHandoffRecordAWorkerNamed(t *testing.T) {
	const record1 = "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	tester, implementer, reviewer := claim.Token("legion-208-tester"), claim.Token("legion-208-implementer"), claim.Token("legion-208-reviewer")
	store := projectionStore{
		issues: []record.Issue{{Key: "LEGION-208", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Status: "needs_review", Phase: phase.Reviewing}},
		phases: map[string][]record.PhaseRow{"LEGION-208": {
			{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: implementer, HandoffCommit: "head", HandoffRecord: "implement-record", HandoffVersion: 2, Rounds: 1},
			{Issue: "LEGION-208", Role: claim.RoleTester, Claim: tester, HandoffRecord: record1, HandoffVersion: 3},
			{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: reviewer, LastHandoff: "review-record"},
		}},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{
		{Token: implementer, Issue: "LEGION-208", Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "ses_implementer"},
		{Token: tester, Issue: "LEGION-208", Role: claim.RoleTester, State: supervise.StateIdle, Session: "ses_tester"},
		{Token: reviewer, Issue: "LEGION-208", Role: claim.RoleReviewer, State: supervise.StateIdle, Session: "ses_reviewer"},
	}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	workers := got.Issues["LEGION-208"].Workers
	if got, want := workers[claim.RoleTester].HandoffRecord, (&api.HandoffRecordView{ID: record1, Version: 3}); !reflect.DeepEqual(got, want) || workers[claim.RoleTester].HandoffCommit != "" {
		t.Fatalf("the tester's handoff = record %+v, commit %q; want %+v and no commit", got, workers[claim.RoleTester].HandoffCommit, want)
	}
	if got, want := workers[claim.RoleImplementer].HandoffRecord, (&api.HandoffRecordView{ID: "implement-record", Version: 2}); !reflect.DeepEqual(got, want) || workers[claim.RoleImplementer].HandoffCommit != "head" {
		t.Fatalf("the implementer's handoff = record %+v, commit %q; want %+v at head", got, workers[claim.RoleImplementer].HandoffCommit, want)
	}
	if got := workers[claim.RoleReviewer]; got.HandoffRecord != nil || got.HandoffCommit != "" {
		t.Fatalf("the reviewer mid-round shows handoff %+v, commit %q; want none: the record it named last round is not this round's", got.HandoffRecord, got.HandoffCommit)
	}
}

// The pull request's review decision on the state route is the latest review round's, the one the
// workflow ends the round on when the reviewer completes - whether or not the reviewer's pane is
// running - and no decision shows before a review in a round decides.
func TestProjectShowsTheReviewRoundsDecisionOnThePullRequest(t *testing.T) {
	store := projectionStore{
		issues: []record.Issue{
			{Key: "LEGION-208", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Status: "needs_review", Phase: phase.Reviewing},
			{Key: "LEGION-209", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-209", Status: "needs_review", Phase: phase.Reviewing},
		},
		phases: map[string][]record.PhaseRow{
			"LEGION-208": {{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "gone-reviewer",
				Decision: &record.ReviewDecision{State: "changes_requested", Body: "rename it", Head: "head"}}},
			"LEGION-209": {{Issue: "LEGION-209", Role: claim.RoleReviewer, Claim: "gone-reviewer"}},
		},
		pullRequests: map[string]*record.PullRequest{
			"LEGION-208": {Issue: "LEGION-208", Number: 42, HeadSHA: "head-2"},
			"LEGION-209": {Issue: "LEGION-209", Number: 43, HeadSHA: "head"},
		},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if pr := got.Issues["LEGION-208"].PullRequest; pr == nil || pr.ReviewDecision != "changes_requested" {
		t.Fatalf("LEGION-208's pull request = %+v, want the round's changes_requested", pr)
	}
	if pr := got.Issues["LEGION-209"].PullRequest; pr == nil || pr.ReviewDecision != "" {
		t.Fatalf("LEGION-209's pull request = %+v, want no decision before one is made", pr)
	}
}

// An issue's open production check shows on its record as the check view: the repository, pull
// request and merge commit, the looks and whether the latest is open, what woke it — absent
// before a look wakes — the bound as the next wake, and whether the last look has started. An
// issue the store lists no open check for, closed or never opened, shows none.
func TestProjectShowsTheOpenProductionCheckOnItsIssue(t *testing.T) {
	opened := projectedAt.Add(-2 * time.Hour)
	woke := projectedAt.Add(-time.Hour)
	store := projectionStore{
		issues: []record.Issue{
			{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Status: "done", Phase: phase.Done},
			{Key: "LEGION-209", Project: "LEGION", Tree: "LEGION-209", Status: "done", Phase: phase.Done},
			{Key: "LEGION-210", Project: "LEGION", Tree: "LEGION-210", Status: "done", Phase: phase.Done},
		},
		checks: []record.ProductionCheck{
			{Issue: "LEGION-208", Repo: "acme/widgets", Number: 42, MergeSHA: "merge-208", OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour),
				Looks: 2, LookOpen: true, LookWakeAt: &woke, LastWakeKind: "issue.deployed", LastWakeDetail: "run 7 completed 2026-09-22T23:00:00Z", LastWakeAt: &woke, Last: true},
			{Issue: "LEGION-209", Repo: "acme/widgets", Number: 43, OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour)},
		},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{}, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	want := &api.CheckView{
		Repo: "acme/widgets", PullRequest: 42, MergeCommit: "merge-208", OpenedAt: opened, Looks: 2, LookOpen: true,
		LastWake:   &api.CheckWakeView{Kind: "issue.deployed", Detail: "run 7 completed 2026-09-22T23:00:00Z", At: woke},
		NextWakeAt: opened.Add(336 * time.Hour), Last: true,
	}
	if check := got.Issues["LEGION-208"].Check; !reflect.DeepEqual(check, want) {
		t.Fatalf("LEGION-208's check = %+v, want %+v", check, want)
	}
	if check := got.Issues["LEGION-209"].Check; check == nil || check.LastWake != nil || check.MergeCommit != "" || check.Looks != 0 {
		t.Fatalf("LEGION-209's check = %+v, want the check with no wake and no merge commit yet", check)
	}
	if check := got.Issues["LEGION-210"].Check; check != nil {
		t.Fatalf("LEGION-210's check = %+v, want none", check)
	}
}

// A claim's capability report (LEGION-663) shows on its view, whatever process reported it — the
// view carries that process's incarnation for the reader to compare — and a claim no session has
// reported for shows no capabilities rather than an empty report.
func TestProjectShowsACapabilityReportOnlyOnTheClaimThatSentOne(t *testing.T) {
	const issue = "LEGION-208"
	architect := claim.Token("legion-legion-legion-208-architect")
	implementer := claim.Token("legion-legion-legion-208-implementer")
	report := capabilities.Report{
		Claim: implementer, Generation: 2,
		MeasuredAt: time.Date(2026, 10, 10, 2, 18, 59, 0, time.UTC),
		Rows: []capabilities.Row{
			{Name: capabilities.Subagents, OK: true, Detail: "6 task agents discovered"},
			{Name: capabilities.GitHub, Detail: "gh api user: HTTP 401"},
		},
	}
	report.Locator.Incarnation = "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/1"
	got, err := Project(context.Background(), nil, projectionStore{}, "LEGION", []supervise.Claim{
		{Token: architect, Issue: issue, Role: claim.RoleArchitect, State: supervise.StateReady, Session: "session-1"},
		{Token: implementer, Issue: issue, Role: claim.RoleImplementer, State: supervise.StateWorking, Session: "session-2"},
	}, map[claim.Token]capabilities.Report{implementer: report}, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	view := got.Issues[issue]
	if view.Architect == nil || view.Architect.Capabilities != nil {
		t.Fatalf("architect = %#v, want its view with no capabilities: no session reported for it", view.Architect)
	}
	want := &api.CapabilityReportView{
		MeasuredAt:  report.MeasuredAt,
		Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11/1",
		OK:          []string{"subagents"},
		Open:        []api.CapabilityGap{{Name: "github", Detail: "gh api user: HTTP 401"}},
	}
	if got := view.Workers[claim.RoleImplementer].Claim.Capabilities; !reflect.DeepEqual(got, want) {
		t.Fatalf("implementer capabilities = %#v, want %#v", got, want)
	}
}

// A due lifecycle row held behind its issue's running one (record.Store.HeldRows) shows on the
// state with its id, kind, issue, the row it waits on and how long it has waited, measured from
// the moment it became due; a row due in the future — not yet held, whatever the store says —
// shows no negative wait. The rows keep the store's order, and a state with none carries an empty
// list rather than none.
func TestProjectShowsTheHeldLifecycleRowsWithTheirWaits(t *testing.T) {
	start := record.OutboxRow{ID: 802, Kind: record.OutboxKindSupervise, Issue: "LEGION-208", NextAt: projectedAt.Add(-150 * time.Second)}
	removal := record.OutboxRow{ID: 804, Kind: record.OutboxKindWorkspaceRemove, Issue: "LEGION-209", NextAt: projectedAt.Add(time.Minute)}
	store := projectionStore{
		issues: []record.Issue{{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Status: "in_progress", Phase: phase.Implementing}},
		held:   []record.HeldRow{{OutboxRow: start, WaitsOn: 801}, {OutboxRow: removal, WaitsOn: 803}},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	want := []api.HeldOutboxRow{
		{ID: 802, Kind: "supervise", Issue: "LEGION-208", WaitedSeconds: 150, WaitsOn: 801},
		{ID: 804, Kind: "workspace_remove", Issue: "LEGION-209", WaitedSeconds: 0, WaitsOn: 803},
	}
	if !reflect.DeepEqual(got.HeldOutboxRows, want) {
		t.Fatalf("heldOutboxRows = %#v, want %#v", got.HeldOutboxRows, want)
	}
	none, err := Project(context.Background(), nil, projectionStore{}, "LEGION", nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project with nothing held: %v", err)
	}
	if none.HeldOutboxRows == nil || len(none.HeldOutboxRows) != 0 {
		t.Fatalf("heldOutboxRows with nothing held = %#v, want an empty list", none.HeldOutboxRows)
	}
}

// The state names every issue Legion is waiting to start behind a Dispatch issue nobody has
// finished, judged from the project's mirror in the same transaction the rest of the state is
// read in: the waiting root admission skips and the child the workflow holds at `admitted`, in
// one list sorted by issue key. A root carrying a run is never judged, whatever its links say,
// and a state with nothing blocked carries an empty list rather than none.
func TestProjectNamesWhatEveryBlockedRootAndChildWaitsOn(t *testing.T) {
	store := projectionStore{
		issues: []record.Issue{
			{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Status: "in_progress", Phase: phase.Implementing},
			{Key: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Status: "todo", Phase: phase.Admitted},
			{Key: "LEGION-212", Project: "LEGION", Tree: "LEGION-212", Status: "todo", Phase: phase.Admitted, Rank: "00012", HandedOver: true},
			// A root another tree handed over, waiting for a slot with its run already under
			// way: promotion admits it whatever its links say, so the state reports no wait for
			// it either.
			{Key: "LEGION-211", Project: "LEGION", Tree: "LEGION-211", Status: "todo", Phase: phase.Testing, Rank: "00011", HandedOver: true, HandedOverFrom: "LEGION-200"},
		},
		slots: []record.Slot{{Issue: "LEGION-208", Index: 0, AdmittedAt: projectedAt}},
		mirror: []record.DispatchIssueRow{
			{Key: "LEGION-209", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-300", "LEGION-301"}},
			{Key: "LEGION-211", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-301"}},
			{Key: "LEGION-212", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-302"}},
			{Key: "LEGION-300", Project: "LEGION", Status: "done"},
			{Key: "LEGION-301", Project: "LEGION", Status: "in_progress"},
		},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	// LEGION-302 has no row of its own, which is not a finished issue: the wait stands.
	want := []api.BlockedIssue{
		{Issue: "LEGION-209", BlockedBy: []string{"LEGION-301"}},
		{Issue: "LEGION-212", BlockedBy: []string{"LEGION-302"}},
	}
	if !reflect.DeepEqual(got.Admission.Blocked, want) {
		t.Fatalf("blocked = %#v, want %#v", got.Admission.Blocked, want)
	}
	if !reflect.DeepEqual(got.Admission.Waiting, []string{"LEGION-211", "LEGION-212"}) {
		t.Fatalf("waiting = %#v, want the roots' waiting line unchanged", got.Admission.Waiting)
	}

	none, err := Project(context.Background(), nil, projectionStore{}, "LEGION", nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project with nothing blocked: %v", err)
	}
	if none.Admission.Blocked == nil || len(none.Admission.Blocked) != 0 {
		t.Fatalf("blocked with nothing waiting on a link = %#v, want an empty list", none.Admission.Blocked)
	}
}

// lineIssue is one record of the waiting line's fixture, carrying the fields the line reads and
// the repository fields every projected issue has. It is internal/admit's own lineIssue
// (line_order_test.go) written for this package, so the two fixtures are read side by side.
func lineIssue(key, tree, status string, stands phase.Phase, rank string) record.Issue {
	return record.Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: rank, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// A4, read through the projected state rather than through record.Waiting: admission.waiting is
// the line in the line's own order, so a reader of GET /legion/v1/state — the controller, the
// Legion view — sees what admission will promote next and in what order.
//
// The fixture is the one internal/admit's TestTheWaitingLineRunsReturnsFirstThenTheBoardsRank
// proves over record.Waiting, so the two say the same thing about the same facts: the issues
// whose work came back first, oldest first on the one time axis an owed start and a gate's
// approval share, and then the issues whose first phase has never started, by the Dispatch rank
// of their tree's root. A root a person moved back to todo while its own tree runs is in no part
// of it: it holds no slot, and it is no root that never ran.
func TestProjectShowsTheWaitingLineInTheLinesOwnOrder(t *testing.T) {
	opened := projectedAt.Add(-2 * time.Hour)
	approved := 1
	store := projectionStore{
		issues: []record.Issue{
			lineIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
			lineIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"),
			lineIssue("LEGION-12", "LEGION-10", "retro", phase.Merging, "O"),
			lineIssue("LEGION-20", "LEGION-20", "todo", phase.Admitted, "A"),
			lineIssue("LEGION-30", "LEGION-30", "todo", phase.Admitted, "Z"),
			lineIssue("LEGION-50", "LEGION-50", "todo", phase.Accepting, "B"),
		},
		lifecycles: []string{"LEGION-10", "LEGION-50"},
		gates: map[string]record.DesignGate{
			"LEGION-10": {Issue: "LEGION-10", LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: opened}, EverOpened: true},
			"LEGION-50": {Issue: "LEGION-50", LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 8, At: opened}, EverOpened: true},
		},
		owed: []record.OwedStart{{Issue: "LEGION-12", ID: 42, CreatedAt: projectedAt.Add(-time.Hour)}},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	want := []string{"LEGION-10", "LEGION-12", "LEGION-20", "LEGION-11", "LEGION-30"}
	if !reflect.DeepEqual(got.Admission.Waiting, want) {
		t.Fatalf("admission.waiting = %#v, want %#v: the approved root's own first phase and the returning child first, then the board's rank",
			got.Admission.Waiting, want)
	}

	// The gate opened before the child's start came back, so the root goes first; an approval
	// that lands after it puts the root behind the child, which is the one axis the two share.
	gate := store.gates["LEGION-10"]
	gate.ApprovedMark.At = projectedAt
	store.gates["LEGION-10"] = gate
	got, err = Project(context.Background(), nil, store, "LEGION", nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project with the gate opened last: %v", err)
	}
	if want := []string{"LEGION-12", "LEGION-10", "LEGION-20", "LEGION-11", "LEGION-30"}; !reflect.DeepEqual(got.Admission.Waiting, want) {
		t.Fatalf("admission.waiting with the gate opened last = %#v, want %#v", got.Admission.Waiting, want)
	}
}
