package supervise

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A phase task first hands the agent's working copy its role's App identity for the claim's
// issue, so a commit the task makes is authored by the bot rather than by whoever the workspace
// last named.
func TestAPhaseTaskAdoptsTheRoleIdentityBeforeThePrompt(t *testing.T) {
	bot := runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}
	for _, tc := range []struct {
		name      string
		fail      error
		prompts   int
		adoptions int
	}{
		{"an adopted working copy gets the task", nil, 1, 1},
		{"a working copy that cannot be adopted does not", errors.New("the working copy is stale"), 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var asked []string
			h.deps.Identity = func(_ context.Context, role claim.Role, issue string) (runtime.GitIdentity, error) {
				asked = append(asked, string(role)+" "+issue)
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
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 5})
			h.wantPrompts(tc.prompts)

			adoptions := h.rt.CallsOf("AdoptWorkingCopy")
			if len(adoptions) != tc.adoptions || tc.adoptions == 1 && adoptions[0].Identity != bot {
				t.Fatalf("adoptions = %+v, want %d for %v", adoptions, tc.adoptions, bot)
			}
			if want := string(h.claim().Role) + " " + h.claim().Issue; len(asked) != 1 || asked[0] != want {
				t.Fatalf("identity asked for %v, want once for %q, the claim's role and issue", asked, want)
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
	h.deps.Identity = func(_ context.Context, role claim.Role, _ string) (runtime.GitIdentity, error) {
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

// testerToken and testerClaim are the tester of the implementer's own issue (queuedClaim): two
// roles of one issue, and so of the one jj workspace its roles share.
const testerToken claim.Token = "legion-LEGION-209-tester"

func testerClaim() Claim {
	return Claim{
		Token:   testerToken,
		Project: "legion",
		Tree:    "LEGION-208",
		Issue:   "LEGION-209",
		Role:    claim.RoleTester,
		State:   StateQueued,
	}
}

// The roles of an issue work one jj workspace, so the adoption a phase task makes of its working
// copy is not the adopting claim's own act: run while another role is in a turn, it moves that
// role's worker onto a fresh commit mid-write, and the work it goes on to do lands authored by
// this role (dispatch://INFRA-714). The tester's task therefore waits while the implementer's
// turn is open, and is adopted and sent when that turn ends — once, whatever arrives after it.
func TestATesterPhaseTaskWaitsForTheImplementersTurnToEnd(t *testing.T) {
	bot := runtime.GitIdentity{Name: "legion-tester[bot]", Email: "7+legion-tester[bot]@users.noreply.github.com"}
	for _, tc := range []struct {
		name string
		send func(h *harness)
	}{
		// The daemon posts this the moment the implementer's own machine has recorded its turn's
		// end (supervisor's wakeIssue), so the task goes as that turn ends rather than a sweep
		// later.
		{name: "the wake the implementer's turn end posts", send: func(h *harness) { h.must(IssueTurnEnded{Claim: testerToken}) }},
		// Nothing of the wait is kept anywhere, so a wake that never comes strands nothing: the
		// implementer's process died in its turn, its claim was suspended, its issue closed —
		// none of those ends a turn on the stream, and all of them leave it not working. The
		// sweep's next observation asks the same question and sends the task.
		{name: "the sweep, where no wake ever comes", send: func(h *harness) { h.observe(runtime.Alive) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, testerClaim())
			h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) { return bot, nil }
			implementing := true
			h.deps.IssueInTurn = func(issue string, except claim.Token) (claim.Token, bool) {
				if issue != testerClaim().Issue || except != testerToken || !implementing {
					return "", false
				}
				return testToken, true
			}
			h.restart()
			h.reach(StateReady)

			h.must(RequestDeliver{Claim: testerToken, Task: "test the branch", Phase: phase.Testing, Generation: 5})

			h.wantPrompts(0)
			h.wantCalls("AdoptWorkingCopy", 0)
			if p := h.pending(); p.Prompted || !p.ConfirmedAt.IsZero() {
				t.Fatalf("pending %+v, want the task held, unprompted, while the implementer's turn is open", p)
			}

			implementing = false
			tc.send(h)

			if sent := h.wantPrompts(1)[0]; sent.Message != "test the branch" {
				t.Fatalf("sent %+v, want the tester's task once the implementer's turn ended", sent)
			}
			if adoptions := h.wantCalls("AdoptWorkingCopy", 1); adoptions[0].Identity != bot {
				t.Fatalf("adoptions = %+v, want one for %v", adoptions, bot)
			}

			// A second wake, and the sweep behind it, find the task on its way to a turn: it is
			// neither adopted nor prompted again.
			h.must(IssueTurnEnded{Claim: testerToken})
			h.observe(runtime.Alive)
			h.wantPrompts(1)
			h.wantCalls("AdoptWorkingCopy", 1)
		})
	}
}

// A delivery that is no phase task adopts nothing: a message another role sent, a notice, a
// reminder, an operator's own task. The role is not about to commit for a phase, and the working
// copy is the issue's rather than this claim's, so nothing is asked of the App and nothing is
// taken from the role working in that copy — which is also why such a prompt waits for no turn of
// another role: it has no adoption to hold back.
func TestADeliveryThatIsNoPhaseTaskAdoptsNothingAndWaitsForNoTurn(t *testing.T) {
	h := newHarness(t)
	var asked []claim.Role
	h.deps.Identity = func(_ context.Context, role claim.Role, _ string) (runtime.GitIdentity, error) {
		asked = append(asked, role)
		return runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}, nil
	}
	reads := 0
	h.deps.IssueInTurn = func(string, claim.Token) (claim.Token, bool) {
		reads++
		return testerToken, true
	}
	h.restart()
	h.reach(StateReady)

	h.must(RequestDeliver{Claim: testToken, Task: "the tester asks how the branch builds"})

	if sent := h.wantPrompts(1)[0]; sent.Message != "the tester asks how the branch builds" {
		t.Fatalf("sent %+v, want the message, which waits for no turn", sent)
	}
	h.wantCalls("AdoptWorkingCopy", 0)
	if len(asked) != 0 {
		t.Fatalf("identity asked for roles %v, want none for a delivery that is no phase task", asked)
	}
	if reads != 0 {
		t.Fatalf("the issue's turns were read %d times, want none: a delivery that adopts nothing is held by no turn", reads)
	}
}
