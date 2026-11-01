package supervise

import "testing"

// A live claim whose registration declared a plugin contract below PodUpdateContract — a worker
// started from the image of the release before this one, which is what every roll of this
// deployment leaves behind for a moment — has no ready_for_update op: the op list is a closed enum
// compiled into its plugin. An update round still asks it, and it answers by ending the turn its
// task started (answerByTurnEnd). Outside a round, the stale-plugin relaunch is what brings an Oh
// My Pi worker onto this daemon's plugin; it keeps the pod and the disk and resumes the claim's
// own session. Once it has registered at this daemon's contract it answers with the op.
//
// It is relaunched when it is not in a turn, and not before. A relaunch ends the process, and the
// stale-plugin lane asks the worker nothing first, so ending a turn in flight where it stands
// throws away whatever it had not pushed, which is the harm this issue exists to end. The wait
// costs nothing: the claim holds no round open, nothing else of its pod is waiting on it, and the
// turn ends on its own. The machine already waits out a turn this way for a held suspension and
// for a plugin round's own end (TestPodUpdateEndedInAPluginRoundRelaunchesOnceTheTurnIsOver).
//
// Observed live on 2026-10-30: after a daemon boot on a newer contract, an issue's claims came up
// on the previous image at plugin contract 42, every `legion` tool call failed, and the daemon
// went on routing work to them until an operator moved the issue by hand.
func TestAStalePluginBelowTheUpdateContractIsRelaunchedWhenItsTurnEnds(t *testing.T) {
	t.Run("idle, so it is relaunched at once", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract-2)
		h.reach(StateReady)
		h.m.deps.PluginContract = PodUpdateContract
		recorded := h.locator()

		h.must(StalePlugin{Claim: h.token, Trigger: "boot"})

		h.wantState(StateLaunching)
		h.wantPrompts(0)
		c := h.wantRound("")
		if c.PluginRelaunchFor != PodUpdateContract {
			t.Errorf("PluginRelaunchFor = %d, want this daemon's %d", c.PluginRelaunchFor, PodUpdateContract)
		}
		resume := h.wantCalls("Resume", 1)[0]
		if resume.Previous == nil || *resume.Previous != recorded {
			t.Errorf("resumed over %+v, want the recorded process %+v: the relaunch keeps the pod and its disk", resume.Previous, recorded)
		}
		if resume.Spec.ResumeSessionFile != sessionFile {
			t.Errorf("resumed session file %q, want the claim's own %q", resume.Spec.ResumeSessionFile, sessionFile)
		}
	})

	t.Run("in a turn, so it is relaunched once that turn ends", func(t *testing.T) {
		h := updateHarness(t, PodUpdateContract-2)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
		h.must(StreamTurnStart{Claim: h.token})
		h.m.deps.PluginContract = PodUpdateContract

		h.must(StalePlugin{Claim: h.token, Trigger: "boot"})

		if got := h.claim().State; got != StateWorking {
			t.Fatalf("the claim is %s while its worker runs a turn, want it left %s: ending that turn throws away whatever the worker had not pushed, and a worker below this contract cannot be asked to push first",
				got, StateWorking)
		}
		h.wantCalls("Resume", 0)
		if p := h.pending(); p.Interrupted {
			t.Errorf("the task was taken back as interrupted while its turn still runs: %+v", p)
		}

		h.must(StreamTurnEnd{Claim: h.token})

		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
		if c := h.claim(); c.PluginRelaunchFor != PodUpdateContract {
			t.Errorf("PluginRelaunchFor after the turn ended = %d, want this daemon's %d", c.PluginRelaunchFor, PodUpdateContract)
		}
	})
}

// The turn the relaunch waited for ends as any turn does, and its own finished task retires
// before the relaunch rather than being taken back and sent again: the worker did that task, and
// a relaunch that re-delivered it would have the resumed agent do it twice. The relaunch itself
// prompts the stale process nothing — it is being replaced in the same decision.
func TestAStalePluginBelowTheUpdateContractRetiresTheFinishedTaskBeforeItRelaunches(t *testing.T) {
	h := updateHarness(t, PodUpdateContract-2)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: h.token})
	sent := h.wantPrompts(1)
	h.m.deps.PluginContract = PodUpdateContract
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.must(StreamTurnEnd{Claim: h.token})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.wantPrompts(len(sent))
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending after the relaunch = %+v, want the finished turn's task retired, not taken back for the new process", p)
	}
}

// A claim whose process goes while its relaunch waits for the turn — it died, the operator
// suspended it, its tree closed — drops the hold with the process it was made for (letGo), so
// nothing relaunches a claim that has no process to replace, and the launch that brings it back
// installs this daemon's plugin anyway.
func TestAStalePluginBelowTheUpdateContractDropsTheHeldRelaunchWithItsProcess(t *testing.T) {
	h := updateHarness(t, PodUpdateContract-2)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: h.token})
	h.m.deps.PluginContract = PodUpdateContract
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	if h.m.staleRelaunch == nil {
		t.Fatal("no relaunch is held for the turn")
	}

	h.must(RequestStop{Claim: h.token})

	if h.m.staleRelaunch != nil {
		t.Errorf("the relaunch still waits for a turn of a process the claim no longer runs: %q", *h.m.staleRelaunch)
	}
	h.wantCalls("Resume", 0)
}

// A held relaunch that decides nothing at the turn's end still sends the task that waited behind
// it. refreshPlugin decides the relaunch again as the claim stands then, and by then a
// registration may have recorded this daemon's own contract — the case holdStaleRelaunch's own
// rule allows — so nothing is relaunched and nothing is launching. The queued task must go then:
// the turn's end is what sends it, no relaunch is coming to send it instead, and a claim left
// holding a task nobody sends waits for an event nothing posts.
func TestAStalePluginRelaunchThatDecidesNothingStillSendsTheWaitingTask(t *testing.T) {
	h := updateHarness(t, PodUpdateContract-2)
	h.reach(StateIdle)
	// A turn of the agent's own — an Envoy notice, a person's steer — with the next task queued
	// behind it: what holds that task back here is the turn, not a round.
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	h.m.deps.PluginContract = PodUpdateContract
	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
	if h.m.staleRelaunch == nil {
		t.Fatal("no relaunch is held for the turn")
	}
	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
	sent := len(h.prompts())
	// The claim's process registers again, at this daemon's own contract, while its turn runs, so
	// the relaunch the turn's end re-decides has nothing left to do.
	h.must(RequestRegister{
		Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: PodUpdateContract,
	})

	h.must(StreamTurnEnd{Claim: h.token})

	h.wantState(StateIdle)
	h.wantCalls("Resume", 0)
	released := h.wantPrompts(sent + 1)[sent]
	if released.Message != "review the pull request" {
		t.Errorf("the task sent after the turn = %q, want the one that waited behind the relaunch", released.Message)
	}
}
