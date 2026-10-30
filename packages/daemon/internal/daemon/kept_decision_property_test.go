package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The property test's bounds. A run is one root's life under one seed; a step is one operation the
// seed's generator chose. The defaults are 30 runs of 100 steps, 3000 steps in all, which the
// isolated database of each run (isolatedOutboxPool) dominates.
const (
	keptPropertyRuns  = 30
	keptPropertySteps = 100
	// keptQuiesceBound caps the quiescence loop: a root whose rows and deliveries do not drain
	// with every failure switched off is itself a failure of the invariant, reported with its
	// trace rather than left to the package deadline.
	keptQuiesceBound = 400
	// keptRoot is the one root every run drives.
	keptRoot = "LEGION-1"
)

// keptTelling is one time an architect of the root was told a kept decision: the generation the
// telling names (a catch-up's; a start task names none), whether the root had an architect to hear
// it when the architect received it, the generation the root stood at then, and what carried it.
type keptTelling struct {
	ref string
	// prompt is whether a start task's prompt carried it. A start task names no generation
	// (workflow.ParkedDecisionTask writes none) and the claim it reaches is the root's one
	// architect session, which holds its task across the tree's re-admissions, so what it is held
	// to is that the root has an architect to hear it (hears) rather than the generation named.
	prompt bool
	hears  bool
	named  uint64
	root   uint64
	how    string
	step   int
}

// keptPublishMode is what the listener does with a role publish this run.
type keptPublishMode int

const (
	// keptDelivers: every publish reaches its topic's holder.
	keptDelivers keptPublishMode = iota
	// keptNoHolder: no live session holds the role, so the row waits (notify.ErrNoHolder).
	keptNoHolder
	// keptRefuses: the holder does not advertise the delivery mode, so the row finishes
	// undelivered (notify.ErrRefusedByHolder) and releases what it claimed.
	keptRefuses
	// keptReceiptTimeout: the holder did not confirm in time, so the daemon re-holds the notice
	// (notify.ErrNotDelivered) and the copy takes the original's claims over (MoveKeptClaims).
	// Only a notice nobody has re-held yet is answered this way, so the re-hold budget
	// (noticeReholdDelays) is never spent and the copy is never dropped for exhausting it.
	keptReceiptTimeout
)

func (m keptPublishMode) String() string {
	switch m {
	case keptNoHolder:
		return "no-holder"
	case keptRefuses:
		return "refused"
	case keptReceiptTimeout:
		return "receipt-timeout"
	default:
		return "delivers"
	}
}

// keptPublisher is the listener as a role publish meets it, under one mode the run switches.
type keptPublisher struct {
	mu        sync.Mutex
	mode      keptPublishMode
	delivered []outboxPublish
}

func (p *keptPublisher) Publish(_ context.Context, topic, message string, payload any, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.mode {
	case keptNoHolder:
		return fmt.Errorf("publish notice to %s: listener returned 404 Not Found: %w", topic, notify.ErrNoHolder)
	case keptRefuses:
		return fmt.Errorf("publish notice to %s: listener returned 403 Forbidden: %w", topic,
			notify.ErrRefusedByHolder{Holder: "ses-other"})
	case keptReceiptTimeout:
		if notice, ok := payload.(record.Notice); ok && notice.Resends == 0 {
			return fmt.Errorf("publish notice to %s: listener returned 504 Gateway Timeout: %w", topic,
				notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses-other"})
		}
	}
	p.delivered = append(p.delivered, outboxPublish{topic: topic, message: message, key: key, payload: payload})
	return nil
}

func (p *keptPublisher) setMode(mode keptPublishMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
}

func (p *keptPublisher) published() []outboxPublish {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]outboxPublish(nil), p.delivered...)
}

// keptForgetStore is the record store with ForgetKeptDecisions failing while the run's toggle is
// on, as a database error — or the daemon dying — between a start's delivery, or a catch-up's
// publish, and the forget's commit leaves it.
type keptForgetStore struct {
	record.Store
	fails bool
}

func (s *keptForgetStore) ForgetKeptDecisions(ctx context.Context, tx pgx.Tx, root string, kept record.KeptDecisions, row int64) error {
	if s.fails {
		return errors.New("tester: the forget's transaction did not commit")
	}
	return s.Store.ForgetKeptDecisions(ctx, tx, root, kept, row)
}

// keptRun is one seeded life of one root: the real record store, workflow engine, outbox runner
// and claim supervisor over a database of its own, the model the generator drives them from, and
// what the architect was told.
type keptRun struct {
	t     *testing.T
	ctx   context.Context
	seed  int64
	rng   *rand.Rand
	steps int
	step  int
	trace []string

	pool      *pgxpool.Pool
	base      record.Store
	records   *keptForgetStore
	engine    *workflow.Engine
	trees     *store.Store
	claims    *outboxClaimStore
	rt        *fake.Runtime
	conn      *fake.Conn
	publisher *keptPublisher
	sup       *supervisor
	runner    *outbox
	token     claim.Token
	topic     string
	stateDir  string

	root    record.Issue
	decided int
	at      time.Time
	tracked []string
	told    map[string][]keptTelling
	rowGen  map[int64]uint64
	// sent is every prompt delivery id the architect has been sent, and keys every dedupe key a
	// notice was delivered under: a repeat of either is the same telling, not a second one.
	sent     map[string]bool
	keys     map[string]bool
	prompts  int
	publishd int
}

// A person's decision on a root nobody in its tree can hear reaches an architect of the root's
// current generation exactly once, whatever order the machinery runs in. The test drives random
// interleavings of the real parts — record.RelayParkedDecision for the keeping,
// workflow.Engine.ParkedDecisionTask and EnqueueNaming for the naming, the outbox runner for the
// delivery and the forget, the supervisor's machines for the architect, and intake.ApplyFact for
// the ready — over a real PostgreSQL, and holds each run to the invariant after every step:
//
//   - no decision is told twice (a start task prompt or a published catch-up naming it again);
//   - every catch-up telling names the generation the root stands at when the architect receives
//     it, and every start-task telling reaches a root that has an architect to hear it: a start
//     task names no generation, and it is held on its claim, unsent, while its root is parked,
//     lingering or waiting for its slot (supervise's Deps.RootHears);
//   - once the run quiesces — every injected failure off, every row drained, the architect ready
//     and its turns ended — every decision kept has been told once and nothing is left kept.
//
// Each run is reproducible from its seed alone: the generator is the only source of choices, the
// machines' clock never moves (stillClock), and every wait is for a goroutine the step started.
// LEGION_KEPT_PROPERTY_SEED runs one seed, LEGION_KEPT_PROPERTY_RUNS how many seeds and
// LEGION_KEPT_PROPERTY_STEPS how many operations each drives.
//
// The operations a step chooses among are a person's decision, the root's park (to backlog, or
// lingering after its close) with the architect's suspend, its hand back to todo with no slot, its
// re-admission with the start admission writes, one due supervise or notice row of the root run in
// no particular order (finished, or its finish lost to a crash), the architect's launch coming
// ready, its turn on the task it holds, the forget's failure switched on and off, the listener
// switched between delivering, holding no session, refusing the delivery mode and not confirming
// in time (the re-hold), and the daemon's restart.
//
// Three the machinery has are left out. The architect's process dying takes its task back and the
// relaunch sends the same delivery to a fresh agent, which no observation here can tell from a
// second telling, and whether that is one is a question for the specification rather than for this
// test. A newer decision on a reference already kept replaces its row (Store.AddParkedDecision),
// so that reference is told twice by design and "told once per reference" stops meaning anything;
// every decision here is made on a reference of its own. The outbox's own parkDecision, which
// keeps the decision a spec-decision notice could not deliver, reaches the same
// record.RelayParkedDecision this test calls directly, and adds only the notice routing its own
// tests cover.
func TestKeptDecisionsReachTheCurrentArchitectOnce(t *testing.T) {
	steps := keptPropertyEnv(t, "LEGION_KEPT_PROPERTY_STEPS", keptPropertySteps)
	for _, seed := range keptPropertySeeds(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			run := newKeptRun(t, seed, steps)
			run.drive()
			run.quiesce()
			run.held()
		})
	}
}

// keptPropertySeeds is the seeds this invocation runs: the one LEGION_KEPT_PROPERTY_SEED names, or
// 1..runs, which are the same seeds at every invocation so a failure found by the default run is
// replayed by naming its seed.
func keptPropertySeeds(t *testing.T) []int64 {
	t.Helper()
	if named := os.Getenv("LEGION_KEPT_PROPERTY_SEED"); named != "" {
		seed, err := strconv.ParseInt(named, 10, 64)
		if err != nil {
			t.Fatalf("LEGION_KEPT_PROPERTY_SEED=%q is no seed: %v", named, err)
		}
		return []int64{seed}
	}
	runs := keptPropertyEnv(t, "LEGION_KEPT_PROPERTY_RUNS", keptPropertyRuns)
	seeds := make([]int64, 0, runs)
	for seed := 1; seed <= runs; seed++ {
		seeds = append(seeds, int64(seed))
	}
	return seeds
}

func keptPropertyEnv(t *testing.T, name string, fallback int) int {
	t.Helper()
	set := os.Getenv(name)
	if set == "" {
		return fallback
	}
	value, err := strconv.Atoi(set)
	if err != nil || value <= 0 {
		t.Fatalf("%s=%q is no count: %v", name, set, err)
	}
	return value
}

func newKeptRun(t *testing.T, seed int64, steps int) *keptRun {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	base := record.NewStore()
	records := &keptForgetStore{Store: base}
	run := &keptRun{
		t: t, ctx: ctx, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed))), steps: steps,
		pool: pool, base: base, records: records,
		engine:    workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories}, quietLogger()),
		trees:     outboxTreeStore(t, pool, keptRoot),
		claims:    &outboxClaimStore{},
		rt:        fake.NewRuntime(),
		conn:      fake.NewConn(),
		publisher: &keptPublisher{},
		token:     mustClaimToken(t, keptRoot, claim.RoleArchitect),
		topic:     architectTopic(t, keptRoot),
		stateDir:  t.TempDir(),
		at:        time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC),
		told:      map[string][]keptTelling{},
		rowGen:    map[int64]uint64{},
		sent:      map[string]bool{},
		keys:      map[string]bool{},
	}
	run.root = record.Issue{Key: keptRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: keptRoot, Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress"}
	run.putRoot()
	run.restart()
	// The run opens as a fresh admission does: the architect's start, naming what the root keeps,
	// which is nothing yet.
	run.enqueueStart()
	return run
}

// restart builds the supervisor and the outbox runner again over the claim store and the fake
// runtime the run keeps: a daemon that died and booted, its memory gone and Postgres and the
// stored claims as they were. The first call is the run's first boot, which restores nothing.
func (r *keptRun) restart() {
	r.t.Helper()
	first := r.sup == nil
	if !first {
		r.sup.stop()
	}
	sup := newSupervisor(r.ctx, nil, "legion", r.stateDir, quietLogger())
	sup.deps = supervise.Deps{
		Runtime: r.rt, Conns: fake.NewConns(), Store: r.claims, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		// The daemon's own reading of whether a root architect's task may be sent
		// (workflowRuntime.rootHears), over this run's pool and store.
		RootHears:      (&workflowRuntime{pool: r.pool, records: r.records}).rootHears,
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
	}
	r.t.Cleanup(sup.stop)
	if !first {
		if _, err := sup.restore(r.ctx, r.storedClaims()); err != nil {
			r.fatalf("restore the stored claims: %v", err)
		}
	}
	r.sup = sup
	r.runner = &outbox{
		log: quietLogger(), pool: r.pool, dispatchProject: "LEGION", records: r.records, notices: r.publisher,
		supervisor: sup, trees: r.trees, tokens: outboxTokens{}, handlers: []intake.Handler{r.engine},
		project: "legion", stateDir: r.stateDir, now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: r.t.TempDir(), Bookmark: "legion/" + keptRoot}, nil
		},
	}
}

// storedClaims is every claim the store holds, each with the delivery the store holds for it, as
// the boot reads them (store.Store.Claims): a restart rebuilds the machines from these alone.
func (r *keptRun) storedClaims() []supervise.Claim {
	r.claims.mu.Lock()
	defer r.claims.mu.Unlock()
	stored := make([]supervise.Claim, 0, len(r.claims.claims))
	for token, held := range r.claims.claims {
		if delivery, ok := r.claims.deliveries[token]; ok {
			pending := delivery
			held.Pending = &pending
		}
		stored = append(stored, held)
	}
	return stored
}

// drive runs the seed's operations, each chosen among those the model state enables.
func (r *keptRun) drive() {
	for r.step = 1; r.step <= r.steps; r.step++ {
		r.snapshotRows()
		enabled := r.enabled()
		enabled[r.rng.IntN(len(enabled))]()
		r.observe()
		r.deliverControllerNotices()
	}
}

// enabled is every operation the model state allows now, in a fixed order so one seed always picks
// the same one.
func (r *keptRun) enabled() []func() {
	ops := []func(){r.toggleForget, r.togglePublisher, r.restartDaemon}
	if !r.architectHears() {
		ops = append(ops, r.decide)
	}
	if r.root.Status == "in_progress" && !r.root.Lingers() {
		ops = append(ops, r.park)
	}
	if record.OutOfWorkflow(r.root.Status) && !r.root.Lingers() {
		ops = append(ops, r.todoWait)
	}
	if r.parkedOrWaiting() {
		ops = append(ops, r.readmit)
	}
	if len(r.rows(true)) > 0 {
		ops = append(ops, r.runRow)
	}
	if r.readyEnabled() {
		ops = append(ops, r.ready)
	}
	if r.turnEnabled() {
		ops = append(ops, r.turn)
	}
	return ops
}

// architectHears is whether an agent holds the root's architect role now, so a decision made on
// the root reaches the tree itself and nothing is kept for it.
func (r *keptRun) architectHears() bool {
	machine, found := r.sup.Machine(r.token)
	return found && claimTookRole(machine.Claim().State)
}

// parkedOrWaiting is whether the root has no run of its own: parked out of the workflow, lingering
// after its close, or waiting in todo for an admission slot.
func (r *keptRun) parkedOrWaiting() bool {
	return r.root.Status != "in_progress" || r.root.Lingers()
}

// decide is a person deciding on an ask of the root while nobody in its tree hears it, kept by the
// one rule both paths that find such a decision follow (record.RelayParkedDecision).
func (r *keptRun) decide() {
	r.decided++
	ref := fmt.Sprintf("dispatch://%s/ask/ask-%04d", keptRoot, r.decided)
	actor := []string{"user:alice", "user:bob"}[r.rng.IntN(2)]
	kind := []record.DecisionKind{record.DecisionAskAnswered, record.DecisionAskResolved}[r.rng.IntN(2)]
	r.at = r.at.Add(time.Minute)
	outcome := record.RelayNone
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		stored, err := r.records.Issue(r.ctx, tx, keptRoot)
		if err != nil || stored == nil {
			return err
		}
		outcome, err = record.RelayParkedDecision(r.ctx, tx, r.records, keptRoot, *stored, true,
			record.ParkedDecision{Ref: ref, Decision: kind, Actor: actor, At: r.at}, 0, "")
		return err
	}); err != nil {
		r.fatalf("keep the decision %s: %v", ref, err)
	}
	if outcome == record.RelayKept || outcome == record.RelaySent {
		r.tracked = append(r.tracked, ref)
	}
	r.tracef("decide ref=%s by=%s kind=%s outcome=%s kept=%t", ref, actor, kind, keptOutcome(outcome),
		outcome == record.RelayKept || outcome == record.RelaySent)
}

func keptOutcome(outcome record.RelayOutcome) string {
	switch outcome {
	case record.RelayKept:
		return "kept"
	case record.RelaySent:
		return "sent"
	case record.RelayNoPerson:
		return "no-person"
	case record.RelayNoPersonWaiting:
		return "no-person-waiting"
	case record.RelayNotParked:
		return "not-parked"
	default:
		return "none"
	}
}

// park takes the root out of the workflow — set back to backlog, or left lingering after its
// close — and writes the architect's suspend as the workflow writes one (workflow's suspend).
func (r *keptRun) park() {
	lingers := r.rng.IntN(2) == 0
	if lingers {
		until := time.Now().Add(time.Hour)
		r.root.Phase, r.root.Status, r.root.LingerUntil = phase.Done, "done", &until
	} else {
		r.root.Phase, r.root.Status, r.root.LingerUntil = phase.Done, "backlog", nil
	}
	r.putRoot()
	r.enqueue(record.SuperviseRequest{Op: "suspend", Tree: r.root.Tree, Role: claim.RoleArchitect,
		Generation: r.root.Generation, Reason: "the tree left the workflow"})
	r.tracef("park status=%s lingers=%t generation=%d suspend-row=%d", r.root.Status, lingers, r.root.Generation, r.lastRow())
}

// todoWait hands the parked root back to Legion in todo with no slot, where record.WaitsForSlot
// finds it waiting: a person's decision on it is kept for the start its admission will queue, and
// the controller is told nothing.
func (r *keptRun) todoWait() {
	r.root.Status, r.root.HandedOver, r.root.LingerUntil = "todo", true, nil
	r.putRoot()
	r.tracef("todo-wait generation=%d waits=%t", r.root.Generation, r.waitsForSlot())
}

// readmit is the root's next generation: admission's record, and the architect start it enqueues,
// naming what the root keeps and claiming those rows for that start in one transaction, exactly as
// admit.Admission writes them.
func (r *keptRun) readmit() {
	r.root.Generation++
	r.root.Status, r.root.Phase, r.root.LingerUntil = "in_progress", phase.Admitted, nil
	r.putRoot()
	r.enqueueStart()
}

func (r *keptRun) enqueueStart() {
	r.t.Helper()
	var task string
	var kept *record.KeptDecisions
	now := time.Now()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		if task, kept, err = r.engine.ParkedDecisionTask(r.ctx, tx, r.root); err != nil {
			return err
		}
		return r.engine.EnqueueNaming(r.ctx, tx, r.root.Key, r.root.Key, record.SuperviseRequest{Op: "start",
			Tree: r.root.Tree, Role: claim.RoleArchitect, Generation: r.root.Generation, Task: task, Kept: kept}, kept, now)
	}); err != nil {
		r.fatalf("enqueue the architect start of generation %d: %v", r.root.Generation, err)
	}
	r.tracef("re-admit generation=%d start-row=%d names=%d", r.root.Generation, r.lastRow(), keptCount(kept))
}

func keptCount(kept *record.KeptDecisions) int {
	if kept == nil {
		return 0
	}
	return len(kept.Decisions)
}

// runRow runs one due supervise or notice row of the root, chosen at random rather than in row
// order: the supervise rows of one issue do not run in row order (record's outboxInOrder). A row
// whose run returns nothing is finished as its lane finishes it, unless the generator chose the
// crash: the finish never commits, so the row runs again later. A row whose run failed is left for
// its next run, as a lane leaves it.
func (r *keptRun) runRow() {
	due := r.rows(true)
	row := due[r.rng.IntN(len(due))]
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.fatalf("decode outbox row %d: %v", row.ID, err)
	}
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	runErr := r.runner.execute(r.ctx, row)
	r.waitMachine()
	crashed := runErr == nil && r.rng.IntN(6) == 0
	if runErr == nil && !crashed {
		if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
			return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
		}); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	r.tracef("run-row id=%d kind=%s what=%s outcome=%s", row.ID, row.Kind, keptRowWhat(payload), keptRunOutcome(runErr, crashed))
}

func keptRunOutcome(err error, crashed bool) string {
	switch {
	case err != nil:
		return "error: " + err.Error()
	case crashed:
		return "ran, finish lost to a crash"
	default:
		return "ran and finished"
	}
}

// keptRowWhat names an outbox row's payload for the trace.
func keptRowWhat(payload record.OutboxPayload) string {
	switch value := payload.(type) {
	case record.SuperviseRequest:
		return fmt.Sprintf("%s %s generation=%d names=%d", value.Op, value.Role, value.Generation, keptCount(value.Kept))
	case record.Notice:
		if value.CatchUp != nil {
			return fmt.Sprintf("catch-up generation=%d launch=%d names=%d resends=%d",
				value.CatchUp.Generation, value.CatchUp.Launch, keptCount(value.CatchUp.Kept), value.Resends)
		}
		return string(value.Kind)
	default:
		return fmt.Sprintf("%T", payload)
	}
}

// readyEnabled is whether the architect's claim has a launch to make ready: one coming up, or one
// live whose connection a restart took with it.
func (r *keptRun) readyEnabled() bool {
	machine, found := r.sup.Machine(r.token)
	if !found {
		return false
	}
	switch state := machine.Claim().State; state {
	case supervise.StateLaunching, supervise.StateShimConnected, supervise.StateRegistered:
		return true
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		_, connected := r.sup.deps.Conns.Conn(r.token)
		return !connected
	default:
		return false
	}
}

// ready is the architect's agent coming up on its launch: its connection, the shim's hello, the
// registration and the ready, and then the ready fact the daemon applies for that launch
// (workflowRuntime.applyTerminal), which is what writes the launch its catch-up.
func (r *keptRun) ready() {
	machine, found := r.sup.Machine(r.token)
	if !found {
		r.fatalf("the architect claim is gone")
	}
	r.sup.deps.Conns.(*fake.Conns).Register(r.token, r.conn)
	launch := machine.Claim().Generation
	r.handle(machine, supervise.StreamHello{Claim: r.token, Generation: launch})
	if state := machine.Claim().State; state == supervise.StateShimConnected || state == supervise.StateRegistered || claimTookRole(state) {
		r.handle(machine, supervise.RequestRegister{Claim: r.token, Generation: launch, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"})
	}
	if state := machine.Claim().State; state == supervise.StateRegistered || claimTookRole(state) {
		r.handle(machine, supervise.RequestReady{Claim: r.token, Generation: launch, Session: "ses-architect"})
	}
	r.waitMachine()
	key := terminalEventID(supervise.Claim{Token: r.token, Generation: launch}, supervise.StateReady)
	if _, err := intake.ApplyFact(r.ctx, r.pool, terminalSource, key,
		intake.ClaimReady{Issue: keptRoot, Role: claim.RoleArchitect, Launch: launch}, r.engine); err != nil {
		r.fatalf("apply the architect's ready for launch %d: %v", launch, err)
	}
	r.waitMachine()
	r.tracef("ready launch=%d state=%s catch-up-row=%d", launch, machine.Claim().State, r.lastRow())
}

// turnEnabled is whether the architect holds a task it acknowledged and has not started a turn on.
func (r *keptRun) turnEnabled() bool {
	machine, found := r.sup.Machine(r.token)
	if !found {
		return false
	}
	pending := machine.Claim().Pending
	return pending != nil && !pending.DeliveredAt.IsZero() && pending.ConfirmedAt.IsZero()
}

// turn is the architect's turn on the task it holds, from the stream's agent_start to its
// agent_end, which retires the delivery.
func (r *keptRun) turn() {
	machine, found := r.sup.Machine(r.token)
	if !found {
		r.fatalf("the architect claim is gone")
	}
	r.handle(machine, supervise.StreamTurnStart{Claim: r.token})
	r.waitMachine()
	r.handle(machine, supervise.StreamTurnEnd{Claim: r.token})
	r.waitMachine()
	r.tracef("turn state=%s pending=%t", machine.Claim().State, machine.Claim().Pending != nil)
}

func (r *keptRun) toggleForget() {
	r.records.fails = !r.records.fails
	r.tracef("forget-failure=%t", r.records.fails)
}

func (r *keptRun) togglePublisher() {
	mode := keptPublishMode(r.rng.IntN(4))
	r.publisher.setMode(mode)
	r.tracef("publisher=%s", mode)
}

func (r *keptRun) restartDaemon() {
	r.restart()
	machine, found := r.sup.Machine(r.token)
	state := supervise.ClaimState("none")
	if found {
		state = machine.Claim().State
	}
	r.tracef("restart claims=%d architect=%s", r.sup.count(), state)
}

// quiesce ends the run: every injected failure off, the root re-admitted when it is parked or
// waiting — never when it runs, which would hand a lost decision a fresh namer and hide it — and
// then its rows, its architect's launches and its turns driven, in random order, until no row of
// the root is left and no delivery is pending.
func (r *keptRun) quiesce() {
	r.records.fails = false
	r.publisher.setMode(keptDelivers)
	r.tracef("quiesce: every failure off, publisher delivers")
	if r.parkedOrWaiting() {
		r.readmit()
	}
	for range keptQuiesceBound {
		r.step++
		r.snapshotRows()
		actions := []func(){}
		if len(r.rows(false)) > 0 {
			actions = append(actions, r.runLeftoverRow)
		}
		if r.readyEnabled() {
			actions = append(actions, r.ready)
		}
		if r.turnEnabled() {
			actions = append(actions, r.turn)
		}
		if len(actions) == 0 {
			r.tracef("quiesced")
			return
		}
		actions[r.rng.IntN(len(actions))]()
		r.observe()
		r.deliverControllerNotices()
	}
	r.fatalf("the root's rows and deliveries did not drain in %d quiescence steps", keptQuiesceBound)
}

// runLeftoverRow runs one of the root's remaining rows whatever its backoff says: a quiesced
// daemon has run every row it holds, including a re-held copy waiting out its delay.
func (r *keptRun) runLeftoverRow() {
	left := r.rows(false)
	row := left[r.rng.IntN(len(left))]
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.fatalf("decode outbox row %d: %v", row.ID, err)
	}
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	runErr := r.runner.execute(r.ctx, row)
	r.waitMachine()
	if runErr == nil {
		if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
			return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
		}); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	r.tracef("run-row id=%d kind=%s what=%s outcome=%s", row.ID, row.Kind, keptRowWhat(payload), keptRunOutcome(runErr, false))
}

// held is the run's verdict once it has quiesced: every decision kept was told once, and the root
// keeps nothing.
func (r *keptRun) held() {
	for _, ref := range r.tracked {
		if told := r.told[ref]; len(told) != 1 {
			r.fatalf("%s was told %d times, want once: a person's decision kept while nobody in %s's tree could hear it must reach an architect of its current generation exactly once",
				ref, len(told), keptRoot)
		}
	}
	if left := keptDecisions(r.t, r.pool, r.base, keptRoot); len(left) != 0 {
		r.fatalf("the root still keeps %d decisions after the run quiesced (%s), want none: each was named by a start or catch-up that never reached an architect, and nothing names them again",
			len(left), keptRefs(left))
	}
}

func keptRefs(decisions []record.ParkedDecision) string {
	refs := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		refs = append(refs, fmt.Sprintf("%s claimed-by=%d", decision.Ref, decision.ClaimedBy))
	}
	return strings.Join(refs, ", ")
}

// observe reads what the architect was told since the last step — the prompts its connection
// received and the catch-ups the publisher delivered to its role topic — and holds each telling to
// the invariant: no reference twice, a catch-up naming the generation the root stands at now, and
// a start task reaching a root that has an architect to hear it.
//
// One telling is one delivery, not one frame: a prompt frame carries the daemon's delivery id, and
// a shim that sees the same id twice answers it without a second turn (supervise.Delivery), so the
// re-send a reconnect or a retried row makes is the same telling; a published notice carries the
// outbox key of the row it is a copy of (record.Notice.Published), which the listener's role lane
// and the hosts dedupe on, so a copy published under a key already delivered is too.
func (r *keptRun) observe() {
	generation := r.storedGeneration()
	prompts := r.conn.Prompts()
	for ; r.prompts < len(prompts); r.prompts++ {
		prompt := prompts[r.prompts]
		if r.sent[prompt.DeliveryID] {
			continue
		}
		r.sent[prompt.DeliveryID] = true
		row := keptRowOfDelivery(prompt.DeliveryID)
		named, known := r.rowGen[row]
		if !known {
			r.fatalf("the architect was prompted by delivery %q, whose outbox row %d the run never saw", prompt.DeliveryID, row)
		}
		for _, ref := range r.refsIn(prompt.Message) {
			r.tell(keptTelling{ref: ref, prompt: true, hears: r.architectToHear(), named: named, root: generation, step: r.step,
				how: fmt.Sprintf("the start task of row %d, prompt %s", row, prompt.DeliveryID)})
		}
	}
	published := r.publisher.published()
	for ; r.publishd < len(published); r.publishd++ {
		delivered := published[r.publishd]
		if delivered.topic != r.topic || r.keys[delivered.key] {
			continue
		}
		notice, ok := delivered.payload.(record.Notice)
		if !ok || notice.CatchUp == nil {
			continue
		}
		r.keys[delivered.key] = true
		for _, ref := range r.refsIn(notice.CatchUp.Decided) {
			r.tell(keptTelling{ref: ref, named: notice.CatchUp.Generation, root: generation, step: r.step,
				how: fmt.Sprintf("the catch-up published under %s", delivered.key)})
		}
	}
}

// architectToHear is whether the root, as the record holds it now, has an architect to hear a
// decision: its tree's root, in the workflow, not lingering, and not waiting in `todo` for its
// slot (workflow.ArchitectHears, the reading the daemon holds a start task to). A start task's
// prompt is held to this rather than to the generation it names, since it names none and reaches
// the root's one architect session whenever that session is next ready.
func (r *keptRun) architectToHear() bool {
	var hears bool
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		root, err := r.records.Issue(r.ctx, tx, keptRoot)
		if err != nil || root == nil {
			return err
		}
		hears, err = workflow.ArchitectHears(r.ctx, tx, r.records, *root)
		return err
	}); err != nil {
		r.fatalf("read whether the root has an architect to hear a decision: %v", err)
	}
	return hears
}

// tell records one telling and holds it to the two invariants a telling can break on its own.
func (r *keptRun) tell(telling keptTelling) {
	if before := r.told[telling.ref]; len(before) > 0 {
		r.fatalf("%s was told twice: first at step %d by %s, now by %s; a person's decision reaches its architect once",
			telling.ref, before[0].step, before[0].how, telling.how)
	}
	switch {
	case telling.prompt && !telling.hears:
		r.fatalf("%s was told by %s while %s has no architect to hear it (parked, lingering, or waiting for its slot): a start task reaches an architect of the root's current run",
			telling.ref, telling.how, keptRoot)
	case !telling.prompt && telling.named != telling.root:
		r.fatalf("%s was told by %s, which names generation %d, while %s stands at generation %d: every catch-up names the generation the root is at when its architect receives it",
			telling.ref, telling.how, telling.named, keptRoot, telling.root)
	}
	r.told[telling.ref] = append(r.told[telling.ref], telling)
	r.tracef("told ref=%s generation=%d by=%s", telling.ref, telling.named, telling.how)
}

// refsIn is every tracked reference the text names. The references are fixed width, so no one is a
// substring of another.
func (r *keptRun) refsIn(text string) []string {
	var named []string
	for _, ref := range r.tracked {
		if strings.Contains(text, ref) {
			named = append(named, ref)
		}
	}
	return named
}

// keptRowOfDelivery is the outbox row a delivery id was minted from (outboxDeliveryPrefix), and 0
// for an id no row minted.
func keptRowOfDelivery(id string) int64 {
	rest, found := strings.CutPrefix(id, outboxDeliveryPrefix)
	if !found {
		return 0
	}
	row, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return row
}

// rows is the root's supervise and notice rows, those due now or every one left, in id order; the
// caller picks among them at random.
func (r *keptRun) rows(dueOnly bool) []record.OutboxRow {
	r.t.Helper()
	query := `select id, kind, issue, payload from outbox where issue = $1 and kind = any($2)`
	args := []any{keptRoot, []string{string(record.OutboxKindSupervise), string(record.OutboxKindNotice)}}
	if dueOnly {
		query += " and next_at <= $3"
		args = append(args, time.Now())
	}
	rows, err := r.pool.Query(r.ctx, query+" order by id", args...)
	if err != nil {
		r.fatalf("list the root's outbox rows: %v", err)
	}
	defer rows.Close()
	var listed []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		var kind string
		if err := rows.Scan(&row.ID, &kind, &row.Issue, &row.Payload); err != nil {
			r.fatalf("scan an outbox row: %v", err)
		}
		row.Kind = record.OutboxKind(kind)
		listed = append(listed, row)
	}
	if err := rows.Err(); err != nil {
		r.fatalf("list the root's outbox rows: %v", err)
	}
	return listed
}

// snapshotRows records the generation every outbox row of the root names, so a prompt the
// architect receives later can be attributed to the start that queued it even once that row is
// gone. A row's generation never changes, and a row is never seen under two of them.
func (r *keptRun) snapshotRows() {
	for _, row := range r.rows(false) {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			continue
		}
		switch value := payload.(type) {
		case record.SuperviseRequest:
			r.rowGen[row.ID] = value.Generation
		case record.Notice:
			if value.CatchUp != nil {
				r.rowGen[row.ID] = value.CatchUp.Generation
			}
		}
	}
}

// deliverControllerNotices takes the controller's notices off the outbox: they go to the project's
// controller, not to the root's architect, and the controller is nobody this run drives.
func (r *keptRun) deliverControllerNotices() {
	if _, err := r.pool.Exec(r.ctx, "delete from outbox where kind = $1", string(record.OutboxKindControllerNotice)); err != nil {
		r.fatalf("deliver the controller's notices: %v", err)
	}
}

func (r *keptRun) putRoot() {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutIssue(r.ctx, tx, r.root)
	}); err != nil {
		r.fatalf("record the root: %v", err)
	}
}

func (r *keptRun) enqueue(payload record.OutboxPayload) {
	r.t.Helper()
	row, err := record.NewOutboxRow(keptRoot, payload, time.Now())
	if err != nil {
		r.fatalf("build an outbox row: %v", err)
	}
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.Enqueue(r.ctx, tx, row)
	}); err != nil {
		r.fatalf("enqueue an outbox row: %v", err)
	}
}

// lastRow is the newest outbox row's id, which names in the trace the row an operation wrote.
func (r *keptRun) lastRow() int64 {
	var id int64
	if err := r.pool.QueryRow(r.ctx, "select coalesce(max(id), 0) from outbox").Scan(&id); err != nil {
		r.fatalf("read the newest outbox row: %v", err)
	}
	return id
}

func (r *keptRun) storedGeneration() uint64 {
	var generation uint64
	if err := r.pool.QueryRow(r.ctx, "select generation from issues where key = $1", keptRoot).Scan(&generation); err != nil {
		r.fatalf("read the root's generation: %v", err)
	}
	return generation
}

func (r *keptRun) waitsForSlot() bool {
	var waits bool
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		waits, err = record.WaitsForSlot(r.ctx, tx, r.records, r.root)
		return err
	}); err != nil {
		r.fatalf("read whether the root waits for its slot: %v", err)
	}
	return waits
}

// waitMachine waits for every send the architect's machine started to have had its outcome
// handled, which is what makes a prompt and its acknowledgement part of the step that caused them.
func (r *keptRun) waitMachine() {
	if machine, found := r.sup.Machine(r.token); found {
		machine.Wait()
	}
}

func (r *keptRun) handle(machine *supervise.Machine, ev supervise.Event) {
	r.t.Helper()
	if err := machine.Handle(r.ctx, ev); err != nil {
		r.fatalf("handle %T: %v", ev, err)
	}
}

func (r *keptRun) tracef(format string, args ...any) {
	r.trace = append(r.trace, fmt.Sprintf("%3d %s", r.step, fmt.Sprintf(format, args...)))
}

// fatalf ends the run with what broke, the seed that replays it, the step it broke at and every
// operation that led there.
func (r *keptRun) fatalf(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("%s\n\nseed %d, step %d (LEGION_KEPT_PROPERTY_SEED=%d replays this run)\ntrace:\n%s",
		fmt.Sprintf(format, args...), r.seed, r.step, r.seed, strings.Join(r.trace, "\n"))
}
