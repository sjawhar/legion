package supervise

// A runtime the operator moved a project onto, taking over the claims another runtime recorded
// (the tmux runtime's `runtime.tmux.take_over: kubernetes`), may find a taken-over claim's session
// file nowhere it runs the agent: the session stayed with the other runtime. It answers the resume
// with runtime.ErrSessionFileMissing, the same on every try, so rather than spend the launch budget
// on it and fail the claim, the same try spawns a fresh agent, which registers a session of its own.
// Every other refusal of a resume — a session file missing anywhere else among them — is a charged
// launch failure, and the claim keeps the session it recorded: a fresh agent on a claim that had
// one is what the same-agent refusal exists to catch.

import (
	"fmt"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// podLocator is a Sandbox role process's locator for the test claim: what a daemon that ran the
// project under kubernetes left in the store.
var podLocator = runtime.Locator{
	Runtime: runtime.RuntimeSandbox, Claim: testToken, Incarnation: runtime.SandboxIncarnation("pod-uid-9", 1),
	Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: "pod-uid-9", Container: "roles", Generation: 1},
}

// A taken-over claim — its process another runtime's, reported displaced — whose resume the runtime
// answers with runtime.ErrSessionFileMissing is followed, in the same try, by a fresh spawn at the
// same generation: no session file to resume, the recorded session forgotten in memory and in the
// store, nothing charged. The fresh agent's registration, a session the claim never recorded, is
// accepted.
func TestATakenOverResumeOfASessionFoundNowhereSpawnsAFreshSessionUncharged(t *testing.T) {
	h := newHarness(t)
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: podLocator})
	h.reach(StateReady)
	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: no OMP session at %s", testToken, runtime.ErrSessionFileMissing, sessionFile)})

	h.observeDisplaced(runtime.Gone)

	resumes := h.wantCalls("Resume", 1)
	if resumes[0].Spec.ResumeSessionFile != sessionFile || resumes[0].Previous == nil || *resumes[0].Previous != podLocator {
		t.Fatalf("resumed %+v over %+v, want the recorded session asked for first, over the other runtime's process", resumes[0].Spec, resumes[0].Previous)
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
// kept for the next try: the refusal to start fresh over a session file missing where the claim's
// own runtime recorded it (no takeover), and a refusal for another reason alike. Nothing is
// spawned in the session's place.
func TestAResumeRefusedForAnyOtherReasonIsChargedAndKeepsTheSession(t *testing.T) {
	for name, refusal := range map[string]error{
		"a session file missing without a takeover": fmt.Errorf("Refusing to start LEGION-209 fresh while respawning: recorded OMP session file is missing: %s, and no other folder of /state/sessions holds that session", sessionFile),
		"another reason": fmt.Errorf("resume %s: tmux new-window failed", testToken),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateSuspended)
			h.rt.ScriptResume(fake.SpawnResult{Err: refusal})

			h.must(RequestResume{Claim: testToken})

			h.wantCalls("Spawn", 1)
			if c := h.claim(); c.Session != session || c.SessionFile != sessionFile || c.Budgets.LaunchFailures == 0 {
				t.Errorf("claim %+v, want its session kept and the refusal charged", c)
			}
			if stored := h.store.load(testToken); stored.Session != session || stored.SessionFile != sessionFile {
				t.Errorf("stored %+v, want the recorded session kept", stored)
			}
		})
	}
}
