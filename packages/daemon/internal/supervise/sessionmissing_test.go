package supervise

// A runtime that reads a claim's session file where the agent runs and finds none there
// (runtime.ErrSessionFileMissing: a session recorded in another runtime's pod, or on a volume that
// is gone) answers the same on every try. Rather than spend the launch budget on it and fail the
// claim, the same try spawns a fresh agent, which registers a session of its own.

import (
	"fmt"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A resume whose session file is missing is followed, in the same try, by a fresh spawn at the same
// generation: no session file to resume, the recorded session forgotten in memory and in the store,
// nothing charged. The fresh agent's registration, a session the claim never recorded, is accepted.
func TestAResumeOfAMissingSessionFileSpawnsAFreshSessionUncharged(t *testing.T) {
	h := newHarness(t)
	h.reach(StateSuspended)
	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: no OMP session at %s", testToken, runtime.ErrSessionFileMissing, sessionFile)})

	h.must(RequestResume{Claim: testToken})

	resumes := h.wantCalls("Resume", 1)
	if resumes[0].Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %+v, want the recorded session %s asked for first", resumes[0].Spec, sessionFile)
	}
	spawns := h.wantCalls("Spawn", 2)
	if fresh := spawns[1].Spec; fresh.ResumeSessionFile != "" || fresh.Generation != resumes[0].Spec.Generation || fresh.BootToken != resumes[0].Spec.BootToken {
		t.Errorf("spawned %+v, want a fresh spawn in the resume's own try (generation %d)", fresh, resumes[0].Spec.Generation)
	}
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if c := h.claim(); c.Session != "" || c.SessionFile != "" || c.Locator == nil {
		t.Errorf("claim %+v, want the missing session forgotten and the fresh process recorded", c)
	}
	if stored := h.store.load(testToken); stored.Session != "" || stored.SessionFile != "" || stored.Generation != h.generation() {
		t.Errorf("stored %+v, want the missing session dropped at generation %d", stored, h.generation())
	}

	h.connect()
	h.must(RequestRegister{Claim: testToken, Generation: h.generation(), Session: "ses_fresh", SessionFile: "/state/sessions/fresh.jsonl",
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract})

	if c := h.claim(); c.Session != "ses_fresh" || c.SessionFile != "/state/sessions/fresh.jsonl" {
		t.Errorf("claim %+v after the fresh registration, want its new session recorded", c)
	}
}

// Any other refusal of a resume is a launch failure as before, charged, and the recorded session
// kept for the next try.
func TestAResumeRefusedForAnotherReasonKeepsTheSession(t *testing.T) {
	h := newHarness(t)
	h.reach(StateSuspended)
	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: tmux new-window failed", testToken)})

	h.must(RequestResume{Claim: testToken})

	h.wantCalls("Spawn", 1)
	if c := h.claim(); c.Session != session || c.SessionFile != sessionFile || c.Budgets.LaunchFailures == 0 {
		t.Errorf("claim %+v, want its session kept and the refusal charged", c)
	}
}
