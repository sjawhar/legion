package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	heldTaskRoot    = "LEGION-310"
	heldTaskChild   = "LEGION-311"
	heldTaskProject = "LEGION"
)

// A root architect's start task names the decisions a person made while nobody in the root's tree
// could hear them, and the claim's write of the delivery that task is queued as takes those rows
// from the start row and holds them: they stand where they always stood, under the issue each was
// decided on, and the delivery's confirmation is what tells them. Such a task waits on the claim,
// unsent, while its root is parked, lingering or waiting for its slot (supervise's
// Deps.RootHears), and the root's linger can hand a child of that tree to Legion meanwhile. The
// child is a tree of its own from then on, so a hold of a claim of the tree it left counts for
// nothing and the child's own architect names what was decided on it; the old architect's task
// must stop naming it, or the next run of the tree the child left is told a decision it is owed
// nothing of. The task is re-read where the machine owns it, as it is about to be sent
// (supervise's Deps.RootTask, the daemon's rootTask), and what the record still shows in this
// root's tree is what goes.
func TestAHeldTaskIsSentWithoutTheDecisionsAHandOverGaveToTheChild(t *testing.T) {
	rig := newHeldTaskRig(t)
	rig.waitForSlot()
	onTheRoot := rig.decide(heldTaskRoot)
	onTheChild := rig.decide(heldTaskChild)
	rig.runRow(rig.enqueueStart())
	pending := rig.pending()
	if pending == nil || !strings.Contains(pending.Task, onTheRoot) || !strings.Contains(pending.Task, onTheChild) {
		t.Fatalf("the architect's held task = %#v, want the start's task naming %s and %s", pending, onTheRoot, onTheChild)
	}
	if prompts := rig.conn.Prompts(); len(prompts) != 0 {
		t.Fatalf("the lingering root's architect was prompted %#v, want the task held while nobody hears it", prompts)
	}
	held := keptDecisions(t, rig.pool, rig.records, heldTaskRoot)
	if len(held) != 2 {
		t.Fatalf("%s keeps %s, want both decisions standing, held by the task the claim took: a held task's decisions are "+
			"told at its own confirmation, not when it is queued", heldTaskRoot, keptRefs(held))
	}
	for _, kept := range held {
		if kept.HeldBy.Claim != string(rig.token) || kept.HeldBy.Delivery != pending.ID {
			t.Fatalf("%s keeps %#v, want it held by the delivery the claim queued the task as", heldTaskRoot, kept)
		}
	}

	rig.linger()
	rig.handOver()
	if kept := keptDecisions(t, rig.pool, rig.records, heldTaskChild); len(kept) != 1 || kept[0].Ref != onTheChild {
		t.Fatalf("the handed-over child keeps %s, want the one decision the held task named of it", keptRefs(kept))
	}

	rig.readmit()
	rig.runRow(rig.enqueueStart())
	prompts := rig.conn.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("the architect was prompted %d times, want the held task sent once once its root ran again: %#v", len(prompts), prompts)
	}
	if !strings.Contains(prompts[0].Message, onTheRoot) {
		t.Fatalf("the prompt %q does not name %s, the decision still owed to this root", prompts[0].Message, onTheRoot)
	}
	if strings.Contains(prompts[0].Message, onTheChild) {
		t.Fatalf("the prompt %q names %s, which the hand-over gave to %s's own architect", prompts[0].Message, onTheChild, heldTaskChild)
	}
	// What was sent is what the store holds, so a restart sends no more than this one did.
	sent := rig.storedDelivery()
	if sent == nil || sent.Task != prompts[0].Message {
		t.Fatalf("the stored task = %#v, want the text the prompt carried", sent)
	}
	if named := rig.named(*sent); len(named.Decisions) != 1 || named.Decisions[0].Ref != onTheRoot {
		t.Fatalf("the stored task names %#v, want %s alone", named.Decisions, onTheRoot)
	}
	restored := rig.restoredPending()
	if restored == nil || restored.Task != prompts[0].Message {
		t.Fatalf("the task a restart restores = %#v, want the one this send carried", restored)
	}
}

// A held task the hand-over leaves naming nothing is retired rather than sent: everything it
// carried is owed to the child's architect now, the start row that queued it is recorded on the
// claim (supervise.Claim.DeliveredRows) so no retry gives it again, and a prompt would tell this
// architect a decision of a tree it no longer holds.
func TestAHeldTaskNamingOnlyTheHandedOverChildsDecisionsIsRetired(t *testing.T) {
	rig := newHeldTaskRig(t)
	rig.waitForSlot()
	onTheChild := rig.decide(heldTaskChild)
	rig.runRow(rig.enqueueStart())
	if pending := rig.pending(); pending == nil || !strings.Contains(pending.Task, onTheChild) {
		t.Fatalf("the architect's held task = %#v, want the start's task naming %s", pending, onTheChild)
	}

	rig.linger()
	rig.handOver()
	if kept := keptDecisions(t, rig.pool, rig.records, heldTaskChild); len(kept) != 1 || kept[0].Ref != onTheChild {
		t.Fatalf("the handed-over child keeps %s, want the one decision the held task named of it", keptRefs(kept))
	}

	rig.readmit()
	rig.runRow(rig.enqueueStart())
	if prompts := rig.conn.Prompts(); len(prompts) != 0 {
		t.Fatalf("the architect was prompted %#v, want a task that names nothing of its own tree retired rather than sent", prompts)
	}
	if pending := rig.pending(); pending != nil {
		t.Fatalf("the claim still holds %#v, want the held task retired", pending)
	}
	if stored := rig.storedDelivery(); stored != nil {
		t.Fatalf("the store still holds %#v, want the retired task gone from it", stored)
	}
}

// heldTaskRig is one root whose architect is up, with a child of its tree, over the real parts the
// daemon wires: the record store on a database of its own, the workflow engine, admission, the
// outbox runner, and a claim supervisor whose Deps.RootHears and Deps.RootTask are the production
// answers (workflowRuntime) and whose claim store is the daemon's own, so the task pending on the
// claim stands in `pending_task_deliveries` where a hand-over reads it.
type heldTaskRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	trees     *store.Store
	sup       *supervisor
	runner    *outbox
	conn      *fake.Conn
	token     claim.Token
	root      record.Issue
	child     record.Issue
	at        time.Time
	decided   int
}

func newHeldTaskRig(t *testing.T) *heldTaskRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: heldTaskProject, Repositories: testRepositories}, quietLogger())
	trees := outboxTreeStore(t, pool, heldTaskRoot)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = keptClaimStore{Store: trees}
	sup.deps.RootHears = (&workflowRuntime{pool: pool, records: records}).rootHears
	sup.deps.RootTask = (&workflowRuntime{pool: pool, records: records}).rootTask
	conn := fake.NewConn()
	token := mustClaimToken(t, heldTaskRoot, claim.RoleArchitect)
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	parent := heldTaskRoot
	rig := &heldTaskRig{
		t: t, ctx: ctx, pool: pool, records: records, engine: engine,
		admission: admit.New(records, engine, 2, heldTaskProject, quietLogger()),
		trees:     trees, sup: sup, conn: conn, token: token,
		at: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC),
		root: record.Issue{Key: heldTaskRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: heldTaskProject, Tree: heldTaskRoot, Title: "Root", Phase: phase.Admitted, Generation: 1,
			Status: "in_progress", Rank: "A", LastDispatchSeq: 1},
		// The child stands in the root's tree in a phase of its own, which is what makes it work
		// the linger's end hands to Legion with its run (record.Issue.CarriesRun).
		child: record.Issue{Key: heldTaskChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: heldTaskProject, Tree: heldTaskRoot, Parent: &parent, Title: "Child", Phase: phase.Planning,
			Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1},
	}
	rig.runner = &outbox{
		log: quietLogger(), pool: pool, dispatchProject: heldTaskProject, records: records, supervisor: sup,
		notices: &outboxPublisher{}, handlers: []intake.Handler{engine, rig.admission}, trees: trees,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + heldTaskRoot}, nil
		},
	}
	rig.put(rig.root)
	rig.put(rig.child)
	rig.launchArchitect()
	return rig
}

func (r *heldTaskRig) put(issue record.Issue) {
	r.t.Helper()
	putOutboxIssue(r.t, r.pool, r.records, issue)
}

// launchArchitect creates the root architect's claim, launches it and brings its agent up: the one
// session that goes on running across the root's park and its next run.
func (r *heldTaskRig) launchArchitect() {
	r.t.Helper()
	machine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: r.token, Project: "legion", Tree: heldTaskRoot,
		Issue: heldTaskRoot, Role: claim.RoleArchitect, State: supervise.StateQueued}, "")
	if err != nil {
		r.t.Fatalf("create the architect claim: %v", err)
	}
	if err := machine.Handle(r.ctx, supervise.RequestSpawn{Claim: r.token}); err != nil {
		r.t.Fatalf("spawn the architect: %v", err)
	}
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.token, Generation: launch},
		supervise.RequestRegister{Claim: r.token, Generation: launch, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"},
		supervise.RequestReady{Claim: r.token, Generation: launch, Session: "ses-architect"},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T for the architect: %v", ev, err)
		}
	}
	machine.Wait()
	if got := machine.Claim().State; !claimTookRole(got) {
		r.t.Fatalf("the architect after its ready = %s, want a claim that took its role", got)
	}
}

// decide is a person deciding on an ask of issue while nobody in the root's tree hears it, kept
// under the root by the one rule both paths that find such a decision follow
// (record.RelayParkedDecision). It returns the reference a start task of the root must name.
func (r *heldTaskRig) decide(issue string) string {
	r.t.Helper()
	r.decided++
	ref := fmt.Sprintf("dispatch://%s/ask/ask-%04d", issue, r.decided)
	r.at = r.at.Add(time.Minute)
	outcome := record.RelayNone
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		stored, err := r.records.Issue(r.ctx, tx, heldTaskRoot)
		if err != nil || stored == nil {
			return err
		}
		outcome, err = record.RelayParkedDecision(r.ctx, tx, r.records, issue, *stored, true,
			record.ParkedDecision{Ref: ref, Decision: record.DecisionAskAnswered, Actor: "user:alice", At: r.at}, 0, "")
		return err
	}); err != nil {
		r.t.Fatalf("keep the decision %s: %v", ref, err)
	}
	if outcome != record.RelayKept && outcome != record.RelaySent {
		r.t.Fatalf("the decision %s on %s was not kept: outcome %v", ref, issue, outcome)
	}
	return ref
}

// waitForSlot is the root handed to Legion and waiting in `todo` for an admission slot, with no
// slot of its own: a root with no architect to hear a person's decision (record.WaitsForSlot,
// workflow.ArchitectHears), which is the reading Deps.RootHears holds a start task to. Its tree is
// not lingering, so the start row of that task runs against the claim rather than finishing
// without acting.
func (r *heldTaskRig) waitForSlot() {
	r.t.Helper()
	r.root.Status, r.root.HandedOver, r.root.LingerUntil = "todo", true, nil
	r.put(r.root)
}

// linger is the root closed with its tree lingering, which is where the end of a linger hands a
// child still in the workflow to Legion (workflow's settleUnfinished).
func (r *heldTaskRig) linger() {
	r.t.Helper()
	until := time.Now().Add(time.Hour)
	r.root.Phase, r.root.Status, r.root.LingerUntil = phase.Done, "done", &until
	r.put(r.root)
}

// readmit is the root's next generation, as admission records one: running again, so its architect
// has a tree to hear for.
func (r *heldTaskRig) readmit() {
	r.t.Helper()
	r.root.Generation++
	r.root.Phase, r.root.Status, r.root.LingerUntil = phase.Admitted, "in_progress", nil
	r.put(r.root)
}

// enqueueStart queues the architect start of the root's generation, naming what the root keeps and
// claiming those rows for that row in one transaction, exactly as admission writes it, and returns
// the row's id.
func (r *heldTaskRig) enqueueStart() int64 {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		task, named, err := r.engine.ParkedDecisionTask(r.ctx, tx, r.root)
		if err != nil {
			return err
		}
		return r.engine.EnqueueNaming(r.ctx, tx, r.root.Key, r.root.Key, record.SuperviseRequest{Op: "start",
			Tree: r.root.Tree, Role: claim.RoleArchitect, Generation: r.root.Generation, Task: task, Kept: named},
			named, time.Now())
	}); err != nil {
		r.t.Fatalf("enqueue the architect start of generation %d: %v", r.root.Generation, err)
	}
	var id int64
	if err := r.pool.QueryRow(r.ctx, "select max(id) from outbox").Scan(&id); err != nil {
		r.t.Fatalf("read the start row just enqueued: %v", err)
	}
	return id
}

// runRow runs one outbox row as its lane runs it: leased, executed, and finished when it acted.
func (r *heldTaskRig) runRow(id int64) {
	r.t.Helper()
	var row record.OutboxRow
	if err := r.pool.QueryRow(r.ctx, "select id, kind, issue, payload from outbox where id = $1",
		id).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
		r.t.Fatalf("read outbox row %d: %v", id, err)
	}
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	if err := r.runner.execute(r.ctx, row); err != nil {
		r.t.Fatalf("run outbox row %d: %v", row.ID, err)
	}
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
	}); err != nil {
		r.t.Fatalf("finish outbox row %d: %v", row.ID, err)
	}
	if machine, found := r.sup.Machine(r.token); found {
		machine.Wait()
	}
}

// handOver is the linger's end for the child: the durable fence the workflow writes and the
// labelled todo Dispatch then shows for it, which admission re-admits as an orphan carrying its
// run and promotes into a tree of its own.
func (r *heldTaskRig) handOver() {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutHandover(r.ctx, tx, record.Handover{Issue: heldTaskChild, Tree: r.root.Tree,
			Generation: r.root.Generation})
	}); err != nil {
		r.t.Fatalf("record the hand-over of %s: %v", heldTaskChild, err)
	}
	seq := r.child.LastDispatchSeq + 1
	if _, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", fmt.Sprintf("hand-over:%s:%d", heldTaskChild, seq),
		intake.DispatchIssue{Key: heldTaskChild, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated",
			Status: "todo", Title: r.child.Title, Parent: heldTaskRoot, Rank: r.child.Rank, HandedOver: true},
		r.engine, r.admission); err != nil {
		r.t.Fatalf("apply the hand-over of %s: %v", heldTaskChild, err)
	}
	carried := readIssue(r.t, r.pool, heldTaskChild)
	if carried == nil || carried.Tree != heldTaskChild {
		r.t.Fatalf("the handed-over child = %+v, want a tree of its own", carried)
	}
	r.child = *carried
}

// pending is the task the architect's machine holds now, nil for none.
func (r *heldTaskRig) pending() *supervise.Delivery {
	r.t.Helper()
	machine, found := r.sup.Machine(r.token)
	if !found {
		r.t.Fatalf("%s has no architect claim", heldTaskRoot)
	}
	return machine.Claim().Pending
}

// storedDelivery is the task the claim store holds for the architect, nil for none: what a restart
// would find.
func (r *heldTaskRig) storedDelivery() *supervise.Delivery {
	r.t.Helper()
	stored, err := r.trees.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	for _, c := range stored {
		if c.Token == r.token {
			return c.Pending
		}
	}
	r.t.Fatalf("%s has no stored architect claim", heldTaskRoot)
	return nil
}

// restoredPending is the task a daemon that died and booted holds, rebuilt from the stored claims
// alone, which is the restart path this rewrite has to say the same thing after.
func (r *heldTaskRig) restoredPending() *supervise.Delivery {
	r.t.Helper()
	stored, err := r.trees.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	sup, _ := newOutboxSupervisor(r.t, "legion", r.t.TempDir())
	sup.deps = r.sup.deps
	if _, err := sup.restore(r.ctx, stored); err != nil {
		r.t.Fatalf("restore the stored claims: %v", err)
	}
	machine, found := sup.Machine(r.token)
	if !found {
		r.t.Fatalf("the restored daemon has no architect claim of %s", heldTaskRoot)
	}
	return machine.Claim().Pending
}

// named is what a delivery says it names of what its root keeps (supervise.Delivery.Kept).
func (r *heldTaskRig) named(d supervise.Delivery) record.KeptDecisions {
	r.t.Helper()
	var kept record.KeptDecisions
	if len(d.Kept) == 0 {
		return kept
	}
	if err := json.Unmarshal(d.Kept, &kept); err != nil {
		r.t.Fatalf("decode what the task names: %v", err)
	}
	return kept
}
