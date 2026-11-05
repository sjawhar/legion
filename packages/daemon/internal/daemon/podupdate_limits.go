package daemon

// The pacing of this daemon's pod update openings: the two limits of the project
// (pod_update_max_open_rounds and pod_update_rounds_per_minute), how much of each one sweep has
// left, this daemon's own record of the openings it has made, and the view of what the last
// sweep decided that the operator's claim listing reads. The round logic itself is in
// podupdate.go, and reaches all of this through the podUpdateSweep each sweep begins before its
// walk over the issues (beginPodUpdateSweep) and carries into every issue of that walk: the walk
// asks the sweep whether a limit holds this issue's opening back (holds) and whether its round is
// an opening of this daemon's own that is still in flight (reasks), and charges an opening that
// reached a worker to it (recordPodUpdateOpening).

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// podUpdateOpeningGap is how soon after its last opening this daemon opens nothing at all. The
// limit the operator sets is a number of rounds a minute, and a sweep is the only thing that
// opens one: the gap is therefore the minute less what a sweep's own arrival wanders by, so two
// sweeps a minute apart that arrive a little early or a little late open their round each,
// while two sweeps of one burst — a sweep that ran long, and the one the ticker fires straight
// after it — do not open two minutes' worth of rounds between them.
const podUpdateOpeningGap = 50 * time.Second

// podUpdateLimit is the limit of the project that held an issue's opening back at a sweep, as
// legion.yaml names it.
type podUpdateLimit string

const (
	limitPodUpdateOpenRounds      podUpdateLimit = "pod_update_max_open_rounds"
	limitPodUpdateRoundsPerMinute podUpdateLimit = "pod_update_rounds_per_minute"
)

// podUpdateHeld is one issue whose update round a limit of the project held back at this sweep.
type podUpdateHeld struct {
	issue string
	limit podUpdateLimit
}

// podUpdateAllowance is how much of one limit a sweep has left, and whether the project sets
// that limit at all: a legion.yaml that names neither key asks for no limit, which no number
// says (config.Config's PodUpdateMaxOpenRounds).
type podUpdateAllowance struct {
	left    int
	limited bool
}

func (a podUpdateAllowance) exhausted() bool { return a.limited && a.left <= 0 }

func (a *podUpdateAllowance) take() {
	if a.limited {
		a.left--
	}
}

// podUpdateSweep is what one sweep may still open under the project's two limits, the issues it
// has held back so far, and the issues it reads as being in a round. slots is how many more
// issues may hold an open round, counted over the whole project at the top of the sweep; budget
// is how many more rounds this one sweep may open. Both are spent only by an opening that
// reached a worker (openPodUpdate), because an ask that reached nobody opens no round.
//
// inFlight is this daemon's own openings whose claims record no round yet, by issue, as
// podUpdateOpenings read them once before the walk. An issue whose round is one of them is that
// opening being asked again rather than a new one (reasks), and the walk reads it here rather
// than taking the openings' own mutex at every issue.
type podUpdateSweep struct {
	slots    podUpdateAllowance
	budget   podUpdateAllowance
	inFlight map[string]string
	waiting  []podUpdateHeld
	inRound  []string
}

// holds is whether a limit of the project holds this issue's opening back, and records the issue
// as waiting under the limit that did. Where both hold it back the limit on open rounds is the
// one recorded: it is the one that will still hold the issue back at the next sweep, and the
// one an operator reading the record has to weigh.
func (sweep *podUpdateSweep) holds(issue string) bool {
	switch {
	case sweep.slots.exhausted():
		sweep.waiting = append(sweep.waiting, podUpdateHeld{issue: issue, limit: limitPodUpdateOpenRounds})
	case sweep.budget.exhausted():
		sweep.waiting = append(sweep.waiting, podUpdateHeld{issue: issue, limit: limitPodUpdateRoundsPerMinute})
	default:
		return false
	}
	return true
}

// charge takes one opening that reached a worker from both limits, and names the issue it was
// made for. The claims this sweep read cannot name it — the post that carries a round to a
// machine is asynchronous — and the sweep's own record says it is in a round all the same
// (recordPodUpdateSweep), as the next sweep's count of the slots does (podUpdateOpenings).
func (sweep *podUpdateSweep) charge(issue string) {
	sweep.slots.take()
	sweep.budget.take()
	sweep.inRound = append(sweep.inRound, issue)
}

// reasks is whether this issue's round is an opening of this daemon's own that is still in
// flight — the sweep that made it charged a slot for it, and the machine it was posted to has
// not recorded it yet — and records the issue as being in a round where it is.
//
// Such an ask is that opening again rather than a second one, and it is a round for both of the
// sweep's readings. No limit holds it back: the slot it is held behind is the one it is itself
// holding (beginPodUpdateSweep), and an issue whose workers the daemon has already told their
// pod is going waits for nothing. Nothing more is charged for it either: charged a second slot,
// it would leave another drifted issue waiting for a slot nothing is using. And the record the
// operator reads names it in a round rather than waiting for one, which is where it is
// (recordPodUpdateSweep).
//
// An entry naming another round says the pod changed under that opening, and the round over the
// pod the claims record now is an opening of its own, charged like any other.
func (sweep *podUpdateSweep) reasks(issue, round string) bool {
	if sweep.inFlight[issue] != round {
		return false
	}
	sweep.inRound = append(sweep.inRound, issue)
	return true
}

// held names every issue a limit held back at this sweep, with the limit that held it, for the
// one line the sweep logs of them (reconcilePodUpdates).
func (sweep podUpdateSweep) held() []string {
	named := make([]string, len(sweep.waiting))
	for i, h := range sweep.waiting {
		named[i] = fmt.Sprintf("%s (%s)", h.issue, h.limit)
	}
	return named
}

// beginPodUpdateSweep is what this sweep may open, read once before its walk over the issues.
//
// A slot is held by an issue whose claims record an open pod round and a pod, whichever pod that
// is: a round is what the slot is for, and a claim of the issue running in a pod again is what
// says the issue is still being carried by it. To those are added the rounds this daemon opened
// at an earlier sweep whose claims have not recorded one yet, which are openings all the same:
// the post that carries a round to a machine is asynchronous and a machine in a relaunch can
// take minutes to take it, so a sweep that counted only the claims would read one slot short
// and open an issue above the limit. Those openings are carried in the sweep, because its walk
// weighs them again at the issue each was made for (podUpdateSweep.reasks).
//
// The sweep's own budget is the whole of the per-minute limit, spent at this one sweep rather
// than paced inside it, and nothing at all within podUpdateOpeningGap of the last opening.
func (s *supervision) beginPodUpdateSweep(now time.Time, byIssue map[string][]supervise.Claim) podUpdateSweep {
	opened, inFlight := s.podUpdateOpenings(byIssue)
	sweep := podUpdateSweep{inFlight: inFlight}
	if rounds := s.cfg.PodUpdateMaxOpenRounds; rounds != nil {
		held := len(inFlight)
		for _, group := range byIssue {
			if holdsOpenRound(group) {
				held++
			}
		}
		sweep.slots = podUpdateAllowance{left: *rounds - held, limited: true}
	}
	if perMinute := s.cfg.PodUpdateRoundsPerMinute; perMinute != nil {
		sweep.budget = podUpdateAllowance{left: *perMinute, limited: true}
		if !opened.IsZero() && now.Sub(opened) <= podUpdateOpeningGap {
			sweep.budget.left = 0
		}
	}
	return sweep
}

// podUpdateOpenings is this daemon's own record of the rounds it has opened: when it last opened
// one, and the openings the claims have not recorded yet, by issue. Nothing of it is in the
// store, so a daemon that has just booted opens at its first sweep whatever the one before it
// opened a moment earlier — one interval of openings at worst, which is what the limits are
// written in.
//
// An opening is dropped from the record as soon as a claim of its issue records that round,
// because the claims then count the slot themselves, and dropped too when no claim of the issue
// records a pod at all: the issue holds no slot then, however the post went, and the next sweep
// reads its pod round from the claims as it reads every other.
//
// The openings are answered rather than counted, because the sweep weighs them twice: once as
// the slots they hold over the whole project, and once at the issue each was made for, whose
// round they say is this daemon's own ask still in flight (beginPodUpdateSweep). What is
// answered is a copy of the sweep's own, so the walk reads it without this mutex.
func (s *supervision) podUpdateOpenings(byIssue map[string][]supervise.Claim) (time.Time, map[string]string) {
	s.podUpdateMu.Lock()
	defer s.podUpdateMu.Unlock()
	for issue, round := range s.podUpdateOpened {
		group := byIssue[issue]
		if recordsRound(group, round) || !recordsAPod(group) {
			delete(s.podUpdateOpened, issue)
		}
	}
	return s.podUpdateOpenedAt, maps.Clone(s.podUpdateOpened)
}

// recordPodUpdateOpening charges one opening that reached a worker to this sweep and to this
// daemon's record of its own openings. The time written is the sweep's own now, the one every
// opening of the sweep shares: the per-minute limit is a budget for a sweep, not a gap between
// the openings inside one, and the sweep that follows measures its gap from the sweep that
// opened rather than from whatever the openings' own replacements took afterwards.
func (s *supervision) recordPodUpdateOpening(sweep *podUpdateSweep, issue, round string, now time.Time) {
	sweep.charge(issue)
	s.podUpdateMu.Lock()
	defer s.podUpdateMu.Unlock()
	if s.podUpdateOpened == nil {
		s.podUpdateOpened = map[string]string{}
	}
	s.podUpdateOpened[issue] = round
	s.podUpdateOpenedAt = now
}

// recordPodUpdateSweep writes what this sweep decided under the project's two limits, whole and
// at once, for the operator's claim listing to read (podUpdateRoundsView, api.PodUpdateRounds).
// It is written after the walk over the issues and before the replacements that walk left,
// because a replacement decides nothing of it and waits its pod out: written behind them, the
// record an operator reads during a fleet-wide roll would be minutes old at every sweep.
//
// An issue is in a round where its claims hold one of the project's slots (holdsOpenRound) or
// where the walk read it in one of this daemon's own openings, which those same claims cannot
// yet say: the post that carries a round to a machine is asynchronous, and the slot is held from
// the opening — this sweep's own (podUpdateSweep.charge), or an earlier sweep's that is still in
// flight (podUpdateSweep.reasks). The walk is in key order, so reading the issues back in that
// order names each of them once and in the order the operator's line prints them.
//
// The waiting issues are the walk's own record of them (podUpdateSweep.holds), where an issue
// both limits held back is already under the limit on open rounds alone.
func (s *supervision) recordPodUpdateSweep(issues []string, byIssue map[string][]supervise.Claim, sweep podUpdateSweep) {
	inRound := map[string]bool{}
	for _, issue := range sweep.inRound {
		inRound[issue] = true
	}
	rounds := api.PodUpdateRounds{
		Swept:           true,
		MaxOpenRounds:   s.cfg.PodUpdateMaxOpenRounds,
		RoundsPerMinute: s.cfg.PodUpdateRoundsPerMinute,
	}
	for _, issue := range issues {
		if inRound[issue] || holdsOpenRound(byIssue[issue]) {
			rounds.InRound = append(rounds.InRound, issue)
		}
	}
	for _, held := range sweep.waiting {
		switch held.limit {
		case limitPodUpdateOpenRounds:
			rounds.WaitingOnMaxOpenRounds = append(rounds.WaitingOnMaxOpenRounds, held.issue)
		case limitPodUpdateRoundsPerMinute:
			rounds.WaitingOnRoundsPerMinute = append(rounds.WaitingOnRoundsPerMinute, held.issue)
		}
	}
	s.podUpdateRoundsMu.Lock()
	defer s.podUpdateRoundsMu.Unlock()
	s.podUpdateRounds = rounds
}

// podUpdateRoundsView is what the last sweep decided, as the operator's claim listing serves it
// (api.Options.PodUpdateRounds). A daemon that has not swept answers the zero record, whose
// Swept says so: its empty lists are what nothing has decided yet, which is another thing than a
// sweep that held no issue back. A record is written whole and never changed afterwards
// (recordPodUpdateSweep), so the lists this hands out are nobody's to write to.
func (s *supervision) podUpdateRoundsView() api.PodUpdateRounds {
	s.podUpdateRoundsMu.Lock()
	defer s.podUpdateRoundsMu.Unlock()
	return s.podUpdateRounds
}

// holdsOpenRound is whether this issue holds one of the project's open-round slots: its claims
// record an open pod round, and one of them records a pod to carry it in.
func holdsOpenRound(group []supervise.Claim) bool {
	return openPodRound(group) != "" && recordsAPod(group)
}

// recordsAPod is whether any claim of the group records a pod, whichever pod that is: two pods
// are a replacement in flight, which is a round being carried like any other.
func recordsAPod(group []supervise.Claim) bool {
	pod, split := groupPod(group)
	return split || pod != ""
}

// recordsRound is whether any claim of the group records round, answered or not and open or not.
func recordsRound(group []supervise.Claim, round string) bool {
	return slices.ContainsFunc(group, func(c supervise.Claim) bool { return memberOf(c, round) })
}
