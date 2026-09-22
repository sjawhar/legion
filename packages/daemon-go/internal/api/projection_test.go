package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func paneOf(token claim.Token, pane string) *runtime.Locator {
	return &runtime.Locator{
		Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "4242:77",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: pane},
	}
}

// Stage 2 has claims and no issue record, so an issue is in the state because a claim is on it:
// the architect's claim is the issue's architect, every other role's claim is that role's worker,
// and the locator is the runtime's own nested shape.
func TestClaimsProjectIntoTheIssuesTheyAreOn(t *testing.T) {
	architect := claim.Token("legion-legion-legion-208-architect")
	planner := claim.Token("legion-legion-legion-208-planner")
	implementer := claim.Token("legion-legion-legion-209-implementer")

	issues, err := ProjectClaims([]supervise.Claim{
		{Token: architect, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect, Generation: 4,
			Session: "ses_architect", State: supervise.StateReady, Locator: paneOf(architect, "%1")},
		{Token: planner, Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RolePlanner, Generation: 1,
			Session: "ses_planner", State: supervise.StateWorking, Locator: paneOf(planner, "%2")},
		{Token: implementer, Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, Generation: 2,
			Session: "ses_implementer", State: supervise.StateSuspended},
	})
	if err != nil {
		t.Fatalf("ProjectClaims: %v", err)
	}

	want := map[string]Issue{
		"LEGION-208": {
			Key: "LEGION-208", Phase: PhaseAdmitted,
			Architect: &ClaimView{Session: "ses_architect", State: "ready", Locator: paneOf(architect, "%1")},
			Workers: map[claim.Role]PhaseView{
				claim.RolePlanner: {Claim: ClaimView{Session: "ses_planner", State: "working", Locator: paneOf(planner, "%2")}},
			},
		},
		"LEGION-209": {
			Key: "LEGION-209", Phase: PhaseAdmitted,
			Workers: map[claim.Role]PhaseView{
				claim.RoleImplementer: {Claim: ClaimView{Session: "ses_implementer", State: "suspended"}},
			},
		},
	}
	if !reflect.DeepEqual(issues, want) {
		got, _ := json.Marshal(issues)
		wanted, _ := json.Marshal(want)
		t.Fatalf("projected\n got %s\nwant %s", got, wanted)
	}

	encoded, err := json.Marshal(issues["LEGION-208"].Architect)
	if err != nil {
		t.Fatalf("marshal the architect's view: %v", err)
	}
	if !strings.Contains(string(encoded), `"locator":{"runtime":"tmux","claim":"legion-legion-legion-208-architect","incarnation":"4242:77","tmux":{"window":"@1","pane":"%1"}}`) {
		t.Fatalf("the view's locator is not the runtime's nested shape: %s", encoded)
	}
}

// A locator is validated wherever it is read back, the projection included: a record nothing
// could act on is named rather than shown as a process the daemon believes in.
func TestAClaimWhoseLocatorDoesNotValidateIsNotProjected(t *testing.T) {
	broken := paneOf("legion-legion-legion-208-architect", "")
	_, err := ProjectClaims([]supervise.Claim{{
		Token: "legion-legion-legion-208-architect", Issue: "LEGION-208", Role: claim.RoleArchitect,
		State: supervise.StateReady, Locator: broken,
	}})
	if err == nil || !strings.Contains(err.Error(), "legion-legion-legion-208-architect") {
		t.Fatalf("ProjectClaims = %v, want a refusal naming the claim", err)
	}
}
