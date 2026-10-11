package supervise

// A child issue closed as done releases its volume at once (workflow's leave), so its claims are
// closed rather than suspended: each ends as a tree close ends it, and the session it recorded —
// which lived on the volume the close releases — goes with it. The workspace is not lost: nothing is
// recovered, and the issue's next run, re-entered by a person's todo, is a fresh spawn.

import "testing"

// The claim is released as a tree close releases it — with its process where one runs, with no
// locator where none does — and retires with no session and no recorded loss, in memory and in the
// store, so IssueHasSessions of its issue reads nothing from it.
func TestAnIssueCloseRetiresTheClaimAndDropsItsSessionWithoutALostWorkspace(t *testing.T) {
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
			if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.WorkspaceLost || c.Locator != nil {
				t.Errorf("claim %+v after the close, want it retired with no session, no loss and no locator", c)
			}
			if stored := h.store.load(testToken); stored.State != StateRetired || stored.Session != "" || stored.SessionFile != "" || stored.WorkspaceLost {
				t.Errorf("stored %+v, want the retired claim with its session dropped and no loss", stored)
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
	if c := h.claim(); c.Session != "" || c.SessionFile != "" {
		t.Fatalf("claim %+v after the retried close, want its session dropped", c)
	}
}

// A claim a tree close already retired keeps its session for a re-admission to resume; the issue's
// close reaching it drops that session, since its volume goes, and asks nothing of the runtime. One
// that records no session is left as it is.
func TestAnIssueCloseOfARetiredClaimDropsTheSessionItKept(t *testing.T) {
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
	if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.WorkspaceLost {
		t.Errorf("claim %+v after the issue close, want its session dropped and no loss", c)
	}
	if stored := h.store.load(testToken); stored.Session != "" || stored.SessionFile != "" {
		t.Errorf("stored %+v, want the session dropped", stored)
	}
	if got := h.rt.Calls()[calls:]; len(got) > 0 {
		t.Errorf("asked the runtime %v, want nothing of a retired claim", got)
	}
	if got := len(h.store.history()); got != puts+1 {
		t.Errorf("wrote the claim %d times, want once for the dropped session", got-puts)
	}

	h.must(RequestIssueClose{Claim: testToken})
	if got := len(h.store.history()); got != puts+1 {
		t.Errorf("a second close of the claim wrote it %d more times, want nothing to write", got-puts-1)
	}
}

// A closed claim relaunched by the workflow — the child set back to todo — is a fresh spawn
// built for a claim with no session and no lost workspace: it provisions anew, expecting no volume,
// rather than resuming a session the release deleted or recovering a workspace nothing lost.
func TestAClaimAnIssueCloseRetiredRelaunchesFresh(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestIssueClose{Claim: testToken})
	h.restart()

	h.must(RequestRetry{Claim: testToken})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 0)
	if last := h.rt.Calls()[len(h.rt.Calls())-1]; last.Method != "Spawn" || last.Spec.ResumeSessionFile != "" {
		t.Errorf("the relaunch was %s %+v, want a fresh spawn", last.Method, last.Spec)
	}
	if built := h.specs.last(t); built.WorkspaceLost || built.Session != "" || built.SessionFile != "" {
		t.Errorf("the relaunch was built for %+v, want a claim with no session and no lost workspace", built)
	}
}
