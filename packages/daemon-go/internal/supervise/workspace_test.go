package supervise

// A claim's session lives on its tree's volume. When workspace-init finds that volume lost — no
// clone and no session file, exit 3 — the session is gone, and resuming it would fail the same way
// on every attempt. That one fact is the exception to the same-agent rule: the claim relaunches as
// a fresh session, and the tree's other claims drop the sessions the same volume held.

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

const lostDetail = runtime.WorkspaceLostDetail +
	" pod legion-legion-208-architect (uid u2) Failed: init container workspace-init terminated (Error, exit code 3)"

// resumedHarness is a machine over c that has suspended its recorded session and is resuming it;
// told records every claim the machine reports its tree volume lost for.
func resumedHarness(t *testing.T, c Claim) (*harness, *[]Claim) {
	t.Helper()
	h := newHarnessOf(t, c)
	var told []Claim
	h.deps.VolumeLost = func(c Claim) { told = append(told, c) }
	h.start(h.store.load(h.token))
	h.reach(StateSuspended)
	h.must(RequestResume{Claim: h.token})
	h.wantState(StateLaunching)
	return h, &told
}

// gone is the runtime reporting the claim's current process gone, with detail.
func (h *harness) gone(detail string) {
	h.t.Helper()
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), Detail: detail}})
}

// One exit 3 relaunches the claim once, as a fresh recorded session: no session file to resume, the
// next generation, a launch built for a claim whose workspace was lost, and no launch failure
// charged — the volume ended the process, not the launch. The daemon is told, and the fresh agent's
// registration, a session the claim never recorded, is accepted and resumed from then on.
func TestAWorkspaceLostGoneRelaunchesAFreshSessionOnce(t *testing.T) {
	h, told := resumedHarness(t, rootClaim())
	generation := h.generation()

	h.gone(lostDetail)

	spawns := h.wantCalls("Spawn", 2)
	if fresh := spawns[1].Spec; fresh.ResumeSessionFile != "" || fresh.Generation != generation+1 {
		t.Errorf("relaunched %+v, want a fresh spawn at generation %d", fresh, generation+1)
	}
	h.wantCalls("Resume", 1)
	if built := h.specs.last(t); !built.WorkspaceLost || built.Session != "" || built.SessionFile != "" {
		t.Errorf("the relaunch was built for %+v, want a claim whose workspace was lost, with no session", built)
	}
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if stored := h.store.load(rootToken); !stored.WorkspaceLost || stored.Session != "" || stored.SessionFile != "" || stored.Generation != generation+1 {
		t.Errorf("stored %+v, want the lost session dropped at generation %d", stored, generation+1)
	}
	if len(*told) != 1 || (*told)[0].Token != rootToken || (*told)[0].Tree != "LEGION-208" {
		t.Errorf("told %+v, want the root's lost tree volume reported once", *told)
	}

	h.connect()
	h.must(RequestRegister{Claim: rootToken, Generation: h.generation(), Session: "ses_fresh", SessionFile: "/state/sessions/fresh.jsonl", CapabilityHash: []byte("capability")})

	if c := h.claim(); c.Session != "ses_fresh" || c.WorkspaceLost {
		t.Errorf("claim %+v after the fresh registration, want its new session recorded and the loss over", c)
	}
	if stored := h.store.load(rootToken); stored.Session != "ses_fresh" || stored.WorkspaceLost {
		t.Errorf("stored %+v, want the new session and the loss over", stored)
	}
}

// Any other end of a resumed process is an ordinary death: one launch failure, and the recorded
// session resumed again after the process that died.
func TestAGoneWithoutTheWorkspaceLostDetailStillResumesTheSession(t *testing.T) {
	h, told := resumedHarness(t, rootClaim())
	dead := h.locator()

	h.gone("pod legion-legion-208-architect (uid u2) Failed: init container workspace-init terminated (Error, exit code 1)")

	resumes := h.wantCalls("Resume", 2)
	if again := resumes[1]; again.Spec.ResumeSessionFile != sessionFile || again.Previous == nil || *again.Previous != dead {
		t.Errorf("relaunched %+v after %+v, want the recorded session after the dead process", again.Spec, again.Previous)
	}
	h.wantCalls("Spawn", 1)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	if c := h.claim(); c.Session != session || c.WorkspaceLost {
		t.Errorf("claim %+v, want the recorded session kept", c)
	}
	if len(*told) != 0 {
		t.Errorf("told %+v, want no lost volume reported", *told)
	}
}

// A claim of the tree that runs no process drops the session the lost volume held: its next launch
// — whenever it comes, across a restart too — is a fresh session, never a resume that would find the
// session missing beside a clone the root already recreated and fail until its budget ran out.
func TestAClaimWithNoProcessDropsTheSessionTheLostVolumeHeld(t *testing.T) {
	for _, tc := range []struct {
		state    ClaimState
		end      func(h *harness)
		relaunch Event
	}{
		{StateSuspended, func(h *harness) { h.reach(StateSuspended) }, RequestResume{Claim: testToken}},
		{StateRetired, func(h *harness) {
			h.reach(StateIdle)
			h.must(RequestStop{Claim: testToken, TreeClose: true})
		}, RequestRetry{Claim: testToken}},
		{StateFailed, func(h *harness) {
			h.reach(StateReady)
			for range 3 {
				h.observe(runtime.Gone)
			}
		}, RequestRetry{Claim: testToken}},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			h := newHarness(t)
			tc.end(h)
			h.wantState(tc.state)

			h.must(TreeVolumeLost{Claim: testToken})

			if c := h.claim(); c.Session != "" || c.SessionFile != "" || !c.WorkspaceLost {
				t.Errorf("claim %+v, want its session dropped and its workspace lost", c)
			}
			h.restart()
			h.must(tc.relaunch)
			h.wantCalls("Resume", map[ClaimState]int{StateSuspended: 0, StateRetired: 0, StateFailed: 2}[tc.state])
			if built := h.specs.last(t); !built.WorkspaceLost || built.SessionFile != "" {
				t.Errorf("the relaunch was built for %+v, want a fresh session recreating the workspace", built)
			}
			if last := h.rt.Calls()[len(h.rt.Calls())-1]; last.Method != "Spawn" || last.Spec.ResumeSessionFile != "" {
				t.Errorf("the relaunch was %s %+v, want a fresh spawn", last.Method, last.Spec)
			}
		})
	}
}

// A claim resuming its session when the tree's volume is found lost drops it too: its process will
// not find the session, and the relaunch after it is fresh.
func TestABootingResumeDropsTheSessionTheLostVolumeHeld(t *testing.T) {
	h := newHarness(t)
	h.reach(StateSuspended)
	h.must(RequestResume{Claim: testToken})

	h.must(TreeVolumeLost{Claim: testToken})
	h.gone("pod legion-legion-209-implementer (uid u2) Failed: init container workspace-init terminated (Error, exit code 1)")

	if last := h.rt.Calls()[len(h.rt.Calls())-1]; last.Method != "Spawn" || last.Spec.ResumeSessionFile != "" {
		t.Errorf("the relaunch was %s %+v, want a fresh spawn", last.Method, last.Spec)
	}
	if built := h.specs.last(t); !built.WorkspaceLost {
		t.Errorf("the relaunch was built for %+v, want a claim whose workspace was lost", built)
	}
}

// An agent that registered runs on the volume its session is on, so a report of a lost volume
// changes nothing about it.
func TestAClaimWhoseAgentRegisteredKeepsItsSession(t *testing.T) {
	for _, state := range []ClaimState{StateRegistered, StateReady, StateWorking, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.reach(state)

			h.must(TreeVolumeLost{Claim: testToken})

			if c := h.claim(); c.Session != session || c.SessionFile != sessionFile || c.WorkspaceLost {
				t.Errorf("claim %+v, want its session kept", c)
			}
			h.wantState(state)
		})
	}
}
