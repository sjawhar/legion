package supervise

// A child issue closed as done releases its volume at once (workflow's leave), so its claims are
// closed rather than suspended: each ends as a tree close ends it. The session each recorded is
// kept, because a done issue is reopened — a person moves it back, a production check fails — and
// its roles are then started again on the same issue; a session dropped here would start each as a
// fresh agent that has read none of the work it is continuing (dispatch://INFRA-714 R8). The
// session is a row in the session database rather than a file on the volume the close releases, so
// it outlives both the pod and the workspace, and it is also what the Sandbox-volume reap guard
// reads as its evidence. What does go is the resume path, which named a file on that volume. The
// workspace is not recovered, and the issue's next run is a fresh spawn.

import "testing"

// The claim is released as a tree close releases it — with its process where one runs, with no
// locator where none does — and retires with the session it recorded and no resume path, in memory
// and in the store.
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
			if c := h.claim(); c.Session != session || c.SessionFile != "" || c.Locator != nil {
				t.Errorf("claim %+v after the close, want it retired with its session, no resume path and no locator", c)
			}
			if stored := h.store.load(testToken); stored.State != StateRetired || stored.Session != session || stored.SessionFile != "" {
				t.Errorf("stored %+v, want the retired claim with its session kept and no resume path", stored)
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
	if c := h.claim(); c.Session != session || c.SessionFile != "" {
		t.Fatalf("claim %+v after the retried close, want its session kept and no resume path", c)
	}
}

// A claim a tree close already retired keeps its session for a re-admission to resume, and the
// issue's close reaching it clears the resume path alone, since the volume that path named goes
// with the pod while the session it kept is the session a reopen resumes and the reap guard's
// evidence. One that records no resume path is left as it is.
func TestAnIssueCloseOfARetiredClaimKeepsItsSessionAndClearsItsResumePath(t *testing.T) {
	h := newHarness(t)
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
	if c := h.claim(); c.Session != session || c.SessionFile != "" {
		t.Errorf("claim %+v after the issue close, want its session kept and no resume path", c)
	}
	if stored := h.store.load(testToken); stored.Session != session || stored.SessionFile != "" {
		t.Errorf("stored %+v, want the session kept and no resume path", stored)
	}
	if got := h.rt.Calls()[calls:]; len(got) > 0 {
		t.Errorf("asked the runtime %v, want nothing of a retired claim", got)
	}
	if got := len(h.store.history()); got != puts+1 {
		t.Errorf("wrote the claim %d times, want once for the cleared resume path", got-puts)
	}

	h.must(RequestIssueClose{Claim: testToken})
	if got := len(h.store.history()); got != puts+1 {
		t.Errorf("a second close of the claim wrote it %d more times, want nothing to write", got-puts-1)
	}
}

// A closed claim relaunched by the workflow — the child set back to todo — is built for the
// session it kept and for no volume: the worker that continues the reopened issue is the one that
// worked it, which its session id is what carries (dispatch://INFRA-714 R8), while the resume path
// is gone with the volume it named, so the launch provisions the workspace anew rather than
// recovering one.
func TestAClaimAnIssueCloseRetiredRelaunchesOnItsSessionWithNoVolume(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestIssueClose{Claim: testToken})
	h.restart()

	h.must(RequestRetry{Claim: testToken})

	h.wantState(StateLaunching)
	// The runtime is asked to spawn rather than to resume, because a resume is of a session file
	// on the issue's own volume and that volume went with the pod. The session the claim kept
	// reaches the new process through the spec instead, so a deployment whose sessions live in a
	// database continues the same session in a workspace provisioned afresh.
	h.wantCalls("Resume", 0)
	if last := h.rt.Calls()[len(h.rt.Calls())-1]; last.Method != "Spawn" || last.Spec.ResumeSessionFile != "" {
		t.Errorf("the relaunch was %s %+v, want a spawn with no resume path", last.Method, last.Spec)
	}
	if built := h.specs.last(t); built.Session != session || built.SessionFile != "" {
		t.Errorf("the relaunch was built for %+v, want the session it kept and no resume path", built)
	}
}
