package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reconcilePodUpdates carries every update round of this project one step, over the claims the
// orphan sweep already read this tick. A running issue pod whose configuration has drifted is
// never replaced where it is found: that ends every role process in it mid-turn and deletes the
// pod's disk with everything unpushed on it. The daemon opens a round over the pod instead — each
// worker claim of the issue is given a task saying "push your work and answer" (supervise's
// PodUpdateOpen, api's claims/ready-for-update) — and replaces the pod only once each has
// answered.
//
// The round is the issue's, not the locator's: a suspended claim's locator is nil (supervise's
// letGo), and no pod disk may be deleted before every worker in it, suspended ones included, has
// signalled. Every role claim of one issue shares that issue's one Sandbox and pod, so the issue
// is what groups them. A group whose claims' locators name two different pods is left alone this
// tick: a replacement is already in flight, and the next tick reads it settled.
//
// Two kinds of claim are left out of every group, and so out of every decision below. A retired
// claim runs nothing on any pod: it holds no process, it blocks no round, and a round an earlier
// release left recorded on it names a pod that is long gone — read beside a live claim's pod it
// would say, at every tick, that this issue's pod went without its round. A claim with no issue
// (the project controller's) is in no round at all: its pod has no issue workspace and nothing to
// push, and its plugin contract is carried by the stale-plugin relaunch instead
// (supervise's refreshPlugin).
//
// Everything a round decides comes from the durable claim columns — the round, its reasons, when
// it opened, when each worker was asked, when each answered, and when the operator's side was
// told. A daemon restart therefore resumes a round exactly where it stood, and the same record
// gives the same decision: this pass holds no memory of its own between ticks.
func (s *supervision) reconcilePodUpdates(ctx context.Context, claims []supervise.Claim) {
	now := s.plan.clock.Now()
	byIssue := map[string][]supervise.Claim{}
	var issues []string
	for _, c := range claims {
		if c.Issue == "" || c.State == supervise.StateRetired {
			continue
		}
		if _, grouped := byIssue[c.Issue]; !grouped {
			issues = append(issues, c.Issue)
		}
		byIssue[c.Issue] = append(byIssue[c.Issue], c)
	}
	slices.Sort(issues)
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		s.reconcileIssuePodUpdate(ctx, issue, byIssue[issue], now)
	}
}

// reconcileIssuePodUpdate carries one issue's pod round a step. group is every claim of the issue
// that has not retired, whatever state it is in.
//
// The decisions are taken in this order, and the order is the rule. A pod the group has replaced
// with another went while its round was open, whatever the runtime says of a pod that is not
// there. Then the drift, which is also what says the pod is not there: a round open over a pod
// the runtime no longer has ends at that reading, and a pod that has drifted from nothing and
// has no round open over it needs no replacement, as does one whose round's own pod is already
// gone, which ends that round over the pod that replaced it. A round open over the pod the
// claims still record goes on, drift or no drift: once opened, a round ends only by replacing
// its pod, or because that pod is gone. Only then is a complete round acted on, and only then is
// a round opened or kept open.
//
// A group whose claims record no pod at all decides nothing: not that the pod went, and not that
// it has drifted from nothing. No locator is no evidence either way — every claim of the issue
// may be suspended (letGo clears the locator), or the one live claim may be minutes into a
// relaunch — and the round waits for a claim to record a pod again. The one thing that says a
// pod went is a different pod in its place.
//
// A group whose claims record a pod the runtime no longer has ends the round open over it at
// that sweep, and decides nothing else (groupDrift, podGone, endRoundOverAGonePod). It is the
// one place a round ends on something other than its own replacement, and it ends there rather
// than at the tick that reads the claims recording the pod that replaced this one: inside that
// window every role is relaunched into the new pod, and each of those processes' turns and
// answers is counted by its machine against the old pod's round. The issue would then be told
// work was lost where every worker had answered, or told nothing where work really was lost.
func (s *supervision) reconcileIssuePodUpdate(ctx context.Context, issue string, group []supervise.Claim, now time.Time) {
	slices.SortFunc(group, func(a, b supervise.Claim) int { return strings.Compare(string(a.Token), string(b.Token)) })
	pod, split := groupPod(group)
	if split {
		s.log.Info("supervise: the claims of this issue run in two pods; their update round waits for the replacement in flight",
			"issue", issue)
		return
	}
	open := openPodRound(group)
	if pod == "" {
		if open != "" {
			s.log.Info("supervise: no claim of this issue records a pod; its update round waits for one to run again",
				"issue", issue, "round", open)
			s.tellPodlessRound(ctx, issue, group, open, now)
			return
		}
		// Nothing can be read of a pod no claim records, so no pod round opens and none ends. A
		// claim's own plugin round is carried all the same: it is one claim's process, not the
		// pod's, and a suspended claim in one records no pod either.
		s.reconcilePluginUpdates(ctx, group, now)
		return
	}
	// The pod went without the round: it was replaced where it stood for a failure of its own — a
	// terminal pod, a failed init container, a superseded disk, a lost launcher binding — and its
	// disk went with it while a worker still had work to push. The claims record the pod that
	// replaced it, which is the evidence. A round every blocking claim answered lost nothing, and
	// ends below with the drift or the replacement.
	if open != "" && open != supervise.PodRound(pod) && len(waitingClaims(group, open)) > 0 {
		s.podWentWithoutItsRound(ctx, issue, group, open, pod)
		return
	}
	reasons, reading := s.groupDrift(ctx, issue, group)
	if reading == podGone {
		s.endRoundOverAGonePod(ctx, issue, group, open, pod)
		return
	}
	if reading != podRead {
		return
	}
	if len(reasons) == 0 && open != supervise.PodRound(pod) {
		// The round's own pod is gone: the claims record another one in its place, and that pod
		// has drifted from nothing. A round that still had a worker to lose work with the pod
		// was taken by the branch above, so this one lost nothing and ends here, over the pod
		// that replaced its own.
		if open != "" {
			s.log.Info("supervise: the pod this issue's update round was opened over is gone, and the pod in its place has drifted from nothing; the round is over",
				"issue", issue, "round", open, "pod", pod)
			s.endPodUpdates(group)
		}
		s.reconcilePluginUpdates(ctx, group, now)
		return
	}
	joined := strings.Join(reasons, ", ")
	if len(reasons) == 0 {
		// The drift this round was opened for has gone away — an operator rolled the release
		// back — and the round replaces the pod all the same. The round has already asked every
		// worker in that pod to push its work and to start no new work, each has answered or is
		// about to, and no path is left that lets a worker carry on in the pod it was told would
		// be replaced. Replacing it is one restart of each worker in its own session, which the
		// alternative — a release of the answered workers that the daemon alone knows about —
		// is not.
		//
		// What the task the worker reads and the notice the operator reads name is the round's
		// own recorded reasons: the live drift says nothing now, and both of them must say why
		// the round was opened.
		joined = roundReasons(group, open)
	}
	// One live read, given to both decisions below, so the claims the replacement waits on and
	// the claims the post asks again are one set. Read live by the replacement and from the
	// snapshot by the post, a claim whose session registered after the tick's read would block
	// the round in the one and be no worker of it in the other: the replacement would wait on
	// that claim for an answer the post never asked it for, and the round would run out its
	// notice bound waiting for a worker nothing had asked anything of.
	live := s.liveGroup(group)
	if open != "" && s.completePodUpdate(ctx, issue, group, live, open, pod, joined, now) {
		return
	}
	s.openPodUpdate(ctx, issue, live, pod, joined, now)
}

// completePodUpdate replaces the pod and ends the round when the round is over: every claim that
// blocks it has answered, and no claim of the group runs a turn. Waiting for no claim to be
// working is what keeps the promise that no worker is stopped mid-turn — a worker answers from
// inside a turn, and that turn ends a moment later. It reports whether it decided the round.
//
// Both waits are read from the claims as their machines hold them now (live), never from the
// sweep's snapshot, because this is where a disk is deleted. The snapshot is the orphan sweep's,
// read once at the start of the tick, and every earlier issue's replacement in that same tick can
// take up to one sweep interval, so after a fleet-wide roll it is minutes old by the time this
// issue is reached. A turn that starts inside that window takes the worker's answer back in its
// machine alone (takeBackSignal); a sweep deciding from the snapshot would see neither the turn
// nor the answer going, and would end that turn and delete its disk. A round the fresh read finds
// unanswered is simply not complete: this returns false, and the post below asks that worker
// again. The snapshot is kept for one thing only: saying, in the log line, that this is the race
// rather than a worker that never answered.
//
// Even that read is a read before the act, so the replacement carries its own last word into the
// runtime (replacementDue): ReplacePod takes the pod's launch turn and waits it out, and a round
// complete when this call began is a round a worker can start a turn in while it waits. The
// runtime asks one statement before the write that ends every process in the pod, and a
// replacement no longer due is refused there, ends nothing and leaves the round open for the next
// sweep to decide from claims it reads afresh.
//
// ReplacePod is called once, with the process of a claim that records this round's own pod; a
// round whose pod is already gone replaces nothing and only ends, and a group whose claims now
// run in two pods is left to the tick that reads the replacement in flight settled. A failed
// ReplacePod is logged and retried at the next tick: nothing is ended, so the round still holds
// every worker's next task.
//
// Neither wait is silent past the round's notice bound. A turn that never ends and a ReplacePod
// that keeps failing each hold every answered worker's next task for as long as they last, which
// is the same stall as a worker that never answers, so each is told to the operator's side the
// same way (tellUpdateStalled) and names what the round is waiting on. One round is told once.
//
// The call is bounded to one sweep interval, because it is the only thing in this loop that waits
// on a pod: it takes that pod's launch turn and then waits the pod out, up to
// worker_boot_timeout_seconds, while every other issue's round, this tick's stale-plugin re-posts
// and the orphan reaping behind it wait. The bound costs the round nothing. The suspension that
// displaces the pod has already landed when it hits, so the next sweep reads the Sandbox
// suspended and naming that very pod as the one it displaced, which is this round's own
// replacement in flight: PodDrift answers for it as it does for a pod that is there and has not
// drifted, and the round is finished here, where only the pod's end is left to wait for. Once
// the pod itself is gone, that sweep reads it gone and the round ends there instead, with
// nothing lost — every claim that blocks the round had answered before this call was made — so
// nothing is deleted earlier than it would have been.
func (s *supervision) completePodUpdate(ctx context.Context, issue string, snapshot, live []supervise.Claim, round, pod, reasons string, now time.Time) bool {
	if waiting := waitingClaims(live, round); len(waiting) > 0 {
		if len(waitingClaims(snapshot, round)) == 0 {
			s.log.Info("supervise: a worker of this update round answered and then started a turn of its own; the replacement waits for its next answer",
				"issue", issue, "round", round, "waiting", namesOf(waiting))
		}
		return false
	}
	if working := workingClaims(live); len(working) > 0 {
		s.log.Info("supervise: every worker of this update round has answered; the replacement waits for the turns still running",
			"issue", issue, "round", round, "working", working)
		s.tellLateRound(ctx, issue, live, round, pod, reasons, now,
			fmt.Sprintf("every worker of the update round has answered, and the turns of %s have not ended",
				strings.Join(working, ", ")))
		return true
	}
	if _, split := groupPod(live); split {
		s.log.Info("supervise: the claims of this issue run in two pods now; this round's replacement waits for the one in flight",
			"issue", issue, "round", round, "pod", pod)
		return true
	}
	if k, running := podProcess(live, round); running {
		bounded, done := context.WithTimeout(ctx, s.plan.orphanSweep)
		defer done()
		if err := s.runtime.ReplacePod(bounded, k, s.replacementDue(issue, round)); err != nil {
			if errors.Is(err, errReplacementNotDue) {
				s.log.Info("supervise: this update round's replacement was no longer due when its pod's launch turn came; nothing was replaced and the round stays open",
					"issue", issue, "round", round, "pod", pod, "reason", err)
				return true
			}
			if ctx.Err() == nil {
				s.log.Error("supervise: replace the pod of a finished update round; retrying at the next sweep",
					"issue", issue, "round", round, "pod", pod, "error", err)
				s.tellLateRound(ctx, issue, live, round, pod, reasons, now,
					fmt.Sprintf("every worker of the update round has answered, and the pod replacement fails: %v", err))
			}
			return true
		}
	}
	s.log.Info("supervise: every worker of this update round has answered; the pod is replaced and the round is over",
		"issue", issue, "round", round, "pod", pod)
	s.endPodUpdates(live)
	return true
}

// errReplacementNotDue is every answer replacementDue gives that is not "go on": the round the
// sweep found complete is not complete now, or the claims could not be read to say that it is.
// The runtime hands it back from inside ReplacePod having written nothing (runtime.Runtime), and
// completePodUpdate tells it from a replacement that failed by this error alone — a failure is a
// stall the operator's side is told of, and this is the daemon's own refusal, which costs the
// round one sweep and nothing else.
var errReplacementNotDue = errors.New("the pod replacement is no longer due")

// replacementDue is the daemon's last word on a replacement, which the runtime asks under the
// pod's launch turn and one statement before the write that ends every process in that pod. It
// re-runs the round's own two blocking checks — every claim that blocks the round has answered,
// and no claim of the issue runs a turn — over the issue's claims as they are at that instant,
// and refuses the replacement when either fails.
//
// Which claims exist is read from the store, not from the group this sweep carries: a claim
// created after the sweep's snapshot is a claim the group never held, and the next phase's role,
// launched into the very pod this round keeps, is exactly that claim. Ending the pod under it
// would cut a turn the round never asked anything of.
//
// What each claim holds is read from its machine's published view (Machine.View), never from
// Machine.Claim: Claim waits out the decision in flight, and the decision in flight can be the
// launch or the suspension that is waiting on the very pod lock this call is made under, which
// would deadlock the daemon's sweep against its own runtime. The view is at least as fresh as the
// store for a claim a machine holds — it is what the latest decision left, or what the decision
// in flight last tried to write — and the store's row stands for a claim no machine of this
// daemon holds.
func (s *supervision) replacementDue(issue, round string) func(context.Context) error {
	return func(ctx context.Context) error {
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			return fmt.Errorf("%w: the claims of %s cannot be read: %w", errReplacementNotDue, issue, err)
		}
		group := s.viewedGroup(claims, issue)
		if waiting := waitingClaims(group, round); len(waiting) > 0 {
			return fmt.Errorf("%w: %s has not answered it", errReplacementNotDue, strings.Join(namesOf(waiting), ", "))
		}
		if working := workingClaims(group); len(working) > 0 {
			return fmt.Errorf("%w: the turn of %s runs now", errReplacementNotDue, strings.Join(working, ", "))
		}
		return nil
	}
}

// viewedGroup is every claim of the issue a round weighs, as the store names them and as each
// machine last published it: the store says which claims there are, and roundGroup says what
// each holds now, with the answer of a claim whose machine is deciding masked, as the decision
// the replacement rests on needs it.
func (s *supervision) viewedGroup(claims []supervise.Claim, issue string) []supervise.Claim {
	var of []supervise.Claim
	for _, c := range claims {
		if c.Issue == issue {
			of = append(of, c)
		}
	}
	return s.roundGroup(of, true)
}

// liveGroup is the group as its claims' own machines hold them now, in the group's order, with
// the same mask: it is what the sweep's own decision to replace the pod is read from.
func (s *supervision) liveGroup(group []supervise.Claim) []supervise.Claim {
	return s.roundGroup(group, true)
}

// noticeGroup is the group as the operator's side is told of it: the same fresh read, unmasked.
// The mask is a decision the replacement makes — a claim whose machine is deciding has not
// answered, for the purpose of deleting a disk — and it is not a fact about the claim. Told to
// the controller it would be one: an answered claim marked `Answered: false` is one the
// controller's playbook publishes the update task to, and that publish starts a turn, which
// takes the real answer back (skill://legion-controller/references/escalation.md).
func (s *supervision) noticeGroup(group []supervise.Claim) []supervise.Claim {
	return s.roundGroup(group, false)
}

// roundGroup is the claims of group as a round reads them now, less the ones no round weighs.
// It is the one read the sweep's decision, the replacement's last word and the operator's notice
// are all built from, so no two of them can disagree about what a claim holds.
//
// What it reads is each machine's published view (Machine.View), never Machine.Claim: Claim
// waits out the decision in flight, and that decision can be a Sandbox relaunch that holds the
// machine for minutes. This runs for every drifted issue on every tick, so one relaunching claim
// would otherwise stall the whole sweep behind it — the orphan reaping, the stale-plugin posts
// and every later issue's round. The view is at least as fresh as the store for a claim a
// machine holds, and the store's row stands for a claim no machine of this daemon holds.
//
// A retired claim is weighed by no round: it runs nothing in any pod.
//
// mask is whether a claim whose machine is deciding something now (Machine.Deciding, which waits
// on nothing either) is read as not having answered. The view of such a claim is what the
// decision in flight last tried to write, and the decision may be the turn that takes the answer
// back (takeBackSignal), so a reader about to delete a disk masks it: the cost is one sweep, and
// the cost of the other way is work that turn can still commit. A reader that only reports masks
// nothing (noticeGroup).
func (s *supervision) roundGroup(group []supervise.Claim, mask bool) []supervise.Claim {
	read := make([]supervise.Claim, 0, len(group))
	for _, c := range group {
		viewed := c
		if m, ok := s.supervisor.Machine(c.Token); ok {
			viewed = m.View().Claim
			if mask && m.Deciding() != nil {
				viewed.UpdateSignalledAt, viewed.UpdateHead = nil, ""
			}
		}
		if viewed.State != supervise.StateRetired {
			read = append(read, viewed)
		}
	}
	return read
}

// tellLateRound tells the operator's side of a round that has answered everything the daemon
// asked of it and still has not replaced its pod, once the notice bound has passed since the last
// of those answers. why is the one line that says what it waits on. The issue is written nothing:
// there is no instruction for its roles here — each has already pushed and answered — and the
// operator reads the round through the controller's notice.
//
// The bound runs from the last answer, not from the round's opening, because what this notice
// reports began with that answer: before it, the round was waiting for a worker, which is the
// other notice's business. A round whose last worker answers an hour before the opening bound
// would otherwise spend its one notice on a turn that ends seconds later, and the stall that
// follows would be told nothing. A round the claims record no answer for at all falls back to the
// opening, which is the only clock it has.
//
// Every claim listed has answered by now (waitingClaims is empty), so the notice carries the
// blocking claims with that mark on each (record.UpdateWaiting.Answered): the controller's
// playbook publishes the update task to a claim that has not answered, and a publish starts a
// turn, which takes the answer back (takeBackSignal). Telling it to do that here would undo
// every answer the round is waiting on the daemon's own side to use.
func (s *supervision) tellLateRound(ctx context.Context, issue string, group []supervise.Claim, round, pod, reasons string, now time.Time, why string) {
	since := roundAnsweredAt(group, round)
	if since == nil {
		since = roundOpenedAt(group, round)
	}
	if since == nil || now.Sub(*since) < s.cfg.PodUpdateNotice {
		return
	}
	s.tellUpdateStalled(ctx, updateStall{
		issue: issue, round: round, pod: pod, reasons: reasons, group: group,
		waiting: waitingOf(round, blockingClaims(group)),
		reason:  why,
	})
}

// tellPodlessRound tells the operator's side of a round whose issue records no pod at all, once
// that round is older than the notice bound and a worker of it has still not answered. Nothing
// of such a round can move: no drift can be read, no pod can be replaced, and it waits for one of
// the issue's workers to run again. Where every claim of the issue is suspended — a lone worker
// an operator suspended is the common shape, since a suspension lets the process go (letGo clears
// the locator) — only that operator can end the wait, and no other path here would ever tell
// them: the pod round's own notice is written where a pod is recorded, and this round reaches
// neither it nor a replacement.
//
// A round every claim of which has answered is told nothing, and that is the ordinary end of a
// phase rather than a stall: the workers answered, the daemon suspended each of them, and letGo
// took the last locator with the last process. Nobody has to do anything about such a round —
// the pod it was opened over is replaced at the next launch into the issue, which provisions a
// fresh one — so telling the controller only opens a finding ask over an operator's ordinary
// suspension. The notice is for the workers that have not answered, and lists those alone.
func (s *supervision) tellPodlessRound(ctx context.Context, issue string, group []supervise.Claim, round string, now time.Time) {
	waiting := waitingClaims(group, round)
	if len(waiting) == 0 {
		return
	}
	opened := roundOpenedAt(group, round)
	if opened == nil || now.Sub(*opened) < s.cfg.PodUpdateNotice {
		return
	}
	s.tellUpdateStalled(ctx, updateStall{
		issue: issue, round: round, reasons: roundReasons(group, round), group: group,
		waiting: waitingOf(round, waiting),
		reason: fmt.Sprintf("no claim of this issue records a pod, so the update round has waited %s for %s to run again and answer",
			now.Sub(*opened).Round(time.Minute), strings.Join(namesOf(waiting), ", ")),
	})
}

// podWentWithoutItsRound is the round's pod gone before the round could finish: the message the
// issue's spec requires is written on the issue, naming what was lost, and the round ends.
// Nothing here can bring the work back — the disk went with the pod — so the message is the whole
// of it.
//
// Two readings say the pod went, and each calls this with the claims it decided from. The claims
// recording a pod other than the round's own is one (reconcileIssuePodUpdate), and the runtime
// answering that the pod is not there is the other (endRoundOverAGonePod). What was lost is read
// from the group this is given, so the caller's own reading of the claims is the one the issue
// is told of and the one the round ends on.
func (s *supervision) podWentWithoutItsRound(ctx context.Context, issue string, group []supervise.Claim, round, pod string) {
	lost := waitingClaims(group, round)
	reasons := roundReasons(group, round)
	s.log.Warn("supervise: the pod of an open update round is gone; the roles that had not answered lost what they had not pushed",
		"issue", issue, "round", round, "pod", pod, "lost", namesOf(lost))
	if err := s.writeOnIssue(ctx, issue, podWentMessage(round, reasons, lost)); err != nil {
		s.log.Error("supervise: write on the issue that its pod went without its update round",
			"issue", issue, "round", round, "error", err)
	}
	s.endPodUpdates(group)
}

// endRoundOverAGonePod ends the round the sweep has just read the pod of as gone. The report is
// decided here and the round ends here, at the first sweep that reads it: a round left open over
// a pod that is not there is relaunched into the pod that replaced it one role at a time, and
// every turn and every answer of those processes is counted by its machine against the round of
// a pod they never ran in.
//
// What was lost is the claims of the round that had not answered by now, so a round every
// blocking claim answered lost nothing and is told to nobody: the workers pushed everything they
// wanted kept before the pod went, and a message naming no role would say an issue lost work it
// did not lose.
//
// Those answers are read from the machines as they stand rather than from the tick's snapshot,
// and unmasked (noticeGroup): an answer recorded is an answer whatever its machine is deciding
// now, and what this decides is a report of what is already gone, never the deletion of a disk —
// which is what the mask exists for (roundGroup).
//
// A pod that is not there with no round open over it decides nothing, as a group whose claims
// record no pod at all does: there is no round to end, the drift of a pod that is not there says
// nothing, and the next launch into the issue builds a pod from the configuration the runtime
// has now.
func (s *supervision) endRoundOverAGonePod(ctx context.Context, issue string, group []supervise.Claim, round, pod string) {
	if round == "" {
		s.log.Info("supervise: the pod this issue's claims record is not there, and no update round is open over it; nothing is decided of this issue",
			"issue", issue, "pod", pod)
		return
	}
	told := s.noticeGroup(group)
	if len(waitingClaims(told, round)) > 0 {
		s.podWentWithoutItsRound(ctx, issue, told, round, pod)
		return
	}
	s.log.Info("supervise: the pod of this issue's update round is not there, and every claim of the round had answered; nothing was lost and the round is over",
		"issue", issue, "round", round, "pod", pod)
	s.endPodUpdates(told)
}

// openPodUpdate opens the round, or keeps it open: every claim that blocks it is posted
// PodUpdateOpen at every tick while the round runs. The event is idempotent and carries the
// re-task clock itself (supervise's openPodUpdate), so the sweep states the round and the machine
// decides whether this is the first ask or the next one, pod_update_retask_hours after the last.
//
// A claim that blocks the round has a session — an agent has run for it, and may hold unpushed
// work. A claim with no session has never had a worker and blocks nothing. A failed or
// operator-suspended claim blocks until the operator acts, which is this issue's own rule, and
// the notice below is what tells the operator. A worker whose plugin has no ready_for_update op
// is asked like any other and answers by ending the turn its task starts (supervise's
// answerByTurnEnd), so no claim of the round is one the operator must act on for its contract.
func (s *supervision) openPodUpdate(ctx context.Context, issue string, group []supervise.Claim, pod, reasons string, now time.Time) {
	round := supervise.PodRound(pod)
	blocking := blockingClaims(group)
	task := supervise.UpdateTask(round, reasons)
	for _, c := range blocking {
		s.supervisor.post(c.Token, supervise.PodUpdateOpen{Claim: c.Token, Round: round, Reasons: reasons, Task: task})
	}
	// The round is told of only once a claim records it: the operator reads the round on the
	// claims the notice names, the mark that tells it once is a claim's own (UpdateToldAt), and
	// the posts above are what write both. The tick that opens a round therefore tells nothing,
	// and the next one tells what it finds — the round's own bound is hours, and the stall that
	// waits for no bound waits one sweep.
	opened := roundOpenedAt(group, round)
	if opened == nil {
		return
	}
	if now.Sub(*opened) < s.cfg.PodUpdateNotice {
		return
	}
	// The notice is built unmasked: the mask is the replacement's own caution about a claim
	// whose machine is deciding, and told to the controller it would read as a claim that
	// has not answered, which its playbook prompts (noticeGroup).
	told := s.noticeGroup(group)
	waiting := waitingClaims(told, round)
	if len(waiting) == 0 {
		// Unmasked, every claim of the round has answered, so this round is not stalled at
		// all: the only claim the sweep's own read counted as waiting was counted so because
		// its machine was deciding at that moment (roundGroup's mask), and the round is one
		// decision away from complete. A notice here would carry an empty waiting list, which
		// the store refuses — an update-stalled notice requires its round and the claims it
		// waits on (record.UpdateStalled) — and a refused row leaves the round unmarked, so
		// every tick until that decision ends would build the same row, be refused again and
		// log it at Error. The next sweep reads the decision settled: it replaces the pod, or
		// it tells the round then if it is waiting after all. tellPodlessRound returns on an
		// empty list of its own for the same reason.
		return
	}
	s.tellUpdateStalled(ctx, updateStall{
		issue: issue, round: round, pod: pod, reasons: reasons, group: told,
		waiting: waitingOf(round, waiting),
		reason: fmt.Sprintf("the update round has waited %s for its workers to answer",
			now.Sub(*opened).Round(time.Minute)),
	})
}

// reconcilePluginUpdates carries the plugin round of every claim of a group that is in one. It
// runs only where no pod round is open over the group: a pod round takes every claim of the issue,
// a plugin round's claim among them, and replacing the pod is what that claim was waiting for
// anyway.
func (s *supervision) reconcilePluginUpdates(ctx context.Context, group []supervise.Claim, now time.Time) {
	for _, c := range group {
		if c.InPluginRound() {
			s.reconcilePluginUpdate(ctx, c, now)
		}
	}
}

// reconcilePluginUpdate carries one claim's own plugin round: the round supervise's refreshPlugin
// opens over a claim whose process speaks a plugin contract this daemon does not. The sweep is
// what ends it — once the worker has answered and runs no turn, PodUpdateEnded replaces the
// process onto this daemon's plugin, which is what the round was opened for. Until then the claim
// is asked again on the round's own re-task clock, and the round's notice and bounds are the pod
// round's.
//
// The end is posted for a claim whose process is gone too — suspended, launching, queued — and
// there it relaunches nothing (supervise's endPodUpdate): the round has nothing left to wait for,
// and the claim's next launch installs this daemon's plugin itself. Relaunching such a claim
// would start a process an operator's suspension had stopped, which only the operator's resume
// may do.
//
// An answered round that does not end is the pod round's own late stall, one claim wide, and is
// told the same way (tellLateRound): the turn the relaunch waits for may never end, and the
// relaunch the end posts may be refused for as long as the refusal lasts (a reserved tree
// cleanup, a moved epoch), which leaves the round open, the claim's next task held behind it and
// nothing but a log line per tick to show for it. The bound runs from the answer, as the pod
// round's does, and one round is told once.
func (s *supervision) reconcilePluginUpdate(ctx context.Context, c supervise.Claim, now time.Time) {
	round := c.UpdateRound
	if c.UpdateSignalledAt != nil {
		waits := "its process is not replaced"
		switch {
		case c.State == supervise.StateWorking:
			s.log.Info("supervise: this claim's plugin round has its answer; the relaunch waits for the turn still running",
				"claim", c.Token, "round", round)
			waits = "its turn has not ended"
		case !supervise.HoldsCapability(c.State):
			s.log.Info("supervise: this claim's plugin round has its answer, and its process is gone; the round ends and launches nothing",
				"claim", c.Token, "round", round, "state", c.State)
		default:
			s.log.Info("supervise: this claim's plugin round has its answer; its process is replaced onto this daemon's plugin",
				"claim", c.Token, "round", round)
		}
		if c.State != supervise.StateWorking {
			s.supervisor.post(c.Token, supervise.PodUpdateEnded{Claim: c.Token, Round: round})
		}
		s.tellLateRound(ctx, c.Issue, []supervise.Claim{c}, round, "", c.UpdateReasons, now,
			"the worker of the plugin round has answered, and "+waits)
		return
	}
	s.supervisor.post(c.Token, supervise.PodUpdateOpen{
		Claim: c.Token, Round: round, Reasons: c.UpdateReasons, Task: supervise.UpdateTask(round, c.UpdateReasons),
	})
	late := c.UpdateOpenedAt != nil && now.Sub(*c.UpdateOpenedAt) >= s.cfg.PodUpdateNotice
	if c.UpdateToldAt != nil || !late {
		return
	}
	s.tellUpdateStalled(ctx, updateStall{
		issue: c.Issue, round: round, reasons: c.UpdateReasons, group: []supervise.Claim{c},
		waiting: waitingOf(round, []supervise.Claim{c}),
		reason: fmt.Sprintf("the plugin round has waited %s for its worker to answer",
			now.Sub(*c.UpdateOpenedAt).Round(time.Minute)),
	})
}

// updateStall is one round the sweep cannot finish, as the operator's side is told of it: the
// issue and round it is of, the pod it was opened over (none for a plugin round), what the round
// was opened for, the claims it waits on, the one-line why, and the group whose claims mark the
// round told. Nothing is written on the issue: a round that stalls asks nothing of its workers
// that they have not already been asked, and the operator reads it through the controller.
//
// reasons is the one string the round records and the task states (supervise.Claim.UpdateReasons),
// never a list: each reason carries ", " inside it already, so nothing here may cut one up
// (record.UpdateStalled).
type updateStall struct {
	issue   string
	round   string
	pod     string
	reasons string
	waiting []record.UpdateWaiting
	reason  string
	group   []supervise.Claim
}

// tellUpdateStalled tells the controller that a round is not finishing. One round is told once
// however many ticks find it open: the telling is marked on every claim of the round
// (UpdateToldAt), and a round already marked is told nothing more. The mark is written after the
// row is enqueued, so a row the store refuses is written again at the next tick rather than
// swallowed.
func (s *supervision) tellUpdateStalled(ctx context.Context, stall updateStall) {
	if roundTold(stall.group, stall.round) {
		return
	}
	if s.enqueue == nil {
		s.log.Warn("supervise: an update round is stalled, and this deployment has no workflow to tell the operator's side through",
			"issue", stall.issue, "round", stall.round, "reason", stall.reason)
		return
	}
	rows := []record.OutboxPayload{record.ControllerNotice{
		Kind:   record.UpdateStalledNotice,
		Reason: stall.reason,
		Update: &record.UpdateStalled{Round: stall.round, Pod: stall.pod, Reasons: stall.reasons, Waiting: stall.waiting},
	}}
	if err := s.enqueue(ctx, stall.issue, rows...); err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: tell the operator's side that an update round is stalled; telling again at the next sweep",
				"issue", stall.issue, "round", stall.round, "error", err)
		}
		return
	}
	s.log.Warn("supervise: an update round is stalled; the controller is told", "issue", stall.issue, "round", stall.round,
		"pod", stall.pod, "reason", stall.reason, "waiting", stall.waiting)
	s.markUpdateTold(ctx, stall.group, stall.round)
}

// markUpdateTold records on every claim of the round that this round was told, so no later tick
// tells it again. A claim whose machine the daemon no longer holds, or whose round has moved on,
// marks nothing (supervise's Machine.PodUpdateTold).
func (s *supervision) markUpdateTold(ctx context.Context, group []supervise.Claim, round string) {
	for _, c := range group {
		if c.UpdateRound != round {
			continue
		}
		m, ok := s.supervisor.Machine(c.Token)
		if !ok {
			continue
		}
		if _, err := m.PodUpdateTold(ctx, round); err != nil && ctx.Err() == nil {
			s.log.Error("supervise: record that this claim's update round was told", "claim", c.Token, "round", round, "error", err)
		}
	}
}

// writeOnIssue posts one message on the issue, through the outbox's Dispatch message row.
func (s *supervision) writeOnIssue(ctx context.Context, issue, body string) error {
	if s.enqueue == nil {
		return fmt.Errorf("write on %s: this deployment has no workflow to write through", issue)
	}
	return s.enqueue(ctx, issue, record.MessagePost{Body: body})
}

// endPodUpdates ends the round on every claim of the group that is in one. A round ends only
// where the pod it was opened over is gone — replaced by the round itself, replaced under it, or
// gone for a failure of its own — so the post carries nothing of that pod's fate
// (supervise.PodUpdateEnded).
func (s *supervision) endPodUpdates(group []supervise.Claim) {
	for _, c := range group {
		if c.UpdateRound == "" {
			continue
		}
		s.supervisor.post(c.Token, supervise.PodUpdateEnded{Claim: c.Token, Round: c.UpdateRound})
	}
}

// podReading is what one sweep could read of the pod the group's claims record, and the one
// thing that says what the reasons beside it are worth (groupDrift). The three answers are
// decisions, not degrees of one: a pod that has drifted from nothing is still running and its
// round replaces it, a pod that cannot be read decides nothing at all, and a pod that is not
// there has taken its disk with it and ends the round open over it. No two of them may be read
// from the same empty list of reasons, which is why this is an answer of its own rather than a
// flag a caller may drop.
//
// podUnread is the zero value, so a reading nobody set decides nothing: of the three, it is the
// only one that is safe to be wrong about.
type podReading int

const (
	// podUnread is the runtime refusing the question: an error is never a statement that
	// something drifted, and a round must neither open nor end on one.
	podUnread podReading = iota
	// podRead is the pod being there and read, and only under it do the reasons beside this
	// reading mean anything: no reason at all is then a pod that has drifted from nothing.
	podRead
	// podGone is the pod the claims record not being there (runtime.ErrPodGone): it was replaced
	// where it stood for a failure of its own, or it is on its way out for an act that is not
	// this round's, and whatever was unpushed on its disk went with it.
	podGone
)

// groupDrift is why the group's pod has drifted: the union of the runtime's PodDrift over every
// claim of the group that records a process, in the claims' own order and without repeats, with
// the reading that says what those reasons are worth (podReading).
//
// It reads podUnread when the runtime could not be asked at all: an error is never a statement
// that something drifted, and a round must neither open nor end on one.
//
// The pod the claims record not being there is the one error of its own (runtime.ErrPodGone),
// and it reads podGone, which is a statement rather than a failure: nothing more can be read of
// that pod, and the round open over it is over with it (endRoundOverAGonePod). Read as a pod
// that has drifted from nothing, it would carry the round on over a pod that took its workers'
// unpushed work with it. Read as nothing at all, it would leave that round open while the
// displaced path relaunches each role into the pod that replaced this one, and every turn and
// answer of those processes would be counted against a round their pod never ran in. The caller
// is what logs it, because the caller is what decides it.
func (s *supervision) groupDrift(ctx context.Context, issue string, group []supervise.Claim) ([]string, podReading) {
	var reasons []string
	for _, c := range group {
		if c.Locator == nil {
			continue
		}
		drift, err := s.runtime.PodDrift(ctx, runtime.Known{Claim: c.Token, Locator: c.Locator})
		if errors.Is(err, runtime.ErrPodGone) {
			return nil, podGone
		}
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("supervise: read whether this issue's pod has drifted", "issue", issue, "claim", c.Token, "error", err)
			}
			return nil, podUnread
		}
		for _, reason := range drift {
			if !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		}
	}
	return reasons, podRead
}

// groupPod is the pod every claim of the group that records one runs in, and whether they record
// two different pods, which is a replacement already in flight.
func groupPod(group []supervise.Claim) (string, bool) {
	pod := ""
	for _, c := range group {
		found := podOf(c.Locator)
		if found == "" {
			continue
		}
		if pod != "" && pod != found {
			return "", true
		}
		pod = found
	}
	return pod, false
}

// podOf is the pod a locator names, and "" for a claim with no process and for a runtime whose
// processes are in no pod (a tmux pane).
func podOf(locator *runtime.Locator) string {
	if locator == nil || locator.Sandbox == nil {
		return ""
	}
	return locator.Sandbox.PodUID
}

// openPodRound is the pod round open over the group, and "" when none is. Every claim the sweep
// groups is live (reconcilePodUpdates drops the retired ones), so the round read here is one a
// claim of this issue is still in.
func openPodRound(group []supervise.Claim) string {
	for _, c := range group {
		if c.InPodRound() {
			return c.UpdateRound
		}
	}
	return ""
}

// roundReasons is what the round was opened for, as its claims recorded it: the one string the
// round's task states, never cut up (record.UpdateStalled).
func roundReasons(group []supervise.Claim, round string) string {
	for _, c := range group {
		if c.UpdateRound == round && c.UpdateReasons != "" {
			return c.UpdateReasons
		}
	}
	return ""
}

// roundOpenedAt is when the round opened, the earliest its claims record, and nil when no claim of
// the group is in it.
func roundOpenedAt(group []supervise.Claim, round string) *time.Time {
	var opened *time.Time
	for _, c := range group {
		if c.UpdateRound != round || c.UpdateOpenedAt == nil {
			continue
		}
		if opened == nil || c.UpdateOpenedAt.Before(*opened) {
			opened = c.UpdateOpenedAt
		}
	}
	return opened
}

// answered is whether c has answered this round: the round it records is this one, and it
// signalled in that round. It is the one statement of "answered this round", read by every place
// the round's completeness is weighed — the claims it still waits on (waitingClaims), when the
// last answer came in (roundAnsweredAt) and the mark the controller's notice carries per claim
// (waitingOf). An answer a claim gave to another round says nothing of this one: the claim's
// recorded answer goes with the round that ends (clearUpdate), and a round that replaced another
// promises something else of the pod and the disk.
func answered(c supervise.Claim, round string) bool {
	return c.UpdateRound == round && c.UpdateSignalledAt != nil
}

// roundAnsweredAt is when this round's last answer came in, the latest its claims record, and nil
// when no claim of the group has answered it. It is what the late-round notice's bound runs from
// (tellLateRound): the stall that notice reports — a turn that will not end, a replacement that
// will not run — begins at the answer that completed the round, not at the round's opening.
func roundAnsweredAt(group []supervise.Claim, round string) *time.Time {
	var last *time.Time
	for _, c := range group {
		if !answered(c, round) {
			continue
		}
		if last == nil || c.UpdateSignalledAt.After(*last) {
			last = c.UpdateSignalledAt
		}
	}
	return last
}

// roundTold is whether this round was already told to the operator's side.
func roundTold(group []supervise.Claim, round string) bool {
	for _, c := range group {
		if c.UpdateRound == round && c.UpdateToldAt != nil {
			return true
		}
	}
	return false
}

// blockingClaims is every claim of the group the round waits on: one with a session, whose agent
// has run and may hold unpushed work. Every claim the sweep groups is live already
// (reconcilePodUpdates drops the retired ones), and a claim with no session has never had a
// worker.
func blockingClaims(group []supervise.Claim) []supervise.Claim {
	var blocking []supervise.Claim
	for _, c := range group {
		if c.Session != "" {
			blocking = append(blocking, c)
		}
	}
	return blocking
}

// waitingClaims is every blocking claim that has not answered this round. A claim the round has
// not reached yet — one whose recorded round is another, or none — has not answered it either.
func waitingClaims(group []supervise.Claim, round string) []supervise.Claim {
	var waiting []supervise.Claim
	for _, c := range blockingClaims(group) {
		if !answered(c, round) {
			waiting = append(waiting, c)
		}
	}
	return waiting
}

// workingClaims is every claim of the group whose worker runs a turn now, by role.
func workingClaims(group []supervise.Claim) []string {
	var working []string
	for _, c := range group {
		if c.State == supervise.StateWorking {
			working = append(working, string(c.Role))
		}
	}
	return working
}

// podProcess is the process the round's pod is ended through: the first claim of the group that
// records a process in that round's own pod. The pod is what the filter is on, not merely a
// recorded process: a claim the sweep relaunched while the round ran records the pod that
// replaced this one, and ending that pod would delete a pod no round covers, which has drifted
// from nothing and which the round's own replacement has just built.
func podProcess(group []supervise.Claim, round string) (runtime.Known, bool) {
	for _, c := range group {
		if c.Locator != nil && supervise.PodRound(podOf(c.Locator)) == round {
			return runtime.Known{Claim: c.Token, Locator: c.Locator}, true
		}
	}
	return runtime.Known{}, false
}

// waitingOf is the claims as the controller's notice lists them, so it can retry one claim or ask
// the operator. Each is marked with whether it has already answered this round.
//
// Answered is what keeps the controller's playbook from undoing the round. The notice of a round
// that is waiting on a turn or on a failing replacement lists claims that have every one of them
// answered, and the playbook's move for a claim that has not answered is to publish the update
// task to it — which starts a turn, which takes that claim's answer back (takeBackSignal). The
// mark is the round's own: an answer a claim gave to another round says nothing here.
func waitingOf(round string, claims []supervise.Claim) []record.UpdateWaiting {
	waiting := make([]record.UpdateWaiting, len(claims))
	for i, c := range claims {
		waiting[i] = record.UpdateWaiting{
			Claim: string(c.Token), Role: c.Role, State: string(c.State),
			Answered: answered(c, round),
		}
	}
	return waiting
}

// namesOf is the roles of claims, for a log line.
func namesOf(claims []supervise.Claim) []string {
	names := make([]string, len(claims))
	for i, c := range claims {
		names[i] = string(c.Role)
	}
	return names
}

// roleStates is each claim as a message names it: its role and the state it is in.
func roleStates(claims []supervise.Claim) string {
	named := make([]string, len(claims))
	for i, c := range claims {
		named[i] = fmt.Sprintf("%s (%s)", c.Role, c.State)
	}
	return strings.Join(named, ", ")
}

// podWentMessage is what the issue is told when its pod went before its round was over: the pod
// was replaced where it stood for a failure of its own, and the disk went with it. The issue's
// spec requires this message. Another agent reads it, so it follows ASD-STE100.
func podWentMessage(round, reasons string, lost []supervise.Claim) string {
	opened := "The daemon did not record why."
	if reasons != "" {
		opened = "The reasons were: " + reasons + "."
	}
	return fmt.Sprintf(`The pod that ran this issue's roles is gone. The daemon opened an update round over that pod (%s). %s The pod went before the round was over.

These roles did not answer the update task before the pod went: %s. The pod's disk went with the pod. Work that these roles did not push is lost.

The daemon starts each role again, in the same session, on a new pod. Each role must read its issue branch at origin to see the work that it pushed.`,
		round, opened, roleStates(lost))
}
