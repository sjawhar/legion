package daemon

// The two limits on how fast the sweep opens pod rounds (INFRA-1116): pod_update_max_open_rounds,
// how many issues may hold an open round at once, and pod_update_rounds_per_minute, how many
// rounds one sweep may open. One change to the pod template drifts every pod of the project at
// once, and with neither limit set the first sweep after it asks every worker of every issue to
// stop what it is doing and push: the whole fleet goes quiet inside a minute, and every disk in
// it is deleted and rebuilt at once. Under the limits the change reaches a few pods first, and
// the rest as those finish.
//
// These tests drive the fleet rig (podupdate_fleet_test.go) over several issues, each with a pod
// and one architect of its own, and tick it by hand: its clock is held, so which sweep runs when
// is the test's to say and no ticker of Go's decides it. Which claim of an issue answers when is
// podupdate_test.go's business, so one role per issue answers every round here, and what each
// sweep opens is what these tests read. The two issues whose round reaches no claim that can
// record it carry a second role of their own, because which claims an opening can reach is what
// the sweep charges it against.
//
// A pod a replacement built carries the template the runtime has now, so an issue is scripted no
// drift once its pod has been replaced. Left drifted for ever it would open a round again at
// every sweep, hold its slot for ever, and no issue behind it would ever get one.

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// limitDrift is what the runtime answers for every pod of these rigs: the one change to the pod
// template that drifts every pod built before it.
const limitDrift = "the pod image, now ghcr.io/cybermonklabs/legion/worker:v11"

// limitIssue is one issue of a limit rig: its key, a pod uid of its own as a cluster gives each
// pod one, and the architect that runs in it.
func limitIssue(n int, key string) fleetIssue {
	return fleetIssue{
		key: key, phase: phase.Implementing,
		pod:   fmt.Sprintf("%08d-7a92-4c1f-8d35-2f6b9c0ad417", n),
		roles: []claim.Role{claim.RoleArchitect},
	}
}

// newLimitRig is the fleet rig over keys, every pod of it drifted by the one template change and
// neither limit set: a test sets the limits it is of on rig.s.cfg before its first sweep.
func newLimitRig(t *testing.T, keys ...string) *fleetRig {
	t.Helper()
	issues := make([]fleetIssue, 0, len(keys))
	for i, key := range keys {
		issues = append(issues, limitIssue(i+1, key))
	}
	rig := newFleetRig(t, issues...)
	for _, key := range keys {
		rig.drifts(key, limitDrift)
	}
	return rig
}

// openRounds is every issue of the rig whose claims record the round over its own pod, in key
// order: what the sweep has opened and not yet ended.
func (r *fleetRig) openRounds() []string {
	r.t.Helper()
	open := map[string]bool{}
	for key := range r.tokens {
		if r.claimOf(key.issue, key.role).UpdateRound == r.round(key.issue) {
			open[key.issue] = true
		}
	}
	return slices.Sorted(maps.Keys(open))
}

// awaitRounds waits for exactly the issues named to hold a round, which the sweep's posts reach
// through each machine's own queue. It sweeps nothing: an issue a limit held back was posted
// nothing at all, so what the sweep left it is there to be read as soon as what it did post has
// landed.
func (r *fleetRig) awaitRounds(what string, want ...string) {
	r.t.Helper()
	slices.Sort(want)
	testwait.Eventually(r.t, fmt.Sprintf("the issues in a round to be %v after %s", want, what), func() bool {
		return slices.Equal(r.openRounds(), want)
	})
}

// awaitRoundEnded waits for the round of issue to be gone from every claim of it, which the
// replacement posts to each of them (endPodUpdates). It is what a test waits for before it
// sweeps again: a sweep that read that end half landed would find the round open and complete,
// and would ask its workers again over the claims the end has already reached, which puts the
// round back on a claim that had left it (podupdate_test.go's awaitSwept).
func (r *fleetRig) awaitRoundEnded(issue string) {
	r.t.Helper()
	testwait.Eventually(r.t, "the round of "+issue+" to end on every claim of it", func() bool {
		return !slices.Contains(r.openRounds(), issue)
	})
}

// sweepUntilRounds sweeps until exactly the issues named hold a round. The slot an ended round
// frees is read by a sweep after the one that ended it, since the end reaches each claim through
// its machine's own queue, so how many sweeps a step takes is no part of what these tests pin:
// that no sweep opens more than the limits allow is. The first poll sweeps nothing, which leaves
// what the sweep before it decided the time to land (awaitRoundEnded).
func (r *fleetRig) sweepUntilRounds(what string, want ...string) {
	r.t.Helper()
	slices.Sort(want)
	landing := true
	testwait.Eventually(r.t, fmt.Sprintf("the issues in a round to be %v after %s", want, what), func() bool {
		if slices.Equal(r.openRounds(), want) {
			return true
		}
		if landing {
			landing = false
			return false
		}
		r.tick()
		return slices.Equal(r.openRounds(), want)
	})
}

// sweepUntilReplaced sweeps until the pod of issue has been replaced, as the single-issue rig's
// awaitReplacement does: a sweep that reads a machine mid-decision leaves the replacement to the
// next one (roundGroup's mask), so which sweep makes it is no part of what these tests pin. Its
// first poll sweeps nothing either, for the reason sweepUntilRounds's does not sweep.
func (r *fleetRig) sweepUntilReplaced(issue string) {
	r.t.Helper()
	replaced := func() bool { return len(r.replacementsOf(r.pods[issue])) > 0 }
	landing := true
	testwait.Eventually(r.t, "the pod of "+issue+" to be replaced", func() bool {
		if replaced() {
			return true
		}
		if landing {
			landing = false
			return false
		}
		r.tick()
		return replaced()
	})
}

// answer is every worker of issue answering its round, as the `legion` tool's ready_for_update
// posts it. Each waits for its own machine to have handled what it is owed first, so the sweep
// that follows reads the answer rather than a machine still deciding (roundGroup's mask).
func (r *fleetRig) answer(issue string) {
	r.t.Helper()
	for _, role := range r.rolesOf(issue) {
		m, ok := r.sup.Machine(r.token(issue, role))
		if !ok {
			r.t.Fatalf("%s's %s is not supervised", issue, role)
		}
		m.Wait()
		r.signal(issue, role, "head-"+string(role))
	}
}

// rolesOf is every role of issue this rig holds a claim for, in name order.
func (r *fleetRig) rolesOf(issue string) []claim.Role {
	var roles []claim.Role
	for key := range r.tokens {
		if key.issue == issue {
			roles = append(roles, key.role)
		}
	}
	slices.Sort(roles)
	return roles
}

// replacedOnce holds every issue named to exactly one replacement of its pod: the limits put a
// fleet-wide change through in several sweeps rather than one, and every pod of it is carried in
// the end.
func (r *fleetRig) replacedOnce(issues ...string) {
	r.t.Helper()
	for _, issue := range issues {
		if calls := r.replacementsOf(r.pods[issue]); len(calls) != 1 {
			r.t.Errorf("the pod of %s was replaced %d times, want once", issue, len(calls))
		}
	}
}

// wantPodUpdateRecord holds the record the rig's last sweep wrote (podUpdateRoundsView) to want.
// Both are read as the operator's route serves them (api.PodUpdateRounds.MarshalJSON), which is
// where the two differences that are no difference go away: a limit is the number it holds rather
// than the address of a pointer, and a list nothing filled reads as the empty list the wire
// carries.
func (r *fleetRig) wantPodUpdateRecord(what string, want api.PodUpdateRounds) {
	r.t.Helper()
	if got := r.recordAsServed(r.s.podUpdateRoundsView()); got != r.recordAsServed(want) {
		r.t.Errorf("the record after %s is %s, want %s", what, got, r.recordAsServed(want))
	}
}

// recordAsServed is one sweep record as the claim listing's route answers it.
func (r *fleetRig) recordAsServed(rounds api.PodUpdateRounds) string {
	r.t.Helper()
	served, err := json.Marshal(rounds)
	if err != nil {
		r.t.Fatalf("encode the sweep's record: %v", err)
	}
	return string(served)
}

// The cap on open rounds is a cap on how much of the fleet is held waiting at once: an issue in a
// round has every worker of it told to push and start nothing new, so a fleet-wide drift with no
// cap stops every worker of every issue at the first sweep. Under the cap the issues that fit go
// first — in the order of their keys, which is the order the sweep walks them in, and which is a
// fleet's keys as strings rather than as numbers — and an issue waits until one of them has
// finished and freed its slot. Nothing is lost by waiting: every pod of the fleet is replaced in
// the end, each after its own workers have pushed.
func TestPodUpdateMaxOpenRounds(t *testing.T) {
	keys := []string{"INFRA-2", "INFRA-10", "INFRA-1000", "INFRA-30", "INFRA-4"}
	rig := newLimitRig(t, keys...)
	rig.s.cfg.PodUpdateMaxOpenRounds = new(2)

	rig.tick()
	rig.awaitRounds("the first sweep under a cap of two open rounds", "INFRA-10", "INFRA-1000")

	// Nothing a later sweep reads opens a third while those two hold the slots.
	rig.tick()
	rig.tick()
	rig.awaitRounds("two more sweeps with both slots held", "INFRA-10", "INFRA-1000")
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before a worker answered anything", len(calls))
	}

	for _, step := range []struct {
		answered string
		then     []string
	}{
		{answered: "INFRA-10", then: []string{"INFRA-1000", "INFRA-2"}},
		{answered: "INFRA-1000", then: []string{"INFRA-2", "INFRA-30"}},
		{answered: "INFRA-2", then: []string{"INFRA-30", "INFRA-4"}},
		{answered: "INFRA-30", then: []string{"INFRA-4"}},
		{answered: "INFRA-4"},
	} {
		rig.answer(step.answered)
		rig.sweepUntilReplaced(step.answered)
		rig.awaitRoundEnded(step.answered)
		rig.drifts(step.answered)
		rig.sweepUntilRounds("the pod of "+step.answered+" was replaced and its slot freed", step.then...)
	}

	rig.replacedOnce(keys...)
}

// The slots are counted from the claims, so a daemon that boots into a fleet already in rounds
// counts the rounds it finds rather than the ones it opened itself. Counted from its own memory
// — which a boot empties — it would read the whole project as idle and open the cap again over
// a fleet that is already at it, which is the one thing the cap is there to stop. And a round it
// did not open is still none of its business to end: a worker told to push is waited for, boot or
// no boot.
func TestPodUpdateMaxOpenRoundsRestart(t *testing.T) {
	keys := []string{"INFRA-1", "INFRA-2", "INFRA-3", "INFRA-4"}
	rig := newLimitRig(t, keys...)
	// The daemon before the restart ran under no cap and opened three rounds; the fourth issue's
	// pod has not drifted yet, as a pod built after that change has not.
	rig.drifts("INFRA-4")

	rig.tick()
	rig.awaitRounds("the first daemon's sweep under no cap", "INFRA-1", "INFRA-2", "INFRA-3")

	next := rig.restart()
	next.s.cfg.PodUpdateMaxOpenRounds = new(2)
	for _, key := range keys {
		next.drifts(key, limitDrift)
	}

	next.tick()
	next.tick()
	next.awaitRounds("two sweeps of a daemon that booted into three open rounds", "INFRA-1", "INFRA-2", "INFRA-3")
	if calls := next.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before a worker answered: the cap ends no round it finds open", len(calls))
	}

	next.answer("INFRA-1")
	next.sweepUntilReplaced("INFRA-1")
	next.awaitRoundEnded("INFRA-1")
	next.drifts("INFRA-1")
	next.sweepUntilRounds("the first of the three was replaced, which leaves two slots held", "INFRA-2", "INFRA-3")

	next.answer("INFRA-2")
	next.sweepUntilReplaced("INFRA-2")
	next.awaitRoundEnded("INFRA-2")
	next.drifts("INFRA-2")
	next.sweepUntilRounds("the second was replaced, which leaves fewer than two", "INFRA-3", "INFRA-4")
}

// A slot is a pod being carried, not a row in a table. An issue whose claims record no pod has
// nothing for its round to replace and nothing the round holds up — the round waits for a claim
// to run in a pod again — so holding a slot for it would spend the project's capacity on an
// issue the sweep can decide nothing of. The other way round, an issue waiting for a slot is
// asked afresh when it gets one: a pod whose drift has gone away in the meantime, because an
// operator rolled the template back, is a pod no round need stop any worker for.
func TestPodUpdateMaxOpenRoundsPodless(t *testing.T) {
	rig := newLimitRig(t, "INFRA-1", "INFRA-2", "INFRA-3")
	rig.s.cfg.PodUpdateMaxOpenRounds = new(1)

	rig.tick()
	rig.awaitRounds("the first sweep under a cap of one", "INFRA-1")

	// The claims of the one open round record no pod now, as an operator's suspension leaves
	// them: the round stays open, and the slot it held goes to the issue behind it.
	rig.podless("INFRA-1")
	rig.tick()
	rig.awaitRounds("the sweep that read the round's own issue recording no pod", "INFRA-1", "INFRA-2")

	// The last issue's pod is no longer drifted by the time its slot comes free, so the sweep
	// opens nothing for it.
	rig.drifts("INFRA-3")
	rig.answer("INFRA-2")
	rig.sweepUntilReplaced("INFRA-2")
	rig.awaitRoundEnded("INFRA-2")
	rig.drifts("INFRA-2")
	for range 3 {
		rig.tick()
	}
	rig.awaitRounds("three sweeps with a free slot and nothing left that has drifted", "INFRA-1")
}

// An issue a limit holds back is still carried: the plugin round of one of its claims is one
// process to replace, not a pod, and it ends as soon as that worker answers. Held behind the pod
// round the issue is waiting for, it would hold that claim's next task for as long as the issue
// waits for a slot, which under a fleet-wide roll is as long as the roll.
func TestPodUpdateMaxOpenRoundsPluginRound(t *testing.T) {
	// The architect of the issue the cap holds back runs on the contract before this daemon's,
	// and the stale-plugin lane has a round of its own open over it.
	held := limitIssue(2, "INFRA-2")
	held.pluginRound = true
	rig := newFleetRig(t, limitIssue(1, "INFRA-1"), held)
	for _, key := range []string{"INFRA-1", "INFRA-2"} {
		rig.drifts(key, limitDrift)
	}
	rig.s.cfg.PodUpdateMaxOpenRounds = new(1)
	plugin := supervise.PluginRound(api.DaemonAPIVersion)

	rig.tick()
	rig.awaitRounds("the first sweep under a cap of one", "INFRA-1")
	if got := rig.claimOf("INFRA-2", claim.RoleArchitect).UpdateRound; got != plugin {
		t.Fatalf("the architect of the held-back issue is in round %q, want its plugin round %q", got, plugin)
	}

	rig.answer("INFRA-2")
	rig.tick()

	testwait.Eventually(t, "the plugin round of INFRA-2's architect to end at a sweep that held its issue back", func() bool {
		return rig.claimOf("INFRA-2", claim.RoleArchitect).UpdateRound == ""
	})
	rig.awaitRounds("the plugin round ended under the cap", "INFRA-1")
}

// An opening is an ask that reached a worker. An issue no agent has run for has nobody to ask,
// so the sweep posts it nothing and its claims record no round: charged a slot and an opening all
// the same, such an issue would take the one opening of every sweep for ever — it can never
// record the round that would free it — and every issue behind it would wait on a round that
// never opened.
func TestPodUpdateMaxOpenRoundsChargesOnlyAnAskThatReachedAWorker(t *testing.T) {
	workerless := "INFRA-1"
	keys := []string{workerless, "INFRA-2", "INFRA-3", "INFRA-4", "INFRA-5"}
	issues := make([]fleetIssue, 0, len(keys))
	for i, key := range keys {
		issue := limitIssue(i+1, key)
		if key == workerless {
			issue.unregistered = issue.roles
		}
		issues = append(issues, issue)
	}
	rig := newFleetRig(t, issues...)
	for _, key := range keys {
		rig.drifts(key, limitDrift)
	}
	rig.s.cfg.PodUpdateMaxOpenRounds = new(1)

	// The issue with no worker sorts first, so every sweep reaches it before any other.
	for _, key := range keys[1:] {
		rig.tick()
		rig.awaitRounds("the sweep whose one opening went to "+key, key)
		rig.answer(key)
		rig.sweepUntilReplaced(key)
		rig.drifts(key)
		rig.awaitRounds("the round of " + key + " ended with its pod")
	}

	if got := rig.claimOf(workerless, claim.RoleArchitect).UpdateRound; got != "" {
		t.Errorf("the issue with no worker records round %q, want none: nothing was asked of it", got)
	}
	if calls := rig.replacementsOf(rig.pods[workerless]); len(calls) != 0 {
		t.Errorf("the pod of the issue with no worker was replaced %d times, want never: no round was ever opened over it", len(calls))
	}
	rig.replacedOnce(keys[1:]...)
}

// An opening is charged for an ask that can reach a claim whose machine records it, and a failed
// claim is not one: it is posted like every other claim with a session and its machine ignores
// the post (supervise's table). An issue whose only claim with a session has failed therefore
// records no round however often it is asked, while another claim of it — one launching for the
// first time — records the pod, which is what keeps the sweep reading the issue as one whose
// round this daemon opened and whose claims have not recorded it yet. Charged an opening, it
// would hold its own slot for ever: at a cap of one, every other drifted issue of the fleet
// would wait for the daemon to restart.
func TestPodUpdateMaxOpenRoundsChargesNoOpeningPostedOnlyToAFailedClaim(t *testing.T) {
	unreachable := "INFRA-1"
	keys := []string{unreachable, "INFRA-2", "INFRA-3"}
	issues := make([]fleetIssue, 0, len(keys))
	for i, key := range keys {
		issue := limitIssue(i+1, key)
		if key == unreachable {
			// The architect failed, which keeps the session its agent registered and drops its
			// locator; the implementer is a process that has started whose agent has not
			// registered yet, which is what records the pod. The round's post goes to the
			// architect alone, and its machine ignores it.
			issue.roles = []claim.Role{claim.RoleArchitect, claim.RoleImplementer}
			issue.failed = []claim.Role{claim.RoleArchitect}
			issue.unregistered = []claim.Role{claim.RoleImplementer}
		}
		issues = append(issues, issue)
	}
	rig := newFleetRig(t, issues...)
	for _, key := range keys {
		rig.drifts(key, limitDrift)
	}
	rig.s.cfg.PodUpdateMaxOpenRounds = new(1)

	// The issue whose round can reach nobody sorts first, so the sweep reaches it before either
	// of the others: the one slot goes to the issue behind it all the same. The sweep's own
	// record is read before the round lands, because the sweep writes it where it decides it.
	rig.tick()
	rig.wantPodUpdateRecord("the first sweep", api.PodUpdateRounds{
		Swept: true, MaxOpenRounds: new(1),
		InRound:                []string{"INFRA-2"},
		WaitingOnMaxOpenRounds: []string{"INFRA-3"},
	})
	rig.awaitRounds("the first sweep under a cap of one, walked from the issue no round can reach", "INFRA-2")

	// And the slot comes back: the issue that took it answers, its pod is replaced, and the last
	// issue of the fleet gets the slot rather than waiting behind an opening nothing recorded.
	rig.answer("INFRA-2")
	rig.sweepUntilReplaced("INFRA-2")
	rig.awaitRoundEnded("INFRA-2")
	rig.drifts("INFRA-2")
	rig.sweepUntilRounds("the pod of INFRA-2 was replaced and its slot freed", "INFRA-3")

	if got := rig.claimOf(unreachable, claim.RoleArchitect).UpdateRound; got != "" {
		t.Errorf("the failed architect records round %q, want none: no machine of a failed claim takes the round's post", got)
	}
	if calls := rig.replacementsOf(rig.pods[unreachable]); len(calls) != 0 {
		t.Errorf("the pod of the issue no round can reach was replaced %d times, want never: no round was ever opened over it", len(calls))
	}
	rig.replacedOnce("INFRA-2")
}

// An opening still in flight is one round, not two. The post that carries a round to a machine
// is asynchronous and a machine in a relaunch takes minutes to take it, so the sweep after an
// opening reads the claims of that issue recording no round at all. Read there as a new opening,
// the issue is either held behind the slot it is itself holding — and the operator's line then
// names an issue in a round as one waiting for a slot — or opened again and charged a second
// slot, which leaves another drifted issue waiting for a slot nothing is using.
//
// The lever is the state the defect is about. The issue's architect is held inside a Resume, as
// a relaunch into a pod being built holds it: its machine's lock is held for the whole of the
// sweeps below, so the round posted to it waits in its inbox and no sleep is needed to say so. A
// relaunching claim records no locator (supervise's letGo), so the pod the sweep reads is the
// one the issue's implementer records — a process that has started whose agent has not
// registered yet, which the round posts nothing to — and no claim of the issue can record the
// round until that relaunch ends.
func TestPodUpdateMaxOpenRoundsChargesNoSecondSlotForAnOpeningStillInFlight(t *testing.T) {
	inFlight, second := "INFRA-1", "INFRA-2"
	relaunching := limitIssue(1, inFlight)
	relaunching.roles = []claim.Role{claim.RoleArchitect, claim.RoleImplementer}
	relaunching.unregistered = []claim.Role{claim.RoleImplementer}
	rig := newFleetRig(t, relaunching, limitIssue(2, second))
	for _, key := range []string{inFlight, second} {
		rig.drifts(key, limitDrift)
	}
	rig.s.cfg.PodUpdateMaxOpenRounds = new(2)
	rig.s.cfg.PodUpdateRoundsPerMinute = new(1)

	// The relaunch comes back into the pod the claim already ran in, which is what a role whose
	// process died in a pod that is still there does. Its locator is read before the machine is
	// held: a read of the claim itself waits the decision in flight out (Machine.Claim).
	architect := rig.token(inFlight, claim.RoleArchitect)
	died := *rig.claimOf(inFlight, claim.RoleArchitect).Locator
	rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(architect, inFlight, rig.pods[inFlight])})
	entered, release := make(chan struct{}), make(chan struct{})
	releaseMachine := sync.OnceFunc(func() { close(release) })
	rig.rt.HoldResume(func() {
		rig.rt.HoldResume(nil)
		close(entered)
		<-release
	})
	defer releaseMachine()
	go func() {
		m, _ := rig.sup.Machine(architect)
		_ = m.Handle(rig.ctx, supervise.RuntimeObservation{
			Observation: runtime.Observation{Locator: died, Kind: runtime.Gone, Detail: "the process ended"},
		})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the architect's machine never reached the Resume this test holds it in")
	}

	// The first sweep opens the round of the held issue and spends the minute's budget on it.
	// Nothing of it reaches any claim's record: the one machine it was posted to is relaunching.
	rig.tick()
	rig.wantPodUpdateRecord("the sweep that opened the round of the relaunching issue", api.PodUpdateRounds{
		Swept: true, MaxOpenRounds: new(2), RoundsPerMinute: new(1),
		InRound:                  []string{inFlight},
		WaitingOnRoundsPerMinute: []string{second},
	})

	// A minute on, that opening is still in flight and is still the one round it was: the second
	// issue opens its own under the cap's other slot, rather than waiting for a slot the first
	// issue was charged twice for.
	rig.clock.advance(time.Minute)
	rig.tick()
	rig.wantPodUpdateRecord("the sweep after the opening, with its post still in flight", api.PodUpdateRounds{
		Swept: true, MaxOpenRounds: new(2), RoundsPerMinute: new(1),
		InRound: []string{inFlight, second},
	})
	rig.awaitRound(second, claim.RoleArchitect)

	// With both slots held — one by the opening in flight, one by the round the second issue's
	// claims now record — the issue whose post has not landed is in a round, and the operator's
	// line says so rather than naming it as one waiting for the slot it is holding.
	rig.clock.advance(time.Minute)
	rig.tick()
	rig.wantPodUpdateRecord("the sweep with both of the cap's slots held", api.PodUpdateRounds{
		Swept: true, MaxOpenRounds: new(2), RoundsPerMinute: new(1),
		InRound: []string{inFlight, second},
	})

	// The relaunch ends and the machine takes the post it was holding: the round the sweeps have
	// been counting all along is the round its claim records, and nothing is charged for it
	// again now that the claims say it themselves.
	releaseMachine()
	rig.awaitRounds("the relaunch ended and the held machine took the round it was posted", inFlight, second)

	rig.clock.advance(time.Minute)
	rig.tick()
	rig.wantPodUpdateRecord("the sweep after the opening landed on its claim", api.PodUpdateRounds{
		Swept: true, MaxOpenRounds: new(2), RoundsPerMinute: new(1),
		InRound: []string{inFlight, second},
	})
}

// The per-minute limit is what paces a fleet-wide roll in time rather than in slots: with it, the
// workers of the fleet are asked a few at a minute, so a change lands as a sequence of small
// interruptions rather than one that stops everyone at once. It paces openings and nothing else
// — a round already open is neither ended nor paused nor asked again by it, since a worker told
// to push and wait must be waited for — and every pod of the fleet is replaced in the end.
func TestPodUpdateRoundsPerMinute(t *testing.T) {
	keys := []string{"INFRA-1", "INFRA-2", "INFRA-3", "INFRA-4", "INFRA-5"}
	rig := newLimitRig(t, keys...)
	rig.s.cfg.PodUpdateRoundsPerMinute = new(1)

	asked := map[string]time.Time{}
	for i, key := range keys {
		if i > 0 {
			rig.clock.advance(time.Minute)
		}
		rig.tick()
		rig.awaitRounds(fmt.Sprintf("sweep %d at one round a minute", i+1), keys[:i+1]...)
		opened := rig.claimOf(key, claim.RoleArchitect)
		if opened.UpdateTaskedAt == nil {
			t.Fatalf("%s holds the round with no ask recorded: %+v", key, opened)
		}
		asked[key] = *opened.UpdateTaskedAt
	}

	for _, key := range keys {
		held := rig.claimOf(key, claim.RoleArchitect)
		if held.UpdateRound != rig.round(key) {
			t.Errorf("%s records round %q after the five sweeps, want its own open round: the limit ends none", key, held.UpdateRound)
		}
		if held.UpdateTaskedAt == nil || !held.UpdateTaskedAt.Equal(asked[key]) {
			t.Errorf("%s was asked at %v, want the ask it was opened with at %v: the limit asks no open round again",
				key, held.UpdateTaskedAt, asked[key])
		}
	}

	for _, key := range keys {
		rig.answer(key)
	}
	for _, key := range keys {
		rig.sweepUntilReplaced(key)
		rig.drifts(key)
	}
	rig.replacedOnce(keys...)

	// The same fleet under a key of two opens two issues at one sweep: the key is a budget for
	// the sweep, spent at it, rather than a gap between one opening and the next.
	two := newLimitRig(t, keys...)
	two.s.cfg.PodUpdateRoundsPerMinute = new(2)

	two.tick()
	two.awaitRounds("the first sweep at two rounds a minute", "INFRA-1", "INFRA-2")
}

// The gap the limit holds is shorter than the minute it is written in, because a sweep's own
// arrival wanders: ticks a minute apart reach the sweep a little early and a little late, and a
// gap of exactly a minute would drop every opening of a sweep that arrived early — one round a
// minute would become one every two. A gap short enough to pass that wander is still long
// enough to stop the burst it is there to stop: a sweep that ran long and the tick that follows
// it do not open two minutes' worth of rounds between them.
func TestPodUpdateRoundsPerMinuteJitter(t *testing.T) {
	keys := []string{"INFRA-1", "INFRA-2", "INFRA-3", "INFRA-4", "INFRA-5"}
	rig := newLimitRig(t, keys...)
	rig.s.cfg.PodUpdateRoundsPerMinute = new(1)

	rig.tick()
	rig.awaitRounds("the first sweep of a daemon that has opened nothing", "INFRA-1")

	rig.clock.advance(59500 * time.Millisecond)
	rig.tick()
	rig.awaitRounds("a sweep 59.5 seconds after the last opening", "INFRA-1", "INFRA-2")

	rig.clock.advance(60500 * time.Millisecond)
	rig.tick()
	rig.awaitRounds("a sweep 60.5 seconds after the last opening", "INFRA-1", "INFRA-2", "INFRA-3")

	rig.clock.advance(50500 * time.Millisecond)
	rig.tick()
	rig.awaitRounds("a sweep 50.5 seconds after the last opening", "INFRA-1", "INFRA-2", "INFRA-3", "INFRA-4")

	rig.clock.advance(50 * time.Second)
	rig.tick()
	rig.tick()
	rig.awaitRounds("two sweeps 50 seconds after the last opening", "INFRA-1", "INFRA-2", "INFRA-3", "INFRA-4")
}

// A sweep's openings are dated from the sweep rather than from whatever its replacements took
// afterwards. A replacement waits its pod out, which is minutes, and the sweep makes them after
// it has walked the issues: dated from the end of the call, the openings of a slow sweep would
// be read as made minutes after they were, every sweep behind a slow one would fall inside the
// gap, and the fleet would be paced by how long its pods take to come back rather than by the
// key the operator set. A restarted daemon is at the other end of the same question: it has no
// record of what the daemon before it opened, so its first sweep opens as a first sweep does.
func TestPodUpdateRoundsPerMinuteSlowSweep(t *testing.T) {
	keys := []string{"INFRA-1", "INFRA-2", "INFRA-3", "INFRA-4"}
	rig := newLimitRig(t, keys...)
	rig.s.cfg.PodUpdateRoundsPerMinute = new(1)
	start := rig.clock.Now()

	rig.tick()
	rig.awaitRounds("the first sweep", "INFRA-1")
	rig.answer("INFRA-1")

	// The sweep a minute later has a pod to replace as well as a round to open, and the
	// replacement waits that pod out for seventy seconds. One hold only: the sweeps after it
	// find the runtime as this one left it.
	rig.rt.HoldReplacePod(func() {
		rig.rt.HoldReplacePod(nil)
		rig.clock.advance(70 * time.Second)
	})
	rig.clock.advance(time.Minute)
	rig.tick()
	rig.sweepUntilReplaced("INFRA-1")
	rig.drifts("INFRA-1")
	if got, want := rig.clock.Now().Sub(start), 130*time.Second; got != want {
		t.Fatalf("the sweep's replacement held the clock for %s, want the minute to the sweep and the pod's seventy seconds, %s", got, want)
	}
	rig.sweepUntilRounds("the slow sweep opened one and held the rest back", "INFRA-2")

	// Seventy seconds after the opening that slow sweep recorded, the next sweep opens one.
	rig.tick()
	rig.awaitRounds("the sweep seventy seconds after the slow sweep's opening", "INFRA-2", "INFRA-3")

	// Fifty seconds after that opening, no sweep opens anything.
	rig.clock.advance(50 * time.Second)
	rig.tick()
	rig.tick()
	rig.awaitRounds("two sweeps fifty seconds after the last opening", "INFRA-2", "INFRA-3")

	// A daemon that boots knows nothing of what the one before it opened, and opens one.
	next := rig.restart()
	for _, key := range []string{"INFRA-2", "INFRA-3", "INFRA-4"} {
		next.drifts(key, limitDrift)
	}

	next.tick()
	next.awaitRounds("the first sweep of a restarted daemon", "INFRA-2", "INFRA-3", "INFRA-4")
}

// The one line an operator reads for the sweep (`legion claims list`) is this record, and these
// are real sweeps of the fleet writing it. Nothing else holds what it says: an issue in a round
// has that round on its claims, but an issue a limit held back is posted nothing at all and no
// claim of it says a word, so a fleet-wide roll that has stalled and one the limits are pacing
// read exactly alike without it. Each waiting issue is under the limit that held it back at that
// sweep, because that is the one an operator would have to raise to free it: the limits are read
// off the record together with the issues they are holding.
func TestClaimsListPodUpdateLine(t *testing.T) {
	keys := []string{"INFRA-2", "INFRA-10", "INFRA-1000", "INFRA-30", "INFRA-4"}
	rig := newLimitRig(t, keys...)
	rig.s.cfg.PodUpdateMaxOpenRounds = new(2)
	rig.s.cfg.PodUpdateRoundsPerMinute = new(1)

	rig.wantPodUpdateRecord("a daemon whose first sweep has not run", api.PodUpdateRounds{})

	// The first sweep has both slots free and spends the minute's whole budget on the first issue
	// of the walk, so every issue behind it waits on the limit a minute.
	rig.tick()
	rig.awaitRounds("the first sweep at two open rounds and one a minute", "INFRA-10")
	rig.wantPodUpdateRecord("the first sweep", api.PodUpdateRounds{
		Swept: true, MaxOpenRounds: new(2), RoundsPerMinute: new(1),
		InRound:                  []string{"INFRA-10"},
		WaitingOnRoundsPerMinute: []string{"INFRA-1000", "INFRA-2", "INFRA-30", "INFRA-4"},
	})

	// A minute on, the budget is back and the second opening takes the last slot: from there the
	// limit on open rounds is what holds the rest, and it is the one they are named under.
	rig.clock.advance(time.Minute)
	rig.tick()
	rig.awaitRounds("a second sweep one minute later", "INFRA-10", "INFRA-1000")
	rig.wantPodUpdateRecord("the second sweep", api.PodUpdateRounds{
		Swept: true, MaxOpenRounds: new(2), RoundsPerMinute: new(1),
		InRound:                []string{"INFRA-10", "INFRA-1000"},
		WaitingOnMaxOpenRounds: []string{"INFRA-2", "INFRA-30", "INFRA-4"},
	})

	// A project that sets neither key asks for no limit, which no number says: the whole fleet is
	// in a round at the one sweep and nothing waits on anything.
	none := newLimitRig(t, keys...)
	none.tick()
	none.awaitRounds("one sweep under neither limit", keys...)
	none.wantPodUpdateRecord("a sweep of a project that sets neither key", api.PodUpdateRounds{
		Swept: true, InRound: []string{"INFRA-10", "INFRA-1000", "INFRA-2", "INFRA-30", "INFRA-4"},
	})
}
