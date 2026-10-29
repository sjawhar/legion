package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The two kinds of round supervise.Claim.UpdateRound spells: a pod's, which every role claim of
// one issue shares, and one claim's own plugin round (supervise's refreshPlugin).
const (
	podRoundPrefix    = "pod:"
	pluginRoundPrefix = "plugin:"
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
// is what groups them. A claim with no issue (the project controller's) is in no pod round: its
// pod has no issue workspace and nothing to push. A group whose claims' locators name two
// different pods is left alone this tick: a replacement is already in flight, and the next tick
// reads it settled.
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
		if c.Issue == "" {
			// The project controller's claim: no pod round, but a plugin round of its own runs
			// here as any other claim's does.
			if strings.HasPrefix(c.UpdateRound, pluginRoundPrefix) {
				s.reconcilePluginUpdate(ctx, c, now)
			}
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

// reconcileIssuePodUpdate carries one issue's pod round a step. group is every claim of the issue,
// whatever its state.
//
// The decisions are taken in this order, and the order is the rule. A pod the group no longer
// names went while its round was open, whatever the runtime says of a pod that is not there. Then
// the drift: a pod that has drifted from nothing needs no replacement, so a round open over it
// ends rather than replaces it, however many workers have answered. Only then is a complete round
// acted on, and only then is a round opened or kept open.
func (s *supervision) reconcileIssuePodUpdate(ctx context.Context, issue string, group []supervise.Claim, now time.Time) {
	slices.SortFunc(group, func(a, b supervise.Claim) int { return strings.Compare(string(a.Token), string(b.Token)) })
	pod, split := groupPod(group)
	if split {
		s.log.Info("supervise: the claims of this issue run in two pods; their update round waits for the replacement in flight",
			"issue", issue)
		return
	}
	open := openPodRound(group)
	// The pod went without the round: it was replaced where it stood for a failure of its own — a
	// terminal pod, a failed init container, a superseded disk, a lost launcher binding — and its
	// disk went with it while a worker still had work to push. A round every blocking claim
	// answered lost nothing, and ends below with the drift or the replacement.
	if open != "" && open != podRoundPrefix+pod && len(waitingClaims(group, open)) > 0 {
		s.podWentWithoutItsRound(ctx, issue, group, open, pod)
		return
	}
	reasons, ok := s.groupDrift(ctx, issue, group)
	if !ok {
		return
	}
	if len(reasons) == 0 {
		if open != "" {
			s.log.Info("supervise: the pod of this issue has drifted from nothing now; its update round is over",
				"issue", issue, "round", open)
			s.endPodUpdates(group)
		}
		s.reconcilePluginUpdates(ctx, group, now)
		return
	}
	if open != "" && s.completePodUpdate(ctx, issue, group, open, pod) {
		return
	}
	if pod == "" {
		s.log.Error("supervise: this issue's pod has drifted, and no claim of it records the pod to open a round over",
			"issue", issue, "reasons", reasons)
		return
	}
	s.openPodUpdate(ctx, issue, group, podRoundPrefix+pod, pod, reasons, now)
}

// completePodUpdate replaces the pod and ends the round when the round is over: every claim that
// blocks it has answered, and no claim of the group runs a turn. Waiting for no claim to be
// working is what keeps the promise that no worker is stopped mid-turn — a worker answers from
// inside a turn, and that turn ends a moment later. It reports whether it decided the round.
//
// ReplacePod is called once, with the process of the first claim that records one; a round whose
// pod is already gone replaces nothing and only ends. A failed ReplacePod is logged and retried at
// the next tick: nothing is ended, so the round still holds every worker's next task.
func (s *supervision) completePodUpdate(ctx context.Context, issue string, group []supervise.Claim, round, pod string) bool {
	waiting := waitingClaims(group, round)
	if len(waiting) > 0 {
		return false
	}
	if working := workingClaims(group); len(working) > 0 {
		s.log.Info("supervise: every worker of this update round has answered; the replacement waits for the turns still running",
			"issue", issue, "round", round, "working", working)
		return true
	}
	if k, running := podProcess(group); running {
		if err := s.runtime.ReplacePod(ctx, k); err != nil {
			if ctx.Err() == nil {
				s.log.Error("supervise: replace the pod of a finished update round; retrying at the next sweep",
					"issue", issue, "round", round, "pod", pod, "error", err)
			}
			return true
		}
	}
	s.log.Info("supervise: every worker of this update round has answered; the pod is replaced and the round is over",
		"issue", issue, "round", round, "pod", pod)
	s.endPodUpdates(group)
	return true
}

// podWentWithoutItsRound is the round's pod replaced before the round could finish: the message
// the issue's spec requires is written on the issue, naming what was lost, and the round ends.
// Nothing here can bring the work back — the disk went with the pod — so the message is the whole
// of it.
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

// openPodUpdate opens the round, or keeps it open: every claim that blocks it is posted
// PodUpdateOpen at every tick while the round runs. The event is idempotent and carries the
// re-task clock itself (supervise's openPodUpdate), so the sweep states the round and the machine
// decides whether this is the first ask or the next one, pod_update_retask_hours after the last.
//
// A claim that blocks the round has a session — an agent has run for it, and may hold unpushed
// work — and is not retired. A claim with no session has never had a worker and blocks nothing. A
// failed or operator-suspended claim blocks until the operator acts, which is this issue's own
// rule, and the notice below is what tells the operator.
func (s *supervision) openPodUpdate(ctx context.Context, issue string, group []supervise.Claim, round, pod string, reasons []string, now time.Time) {
	blocking := blockingClaims(group)
	joined := strings.Join(reasons, ", ")
	task := supervise.PodUpdateTask(joined)
	for _, c := range blocking {
		s.supervisor.post(c.Token, supervise.PodUpdateOpen{Claim: c.Token, Round: round, Reasons: joined, Task: task})
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
	if deaf := cannotAnswer(blocking); len(deaf) > 0 {
		// A claim whose plugin predates PodUpdateContract can never answer: the op is a closed
		// enum compiled into its plugin, and a Claude Code role's plugin is its image's own, never
		// installed. Nothing is replaced, and the operator drains the pod by hand.
		s.tellUpdateStalled(ctx, updateStall{
			issue: issue, round: round, pod: pod, reasons: reasons, group: group,
			waiting: waitingOf(deaf, true),
			reason: fmt.Sprintf("a worker's plugin predates daemon contract %d, so it can never answer the update task",
				supervise.PodUpdateContract),
			message: cannotAnswerMessage(reasons, deaf),
		})
		return
	}
	if now.Sub(*opened) >= s.cfg.PodUpdateNotice {
		s.tellUpdateStalled(ctx, updateStall{
			issue: issue, round: round, pod: pod, reasons: reasons, group: group,
			waiting: waitingOf(waitingClaims(group, round), false),
			reason: fmt.Sprintf("the update round has waited %s for its workers to answer",
				now.Sub(*opened).Round(time.Minute)),
		})
	}
}

// reconcilePluginUpdates carries the plugin round of every claim of a group that is in one. It
// runs only where no pod round is open over the group: a pod round takes every claim of the issue,
// a plugin round's claim among them, and replacing the pod is what that claim was waiting for
// anyway.
func (s *supervision) reconcilePluginUpdates(ctx context.Context, group []supervise.Claim, now time.Time) {
	for _, c := range group {
		if strings.HasPrefix(c.UpdateRound, pluginRoundPrefix) {
			s.reconcilePluginUpdate(ctx, c, now)
		}
	}
}

// reconcilePluginUpdate carries one claim's own plugin round: the round supervise's refreshPlugin
// opens over a claim whose process speaks a plugin contract this daemon does not. The sweep is
// what ends it — once the worker has answered and runs no turn, PodUpdateEnded with Relaunch true
// replaces the process onto this daemon's plugin, which is what the round was opened for. Until
// then the claim is asked again on the round's own re-task clock, and the round's notice and
// bounds are the pod round's.
func (s *supervision) reconcilePluginUpdate(ctx context.Context, c supervise.Claim, now time.Time) {
	round := c.UpdateRound
	if c.UpdateSignalledAt != nil {
		if c.State == supervise.StateWorking {
			s.log.Info("supervise: this claim's plugin round has its answer; the relaunch waits for the turn still running",
				"claim", c.Token, "round", round)
			return
		}
		s.log.Info("supervise: this claim's plugin round has its answer; its process is replaced onto this daemon's plugin",
			"claim", c.Token, "round", round)
		s.supervisor.post(c.Token, supervise.PodUpdateEnded{Claim: c.Token, Relaunch: true})
		return
	}
	s.supervisor.post(c.Token, supervise.PodUpdateOpen{
		Claim: c.Token, Round: round, Reasons: c.UpdateReasons, Task: supervise.PodUpdateTask(c.UpdateReasons),
	})
	deaf := c.PluginContract < supervise.PodUpdateContract
	late := c.UpdateOpenedAt != nil && now.Sub(*c.UpdateOpenedAt) >= s.cfg.PodUpdateNotice
	if c.UpdateToldAt != nil || !(deaf || late) {
		return
	}
	if c.Issue == "" {
		// The project controller's own claim: there is no issue to write on, and the round the
		// controller would be told of is its own process's. The log line is the whole record.
		s.log.Warn("supervise: the controller's own plugin round is not finishing", "claim", c.Token, "round", round,
			"pluginContract", c.PluginContract, "openedAt", c.UpdateOpenedAt)
		return
	}
	reasons := splitReasons(c.UpdateReasons)
	stall := updateStall{
		issue: c.Issue, round: round, reasons: reasons, group: []supervise.Claim{c},
		waiting: waitingOf([]supervise.Claim{c}, deaf),
	}
	switch {
	case deaf:
		stall.reason = fmt.Sprintf("the worker's plugin predates daemon contract %d, so it can never answer the update task",
			supervise.PodUpdateContract)
		stall.message = cannotAnswerMessage(reasons, []supervise.Claim{c})
	default:
		stall.reason = fmt.Sprintf("the plugin round has waited %s for its worker to answer",
			now.Sub(*c.UpdateOpenedAt).Round(time.Minute))
	}
	s.tellUpdateStalled(ctx, stall)
}

// updateStall is one round the sweep cannot finish, as the operator's side is told of it: the
// issue and round it is of, the pod it was opened over (none for a plugin round), what the round
// was opened for, the claims it waits on, the one-line why, the message to write on the issue
// (empty for a stall the issue is not told of), and the group whose claims mark the round told.
type updateStall struct {
	issue   string
	round   string
	pod     string
	reasons []string
	waiting []record.UpdateWaiting
	reason  string
	message string
	group   []supervise.Claim
}

// tellUpdateStalled tells the controller that a round is not finishing, and writes the same thing
// on the issue when the stall is one an operator must act on. One round is told once however many
// ticks find it open: the telling is marked on every claim of the round (UpdateToldAt), and a
// round already marked is told nothing more. The mark is written after the rows are enqueued, so a
// row the store refuses is written again at the next tick rather than swallowed.
func (s *supervision) tellUpdateStalled(ctx context.Context, stall updateStall) {
	if roundTold(stall.group, stall.round) {
		return
	}
	if s.enqueue == nil {
		s.log.Warn("supervise: an update round is stalled, and this deployment has no workflow to tell the operator's side through",
			"issue", stall.issue, "round", stall.round, "reason", stall.reason)
		return
	}
	notice := record.ControllerNotice{
		Kind:   record.UpdateStalledNotice,
		Reason: stall.reason,
		Update: &record.UpdateStalled{Round: stall.round, Pod: stall.pod, Reasons: stall.reasons, Waiting: stall.waiting},
	}
	if err := s.enqueue(ctx, stall.issue, notice); err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: tell the controller that an update round is stalled; telling again at the next sweep",
				"issue", stall.issue, "round", stall.round, "error", err)
		}
		return
	}
	if stall.message != "" {
		if err := s.writeOnIssue(ctx, stall.issue, stall.message); err != nil {
			s.log.Error("supervise: write on the issue that its update round is stalled; writing again at the next sweep",
				"issue", stall.issue, "round", stall.round, "error", err)
			return
		}
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

// endPodUpdates ends the round on every claim of the group that is in one.
func (s *supervision) endPodUpdates(group []supervise.Claim) {
	for _, c := range group {
		if c.UpdateRound == "" {
			continue
		}
		s.supervisor.post(c.Token, supervise.PodUpdateEnded{Claim: c.Token, Relaunch: false})
	}
}

// groupDrift is why the group's pod has drifted: the union of the runtime's PodDrift over every
// claim of the group that records a process, in the claims' own order and without repeats. It
// reports false when the runtime could not be asked at all: an error is never a statement that
// something drifted, and a round must neither open nor end on one.
func (s *supervision) groupDrift(ctx context.Context, issue string, group []supervise.Claim) ([]string, bool) {
	var reasons []string
	for _, c := range group {
		if c.Locator == nil {
			continue
		}
		drift, err := s.runtime.PodDrift(ctx, runtime.Known{Claim: c.Token, Locator: c.Locator})
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("supervise: read whether this issue's pod has drifted", "issue", issue, "claim", c.Token, "error", err)
			}
			return nil, false
		}
		for _, reason := range drift {
			if !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		}
	}
	return reasons, true
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

// openPodRound is the pod round open over the group, and "" when none is.
func openPodRound(group []supervise.Claim) string {
	for _, c := range group {
		if strings.HasPrefix(c.UpdateRound, podRoundPrefix) {
			return c.UpdateRound
		}
	}
	return ""
}

// roundReasons is what the round was opened for, as its claims recorded it.
func roundReasons(group []supervise.Claim, round string) []string {
	for _, c := range group {
		if c.UpdateRound == round && c.UpdateReasons != "" {
			return splitReasons(c.UpdateReasons)
		}
	}
	return nil
}

// splitReasons is the reasons a claim records as one string, back as the list the notice carries.
func splitReasons(reasons string) []string {
	if reasons == "" {
		return nil
	}
	return strings.Split(reasons, ", ")
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
// has run and may hold unpushed work, that is not retired.
func blockingClaims(group []supervise.Claim) []supervise.Claim {
	var blocking []supervise.Claim
	for _, c := range group {
		if c.Session != "" && c.State != supervise.StateRetired {
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
		if c.UpdateRound != round || c.UpdateSignalledAt == nil {
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

// cannotAnswer is every claim that can never answer an update task: its registration declared a
// plugin contract below the one that introduced the op (supervise.PodUpdateContract).
func cannotAnswer(claims []supervise.Claim) []supervise.Claim {
	var deaf []supervise.Claim
	for _, c := range claims {
		if c.PluginContract < supervise.PodUpdateContract {
			deaf = append(deaf, c)
		}
	}
	return deaf
}

// podProcess is the process the round's pod is ended through: the first claim of the group that
// records one.
func podProcess(group []supervise.Claim) (runtime.Known, bool) {
	for _, c := range group {
		if c.Locator != nil {
			return runtime.Known{Claim: c.Token, Locator: c.Locator}, true
		}
	}
	return runtime.Known{}, false
}

// waitingOf is the claims as the controller's notice lists them, so it can retry one claim or ask
// the operator. deaf marks claims whose plugin can never answer.
func waitingOf(claims []supervise.Claim, deaf bool) []record.UpdateWaiting {
	waiting := make([]record.UpdateWaiting, len(claims))
	for i, c := range claims {
		waiting[i] = record.UpdateWaiting{
			Claim: string(c.Token), Role: c.Role, State: string(c.State),
			CannotAnswer: deaf || c.PluginContract < supervise.PodUpdateContract,
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

// cannotAnswerMessage is what the issue is told when its pod cannot be replaced at all: one of its
// workers runs a plugin that has no ready_for_update op, so the round can never complete and an
// operator must drain the pod. Another agent reads it, so it follows ASD-STE100: one instruction
// per sentence, active voice, and no word that has a second reading.
func cannotAnswerMessage(reasons []string, deaf []supervise.Claim) string {
	return fmt.Sprintf(`The daemon must replace the pod that runs this issue's roles. The reasons are: %s.

The daemon cannot replace the pod now. These roles run a plugin that is older than daemon contract %d: %s. A plugin that old has no `+"`ready_for_update`"+` operation. These roles can never answer the daemon's update task.

An operator must do these three steps:

1. Tell each role of this issue to push its work.
2. Suspend each role of this issue.
3. Start each role of this issue again.

The new processes run on a pod that the daemon builds from the current configuration.`,
		strings.Join(reasons, ", "), supervise.PodUpdateContract, roleStates(deaf))
}

// podWentMessage is what the issue is told when its pod went before its round was over: the pod
// was replaced where it stood for a failure of its own, and the disk went with it. The issue's
// spec requires this message. Another agent reads it, so it follows ASD-STE100.
func podWentMessage(round string, reasons []string, lost []supervise.Claim) string {
	opened := "The daemon did not record why."
	if len(reasons) > 0 {
		opened = "The reasons were: " + strings.Join(reasons, ", ") + "."
	}
	return fmt.Sprintf(`The pod that ran this issue's roles is gone. The daemon opened an update round over that pod (%s). %s The pod went before the round was over.

These roles did not answer the update task before the pod went: %s. The pod's disk went with the pod. Work that these roles did not push is lost.

The daemon starts each role again, in the same session, on a new pod. Each role must read its issue branch at origin to see the work that it pushed.`,
		round, opened, roleStates(lost))
}
