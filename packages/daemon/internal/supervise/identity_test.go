package supervise

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Every delivery first hands the agent's working copy its role's App identity, so a commit the
// task makes is authored by the bot rather than by whoever the workspace last named.
func TestADeliveryAdoptsTheRoleIdentityBeforeThePrompt(t *testing.T) {
	bot := runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}
	for _, tc := range []struct {
		name    string
		fail    error
		prompts int
	}{
		{"an adopted working copy gets the task", nil, 1},
		{"a working copy that cannot be adopted does not", errors.New("the working copy is stale"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var asked []claim.Role
			h.deps.Identity = func(_ context.Context, role claim.Role) (runtime.GitIdentity, error) {
				asked = append(asked, role)
				return bot, nil
			}
			h.restart()
			if tc.fail != nil {
				h.rt.FailAdoptWorkingCopy(tc.fail)
			}
			h.launch()
			h.connect()
			h.register()
			h.ready()
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
			h.wantPrompts(tc.prompts)

			adoptions := h.rt.CallsOf("AdoptWorkingCopy")
			if len(adoptions) != 1 || adoptions[0].Identity != bot {
				t.Fatalf("adoptions = %+v, want one for %v", adoptions, bot)
			}
			if len(asked) != 1 {
				t.Fatalf("identity asked for roles %v, want once for the claim's role", asked)
			}
		})
	}
}

// controllerClaim is the project controller's claim, queued: the controller role, its token, and no
// issue and no tree.
func controllerClaim() Claim {
	return Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, State: StateQueued}
}

// The project's controller works no checkout and commits nothing, so a task it is handed — the
// start message each of its launches gets — reaches it with no working copy adopted and no App
// identity asked for, where a workflow role's adoption would ask for an App the controller has none
// of and keep the task from it.
func TestTheControllersTaskReachesItWithNoWorkingCopyAdopted(t *testing.T) {
	h := newHarnessOf(t, controllerClaim())
	var asked []claim.Role
	h.deps.Identity = func(_ context.Context, role claim.Role) (runtime.GitIdentity, error) {
		asked = append(asked, role)
		return runtime.GitIdentity{}, errors.New("no App for the controller role")
	}
	h.restart()
	h.launch()
	h.connect()
	h.register()
	h.ready()
	h.must(RequestDeliver{Claim: h.token, Task: "Legion controller start"})
	h.wantPrompts(1)
	if adoptions := h.rt.CallsOf("AdoptWorkingCopy"); len(adoptions) != 0 {
		t.Fatalf("adoptions = %+v, want none for the controller", adoptions)
	}
	if len(asked) != 0 {
		t.Fatalf("identity asked for roles %v, want none for the controller", asked)
	}
}
