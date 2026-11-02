// Package pods holds what the daemon's two sides of an idle issue pod both name: the view of one
// pod the idle rule decides over, which the store fills and the workflow reads, and the
// vocabulary of a wake. It is a package of its own because those two sides are the store and the
// workflow, and neither may import the other.
package pods

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Wake kinds, the vocabulary of a recorded resume's kind and of a wake mark's: the five ways a
// released pod comes back. WakeDelivery is a delivery the daemon already had for a role of the
// pod — a phase start, a task, a backward move, a notice, a phase-ended message, a production
// check's look, a slot resume; WakeChecks is a CI settlement on the head of the issue's pull
// request; WakeRoleMessage and WakeAgentMessage are what only the Envoy listener sees, a message
// published to a role topic of the pod and one sent to the agent subject of a released session;
// WakeBackstop is the stored backstop a release armed.
//
// They are records rather than the rule's own, because the rule that names a wake (workflow) and
// the store that files it under this word are different packages, and one vocabulary is what
// keeps a wake's mark, its resume row and the task it writes saying the same thing.
const (
	WakeDelivery     = "delivery"
	WakeChecks       = "checks"
	WakeRoleMessage  = "role_message"
	WakeAgentMessage = "agent_message"
	WakeBackstop     = "backstop"
)

// IdleClaim is one claim of an issue's pod as the idle rule reads it. Every field comes from the
// claims table and none from a live machine: the activity columns are written by the session's
// activity route alone and are deliberately absent from the claim insert's conflict update, so a
// machine's in-memory claim shows their zero values until the claim is read again.
type IdleClaim struct {
	Role           claim.Role
	State          supervise.ClaimState
	PluginContract int
	// ServingGeneration is the run the claim is serving, which tells a phase worker of the
	// issue's current run from one left over by an older one.
	ServingGeneration uint64
	// LastStartRow is the newest start row the outbox ran against the claim. The release records
	// the highest of the pod's as its mark's started_row, which is what tells a start queued
	// before the boundary and already run from one of the release's own rows.
	LastStartRow int64
	// LastTurnAt is the newest moment a turn of this claim is known to have been under way: the
	// latest of its pending delivery's send acknowledgement and the confirmation of the turn that
	// send started, zero for a claim holding no delivery. A session's report counts only when it
	// was read after it (IdleClaim.Reported).
	LastTurnAt time.Time
	// ActivityAt, ActivityBackground, ActivityKinds and ActivityNextReminderAt are the session's
	// last activity report (POST /legion/v1/claims/activity): when it read what it was doing,
	// whether it found background work of its own running, what kinds it named, and when the
	// phase reminder armed in it is due. ActivityAt is zero for a session that has reported none.
	ActivityAt             time.Time
	ActivityBackground     bool
	ActivityKinds          []string
	ActivityNextReminderAt time.Time
	// UpdatedAt is when the claim's row was last written: every state the machine persisted and
	// every activity report. The newest UpdatedAt of a pod's claims is when the pod last did
	// anything the daemon recorded, which is what the idle period is measured from.
	UpdatedAt time.Time
}

// Live is whether the claim still holds its place in the pod: a failed, retired or already
// suspended claim stays as it is and holds no pod.
func (c IdleClaim) Live() bool {
	switch c.State {
	case supervise.StateFailed, supervise.StateRetired, supervise.StateSuspended:
		return false
	}
	return true
}

// Reported is whether the session read what it was doing after its last turn: a claim with no
// report at all, or one whose newest report predates its last turn, has said nothing about the
// turn that just ran. Such a claim is never released, which is what makes a host that cannot
// report degrade safely rather than lose work it was still doing.
func (c IdleClaim) Reported() bool {
	return !c.ActivityAt.IsZero() && !c.ActivityAt.Before(c.LastTurnAt)
}

// IdleRoot is the tree root of the issue the idle rule is deciding, read with it.
type IdleRoot struct {
	Phase       phase.Phase
	Status      string
	Generation  uint64
	LingerUntil *time.Time
	// AwaitingPerson is whether the root waits on a person (phase awaiting_person) and Answered
	// whether that person has answered, which leaves the root waiting only for a slot.
	AwaitingPerson bool
	Answered       bool
}

// IdleView is one issue's pod as the idle rule reads it, in one read per issue: every claim of
// the pod, whether any queued outbox row is addressed to one of its roles, the issue's own place
// in the workflow and its tree root's. Found is false for an issue the record does not hold,
// which is no pod of the workflow's to release.
//
// It is a record type rather than a store type because the rule that reads it is the workflow's
// (workflow.IdleRelease) and the store is what fills it, so neither of the two imports the other
// for it.
type IdleView struct {
	Issue       string
	Tree        string
	Found       bool
	Phase       phase.Phase
	Status      string
	Generation  uint64
	LingerUntil *time.Time
	Root        IdleRoot
	Claims      []IdleClaim
	// Waiting is whether a task, notice, message or start of the issue waits for delivery to a
	// role of its pod. The rule reads only whether there is one, so the store reads only that.
	Waiting bool
	// Releasing is whether a release of this pod is already marked in progress, which no second
	// release starts behind.
	Releasing bool
	// BundleUnrestored is whether the issue holds a saved workspace bundle whose restore is not
	// recorded: work a release took out of a pod that no pod has put back. Releasing such a pod
	// again would store a bundle over it, taken from a workspace that never had that work, so
	// the rule keeps it until the restore is confirmed.
	BundleUnrestored bool
	// Holding is whether a hold streak of this pod is running (store's pod_holds). The rule
	// never reads it: it is here because the daemon's pass needs it to know whether it has a
	// streak to end, and the view is the one read of the pod that pass already makes.
	Holding bool
}

// IsRoot is whether the issue is its tree's root, the one issue a root's own wait on a person
// holds: the children of a root that waits on a person are released like any other issue.
func (v IdleView) IsRoot() bool { return v.Issue == v.Tree }

// Standing is where one issue stands in the three plain fields the pod gate reads: its phase,
// its Dispatch status and whether it lingers after a close.
type Standing struct {
	Phase   phase.Phase
	Status  string
	Lingers bool
}

// Standing is the issue's own standing, and RootStanding its tree root's.
func (v IdleView) Standing() Standing {
	return Standing{Phase: v.Phase, Status: v.Status, Lingers: v.LingerUntil != nil}
}

func (v IdleView) RootStanding() Standing {
	return Standing{Phase: v.Root.Phase, Status: v.Root.Status, Lingers: v.Root.LingerUntil != nil}
}

// InWorkflowNotLingering is whether an issue and its tree root are both still the workflow's to
// run: neither carries a status that takes an issue out of the workflow (outOfWorkflow, which is
// record.OutOfWorkflow — this package sits below record, so the rule is passed in rather than
// restated), neither is done, and neither lingers after its close.
//
// It is the one gate every path that acts on an idle or released pod reads — the idle rule, a
// wake applied inside a fact, a wake the outbox takes for a delivery it holds, and the store's
// own authorization of a release's teardown — because a pod whose tree has stopped being the
// workflow's is a close's to stop, never a release's to take or a wake's to bring back. Those
// four sit in three packages that cannot import one another, which is why the rule lives here.
func InWorkflowNotLingering(issue, root Standing, outOfWorkflow func(status string) bool) bool {
	return issue.inWorkflow(outOfWorkflow) && root.inWorkflow(outOfWorkflow)
}

func (s Standing) inWorkflow(outOfWorkflow func(status string) bool) bool {
	return !outOfWorkflow(s.Status) && s.Phase != phase.Done && !s.Lingers
}

// QuietSince is when anything of the pod was last recorded — the newest claim write of any of its
// claims, state and activity report alike — and false while the pod has no claim at all.
func (v IdleView) QuietSince() (time.Time, bool) {
	var newest time.Time
	for _, c := range v.Claims {
		if c.UpdatedAt.After(newest) {
			newest = c.UpdatedAt
		}
	}
	return newest, !newest.IsZero()
}
