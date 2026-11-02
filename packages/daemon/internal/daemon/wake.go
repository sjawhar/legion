package daemon

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Bringing a released issue pod back. A release suspends every live claim of the pod with the
// source `released` and deletes the pod, its disk and its Sandbox; the claims, their sessions,
// their phases and the tasks they hold stay exactly as they were. A wake resumes those claims,
// and the resume is what makes the runtime raise a pod again, restore the saved bundle into the
// workspace and start the role's session where it left off.
//
// Five wakes bring a pod back, and every one of them ends here (wakePod):
//
//   - a daemon delivery for a role of the pod — a phase start, task, backward move, notice,
//     phase-ended message, production-check look or slot resume. A delivery to a suspended claim
//     resumes it already (supervise's deliverResuming) and a released claim may be resumed by rule,
//     so the starts and tasks need nothing here; the notice and the phase-ended message, which
//     reach a role over Envoy rather than through its claim, call in from the outbox;
//   - a CI settlement the daemon records for the head of the issue's pull request while a phase
//     worker of the pod has its phase open (check_settlement.go);
//   - a message published to the role topic of a role of the pod, a keep: false one included, and
//   - a message sent to the agent subject of a released session. Neither reaches the daemon on its
//     own: the Envoy listener reports both on notifications.legion.wake, which this daemon reads on
//     a durable of its own (intake.PodWake) and routes here through podWakeHandler;
//   - the backstop, backstop_minutes after the release, for each phase worker of the pod whose
//     phase is open. It is a stored row with a due time (store.PodWakeBackstop) rather than a timer
//     in memory, so one that came due while the daemon was down fires after the daemon starts.
//
// Each wake writes a store.PodResume row naming its kind, and each wake that carries no work of
// its own gives the woken role one message naming it (record.PodWoken), so the role's first turn
// in the replacement pod knows why it came back.

// The wake kinds, as a resume record spells them (store.PodResume.Kind).
const (
	wakeDelivery     = "delivery"
	wakeSettlement   = "ci_settlement"
	wakeRoleMessage  = "role_message"
	wakeAgentMessage = "agent_message"
	wakeBackstop     = "backstop"
)

// wakeStuckAfter is how long a resume may go without the woken role's first turn before the
// daemon warns and records it stuck (R5): long enough for a pod to be scheduled, pull its image,
// restore the bundle and boot its agent on a cold node, and short enough that an operator hears
// about a pod that is not coming back inside the hour the backstop works in.
const wakeStuckAfter = 10 * time.Minute

// wakeTickInterval is how often the daemon fires due backstops and looks for stuck resumes. It is
// the idle tick's period: both are one small query per pass over one project.
const wakeTickInterval = time.Minute

// wakeRaiseTimeout bounds one wake taken off a listener report: the resume it makes hands the
// runtime a pod to raise and waits for its launch, which on a cold node is minutes. A wake that
// runs this long is one the runtime never answered, and the next report for the same pod — the
// sender's next try, or the backstop — asks again.
const wakeRaiseTimeout = 10 * time.Minute

// podWake is one wake: which pod it brings back, which role it is for, what kind of wake it is,
// and what the woken role is told about it.
type podWake struct {
	// Issue is the released pod's issue; Role the role the wake is for, or "" for every released
	// role of the pod.
	Issue string
	Role  claim.Role
	// Kind is one of the five above.
	Kind string
	// Detail is the wake in the daemon's own words, which the woken role is given as a message
	// (record.PodWoken). It is empty for a wake that carries work of its own — the delivery, the
	// notice, the phase-ended message — whose own text names the wake already.
	Detail string
	// Key is what makes this wake one wake, inside its issue and kind (store.TakePodWake): the
	// head a settlement settled, the id the listener gave a report, the release a backstop
	// belongs to. A wake with no key takes no mark and may happen again.
	Key string
	At  time.Time
}

// wakePod brings the released claims a wake names back, and answers how many it woke. It is the
// one path every wake takes, so every wake is recorded, keyed and told the same way.
//
// Nothing happens when the pod holds no claim suspended as released: the wake is for a pod that
// is running, one whose claims a linger, a park, a close or an operator took over, or one of
// another daemon's projects. Nothing happens either when the issue or its tree has left the
// workflow or the tree lingers: such a pod is a close's to stop, never a wake's to raise.
//
// A keyed wake is taken once: a head settles again as the rest of its checks finish, and the
// listener reports one undeliverable message on each of its delivery attempts, and the pod comes
// back for the first of them alone. The mark is taken before anything is resumed, so a second
// caller racing the first finds it taken and does nothing.
func (r *outbox) wakePod(ctx context.Context, w podWake) (int, error) {
	if r.supervisor == nil || r.trees == nil {
		return 0, nil
	}
	if w.At.IsZero() {
		w.At = r.now().UTC()
	}
	woken := r.releasedClaims(w.Issue, w.Role)
	if len(woken) == 0 {
		return 0, nil
	}
	live, err := r.wakeableTree(ctx, w.Issue)
	if err != nil || !live {
		return 0, err
	}
	if w.Key != "" {
		took, err := r.trees.TakePodWake(ctx, r.project, w.Issue, w.Kind, w.Key, w.At)
		if err != nil || !took {
			return 0, err
		}
	}
	release, _, err := r.trees.LatestPodRelease(ctx, r.project, w.Issue)
	if err != nil {
		return 0, err
	}
	for _, machine := range woken {
		held := machine.Claim()
		if _, err := r.trees.RecordPodWake(ctx, store.PodResume{Release: release, Project: r.project,
			Issue: w.Issue, Role: string(held.Role), Kind: w.Kind, At: w.At}); err != nil {
			return 0, err
		}
		if w.Detail != "" {
			if err := r.tellWokenRole(ctx, w, held.Role); err != nil {
				return 0, err
			}
		}
		if err := machine.Handle(ctx, supervise.RequestRuleResume{Claim: held.Token,
			Reason: fmt.Sprintf("%s woke the released pod of %s", w.Kind, w.Issue)}); err != nil {
			return 0, fmt.Errorf("resume released claim %s: %w", held.Token, err)
		}
		r.log.Info("wake: a released issue pod comes back, and its role resumes in the same session",
			"issue", w.Issue, "role", held.Role, "kind", w.Kind, "release", release, "detail", w.Detail)
	}
	// Whatever woke the pod, its backstop has done its work and is forgotten: the pod is coming
	// back, and the next release arms a backstop of its own.
	if err := r.trees.ClearPodWakeBackstop(ctx, r.project, w.Issue); err != nil {
		return 0, err
	}
	r.wake(w.Issue, record.OutboxKindPodWoken)
	return len(woken), nil
}

// tellWokenRole queues the one message a woken role is given, on the role-message lane so it
// waits behind no lifecycle row of the issue. It is written before the resume, in the same pass,
// so the row is durable before anything raises a pod: a daemon that stops between the two resumes
// the claim at its next boot through this row's own retry, never silently without it.
func (r *outbox) tellWokenRole(ctx context.Context, w podWake, role claim.Role) error {
	row, err := record.NewOutboxRow(w.Issue, record.PodWoken{Role: role, Kind: w.Kind, Detail: w.Detail}, r.now().UTC())
	if err != nil {
		return fmt.Errorf("encode the wake message for %s's %s: %w", w.Issue, role, err)
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error { return r.records.Enqueue(ctx, tx, row) })
}

// releasedClaims are the machines of issue whose claims are suspended as released, for role or
// for every role when role is "". A claim suspended by anyone else is not a released pod's: a
// linger, a park, a close, a hand-over and the operator's own suspend each take a released claim
// over (supervise's suspendedAgain), which is what stops a later wake acting on it.
func (r *outbox) releasedClaims(issue string, role claim.Role) []*supervise.Machine {
	var woken []*supervise.Machine
	for _, machine := range r.supervisor.supervised() {
		c := machine.Claim()
		if c.Issue != issue || (role != "" && c.Role != role) {
			continue
		}
		if c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByReleased {
			woken = append(woken, machine)
		}
	}
	return woken
}

// releasedClaimOf is the machine of the one released claim token names, and false when the token
// names no claim of this daemon's or one that is not released. It is how a role token the Envoy
// listener reports is matched: the listener does not know which daemon owns a role, and this is
// the whole of what a daemon does to find out.
func (r *outbox) releasedClaimOf(token claim.Token) (*supervise.Machine, bool) {
	for _, machine := range r.supervisor.supervised() {
		c := machine.Claim()
		if c.Token == token && c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByReleased {
			return machine, true
		}
	}
	return nil, false
}

// releasedClaimOfSession is the machine of the released claim whose session is sessionID. A
// session id is the only thing an agent-subject message carries that names a claim, and a
// released claim keeps the session it was released with, so this is how such a message finds its
// pod.
func (r *outbox) releasedClaimOfSession(sessionID string) (*supervise.Machine, bool) {
	for _, machine := range r.supervisor.supervised() {
		c := machine.Claim()
		if c.Session == sessionID && c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByReleased {
			return machine, true
		}
	}
	return nil, false
}

// wakeableTree is whether issue's pod is one a wake may raise: the issue and its tree root are
// still the workflow's to run, and neither lingers. It is idleTreeLive's rule read from the
// record, so the rule that releases a pod and the rule that brings it back never disagree about
// which pod belongs to the workflow. An issue the record does not hold is not wakeable.
func (r *outbox) wakeableTree(ctx context.Context, key string) (bool, error) {
	var issue, root *record.Issue
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		if issue, err = r.records.Issue(ctx, tx, key); err != nil || issue == nil {
			return err
		}
		root, err = r.records.Issue(ctx, tx, issue.Tree)
		return err
	}); err != nil {
		return false, fmt.Errorf("read %s before waking its pod: %w", key, err)
	}
	if issue == nil || root == nil {
		return false, nil
	}
	return workflow.InWorkflow(*issue) && workflow.InWorkflow(*root) && !issue.Lingers() && !root.Lingers(), nil
}

// phaseOpenFor is whether c is the phase worker of issue's current phase, serving the current
// generation: the one shape of claim the backstop wakes, since it is the one with a phase of its
// own left to finish.
func phaseOpenFor(issue record.Issue, c supervise.Claim) bool {
	if issue.Phase == phase.Done || c.Role == claim.RoleArchitect {
		return false
	}
	return workflow.RoleFor(issue.Phase) == c.Role && c.ServingGeneration == issue.Generation
}

// armBackstop records that issue's released pod comes back backstop from now if nothing else
// wakes it first. A deployment that sets backstop_minutes to zero arms none, and such a pod waits
// for a wake of its own.
func (r *outbox) armBackstop(ctx context.Context, issue string, release int64) error {
	if r.backstop <= 0 {
		return nil
	}
	return r.trees.ArmPodWakeBackstop(ctx, r.project, issue, release, r.now().UTC().Add(r.backstop))
}

// fireBackstops brings back every released pod of the project whose backstop has fallen due. One
// backstop wakes each phase worker of the pod whose phase is open, and nothing else: an architect
// and a role with no phase left to work have no turn the backstop is buying them. A pod whose
// backstop is due and that holds no such claim has its backstop forgotten rather than retried for
// ever.
func (r *outbox) fireBackstops(ctx context.Context) {
	due, err := r.trees.DuePodWakeBackstops(ctx, r.project, r.now().UTC())
	if err != nil {
		r.log.Warn("wake: could not read the due backstops", "error", err)
		return
	}
	for _, backstop := range due {
		if ctx.Err() != nil {
			return
		}
		if err := r.fireBackstop(ctx, backstop); err != nil {
			r.log.Warn("wake: a backstop did not fire", "issue", backstop.Issue, "due", backstop.DueAt, "error", err)
		}
	}
}

func (r *outbox) fireBackstop(ctx context.Context, backstop store.PodWakeBackstop) error {
	issue, err := r.issue(ctx, backstop.Issue)
	if err != nil {
		return err
	}
	detail := fmt.Sprintf("the release's backstop ran out %s after the pod was released, with no other wake for it",
		r.backstop.String())
	woken := 0
	for _, machine := range r.releasedClaims(backstop.Issue, "") {
		if !phaseOpenFor(issue, machine.Claim()) {
			continue
		}
		role := machine.Claim().Role
		count, err := r.wakePod(ctx, podWake{Issue: backstop.Issue, Role: role, Kind: wakeBackstop,
			Detail: detail, Key: string(role) + "@" + strconv.FormatInt(backstop.Release, 10)})
		if err != nil {
			return err
		}
		woken += count
	}
	if woken > 0 {
		return nil
	}
	// Nothing to wake: the pod is back, its claims were taken over, or no phase of it is open.
	// The backstop has nothing left to do and is forgotten, so the tick does not read it again.
	return r.trees.ClearPodWakeBackstop(ctx, r.project, backstop.Issue)
}

// warnStuckResumes writes one warning per resume that has had no first turn wakeStuckAfter after
// its wake, and marks it stuck so it warns once (R5). The warning names the issue, the role and
// the step the resume is waiting on: the pod has not reported ready, or it has and the woken
// role has taken no turn in it.
func (r *outbox) warnStuckResumes(ctx context.Context) {
	stuck, err := r.trees.StuckPodResumes(ctx, r.project, r.now().UTC().Add(-wakeStuckAfter))
	if err != nil {
		r.log.Warn("wake: could not read the resumes that have had no first turn", "error", err)
		return
	}
	for _, resume := range stuck {
		waiting := "the replacement pod has not reported ready"
		if !resume.PodReadyAt.IsZero() {
			waiting = "the replacement pod is ready and the woken role has taken no turn in it"
		}
		r.log.Warn("wake: a released pod was woken and its role has taken no turn since",
			"issue", resume.Issue, "role", resume.Role, "kind", resume.Kind, "woken", resume.At,
			"after", wakeStuckAfter.String(), "waiting-on", waiting)
		if err := r.trees.MarkPodResumeStuck(ctx, resume.ID); err != nil {
			r.log.Warn("wake: could not mark a resume stuck", "issue", resume.Issue, "role", resume.Role, "error", err)
		}
	}
}

// tickWake fires the due backstops and warns about the stuck resumes on the daemon's own period.
// It runs only where pods are released: a deployment with release off has no released pod, so no
// backstop falls due and no resume is measured.
func (w *workflowRuntime) tickWake(ctx context.Context) {
	if w.idlePeriod <= 0 || w.idleStore == nil || w.idleImages == nil {
		return
	}
	interval := w.idleTick
	if interval <= 0 {
		interval = wakeTickInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runner := w.outboxWake.Load()
			if runner == nil {
				continue
			}
			runner.fireBackstops(ctx)
			runner.warnStuckResumes(ctx)
		}
	}
}

// podWakeHandler applies the Envoy listener's wake reports (intake.PodWake): the listener's own
// heartbeat, which the release decision reads, and the two messages that reach nobody because the
// pod their recipient runs in was released. It is an intake handler rather than an arm of the
// workflow engine because what it decides is held in the supervisor's machines, not in the
// record: which claims of this daemon are suspended as released, and which of them a role token
// or a session id names.
//
// The resume itself runs after the fact's transaction commits (intake.WithCommitHooks). A resume
// relaunches a session and can wait on the runtime for as long as a pod takes to schedule, which
// is not work to hold the one fact lock every project's daemon shares.
type podWakeHandler struct{ runtime *workflowRuntime }

func (h podWakeHandler) Apply(ctx context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	wake, ok := fact.(intake.PodWake)
	if !ok {
		return intake.Result{}, nil
	}
	w := h.runtime
	if wake.Kind == intake.PodWakeListenerReady {
		w.listenerHeard.Store(wake.At.UnixMilli())
		return intake.Result{}, nil
	}
	runner := w.outboxWake.Load()
	if runner == nil || runner.supervisor == nil {
		return intake.Result{}, nil
	}
	var machine *supervise.Machine
	var kind, detail string
	switch wake.Kind {
	case intake.PodWakeRoleMessage:
		machine, ok = runner.releasedClaimOf(claim.Token(wake.Role))
		kind, detail = wakeRoleMessage, fmt.Sprintf("a message was published to your role topic while your pod was gone,"+
			" and the Envoy listener answered its sender that the role had no holder (%s); the sender's next try reaches you", wake.Reason)
	case intake.PodWakeAgentMessage:
		machine, ok = runner.releasedClaimOfSession(wake.Session)
		kind, detail = wakeAgentMessage, fmt.Sprintf("a message was sent to your session while your pod was gone"+
			" and the Envoy listener could place it nowhere (%s); it is redelivered to you now", wake.Reason)
	default:
		return intake.Result{}, nil
	}
	if !ok {
		// The report names no released claim of this daemon's. Every daemon reads every
		// listener's reports, so this is the ordinary case and says nothing is wrong.
		return intake.Result{}, nil
	}
	held := machine.Claim()
	woken := podWake{Issue: held.Issue, Role: held.Role, Kind: kind, Detail: detail, Key: wake.ReportID, At: wake.At}
	raise := func() {
		// On a goroutine of its own, bounded like a terminal fact: a resume relaunches a session
		// and waits on the runtime for as long as a pod takes to schedule, and the fact lock
		// every project's daemon shares is not held for that.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), wakeRaiseTimeout)
			defer cancel()
			if _, err := runner.wakePod(ctx, woken); err != nil {
				w.log.Warn("wake: the listener's report did not bring its pod back",
					"issue", woken.Issue, "role", woken.Role, "kind", woken.Kind, "error", err)
			}
		}()
	}
	if intake.CommitScoped(ctx) {
		intake.OnCommit(ctx, raise)
	} else {
		raise()
	}
	return intake.Result{}, nil
}

// listenerReported is whether this daemon has heard the Envoy listener say it is listening inside
// listenerReadyWindow. The release decision reads it, and nothing else does: a listener that
// stopped reporting and a listener with nothing to report are the same silence, so a daemon that
// has heard neither must assume the first and release no pod it could not then wake.
func (w *workflowRuntime) listenerReported(now time.Time) bool {
	heard := w.listenerHeard.Load()
	if heard == 0 {
		return false
	}
	return now.Sub(time.UnixMilli(heard)) < listenerReadyWindow
}

// listenerReadyWindow is how stale the listener's last report may be before the daemon stops
// releasing pods. It is three of the listener's own five-minute reports, so one report lost to a
// NATS hiccup, or one the daemon's own consumer was slow to take, changes nothing.
const listenerReadyWindow = 15 * time.Minute
