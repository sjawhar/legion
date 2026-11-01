package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// idleNow is the moment every row of the table decides at, and idlePeriod the idle period it
// decides against: a pod that has recorded nothing for ten minutes is released.
var (
	idleNow      = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	idlePeriod   = 10 * time.Minute
	idleContract = 47
)

// idleChild is a child issue's pod that every condition of the rule holds for: in the workflow
// under a live root, on the configured image, one live implementer on the daemon's contract that
// finished its turn twenty minutes ago and reported five minutes later that it runs no background
// work with no reminder armed, a tester the daemon suspended beside it, and nothing queued for
// either. Each row of the table below changes exactly one thing about it.
func idleChild() store.IdleView {
	return store.IdleView{
		Issue: "LEGION-209", Tree: "LEGION-208", Found: true,
		Phase: phase.Implementing, Status: "in_progress", Generation: 3,
		Root: store.IdleRoot{Key: "LEGION-208", Phase: phase.Admitted, Status: "in_progress", Generation: 2},
		Claims: []store.IdleClaim{
			{
				Token: "legion-legion-209-implementer", Role: claim.RoleImplementer,
				State: supervise.StateIdle, PluginContract: idleContract, ServingGeneration: 3,
				LastTurnAt: idleNow.Add(-20 * time.Minute),
				ActivityAt: idleNow.Add(-15 * time.Minute), UpdatedAt: idleNow.Add(-15 * time.Minute),
			},
			{
				Token: "legion-legion-209-tester", Role: claim.RoleTester,
				State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon,
				PluginContract: 11, UpdatedAt: idleNow.Add(-40 * time.Minute),
			},
		},
	}
}

// idleRoot is the same pod as its tree's root, which is the only shape a root's own wait on a
// person holds.
func idleRoot() store.IdleView {
	view := idleChild()
	view.Issue, view.Tree, view.Phase = "LEGION-208", "LEGION-208", phase.Admitted
	view.Generation = 2
	view.Root.Key = "LEGION-208"
	view.Claims = view.Claims[:1]
	view.Claims[0].Token, view.Claims[0].Role = "legion-legion-208-architect", claim.RoleArchitect
	view.Claims[0].ServingGeneration = 2
	return view
}

// Every condition of the idle rule is one row here: it holds the pod and names the hold the
// daemon records, and the pod with every condition met is released with its live roles named.
// The rule is pure, so each row is the view, the clock, the idle period, the image verdict and
// the daemon's plugin contract and nothing else.
//
// One condition the plan names is deliberately absent: "no INFRA-484 update round open on the
// pod" reads supervise.Claim.UpdateRound, a field this branch does not have. It arrives with
// sjawhar/legion#2538, whose forward merge adds its row here.
func TestTheIdleRuleNamesTheOneConditionThatHeldEachPod(t *testing.T) {
	for _, tc := range []struct {
		name  string
		view  store.IdleView
		image bool
		want  IdleHold
		roles []claim.Role
	}{
		{
			name: "every condition holds", view: idleChild(), image: true, want: IdleHoldNone,
			roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name:  "the record holds no such issue",
			view:  func() store.IdleView { v := idleChild(); v.Found = false; return v }(),
			image: true, want: IdleHoldNoPod,
		},
		{
			name: "the pod has no live claim",
			view: func() store.IdleView {
				v := idleChild()
				v.Claims[0].State = supervise.StateRetired
				return v
			}(),
			image: true, want: IdleHoldNoPod,
		},
		{
			name:  "a release of the pod is already in progress",
			view:  func() store.IdleView { v := idleChild(); v.Releasing = true; return v }(),
			image: true, want: IdleHoldReleasing,
		},
		{
			name:  "the issue has left the workflow",
			view:  func() store.IdleView { v := idleChild(); v.Status = "backlog"; return v }(),
			image: true, want: IdleHoldTreeNotLive,
		},
		{
			name:  "the tree root lingers",
			view:  func() store.IdleView { v := idleChild(); v.Root.Lingering = true; return v }(),
			image: true, want: IdleHoldTreeNotLive,
		},
		{
			name: "the root waits on a person who has not answered",
			view: func() store.IdleView {
				v := idleRoot()
				v.Phase, v.Root.Phase, v.Root.AwaitingPerson = phase.AwaitingPerson, phase.AwaitingPerson, true
				return v
			}(),
			image: true, want: IdleHoldAwaitingPerson,
		},
		{
			name: "the root's person has answered and it waits only for a slot",
			view: func() store.IdleView {
				v := idleRoot()
				v.Phase, v.Root.Phase = phase.AwaitingPerson, phase.AwaitingPerson
				v.Root.AwaitingPerson, v.Root.Answered = true, true
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleArchitect},
		},
		{
			name: "a child of a root that waits on a person",
			view: func() store.IdleView {
				v := idleChild()
				v.Root.Phase, v.Root.AwaitingPerson = phase.AwaitingPerson, true
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "the pod does not run the configured image", view: idleChild(), image: false, want: IdleHoldImage,
		},
		{
			name:  "a live claim speaks an older plugin contract",
			view:  func() store.IdleView { v := idleChild(); v.Claims[0].PluginContract = idleContract - 1; return v }(),
			image: true, want: IdleHoldPluginContract,
		},
		{
			name:  "a role is in a turn",
			view:  func() store.IdleView { v := idleChild(); v.Claims[0].State = supervise.StateWorking; return v }(),
			image: true, want: IdleHoldInTurn,
		},
		{
			name:  "a role's launch is not yet known",
			view:  func() store.IdleView { v := idleChild(); v.Claims[0].State = supervise.StateLaunchUncertain; return v }(),
			image: true, want: IdleHoldInTurn,
		},
		{
			name: "a task waits for delivery to a role of the pod",
			view: func() store.IdleView {
				v := idleChild()
				v.Waiting = []store.IdleWaiting{{ID: 41, Kind: "supervise", Role: claim.RoleImplementer}}
				return v
			}(),
			image: true, want: IdleHoldDelivery,
		},
		{
			name: "a live role's session reported background work of its own",
			view: func() store.IdleView {
				v := idleChild()
				v.Claims[0].ActivityBackground = true
				v.Claims[0].ActivityKinds = []string{"async-job"}
				return v
			}(),
			image: true, want: IdleHoldBackgroundWork,
		},
		{
			name:  "a live role's session has reported nothing",
			view:  func() store.IdleView { v := idleChild(); v.Claims[0].ActivityAt = time.Time{}; return v }(),
			image: true, want: IdleHoldUnreported,
		},
		{
			name: "a live role's report predates its last turn",
			view: func() store.IdleView {
				v := idleChild()
				v.Claims[0].ActivityAt = v.Claims[0].LastTurnAt.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldUnreported,
		},
		{
			name: "an open phase's reminder is due inside the period",
			view: func() store.IdleView {
				v := idleChild()
				v.Claims[0].ActivityNextReminderAt = idleNow.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldReminder,
		},
		{
			name: "an open phase's reminder is due after the period",
			view: func() store.IdleView {
				v := idleChild()
				v.Claims[0].ActivityNextReminderAt = idleNow.Add(5 * time.Minute)
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "a reminder of a worker whose phase is not open",
			view: func() store.IdleView {
				v := idleChild()
				v.Phase = phase.Reviewing
				v.Claims[0].ActivityNextReminderAt = idleNow.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "a reminder of a worker serving an earlier generation",
			view: func() store.IdleView {
				v := idleChild()
				v.Claims[0].ServingGeneration = 2
				v.Claims[0].ActivityNextReminderAt = idleNow.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldNone, roles: []claim.Role{claim.RoleImplementer},
		},
		{
			name: "the pod recorded something inside the idle period",
			view: func() store.IdleView {
				v := idleChild()
				v.Claims[0].UpdatedAt = idleNow.Add(-time.Minute)
				return v
			}(),
			image: true, want: IdleHoldNotIdle,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := IdleRelease(tc.view, idleNow, idlePeriod, tc.image, idleContract)
			if got.Hold != tc.want {
				t.Fatalf("hold = %s, want %s", got.Hold, tc.want)
			}
			if want := tc.want == IdleHoldNone; got.Release != want {
				t.Fatalf("release = %v, want %v (hold %s)", got.Release, want, got.Hold)
			}
			if len(got.Roles) != len(tc.roles) {
				t.Fatalf("roles = %v, want %v", got.Roles, tc.roles)
			}
			for i, role := range tc.roles {
				if got.Roles[i] != role {
					t.Fatalf("roles = %v, want %v", got.Roles, tc.roles)
				}
			}
		})
	}
}

// Every hold the rule can report has a name of its own, which is what the daemon records of a pod
// it kept: two holds that printed the same would make the record unreadable.
func TestEveryIdleHoldHasANameOfItsOwn(t *testing.T) {
	seen := map[string]IdleHold{}
	for hold := IdleHoldNone; hold <= IdleHoldBundleSave; hold++ {
		name := hold.String()
		if name == "unknown" {
			t.Fatalf("hold %d has no name", int(hold))
		}
		if other, taken := seen[name]; taken {
			t.Fatalf("holds %d and %d are both %q", int(other), int(hold), name)
		}
		seen[name] = hold
	}
	if got := IdleHold(len(seen) + 1).String(); got != "unknown" {
		t.Fatalf("an unmapped hold prints %q, want unknown", got)
	}
}
