package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

type projectionStore struct {
	record.Store
	issues       []record.Issue
	phases       map[string][]record.PhaseRow
	slots        []record.Slot
	pullRequests map[string]*record.PullRequest
}

func (s projectionStore) Issues(context.Context, pgx.Tx) ([]record.Issue, error) {
	return s.issues, nil
}
func (s projectionStore) Slots(context.Context, pgx.Tx) ([]record.Slot, error) { return s.slots, nil }
func (projectionStore) PendingStatusWrites(context.Context, pgx.Tx, string) ([]record.OutboxRow, error) {
	return nil, nil
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

func TestProjectShowsOnlySlotlessTodoIssuesInDispatchRankOrder(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	store := projectionStore{
		issues: []record.Issue{
			{Key: "LEGION-210", Tree: "LEGION-210", Status: "done", Rank: "00000"},
			{Key: "LEGION-211", Tree: "LEGION-211", Status: "backlog", Rank: "00001"},
			{Key: "LEGION-212", Tree: "LEGION-212", Status: "todo", Rank: "00004", LastDispatchSeq: 1},
			{Key: "LEGION-213", Tree: "LEGION-213", Status: "todo", Rank: "00003", LastDispatchSeq: 10, Phase: phase.Implementing},
			{Key: "LEGION-214", Tree: "LEGION-214", Status: "todo", Rank: "00002"},
		},
		slots: []record.Slot{{Issue: "LEGION-214", Index: 0, AdmittedAt: now}},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{})
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

func TestProjectShowsOperatorSpawnedClaimsWithoutRecordIssue(t *testing.T) {
	const issue = "S2-1"
	architect := claim.Token("legion-s2-1-architect")
	implementer := claim.Token("legion-s2-1-implementer")
	got, err := Project(context.Background(), nil, projectionStore{}, "LEGION", []supervise.Claim{
		{Token: architect, Issue: issue, Role: claim.RoleArchitect, State: supervise.StateReady, Session: "session-1"},
		{Token: implementer, Issue: issue, Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "session-2"},
	})
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

func TestProjectShowsLaunchUncertainClaimsWithoutALocator(t *testing.T) {
	token := claim.Token("legion-208-architect")
	store := projectionStore{
		issues: []record.Issue{{Key: "LEGION-208", Tree: "LEGION-208", Status: "todo", Rank: "00042U"}},
		phases: map[string][]record.PhaseRow{
			"LEGION-208": {{Issue: "LEGION-208", Role: claim.RoleArchitect, Claim: token}},
		},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{{
		Token: token, Issue: "LEGION-208", Role: claim.RoleArchitect, State: supervise.StateLaunchUncertain,
	}})
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

// The pull request's review decision on the state route is the latest review round's, the one the
// workflow ends the round on when the reviewer completes - whether or not the reviewer's pane is
// running - and no decision shows before a review in a round decides.
func TestProjectShowsTheReviewRoundsDecisionOnThePullRequest(t *testing.T) {
	store := projectionStore{
		issues: []record.Issue{
			{Key: "LEGION-208", Tree: "LEGION-208", Status: "needs_review", Phase: phase.Reviewing},
			{Key: "LEGION-209", Tree: "LEGION-209", Status: "needs_review", Phase: phase.Reviewing},
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
	got, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{})
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
