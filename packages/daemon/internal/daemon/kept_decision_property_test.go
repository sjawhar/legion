package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
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
	// keptRoot is the root every run opens with.
	keptRoot = "LEGION-1"
	// keptChild is the root's one child: an issue of the root's tree a person can decide on while
	// the root lingers, and the issue the root hands over into a tree of its own at the linger's
	// end (handOver), where it becomes a root with an architect of its own.
	keptChild = "LEGION-2"
	// keptTogether is the most rows the concurrent step runs at once, each on a lane of its own.
	keptTogether = 3
)

// keptTelling is one time an architect was told a kept decision: the generation the telling names
// (a catch-up's; a start task names none), whether the root had an architect to hear it when the
// architect received it, the generation that root stood at then, whose architect heard it, the
// tree the decision's own issue stood in then, and what carried it.
type keptTelling struct {
	ref string
	// prompt is whether a start task's prompt carried it. A start task names no generation
	// (workflow.ParkedDecisionTask writes none) and the claim it reaches is the root's one
	// architect session, which holds its task across the tree's re-admissions, so what it is held
	// to is that the root has an architect to hear it (hears) rather than the generation named.
	prompt bool
	hears  bool
	// named is the generation the telling itself names, and root the generation its root stood at
	// when the daemon decided to send it — read where the catch-up's publish began, not once it
	// returned (observe).
	named uint64
	root  uint64
	// row is the outbox row that carried the telling: the start row a prompt's delivery id names,
	// or the row a published notice's dedupe key names (its original, for a copy). It is what says
	// whether the row was composed before a hand-over (held).
	row int64
	// tree is the root whose architect heard it, and issueTree the tree the decision's issue stood
	// in then. A decision is owed to the architect of the tree its issue is in, so a telling whose
	// two differ went to an architect that is not the one it was owed to (held).
	tree      string
	issueTree string
	how       string
	step      int
}

// keptRef is one decision the run tracks: the Dispatch reference a person decided on, and the
// issue of that ask, which is the root itself or its child.
type keptRef struct {
	ref   string
	issue string
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
	// Under the run's own mode only a notice nobody has re-held yet is answered this way, so the
	// re-hold budget (noticeReholdDelays) is never spent and the copy is never dropped for
	// exhausting it; the interleaving step answers the one publish it holds open whatever its
	// resends, which is how a catch-up the run superseded during its publish reaches the
	// re-hold's superseded branch (raceRow).
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

// keptRace is one armed interleaving: the operation that runs, on a goroutine of its own and to
// completion, while one catch-up's publish is held open, and the answer that publish is then
// given. The publisher holds no lock of its own while it runs, so the operation meets the daemon
// exactly where a lane would: the publish is outside every transaction of the row's run.
type keptRace struct {
	what string
	mode keptPublishMode
	run  func() error
	// ran says the publish the race was armed for happened, err what the operation failed with,
	// and outcome what it made of itself, each written by the race's own goroutine before the
	// publish is answered and read by the step that armed it afterwards.
	ran     bool
	err     error
	outcome string
}

// keptPublisher is the listener as a role publish meets it, under one mode the run switches. It
// counts the answer each catch-up drew, so a run reports which modes its catch-ups actually met,
// holds one publish open for the interleaving a step armed (keptRace), and records, for each
// catch-up it takes, the generation that catch-up's root stood at then (atPublish).
type keptPublisher struct {
	mu        sync.Mutex
	mode      keptPublishMode
	delivered []outboxPublish
	answered  [keptModes]int

	raceMu sync.Mutex
	race   *keptRace

	// atPublish is the generation the root whose architect holds a topic stands at, false for a
	// topic no driven tree's architect holds. genMu guards what it answered: genAt, by the
	// notice's dedupe key, and genErr, the read that failed, which ends the run.
	atPublish func(topic string) (uint64, bool, error)
	genMu     sync.Mutex
	genAt     map[string]uint64
	genErr    error
}

func (p *keptPublisher) Publish(_ context.Context, topic, message string, payload any, key string) error {
	notice, isNotice := payload.(record.Notice)
	catchUp := isNotice && notice.CatchUp != nil
	var raced *keptRace
	if catchUp {
		// The generation is read before anything else of this publish: it is the one the daemon
		// decided to send this catch-up at, where its lease-time check read the root, and a
		// re-admission landing inside the publish is followed by its own catch-up rather than
		// making this telling wrong.
		p.noteGeneration(topic, key)
		raced = p.disarm()
	}
	if raced != nil {
		// The listener has the notice and has answered nothing. One other operation runs to
		// completion on a goroutine of its own, and only then is the publish answered.
		done := make(chan struct{})
		go func() {
			defer close(done)
			raced.err = raced.run()
		}()
		<-done
		raced.ran = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mode, err := p.answer(topic, payload, key, raced)
	if catchUp {
		p.answered[mode]++
	}
	if err != nil {
		return err
	}
	p.delivered = append(p.delivered, outboxPublish{topic: topic, message: message, key: key, payload: payload})
	return nil
}

// noteGeneration records the generation the publish's root stands at now, under the notice's
// dedupe key, keeping the first reading of a key: one key is one telling, and that telling is the
// first delivery under it.
func (p *keptPublisher) noteGeneration(topic, key string) {
	if p.atPublish == nil {
		return
	}
	generation, known, err := p.atPublish(topic)
	p.genMu.Lock()
	defer p.genMu.Unlock()
	if err != nil {
		p.genErr = errors.Join(p.genErr, err)
		return
	}
	if _, seen := p.genAt[key]; known && !seen {
		p.genAt[key] = generation
	}
}

// generationAtPublish is the generation the root stood at when the listener took the catch-up
// published under key, and whatever read of one failed, which ends the run.
func (p *keptPublisher) generationAtPublish(key string) (uint64, bool, error) {
	p.genMu.Lock()
	defer p.genMu.Unlock()
	generation, known := p.genAt[key]
	return generation, known, p.genErr
}

// answer is what this run's mode makes of one publish, and the mode that answer is of: a copy the
// re-hold already queued is delivered rather than timed out again, so the re-hold budget is never
// spent. A publish a race held open is answered as the step that armed it chose, whatever the
// notice's resends: that answer is one the generator picked, not the run's standing mode.
func (p *keptPublisher) answer(topic string, payload any, key string, raced *keptRace) (keptPublishMode, error) {
	mode := p.mode
	if raced != nil {
		mode = raced.mode
	}
	switch mode {
	case keptNoHolder:
		return mode, fmt.Errorf("publish notice to %s: listener returned 404 Not Found: %w", topic, notify.ErrNoHolder)
	case keptRefuses:
		return mode, fmt.Errorf("publish notice to %s: listener returned 403 Forbidden: %w", topic,
			notify.ErrRefusedByHolder{Holder: "ses-other"})
	case keptDenied:
		return mode, fmt.Errorf("publish notice to %s: listener returned 403 Forbidden: %w", topic,
			notify.ErrPublishDenied{Message: fmt.Sprintf(
				"envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to %q", topic)})
	case keptDeliveryFailed:
		return mode, fmt.Errorf("publish notice to %s: listener returned 504 Gateway Timeout: %w", topic,
			notify.ErrNotDelivered{Reason: notify.ReasonDeliveryFailed, EventID: key})
	case keptReceiptTimeout:
		if notice, ok := payload.(record.Notice); ok && (raced != nil || notice.Resends == 0) {
			return mode, fmt.Errorf("publish notice to %s: listener returned 504 Gateway Timeout: %w", topic,
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

// arm holds the next catch-up publish open for race, and disarm takes the armed race off, so one
// arming is one interleaving: a step whose publish never came takes its own race back unrun.
func (p *keptPublisher) arm(race *keptRace) {
	p.raceMu.Lock()
	defer p.raceMu.Unlock()
	p.race = race
}

func (p *keptPublisher) disarm() *keptRace {
	p.raceMu.Lock()
	defer p.raceMu.Unlock()
	race := p.race
	p.race = nil
	return race
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
// publish, and the forget's commit leaves it. The toggle is read from the lanes the concurrent
// step runs, so it is held as one word rather than as a field.
type keptForgetStore struct {
	record.Store
	fails atomic.Bool
}

func (s *keptForgetStore) ForgetKeptDecisions(ctx context.Context, tx pgx.Tx, root string, kept record.KeptDecisions, row int64) error {
	if s.fails.Load() {
		return errors.New("tester: the forget's transaction did not commit")
	}
	return s.Store.ForgetKeptDecisions(ctx, tx, root, kept, row)
}

// keptClaimStore is the claim store this run's supervisor writes through: the daemon's own
// (store.Store), over the run's database, so every claim and the task pending on it stand in
// `claims` and `pending_task_deliveries` as a live daemon leaves them. The held task's decisions
// are the reason it is the real one: what such a task names of what its root keeps is persisted
// with the delivery (pending_task_deliveries.kept_decisions), and that row is where a hand-over
// reads what to keep again for the child (record.Store.ReKeepDeliveredDecisions) and where a
// restart reads the claims back from (keptRun.storedClaims).
//
// Two answers are not the daemon's: a claim is admitted by writing it, and every launch passes.
// Both are the tree lifecycle's, which this run opens for the root alone — the child it hands over
// takes an architect claim of its own, under a tree no lifecycle here opens.
type keptClaimStore struct{ *store.Store }

func (s keptClaimStore) AdmitClaim(ctx context.Context, c supervise.Claim) (supervise.Claim, error) {
	return c, s.PutClaim(ctx, c)
}

func (keptClaimStore) CheckLaunch(context.Context, supervise.Claim) error { return nil }

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
	// failedClaim is every time an architect's claim spent its launch budget and failed.
	failedClaim int
	// retriedFailed is every start row that found the claim failed and relaunched it (the
	// outbox's start, case StateFailed).
	retriedFailed int
	// endedCatchUps is every catch-up the executor finished undelivered because nobody will hold
	// the architect's role again (architectEnded), and endedByFailure those of them whose reason
	// was the claim itself — failed, retired or unsupervised — rather than the lingering tree.
	endedCatchUps  int
	endedByFailure int
	// raced is every catch-up publish held open for an interleaving, together every step that ran
	// several rows at once, handovers every child handed into a tree of its own, streamed every
	// restart whose agent then answered get_state in a turn, and foreign every turn the agent
	// started on its own on a claim holding a task nobody sent.
	raced     int
	together  int
	handovers int
	streamed  int
	foreign   int
	// supersededTimeouts is every catch-up whose receipt timeout drew no copy from the re-hold
	// because the operation that ran inside its publish superseded it: the branch that leaves what
	// the row claims to the row's own deletion (notice_exceptions.go's reholdNotice).
	supersededTimeouts int
	// publishes is how many catch-up publishes each listener mode answered.
	publishes [keptModes]int
}

func (c *keptCounts) add(other keptCounts) {
	c.refusedLaunches += other.refusedLaunches
	c.failedClaim += other.failedClaim
	c.retriedFailed += other.retriedFailed
	c.endedCatchUps += other.endedCatchUps
	c.endedByFailure += other.endedByFailure
	c.raced += other.raced
	c.together += other.together
	c.handovers += other.handovers
	c.streamed += other.streamed
	c.foreign += other.foreign
	c.supersededTimeouts += other.supersededTimeouts
	for mode := range c.publishes {
		c.publishes[mode] += other.publishes[mode]
	}
}

func (c keptCounts) String() string {
	return fmt.Sprintf("%d launches refused, %d claim failures, %d starts relaunched a failed claim, "+
		"%d catch-ups finished for an architect that will not hold its role again (%d of them a failed claim); "+
		"%d publishes held open for an interleaving, %d steps ran rows at once, %d children handed over, "+
		"%d restarts met a streaming agent, %d foreign turns, %d receipt timeouts the re-hold found superseded; "+
		"catch-up publishes answered: %d delivered, %d no-holder, %d refused, %d receipt-timeout, %d denied, %d delivery-failed",
		c.refusedLaunches, c.failedClaim, c.retriedFailed, c.endedCatchUps, c.endedByFailure,
		c.raced, c.together, c.handovers, c.streamed, c.foreign, c.supersededTimeouts,
		c.publishes[keptDelivers], c.publishes[keptNoHolder], c.publishes[keptRefuses],
		c.publishes[keptReceiptTimeout], c.publishes[keptDenied], c.publishes[keptDeliveryFailed])
}

// keptTree is one root the run drives and the architect that hears for it: the root every run
// opens with, and the child it hands over, which is a root of its own from then on. Each has its
// claim, its agent's connection and its role topic, and the decisions on the issues of its tree
// are owed to its architect.
type keptTree struct {
	key   string
	issue record.Issue
	token claim.Token
	conn  *fake.Conn
	topic string
	// prompts is how many of the connection's prompts observe has read.
	prompts int
	// handed is whether this tree exists because the root handed this issue over.
	handed bool
}

// keptRun is one seeded life of one root: the real record store, workflow engine, admission,
// outbox runner and claim supervisor over a database of its own, the model the generator drives
// them from, and what each architect was told.
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
	admission *admit.Admission
	trees     *store.Store
	claims    keptClaimStore
	rt        *keptRuntime
	publisher *keptPublisher
	sup       *supervisor
	runner    *outbox
	stateDir  string

	// root is the root the run opens with, child its one child, and roots every tree whose
	// architect the run drives: the root alone until the hand-over, both after it.
	root  *keptTree
	child *keptTree
	roots []*keptTree

	decided  int
	at       time.Time
	tracked  []keptRef
	told     map[string][]keptTelling
	rowGen   map[int64]uint64
	refIssue map[string]string
	// sent is every prompt delivery id an architect has been sent, keys every dedupe key a notice
	// was delivered under — a repeat of either is the same telling, not a second one — and applied
	// every terminal fact key the run has applied, which is what says a launch's ready has been
	// told to the workflow already.
	sent    map[string]bool
	keys    map[string]bool
	applied map[string]bool
	// refused is every driven tree whose catch-up a holder's refusal or the listener's grant
	// finished undelivered while its root heard, which quiesce ends with that architect's relaunch
	// (relaunchAfterRefusal).
	refused map[string]bool
	// handedRow is the newest outbox row at the moment the root handed its child over, 0 before
	// that: every row at or below it was composed while the child was still in the root's tree.
	handedRow int64
	// movedByTheHandOver is every decision the hand-over found still owed inside the old tree —
	// kept for the root, claimed or not, or named by the unconfirmed task held on its architect's
	// claim — so the debt moved into the child's tree with its issue
	// (record.Store.MoveKeptDecisions, ReKeepDeliveredDecisions) and only the child's architect
	// pays it. A decision told and forgotten before then owes that tree nothing (held).
	movedByTheHandOver map[string]bool
	publishd           int
	counts             keptCounts
}

// A person's decision on a tree nobody in it can hear reaches an architect of that tree exactly
// once, whatever order the machinery runs in and whatever runs beside it. The test drives random
// interleavings of the real parts — record.RelayParkedDecision for the keeping,
// workflow.Engine.ParkedDecisionTask and EnqueueNaming for the naming, the outbox runner for the
// delivery and the forget, the supervisor's machines for the architects, admit.Admission for the
// hand-over of a child into a tree of its own, and intake.ApplyFact for the ready — over a real
// PostgreSQL, and holds each run to the invariant after every step:
//
//   - no decision is told twice (a start task prompt or a published catch-up naming it again);
//   - every catch-up telling names the generation its root stood at when the daemon decided to
//     send it — read where its publish began, not once the publish returned, since the publish is
//     network I/O outside every transaction and a re-admission landing inside it is followed by
//     that generation's own catch-up (observe) — and every start-task telling reaches a root that
//     has an architect to hear it: a start task names no generation, and it is held on its claim,
//     unsent, while its root is parked, lingering or waiting for its slot (supervise's
//     Deps.RootHears);
//   - once the run quiesces — every injected failure off, every row drained, each architect ready
//     and its turns ended, and each architect whose catch-up a refusal or a denial finished
//     undelivered while its root heard relaunched once, which is what ends such a refusal in life
//     (relaunchAfterRefusal) — every decision kept has been told once to the architect it was owed
//     to, and nothing is left kept under any root.
//
// Owed to whom is read at the telling, not at the end of the run: the root hands its child into a
// tree of its own (handOver, the status write a linger's end makes and the re-admission admission
// makes of it), and a decision told to the root's architect while the child's issue still stood in
// the root's tree was told to the architect that owed the person an answer, which pays it. What
// the hand-over moves — what the root still keeps of the child, and what the unconfirmed task held
// on its architect's claim names of it — is owed to the child's own architect from then on, and
// only a telling to that architect pays it (held). A telling to an architect that was owed nothing
// is no telling of any debt, and is reported with the debt it left unpaid.
//
// Each seed's operations are its own: the generator is the only source of choices, the machines'
// clock never moves (stillClock), and every wait is for a goroutine the step started. A run whose
// steps all ran one at a time replays exactly from its seed; the two steps that run rows beside
// each other — raceRow, which holds one catch-up's publish open while one other operation runs,
// and runTogether, which runs several due rows at once on lanes of their own — are ordered by
// PostgreSQL and the Go scheduler as much as by the seed, so a failure either reaches is reported
// with its seed and its whole trace and may need more than one replay to come back.
// LEGION_KEPT_PROPERTY_SEED runs one seed, LEGION_KEPT_PROPERTY_RUNS how many seeds and
// LEGION_KEPT_PROPERTY_STEPS how many operations each drives.
//
// The operations a step chooses among are a person's decision on the root or on its child, the
// root's park (to backlog, or lingering after its close) with the architect's suspend, its hand
// back to todo with no slot, its re-admission with the start admission writes, the hand-over of
// its child, one due supervise or notice row of a driven tree run in no particular order
// (finished, or its finish lost to a crash), several due rows run at once, one catch-up run with
// its publish held open for another operation, an architect's agent coming up on its launch, the
// ready fact the daemon's terminal hook then tells the workflow, its turn on the task it holds, a
// turn the agent started on its own while a task nobody sent waits on the claim,
// the daemon's restart, the restart whose agent then answers get_state in a turn, the forget's
// failure switched on and off, the runtime's refusal of every launch switched on and off, and the
// listener switched between delivering, holding no session, refusing the delivery mode, not
// confirming in time (the re-hold), denying the forward under its own NATS grant and failing to
// forward at all.
//
// The refused launch is what drives a claim through its launch budget
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
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories}, quietLogger())
	trees := outboxTreeStore(t, pool, keptRoot)
	run := &keptRun{
		t: t, ctx: ctx, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed))), steps: steps,
		pool: pool, base: base, records: records,
		engine: engine,
		// Admission runs over the project key the records carry, which is what its own reads of the
		// project's live issues match (record.Store.LiveIssues); the claims it opens are keyed by
		// the normalized token of it (claim.ProjectToken), the one the supervisor uses.
		admission:          admit.New(records, engine, 2, "LEGION", quietLogger()),
		trees:              trees,
		claims:             keptClaimStore{Store: trees},
		rt:                 &keptRuntime{Runtime: fake.NewRuntime()},
		publisher:          &keptPublisher{genAt: map[string]uint64{}},
		stateDir:           t.TempDir(),
		at:                 time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC),
		told:               map[string][]keptTelling{},
		movedByTheHandOver: map[string]bool{},
		rowGen:             map[int64]uint64{},
		refIssue:           map[string]string{},
		sent:               map[string]bool{},
		keys:               map[string]bool{},
		applied:            map[string]bool{},
	}
	run.publisher.atPublish = run.rootGeneration
	parent := keptRoot
	run.root = &keptTree{key: keptRoot, token: mustClaimToken(t, keptRoot, claim.RoleArchitect), conn: fake.NewConn(),
		topic: architectTopic(t, keptRoot),
		issue: record.Issue{Key: keptRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: keptRoot, Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress",
			Rank: "A", LastDispatchSeq: 1},
	}
	// The child stands in the root's tree, in a phase of its own, which is what makes it work the
	// hand-over carries into a tree of its own (record.Issue.CarriesRun) rather than work that
	// starts again.
	run.child = &keptTree{key: keptChild, token: mustClaimToken(t, keptChild, claim.RoleArchitect), conn: fake.NewConn(),
		topic: architectTopic(t, keptChild),
		issue: record.Issue{Key: keptChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: keptRoot, Parent: &parent, Title: "Child", Phase: phase.Planning, Generation: 1,
			Status: "in_progress", Rank: "B", LastDispatchSeq: 1},
	}
	run.roots = []*keptTree{run.root}
	run.putIssue(run.root.issue)
	run.putIssue(run.child.issue)
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
		// The daemon's own readings of whether a root architect's task may be sent and of what that
		// task says now (workflowRuntime.rootHears, rootTask), over this run's pool and store.
		RootHears:      (&workflowRuntime{pool: r.pool, records: r.records}).rootHears,
		RootTask:       (&workflowRuntime{pool: r.pool, records: r.records}).rootTask,
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
	stored, err := r.claims.Claims(r.ctx)
	if err != nil {
		r.fatalf("read the stored claims: %v", err)
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
		r.deliverRowsTheRunDoesNotDrive()
	}
}

// enabled is every operation the model state allows now, in a fixed order so one seed always picks
// the same one.
func (r *keptRun) enabled() []func() {
	ops := []func(){r.toggleForget, r.togglePublisher, r.toggleLaunch, r.restartDaemon}
	if !r.architectHears(r.root) {
		ops = append(ops, r.decide)
	}
	if !r.child.handed && r.root.issue.Lingers() {
		ops = append(ops, r.decideChild)
	}
	if r.root.issue.Status == "in_progress" && !r.root.issue.Lingers() {
		ops = append(ops, r.park)
	}
	if record.OutOfWorkflow(r.root.issue.Status) && !r.root.issue.Lingers() {
		ops = append(ops, r.todoWait)
	}
	if r.parkedOrWaiting() {
		ops = append(ops, r.readmit)
	}
	if r.handOverEnabled() {
		ops = append(ops, r.handOver)
	}
	due := r.rows(true)
	if len(due) > 0 {
		ops = append(ops, r.runRow)
	}
	if len(due) > 1 {
		ops = append(ops, r.runTogether)
	}
	if len(r.keptCatchUps(due)) > 0 && len(r.raceOps(0)) > 0 {
		ops = append(ops, r.raceRow)
	}
	for _, tree := range r.roots {
		if r.readyEnabled(tree) {
			ops = append(ops, func() { r.ready(tree) })
		}
		if _, _, told := r.readyFactEnabled(tree); told {
			ops = append(ops, func() { r.readyFact(tree) })
		}
		if r.turnEnabled(tree) {
			ops = append(ops, func() { r.turn(tree) })
		}
		if r.heldTask(tree) {
			ops = append(ops, func() { r.foreignTurn(tree) }, func() { r.restartStreaming(tree) })
		}
	}
	return ops
}

// architectHears is whether an agent holds the tree's architect role now, so a decision made on
// an issue of that tree reaches the tree itself and nothing is kept for it.
func (r *keptRun) architectHears(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	return found && claimTookRole(machine.Claim().State)
}

// parkedOrWaiting is whether the root has no run of its own: parked out of the workflow, lingering
// after its close, or waiting in todo for an admission slot.
func (r *keptRun) parkedOrWaiting() bool {
	return r.root.issue.Status != "in_progress" || r.root.issue.Lingers()
}

// rootKeys is every root the run drives decisions for, which is where kept decisions can stand.
func (r *keptRun) rootKeys() []string {
	keys := make([]string, 0, len(r.roots))
	for _, tree := range r.roots {
		keys = append(keys, tree.key)
	}
	return keys
}

// decide is a person deciding on an ask of the root while nobody in its tree hears it, kept by the
// one rule both paths that find such a decision follow (record.RelayParkedDecision).
func (r *keptRun) decide() { r.decideOn(keptRoot, keptRoot) }

// decideChild is a person deciding on an ask of the root's child while the root lingers: the
// workflow puts the decision to the same rule under the tree's root as the record holds it
// (workflow's relayParkedDecision reads the root of the ask's tree), so it is kept under the root,
// and it is owed to the architect of whatever tree the child stands in when it is told.
func (r *keptRun) decideChild() { r.decideOn(keptChild, keptRoot) }

// decideOn keeps one person's decision made on an ask of issue, under the root of that issue's
// tree, which is the record RelayParkedDecision judges.
func (r *keptRun) decideOn(issue, rootKey string) {
	r.decided++
	ref := fmt.Sprintf("dispatch://%s/ask/ask-%04d", issue, r.decided)
	actor := []string{"user:alice", "user:bob"}[r.rng.IntN(2)]
	kind := []record.DecisionKind{record.DecisionAskAnswered, record.DecisionAskResolved}[r.rng.IntN(2)]
	r.at = r.at.Add(time.Minute)
	outcome := record.RelayNone
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		stored, err := r.records.Issue(r.ctx, tx, rootKey)
		if err != nil || stored == nil {
			return err
		}
		outcome, err = record.RelayParkedDecision(r.ctx, tx, r.records, issue, *stored, true,
			record.ParkedDecision{Ref: ref, Decision: kind, Actor: actor, At: r.at}, 0, "")
		return err
	}); err != nil {
		r.fatalf("keep the decision %s: %v", ref, err)
	}
	if outcome == record.RelayKept || outcome == record.RelaySent {
		r.tracked = append(r.tracked, keptRef{ref: ref, issue: issue})
		r.refIssue[ref] = issue
	}
	r.tracef("decide ref=%s issue=%s by=%s kind=%s outcome=%s kept=%t", ref, issue, actor, kind, keptOutcome(outcome),
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
		r.root.issue.Phase, r.root.issue.Status, r.root.issue.LingerUntil = phase.Done, "done", &until
	} else {
		r.root.issue.Phase, r.root.issue.Status, r.root.issue.LingerUntil = phase.Done, "backlog", nil
	}
	r.putRoot()
	r.enqueue(keptRoot, record.SuperviseRequest{Op: "suspend", Tree: r.root.issue.Tree, Role: claim.RoleArchitect,
		Generation: r.root.issue.Generation, Reason: "the tree left the workflow"})
	r.tracef("park status=%s lingers=%t generation=%d suspend-row=%d", r.root.issue.Status, lingers, r.root.issue.Generation, r.lastRow())
}

// todoWait hands the parked root back to Legion in todo with no slot, where record.WaitsForSlot
// finds it waiting: a person's decision on it is kept for the start its admission will queue, and
// the controller is told nothing.
func (r *keptRun) todoWait() {
	r.root.issue.Status, r.root.issue.HandedOver, r.root.issue.LingerUntil = "todo", true, nil
	r.putRoot()
	r.tracef("todo-wait generation=%d waits=%t", r.root.issue.Generation, r.waitsForSlot())
}

// readmit is the root's next generation: admission's record, and the architect start it enqueues,
// naming what the root keeps and claiming those rows for that start in one transaction, exactly as
// admit.Admission writes them.
func (r *keptRun) readmit() {
	if err := r.readmitRoot(); err != nil {
		r.fatalf("re-admit the root: %v", err)
	}
}

// readmitRoot is readmit without the test's own end: the interleaving step runs it on a goroutine
// of its own, where a failure is the step's to report.
func (r *keptRun) readmitRoot() error {
	r.root.issue.Generation++
	r.root.issue.Status, r.root.issue.Phase, r.root.issue.LingerUntil = "in_progress", phase.Admitted, nil
	if err := r.putIssueErr(r.root.issue); err != nil {
		return err
	}
	return r.enqueueStartRow()
}

func (r *keptRun) enqueueStart() {
	r.t.Helper()
	if err := r.enqueueStartRow(); err != nil {
		r.fatalf("enqueue the architect start of generation %d: %v", r.root.issue.Generation, err)
	}
}

func (r *keptRun) enqueueStartRow() error {
	var kept *record.KeptDecisions
	now := time.Now()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		task, named, err := r.engine.ParkedDecisionTask(r.ctx, tx, r.root.issue)
		if err != nil {
			return err
		}
		kept = named
		return r.engine.EnqueueNaming(r.ctx, tx, r.root.issue.Key, r.root.issue.Key, record.SuperviseRequest{Op: "start",
			Tree: r.root.issue.Tree, Role: claim.RoleArchitect, Generation: r.root.issue.Generation, Task: task, Kept: named}, named, now)
	}); err != nil {
		return err
	}
	r.tracef("re-admit generation=%d start-row=%d names=%d", r.root.issue.Generation, r.lastRow(), keptCount(kept))
	return nil
}

func keptCount(kept *record.KeptDecisions) int {
	if kept == nil {
		return 0
	}
	return len(kept.Decisions)
}

// handOverEnabled is whether the root's linger has a child to hand over: the child still stands in
// the root's tree, in the workflow, and the root lingers after its close, which is where the
// linger's expiry hands a child that is still working to Legion (workflow's settleUnfinished).
func (r *keptRun) handOverEnabled() bool {
	return !r.child.handed && r.root.issue.Lingers() && !record.OutOfWorkflow(r.child.issue.Status)
}

// handOver is the linger's end for a child still in the workflow: the durable fence and the
// Dispatch status write the workflow queues (its handOver), and then the labelled todo Dispatch
// shows for it, which admission re-admits as an orphan carrying its run (readmit's orphan branch)
// and promotes into a tree of its own. From here the child is a root with an architect of its own,
// and a decision a person made on its asks that the hand-over finds still owed inside the old tree
// is owed to that architect instead (movedByTheHandOver).
//
// The status write itself goes to Dispatch, which this run does not drive, so the row is taken off
// with the others the run leaves to their own tests (deliverRowsTheRunDoesNotDrive) and the event
// it would make is applied here, which is the path a person's label takes.
func (r *keptRun) handOver() {
	now := time.Now()
	// Every row the outbox has issued by now was composed while the child was still in the root's
	// tree: a task or catch-up of one of them may name a decision on the child, and telling the old
	// architect it is the one extra telling the invariant accepts (held). It is the newest id the
	// outbox has issued, not the newest row it still holds: a row whose run is over is deleted, and
	// the task it gave waits on its claim, to be prompted whenever that claim is next ready — which
	// is the very row whose telling the invariant accepts.
	r.handedRow = r.issuedRows()
	r.noteTheDebtsTheHandOverMoves()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		if err := r.records.PutHandover(r.ctx, tx, record.Handover{Issue: keptChild, Tree: r.root.issue.Tree,
			Generation: r.root.issue.Generation}); err != nil {
			return err
		}
		row, err := record.NewOutboxRow(keptChild, record.StatusWrite{Status: "todo", ObservedStatus: r.child.issue.Status,
			HandOver: true, HandOverTree: r.root.issue.Tree, HandOverLinger: r.root.issue.Generation}, now)
		if err != nil {
			return err
		}
		return r.records.Enqueue(r.ctx, tx, row)
	}); err != nil {
		r.fatalf("write the hand-over of %s: %v", keptChild, err)
	}
	seq := r.child.issue.LastDispatchSeq + 1
	if _, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", fmt.Sprintf("hand-over:%s:%d", keptChild, seq),
		intake.DispatchIssue{Key: keptChild, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: "todo",
			Title: r.child.issue.Title, Parent: keptRoot, Rank: r.child.issue.Rank, HandedOver: true},
		r.engine, r.admission); err != nil {
		r.fatalf("apply the hand-over of %s: %v", keptChild, err)
	}
	carried := r.issueRecord(keptChild)
	if carried == nil || carried.Tree != keptChild {
		r.fatalf("the handed-over child = %+v, want a tree of its own", carried)
	}
	r.child.issue, r.child.handed = *carried, true
	r.roots = append(r.roots, r.child)
	r.counts.handovers++
	r.tracef("hand-over child=%s tree=%s generation=%d status=%s phase=%s rows-issued-before=%d last-row=%d", keptChild,
		carried.Tree, carried.Generation, carried.Status, carried.Phase, r.handedRow, r.lastRow())
}

// noteTheDebtsTheHandOverMoves records every decision the hand-over is about to move into the
// child's tree, read before admission moves it: what the root still keeps of the child, claimed or
// not (keptForTheChild), and what the unconfirmed task held on the root architect's claim names of
// it (heldForTheChild), whose rows the claim's queueing forgot. Those debts are the child's
// architect's to be told (record.Store.MoveKeptDecisions, ReKeepDeliveredDecisions); a decision
// told and forgotten before this moment is owed to nobody any more, and nothing names it for the
// child's architect ever again (held).
func (r *keptRun) noteTheDebtsTheHandOverMoves() {
	moved := append(r.keptForTheChild(), r.heldForTheChild()...)
	for _, ref := range moved {
		r.movedByTheHandOver[ref] = true
	}
	r.tracef("hand-over moves %d decisions of %s into its own tree: %s", len(moved), keptChild, keptElsewhere(moved))
}

// keptForTheChild is every decision the root still keeps that a person made on the child, claimed
// by a row or not, in reference order.
func (r *keptRun) keptForTheChild() []string {
	rows, err := r.pool.Query(r.ctx, "select ref from parked_decisions where root = $1 and issue = $2 order by ref",
		keptRoot, keptChild)
	if err != nil {
		r.fatalf("read what %s keeps of %s: %v", keptRoot, keptChild, err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			r.fatalf("scan a decision %s keeps of %s: %v", keptRoot, keptChild, err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		r.fatalf("read what %s keeps of %s: %v", keptRoot, keptChild, err)
	}
	return refs
}

// heldForTheChild is every decision on the child that the unconfirmed task held on the root
// architect's claim names (pending_task_deliveries.kept_decisions): those rows went the moment the
// claim queued the task, so the task is the only thing that carries them, and the hand-over keeps
// them again for the child (record.Store.ReKeepDeliveredDecisions, which reads them here).
func (r *keptRun) heldForTheChild() []string {
	var encoded []byte
	err := r.pool.QueryRow(r.ctx, `select kept_decisions from pending_task_deliveries
		where claim_token = $1 and confirmed_at is null`, string(r.root.token)).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		r.fatalf("read the decisions the task held on the architect of %s names: %v", keptRoot, err)
	}
	if len(encoded) == 0 {
		return nil
	}
	var kept record.KeptDecisions
	if err := json.Unmarshal(encoded, &kept); err != nil {
		r.fatalf("decode the decisions the task held on the architect of %s names: %v", keptRoot, err)
	}
	var refs []string
	for _, named := range kept.Decisions {
		if named.Issue == keptChild {
			refs = append(refs, named.Ref)
		}
	}
	return refs
}

// runRow runs one due supervise or notice row of a driven tree, chosen at random rather than in
// row order: the supervise rows of one issue do not run in row order (record's outboxInOrder). A
// row whose run returns nothing is finished as its lane finishes it, unless the generator chose
// the crash: the finish never commits, so the row runs again later. A row whose run failed is left
// for its next run, as a lane leaves it.
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
		if err := r.finishRow(row); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	r.tracef("run-row id=%d issue=%s kind=%s what=%s outcome=%s", row.ID, row.Issue, row.Kind, keptRowWhat(payload),
		keptRunOutcome(runErr, crashed))
}

// runTogether is the daemon's lanes as they really run: several due rows of the driven trees,
// leased at once, each under a lease token of its own, run on a goroutine each and all waited for.
// One row at a time is the shape every other step has, and it is not the shape the daemon has: two
// lanes publish, settle and drop catch-ups beside each other, and only the database serializes
// them. What a step in this mode did is read once every goroutine is done, so a failure it finds
// is reported with its seed and its trace, which replay may not reproduce exactly.
func (r *keptRun) runTogether() {
	due := r.rows(true)
	r.rng.Shuffle(len(due), func(i, j int) { due[i], due[j] = due[j], due[i] })
	count := 2
	if most := min(len(due), keptTogether); most > 2 {
		count = 2 + r.rng.IntN(most-1)
	}
	rows := due[:count]
	before := r.claimStates()
	answered := r.publisher.answers()
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range rows {
		rows[i].LeaseToken = leaseOutboxRow(r.t, r.pool, rows[i].ID)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.runner.execute(r.ctx, rows[i])
		}(i)
	}
	wg.Wait()
	r.waitMachines()
	what := make([]string, 0, count)
	for i, row := range rows {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.fatalf("decode outbox row %d: %v", row.ID, err)
		}
		if errs[i] == nil {
			if err := r.finishRow(row); err != nil {
				r.fatalf("finish outbox row %d: %v", row.ID, err)
			}
		}
		what = append(what, fmt.Sprintf("id=%d issue=%s what=%s outcome=%s", row.ID, row.Issue, keptRowWhat(payload),
			keptRunOutcome(errs[i], false)))
	}
	r.noteTogether(before, answered)
	r.counts.together++
	r.tracef("run-together rows=%d [%s]", count, strings.Join(what, " | "))
}

// raceRow runs one due catch-up with the listener's answer held open: while the listener has the
// notice and has answered nothing, one other operation runs to completion on a goroutine of its
// own, and only then does the publish answer. It is the one interleaving the daemon holds no lock
// across — the publish is outside every transaction of the row's run — and it is where a ready, a
// start's settlement and a re-admission meet a catch-up that is already on its way: each of them
// drops the root's queued catch-ups (Store.DropCatchUps) or moves the root past the generation the
// catch-up states, while the row that is publishing still claims the decisions it names.
func (r *keptRun) raceRow() {
	catchUps := r.keptCatchUps(r.rows(true))
	row := catchUps[r.rng.IntN(len(catchUps))]
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.fatalf("decode outbox row %d: %v", row.ID, err)
	}
	ops := r.raceOps(row.ID)
	if len(ops) == 0 {
		r.fatalf("no operation to interleave with the publish of outbox row %d", row.ID)
	}
	race := r.prepareRace(ops[r.rng.IntN(len(ops))])
	race.mode = []keptPublishMode{keptDelivers, keptReceiptTimeout}[r.rng.IntN(2)]
	answered := r.publisher.answers()
	r.publisher.arm(race)
	runErr := r.runOne(&row, payload)
	r.publisher.disarm()
	if runErr == nil {
		if err := r.finishRow(row); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	if race.ran {
		r.counts.raced++
	}
	if race.err != nil {
		r.fatalf("the operation interleaved with the publish of outbox row %d (%s) failed: %v", row.ID, race.what, race.err)
	}
	r.tracef("race-row id=%d what=%s answer=%s during=%s ran=%t [%s] outcome=%s", row.ID, keptRowWhat(payload),
		race.mode, race.what, race.ran, race.outcome, keptRunOutcome(runErr, false))
	// A receipt timeout the re-hold answered with no copy of its own is the superseded branch
	// (notice_exceptions.go's reholdNotice): the operation that ran inside the publish moved the
	// root or its architect past the catch-up, so nothing takes over the claims this row holds and
	// its deletion releases them. A first publish is under the re-hold budget, so nothing else
	// refuses it a copy.
	if r.publisher.answers()[keptReceiptTimeout] > answered[keptReceiptTimeout] && r.copiesOf(row.ID) == 0 {
		r.counts.supersededTimeouts++
		r.tracef("the re-hold of row %d queued no copy: the publish was superseded while it ran, so the row's own deletion releases what it claimed", row.ID)
	}
}

// copiesOf is how many copies of an outbox row the re-hold has queued (record.Notice.ResendOf).
func (r *keptRun) copiesOf(row int64) int {
	var copies int
	if err := r.pool.QueryRow(r.ctx, "select count(*) from outbox where (payload->>'resend_of')::bigint = $1", row).Scan(&copies); err != nil {
		r.fatalf("count the copies of outbox row %d: %v", row, err)
	}
	return copies
}

// keptRaceOp is one operation the generator can run inside a catch-up's publish, named before the
// step picks among them so that one seed always picks the same one.
type keptRaceOp struct {
	what   string
	kind   string
	tree   *keptTree
	row    record.OutboxRow
	key    string
	launch uint64
}

// raceOps is every operation that can run inside a catch-up's publish now: the ready fact of a
// launch whose agent is up and whose ready the daemon has not told the workflow yet, which writes
// that launch its catch-up; one other due start row of a driven tree — a stale start, whose
// settlement hands over, or one that finds the architect running and applies the ready fact
// itself; and the root's re-admission, which is the other thing that supersedes a catch-up
// (catchUpSuperseded reads the root's generation against the catch-up's). Each of the three drops
// the root's queued catch-ups or moves the root past the one that is publishing. The row the step
// is publishing is left out, and so is every row that touches no kept decision.
func (r *keptRun) raceOps(publishing int64) []keptRaceOp {
	var ops []keptRaceOp
	for _, tree := range r.roots {
		key, launch, told := r.readyFactEnabled(tree)
		if !told {
			continue
		}
		ops = append(ops, keptRaceOp{kind: "ready", tree: tree, key: key, launch: launch,
			what: fmt.Sprintf("the ready fact of %s's architect at launch %d", tree.key, launch)})
	}
	for _, row := range r.rows(true) {
		if row.ID == publishing || row.Kind != record.OutboxKindSupervise {
			continue
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			continue
		}
		if request, ok := payload.(record.SuperviseRequest); !ok || request.Op != "start" {
			continue
		}
		ops = append(ops, keptRaceOp{kind: "row", row: row,
			what: fmt.Sprintf("the run of row %d of %s (%s)", row.ID, row.Issue, keptRowWhat(payload))})
	}
	if r.parkedOrWaiting() {
		ops = append(ops, keptRaceOp{kind: "readmit", what: fmt.Sprintf("the re-admission of %s", keptRoot)})
	}
	return ops
}

// prepareRace is the armed interleaving for one operation: whatever a lease or a claim token the
// operation needs is taken here, on the test's own goroutine, so the goroutine the publish starts
// does nothing but the daemon's work and reports what it made of it.
func (r *keptRun) prepareRace(op keptRaceOp) *keptRace {
	race := &keptRace{what: op.what}
	switch op.kind {
	case "ready":
		race.run = func() error {
			if err := r.applyReadyFact(op.tree, op.key, op.launch); err != nil {
				return err
			}
			race.outcome = "the ready fact was applied"
			return nil
		}
	case "row":
		row := op.row
		row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
		race.run = func() error {
			runErr := r.runner.execute(r.ctx, row)
			if runErr == nil {
				if err := r.finishRow(row); err != nil {
					return err
				}
			}
			race.outcome = keptRunOutcome(runErr, false)
			return nil
		}
	default:
		race.run = func() error {
			if err := r.readmitRoot(); err != nil {
				return err
			}
			race.outcome = fmt.Sprintf("the root stands at generation %d", r.root.issue.Generation)
			return nil
		}
	}
	return race
}

// runOne leases the row and runs it as its lane would, and records what the run did to the
// architect's claim and to what the row named (noteRun).
func (r *keptRun) runOne(row *record.OutboxRow, payload record.OutboxPayload) error {
	tree := r.treeRow(row.Issue)
	state, answered := r.claimState(tree), r.publisher.answers()
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	runErr := r.runner.execute(r.ctx, *row)
	r.waitMachines()
	r.noteRun(tree, state, answered, payload, runErr)
	return runErr
}

// finishRow finishes a row as its lane finishes it, under the lease the run took.
func (r *keptRun) finishRow(row record.OutboxRow) error {
	return pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
	})
}

// noteRun records what one row's run did, read against the claim and the listener as they stood
// before it: a start that relaunched a failed claim (the outbox's start, case StateFailed), a
// catch-up the executor finished because nobody will hold the architect's role again — the only
// ending a no-holder answer has besides the wait — a catch-up a refusal or a denial finished
// undelivered while its root heard (noteRefusal), and the claim's own fall to failed, which the
// daemon tells the workflow (claimFailed).
func (r *keptRun) noteRun(tree *keptTree, before supervise.ClaimState, answered [keptModes]int, payload record.OutboxPayload, runErr error) {
	switch value := payload.(type) {
	case record.SuperviseRequest:
		if value.Op == "start" && before == supervise.StateFailed {
			r.counts.retriedFailed++
		}
	case record.Notice:
		if value.CatchUp == nil {
			break
		}
		if runErr == nil && r.publisher.answers()[keptNoHolder] > answered[keptNoHolder] {
			r.counts.endedCatchUps++
			if claimEnded(before) {
				r.counts.endedByFailure++
			}
			r.tracef("catch-up finished undelivered: nobody will hold the architect's role again (claim=%s)", keptClaimState(before))
		}
		if tree != nil && refusedOrDenied(r.publisher.answers(), answered) {
			r.noteRefusal(tree)
		}
	}
	if tree == nil {
		return
	}
	if after := r.claimState(tree); before != supervise.StateFailed && after == supervise.StateFailed {
		r.counts.failedClaim++
		r.claimFailed(tree)
	}
}

// noteTogether reads what a step that ran several rows at once did: which architect claims fell to
// failed while they ran, which the daemon's terminal hook tells the workflow, how many catch-ups
// ended for an architect that will not hold its role again, and whether a refusal or a denial
// finished one undelivered. Which row did which is not read: the rows ran beside each other, and
// only what they left behind is a fact of the step, so a refusal is noted for every driven tree
// whose root heard then.
func (r *keptRun) noteTogether(before map[string]supervise.ClaimState, answered [keptModes]int) {
	if ended := r.publisher.answers()[keptNoHolder] - answered[keptNoHolder]; ended > 0 {
		r.counts.endedCatchUps += ended
	}
	refused := refusedOrDenied(r.publisher.answers(), answered)
	for _, tree := range r.roots {
		if refused {
			r.noteRefusal(tree)
		}
		if after := r.claimState(tree); before[tree.key] != supervise.StateFailed && after == supervise.StateFailed {
			r.counts.failedClaim++
			r.claimFailed(tree)
		}
	}
}

// refusedOrDenied says whether a catch-up publish was refused by its architect's holder or denied
// by the listener's own NATS grant between the two readings of the publisher's answers: the two
// endings that finish a catch-up undelivered with its decisions left to the row's deletion, which
// no retry of that row and no fresh catch-up can change while the refusal stands.
func refusedOrDenied(after, before [keptModes]int) bool {
	return after[keptRefuses] > before[keptRefuses] || after[keptDenied] > before[keptDenied]
}

// noteRefusal records that a catch-up of tree was refused or denied while tree's root had an
// architect to hear it, so what that catch-up named was released by the row's deletion with
// nothing to name it again. What ends a refusal in real life is the architect's next launch, whose
// ready names every unclaimed decision, and quiesce drives one (relaunchAfterRefusal).
func (r *keptRun) noteRefusal(tree *keptTree) {
	if !r.architectToHear(tree.key) {
		return
	}
	if r.refused == nil {
		r.refused = map[string]bool{}
	}
	r.refused[tree.key] = true
}

// claimStates is the state of every driven tree's architect claim now, by root key.
func (r *keptRun) claimStates() map[string]supervise.ClaimState {
	states := make(map[string]supervise.ClaimState, len(r.roots))
	for _, tree := range r.roots {
		states[tree.key] = r.claimState(tree)
	}
	return states
}

// claimState is the architect's claim as the supervisor holds it now, and "" when this daemon
// supervises none, which is what the notice executor reads for a claim it does not supervise
// (claimEnded).
func (r *keptRun) claimState(tree *keptTree) supervise.ClaimState {
	if tree == nil {
		return ""
	}
	machine, found := r.sup.Machine(tree.token)
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

// claimFailed tells the workflow that an architect's claim spent its launch budget, as the
// daemon's terminal hook does (workflowRuntime.applyTerminal): the engine writes the tree's
// worker-died notice, which goes to that same architect and waits ahead of its catch-ups.
func (r *keptRun) claimFailed(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	held := machine.Claim()
	if _, err := intake.ApplyFact(r.ctx, r.pool, terminalSource, terminalEventID(held, supervise.StateFailed),
		intake.ClaimFailed{Issue: tree.key, Role: claim.RoleArchitect, Reason: held.Failure}, r.engine); err != nil {
		r.fatalf("apply the failure of %s's architect at launch %d: %v", tree.key, held.Generation, err)
	}
	r.tracef("claim-failed tree=%s launch=%d refusals=%d notice-row=%d", tree.key, held.Generation, r.rt.refused(), r.lastRow())
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

// readyEnabled is whether a tree's architect claim has a launch to make ready: one coming up, or
// one live whose connection a restart took with it.
func (r *keptRun) readyEnabled(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	switch state := machine.Claim().State; state {
	case supervise.StateLaunching, supervise.StateShimConnected, supervise.StateRegistered:
		return true
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		_, connected := r.sup.deps.Conns.Conn(tree.token)
		return !connected
	default:
		return false
	}
}

// ready is an architect's agent coming up on its launch: its connection, the shim's hello, the
// registration and the ready. What the workflow makes of that ready is a step of its own
// (readyFact): the daemon's terminal hook runs outside the machine's own transition, so a claim
// stands ready for as long as that hook takes to tell the workflow, and a restart that forgot it
// has only the claim's stored state to tell it from.
func (r *keptRun) ready(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	r.sup.deps.Conns.(*fake.Conns).Register(tree.token, tree.conn)
	launch := machine.Claim().Generation
	r.handle(machine, supervise.StreamHello{Claim: tree.token, Generation: launch})
	if state := machine.Claim().State; state == supervise.StateShimConnected || state == supervise.StateRegistered || claimTookRole(state) {
		r.handle(machine, supervise.RequestRegister{Claim: tree.token, Generation: launch, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"})
	}
	if state := machine.Claim().State; state == supervise.StateRegistered || claimTookRole(state) {
		r.handle(machine, supervise.RequestReady{Claim: tree.token, Generation: launch, Session: "ses-architect"})
	}
	r.waitMachines()
	r.tracef("ready tree=%s launch=%d state=%s", tree.key, launch, machine.Claim().State)
}

// readyFactEnabled is whether a launch an architect's agent came up on is one the workflow has
// not been told about yet: the claim holds its role, and the fact that launch's ready writes
// (terminalEventID, one per launch) has not been applied.
func (r *keptRun) readyFactEnabled(tree *keptTree) (string, uint64, bool) {
	machine, found := r.sup.Machine(tree.token)
	if !found || !claimTookRole(machine.Claim().State) {
		return "", 0, false
	}
	launch := machine.Claim().Generation
	key := terminalEventID(supervise.Claim{Token: tree.token, Generation: launch}, supervise.StateReady)
	return key, launch, !r.applied[key]
}

// readyFact is the ready fact the daemon applies for a launch its agent came up on
// (workflowRuntime.applyTerminal), which is what writes that launch its catch-up (workflow's
// claimReady).
func (r *keptRun) readyFact(tree *keptTree) {
	key, launch, enabled := r.readyFactEnabled(tree)
	if !enabled {
		r.fatalf("the architect of %s has no ready to tell the workflow", tree.key)
	}
	if err := r.applyReadyFact(tree, key, launch); err != nil {
		r.fatalf("apply the ready of %s's architect for launch %d: %v", tree.key, launch, err)
	}
	r.waitMachines()
	r.tracef("ready-fact tree=%s launch=%d catch-up-row=%d", tree.key, launch, r.lastRow())
}

// applyReadyFact is readyFact's write alone, which the interleaving step runs on a goroutine of
// its own, where a failure is the step's to report.
func (r *keptRun) applyReadyFact(tree *keptTree, key string, launch uint64) error {
	if _, err := intake.ApplyFact(r.ctx, r.pool, terminalSource, key,
		intake.ClaimReady{Issue: tree.key, Role: claim.RoleArchitect, Launch: launch}, r.engine); err != nil {
		return err
	}
	r.applied[key] = true
	return nil
}

// turnEnabled is whether an architect holds a task it acknowledged and has not started a turn on,
// or is working a turn that confirmed its task — the turn a restart's get_state found, or one the
// agent started itself — which only its end retires.
func (r *keptRun) turnEnabled(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	held := machine.Claim()
	pending := held.Pending
	if pending == nil {
		return false
	}
	if !pending.DeliveredAt.IsZero() && pending.ConfirmedAt.IsZero() {
		return true
	}
	return held.State == supervise.StateWorking && !pending.ConfirmedAt.IsZero()
}

// turn is the architect's turn on the task it holds, from the stream's agent_start to its
// agent_end, which retires the delivery. A claim already working is in the turn: only its end is
// left, and the agent's own state says so no longer (SetStreaming).
func (r *keptRun) turn(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	if machine.Claim().State != supervise.StateWorking {
		r.handle(machine, supervise.StreamTurnStart{Claim: tree.token})
		r.waitMachines()
	}
	tree.conn.SetStreaming(false)
	r.handle(machine, supervise.StreamTurnEnd{Claim: tree.token})
	r.waitMachines()
	r.tracef("turn tree=%s state=%s pending=%t", tree.key, machine.Claim().State, machine.Claim().Pending != nil)
}

// heldTask is whether the architect's claim holds a task of a run that nobody has sent and nobody
// has acknowledged: a root architect's start task held back because its root has no architect to
// hear it (supervise's sendPending, Deps.RootHears). Its decisions were forgotten when the start
// row gave it to the claim, so this task is the only thing that carries them.
func (r *keptRun) heldTask(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	held := machine.Claim()
	pending := held.Pending
	return pending != nil && pending.ConfirmedAt.IsZero() && pending.DeliveredAt.IsZero() && pending.Generation != 0 &&
		(held.State == supervise.StateReady || held.State == supervise.StateIdle)
}

// foreignTurn is a turn the agent started on its own — an Envoy notice, a person's message in its
// session — while a task nobody sent waits on its claim: agent_start naming no delivery, and then
// its end. A restored claim the machine still has to ask about (askFirst) counts that turn as the
// held task's (table.go's turnStarted), so the task is confirmed without ever being sent.
func (r *keptRun) foreignTurn(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	before := machine.Claim().Pending.ID
	r.handle(machine, supervise.StreamTurnStart{Claim: tree.token})
	r.waitMachines()
	r.handle(machine, supervise.StreamTurnEnd{Claim: tree.token})
	r.waitMachines()
	r.counts.foreign++
	r.tracef("foreign-turn tree=%s delivery=%s state=%s pending=%t", tree.key, before, machine.Claim().State,
		machine.Claim().Pending != nil)
}

// restartStreaming is the daemon dying and booting while an architect holds a task nobody sent,
// and its agent then answering get_state that it is in a turn. The restored claim is one the
// machine has to ask about before it acts (askFirst), and the answer it gets is what
// sendPending's askFirst block reads before anything asks whether the root hears.
func (r *keptRun) restartStreaming(tree *keptTree) {
	r.restart()
	tree.conn.SetStreaming(true)
	r.counts.streamed++
	r.tracef("restart-streaming tree=%s claims=%d architect=%s: its agent answers get_state in a turn",
		tree.key, r.sup.count(), keptClaimState(r.claimState(tree)))
}

func (r *keptRun) toggleForget() {
	r.records.fails.Store(!r.records.fails.Load())
	r.tracef("forget-failure=%t", r.records.fails.Load())
}

func (r *keptRun) togglePublisher() {
	mode := keptPublishMode(r.rng.IntN(int(keptModes)))
	r.publisher.setMode(mode)
	r.tracef("publisher=%s", mode)
}

// toggleLaunch switches the runtime's refusal of every launch on and off: while it is on, every
// Spawn and Resume is refused, so the next start of a claim spends its launch budget
// (supervise.Limits.LaunchFailures) and leaves it failed, where its start rows relaunch it and its
// catch-ups find nobody to hold its role.
func (r *keptRun) toggleLaunch() {
	r.rt.setRefuses(!r.rt.refusing())
	r.tracef("launch-refusal=%t architect=%s", r.rt.refusing(), keptClaimState(r.claimState(r.root)))
}

func (r *keptRun) restartDaemon() {
	r.restart()
	r.tracef("restart claims=%d architect=%s", r.sup.count(), keptClaimState(r.claimState(r.root)))
}

// quiesce ends the run: every injected failure off, the root re-admitted when it is parked or
// waiting — never when it runs, which would hand a lost decision a fresh namer and hide it — and
// then the rows of every driven tree, its architects' launches and their turns driven, in random
// order, until no row is left and no delivery is pending.
//
// One thing more is driven, and only where the run met it: a catch-up a holder's refusal or the
// listener's own grant finished undelivered while its root heard (noteRefusal) leaves what it
// named to the row's deletion, and nothing names it again while that refusal stands — a fresh
// catch-up would be refused too. What ends a refusal in real life is the architect's next launch,
// so once nothing else is left to drive, each such architect is relaunched once
// (relaunchAfterRefusal) and that launch's ready names every unclaimed decision. Nothing else is
// relaxed: the relaunch is an operation of the machinery, driven as every other step's is.
func (r *keptRun) quiesce() {
	r.records.fails.Store(false)
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
		for _, tree := range r.roots {
			if r.readyEnabled(tree) {
				actions = append(actions, func() { r.ready(tree) })
			}
			if _, _, told := r.readyFactEnabled(tree); told {
				actions = append(actions, func() { r.readyFact(tree) })
			}
			if r.turnEnabled(tree) {
				actions = append(actions, func() { r.turn(tree) })
			}
		}
		if len(actions) == 0 {
			if r.relaunchAfterRefusal() {
				continue
			}
			r.tracef("quiesced")
			return
		}
		actions[r.rng.IntN(len(actions))]()
		r.observe()
		r.deliverRowsTheRunDoesNotDrive()
	}
	r.fatalf("the rows and deliveries of %s did not drain in %d quiescence steps", strings.Join(r.rootKeys(), ", "), keptQuiesceBound)
}

// relaunchAfterRefusal ends one refusal the run met: the architect whose catch-up a holder's
// refusal or the listener's grant finished undelivered while its root heard is relaunched, which
// is what ends such a refusal in life — the session that would not take the delivery is gone, and
// the launch that replaces it is told everything unclaimed in its ready's catch-up. Its process is
// ended through the fake runtime as a death, which the supervisor answers with the relaunch; the
// agent is then brought up (ready) and the workflow told that ready (readyFact), as every launch
// of the run is. False is no refusal left to end.
func (r *keptRun) relaunchAfterRefusal() bool {
	for _, tree := range r.roots {
		if !r.refused[tree.key] {
			continue
		}
		delete(r.refused, tree.key)
		machine, found := r.sup.Machine(tree.token)
		if !found || machine.Claim().Locator == nil {
			r.tracef("the refusal of %s's catch-up ends with no relaunch: its architect has no process", tree.key)
			continue
		}
		r.step++
		if err := machine.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
			Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now()}}); err != nil {
			r.fatalf("end the process of %s's architect after its catch-up was refused: %v", tree.key, err)
		}
		r.waitMachines()
		r.tracef("relaunch tree=%s architect=%s: the session that refused its catch-up is gone", tree.key,
			keptClaimState(r.claimState(tree)))
		if r.readyEnabled(tree) {
			r.ready(tree)
		}
		if _, _, told := r.readyFactEnabled(tree); told {
			r.readyFact(tree)
		}
		r.observe()
		r.deliverRowsTheRunDoesNotDrive()
		return true
	}
	return false
}

// runLeftoverRow runs one of the remaining rows whatever its backoff says: a quiesced daemon has
// run every row it holds, including a re-held copy waiting out its delay.
func (r *keptRun) runLeftoverRow() {
	left := r.rows(false)
	row := left[r.rng.IntN(len(left))]
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.fatalf("decode outbox row %d: %v", row.ID, err)
	}
	runErr := r.runOne(&row, payload)
	if runErr == nil {
		if err := r.finishRow(row); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	r.tracef("run-row id=%d issue=%s kind=%s what=%s outcome=%s", row.ID, row.Issue, row.Kind, keptRowWhat(payload),
		keptRunOutcome(runErr, false))
}

// report says what the run's injected failures drove, so a reader of the test's output sees the
// operations the generator chose actually fire.
func (r *keptRun) report() {
	r.counts.refusedLaunches = r.rt.refused()
	r.counts.publishes = r.publisher.answers()
	r.t.Logf("seed %d: %s", r.seed, r.counts)
}

// held is the run's verdict once it has quiesced: every decision kept was told exactly once to the
// architect it was owed to, and no root keeps anything.
//
// Owed to which architect is read at the telling, not at the end of the run: a decision is told to
// the architect of the tree its own issue stood in when it was told, and that is a correct
// telling, whatever the tree the issue stands in later. A person deciding on the root's child
// while the child is still of the root's tree is deciding on the root's tree, and the root's
// architect is who owes an answer; the hand-over then moves only what is still owed inside that
// tree — what the root still keeps of the child, and what the unconfirmed task held on its
// architect's claim names of it (movedByTheHandOver, record.Store.MoveKeptDecisions,
// ReKeepDeliveredDecisions). What the hand-over moves is owed to the child's own architect from
// then on, and only that architect's telling pays it; what it leaves behind was paid where it
// stood and is named to nobody again.
//
// One telling more is accepted, and only one: a decision on an issue the root handed over, carried
// to the old tree's architect by a row composed before that hand-over. Such a row's task or
// catch-up names the reference in text written while the issue was still in that tree, and the row
// can be prompted or published long after — a start whose launch kept failing, a task held on the
// claim until its root heard again. For a decision the hand-over moved, telling the old architect
// pays nothing of what the child's architect is owed, whether it reached that architect before the
// hand-over or after it, and the decision is kept for the child all the same, so the child's
// architect is told it too: a person's decision is never lost, and told twice is the lesser fault.
// Any other telling to another tree's architect, and any second telling to one architect, is a
// fault.
func (r *keptRun) held() {
	for _, tracked := range r.tracked {
		tree := r.treeOf(tracked.issue)
		var owed int
		var elsewhere, overheard []string
		for _, telling := range r.told[tracked.ref] {
			if r.pays(tracked.ref, telling, tree) {
				owed++
				continue
			}
			line := fmt.Sprintf("at step %d by %s, the architect of %s, while %s stood in the tree of %s",
				telling.step, telling.how, telling.tree, tracked.issue, telling.issueTree)
			if r.beforeTheHandOver(telling, tree) {
				overheard = append(overheard, line)
				continue
			}
			elsewhere = append(elsewhere, line)
		}
		if owed == 1 && len(elsewhere) == 0 && len(overheard) <= 1 {
			continue
		}
		r.fatalf("%s, an ask of %s now in the tree of %s, which the hand-over moved: %t, was told %d times to the architect it was owed to, "+
			"%d times to another tree's architect (%s) and %d times to the architect of the tree it left by a row composed before the hand-over (%s), "+
			"want once, never, and at most once: a person's decision kept while nobody in its tree could hear it must reach the architect of the tree "+
			"its own issue stood in when it was told, and the architect of the tree it moved into when the hand-over found it still kept or still held",
			tracked.ref, tracked.issue, tree, r.movedByTheHandOver[tracked.ref], owed, len(elsewhere), keptElsewhere(elsewhere),
			len(overheard), keptElsewhere(overheard))
	}
	for _, root := range r.rootKeys() {
		if left := keptDecisions(r.t, r.pool, r.base, root); len(left) != 0 {
			r.fatalf("%s still keeps %d decisions after the run quiesced (%s), want none: each was named by a start or catch-up that never reached an architect, and nothing names them again",
				root, len(left), keptRefs(left))
		}
	}
}

// beforeTheHandOver says whether a telling that reached another tree's architect than the one the
// decision's issue stands in now is the one such telling the invariant accepts: it went to the tree
// the issue left — the root the run opens with, the only tree a hand-over here takes an issue out
// of — and the row that carried it was composed before that hand-over (handedRow, the newest row
// when it ran), so its text named the reference while the issue was still in that tree.
func (r *keptRun) beforeTheHandOver(telling keptTelling, tree string) bool {
	return r.handedRow != 0 && tree != keptRoot && telling.tree == keptRoot && telling.row != 0 && telling.row <= r.handedRow
}

// pays says whether a telling pays what the decision owes. A decision the hand-over moved into a
// tree of its own is owed to that tree's architect alone, however much of its life it spent in the
// tree it left; every other decision is owed to the architect of the tree its issue stood in when
// it was told, which is the tree that owed the person an answer then.
func (r *keptRun) pays(ref string, telling keptTelling, tree string) bool {
	if r.movedByTheHandOver[ref] {
		return telling.tree == tree
	}
	return telling.tree == telling.issueTree
}

func keptElsewhere(elsewhere []string) string {
	if len(elsewhere) == 0 {
		return "none"
	}
	return strings.Join(elsewhere, "; ")
}

func keptRefs(decisions []record.ParkedDecision) string {
	refs := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		refs = append(refs, fmt.Sprintf("%s claimed-by=%d", decision.Ref, decision.ClaimedBy))
	}
	return strings.Join(refs, ", ")
}

// observe reads what each architect was told since the last step — the prompts its connection
// received and the catch-ups the publisher delivered to its role topic — and holds each telling to
// the invariant: no reference twice, a catch-up naming the generation its root stood at when the
// daemon decided to send it, and a start task reaching a root that has an architect to hear it.
//
// The generation a catch-up is held to is the one the publisher read as the listener took it
// (generationAtPublish), not the one the root stands at once the publish has returned: the daemon
// decides to send a catch-up at its lease-time check, before the publish, and the publish is
// network I/O outside every transaction. A re-admission that lands inside it leaves this telling
// the generation it was decided at, and is followed by that generation's own catch-up, queued
// behind this one.
//
// One telling is one delivery, not one frame: a prompt frame carries the daemon's delivery id, and
// a shim that sees the same id twice answers it without a second turn (supervise.Delivery), so the
// re-send a reconnect or a retried row makes is the same telling; a published notice carries the
// outbox key of the row it is a copy of (record.Notice.Published), which the listener's role lane
// and the hosts dedupe on, so a copy published under a key already delivered is too.
func (r *keptRun) observe() {
	for _, tree := range r.roots {
		generation := r.storedGeneration(tree.key)
		prompts := tree.conn.Prompts()
		for ; tree.prompts < len(prompts); tree.prompts++ {
			prompt := prompts[tree.prompts]
			if r.sent[prompt.DeliveryID] {
				continue
			}
			r.sent[prompt.DeliveryID] = true
			row := keptRowOfDelivery(prompt.DeliveryID)
			named, known := r.rowGen[row]
			if !known {
				r.fatalf("the architect of %s was prompted by delivery %q, whose outbox row %d the run never saw", tree.key, prompt.DeliveryID, row)
			}
			for _, ref := range r.refsIn(prompt.Message) {
				r.tell(keptTelling{ref: ref, prompt: true, hears: r.architectToHear(tree.key), named: named, root: generation, row: row,
					tree: tree.key, issueTree: r.treeOf(r.refIssue[ref]), step: r.step,
					how: fmt.Sprintf("the start task of row %d, prompt %s", row, prompt.DeliveryID)})
			}
		}
	}
	published := r.publisher.published()
	for ; r.publishd < len(published); r.publishd++ {
		delivered := published[r.publishd]
		tree := r.treeTopic(delivered.topic)
		if tree == nil || r.keys[delivered.key] {
			continue
		}
		notice, ok := delivered.payload.(record.Notice)
		if !ok || notice.CatchUp == nil {
			continue
		}
		r.keys[delivered.key] = true
		generation, known, err := r.publisher.generationAtPublish(delivered.key)
		if err != nil {
			r.fatalf("the generation a catch-up published at could not be read: %v", err)
		}
		if !known {
			r.fatalf("the catch-up published under %s to the architect of %s was taken by the listener without its root's generation read",
				delivered.key, tree.key)
		}
		for _, ref := range r.refsIn(notice.CatchUp.Decided) {
			r.tell(keptTelling{ref: ref, named: notice.CatchUp.Generation, root: generation, row: keptRowOfKey(delivered.key), tree: tree.key,
				issueTree: r.treeOf(r.refIssue[ref]), step: r.step,
				how: fmt.Sprintf("the catch-up published under %s", delivered.key)})
		}
	}
}

// architectToHear is whether the root key, as the record holds it now, has an architect to hear a
// decision: its tree's root, in the workflow, not lingering, and not waiting in `todo` for its
// slot (workflow.ArchitectHears, the reading the daemon holds a start task to). A start task's
// prompt is held to this rather than to the generation it names, since it names none and reaches
// the root's one architect session whenever that session is next ready.
func (r *keptRun) architectToHear(key string) bool {
	var hears bool
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		root, err := r.records.Issue(r.ctx, tx, key)
		if err != nil || root == nil {
			return err
		}
		hears, err = workflow.ArchitectHears(r.ctx, tx, r.records, *root)
		return err
	}); err != nil {
		r.fatalf("read whether %s has an architect to hear a decision: %v", key, err)
	}
	return hears
}

// tell records one telling and holds it to the invariants a telling can break on its own: no
// architect is told one decision twice, a start task reaches a root that has an architect to hear
// it, and a catch-up names the generation its root was at when the daemon decided to send it.
// Which architect heard it is not one of them: a decision's issue can leave for a tree of its own
// between the keeping and the telling, and a row composed before that can still tell the tree it
// left, so where the debt stands is read once the run has quiesced (held).
func (r *keptRun) tell(telling keptTelling) {
	for _, before := range r.told[telling.ref] {
		if before.tree == telling.tree {
			r.fatalf("%s was told twice to the architect of %s: first at step %d by %s, now by %s; a person's decision reaches its architect once",
				telling.ref, telling.tree, before.step, before.how, telling.how)
		}
	}
	switch {
	case telling.prompt && !telling.hears:
		r.fatalf("%s was told by %s while %s has no architect to hear it (parked, lingering, or waiting for its slot): a start task reaches an architect of the root's current run",
			telling.ref, telling.how, telling.tree)
	case !telling.prompt && telling.named != telling.root:
		r.fatalf("%s was told by %s, which names generation %d, while %s stood at generation %d when the daemon decided to send it: "+
			"every catch-up names the generation its root is at when it is sent",
			telling.ref, telling.how, telling.named, telling.tree, telling.root)
	}
	r.told[telling.ref] = append(r.told[telling.ref], telling)
	r.tracef("told ref=%s generation=%d tree=%s issue-tree=%s by=%s", telling.ref, telling.named, telling.tree,
		telling.issueTree, telling.how)
}

// refsIn is every tracked reference the text names. The references are fixed width, so no one is a
// substring of another.
func (r *keptRun) refsIn(text string) []string {
	var named []string
	for _, tracked := range r.tracked {
		if strings.Contains(text, tracked.ref) {
			named = append(named, tracked.ref)
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

// keptRowOfKey is the outbox row a published notice's dedupe key names (record.OutboxKey), and 0
// for a key no row minted. A re-held copy goes out under the key of the row it copies, which is the
// row whose text it carries.
func keptRowOfKey(key string) int64 {
	_, rest, found := strings.Cut(key, ":")
	if !found {
		return 0
	}
	row, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return row
}

// rows is the supervise and notice rows of every driven tree, those due now or every one left, in
// id order; the caller picks among them at random.
func (r *keptRun) rows(dueOnly bool) []record.OutboxRow {
	r.t.Helper()
	query := `select id, kind, issue, payload from outbox where issue = any($1) and kind = any($2)`
	args := []any{r.rootKeys(), []string{string(record.OutboxKindSupervise), string(record.OutboxKindNotice)}}
	if dueOnly {
		query += " and next_at <= $3"
		args = append(args, time.Now())
	}
	rows, err := r.pool.Query(r.ctx, query+" order by id", args...)
	if err != nil {
		r.fatalf("list the outbox rows: %v", err)
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
		r.fatalf("list the outbox rows: %v", err)
	}
	return listed
}

// keptCatchUps is the catch-ups among rows that name decisions their root keeps. They are the
// rows a publish is held open for: a publish that carries nothing is one no interleaving can lose
// or duplicate, and the point of the interleaving is what the drop, the hand-over and the
// re-admission do to what the publishing row claims.
func (r *keptRun) keptCatchUps(rows []record.OutboxRow) []record.OutboxRow {
	var found []record.OutboxRow
	for _, row := range rows {
		if row.Kind != record.OutboxKindNotice {
			continue
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			continue
		}
		if notice, ok := payload.(record.Notice); ok && notice.CatchUp != nil && keptCount(notice.CatchUp.Kept) > 0 {
			found = append(found, row)
		}
	}
	return found
}

// snapshotRows records the generation every outbox row names, so a prompt an architect receives
// later can be attributed to the start that queued it even once that row is gone. A row's
// generation never changes, and a row is never seen under two of them.
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

// deliverRowsTheRunDoesNotDrive takes off the outbox every row that is neither a supervise request
// nor a notice: the controller's notices, which go to the project's controller rather than to an
// architect, and the Dispatch and GitHub writes admission queues for a promoted root — its status
// write and its issue branch — which go where this run drives nobody. Each has tests of its own.
func (r *keptRun) deliverRowsTheRunDoesNotDrive() {
	if _, err := r.pool.Exec(r.ctx, "delete from outbox where kind <> $1 and kind <> $2",
		string(record.OutboxKindSupervise), string(record.OutboxKindNotice)); err != nil {
		r.fatalf("deliver the rows this run does not drive: %v", err)
	}
}

func (r *keptRun) putRoot() {
	r.t.Helper()
	r.putIssue(r.root.issue)
}

func (r *keptRun) putIssue(issue record.Issue) {
	r.t.Helper()
	if err := r.putIssueErr(issue); err != nil {
		r.fatalf("record %s: %v", issue.Key, err)
	}
}

func (r *keptRun) putIssueErr(issue record.Issue) error {
	return pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutIssue(r.ctx, tx, issue)
	})
}

// issueRecord is the record of key, nil for an issue the daemon does not record.
func (r *keptRun) issueRecord(key string) *record.Issue {
	var stored *record.Issue
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		stored, err = r.records.Issue(r.ctx, tx, key)
		return err
	}); err != nil {
		r.fatalf("read the record of %s: %v", key, err)
	}
	return stored
}

// treeOf is the tree key stands in now, which is the root whose architect is owed the decisions a
// person made on its asks.
func (r *keptRun) treeOf(key string) string {
	stored := r.issueRecord(key)
	if stored == nil {
		r.fatalf("%s has no record", key)
	}
	return stored.Tree
}

// treeRow is the driven tree an outbox row belongs to, nil for a row of none.
func (r *keptRun) treeRow(issue string) *keptTree {
	for _, tree := range r.roots {
		if tree.key == issue {
			return tree
		}
	}
	return nil
}

// treeTopic is the driven tree whose architect holds topic, nil for a topic of none.
func (r *keptRun) treeTopic(topic string) *keptTree {
	for _, tree := range r.roots {
		if tree.topic == topic {
			return tree
		}
	}
	return nil
}

func (r *keptRun) enqueue(issue string, payload record.OutboxPayload) {
	r.t.Helper()
	row, err := record.NewOutboxRow(issue, payload, time.Now())
	if err != nil {
		r.fatalf("build an outbox row: %v", err)
	}
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.Enqueue(r.ctx, tx, row)
	}); err != nil {
		r.fatalf("enqueue an outbox row: %v", err)
	}
}

// issuedRows is the newest id the outbox has issued, whether or not that row still stands: a row
// is deleted when its run finishes, so the newest row the outbox holds says nothing about which
// rows were composed before a moment. It is the sequence's own last value, read as the hand-over
// draws the line between the rows composed while the child was still in the root's tree and those
// composed after (handedRow, beforeTheHandOver).
func (r *keptRun) issuedRows() int64 {
	var id int64
	if err := r.pool.QueryRow(r.ctx, "select coalesce(pg_sequence_last_value('outbox_id_seq'), 0)").Scan(&id); err != nil {
		r.fatalf("read the newest outbox row the run has issued: %v", err)
	}
	return id
}

// lastRow is the newest outbox row's id, which names in the trace the row an operation wrote.
func (r *keptRun) lastRow() int64 {
	var id int64
	if err := r.pool.QueryRow(r.ctx, "select coalesce(max(id), 0) from outbox").Scan(&id); err != nil {
		r.fatalf("read the newest outbox row: %v", err)
	}
	return id
}

func (r *keptRun) storedGeneration(key string) uint64 {
	var generation uint64
	if err := r.pool.QueryRow(r.ctx, "select generation from issues where key = $1", key).Scan(&generation); err != nil {
		r.fatalf("read the generation of %s: %v", key, err)
	}
	return generation
}

// rootGeneration is the generation the root whose architect holds topic stands at, false for a
// topic no driven tree's architect holds. The publisher reads it as it takes a catch-up, on
// whichever lane is publishing, so it answers the error rather than ending the run from a
// goroutine of its own.
func (r *keptRun) rootGeneration(topic string) (uint64, bool, error) {
	tree := r.treeTopic(topic)
	if tree == nil {
		return 0, false, nil
	}
	var generation uint64
	if err := r.pool.QueryRow(r.ctx, "select generation from issues where key = $1", tree.key).Scan(&generation); err != nil {
		return 0, false, fmt.Errorf("read the generation of %s while its catch-up published: %w", tree.key, err)
	}
	return generation, true, nil
}

func (r *keptRun) waitsForSlot() bool {
	var waits bool
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		waits, err = record.WaitsForSlot(r.ctx, tx, r.records, r.root.issue)
		return err
	}); err != nil {
		r.fatalf("read whether the root waits for its slot: %v", err)
	}
	return waits
}

// waitMachines waits for every send each architect's machine started to have had its outcome
// handled, which is what makes a prompt and its acknowledgement part of the step that caused them.
func (r *keptRun) waitMachines() {
	for _, tree := range r.roots {
		if machine, found := r.sup.Machine(tree.token); found {
			machine.Wait()
		}
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
