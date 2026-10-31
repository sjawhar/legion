package supervise

// An update round is the daemon asking a claim's worker to push its work and answer
// ready_for_update before the process holding that claim is replaced. These tests pin what the
// machine does with the round's two events: a round that opens once and asks once, a phase task
// the round never displaces, a suspended claim revived for the task, a claim asked again after
// PodUpdateRetask, a claim whose plugin cannot answer and is never asked, the next task held back
// from the moment the worker answers, and the round's end — which releases that task for a pod's
// round, relaunches the process for a claim's own plugin round, and tells a worker whose round
// ended with nothing replaced to carry on. The round the answer names is here too: an answer for
// the claim's own round counts, one for any other round is refused, and a round that replaces
// another under a turn in flight — a late turn, a restart's turn, a turn the daemon sent nothing
// for — switches all the same. The stale-plugin lane's two cases are here as well: a claim that
// can answer gets a round, and one that cannot, like a registration, keeps today's immediate
// relaunch.
//
// Each test drives the machine through Handle, as the daemon's sweep and the API do; no action is
// called directly.

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
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
	h.must(PodUpdateOpen{Claim: h.token, Round: round, Reasons: reasons, Task: UpdateTask(round, reasons)})
}

// asksRound is whether a delivery id is an ask of round. Each ask of a round has an id of its own
// (updateTaskID), so a test names the round the id carries rather than the whole id.
func asksRound(id, round string) bool { return updatesRound(&Delivery{ID: id}, round) }

// signalUpdate is the worker answering ready_for_update through the API, for the round its claim
// stands in — the round the task the worker read named, which is the answer every worker that
// reads its newest task gives.
func (h *harness) signalUpdate(head string) {
	h.t.Helper()
	h.signalUpdateFor(h.claim().UpdateRound, head)
}

// signalUpdateFor is that same answer naming a round of the test's choosing: the round a worker
// read before a later one replaced it, or one no claim of this daemon is in.
func (h *harness) signalUpdateFor(round, head string) {
	h.t.Helper()
	h.must(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: round, Head: head})
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
// of that ask's own, so the sweep's next post is recognised rather than sent again.
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
	if !asksRound(prompt.DeliveryID, testRound) {
		t.Errorf("delivery id = %q, want an ask of the round %q", prompt.DeliveryID, testRound)
	}
	if prompt.Message != UpdateTask(testRound, testReasons) {
		t.Errorf("message = %q, want the round's own words", prompt.Message)
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
	if !asksRound(update.DeliveryID, testRound) {
		t.Errorf("the task after the turn = %q, want an ask of the round %q", update.DeliveryID, testRound)
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
	if got := h.pending().ID; !asksRound(got, testRound) {
		t.Errorf("pending delivery = %q, want an ask of the round %q", got, testRound)
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
	if got := h.pending().ID; !asksRound(got, testRound) {
		t.Errorf("pending delivery = %q, want an ask of the round %q", got, testRound)
	}

	h.must(RequestResume{Claim: h.token})
	h.relaunched()

	if got := h.wantPrompts(sent + 1)[sent].DeliveryID; !asksRound(got, testRound) {
		t.Errorf("the task after the operator's resume = %q, want an ask of the round %q", got, testRound)
	}
}

// A worker that read the round's task and ended its turn without answering is asked again, once
// the last send is PodUpdateRetask old and not before. The second ask is a delivery of its own:
// the shim answers a prompt that carries the delivery id it already holds itself — the
// acknowledgement, the replayed start, and the end of a turn already over — and prompts Oh My Pi
// with nothing (internal/shimwire's Dedupe), so a re-task under the first ask's id would say
// nothing to the one worker it is for.
func TestPodUpdateAsksAnUnansweredClaimAgainUnderANewDeliveryID(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	first := h.wantPrompts(1)[0]
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	h.advance(testPodUpdateRetask - time.Minute)
	h.openUpdate(testRound, testReasons)

	h.wantPrompts(1)

	h.advance(time.Minute)
	h.openUpdate(testRound, testReasons)

	again := h.wantPrompts(2)[1]
	if !asksRound(again.DeliveryID, testRound) {
		t.Errorf("the second ask = %q, want an ask of the round %q", again.DeliveryID, testRound)
	}
	if again.DeliveryID == first.DeliveryID {
		t.Errorf("the second ask carries the first ask's id %q; the shim answers such a prompt itself and never prompts the worker",
			again.DeliveryID)
	}
	if again.Message != UpdateTask(testRound, testReasons) {
		t.Errorf("the second ask says %q, want the round's own words", again.Message)
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
// and the contract that relaunch is made for is recorded. The round's id is what says a relaunch
// follows — nothing beside it carries the round's kind, so no caller can post the wrong one.
func TestPodUpdateEndedInAPluginRoundReplacesTheProcess(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.openUpdate(PluginRound(PodUpdateContract+1), testReasons)
	h.signalUpdate("c0ffee")
	stale := h.locator()

	h.must(PodUpdateEnded{Claim: h.token})

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

// A relaunch the launch path refuses at a plugin round's end leaves the round exactly as the post
// found it. The end is a second entry to the relaunch relaunchStale makes, and that path checks
// the launch before it changes anything for this reason: a refusal here after the ask was dropped,
// the round cleared and PluginRelaunchFor marked would throw the worker's answer away — the next
// StalePlugin post would ask it to push again — and would leave the mark of a relaunch that never
// happened, which the next stale registration reads as the second in a row and charges toward
// failed (staleRegistration). Once the refusal clears, the next sweep's post ends the round for
// real.
func TestPodUpdateARefusedRelaunchLeavesThePluginRoundStanding(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	round := PluginRound(PodUpdateContract + 1)
	h.openUpdate(round, testReasons)
	h.signalUpdate("c0ffee")
	answered := h.claim()
	store := &refusableCheckLaunchStore{memStore: h.store, refuse: true}
	h.m.deps.Store = store

	if err := h.handle(PodUpdateEnded{Claim: h.token}); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
		t.Fatalf("the round's end refused by the launch path = %v, want the reservation's wait", err)
	}

	h.wantState(answered.State)
	h.wantCalls("Resume", 0)
	c := h.wantRound(round)
	if c.PluginRelaunchFor != 0 {
		t.Errorf("PluginRelaunchFor after the refusal = %d, want 0: no relaunch was made", c.PluginRelaunchFor)
	}
	if c.UpdateSignalledAt == nil || c.UpdateHead != answered.UpdateHead {
		t.Errorf("claim after the refusal = %+v, want the worker's answer at %q standing", c, answered.UpdateHead)
	}
	if _, held := h.store.delivery(h.token); !held {
		t.Errorf("the round's ask was taken back by an end that relaunched nothing")
	}

	store.refuse = false
	h.must(PodUpdateEnded{Claim: h.token})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	if got := h.wantRound("").PluginRelaunchFor; got != h.m.deps.PluginContract {
		t.Errorf("PluginRelaunchFor after the end that relaunched = %d, want the daemon's own contract %d", got, h.m.deps.PluginContract)
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
	c := h.wantRound(PluginRound(PodUpdateContract + 1))
	if c.PluginRelaunchFor != 0 {
		t.Errorf("PluginRelaunchFor = %d, want 0: nothing was relaunched yet", c.PluginRelaunchFor)
	}
	wantReasons := "the process speaks plugin contract " + strconv.Itoa(PodUpdateContract) +
		", and this daemon speaks " + strconv.Itoa(PodUpdateContract+1)
	if c.UpdateReasons != wantReasons {
		t.Errorf("UpdateReasons = %q, want %q", c.UpdateReasons, wantReasons)
	}
	prompt := h.wantPrompts(1)[0]
	if !asksRound(prompt.DeliveryID, c.UpdateRound) || prompt.Message != UpdateTask(c.UpdateRound, wantReasons) {
		t.Errorf("prompt = %+v, want the plugin round's own task under an ask of %q", prompt, c.UpdateRound)
	}
	if prompt.Message == UpdateTask(testRound, wantReasons) {
		t.Errorf("the plugin round sent the pod round's words: %q. The restart keeps the pod and the disk", prompt.Message)
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

// The worker's answer is fenced as every other request an agent makes is: it ends a round, and a
// round ending deletes the pod's disk, so a process of a superseded generation or of another
// session must never be what says the work is pushed.
func TestPodUpdateSignalIsFenced(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)

	err := h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: "ses_intruder", Round: testRound, Head: "c0ffee"})

	if !errors.Is(err, claim.SameAgentRefusal) {
		t.Errorf("ready_for_update as another session returned %v, want the same-agent refusal", err)
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the refused signal = %+v, want the round still waiting for its answer", c)
	}

	// The relaunch moves the claim to the next generation; the process of the one before it is the
	// stale one.
	h.observe(runtime.Gone)
	h.wantState(StateLaunching)

	err = h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation() - 1, Session: session, Round: testRound, Head: "c0ffee"})

	if !errors.Is(err, claim.StaleGeneration) {
		t.Errorf("ready_for_update at the superseded generation returned %v, want the stale-generation refusal", err)
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the stale signal = %+v, want the round still waiting for its answer", c)
	}
}

// The sweep posts an open round over every claim of it at every tick, for as long as the round
// runs. A post that decides nothing — the same round, the same reasons, and the next ask hours
// away — writes the claim no more times.
func TestPodUpdateASettledRoundIsNotWrittenAgain(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	h.advance(time.Minute)
	settled := len(h.store.history())

	h.openUpdate(testRound, testReasons)
	h.openUpdate(testRound, testReasons)

	if got := len(h.store.history()); got != settled {
		t.Errorf("two further posts of the settled round wrote the claim %d times, want none", got-settled)
	}
	h.wantPrompts(1)
	c := h.wantRound(testRound)
	if c.UpdateOpenedAt == nil || !c.UpdateOpenedAt.Equal(epoch) {
		t.Errorf("UpdateOpenedAt = %v, want the first post's %v", c.UpdateOpenedAt, epoch)
	}

	// Reasons that have grown are a decision, and are written.
	h.openUpdate(testRound, testReasons+", disk")

	if got := len(h.store.history()); got != settled+1 {
		t.Errorf("the post carrying new reasons wrote the claim %d times, want once", got-settled)
	}
	if stored := h.store.load(h.token); stored.UpdateReasons != testReasons+", disk" {
		t.Errorf("stored reasons = %q, want the grown %q", stored.UpdateReasons, testReasons+", disk")
	}
}

// A claim that retires while a round is open leaves no round behind: a worker that finished its
// phase and exited, and a claim the workflow stopped, each take their round with them. A round
// left recorded on a retired claim outlives the pod it was opened over, and the daemon's sweep
// reads it against the pod that replaced it at every tick it runs.
func TestPodUpdateARetiredClaimLeavesNoRoundBehind(t *testing.T) {
	for name, retire := range map[string]func(*harness){
		"the agent reports its exit": func(h *harness) { h.exit() },
		"the workflow stops it":      func(h *harness) { h.must(RequestStop{Claim: h.token}) },
	} {
		t.Run(name, func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateIdle)
			h.openUpdate(testRound, testReasons)
			h.signalUpdate("c0ffee")
			h.wantRound(testRound)

			retire(h)

			h.wantState(StateRetired)
			c := h.wantRound("")
			if c.UpdateSignalledAt != nil || c.UpdateHead != "" || c.UpdateOpenedAt != nil ||
				c.UpdateTaskedAt != nil || c.UpdateReasons != "" {
				t.Errorf("retired claim = %+v, want every update field cleared with the round", c)
			}
			stored := h.store.load(h.token)
			if stored.UpdateRound != "" || stored.UpdateSignalledAt != nil || stored.UpdateHead != "" {
				t.Errorf("stored claim = %+v, want the round cleared in the store too", stored)
			}
		})
	}
}

// The round ends for a failed claim as it ends for every other: the pod it was opened over has
// been replaced, so the round goes. Nothing is sent — the process that would have read a task died
// — and the claim stays failed for the workflow's retry to decide.
func TestPodUpdateEndedOnAFailedClaimClearsTheRoundAndSendsNothing(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	for death := 1; death < testLimits().LaunchFailures; death++ {
		h.observe(runtime.Gone)
		h.relaunched()
	}
	h.observe(runtime.Gone)
	h.wantState(StateFailed)
	h.wantRound(testRound)
	sent := len(h.prompts())

	h.must(PodUpdateEnded{Claim: h.token})

	h.wantState(StateFailed)
	h.wantPrompts(sent)
	c := h.wantRound("")
	if c.UpdateReasons != "" || c.UpdateOpenedAt != nil || c.UpdateTaskedAt != nil {
		t.Errorf("failed claim after the round ended = %+v, want every update field cleared", c)
	}
	if stored := h.store.load(h.token); stored.UpdateRound != "" || stored.State != StateFailed {
		t.Errorf("stored claim = %+v, want the round cleared and the claim still failed", stored)
	}
}

// A turn the worker starts after it answered is not the update task's: an Envoy notice delivered
// into its session, or a person's steer, starts a turn the daemon sent nothing for, and that turn
// can commit work the answer said was pushed. The answer is taken back, the round asks the worker
// again, and nothing of the round reads as answered until the second answer arrives.
func TestPodUpdateATurnAfterTheAnswerTakesItBackAndAsksAgain(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	first := h.wantPrompts(1)[0]
	h.must(StreamTurnStart{Claim: h.token})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})
	if c := h.claim(); c.UpdateSignalledAt == nil {
		t.Fatalf("claim after the answer = %+v, want the answer recorded", c)
	}

	h.must(StreamTurnStart{Claim: h.token})

	c := h.wantRound(testRound)
	if c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Fatalf("claim after a turn of the agent's own = %+v, want the answer taken back", c)
	}
	if stored := h.store.load(h.token); stored.UpdateSignalledAt != nil || stored.UpdateHead != "" {
		t.Errorf("stored claim = %+v, want the answer taken back in the store too", stored)
	}

	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnEnd{Claim: h.token})

	again := h.wantPrompts(2)[1]
	if !asksRound(again.DeliveryID, testRound) || again.DeliveryID == first.DeliveryID {
		t.Errorf("the ask after the foreign turn = %q, want a new ask of the round %q", again.DeliveryID, testRound)
	}
}

// The round's own task goes when the round ends. A claim left holding it would be prompted with
// "The daemon must replace the pod" for a round that is over, and until some turn read it every
// task of the claim's next phase would wait behind it (ErrDeliveryPending).
func TestPodUpdateAnEndedRoundLeavesNoTaskPending(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.wantState(StateSuspended)
	h.openUpdate(testRound, testReasons)
	if got := h.pending().ID; !asksRound(got, testRound) {
		t.Fatalf("pending delivery = %q, want the ask of the round %q waiting for the resume", got, testRound)
	}

	h.must(PodUpdateEnded{Claim: h.token})

	h.wantRound("")
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending = %+v, want the ask nobody read taken back with the round", p)
	}
	if d, held := h.store.delivery(h.token); held {
		t.Errorf("the store still holds %+v, want the ended round's ask deleted", d)
	}

	// The claim takes its next phase task, which nothing of the ended round is in the way of.
	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
	if got := h.pending().Task; got != "review the pull request" {
		t.Errorf("pending task = %q, want the next phase's", got)
	}
}

// A round that replaces an earlier one takes the earlier round's unread ask with it, and asks
// under its own. The claim holds one task at a time, so an ask of a round that is over would
// refuse this round's ask (ErrDeliveryPending) and the new round would never reach the worker.
func TestPodUpdateANewRoundReplacesTheEarlierRoundsUnreadAsk(t *testing.T) {
	const second = "pod:1f0b4c8d-7e21-4a55-8c3b-9d2e6f0a4b13"
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.openUpdate(testRound, testReasons)
	h.pending()

	h.openUpdate(second, "image, resources")

	c := h.wantRound(second)
	if c.UpdateOpenedAt == nil || c.UpdateTaskedAt == nil {
		t.Fatalf("claim in the second round = %+v, want it opened and asked", c)
	}
	got := h.pending().ID
	if !asksRound(got, second) {
		t.Fatalf("pending delivery = %q, want an ask of the second round %q", got, second)
	}
	if h.pending().Task != UpdateTask(second, "image, resources") {
		t.Errorf("pending task = %q, want the second round's words", h.pending().Task)
	}
}

// The project controller's own claim keeps the immediate relaunch whatever contract it records.
// Its `legion` tool is the controller's — read_state and set_status, and no ready_for_update —
// so a round over it would ask for an answer no turn of it can give and would stay open for ever,
// re-tasking the controller every pod_update_retask_hours. The relaunch costs it the turn in
// flight: its pod holds no issue workspace, so it has nothing to push.
func TestPodUpdateRelaunchesTheControllersStalePluginRatherThanOpeningARound(t *testing.T) {
	h := newBareHarness(t)
	h.deps.PluginContract = PodUpdateContract
	h.deps.PodUpdateRetask = testPodUpdateRetask
	c := queuedClaim()
	c.Token, c.Role, c.Issue, c.Tree = claim.ControllerToken("legion"), claim.RoleController, "", ""
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)
	h.m.deps.PluginContract = PodUpdateContract + 1

	h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.wantPrompts(0)
	got := h.wantRound("")
	if got.PluginRelaunchFor != PodUpdateContract+1 {
		t.Errorf("PluginRelaunchFor = %d, want the contract the relaunch was made for %d",
			got.PluginRelaunchFor, PodUpdateContract+1)
	}
}

// A registration that already speaks this daemon's contract ends a plugin round open over the
// claim: what the round waited for has happened. The ask nothing read goes with it, and the task
// the round was holding back is sent. A round left open here would hold that task until the
// sweep ended the round, hours later, in a relaunch of a process that is already current.
func TestPodUpdateACurrentRegistrationEndsThePluginRound(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	// The walk to idle delivers a phase task and works it, so one prompt is already sent.
	sent := len(h.prompts())
	h.openUpdate(PluginRound(PodUpdateContract+1), testReasons)
	h.wantPrompts(sent + 1)
	// The worker reads the ask, answers it and ends its turn, which retires the round's own task.
	// The next phase task then arrives while the round holds it back.
	h.must(StreamTurnStart{Claim: h.token})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
	h.wantPrompts(sent + 1)
	h.m.deps.PluginContract = PodUpdateContract + 1
	h.m.deps.PluginRelaunchable = true

	h.must(RequestRegister{
		Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: PodUpdateContract + 1,
	})

	c := h.wantRound("")
	if c.UpdateReasons != "" || c.UpdateOpenedAt != nil || c.UpdateTaskedAt != nil || c.UpdateSignalledAt != nil {
		t.Errorf("claim after the current registration = %+v, want every update field cleared", c)
	}
	if stored := h.store.load(h.token); stored.UpdateRound != "" {
		t.Errorf("stored claim = %+v, want the round cleared in the store too", stored)
	}
	released := h.wantPrompts(sent + 2)[sent+1]
	if released.Message != "review the pull request" {
		t.Errorf("the task after the registration = %q, want the one the round held back", released.Message)
	}
	h.wantCalls("Resume", 0)
}

// The sweep posts the end from what it read at the start of a tick, so a claim whose round ended
// in between is posted an end for a round that is already over. That post decides nothing: a
// second end must never replace a process this round never asked anything of — here the fresh
// process the first end's relaunch brought up.
func TestPodUpdateEndedOnAClaimInNoRoundLaunchesNothing(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateIdle)
	h.openUpdate(PluginRound(PodUpdateContract+1), testReasons)
	h.signalUpdate("c0ffee")
	h.must(PodUpdateEnded{Claim: h.token})
	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.relaunched()
	h.wantRound("")
	writes := len(h.store.history())

	h.must(PodUpdateEnded{Claim: h.token})

	h.wantState(StateReady)
	h.wantCalls("Resume", 1)
	if got := len(h.store.history()); got != writes {
		t.Errorf("the second end wrote the claim %d times, want none: it decides nothing", got-writes)
	}
}

// The sweep posts a plugin round's end from the claims it read at the start of a tick, and that
// end replaces the claim's process. A claim working a turn by then, or one whose answer was taken
// back by then, is left exactly as it is: the end would stop that turn and lose whatever it has
// not pushed, which is the one thing the round exists to prevent. The next sweep posts the end
// again, and it lands once the turn is over and the answer stands.
func TestPodUpdateEndedInAPluginRoundLeavesAClaimThatIsNotReadyAlone(t *testing.T) {
	round := PluginRound(PodUpdateContract + 1)
	for name, stale := range map[string]func(*harness){
		"a turn is running": func(h *harness) {
			ask := h.wantPrompts(1)[0]
			h.must(StreamTurnStart{Claim: h.token, DeliveryID: ask.DeliveryID})
			h.signalUpdate("c0ffee")
			h.wantState(StateWorking)
		},
		"the answer was taken back": func(h *harness) {
			ask := h.wantPrompts(1)[0]
			h.must(StreamTurnStart{Claim: h.token, DeliveryID: ask.DeliveryID})
			h.signalUpdate("c0ffee")
			h.must(StreamTurnEnd{Claim: h.token})
			// A turn the daemon sent nothing for — an Envoy notice, a person's steer — takes the
			// answer back, and the round waits for the second answer.
			h.must(StreamTurnStart{Claim: h.token})
			h.must(StreamTurnEnd{Claim: h.token})
			if c := h.claim(); c.UpdateSignalledAt != nil {
				t.Fatalf("claim after a turn of the agent's own = %+v, want the answer taken back", c)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateReady)
			h.openUpdate(round, testReasons)
			stale(h)
			writes := len(h.store.history())

			h.must(PodUpdateEnded{Claim: h.token})

			h.wantCalls("Resume", 0)
			h.wantRound(round)
			if got := len(h.store.history()); got != writes {
				t.Errorf("the end wrote the claim %d times, want none: it changed nothing", got-writes)
			}
		})
	}
}

// The same end lands once the claim is idle again with its answer standing: that is the relaunch
// the plugin round was opened for.
func TestPodUpdateEndedInAPluginRoundRelaunchesOnceTheTurnIsOver(t *testing.T) {
	round := PluginRound(PodUpdateContract + 1)
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(round, testReasons)
	ask := h.wantPrompts(1)[0]
	h.must(StreamTurnStart{Claim: h.token, DeliveryID: ask.DeliveryID})
	h.signalUpdate("c0ffee")
	h.must(PodUpdateEnded{Claim: h.token})
	h.wantCalls("Resume", 0)

	h.must(StreamTurnEnd{Claim: h.token})
	h.must(PodUpdateEnded{Claim: h.token})

	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.wantRound("")
}

// An answer counts for the round whose task the worker read, and for no other: the task names
// the round, and the answer carries that id back (UpdateTask, PodUpdateSignalled). An answer for
// any other round is refused, names both rounds, and records nothing — neither the answer nor
// the head it reported — so the round it named cannot be ended by it and the round the claim is
// in is still waiting for an answer of its own.
func TestPodUpdateAnAnswerForARoundTheClaimIsNotInIsRefused(t *testing.T) {
	plugin := PluginRound(PodUpdateContract + 1)
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	writes := len(h.store.history())

	err := h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: plugin, Head: "c0ffee"})

	if !errors.Is(err, ErrForeignUpdateRound) {
		t.Fatalf("the answer for another round returned %v, want the foreign-round refusal", err)
	}
	for _, name := range []string{plugin, testRound} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %q names neither the round answered nor the round the claim is in; want both, missing %q", err, name)
		}
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the refused answer = %+v, want the round still waiting for an answer of its own", c)
	}
	if got := len(h.store.history()); got != writes {
		t.Errorf("the refused answer wrote the claim %d times, want none", got-writes)
	}

	// A claim in no round at all is the same case: the answer names a round it is not in.
	h.must(PodUpdateEnded{Claim: h.token})
	h.wantRound("")

	err = h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: testRound, Head: "c0ffee"})

	if !errors.Is(err, ErrForeignUpdateRound) || !strings.Contains(err.Error(), testRound) {
		t.Fatalf("the answer to a claim in no round returned %v, want the foreign-round refusal naming %q", err, testRound)
	}
	if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Errorf("claim after the answer it was asked nothing for = %+v, want nothing of the answer recorded", c)
	}
}

// The answer that names the round the claim stands in is taken, exactly as it was before the id
// travelled with it: the signal and the head the worker reported are recorded, and the round's
// own sweep is what acts on them.
func TestPodUpdateAnAnswerForTheClaimsOwnRoundIsRecorded(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)

	h.signalUpdateFor(testRound, "c0ffee")

	c := h.wantRound(testRound)
	if c.UpdateSignalledAt == nil || !c.UpdateSignalledAt.Equal(epoch) {
		t.Errorf("UpdateSignalledAt = %v, want the answer's own clock at %v", c.UpdateSignalledAt, epoch)
	}
	if c.UpdateHead != "c0ffee" {
		t.Errorf("UpdateHead = %q, want the head the worker reported", c.UpdateHead)
	}
	if stored := h.store.load(h.token); stored.UpdateSignalledAt == nil {
		t.Errorf("the answer was not persisted: stored claim %+v", stored)
	}
}

// A worker answers the round its task named, so a pod round never takes a plugin round's answer,
// however the two rounds meet. The plugin round's task says the restart keeps the pod and the
// disk; the pod round's deletes both. The switch happens the moment the daemon opens the pod
// round, whatever turn is in flight, and the answer the worker gives under the plugin round's
// words is refused by its id rather than recorded against the round that replaced it — which
// would have the sweep replace a pod no worker has pushed for. The worker then reads the pod
// round's own words and answers that round, which counts.
//
// The three turns are the three ways a turn can be in flight across a switch, each of which the
// daemon once had to guess at: a turn that started late, after noTurn took the round's ask back
// unconfirmed; a turn an earlier daemon saw start, whose claim this daemon restored with the
// question still to ask (askFirst); and a turn the daemon sent nothing for, an Envoy notice or a
// person's steer. With the id carried, the daemon has to know nothing about any of them.
func TestPodUpdateAPodRoundDoesNotTakeAPluginRoundsAnswer(t *testing.T) {
	plugin := PluginRound(PodUpdateContract + 1)
	for name, turn := range map[string]func(*harness){
		"a late turn of the old round's ask": func(h *harness) {
			// The ask was acknowledged and started no turn within the bound, so it went back to
			// waiting unconfirmed; the worker's turn on those same words starts after that.
			h.advance(2 * testRPC)
			if p := h.pending(); !p.ConfirmedAt.IsZero() || !asksRound(p.ID, plugin) {
				h.t.Fatalf("pending after the wait ran out = %+v, want the round's own ask back in waiting, unconfirmed", p)
			}
			h.must(StreamTurnStart{Claim: h.token})
		},
		"a restart that has not heard from the agent": func(h *harness) {
			h.restart()
			if !h.m.askFirst {
				h.t.Fatal("the restored claim has nothing to ask its agent; this test needs the question a restart leaves open")
			}
			h.must(StreamTurnStart{Claim: h.token})
		},
		"a turn the daemon sent nothing for": func(h *harness) {
			// The round's ask had its turn, which ended without an answer, and a turn of the
			// agent's own starts after it.
			h.must(StreamTurnStart{Claim: h.token, DeliveryID: h.pending().ID})
			h.must(StreamTurnEnd{Claim: h.token})
			h.must(StreamTurnStart{Claim: h.token})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateReady)
			h.openUpdate(plugin, testReasons)
			h.wantPrompts(1)
			turn(h)
			h.wantState(StateWorking)

			h.openUpdate(testRound, testReasons)

			c := h.wantRound(testRound)
			if c.UpdateSignalledAt != nil || c.UpdateHead != "" {
				t.Fatalf("claim in the pod round = %+v, want the plugin round's fields left behind with it", c)
			}

			err := h.handle(PodUpdateSignalled{Claim: h.token, Generation: h.generation(), Session: session, Round: plugin, Head: "c0ffee"})

			if !errors.Is(err, ErrForeignUpdateRound) || !strings.Contains(err.Error(), plugin) ||
				!strings.Contains(err.Error(), testRound) {
				t.Fatalf("the answer under the plugin round's words returned %v, want the refusal naming %q and %q", err, plugin, testRound)
			}
			if c := h.claim(); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
				t.Fatalf("claim after the refused answer = %+v, want the pod round still waiting for an answer of its own", c)
			}

			// The worker's turn ends, the sweep posts the round again, and the ask it gets now is
			// the pod round's own words. The answer to that round is the one that counts.
			h.must(StreamTurnEnd{Claim: h.token})
			h.openUpdate(testRound, testReasons)

			asked := h.prompts()[len(h.prompts())-1]
			if !asksRound(asked.DeliveryID, testRound) || asked.Message != UpdateTask(testRound, testReasons) {
				t.Errorf("the ask after the switch = %+v, want the pod round's own words", asked)
			}
			h.signalUpdateFor(testRound, "c0ffee")
			if c := h.claim(); c.UpdateSignalledAt == nil || c.UpdateHead != "c0ffee" {
				t.Errorf("claim after the pod round's own answer = %+v, want that answer recorded", c)
			}
		})
	}
}

// An ask's delivery id is the one record of which round it is of. Three paths rotate a pending
// id in the middle of a round — a late agent_start, Oh My Pi's "Agent is already processing"
// refusal, and a death mid-turn — and the rotation keeps the round (rotateTaskID). An ask that
// lost it would read as a phase task: the round's end would leave it pending, the worker would be
// prompted with the words of a round whose pod is already replaced, and every task of its next
// phase would wait behind it.
func TestPodUpdateAnAskKeepsItsRoundWhenItsIdRotates(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	ask := h.wantPrompts(1)[0]

	// The ask was acknowledged and started no turn: the wait for it runs out, and the task goes
	// back to waiting under a new id the shim has no record of.
	h.advance(2 * testRPC)

	rotated := h.pending()
	if rotated.ID == ask.DeliveryID {
		t.Fatalf("pending = %q, want an id of its own after the wait ran out", rotated.ID)
	}
	if !asksRound(rotated.ID, testRound) {
		t.Fatalf("the rotated id = %q, want it still an ask of the round %q", rotated.ID, testRound)
	}

	h.must(PodUpdateEnded{Claim: h.token})

	h.wantRound("")
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending after the round ended = %+v, want the round's own ask taken back with it", p)
	}
	if d, held := h.store.delivery(h.token); held {
		t.Errorf("the store still holds %+v, want the ended round's ask deleted", d)
	}
}

// A round can end with nothing replaced: the drift it was opened for goes away — an operator
// rolls the release back — and the round simply ends. The worker that answered holds nothing by
// then: the turn it answered in retired the round's own ask, and the round held every task since
// (sendPending). Its plugin arms no reminder after ready_for_update either, so without a word
// from the daemon it would sit idle, its phase open, until some Envoy message happened to reach
// it. The daemon is the only side that knows the round ended this way, so it tells the worker so
// itself.
func TestPodUpdateARoundThatReplacedNothingTellsTheWorkerThatAnswered(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	ask := h.wantPrompts(1)[0]
	h.must(StreamTurnStart{Claim: h.token, DeliveryID: ask.DeliveryID})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending after the turn that answered = %+v, want the claim holding nothing", p)
	}

	h.must(PodUpdateEnded{Claim: h.token})

	h.wantRound("")
	h.wantState(StateIdle)
	h.wantCalls("Resume", 0)
	told := h.wantPrompts(2)[1]
	if told.Message != podUpdateOverTask {
		t.Errorf("the task after the round = %q, want the words that end the round for its worker", told.Message)
	}
	if got := h.pending().ID; got != told.DeliveryID {
		t.Errorf("pending delivery = %q, want the task just sent, %q", got, told.DeliveryID)
	}
}

// A claim that holds a task when its round ends is told nothing of the daemon's own: the round
// was holding that task back, and the end sends it (leaveRound). The claim holds one task at a
// time, so a word about the round would only wait behind the work — and the work is what tells
// the worker to carry on.
func TestPodUpdateARoundThatReplacedNothingSendsAHeldTaskRatherThanTellingTheWorker(t *testing.T) {
	h := updateHarness(t, PodUpdateContract)
	h.reach(StateReady)
	h.openUpdate(testRound, testReasons)
	ask := h.wantPrompts(1)[0]
	h.must(StreamTurnStart{Claim: h.token, DeliveryID: ask.DeliveryID})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: h.token})
	h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
	h.wantPrompts(1)

	h.must(PodUpdateEnded{Claim: h.token})

	released := h.wantPrompts(2)[1]
	if released.Message != "review the pull request" {
		t.Errorf("the task after the round = %q, want the one the round held back", released.Message)
	}
	if got := h.pending().Task; got != "review the pull request" {
		t.Errorf("pending task = %q, want the held task, with no word of the daemon's queued behind it", got)
	}
}
