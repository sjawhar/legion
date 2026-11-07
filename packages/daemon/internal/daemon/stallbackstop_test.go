package daemon

// The daemon's half of the stall backstop: the pass over the claims the orphan sweep already
// read, which ends the turn of every claim whose recorded last step is older than its role's
// bound and tells the owner once per stall. It is driven directly over a bare supervisor (no
// store, no HTTP), the construction plugin_test.go's boot cases use, because what it decides is
// decided from the claims it is handed and the configuration it holds.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// backstopBound is the bound the rig gives every role it tests, short enough to write out and
// long enough that no claim of the rig is stalled by accident.
const backstopBound = 4 * time.Hour

// backstopRig is a bare supervisor over the fake runtime, the supervision that sweeps it, and
// the outbox rows that supervision wrote.
type backstopRig struct {
	t      *testing.T
	s      *supervision
	sup    *supervisor
	rt     *fake.Runtime
	rows   map[string][]record.OutboxPayload
	refuse error
}

// newBackstopRig builds the rig with a bound for every role it drives but the merger, whose 0 is
// the deployment switching the backstop off for that role.
func newBackstopRig(t *testing.T) *backstopRig {
	t.Helper()
	sup, rt := newPluginSupervisor(t, api.DaemonAPIVersion, true)
	rig := &backstopRig{t: t, sup: sup, rt: rt, rows: map[string][]record.OutboxPayload{}}
	cfg := config.Config{
		Project: "legion",
		WorkerStallBackstops: map[claim.Role]time.Duration{
			claim.RoleImplementer: backstopBound,
			claim.RoleTester:      backstopBound,
			claim.RolePlanner:     backstopBound,
			claim.RoleController:  backstopBound,
			claim.RoleMerger:      0,
		},
	}
	rig.s = &supervision{
		cfg: cfg, log: quietLogger(), supervisor: sup, plan: plan{clock: stillClock{}},
		enqueue: func(_ context.Context, issue string, payloads ...record.OutboxPayload) error {
			if rig.refuse != nil {
				return rig.refuse
			}
			rig.rows[issue] = append(rig.rows[issue], payloads...)
			return nil
		},
	}
	return rig
}

// claim builds a claim of role, working a task, whose record says its agent last stepped silent
// ago; call says whether that record names a call the agent is in, a claim with none carrying
// its turn's start alone. It is handed to the supervisor as the boot's restore hands it.
func (r *backstopRig) claim(role claim.Role, issue string, silent time.Duration, call bool) supervise.Claim {
	r.t.Helper()
	project, err := claim.ProjectToken("legion")
	if err != nil {
		r.t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	now := time.Now()
	at := now.Add(-silent)
	c := supervise.Claim{
		Token: token, Project: project, Tree: issue, Issue: issue, Role: role, Generation: 1,
		State: supervise.StateWorking, Session: "ses_" + string(role), SessionFile: "/sessions/" + string(role) + ".jsonl",
		PluginContract: api.DaemonAPIVersion, Locator: pluginTestLocator(token),
		Pending: &supervise.Delivery{ID: "delivery-" + string(role), Task: "work the phase",
			QueuedAt: at.Add(-time.Hour), DeliveredAt: at.Add(-time.Hour), ConfirmedAt: at.Add(-time.Hour)},
		Step: supervise.StepRecord{TurnStartedAt: at},
	}
	if call {
		c.Step.At, c.Step.StartedAt = at, at
		c.Step.TurnStartedAt = at.Add(-time.Hour)
		c.Step.Tool, c.Step.CallID, c.Step.ArgsBytes, c.Step.ArgsHash = "bash", "toolu_impl_11", 128, "9f2b7c0d1e3a4b5c"
	}
	return c
}

// supervising hands the claims to the supervisor, as the boot's restore does, and returns them
// for the sweep to read.
func (r *backstopRig) supervising(claims ...supervise.Claim) []supervise.Claim {
	r.t.Helper()
	if _, err := r.sup.restore(context.Background(), claims); err != nil {
		r.t.Fatalf("restore: %v", err)
	}
	return claims
}

// sweep runs one orphan-sweep pass of the backstop. What it posts is decided by each claim's own
// machine, on its own goroutine, so a case reads the outcome through awaitResumed or settled.
func (r *backstopRig) sweep(claims []supervise.Claim) {
	r.t.Helper()
	r.s.backstopStalledTurns(context.Background(), claims)
}

// settled waits for every event the sweep posted for c to have been decided: a tree close of its
// own, posted through the same inbox and so behind whatever the sweep put there, which the
// machine answers by retiring the claim. A case that must see nothing happen reads its
// assertions after this.
func (r *backstopRig) settled(c supervise.Claim) {
	r.t.Helper()
	r.sup.post(c.Token, supervise.RequestTreeClose{Claim: c.Token})
	testwait.Eventually(r.t, "the claim to answer a close posted behind the sweep's own events", func() bool {
		m, ok := r.sup.Machine(c.Token)
		return ok && m.Claim().State == supervise.StateRetired
	})
}

// awaitResumed waits for the backstop's relaunch of token's session.
func (r *backstopRig) awaitResumed(token claim.Token, want int) {
	r.t.Helper()
	testwait.Eventually(r.t, "the stalled claim's session to be brought back", func() bool {
		return len(r.resumes(token)) == want
	})
}

// notices are the notice rows supervision wrote on issue.
func (r *backstopRig) notices(issue string) []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	for _, payload := range r.rows[issue] {
		if notice, ok := payload.(record.Notice); ok {
			notices = append(notices, notice)
		}
	}
	return notices
}

// resumes are the relaunches the runtime was asked to make for token.
func (r *backstopRig) resumes(token claim.Token) []fake.Call {
	r.t.Helper()
	var made []fake.Call
	for _, call := range r.rt.CallsOf("Resume") {
		if call.Spec.Claim == token {
			made = append(made, call)
		}
	}
	return made
}

// A claim whose recorded last step is older than its role's bound has its turn ended by the
// daemon — its session resumed on a new process, with the task it was serving still owed — and
// its owner is told once, with the record in the notice: how long the agent was silent, the
// bound it passed, when the turn began and the call the record names. The arguments of that call
// are in no part of the notice, as they are in no column.
func TestTheStallBackstopEndsAStalledTurnAndTellsTheOwnerOnce(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleImplementer, "LEGION-209", backstopBound+30*time.Minute, true)
	claims := rig.supervising(stalled)

	rig.sweep(claims)

	rig.awaitResumed(stalled.Token, 1)
	m, ok := rig.sup.Machine(stalled.Token)
	if !ok {
		t.Fatal("the stalled claim is not supervised")
	}
	if held := m.Claim(); held.Pending == nil || !held.Pending.Interrupted {
		t.Fatalf("the claim's pending task after the backstop = %+v, want the task it was serving still owed", held.Pending)
	}
	if held := m.Claim(); held.Session != "ses_implementer" || held.Budgets != (supervise.Budgets{}) {
		t.Fatalf("the claim after the backstop = %+v, want the same session and nothing charged", held)
	}
	notices := rig.notices("LEGION-209")
	if len(notices) != 1 {
		t.Fatalf("notices on the issue = %+v, want one", notices)
	}
	notice := notices[0]
	if notice.Kind != "worker-stalled" || notice.Role != claim.RoleImplementer {
		t.Fatalf("the notice = %+v, want a worker-stalled of the implementer", notice)
	}
	for _, want := range []string{
		"the daemon ended the implementer's turn",
		"4h30m0s ago",
		"past the 4h0m0s this deployment gives the role",
		"bash (toolu_impl_11)",
		"has not ended",
		"still owed",
	} {
		if !strings.Contains(notice.Reason, want) {
			t.Errorf("the notice says %q, which does not carry %q", notice.Reason, want)
		}
	}
	if strings.Contains(notice.Reason, "9f2b7c0d1e3a4b5c") {
		t.Errorf("the notice carries the call's fingerprint: %q", notice.Reason)
	}

	// The next sweep reads the same stall — this snapshot is what a stop that changed nothing
	// would leave — and posts the backstop again, which the claim decides as it stands then. The
	// owner is told once per stall, however many sweeps find it.
	rig.sweep(claims)
	rig.settled(stalled)
	if notices := rig.notices("LEGION-209"); len(notices) != 1 {
		t.Fatalf("notices after the second sweep = %+v, want the owner told once per stall", notices)
	}
}

// A claim whose agent has sent nothing since its turn began is judged from that turn's start,
// which is the only moment its record holds.
func TestTheStallBackstopJudgesAClaimWithNoCallFromItsTurnsStart(t *testing.T) {
	rig := newBackstopRig(t)
	stalled := rig.claim(claim.RoleTester, "LEGION-210", backstopBound+time.Minute, false)
	claims := rig.supervising(stalled)

	rig.sweep(claims)

	rig.awaitResumed(stalled.Token, 1)
	notices := rig.notices("LEGION-210")
	if len(notices) != 1 || !strings.Contains(notices[0].Reason, "the daemon saw no call of this turn") {
		t.Fatalf("notices = %+v, want one naming a turn with no call the daemon saw", notices)
	}
}

// What the pass never touches, each for its own reason. The negative control of the whole rule
// is the first two: an agent that is sending frames is never touched however long its turn runs,
// and a role whose bound is 0 is never touched by this rule at all.
func TestTheStallBackstopLeavesTheseClaimsAlone(t *testing.T) {
	for name, build := range map[string]func(*backstopRig) supervise.Claim{
		"an agent still stepping inside its bound": func(r *backstopRig) supervise.Claim {
			return r.claim(claim.RoleImplementer, "LEGION-211", backstopBound-time.Minute, true)
		},
		"a role whose bound is 0, however long its agent is silent": func(r *backstopRig) supervise.Claim {
			return r.claim(claim.RoleMerger, "LEGION-212", 50*backstopBound, true)
		},
		"a role whose bound is 0 and whose turn made no call at all": func(r *backstopRig) supervise.Claim {
			return r.claim(claim.RoleMerger, "LEGION-213", 50*backstopBound, false)
		},
		"a claim in no turn": func(r *backstopRig) supervise.Claim {
			c := r.claim(claim.RoleImplementer, "LEGION-214", backstopBound+time.Hour, true)
			c.State = supervise.StateIdle
			return c
		},
		"a claim an update round is carrying": func(r *backstopRig) supervise.Claim {
			c := r.claim(claim.RoleImplementer, "LEGION-215", backstopBound+time.Hour, true)
			c.UpdateRound, c.UpdateReasons = supervise.PodRound("9d3f0c7a"), "image moved"
			return c
		},
		"a claim whose record holds nothing at all": func(r *backstopRig) supervise.Claim {
			c := r.claim(claim.RoleImplementer, "LEGION-216", backstopBound+time.Hour, true)
			c.Step = supervise.StepRecord{}
			return c
		},
		"a role the configuration names no bound for, inside the ladder it defaults to": func(r *backstopRig) supervise.Claim {
			return r.claim(claim.RoleReviewer, "LEGION-217", 3*time.Hour, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newBackstopRig(t)
			left := build(rig)
			claims := rig.supervising(left)

			rig.sweep(claims)
			rig.settled(left)

			if resumes := rig.resumes(left.Token); len(resumes) != 0 {
				t.Fatalf("resumes = %d, want the claim left alone", len(resumes))
			}
			if notices := rig.notices(left.Issue); len(notices) != 0 {
				t.Fatalf("notices = %+v, want nobody told", notices)
			}
		})
	}
}

// The project controller's claim is on no issue, so there is no owner to tell: its turn is ended
// like any other, and the record is in the line the daemon logs.
func TestTheStallBackstopEndsAStalledControllerAndWritesOnNoIssue(t *testing.T) {
	rig := newBackstopRig(t)
	project, err := claim.ProjectToken("legion")
	if err != nil {
		t.Fatal(err)
	}
	controller := rig.claim(claim.RolePlanner, "LEGION-218", backstopBound+time.Hour, true)
	controller.Token = claim.ControllerToken(project)
	controller.Role, controller.Issue, controller.Tree = claim.RoleController, "", ""
	controller.Locator = pluginTestLocator(controller.Token)
	claims := rig.supervising(controller)

	rig.sweep(claims)

	rig.awaitResumed(controller.Token, 1)
	for issue, rows := range rig.rows {
		t.Fatalf("rows written on %q = %+v, want none: a controller's claim is on no issue", issue, rows)
	}
}

// A notice the store refuses is told again at the next sweep, as a stalled update round's is:
// the mark that says the owner was told is set only where the row was taken.
func TestTheStallBackstopTellsTheOwnerAgainWhenTheRowIsRefused(t *testing.T) {
	rig := newBackstopRig(t)
	rig.refuse = errors.New("the store is down")
	stalled := rig.claim(claim.RolePlanner, "LEGION-219", backstopBound+time.Hour, true)
	claims := rig.supervising(stalled)

	rig.sweep(claims)
	rig.awaitResumed(stalled.Token, 1)
	if notices := rig.notices("LEGION-219"); len(notices) != 0 {
		t.Fatalf("notices = %+v, want none taken", notices)
	}

	rig.refuse = nil
	rig.sweep(claims)
	rig.settled(stalled)

	if notices := rig.notices("LEGION-219"); len(notices) != 1 {
		t.Fatalf("notices after the refusal cleared = %+v, want the owner told then", notices)
	}
}

// A tree architect's own stall is told to the controller as well, in a row of its own: every
// other notice of the tree reaches that architect, so nobody outside it would hear of the turn
// the daemon ended. A worker's stall is its architect's alone.
func TestTheStallBackstopTellsTheControllerOfATreeArchitectsStall(t *testing.T) {
	rig := newBackstopRig(t)
	root := rig.claim(claim.RolePlanner, "LEGION-220", backstopBound+time.Hour, true)
	root.Role = claim.RoleArchitect
	rig.s.cfg.WorkerStallBackstops[claim.RoleArchitect] = backstopBound
	claims := rig.supervising(root)

	rig.sweep(claims)

	rig.awaitResumed(root.Token, 1)
	rows := rig.rows["LEGION-220"]
	if len(rows) != 2 {
		t.Fatalf("rows on the root issue = %+v, want the architect's notice and the controller's", rows)
	}
	notice, isNotice := rows[0].(record.Notice)
	controllers, isController := rows[1].(record.ControllerNotice)
	if !isNotice || !isController || notice.Kind != "worker-stalled" {
		t.Fatalf("rows = %+v, want a worker-stalled notice and the controller's copy of it", rows)
	}
	if !reflect.DeepEqual(record.Notice(controllers), notice) {
		t.Fatalf("the controller's copy = %+v, want the architect's own %+v", controllers, notice)
	}

	// A worker of the same tree tells the architect alone.
	worker := rig.claim(claim.RoleTester, "LEGION-220", backstopBound+time.Hour, true)
	rig.sweep(rig.supervising(worker))
	rig.settled(worker)
	if rows := rig.rows["LEGION-220"]; len(rows) != 3 {
		t.Fatalf("rows after the worker's stall = %+v, want one more, the architect's alone", rows)
	}
}
