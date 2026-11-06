package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

// mainPathProject is the Dispatch project of the root every path here drives; its claims, its tree
// and its runtime are keyed by the normalized token "legion" (claim.ProjectToken).
const mainPathProject = "LEGION"

// mainPathRoot is the one root these paths drive, and mainPathAsk the question of it a person
// decides on where nobody in its tree hears them.
const (
	mainPathRoot = "LEGION-451"
	mainPathAsk  = "ask-1"
)

// rootHearsCall is one question the supervisor put to Deps.RootHears and the answer the production
// reading (workflowRuntime.rootHears, over workflow.ArchitectHears) gave it.
type rootHearsCall struct {
	claim    claim.Token
	delivery supervise.Delivery
	hears    bool
}

// rootHearsCalls records every such question, so a test can hold the prompt it allowed to the
// answer that allowed it.
type rootHearsCalls struct {
	mu    sync.Mutex
	calls []rootHearsCall
}

// wrap is answer with every question and answer recorded: the production answer decides, this only
// watches.
func (a *rootHearsCalls) wrap(answer func(context.Context, supervise.Claim, supervise.Delivery) (bool, error)) func(context.Context, supervise.Claim, supervise.Delivery) (bool, error) {
	return func(ctx context.Context, c supervise.Claim, d supervise.Delivery) (bool, error) {
		hears, err := answer(ctx, c, d)
		if err != nil {
			return hears, err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls = append(a.calls, rootHearsCall{claim: c.Token, delivery: d, hears: hears})
		return hears, nil
	}
}

// about is every question asked about the delivery of this id.
func (a *rootHearsCalls) about(id string) []rootHearsCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var asked []rootHearsCall
	for _, call := range a.calls {
		if call.delivery.ID == id {
			asked = append(asked, call)
		}
	}
	return asked
}

func (a *rootHearsCalls) all() []rootHearsCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]rootHearsCall(nil), a.calls...)
}

// mainPathRig is one root's life under the real parts main wires together: the record store over a
// database of its own, the workflow engine, admission at one slot, the outbox runner, and a claim
// supervisor whose Deps.RootHears is the production answer — wrapped, so every question and answer
// is recorded. wired false is main's behaviour before that answer existed: every delivery is sent.
type mainPathRig struct {
	t         *testing.T
	ctx       context.Context
	key       string
	pool      *pgxpool.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	trees     *store.Store
	sup       *supervisor
	runner    *outbox
	conn      *fake.Conn
	token     claim.Token
	asked     *rootHearsCalls
	// ran is the newest architect start row the rig has run, and seq the newest Dispatch sequence
	// it has used.
	ran  int64
	seq  int64
	tick int
}

func newMainPathRig(t *testing.T, wired bool) *mainPathRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: mainPathProject, Repositories: testRepositories}, quietLogger())
	admission := admit.New(records, engine, 1, mainPathProject, quietLogger())
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	// The hold of what a root architect's task names is the claim store's own write
	// (20261101163106_parked_decisions), so this rig runs through the daemon's own claim store: a task held
	// on the claim holds those rows, and the next start of the root names none of them again.
	trees := outboxTreeStore(t, pool, mainPathRoot)
	sup.deps.Store = keptClaimStore{Store: trees}
	asked := &rootHearsCalls{}
	if wired {
		// The daemon's own reading of whether a root architect's task may be sent
		// (workflowRuntime.rootHears), over this rig's pool and store, exactly as daemon.go wires it.
		sup.deps.RootHears = asked.wrap((&workflowRuntime{pool: pool, records: records}).rootHears)
	}
	conn := fake.NewConn()
	token := mustClaimToken(t, mainPathRoot, claim.RoleArchitect)
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	return &mainPathRig{
		t: t, ctx: ctx, key: mainPathRoot, pool: pool, records: records, engine: engine, admission: admission,
		trees: trees, sup: sup, conn: conn, token: token, asked: asked, seq: 10,
		runner: &outbox{
			log: quietLogger(), pool: pool, dispatchProject: mainPathProject, records: records, supervisor: sup,
			notices: &outboxPublisher{}, handlers: []intake.Handler{engine, admission},
			tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
			provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + mainPathRoot}, nil
			},
		},
	}
}

// waitingRoot is the root as a person's hand-over leaves it: labelled, handed over and `todo`,
// waiting for an admission slot with no architect of its tree running (record.WaitsForSlot), so a
// decision made on it now is kept.
func (r *mainPathRig) waitingRoot() record.Issue {
	return record.Issue{Key: r.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: mainPathProject, Tree: r.key, Title: "root", Phase: phase.Admitted, Generation: 1,
		Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 5}
}

func (r *mainPathRig) put(issue record.Issue) {
	r.t.Helper()
	putOutboxIssue(r.t, r.pool, r.records, issue)
}

func (r *mainPathRig) issue() record.Issue {
	r.t.Helper()
	got := readIssue(r.t, r.pool, r.key)
	if got == nil {
		r.t.Fatalf("%s is not recorded", r.key)
	}
	return *got
}

func (r *mainPathRig) nextSeq() int64 {
	r.seq++
	return r.seq
}

// apply is one fact as the daemon applies it: through the engine and admission, in one transaction
// keyed by its id.
func (r *mainPathRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	result, err := intake.ApplyFact(r.ctx, r.pool, "test", id, fact, r.engine, r.admission)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply %s = %#v, %v", id, result, err)
	}
}

// admitPass is one admission pass, the controller's tick: it reconciles the roots waiting on a
// person and fills every free slot from the waiting line.
func (r *mainPathRig) admitPass() {
	r.t.Helper()
	r.tick++
	r.apply(fmt.Sprintf("tick-%d", r.tick), intake.ControllerTick{})
}

// decide is a person answering ask of the root, as Dispatch's event reaches the engine: on a root
// nobody in whose tree hears it, the one rule for such a decision keeps it for the next architect
// start (record.RelayParkedDecision). It returns the reference the start's task must name.
func (r *mainPathRig) decide(ask string) string {
	r.t.Helper()
	ref := fmt.Sprintf("dispatch://%s/ask/%s", r.key, ask)
	r.apply("ask-answered:"+ask, intake.DispatchAskClosed{Key: r.key, Ask: ask, Kind: record.AskQuestion, State: "answered",
		Actor: alice, Mark: record.EventMark{Seq: r.nextSeq()}})
	kept := keptDecisions(r.t, r.pool, r.records, r.key)
	if len(kept) != 1 || kept[0].Ref != ref {
		r.t.Fatalf("kept after a person answered %s = %#v, want the one decision kept for %s's next start", ask, kept, r.key)
	}
	return ref
}

// park is a person setting the running root to backlog: the engine takes it out of the workflow,
// where its tree lingers and every claim of it is queued a suspend.
func (r *mainPathRig) park() {
	r.t.Helper()
	r.apply("backlog", intake.DispatchIssue{Key: r.key, ResolvedRepo: "acme/widgets", Seq: r.nextSeq(), Type: "issue.updated",
		Status: "backlog", Title: "root", Rank: "A", HandedOver: true})
	if got := r.issue(); got.Status != "backlog" || !got.Lingers() {
		r.t.Fatalf("the parked root = %+v, want it out of the workflow with its tree lingering", got)
	}
}

// handBack is a person setting the parked root to todo again: admission re-admits it at the next
// generation and this same pass admits it, which queues its architect's start.
func (r *mainPathRig) handBack() {
	r.t.Helper()
	before := r.issue().Generation
	r.apply("todo-again", intake.DispatchIssue{Key: r.key, ResolvedRepo: "acme/widgets", Seq: r.nextSeq(), Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: true})
	got := r.issue()
	if got.Generation != before+1 || got.Status != "in_progress" || got.Lingers() {
		r.t.Fatalf("the re-admitted root = %+v, want generation %d running again", got, before+1)
	}
}

// runStarts runs every architect start row of the root the outbox holds past the last one the rig
// ran, oldest first, as the supervise lane runs them: leased, executed, and finished when it acted,
// which is what releases whatever its task named.
func (r *mainPathRig) runStarts() {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, kind, issue, payload from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'architect' and id > $2 order by id`,
		r.key, r.ran)
	if err != nil {
		r.t.Fatalf("read the architect's start rows: %v", err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var out record.OutboxRow
		return out, row.Scan(&out.ID, &out.Kind, &out.Issue, &out.Payload)
	})
	if err != nil {
		r.t.Fatalf("scan the architect's start rows: %v", err)
	}
	if len(queued) == 0 {
		r.t.Fatalf("no architect start row of %s past row %d: the path queued none", r.key, r.ran)
	}
	for _, row := range queued {
		row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
		if err := r.runner.execute(r.ctx, row); err != nil {
			r.t.Fatalf("run the architect start row %d: %v", row.ID, err)
		}
		if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
			return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
		}); err != nil {
			r.t.Fatalf("finish the architect start row %d: %v", row.ID, err)
		}
		r.ran = row.ID
		r.waitMachine()
	}
}

// runSuspend runs the architect's suspend row the park queued (the engine's beginLinger), as the
// supervise lane runs it, and holds the claim to the suspension it writes.
func (r *mainPathRig) runSuspend() {
	r.t.Helper()
	var row record.OutboxRow
	if err := r.pool.QueryRow(r.ctx, `select id, kind, issue, payload from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'suspend' and payload->>'role' = 'architect' order by id desc limit 1`,
		r.key).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
		r.t.Fatalf("read the architect's suspend row: %v", err)
	}
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	if err := r.runner.execute(r.ctx, row); err != nil {
		r.t.Fatalf("run the architect suspend row %d: %v", row.ID, err)
	}
	r.waitMachine()
	if got := r.machine().Claim().State; got != supervise.StateSuspended {
		r.t.Fatalf("the architect after its tree's suspend = %s, want %s", got, supervise.StateSuspended)
	}
}

func (r *mainPathRig) machine() *supervise.Machine {
	r.t.Helper()
	machine, found := r.sup.Machine(r.token)
	if !found {
		r.t.Fatalf("%s has no architect claim", r.key)
	}
	return machine
}

// launchArchitect creates the root architect's claim and launches it, as its first start does: the
// session that goes on running across the root's waits and parks.
func (r *mainPathRig) launchArchitect() {
	r.t.Helper()
	machine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: r.token, Project: "legion", Tree: r.key, Issue: r.key,
		Role: claim.RoleArchitect, State: supervise.StateQueued}, "")
	if err != nil {
		r.t.Fatalf("create the architect claim: %v", err)
	}
	if err := machine.Handle(r.ctx, supervise.RequestSpawn{Claim: r.token}); err != nil {
		r.t.Fatalf("spawn the architect: %v", err)
	}
	r.bringUp()
}

// bringUp is the architect's agent coming up on the launch the claim stands at: the shim's hello,
// the agent's registration and its ready, each from the one session the claim records.
func (r *mainPathRig) bringUp() {
	r.t.Helper()
	machine := r.machine()
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
	r.waitMachine()
	if got := machine.Claim().State; !claimTookRole(got) {
		r.t.Fatalf("the architect after its ready = %s, want a claim that took its role", got)
	}
}

// recordClaim writes the architect's claim as the daemon persists it, which is what the workflow
// reads of it (record.Store.RoleRuns, SessionClaimsTree).
func (r *mainPathRig) recordClaim() {
	r.t.Helper()
	if err := r.trees.PutClaim(r.ctx, r.machine().Claim()); err != nil {
		r.t.Fatalf("persist the architect claim: %v", err)
	}
}

// waitMachine waits for every send the architect's machine started to have had its outcome
// handled, which is what makes a prompt part of the step that caused it.
func (r *mainPathRig) waitMachine() {
	if machine, found := r.sup.Machine(r.token); found {
		machine.Wait()
	}
}

func (r *mainPathRig) prompts() []fake.Prompt { return r.conn.Prompts() }

// mainPath is one way main's own code gives a tree-root architect a task of a run that names no
// phase, which is the only delivery Deps.RootHears is asked about.
type mainPath struct {
	name string
	// run drives the path and returns the text the one prompt it causes must carry.
	run func(r *mainPathRig) string
}

// mainPaths are those ways, each driving the real admission: its fresh start for a root that
// waited in `todo` for its slot, the start of its re-admission after a park, and the resume task an
// answered root's architect is given — from admission's answered branch and from the reconcile that
// finds a person already acted.
var mainPaths = []mainPath{
	{"a fresh admission's start reaches the architect it launches", freshAdmissionPath},
	{"a re-admission's start reaches the architect it resumes", readmissionPath},
	{"an answered root's resume reaches its live architect", answeredResumePath},
	{"a person's act already in the record resumes the architect in place", toldInPlacePath},
}

// freshAdmissionPath: a person decides on a labelled, handed-over `todo` root while it waits for
// its slot, so the decision is kept; an admission pass admits it and queues the architect start
// whose task names that decision (admission's ParkedDecisionTask); the start launches the
// architect, and its ready is what sends the task.
func freshAdmissionPath(r *mainPathRig) string {
	r.put(r.waitingRoot())
	ref := r.decide(mainPathAsk)
	r.admitPass()
	if got := r.issue(); got.Status != "in_progress" || got.Generation != 1 {
		r.t.Fatalf("the admitted root = %+v, want generation 1 running", got)
	}
	r.runStarts()
	if pending := r.machine().Claim().Pending; pending == nil || !strings.Contains(pending.Task, ref) {
		r.t.Fatalf("the architect's pending task = %#v, want the start's task naming %s", pending, ref)
	}
	r.bringUp()
	return ref
}

// readmissionPath: the root ran, a person parked it to backlog and its architect's suspend ran, a
// person then decided on the parked tree (kept), and a person handed it back to `todo`, which
// re-admits it at the next generation. That generation's start names the kept decision, resumes
// the suspended architect, and the resumed agent's ready is what sends the task.
func readmissionPath(r *mainPathRig) string {
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	r.park()
	r.runSuspend()
	ref := r.decide(mainPathAsk)
	r.handBack()
	r.runStarts()
	if pending := r.machine().Claim().Pending; pending == nil || !strings.Contains(pending.Task, ref) {
		r.t.Fatalf("the resumed architect's pending task = %#v, want the re-admission's task naming %s", pending, ref)
	}
	r.bringUp()
	return ref
}

// answeredResumePath: the root waits on a person in awaiting_person, holding no slot, its architect
// still live; a person answers the question it handed them, which answers the wait, and the
// admission pass that same fact runs takes its answered branch, which gives the architect a task of
// its own (workflow.Engine.ResumeAwaitedRoot). That task reaches the live architect at once.
func answeredResumePath(r *mainPathRig) string {
	root := r.waitingRoot()
	root.Status, root.Phase = "in_progress", phase.AwaitingPerson
	root.Awaiting = &record.Awaiting{Ask: mainPathAsk, Since: record.EventMark{Seq: 10}}
	r.put(root)
	r.launchArchitect()
	r.recordClaim()
	r.apply("ask-answered", intake.DispatchAskClosed{Key: r.key, Ask: mainPathAsk, Kind: record.AskQuestion, State: "answered",
		Actor: alice, Mark: record.EventMark{Seq: r.nextSeq()}})
	got := r.issue()
	if got.Phase != phase.Admitted || got.Status != "in_progress" || got.Awaiting != nil || got.HandoutSeq == 0 {
		r.t.Fatalf("the resumed root = %+v, want it admitted and running again on the resume's handout", got)
	}
	r.runStarts()
	return fmt.Sprintf("A person answered your ask %s on %s", mainPathAsk, r.key)
}

// toldInPlacePath: the root runs, holding its slot, with a child of its tree in a phase, which is
// what keeps every pass meanwhile from deciding it waits on a person. Its architect hands two
// questions to the person and a person replies in one of them, leaving its turn with the agent.
// Once the child leaves the workflow, the next pass — the one that same fact runs — finds the root
// would begin a wait and that a person already acted after its hand-back, so its architect is told
// where it stands and the slot is kept (the reconcile's actTellInPlace, through the same
// ResumeAwaitedRoot).
func toldInPlacePath(r *mainPathRig) string {
	const child = "LEGION-452"
	r.put(r.waitingRoot())
	parent := r.key
	r.put(record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: mainPathProject, Tree: r.key, Parent: &parent, Title: "child", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 5})
	r.admitPass()
	r.runStarts()
	r.bringUp()
	r.recordClaim()
	for _, ask := range []string{"ask-a", "ask-b"} {
		r.apply("opened:"+ask, intake.DispatchAskOpened{Key: r.key, Ask: ask, Kind: record.AskQuestion,
			AuthorSession: "ses-architect", Mark: record.EventMark{Seq: r.nextSeq()}})
	}
	r.apply("replied:ask-a", intake.DispatchAskReply{Key: r.key, Ask: "ask-a", WaitingOn: "agent", Actor: alice,
		Mark: record.EventMark{Seq: r.nextSeq()}})
	if got := r.issue(); got.DeclaredSeq == 0 || got.AwaitingPerson() {
		r.t.Fatalf("the root while a child of its tree is in a phase = %+v, want it admitted with its hand-back declared", got)
	}
	r.apply("child-backlog", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 6, Type: "issue.updated",
		Status: "backlog", Title: "child", Parent: r.key, Rank: "B"})
	if got := r.issue(); got.Phase != phase.Admitted || got.AwaitingPerson() {
		r.t.Fatalf("the root told in place = %+v, want it admitted, holding its slot", got)
	}
	r.runStarts()
	return fmt.Sprintf("A person answered or replied in the thread of ask %s on %s", "ask-a", r.key)
}

// A task main's own code gives a tree-root architect — admission's fresh start for a root that
// waited for its slot, the start of its re-admission, and the resume of a root a person answered —
// names no phase and belongs to a run, so it is the one delivery supervise puts to Deps.RootHears
// (the daemon's rootHears, over workflow.ArchitectHears). Every one of those tasks is given to a
// root that has an architect to hear it, so wiring that answer must change none of them: each
// reaches its architect exactly once, the answer was asked for it and said yes, and the prompt
// count is the same as main's own, which sends everything (Deps.RootHears nil).
//
// The negative control is the same root parked again while its start task waits on a ready claim:
// there the answer says no, the task is held rather than prompted to an architect no run of the
// root owns, and the start of the root's next run is what releases it.
func TestARootThatMainsCodeResumesGetsItsTaskWithRootHearsWired(t *testing.T) {
	for _, path := range mainPaths {
		t.Run(path.name, func(t *testing.T) {
			sent := map[bool]int{}
			for _, wired := range []bool{true, false} {
				rig := newMainPathRig(t, wired)
				want := path.run(rig)
				prompts := rig.prompts()
				if len(prompts) != 1 || !strings.Contains(prompts[0].Message, want) {
					t.Fatalf("RootHears wired=%t: the architect was prompted %d times with %+v, want exactly one prompt carrying %q",
						wired, len(prompts), prompts, want)
				}
				sent[wired] = len(prompts)
				if !wired {
					if asked := rig.asked.all(); len(asked) != 0 {
						t.Fatalf("main's own behaviour asked RootHears %+v, want nothing asked with no answer wired", asked)
					}
					continue
				}
				asked := rig.asked.about(prompts[0].DeliveryID)
				if len(asked) == 0 {
					t.Fatalf("the task prompted as %s was never put to RootHears; every root architect's task of a run is",
						prompts[0].DeliveryID)
				}
				for _, call := range asked {
					if !call.hears || call.claim != rig.token || call.delivery.Phase != "" || call.delivery.Generation == 0 {
						t.Fatalf("RootHears answered %#v, want yes for the root architect's phase-less task of a run", call)
					}
				}
			}
			if sent[true] != sent[false] {
				t.Fatalf("the architect was prompted %d times with RootHears wired and %d times with main's own behaviour; "+
					"a task of a root that has an architect to hear it is sent either way", sent[true], sent[false])
			}
		})
	}

	// The same root as the fresh admission, parked again before the architect it launched comes up:
	// the start's task waits on a ready claim while the root has nobody to hear it. The answer holds
	// it there — unsent, still pending — and the start of the root's next run is what sends it.
	t.Run("a task of a parked root is held until the root runs again", func(t *testing.T) {
		rig := newMainPathRig(t, true)
		rig.put(rig.waitingRoot())
		ref := rig.decide(mainPathAsk)
		rig.admitPass()
		rig.runStarts()
		rig.park()
		rig.bringUp()
		pending := rig.machine().Claim().Pending
		if pending == nil || !strings.Contains(pending.Task, ref) || !pending.DeliveredAt.IsZero() {
			t.Fatalf("the parked root's task = %#v, want it pending and unsent on the ready claim", pending)
		}
		if got := rig.prompts(); len(got) != 0 {
			t.Fatalf("the architect of a parked root was prompted %+v, want nothing: no run of the root owns it", got)
		}
		refused := rig.asked.about(pending.ID)
		if len(refused) == 0 {
			t.Fatalf("the held task was never put to RootHears; asked %+v", rig.asked.all())
		}
		for _, call := range refused {
			if call.hears {
				t.Fatalf("RootHears answered yes for %#v while %s is parked and lingering, want no", call, rig.key)
			}
		}

		// A person hands the root back: its re-admission's start carries no task of its own (the
		// held one named what the root kept), and is what releases the held task.
		rig.handBack()
		rig.runStarts()
		prompts := rig.prompts()
		if len(prompts) != 1 || prompts[0].DeliveryID != pending.ID || !strings.Contains(prompts[0].Message, ref) {
			t.Fatalf("after the re-admission the architect was prompted %+v, want the held task %s naming %s once",
				prompts, pending.ID, ref)
		}
		for _, call := range rig.asked.about(pending.ID)[len(refused):] {
			if !call.hears {
				t.Fatalf("RootHears answered no for %#v after the root was admitted again, want yes", call)
			}
		}
	})
}
