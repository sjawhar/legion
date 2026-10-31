package daemon

// Every way a phase holder's claim comes back to ready ends in its phase's task, within two
// minutes of that ready: dispatch://INFRA-714 R7, acceptance A9. An operator's resume on
// 2026-10-30 brought about eight phase holders back ready with no task, and each sat idle for two
// to five hours. The three come-backs here are the ones a claim has — an operator's suspend and
// resume, a relaunch after a death, and a resume after the daemon replaced the pod (the displaced
// path) — each driven through the supervisor that observes it, the ready fact it fires
// (workflowRuntime.terminal), the workflow's readyWorkerTask and the outbox row it queues, to the
// task the agent is prompted with.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// taskWithinOfReady is how long a phase holder may stand ready with no task before the daemon has
// failed it (dispatch://INFRA-714 acceptance A9). It is measured where an operator reads it, over
// the two times the claim view carries: the claim's last ready (supervise.Claim.LastReady) and the
// acknowledgement of the task's prompt (supervise.Delivery.DeliveredAt).
const taskWithinOfReady = 2 * time.Minute

// readyWorkerRig is a tester at work in its issue's testing phase, as the daemon leaves one: the
// start row that moved the issue into testing created and launched its claim, its agent said it
// was ready, and that row's task was delivered to it. Its supervisor fires the ready fact the
// daemon fires (workflowRuntime.terminal) and keeps its claims where the workflow reads them, in
// Postgres, so readyWorkerTask sees the claim and its pending task as the daemon's does.
type readyWorkerRig struct {
	t       *testing.T
	pool    *pgxpool.Pool
	sup     *supervisor
	rt      *fake.Runtime
	runner  *outbox
	machine *supervise.Machine
	conn    *fake.Conn
	token   claim.Token
	issue   record.Issue
}

// newReadyWorkerRig starts the tester of a testing issue and prompts it with the start's task.
func newReadyWorkerRig(t *testing.T) *readyWorkerRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true}
	putOutboxIssue(t, pool, records, issue)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", issue.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = st
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	engine := workflow.New(records, workflow.Config{Project: issue.Project, Repositories: testRepositories}, quietLogger())
	sup.OnTerminal((&workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	rig := &readyWorkerRig{t: t, pool: pool, sup: sup, rt: rt, issue: issue,
		runner: &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
			trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), handlers: []intake.Handler{engine},
			// The outbox reads the clock to lease rows; a minute on runs every row the ready
			// queued in the same pass. The two times the bound is read from are the supervisor's.
			now: func() time.Time { return time.Now().Add(time.Minute) },
			provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
			},
		},
	}
	rig.token = mustClaimToken(t, issue.Key, claim.RoleTester)
	rig.conn = fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(rig.token, rig.conn)
	// The start that moved the issue into testing: it creates the claim, launches it and hands it
	// the phase's task, which waits for the agent to be ready.
	if err := rig.runner.execute(ctx, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree,
		Role: claim.RoleTester, Generation: issue.Generation, Phase: phase.Testing, Task: "Test it."}, time.Now())); err != nil {
		t.Fatalf("start the tester: %v", err)
	}
	machine, found := sup.Machine(rig.token)
	if !found {
		t.Fatalf("the start left no claim for the tester of %s", issue.Key)
	}
	rig.machine = machine
	rig.boot()
	if held := rig.claim().Pending; held == nil || held.Task != "Test it." {
		t.Fatalf("the started tester holds %+v, want the start's task", held)
	}
	return rig
}

// boot drives the launched process through its shim's hello, its agent's registration and its
// ready, at the generation the claim is launching, and waits for what the ready set going.
func (r *readyWorkerRig) boot() {
	r.t.Helper()
	ctx := context.Background()
	generation := r.claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.token, Generation: generation},
		supervise.RequestRegister{Claim: r.token, Generation: generation, Session: "ses-tester", SessionFile: "/tmp/tester.jsonl"},
		supervise.RequestReady{Claim: r.token, Generation: generation, Session: "ses-tester"},
	} {
		if err := r.machine.Handle(ctx, ev); err != nil {
			r.t.Fatalf("boot the tester: handle %T: %v", ev, err)
		}
	}
	r.machine.Wait()
}

// workTheTask runs the turn the delivered task starts and ends it, which retires the task: the
// tester is then the phase holder the issue waits on, idle with nothing pending — where CI, a
// review or an operator's resume leaves one.
func (r *readyWorkerRig) workTheTask() {
	r.t.Helper()
	ctx := context.Background()
	for _, ev := range []supervise.Event{supervise.StreamTurnStart{Claim: r.token}, supervise.StreamTurnEnd{Claim: r.token}} {
		if err := r.machine.Handle(ctx, ev); err != nil {
			r.t.Fatalf("work the task: handle %T: %v", ev, err)
		}
	}
	r.machine.Wait()
	if held := r.claim().Pending; held != nil {
		r.t.Fatalf("after its turn the tester still holds %+v, want the task retired with the turn", held)
	}
}

// run runs the outbox once and waits for every machine it drove.
func (r *readyWorkerRig) run() {
	r.t.Helper()
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("run the outbox: %v", err)
	}
	r.machine.Wait()
}

func (r *readyWorkerRig) claim() supervise.Claim { return r.machine.Claim() }

// relaunched is how many times the runtime was asked to resume the claim's session, which every
// come-back here does once: the process the come-back ended is replaced by one on the session the
// claim keeps.
func (r *readyWorkerRig) relaunched() int { return len(resumesOf(r.rt, r.token)) }

// observe hands the machine what the runtime now says about the claim's process.
func (r *readyWorkerRig) observe(kind runtime.ObservationKind, displaced bool) {
	r.t.Helper()
	if err := r.machine.Handle(context.Background(), supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *r.claim().Locator, Kind: kind, Displaced: displaced, At: time.Now()}}); err != nil {
		r.t.Fatalf("observe the tester's process as %s: %v", kind, err)
	}
	r.machine.Wait()
}

// comeBacks are the ways a phase holder's claim comes back to ready, each ending in the relaunched
// process booting its agent.
var comeBacks = map[string]func(*readyWorkerRig){
	"an operator's suspend and resume": func(r *readyWorkerRig) {
		r.t.Helper()
		ctx := context.Background()
		operatorSuspend(r.t, r.sup, r.token)
		if err := r.machine.Handle(ctx, supervise.RequestResume{Claim: r.token}); err != nil {
			r.t.Fatalf("the operator's resume: %v", err)
		}
		r.machine.Wait()
		r.boot()
	},
	"a relaunch after a death": func(r *readyWorkerRig) {
		r.t.Helper()
		r.observe(runtime.Gone, false)
		r.boot()
	},
	"a resume after a pod replacement": func(r *readyWorkerRig) {
		r.t.Helper()
		r.observe(runtime.Gone, true)
		r.boot()
	},
}

// A phase holder that comes back ready with nothing pending is given its phase's task, whichever
// way it came back, and within taskWithinOfReady of that ready: the resume relaunches the session,
// the ready fires the workflow fact, and the row it queues reaches the agent in the outbox pass
// that follows. Before, nothing was sent at all — the start row that moved the issue into testing
// had run long before the restart — and the issue waited for as long as the worker did.
func TestEveryComeBackToReadyGivesAPhaseHolderItsTaskWithinTwoMinutes(t *testing.T) {
	for name, comeBack := range comeBacks {
		t.Run(name, func(t *testing.T) {
			rig := newReadyWorkerRig(t)
			rig.workTheTask()
			if rows := outboxRows(t, rig.pool); rows != 0 {
				t.Fatalf("the tester that worked its task was queued %d outbox rows, want none before it comes back", rows)
			}

			comeBack(rig)

			if rows, relaunched := outboxRows(t, rig.pool), rig.relaunched(); rows != 1 || relaunched != 1 {
				t.Fatalf("the come-back relaunched the session %d times and left %d outbox rows, want one relaunch and the one start that carries the phase's task",
					relaunched, rows)
			}
			rig.run()
			held, last := rig.claim().Pending, rig.claim().LastReady
			if held == nil || held.Task != workflow.ResumePhaseTask(rig.issue) || held.Phase != phase.Testing {
				t.Fatalf("after the come-back the tester holds %+v, want the testing phase's resume task", held)
			}
			if held.DeliveredAt.IsZero() || held.DeliveredAt.Before(last) || held.DeliveredAt.Sub(last) > taskWithinOfReady {
				t.Fatalf("the task was acknowledged at %s, %s after the claim became ready at %s; want it within %s of that ready",
					held.DeliveredAt, held.DeliveredAt.Sub(last), last, taskWithinOfReady)
			}
		})
	}
}

// A phase holder that comes back still holding its task is given no second copy: the task it holds
// is the work under way, and a second prompt for it would be a second turn of the same work. The
// one it holds is sent again on the same delivery, which is the relaunch's own resend, not a task
// of its own.
func TestAComeBackWithATaskStillPendingGivesNoSecondCopy(t *testing.T) {
	for name, comeBack := range comeBacks {
		t.Run(name, func(t *testing.T) {
			rig := newReadyWorkerRig(t)
			delivered := rig.claim().Pending.ID
			if rows := outboxRows(t, rig.pool); rows != 0 {
				t.Fatalf("the first ready of a tester that holds its task left %d outbox rows, want none", rows)
			}

			comeBack(rig)

			if rows, relaunched := outboxRows(t, rig.pool), rig.relaunched(); rows != 0 || relaunched != 1 {
				t.Fatalf("the come-back of a tester that holds its task relaunched the session %d times and left %d outbox rows, want one relaunch and no row",
					relaunched, rows)
			}
			if held := rig.claim().Pending; held == nil || held.ID != delivered || held.Task != "Test it." {
				t.Fatalf("after the come-back the tester holds %+v, want the one task it held, %s", held, delivered)
			}
			for i, prompt := range rig.conn.Prompts() {
				if prompt.DeliveryID != delivered {
					t.Fatalf("prompt %d carried delivery %s, want every prompt to carry the one task the tester holds, %s", i+1, prompt.DeliveryID, delivered)
				}
			}
		})
	}
}
