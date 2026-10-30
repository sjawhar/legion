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
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
	// keptNoHolder: no live session holds the role (notify.ErrNoHolder), so the row waits while
	// the architect's claim can hold its role again, and finishes undelivered once nobody will
	// hold it (architectEnded).
	keptNoHolder
	// keptRefuses: the holder does not advertise the delivery mode, so the row finishes
	// undelivered (notify.ErrRefusedByHolder) and releases what it claimed.
	keptRefuses
	// keptReceiptTimeout: the holder did not confirm in time, so the daemon re-holds the notice
	// (notify.ErrNotDelivered) and the copy takes the original's claims over (MoveKeptClaims).
	// Only a notice nobody has re-held yet is answered this way, so the re-hold budget
	// (noticeReholdDelays) is never spent and the copy is never dropped for exhausting it.
	keptReceiptTimeout
	// keptDenied: the listener's own NATS grant refuses the forward (notify.ErrPublishDenied),
	// which no retry can change until an operator changes the grant, so the row finishes
	// undelivered as a refused one does.
	keptDenied
	// keptDeliveryFailed: the listener could not forward the notice at all — the hub down, the
	// role or session unreadable (notify.ReasonDeliveryFailed) — which is none of the answers the
	// executor classifies, so the row fails and retries on its backoff without finishing.
	keptDeliveryFailed
	// keptModes is how many modes the generator chooses among.
	keptModes
)

func (m keptPublishMode) String() string {
	switch m {
	case keptNoHolder:
		return "no-holder"
	case keptRefuses:
		return "refused"
	case keptReceiptTimeout:
		return "receipt-timeout"
	case keptDenied:
		return "denied"
	case keptDeliveryFailed:
		return "delivery-failed"
	default:
		return "delivers"
	}
}

// keptPublisher is the listener as a role publish meets it, under one mode the run switches. It
// counts the answer each catch-up drew, so a run reports which modes its catch-ups actually met.
type keptPublisher struct {
	mu        sync.Mutex
	mode      keptPublishMode
	delivered []outboxPublish
	answered  [keptModes]int
}

func (p *keptPublisher) Publish(_ context.Context, topic, message string, payload any, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	mode, err := p.answer(topic, payload, key)
	if notice, ok := payload.(record.Notice); ok && notice.CatchUp != nil {
		p.answered[mode]++
	}
	if err != nil {
		return err
	}
	p.delivered = append(p.delivered, outboxPublish{topic: topic, message: message, key: key, payload: payload})
	return nil
}

// answer is what this run's mode makes of one publish, and the mode that answer is of: a copy the
// re-hold already queued is delivered rather than timed out again, so the re-hold budget is never
// spent.
func (p *keptPublisher) answer(topic string, payload any, key string) (keptPublishMode, error) {
	switch p.mode {
	case keptNoHolder:
		return p.mode, fmt.Errorf("publish notice to %s: listener returned 404 Not Found: %w", topic, notify.ErrNoHolder)
	case keptRefuses:
		return p.mode, fmt.Errorf("publish notice to %s: listener returned 403 Forbidden: %w", topic,
			notify.ErrRefusedByHolder{Holder: "ses-other"})
	case keptDenied:
		return p.mode, fmt.Errorf("publish notice to %s: listener returned 403 Forbidden: %w", topic,
			notify.ErrPublishDenied{Message: fmt.Sprintf(
				"envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to %q", topic)})
	case keptDeliveryFailed:
		return p.mode, fmt.Errorf("publish notice to %s: listener returned 504 Gateway Timeout: %w", topic,
			notify.ErrNotDelivered{Reason: notify.ReasonDeliveryFailed, EventID: key})
	case keptReceiptTimeout:
		if notice, ok := payload.(record.Notice); ok && notice.Resends == 0 {
			return p.mode, fmt.Errorf("publish notice to %s: listener returned 504 Gateway Timeout: %w", topic,
				notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses-other"})
		}
	}
	return keptDelivers, nil
}

func (p *keptPublisher) setMode(mode keptPublishMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
}

// answers is how many catch-up publishes each mode has answered, which a step reads before and
// after a row's run to see what that run's publish met.
func (p *keptPublisher) answers() [keptModes]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.answered
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

// keptRuntime is the fake runtime with every launch of the architect refused while the run's
// toggle is on — the pod the cluster would not open, the session it could not resume — so a start
// spends the claim's launch budget (supervise.Limits.LaunchFailures) and leaves the claim failed.
type keptRuntime struct {
	*fake.Runtime
	mu       sync.Mutex
	refuses  bool
	refusals int
}

var _ runtime.Runtime = (*keptRuntime)(nil)

func (r *keptRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := r.refusal(); err != nil {
		return runtime.Locator{}, err
	}
	return r.Runtime.Spawn(ctx, spec)
}

func (r *keptRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := r.refusal(); err != nil {
		return runtime.Locator{}, err
	}
	return r.Runtime.Resume(ctx, prev, spec)
}

// refusal is the answer a launch draws while the toggle is on, which supervise charges to the
// claim's launch budget.
func (r *keptRuntime) refusal() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.refuses {
		return nil
	}
	r.refusals++
	return errors.New("tester: no process could be opened for the claim")
}

func (r *keptRuntime) setRefuses(refuses bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuses = refuses
}

func (r *keptRuntime) refusing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refuses
}

func (r *keptRuntime) refused() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusals
}

// keptCounts is what a run's injected failures drove, reported at the run's end so the operations
// the generator chose are shown to have fired rather than assumed.
type keptCounts struct {
	// refusedLaunches is every Spawn or Resume the runtime refused.
	refusedLaunches int
	// failedClaim is every time the architect's claim spent its launch budget and failed.
	failedClaim int
	// retriedFailed is every start row that found the claim failed and relaunched it (the
	// outbox's start, case StateFailed).
	retriedFailed int
	// endedCatchUps is every catch-up the executor finished undelivered because nobody will hold
	// the architect's role again (architectEnded), and endedByFailure those of them whose reason
	// was the claim itself — failed, retired or unsupervised — rather than the lingering tree.
	endedCatchUps  int
	endedByFailure int
	// publishes is how many catch-up publishes each listener mode answered.
	publishes [keptModes]int
}

func (c *keptCounts) add(other keptCounts) {
	c.refusedLaunches += other.refusedLaunches
	c.failedClaim += other.failedClaim
	c.retriedFailed += other.retriedFailed
	c.endedCatchUps += other.endedCatchUps
	c.endedByFailure += other.endedByFailure
	for mode := range c.publishes {
		c.publishes[mode] += other.publishes[mode]
	}
}

func (c keptCounts) String() string {
	return fmt.Sprintf("%d launches refused, %d claim failures, %d starts relaunched a failed claim, "+
		"%d catch-ups finished for an architect that will not hold its role again (%d of them a failed claim); "+
		"catch-up publishes answered: %d delivered, %d no-holder, %d refused, %d receipt-timeout, %d denied, %d delivery-failed",
		c.refusedLaunches, c.failedClaim, c.retriedFailed, c.endedCatchUps, c.endedByFailure,
		c.publishes[keptDelivers], c.publishes[keptNoHolder], c.publishes[keptRefuses],
		c.publishes[keptReceiptTimeout], c.publishes[keptDenied], c.publishes[keptDeliveryFailed])
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
	rt        *keptRuntime
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
	counts   keptCounts
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
// ready, its turn on the task it holds, the forget's failure switched on and off, the runtime's
// refusal of every launch switched on and off, the listener switched between delivering, holding
// no session, refusing the delivery mode, not confirming in time (the re-hold), denying the
// forward under its own NATS grant and failing to forward at all, and the daemon's restart.
//
// The refused launch is what drives the claim through its launch budget
// (supervise.Limits.LaunchFailures) into failed, which the run tells the workflow as the daemon's
// terminal hook does (claimFailed), and from there through the start row's relaunch of a failed
// claim and the catch-up the executor finishes because nobody will hold the architect's role again
// (architectEnded). The denied forward finishes a catch-up undelivered as a refusal does; the
// failed forward is none of the answers the executor classifies, so its row retries on its backoff
// without finishing. Each run reports how often each of these fired (keptCounts).
//
// Five the machinery has are left out. The architect's process dying takes its task back and the
// relaunch sends the same delivery to a fresh agent, which no observation here can tell from a
// second telling, and whether that is one is a question for the specification rather than for this
// test. A newer decision on a reference already kept replaces its row (Store.AddParkedDecision),
// so that reference is told twice by design and "told once per reference" stops meaning anything;
// every decision here is made on a reference of its own. The outbox's own parkDecision, which
// keeps the decision a spec-decision notice could not deliver, reaches the same
// record.RelayParkedDecision this test calls directly, and adds only the notice routing its own
// tests cover. The start's relaunch of a retired claim (the outbox's start, case StateRetired) is
// unreachable here: a claim retires at an operator's stop, at its tree's close or when its linger
// expires, and this life has none of the three — its park leaves the architect suspended, which is
// what the workflow's own suspend writes. The notice the executor cannot route
// (errNoticeUnroutable) needs a record with no such issue, no tree or no recorded root, which the
// rest of the run reads as a broken record rather than as a state the root can be in.
func TestKeptDecisionsReachTheCurrentArchitectOnce(t *testing.T) {
	steps := keptPropertyEnv(t, "LEGION_KEPT_PROPERTY_STEPS", keptPropertySteps)
	var all keptCounts
	var seeds, failedSeeds int
	var modeSeeds [keptModes]int
	for _, seed := range keptPropertySeeds(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			run := newKeptRun(t, seed, steps)
			run.drive()
			run.quiesce()
			run.held()
			run.report()
			all.add(run.counts)
			seeds++
			if run.counts.failedClaim > 0 {
				failedSeeds++
			}
			for mode, count := range run.counts.publishes {
				if count > 0 {
					modeSeeds[mode]++
				}
			}
		})
	}
	t.Logf("%d seeds: %s", seeds, all)
	t.Logf("%d of %d seeds reached a failed architect claim; seeds whose catch-ups met each listener mode: "+
		"%d delivered, %d no-holder, %d refused, %d receipt-timeout, %d denied, %d delivery-failed",
		failedSeeds, seeds, modeSeeds[keptDelivers], modeSeeds[keptNoHolder], modeSeeds[keptRefuses],
		modeSeeds[keptReceiptTimeout], modeSeeds[keptDenied], modeSeeds[keptDeliveryFailed])
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
		rt:        &keptRuntime{Runtime: fake.NewRuntime()},
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
	ops := []func(){r.toggleForget, r.togglePublisher, r.toggleLaunch, r.restartDaemon}
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
	runErr := r.runOne(&row, payload)
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

// runOne leases the row and runs it as its lane would, and records what the run did to the
// architect's claim and to what the row named (noteRun).
func (r *keptRun) runOne(row *record.OutboxRow, payload record.OutboxPayload) error {
	state, answered := r.claimState(), r.publisher.answers()
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	runErr := r.runner.execute(r.ctx, *row)
	r.waitMachine()
	r.noteRun(state, answered, payload, runErr)
	return runErr
}

// noteRun records what one row's run did, read against the claim and the listener as they stood
// before it: a start that relaunched a failed claim (the outbox's start, case StateFailed), a
// catch-up the executor finished because nobody will hold the architect's role again — the only
// ending a no-holder answer has besides the wait — and the claim's own fall to failed, which the
// daemon tells the workflow (claimFailed).
func (r *keptRun) noteRun(before supervise.ClaimState, answered [keptModes]int, payload record.OutboxPayload, runErr error) {
	switch value := payload.(type) {
	case record.SuperviseRequest:
		if value.Op == "start" && before == supervise.StateFailed {
			r.counts.retriedFailed++
		}
	case record.Notice:
		if value.CatchUp != nil && runErr == nil && r.publisher.answers()[keptNoHolder] > answered[keptNoHolder] {
			r.counts.endedCatchUps++
			if claimEnded(before) {
				r.counts.endedByFailure++
			}
			r.tracef("catch-up finished undelivered: nobody will hold the architect's role again (claim=%s)", keptClaimState(before))
		}
	}
	if after := r.claimState(); before != supervise.StateFailed && after == supervise.StateFailed {
		r.counts.failedClaim++
		r.claimFailed()
	}
}

// claimState is the architect's claim as the supervisor holds it now, and "" when this daemon
// supervises none, which is what the notice executor reads for a claim it does not supervise
// (claimEnded).
func (r *keptRun) claimState() supervise.ClaimState {
	machine, found := r.sup.Machine(r.token)
	if !found {
		return ""
	}
	return machine.Claim().State
}

func keptClaimState(state supervise.ClaimState) supervise.ClaimState {
	if state == "" {
		return "none"
	}
	return state
}

// claimFailed tells the workflow that the architect's claim spent its launch budget, as the
// daemon's terminal hook does (workflowRuntime.applyTerminal): the engine writes the tree's
// worker-died notice, which goes to that same architect and waits ahead of its catch-ups.
func (r *keptRun) claimFailed() {
	machine, found := r.sup.Machine(r.token)
	if !found {
		r.fatalf("the architect claim is gone")
	}
	held := machine.Claim()
	if _, err := intake.ApplyFact(r.ctx, r.pool, terminalSource, terminalEventID(held, supervise.StateFailed),
		intake.ClaimFailed{Issue: keptRoot, Role: claim.RoleArchitect, Reason: held.Failure}, r.engine); err != nil {
		r.fatalf("apply the architect's failure at launch %d: %v", held.Generation, err)
	}
	r.tracef("claim-failed launch=%d refusals=%d notice-row=%d", held.Generation, r.rt.refused(), r.lastRow())
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
	mode := keptPublishMode(r.rng.IntN(int(keptModes)))
	r.publisher.setMode(mode)
	r.tracef("publisher=%s", mode)
}

// toggleLaunch switches the runtime's refusal of the architect's launches on and off: while it is
// on, every Spawn and Resume is refused, so the next start of the claim spends its launch budget
// (supervise.Limits.LaunchFailures) and leaves it failed, where its start rows relaunch it and its
// catch-ups find nobody to hold its role.
func (r *keptRun) toggleLaunch() {
	r.rt.setRefuses(!r.rt.refusing())
	r.tracef("launch-refusal=%t claim=%s", r.rt.refusing(), keptClaimState(r.claimState()))
}

func (r *keptRun) restartDaemon() {
	r.restart()
	r.tracef("restart claims=%d architect=%s", r.sup.count(), keptClaimState(r.claimState()))
}

// quiesce ends the run: every injected failure off, the root re-admitted when it is parked or
// waiting — never when it runs, which would hand a lost decision a fresh namer and hide it — and
// then its rows, its architect's launches and its turns driven, in random order, until no row of
// the root is left and no delivery is pending.
func (r *keptRun) quiesce() {
	r.records.fails = false
	r.rt.setRefuses(false)
	r.publisher.setMode(keptDelivers)
	r.tracef("quiesce: every failure off, launches open, publisher delivers")
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
	runErr := r.runOne(&row, payload)
	if runErr == nil {
		if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
			return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
		}); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	r.tracef("run-row id=%d kind=%s what=%s outcome=%s", row.ID, row.Kind, keptRowWhat(payload), keptRunOutcome(runErr, false))
}

// report says what the run's injected failures drove, so a reader of the test's output sees the
// operations the generator chose actually fire.
func (r *keptRun) report() {
	r.counts.refusedLaunches = r.rt.refused()
	r.counts.publishes = r.publisher.answers()
	r.t.Logf("seed %d: %s", r.seed, r.counts)
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
