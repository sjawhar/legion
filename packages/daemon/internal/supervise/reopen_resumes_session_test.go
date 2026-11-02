package supervise

// A10 of dispatch://INFRA-714/spec: "A reopened `done` child resumes each worker's old session."
// This file holds that requirement as its own test, because the rule the forward merge at
// e2236b4e settled on — the close keeps the claim's session id and clears its resume path — does
// not meet it. The claim's session id reaches no runtime as a resume handle: runtime.SpawnSpec
// carries only ResumeSessionFile, daemon's specs.SpawnSpec reads c.Session only to decide a
// Kubernetes plugin install, the Sandbox manifest reads Claude Code's resume id from the recorded
// path, and the reap guard reads the id as the volume's evidence, which is what issueClose's own
// comment says it is kept for. So with the path cleared, Machine.start takes its
// `m.claim.SessionFile == ""` branch and spawns, and the worker that continues a reopened issue
// has read none of the work it is continuing — on both runtimes, since Oh My Pi resumes by that
// path and tmux searches its folders for that same path.
//
// Before the merge (03005ece) issueClose cleared nothing, so the reopen resumed. This test fails
// at e2236b4e and passes once a reopened claim resumes the session it kept.

import "testing"

// A claim whose issue closed as done and is then started again — the child set back to todo, or a
// failed production check reopening it — continues the session it recorded rather than starting a
// fresh agent. Which runtime it is does not change the requirement: whatever the deployment's
// session store, the worker that comes back is the one that did the work.
func TestAReopenedClosedClaimResumesTheSessionItKept(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	if before := h.claim(); before.Session != session {
		t.Fatalf("claim %+v before the close, want it recording session %s", before, session)
	}

	h.must(RequestIssueClose{Claim: testToken})
	h.restart()
	if kept := h.claim(); kept.Session != session {
		t.Fatalf("claim %+v after the close, want the session kept for the reopen", kept)
	}

	h.must(RequestRetry{Claim: testToken})
	h.wantState(StateLaunching)

	calls := h.rt.Calls()
	last := calls[len(calls)-1]
	if last.Method != "Resume" {
		t.Fatalf("the reopened claim was launched with %s, want Resume: A10 says a reopened done "+
			"child resumes each worker's old session, and a Spawn starts an agent that has read "+
			"none of the work it continues", last.Method)
	}
}
