package supervise

// A child issue closed as done releases its volume at once (workflow's leave), so its claims are
// closed rather than suspended: each ends as a tree close ends it. The session each recorded is
// kept, because a done issue is reopened — a person moves it back, a production check fails — and
// its roles are then started again on the same issue; a session dropped here would start each as a
// fresh agent that has read none of the work it is continuing (dispatch://INFRA-714 R8). The
// session is a row in the session database rather than a file on the volume the close releases, so
// it outlives both the pod and the workspace; a resume that cannot find it is the one case that
// starts fresh, uncharged and reported (machine.go's start).

import "testing"

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
				t.Errorf("claim %+v after the close, want it retired with its session and no locator", c)
			}
			if stored := h.store.load(testToken); stored.State != StateRetired || stored.Session != session || stored.SessionFile != sessionFile {
				t.Errorf("stored %+v, want the retired claim with its session kept", stored)
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
// issue's close reaching it changes nothing at all: it asks the runtime nothing and writes
// nothing, since the session it would once have dropped is the session a reopen resumes.
func TestAnIssueCloseOfARetiredClaimKeepsItsSessionAndWritesNothing(t *testing.T) {
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
	if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
		t.Errorf("claim %+v after the issue close, want its session kept", c)
	}
	if stored := h.store.load(testToken); stored.Session != session || stored.SessionFile != sessionFile {
		t.Errorf("stored %+v, want the session kept", stored)
	}
	if got := h.rt.Calls()[calls:]; len(got) > 0 {
		t.Errorf("asked the runtime %v, want nothing of a retired claim", got)
	}
	if got := len(h.store.history()); got != puts {
		t.Errorf("wrote the claim %d times, want nothing to write", got-puts)
	}
}

// A closed claim relaunched by the workflow — the child set back to todo — resumes the session it
// kept, so the worker that continues the reopened issue is the one that worked it.
func TestAClaimAnIssueCloseRetiredResumesItsSession(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestIssueClose{Claim: testToken})
	h.restart()

	h.must(RequestRetry{Claim: testToken})

	h.wantState(StateLaunching)
	if last := h.rt.Calls()[len(h.rt.Calls())-1]; last.Method != "Resume" || last.Spec.ResumeSessionFile != sessionFile {
		t.Errorf("the relaunch was %s %+v, want a resume of %s", last.Method, last.Spec, sessionFile)
	}
	if built := h.specs.last(t); built.Session != session || built.SessionFile != sessionFile {
		t.Errorf("the relaunch was built for %+v, want the claim's recorded session", built)
	}
}
