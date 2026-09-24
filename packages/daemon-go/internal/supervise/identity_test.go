package supervise

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Every delivery first hands the agent's working copy its role's App identity, the way the shipped
// daemon adopts it before an assignment (packages/daemon/src/daemon/processes.ts:1126), so a commit
// the task makes is authored by the bot rather than by whoever the workspace last named.
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
