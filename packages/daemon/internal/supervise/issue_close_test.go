package supervise

// A child issue closed as done releases its volume at once (workflow's leave), so its claims are
// closed rather than suspended: each ends as a tree close ends it. The session each recorded is
// kept whole — the id and the resume path both — because a done issue is reopened, a person moving
// it back or a production check failing, and its roles are then started again on the same issue; a
// session dropped here would start each as a fresh agent that has read none of the work it is
// continuing (dispatch://INFRA-714 R8, A10). The id is what the Sandbox-volume reap guard reads as
// its evidence, and the path is the only handle either runtime resumes by, so dropping the path
// would drop the resume whatever the id said.
//
// A store that has since lost the session is not the close's to guess at: a kept path costs one
// resume, which the runtime answers runtime.ErrSessionFileMissing, and the same try then forgets
// the session, spawns a fresh agent charged nothing, and reports the loss to the issue, its
// architect and the controller (Machine.startFresh). Clearing the path at the close would reach
// that same fresh agent silently, which is what R8 forbids. The workspace is not recovered either
// way: the issue's next run provisions a disk of its own and resumes the agent's session on it.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// closingHarness is a harness over the queued claim, which is where every case here starts: the
// claim has a session recorded and no process yet.
func closingHarness(t *testing.T) *harness {
	t.Helper()
	h := newBareHarness(t)
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

// The claim is released as a tree close releases it — with its process where one runs, with no
// locator where none does — and retires with the session it recorded, in memory and in the store.
func TestAnIssueCloseRetiresTheClaimAndKeepsItsSessionForAReopen(t *testing.T) {
	for _, state := range []ClaimState{StateRegistered, StateReady, StateWorking, StateIdle, StateSuspended} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)
			before := h.claim()
			if before.Session != session || before.SessionFile != sessionFile {
				t.Fatalf("claim %+v before the close, want its session recorded", before)
			}

			h.must(RequestIssueClose{Claim: testToken})

			released := h.wantCalls("Release", 1)[0].Released
			switch {
			case released.Claim != testToken:
				t.Errorf("released %+v, want %s", released, testToken)
			case before.Locator == nil && released.Locator != nil:
				t.Errorf("released %+v, want no locator for a claim with no process", released)
			case before.Locator != nil && (released.Locator == nil || *released.Locator != *before.Locator):
				t.Errorf("released %+v, want the process %+v", released, before.Locator)
			}
			h.wantState(StateRetired)
			if c := h.claim(); c.Session != session || c.SessionFile != sessionFile || c.Locator != nil {
				t.Errorf("claim %+v after the close, want it retired with its session, its resume path and no locator", c)
			}
			if stored := h.store.load(testToken); stored.State != StateRetired || stored.Session != session || stored.SessionFile != sessionFile {
				t.Errorf("stored %+v, want the retired claim with its session and resume path kept", stored)
			}
		})
	}
}

// A release the runtime refuses changes nothing — the claim keeps its state and its session — so
// the close can be asked again, as a tree close can.
func TestAnIssueCloseWhoseReleaseFailsKeepsTheClaimForTheRetry(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.rt.FailRelease(errBoom)

	if err := h.handle(RequestIssueClose{Claim: testToken}); err == nil {
		t.Fatal("the close succeeded, want the runtime's refusal")
	}
	h.wantState(StateIdle)
	if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
		t.Fatalf("claim %+v after the refused release, want its session kept", c)
	}

	h.rt.FailRelease(nil)
	h.must(RequestIssueClose{Claim: testToken})
	h.wantState(StateRetired)
	if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
		t.Fatalf("claim %+v after the retried close, want its session kept", c)
	}
}

// A claim a tree close already retired keeps its session for a re-admission to resume, and the
// issue's close reaching it has nothing left to do: it asks the runtime nothing and writes
// nothing, since the session it would once have dropped is the session a reopen resumes. A second
// close finds the same.
func TestAnIssueCloseOfARetiredClaimKeepsItsSessionAndWritesNothing(t *testing.T) {
	h := closingHarness(t)
	h.reach(StateIdle)
	h.must(RequestTreeClose{Claim: testToken})
	h.wantState(StateRetired)
	if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
		t.Fatalf("claim %+v after the tree close, want its session kept", c)
	}
	calls := len(h.rt.Calls())
	puts := len(h.store.history())

	h.must(RequestIssueClose{Claim: testToken})

	h.wantState(StateRetired)
	if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
		t.Errorf("claim %+v after the issue close, want its session kept whole", c)
	}
	if stored := h.store.load(testToken); stored.Session != session || stored.SessionFile != sessionFile {
		t.Errorf("stored %+v, want the session kept whole", stored)
	}
	if got := h.rt.Calls()[calls:]; len(got) > 0 {
		t.Errorf("asked the runtime %v, want nothing of a retired claim", got)
	}
	if got := len(h.store.history()) - puts; got != 0 {
		t.Errorf("wrote the claim %d times, want nothing to write", got)
	}

	h.must(RequestIssueClose{Claim: testToken})
	if got := len(h.store.history()) - puts; got != 0 {
		t.Errorf("a second close of the claim wrote it %d times, want nothing to write", got)
	}
}

// A closed claim the workflow starts again — the child set back to todo, a production check
// reopening it — comes back as the worker that did the work, not as a fresh agent that has read
// none of it (dispatch://INFRA-714 A10). The resume path the close kept is what decides it: the
// session id reaches no runtime as a resume handle, so a close that cleared the path would spawn
// however carefully the id was kept.
func TestAReopenedClaimResumesTheSessionItKept(t *testing.T) {
	h := closingHarness(t)
	h.reach(StateIdle)
	h.must(RequestIssueClose{Claim: testToken})
	h.restart()

	h.must(RequestRetry{Claim: testToken})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	if last := h.rt.Calls()[len(h.rt.Calls())-1]; last.Method != "Resume" || last.Spec.ResumeSessionFile != sessionFile {
		t.Errorf("the relaunch was %s %+v, want a resume of %s", last.Method, last.Spec, sessionFile)
	}
	if built := h.specs.last(t); built.Session != session || built.SessionFile != sessionFile {
		t.Errorf("the relaunch was built for %+v, want the session it kept whole", built)
	}
}

// The other half of keeping the path: where the store really has lost the session — a deployment
// that wrote it to the issue pod's own disk, which the close deletes with the pod — the reopen
// still asks for the resume, and the answer is what reports the loss. The claim starts a fresh
// agent in the same try, charged nothing, with the issue, its architect and the controller told
// and the agent itself told in its own prompt (dispatch://INFRA-714 R8). That is the whole reason
// the close keeps a path it cannot be sure of: clearing it would reach this same fresh agent in
// silence, and nothing would ever say that work had been left behind.
func TestAReopenedClaimWhoseSessionTheStoreLostStartsFreshAndReportsIt(t *testing.T) {
	var reported []string
	h := newBareHarness(t)
	// The hook is set before the machine is built, since the machine takes the deps it is given.
	h.deps.SessionLost = func(_ Claim, detail string) { reported = append(reported, detail) }
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateIdle)
	h.must(RequestIssueClose{Claim: testToken})
	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: the issue's volume went with its pod", testToken, runtime.ErrSessionFileMissing)})

	h.must(RequestRetry{Claim: testToken})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	spawn := h.wantCalls("Spawn", 2)[1]
	if spawn.Spec.ResumeSessionFile != "" {
		t.Errorf("the fresh start is %+v, want it to resume nothing", spawn.Spec)
	}
	if c := h.claim(); c.Session != "" || c.SessionFile != "" {
		t.Errorf("claim %+v after the lost session, want it recording none", c)
	}
	if built := h.specs.last(t); !built.SessionLost {
		t.Error("the fresh agent's spec was built for a claim that does not know its session was lost")
	}
	if len(reported) != 1 || !strings.Contains(reported[0], session) {
		t.Fatalf("the loss was reported %v, want it told once and naming the session", reported)
	}
	h.wantBudgets(Budgets{})
}
