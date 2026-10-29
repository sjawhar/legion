package supervise

// An update round is the daemon asking a claim's worker to push its work and answer
// ready_for_update before the process holding that claim is replaced. These tests pin what the
// machine does with the round's two events: a round that opens once and asks once, a phase task
// the round never displaces, a suspended claim revived for the task, a claim asked again after
// PodUpdateRetask, a claim whose plugin cannot answer and is never asked, the next task held back
// from the moment the worker answers, and the round's end — which releases that task for a pod's
// round and relaunches the process for a claim's own plugin round. The stale-plugin lane's two
// cases are here too: a claim that can answer gets a round, and one that cannot, like a
// registration, keeps today's immediate relaunch.
//
// Each test drives the machine through Handle, as the daemon's sweep and the API do; no action is
// called directly.

import (
	"strconv"
	"testing"
	"time"
)

const (
	// testPodUpdateRetask is the harness's re-task bound, far longer than any wait a test's claim
	// is already in, so only a test that moves the clock itself reaches it.
	testPodUpdateRetask = time.Hour
	// testRound is a pod round's identity, as the daemon's sweep writes it.
	testRound = "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22"
	// testReasons is what the daemon found, as Claim.UpdateReasons holds it.
	testReasons = "image"
)

// updateHarness is a machine whose daemon speaks contract. The register() walk records that same
// number as the claim's own plugin contract, so contract decides whether this claim can answer an
// update round: PodUpdateContract and above can, anything below cannot.
func updateHarness(t *testing.T, contract int) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.PluginContract = contract
	h.deps.PodUpdateRetask = testPodUpdateRetask
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

// openUpdate is the daemon's sweep posting the round over this claim, as it does at every tick
// while the round is open.
func (h *harness) openUpdate(round, reasons string) {
	h.t.Helper()
	h.must(PodUpdateOpen{Claim: h.token, Round: round, Reasons: reasons, Task: PodUpdateTask(reasons)})
}

// signalUpdate is the worker answering ready_for_update through the API.
func (h *harness) signalUpdate(head string) {
	h.t.Helper()
	h.must(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Head: head})
}

func (h *harness) wantRound(want string) Claim {
	h.t.Helper()
	c := h.claim()
	if c.UpdateRound != want {
		h.t.Fatalf("UpdateRound = %q, want %q", c.UpdateRound, want)
	}
	return c
}

// A round opened over a ready claim records itself and sends the round's task once, under an id
// the round is the whole of, so the sweep's next post is recognised rather than sent again.
func TestPodUpdateOpensARoundAndSendsItsTask(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)

	h.openUpdate(testRound, testReasons)

	c := h.wantRound(testRound)
	if c.UpdateReasons != testReasons {
		t.Errorf("UpdateReasons = %q, want %q", c.UpdateReasons, testReasons)
	}
	if c.UpdateOpenedAt == nil || !c.UpdateOpenedAt.Equal(epoch) {
		t.Errorf("UpdateOpenedAt = %v, want the round's own clock at %v", c.UpdateOpenedAt, epoch)
	}
	if c.UpdateTaskedAt == nil || !c.UpdateTaskedAt.Equal(epoch) {
		t.Errorf("UpdateTaskedAt = %v, want the send's clock at %v", c.UpdateTaskedAt, epoch)
	}
	if c.UpdateSignalledAt != nil {
		t.Errorf("UpdateSignalledAt = %v, want nil: the worker has not answered", c.UpdateSignalledAt)
	}
	prompt := h.wantPrompts(1)[0]
	if prompt.DeliveryID != "update:"+testRound {
		t.Errorf("delivery id = %q, want the round-stable %q", prompt.DeliveryID, "update:"+testRound)
	}
	if prompt.Message != PodUpdateTask(testReasons) {
		t.Errorf("message = %q, want PodUpdateTask's words", prompt.Message)
	}
	if h.store.load(h.token).UpdateRound != testRound {
		t.Errorf("the round was not persisted: stored claim %+v", h.store.load(h.token))
	}
}

// The sweep posts the same round at every tick. The second post keeps the round's own clock and
// the task it already sent, and only takes the reasons again, which can grow while the round runs.
func TestPodUpdateASecondOpenOfTheSameRoundSendsNothingAgain(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	opened := h.wantRound(testRound).UpdateOpenedAt

	h.advance(time.Minute)
	h.openUpdate(testRound, testReasons+", resources")

	h.wantPrompts(1)
	c := h.wantRound(testRound)
	if c.UpdateReasons != testReasons+", resources" {
		t.Errorf("UpdateReasons = %q, want the reasons the second post carried", c.UpdateReasons)
	}
	if c.UpdateOpenedAt == nil || !c.UpdateOpenedAt.Equal(*opened) {
		t.Errorf("UpdateOpenedAt = %v, want the first post's %v", c.UpdateOpenedAt, *opened)
	}
	if c.UpdateTaskedAt == nil || !c.UpdateTaskedAt.Equal(epoch) {
		t.Errorf("UpdateTaskedAt = %v, want the first send's %v", c.UpdateTaskedAt, epoch)
	}
}

// A claim already working a phase task is not displaced: the round records itself, the queue
// refuses the second task as a wait, and the update task lands at the next post after that turn
// is over.
func TestPodUpdateDoesNotDisplaceAPhaseTask(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: h.token})
	phase := h.wantPrompts(1)[0]

	h.openUpdate(testRound, testReasons)

	h.wantPrompts(1)
	h.wantState(StateWorking)
	c := h.wantRound(testRound)
	if c.UpdateTaskedAt != nil {
		t.Errorf("UpdateTaskedAt = %v, want nil: the phase task was not displaced", c.UpdateTaskedAt)
	}
	if got := h.pending().ID; got != phase.DeliveryID {
		t.Fatalf("pending delivery = %q, want the phase task %q", got, phase.DeliveryID)
	}

	h.must(StreamTurnEnd{Claim: h.token})
	h.openUpdate(testRound, testReasons)

	update := h.wantPrompts(2)[1]
	if update.DeliveryID != "update:"+testRound {
		t.Errorf("the task after the turn = %q, want the round's %q", update.DeliveryID, "update:"+testRound)
	}
	if c := h.claim(); c.UpdateTaskedAt == nil {
		t.Error("UpdateTaskedAt is nil after the update task was sent")
	}
}

// A suspended claim is queued the round's task and revived for it, the way an outbox delivery
// revives one.
func TestPodUpdateRevivesASuspendedClaim(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByDaemon})
	h.wantState(StateSuspended)

	h.openUpdate(testRound, testReasons)

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	if got := h.pending().ID; got != "update:"+testRound {
		t.Errorf("pending delivery = %q, want the round's %q", got, "update:"+testRound)
	}
	if c := h.wantRound(testRound); c.UpdateTaskedAt == nil {
		t.Error("UpdateTaskedAt is nil after the suspended claim was given the task")
	}
}

// An operator's suspension is the one the daemon does not resume. The round's task is queued for
// that claim and waits for the operator's resume, which is what sends it.
func TestPodUpdateLeavesAnOperatorSuspendedClaimSuspended(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	// The walk to suspended delivers a phase task and works it, so one prompt is already sent.
	h.reach(StateSuspended)
	sent := len(h.prompts())

	h.openUpdate(testRound, testReasons)

	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
	h.wantPrompts(sent)
	if got := h.pending().ID; got != "update:"+testRound {
		t.Errorf("pending delivery = %q, want the round's %q", got, "update:"+testRound)
	}

	h.must(RequestResume{Claim: h.token})
	h.relaunched()

	if got := h.wantPrompts(sent + 1)[sent].DeliveryID; got != "update:"+testRound {
		t.Errorf("the task after the operator's resume = %q, want the round's %q", got, "update:"+testRound)
	}
}

// A worker that read the round's task and ended its turn without answering is asked again, once
// the last send is PodUpdateRetask old and not before.
func TestPodUpdateAsksAnUnansweredClaimAgainAfterTheRetaskBound(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.wantPrompts(1)
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	h.advance(testPodUpdateRetask - time.Minute)
	h.openUpdate(testRound, testReasons)

	h.wantPrompts(1)

	h.advance(time.Minute)
	h.openUpdate(testRound, testReasons)

	again := h.wantPrompts(2)[1]
	if again.DeliveryID != "update:"+testRound {
		t.Errorf("the second ask = %q, want the round's %q", again.DeliveryID, "update:"+testRound)
	}
	c := h.claim()
	if c.UpdateTaskedAt == nil || !c.UpdateTaskedAt.Equal(epoch.Add(testPodUpdateRetask)) {
		t.Errorf("UpdateTaskedAt = %v, want the second send's %v", c.UpdateTaskedAt, epoch.Add(testPodUpdateRetask))
	}
	if c.UpdateOpenedAt == nil || !c.UpdateOpenedAt.Equal(epoch) {
		t.Errorf("UpdateOpenedAt = %v, want the round's own clock, which a re-task never moves", c.UpdateOpenedAt)
	}
}

// A claim whose registration declared a contract below PodUpdateContract cannot answer at all, so
// the round records itself on it and asks it nothing. The round blocks on that claim until an
// operator drains it.
func TestPodUpdateNeverAsksAClaimBelowTheContract(t *testing.T) {
	h := updateHarness(t, PodUpdateContract-1)
	h.reach(StateReady)

	h.openUpdate(testRound, testReasons)

	h.wantPrompts(0)
	h.wantState(StateReady)
	c := h.wantRound(testRound)
	if c.UpdateTaskedAt != nil {
		t.Errorf("UpdateTaskedAt = %v, want nil: this claim's plugin cannot answer", c.UpdateTaskedAt)
	}
	if c.PluginContract >= PodUpdateContract {
		t.Fatalf("PluginContract = %d, want below PodUpdateContract (%d)", c.PluginContract, PodUpdateContract)
	}
}

// Once the worker has answered, the next task is held rather than sent: it would be lost with the
// process. The round's end releases it.
func TestPodUpdateHoldsTheNextTaskUntilTheRoundEnds(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.wantPrompts(1)
	h.must(StreamTurnStart{Claim: h.token})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})

	h.wantPrompts(1)
	h.wantState(StateIdle)
	if got := h.pending().Task; got != "review the pull request" {
		t.Fatalf("pending task = %q, want the held next task", got)
	}

	h.must(PodUpdateEnded{Claim: h.token})

	released := h.wantPrompts(2)[1]
	if released.Message != "review the pull request" {
		t.Errorf("the task after the round = %q, want the held one", released.Message)
	}
	c := h.wantRound("")
	if c.UpdateReasons != "" || c.UpdateOpenedAt != nil || c.UpdateTaskedAt != nil ||
		c.UpdateSignalledAt != nil || c.UpdateHead != "" || c.UpdateToldAt != nil {
		t.Errorf("claim after the round ended = %+v, want every update field cleared", c)
	}
	h.wantCalls("Resume", 0)
}

// A claim's own plugin round ends in the relaunch it was opened for: the process is replaced now,
// and the contract that relaunch is made for is recorded.
func TestPodUpdateEndedWithRelaunchReplacesTheProcess(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.openUpdate("plugin:"+itoa(PodUpdateContract+1), testReasons)
	h.signalUpdate("c0ffee")
	stale := h.locator()

	h.must(PodUpdateEnded{Claim: h.token, Relaunch: true})

	h.wantState(StateLaunching)
	resume := h.wantCalls("Resume", 1)[0]
	if resume.Previous == nil || *resume.Previous != stale || resume.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %+v from %+v, want the recorded session after the replaced process", resume.Spec, resume.Previous)
	}
	c := h.wantRound("")
	if c.PluginRelaunchFor != h.m.deps.PluginContract {
		t.Errorf("PluginRelaunchFor = %d, want the daemon's own contract %d", c.PluginRelaunchFor, h.m.deps.PluginContract)
	}
	if c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the round ended = %+v, want every update field cleared", c)
	}
	h.wantBudgets(Budgets{})
}

// A stale plugin on a claim that can answer opens a round of that claim's own instead of
// relaunching it: the worker keeps its turn, and the relaunch waits for its signal. A second post
// of StalePlugin leaves the open round exactly as it is.
func TestPodUpdateOpensARoundForAStalePluginThatCanAnswer(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.m.deps.PluginContract = PodUpdateContract + 1

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.wantState(StateReady)
	h.wantCalls("Resume", 0)
	c := h.wantRound("plugin:" + itoa(PodUpdateContract+1))
	if c.PluginRelaunchFor != 0 {
		t.Errorf("PluginRelaunchFor = %d, want 0: nothing was relaunched yet", c.PluginRelaunchFor)
	}
	wantReasons := "the process speaks plugin contract " + itoa(PodUpdateContract) +
		", and this daemon speaks " + itoa(PodUpdateContract+1)
	if c.UpdateReasons != wantReasons {
		t.Errorf("UpdateReasons = %q, want %q", c.UpdateReasons, wantReasons)
	}
	prompt := h.wantPrompts(1)[0]
	if prompt.DeliveryID != "update:"+c.UpdateRound || prompt.Message != PodUpdateTask(wantReasons) {
		t.Errorf("prompt = %+v, want the round's task under id %q", prompt, "update:"+c.UpdateRound)
	}

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.wantPrompts(1)
	h.wantState(StateReady)
	if again := h.claim(); again.UpdateOpenedAt == nil || !again.UpdateOpenedAt.Equal(*c.UpdateOpenedAt) {
		t.Errorf("UpdateOpenedAt = %v, want the first post's %v", again.UpdateOpenedAt, *c.UpdateOpenedAt)
	}
}

// A stale plugin on a claim that cannot answer keeps today's immediate relaunch: no round is
// opened, and the process is replaced at once. The relaunch keeps the pod and the disk, so what
// it costs is the turn alone.
func TestPodUpdateRelaunchesAStalePluginThatCannotAnswerAtOnce(t *testing.T) {
	h := updateHarness(t, PodUpdateContract-1)
	h.reach(StateReady)
	h.m.deps.PluginContract = PodUpdateContract

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.wantPrompts(0)
	c := h.wantRound("")
	if c.PluginRelaunchFor != PodUpdateContract {
		t.Errorf("PluginRelaunchFor = %d, want %d", c.PluginRelaunchFor, PodUpdateContract)
	}
}

// A stale registration keeps today's immediate relaunch whatever the claim's contract: the agent
// that registered is refused and its process ends on that refusal, so a round there would wait for
// a signal no running turn could send.
func TestPodUpdateRelaunchesAStaleRegistrationAtOnce(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.m.deps.PluginContract = PodUpdateContract + 1
	h.m.deps.PluginRelaunchable = true

	err := h.handle(RequestRegister{
		Claim:          h.token,
		Generation:     h.generation(),
		Session:        session,
		SessionFile:    sessionFile,
		CapabilityHash: []byte("capability"),
		PluginContract: PodUpdateContract,
	})

	if err == nil {
		t.Fatal("the stale registration was accepted, want the refusal that names both contracts")
	}
	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.wantPrompts(0)
	c := h.wantRound("")
	if c.PluginRelaunchFor != PodUpdateContract+1 {
		t.Errorf("PluginRelaunchFor = %d, want %d", c.PluginRelaunchFor, PodUpdateContract+1)
	}
}

// A round opened over a claim with no registered agent records itself and asks nothing: the next
// sweep asks it once it registers.
func TestPodUpdateRecordsTheRoundOnAClaimWithNoAgent(t *testing.T) {
	for _, state := range []ClaimState{StateQueued, StateLaunching, StateShimConnected} {
		t.Run(string(state), func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(state)

			h.openUpdate(testRound, testReasons)

			h.wantState(state)
			h.wantPrompts(0)
			if c := h.wantRound(testRound); c.UpdateTaskedAt != nil {
				t.Errorf("UpdateTaskedAt = %v, want nil: no agent has registered", c.UpdateTaskedAt)
			}
		})
	}
}

// itoa is strconv.Itoa under a short name, so the round and reason strings these tests build read
// as the code under test writes them.
func itoa(n int) string { return strconv.Itoa(n) }
