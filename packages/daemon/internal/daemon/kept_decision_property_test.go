package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// the root lingers, and the issue the root leaves the tree with at the linger's end — handed
	// over with its run or without it, or orphaned by a person's todo — where it becomes a root
	// with an architect of its own.
	keptChild = "LEGION-2"
	// keptGrandchild is the child's own child, handed over beside its parent at the linger's end,
	// in whichever order the seed chooses: a tree of its own too, and what a person decided on it
	// is its architect's to be told, not its parent's.
	keptGrandchild = "LEGION-3"
	// keptQueued is a second root of the project, waiting in `todo` for an admission slot
	// (record.WaitsForSlot) behind an issue Dispatch has not finished, which is where a person's
	// decision on it is kept for the start its admission will queue. A person can make it a child
	// of a live tree instead, which admission adopts into that tree (adoptQueuedRoots): from then
	// on its decisions are owed to that tree's architect.
	keptQueued = "LEGION-4"
	// keptBlocker is the Dispatch issue the mirror shows a member of this run waiting on: it
	// holds the queued root out of the line and a handed-over child at phase.Admitted, and the
	// quiescence finishes it, as a person finishing that issue does (blockers.Unfinished).
	keptBlocker = "LEGION-9"
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
	// several rows at once, handovers every member handed into a tree of its own (of which
	// handoversWithoutRun carried no run), orphans every child a person's todo orphaned,
	// adoptions every queued root admission moved into a live tree, closes every linger this run
	// expired, streamed every restart whose agent then answered get_state in a turn, and foreign
	// every turn the agent started on its own on a claim holding a task nobody sent.
	raced               int
	together            int
	handovers           int
	handoversWithoutRun int
	orphans             int
	adoptions           int
	closes              int
	streamed            int
	foreign             int
	// handedInPublish is every hand-over that ran inside a catch-up's held-open publish — the
	// issue leaving the tree between that catch-up's compose and its publish — and
	// handedBeforeConfirm every one that ran between a held task's queue and the confirmation
	// that tells what it holds.
	handedInPublish     int
	handedBeforeConfirm int
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
	c.handoversWithoutRun += other.handoversWithoutRun
	c.orphans += other.orphans
	c.adoptions += other.adoptions
	c.closes += other.closes
	c.streamed += other.streamed
	c.foreign += other.foreign
	c.handedInPublish += other.handedInPublish
	c.handedBeforeConfirm += other.handedBeforeConfirm
	c.supersededTimeouts += other.supersededTimeouts
	for mode := range c.publishes {
		c.publishes[mode] += other.publishes[mode]
	}
}

func (c keptCounts) String() string {
	return fmt.Sprintf("%d launches refused, %d claim failures, %d starts relaunched a failed claim, "+
		"%d catch-ups finished for an architect that will not hold its role again (%d of them a failed claim); "+
		"%d publishes held open for an interleaving, %d steps ran rows at once, %d members handed over (%d without their run), "+
		"%d children orphaned by a person's todo, %d queued roots adopted, %d trees closed, "+
		"%d restarts met a streaming agent, %d foreign turns, %d receipt timeouts the re-hold found superseded, "+
		"%d hand-overs inside a catch-up's publish, %d hand-overs between a held task and its confirmation; "+
		"catch-up publishes answered: %d delivered, %d no-holder, %d refused, %d receipt-timeout, %d denied, %d delivery-failed",
		c.refusedLaunches, c.failedClaim, c.retriedFailed, c.endedCatchUps, c.endedByFailure,
		c.raced, c.together, c.handovers, c.handoversWithoutRun, c.orphans, c.adoptions, c.closes,
		c.streamed, c.foreign, c.supersededTimeouts, c.handedInPublish, c.handedBeforeConfirm,
		c.publishes[keptDelivers], c.publishes[keptNoHolder], c.publishes[keptRefuses],
		c.publishes[keptReceiptTimeout], c.publishes[keptDenied], c.publishes[keptDeliveryFailed])
}

// keptTree is one root the run drives and the architect that hears for it: the root every run
// opens with, the members it leaves the tree with (the child and the grandchild below it), which
// are roots of their own from then on, and the second root that waits for an admission slot. Each
// has its claim, its agent's connection and its role topic, and the decisions on the issues of
// its tree are owed to its architect.
type keptTree struct {
	key   string
	issue record.Issue
	token claim.Token
	conn  *fake.Conn
	topic string
	// prompts is how many of the connection's prompts observe has read.
	prompts int
	// handed is whether this issue has left the tree it started in for one of its own: a
	// hand-over at the linger's end, with its run or without it, or a person's todo that orphaned
	// it. It is no member of the old tree from then on, and nothing hands it over twice.
	handed bool
}

// keptDeparture is one time a tracked issue left a tree: the tree it left, and the newest outbox
// row the outbox had issued when it left. Every row at or below that id was composed while the
// issue still stood in that tree, so a task or catch-up of one can name a decision on it in text
// written then, and telling that tree's architect is the one extra telling the invariant accepts
// (held, leftBefore).
type keptDeparture struct {
	from string
	rows int64
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

	// root is the root the run opens with, child its one child, grandchild the child's own child,
	// queued the second root waiting for its slot, and roots every tree whose architect the run
	// drives: the root and the queued root from the start, and each member as it leaves for a
	// tree of its own.
	root       *keptTree
	child      *keptTree
	grandchild *keptTree
	queued     *keptTree
	roots      []*keptTree

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
	// owedTree is the tree each decision is owed to, by reference: the tree its issue stood in
	// when a person made it, and the tree its issue moves into whenever it moves while the
	// decision is still owed inside the tree it leaves — kept there, claimed or not, or named by
	// the unconfirmed task held on that tree's architect's claim (owedRefs). Only that tree's
	// architect pays it (pays); a decision told and forgotten before the move owes the tree it
	// moved into nothing, and nothing names it there again.
	owedTree map[string]string
	// departed is every tree each tracked issue has left, by issue key (keptDeparture).
	departed map[string][]keptDeparture
	// treeBefore is the tree each tracked issue stood in before the step now running, owedBefore
	// every decision still owed then, and rowsBefore the newest row the outbox had issued: the
	// three a move is read against (noteMoves).
	treeBefore map[string]string
	owedBefore map[string]bool
	rowsBefore int64
	// closeLog is what the engine logged, which the close's own lines are read from
	// (warnKeptDecisionsOfAClosedTree, closeTree).
	closeLog *keptLog
	// closed is the root generations whose linger this run has already expired: one close each.
	closed   map[uint64]bool
	publishd int
	counts   keptCounts
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
//   - once the run quiesces — every injected failure off, every row drained, what held a member
//     out of the line finished, each architect ready and its turns ended, and each architect whose
//     catch-up a refusal or a denial finished undelivered while its root heard relaunched once,
//     which is what ends such a refusal in life (relaunchAfterRefusal) — every decision kept has
//     been told once to the architect it was owed to, and nothing is left kept anywhere.
//
// Owed to whom follows the decision's issue: a decision is owed to the architect of the tree its
// issue stands in, and every time that issue moves into another tree while the decision is still
// owed inside the tree it leaves — kept there, claimed or not, or named by the unconfirmed task
// held on that tree's architect's claim (owedRefs) — the debt moves with it, and only the new
// tree's architect pays it (owedTree, pays). An issue moves five ways here, each the real
// machinery's: the linger's hand-over of a child with its run, its hand-over of a child that
// carries none (one held at phase.Admitted by an issue Dispatch has not finished), a person's todo
// that orphans a parked child of a tree nobody runs, the hand-over of the grandchild beside its
// parent in either order, and admission's adoption of a queued root into a live tree
// (adoptQueuedRoots), which makes that root a member and its decisions that tree's architect's.
// A decision told before its issue moved was told to the architect that owed the person an answer
// then, and that pays it: the move carries only what is still owed. A telling to an architect that
// was owed nothing is no telling of any debt, and is reported with the debt it left unpaid.
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
// The operations a step chooses among are a person's decision on the root, on its child, on the
// grandchild or on the queued root, the root's park (to backlog, or lingering after its close)
// with the architect's suspend, its hand back to todo with no slot, its re-admission with the
// start admission writes, the hand-over of its child with its run or without it, the hand-over of
// the grandchild, a person's todo that orphans the parked child of a tree nobody runs, a person's
// re-parenting of the queued root under a live tree, which admission adopts, the expiry of the
// root's linger, which closes the tree for good and hands over what it still had in the workflow
// (closeTree), one due supervise or notice row of a driven tree run in no particular order
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
// The close is the real one: the linger's expiry fact (intake.LingerExpired), whose
// warnKeptDecisionsOfAClosedTree names every decision the tree still owes and whose hand-overs
// the run then applies as Dispatch's labelled todos. Its log is read as far as the invariant
// needs: a close must name every decision of its tree still owed, in a line of its own
// (closeTree). What its tree_close rows then do — retiring each claim and running the tree's
// cleanup through its lifecycle — is left to the tests of that machinery, so those rows are taken
// off with the ones this run does not drive.
//
// The refused launch is what drives a claim through its launch budget
// (supervise.Limits.LaunchFailures) into failed, which the run tells the workflow as the daemon's
// terminal hook does (claimFailed), and from there through the start row's relaunch of a failed
// claim and the catch-up the executor finishes because nobody will hold the architect's role again
// (architectEnded). The denied forward finishes a catch-up undelivered as a refusal does; the
// failed forward is none of the answers the executor classifies, so its row retries on its backoff
// without finishing. Each run reports how often each of these fired (keptCounts).
//
// Four the machinery has are left out. The architect's process dying takes its task back and the
// relaunch sends the same delivery to a fresh agent, which no observation here can tell from a
// second telling, and whether that is one is a question for the specification rather than for this
// test. A newer decision on a reference already kept replaces its row (Store.AddParkedDecision),
// so that reference is told twice by design and "told once per reference" stops meaning anything;
// every decision here is made on a reference of its own. The outbox's own parkDecision, which
// keeps the decision a spec-decision notice could not deliver, reaches the same
// record.RelayParkedDecision this test calls directly, and adds only the notice routing its own
// tests cover. The notice the executor cannot route (errNoticeUnroutable) needs a record with no
// such issue, no tree or no recorded root, which the rest of the run reads as a broken record
// rather than as a state the root can be in.
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
	closeLog := &keptLog{}
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories},
		slog.New(slog.NewJSONHandler(closeLog, nil)))
	trees := outboxTreeStore(t, pool, keptRoot)
	run := &keptRun{
		t: t, ctx: ctx, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed))), steps: steps,
		pool: pool, records: records,
		engine: engine,
		// Admission runs over the project key the records carry, which is what its own reads of the
		// project's live issues match (record.Store.LiveIssues); the claims it opens are keyed by
		// the normalized token of it (claim.ProjectToken), the one the supervisor uses. Its cap
		// holds every tree this run can come to drive at once: the root with its own slot, the
		// child and the grandchild the linger hands over, and the second root, once nothing holds
		// it out of the line.
		admission:  admit.New(records, engine, 5, "LEGION", quietLogger()),
		trees:      trees,
		claims:     keptClaimStore{Store: trees},
		rt:         &keptRuntime{Runtime: fake.NewRuntime()},
		publisher:  &keptPublisher{genAt: map[string]uint64{}},
		stateDir:   t.TempDir(),
		at:         time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC),
		told:       map[string][]keptTelling{},
		owedTree:   map[string]string{},
		departed:   map[string][]keptDeparture{},
		treeBefore: map[string]string{},
		owedBefore: map[string]bool{},
		closeLog:   closeLog,
		closed:     map[uint64]bool{},
		rowGen:     map[int64]uint64{},
		refIssue:   map[string]string{},
		sent:       map[string]bool{},
		keys:       map[string]bool{},
		applied:    map[string]bool{},
	}
	run.publisher.atPublish = run.rootGeneration
	parent, childKey := keptRoot, keptChild
	run.root = &keptTree{key: keptRoot, token: mustClaimToken(t, keptRoot, claim.RoleArchitect), conn: fake.NewConn(),
		topic: architectTopic(t, keptRoot),
		issue: record.Issue{Key: keptRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: keptRoot, Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress",
			Rank: "A", LastDispatchSeq: 1},
	}
	// The child stands in the root's tree, in a phase of its own, which is what makes it work the
	// hand-over carries into a tree of its own (record.Issue.CarriesRun) rather than work that
	// starts again; the hand-over without its run puts it back to phase.Admitted first.
	run.child = &keptTree{key: keptChild, token: mustClaimToken(t, keptChild, claim.RoleArchitect), conn: fake.NewConn(),
		topic: architectTopic(t, keptChild),
		issue: record.Issue{Key: keptChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: keptRoot, Parent: &parent, Title: "Child", Phase: phase.Planning, Generation: 1,
			Status: "in_progress", Rank: "B", LastDispatchSeq: 1},
	}
	// The grandchild stands below the child, in a phase of its own: the linger hands it over
	// beside its parent, and what a person decided on it is owed to its own architect.
	run.grandchild = &keptTree{key: keptGrandchild, token: mustClaimToken(t, keptGrandchild, claim.RoleArchitect), conn: fake.NewConn(),
		topic: architectTopic(t, keptGrandchild),
		issue: record.Issue{Key: keptGrandchild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: keptRoot, Parent: &childKey, Title: "Grandchild", Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", Rank: "C", LastDispatchSeq: 1},
	}
	// The second root is handed to Legion and waits in `todo` behind an issue Dispatch has not
	// finished (keptBlocker), which is what keeps it in the line rather than promoted: a person's
	// decision on it is kept for the start its admission will queue (record.WaitsForSlot). It has
	// no parent until a person gives it one, so nothing adopts it before that step.
	run.queued = &keptTree{key: keptQueued, token: mustClaimToken(t, keptQueued, claim.RoleArchitect), conn: fake.NewConn(),
		topic: architectTopic(t, keptQueued),
		issue: record.Issue{Key: keptQueued, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: keptQueued, Title: "Queued", Phase: phase.Admitted, Generation: 1,
			Status: "todo", DispatchStatus: "todo", Rank: "D", HandedOver: true, LastDispatchSeq: 1},
	}
	run.roots = []*keptTree{run.root, run.queued}
	run.putIssue(run.root.issue)
	run.putIssue(run.child.issue)
	run.putIssue(run.grandchild.issue)
	run.putIssue(run.queued.issue)
	// The root runs, so it holds the slot its promotion gave it (takeSlot).
	if err := run.takeSlot(keptRoot); err != nil {
		run.fatalf("give %s the slot its run holds: %v", keptRoot, err)
	}
	run.mirror(keptBlocker, "in_progress", 1)
	run.mirror(keptQueued, "todo", 1, keptBlocker)
	run.restart()
	// The run opens as a fresh admission does: the architect's start, naming what the root keeps,
	// which is nothing yet.
	run.enqueueStart()
	return run
}

// keptLog is the engine's log as the run reads it: the JSON lines slog writes, under a lock,
// since a step that runs rows at once logs from several lanes.
type keptLog struct {
	mu    sync.Mutex
	lines []byte
}

func (l *keptLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, p...)
	return len(p), nil
}

// since is everything written to the log after mark, and mark is how much it holds now.
func (l *keptLog) since(mark int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.lines[mark:])
}

func (l *keptLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

// mirror writes what Dispatch says of key — its status and the keys it waits on — as the
// blockers.Mirror handler writes one from an event (record.DispatchIssueRow), under the project
// key admission judges the line by ("LEGION", the one the records carry). It is the only thing
// that holds a root out of the line here (blockers.WaitsOn) and a child at phase.Admitted
// (blockers.Holds). The engine of this run reads the mirror under its own project key
// ("legion"), as the daemon's own does, so what holds a member here is admission's reading
// alone: the linger's close hands over what its own reading finds unfinished, and the member a
// blocker holds is handed over by the step that writes that hand-over (handOverChildWithoutItsRun).
func (r *keptRun) mirror(key, status string, seq int64, waits ...string) {
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutDispatchIssue(r.ctx, tx, record.DispatchIssueRow{Key: key, Project: "LEGION",
			Status: status, Blockers: waits, Seq: seq})
	}); err != nil {
		r.fatalf("mirror what Dispatch says of %s: %v", key, err)
	}
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

// drive runs the seed's operations, each chosen among those the model state enables. Every step
// is read for the moves it made: an issue that left one tree for another takes the decisions
// still owed on it with it (noteMoves).
func (r *keptRun) drive() {
	for r.step = 1; r.step <= r.steps; r.step++ {
		r.snapshotRows()
		r.noteBefore()
		enabled := r.enabled()
		enabled[r.rng.IntN(len(enabled))]()
		r.noteMoves()
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
	if !r.grandchild.handed && r.root.issue.Lingers() {
		ops = append(ops, r.decideGrandchild)
	}
	if !r.queued.handed && r.waitsForItsSlot(r.queued) {
		ops = append(ops, r.decideQueued)
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
	if r.handOverEnabled(r.child) {
		ops = append(ops, r.handOverChild, r.handOverChildWithoutItsRun)
	}
	if r.handOverEnabled(r.grandchild) {
		ops = append(ops, r.handOverGrandchild)
	}
	if r.orphanEnabled() {
		ops = append(ops, r.orphanChild)
	}
	if !r.queued.handed && r.adoptionTarget() != nil {
		ops = append(ops, r.adoptQueued)
	}
	if r.closeEnabled() {
		ops = append(ops, r.closeTree)
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
	overs := r.handOvers()
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
	if len(overs) > 0 && r.awaitingConfirmation(r.root) {
		ops = append(ops, func() { r.handOverInsideAHeldTask(overs[r.rng.IntN(len(overs))]) })
	}
	return ops
}

// trackedIssues is every issue this run records decisions on, which is where a move can carry a
// debt from one tree to another.
func (r *keptRun) trackedIssues() []string {
	return []string{keptRoot, keptChild, keptGrandchild, keptQueued}
}

// noteBefore reads what a move is judged against: the tree each tracked issue stands in, every
// decision still owed inside the tree it stands in, and the newest row the outbox has issued, all
// before the step runs. A row composed after this moment was written for whichever tree the step
// left the issue in.
func (r *keptRun) noteBefore() {
	r.treeBefore = r.treesOfTrackedIssues()
	r.owedBefore = r.owedRefs()
	r.rowsBefore = r.issuedRows()
}

// noteMoves records every tracked issue the step just ran moved into another tree: the tree it
// left (departed, with the rows composed before the move), and, for each decision on it still
// owed inside that tree, the tree the debt moved into (owedTree). A decision told and forgotten
// before the move owes the new tree nothing, and nothing names it there again.
func (r *keptRun) noteMoves() {
	after := r.treesOfTrackedIssues()
	for _, issue := range r.trackedIssues() {
		was, now := r.treeBefore[issue], after[issue]
		if was == "" || now == "" || was == now {
			continue
		}
		r.departed[issue] = append(r.departed[issue], keptDeparture{from: was, rows: r.rowsBefore})
		var moved []string
		for _, tracked := range r.tracked {
			if tracked.issue == issue && r.owedBefore[tracked.ref] {
				r.owedTree[tracked.ref] = now
				moved = append(moved, tracked.ref)
			}
		}
		r.tracef("move issue=%s from=%s to=%s rows-issued-before=%d owes-its-new-tree=%s", issue, was, now,
			r.rowsBefore, keptElsewhere(moved))
	}
}

// treesOfTrackedIssues is the tree each tracked issue stands in now, by issue key, read in one
// query: an issue with no record stands in none.
func (r *keptRun) treesOfTrackedIssues() map[string]string {
	rows, err := r.pool.Query(r.ctx, "select key, tree from issues where key = any($1)", r.trackedIssues())
	if err != nil {
		r.fatalf("read the trees the issues stand in: %v", err)
	}
	defer rows.Close()
	trees := map[string]string{}
	for rows.Next() {
		var key, tree string
		if err := rows.Scan(&key, &tree); err != nil {
			r.fatalf("scan the tree an issue stands in: %v", err)
		}
		trees[key] = tree
	}
	if err := rows.Err(); err != nil {
		r.fatalf("read the trees the issues stand in: %v", err)
	}
	return trees
}

// owedRefs is every decision still owed inside the tree its issue stands in: one kept there,
// claimed by a start or a catch-up or not, and one the unconfirmed task held on an architect's
// claim is the only carrier of, its row having gone when the claim queued the task
// (pending_task_deliveries.kept_decisions). A decision in neither place was told and forgotten,
// and nothing names it again.
func (r *keptRun) owedRefs() map[string]bool {
	rows, err := r.pool.Query(r.ctx, `select ref from parked_decisions
		union
		select named.ref from pending_task_deliveries pending,
			lateral jsonb_to_recordset(coalesce(pending.kept_decisions->'decisions', '[]'::jsonb)) as named(ref text)
		where pending.confirmed_at is null`)
	if err != nil {
		r.fatalf("read the decisions still owed: %v", err)
	}
	defer rows.Close()
	owed := map[string]bool{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			r.fatalf("scan a decision still owed: %v", err)
		}
		owed[ref] = true
	}
	if err := rows.Err(); err != nil {
		r.fatalf("read the decisions still owed: %v", err)
	}
	return owed
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
func (r *keptRun) decide() { r.decideOn(keptRoot) }

// decideChild is a person deciding on an ask of the root's child while the root lingers: the
// workflow puts the decision to the same rule under the tree's root as the record holds it
// (workflow's relayParkedDecision reads the root of the ask's tree), so it is kept under the root,
// and it is owed to the architect of whatever tree the child stands in when it is told.
func (r *keptRun) decideChild() { r.decideOn(keptChild) }

// decideGrandchild is the same on an ask of the grandchild, which stands below the child in the
// lingering tree: kept under that tree's root, and owed to the grandchild's own architect once the
// linger hands it over.
func (r *keptRun) decideGrandchild() { r.decideOn(keptGrandchild) }

// decideQueued is a person deciding on an ask of the second root while it waits for its slot: the
// rule keeps it for the start its admission will queue and tells the controller nothing, since
// that tree already runs again (record.RelayKept).
func (r *keptRun) decideQueued() { r.decideOn(keptQueued) }

// decideOn keeps one person's decision made on an ask of issue, under the root of that issue's
// tree as the record holds it now, which is the record RelayParkedDecision judges (workflow's
// relayParkedDecision reads the same root). The debt starts in that tree and moves with the issue
// (noteMoves).
func (r *keptRun) decideOn(issue string) {
	r.decided++
	ref := fmt.Sprintf("dispatch://%s/ask/ask-%04d", issue, r.decided)
	actor := []string{"user:alice", "user:bob"}[r.rng.IntN(2)]
	kind := []record.DecisionKind{record.DecisionAskAnswered, record.DecisionAskResolved}[r.rng.IntN(2)]
	r.at = r.at.Add(time.Minute)
	outcome, tree := record.RelayNone, ""
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		stored, err := r.records.Issue(r.ctx, tx, issue)
		if err != nil || stored == nil {
			return err
		}
		root := stored
		if stored.Tree != issue {
			if root, err = r.records.Issue(r.ctx, tx, stored.Tree); err != nil || root == nil {
				return err
			}
		}
		tree = root.Key
		outcome, err = record.RelayParkedDecision(r.ctx, tx, r.records, issue, *root, true,
			record.ParkedDecision{Ref: ref, Decision: kind, Actor: actor, At: r.at}, 0, "")
		return err
	}); err != nil {
		r.fatalf("keep the decision %s: %v", ref, err)
	}
	if outcome == record.RelayKept || outcome == record.RelaySent {
		r.tracked = append(r.tracked, keptRef{ref: ref, issue: issue})
		r.refIssue[ref] = issue
		r.owedTree[ref] = tree
	}
	r.tracef("decide ref=%s issue=%s tree=%s by=%s kind=%s outcome=%s kept=%t", ref, issue, tree, actor, kind,
		keptOutcome(outcome), outcome == record.RelayKept || outcome == record.RelaySent)
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
// close — releases the slot its run held, as admission's own pass does for a root that left the
// workflow (releaseInactiveSlots), and writes the architect's suspend as the workflow writes one
// (workflow's suspend).
func (r *keptRun) park() {
	lingers := r.rng.IntN(2) == 0
	if lingers {
		until := time.Now().Add(time.Hour)
		r.root.issue.Phase, r.root.issue.Status, r.root.issue.LingerUntil = phase.Done, "done", &until
	} else {
		r.root.issue.Phase, r.root.issue.Status, r.root.issue.LingerUntil = phase.Done, "backlog", nil
	}
	r.putRoot()
	r.releaseSlot(keptRoot)
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

// readmit is the root's next generation: admission's record, the slot its promotion gives it and
// the architect start it enqueues, naming what the root keeps and claiming those rows for that
// start in one transaction, exactly as admit.Admission writes them.
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
	if err := r.takeSlot(keptRoot); err != nil {
		return err
	}
	return r.enqueueStartRow()
}

// takeSlot gives key the admission slot its promotion gives a root it admits: a root that runs
// holds one, which is what says its tree is live (record.TreeLive) and keeps admission's own pass
// from taking it out of the workflow as a root no slot can reach (releaseUnslotted).
func (r *keptRun) takeSlot(key string) error {
	return pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		slots, err := r.records.Slots(r.ctx, tx)
		if err != nil {
			return err
		}
		return r.records.PutSlot(r.ctx, tx, record.Slot{Issue: key, Index: len(slots), AdmittedAt: time.Now().UTC()})
	})
}

// releaseSlot gives back the slot of a root that left the workflow, as admission's own pass does
// (releaseInactiveSlots).
func (r *keptRun) releaseSlot(key string) {
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.ReleaseSlot(r.ctx, tx, key)
	}); err != nil {
		r.fatalf("release the slot of %s: %v", key, err)
	}
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

// handOverEnabled is whether the root's linger has this member to hand over: it still stands in
// the root's tree, in the workflow, and the root lingers after its close, which is where the
// linger's expiry hands a member that is still working to Legion (workflow's settleUnfinished).
func (r *keptRun) handOverEnabled(member *keptTree) bool {
	return !member.handed && r.root.issue.Lingers() && !record.OutOfWorkflow(member.issue.Status)
}

// handOverChild is the linger's end for the child, which carries its run into the tree of its own
// (record.Issue.CarriesRun: it stands in a phase).
func (r *keptRun) handOverChild() { r.handOverMember(r.child, keptRoot, true) }

// handOverChildWithoutItsRun is the linger's end for a child that carries no run: one still at
// phase.Admitted that an issue Dispatch has not finished holds there (blockers.Holds), which is
// the one admitted member a close hands to Legion (workflow's unfinishedHandOvers). Its new root
// starts nothing until that issue closes, so it waits in the line, and the decisions a person
// made on it are owed to the architect that root will have.
func (r *keptRun) handOverChildWithoutItsRun() {
	r.child.issue.Phase = phase.Admitted
	r.putIssue(r.child.issue)
	r.mirror(keptChild, "in_progress", r.child.issue.LastDispatchSeq, keptBlocker)
	r.handOverMember(r.child, keptRoot, false)
}

// handOverGrandchild is the linger's end for the grandchild below the child, handed over beside
// its parent: whichever of the two the seed re-admits first, what a person decided on the
// grandchild is owed to the grandchild's own architect.
func (r *keptRun) handOverGrandchild() { r.handOverMember(r.grandchild, keptChild, true) }

// handOverMember is the linger's end for one member still in the workflow: the durable fence and
// the Dispatch status write the workflow queues (its handOver), and then the labelled todo
// Dispatch shows for it, which admission re-admits as a root of its own — carrying its run
// (readmit's orphan branch) or not — and promotes when nothing holds it in the line. From here
// that member is a root with an architect of its own, and a decision a person made on its asks
// that the hand-over finds still owed inside the old tree is owed to that architect instead
// (noteMoves).
//
// The status write itself goes to Dispatch, which this run does not drive, so the row is taken off
// with the others the run leaves to their own tests (deliverRowsTheRunDoesNotDrive) and the event
// it would make is applied here, which is the path a person's label takes.
func (r *keptRun) handOverMember(member *keptTree, parent string, withRun bool) {
	if err := r.handOverMemberErr(member, parent, withRun); err != nil {
		r.fatalf("hand %s over: %v", member.key, err)
	}
}

// handOverMemberErr is handOverMember without the test's own end: the steps that run a hand-over
// inside another operation's window run it on a goroutine of their own, or with a confirmation
// waiting behind it, where a failure is the step's to report (raceRow, handOverInsideAHeldTask).
func (r *keptRun) handOverMemberErr(member *keptTree, parent string, withRun bool) error {
	now := time.Now()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		if err := r.records.PutHandover(r.ctx, tx, record.Handover{Issue: member.key, Tree: r.root.issue.Tree,
			Generation: r.root.issue.Generation}); err != nil {
			return err
		}
		row, err := record.NewOutboxRow(member.key, record.StatusWrite{Status: "todo", ObservedStatus: member.issue.Status,
			HandOver: true, HandOverTree: r.root.issue.Tree, HandOverLinger: r.root.issue.Generation}, now)
		if err != nil {
			return err
		}
		return r.records.Enqueue(r.ctx, tx, row)
	}); err != nil {
		return fmt.Errorf("write the hand-over of %s: %w", member.key, err)
	}
	if err := r.applyHandOverEventErr(member, parent); err != nil {
		return err
	}
	r.counts.handovers++
	if !withRun {
		r.counts.handoversWithoutRun++
	}
	r.tracef("hand-over member=%s with-run=%t tree=%s generation=%d status=%s phase=%s last-row=%d", member.key,
		withRun, member.issue.Tree, member.issue.Generation, member.issue.Status, member.issue.Phase, r.lastRow())
	return nil
}

// applyHandOverEvent applies the labelled todo Dispatch shows for a member the linger handed over,
// as the daemon's own status write makes it, and records the root of its own it becomes as a tree
// this run drives.
func (r *keptRun) applyHandOverEvent(member *keptTree, parent string) {
	if err := r.applyHandOverEventErr(member, parent); err != nil {
		r.fatalf("%v", err)
	}
}

func (r *keptRun) applyHandOverEventErr(member *keptTree, parent string) error {
	seq := member.issue.LastDispatchSeq + 1
	blocked := r.mirroredBlockers(member.key)
	if _, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", fmt.Sprintf("hand-over:%s:%d", member.key, seq),
		intake.DispatchIssue{Key: member.key, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: "todo",
			Title: member.issue.Title, Parent: parent, Rank: member.issue.Rank, Blockers: blocked, HandedOver: true,
			DaemonActor: true},
		r.engine, r.admission); err != nil {
		return fmt.Errorf("apply the hand-over of %s: %w", member.key, err)
	}
	carried := r.issueRecord(member.key)
	if carried == nil || carried.Tree != member.key {
		return fmt.Errorf("the handed-over member %s = %+v, want a tree of its own", member.key, carried)
	}
	member.issue, member.handed = *carried, true
	r.roots = append(r.roots, member)
	return nil
}

// keptHandOver is one way a tracked issue can leave its tree for one of its own now, named before
// a step picks among them so that one seed always picks the same one, and runnable where a failure
// must be reported rather than ended on.
type keptHandOver struct {
	what string
	run  func() error
}

// handOvers is every hand-over the model state allows now: the child with its run, and the
// grandchild beside it. They are what the two interleaving steps need — an issue that leaves its
// tree inside another operation's window — so each is offered as an operation of its own rather
// than only as a step the generator picks on its own.
func (r *keptRun) handOvers() []keptHandOver {
	var ops []keptHandOver
	if r.handOverEnabled(r.child) {
		ops = append(ops, keptHandOver{what: fmt.Sprintf("the hand-over of %s with its run", keptChild),
			run: func() error { return r.handOverMemberErr(r.child, keptRoot, true) }})
	}
	if r.handOverEnabled(r.grandchild) {
		ops = append(ops, keptHandOver{what: fmt.Sprintf("the hand-over of %s beside its parent", keptGrandchild),
			run: func() error { return r.handOverMemberErr(r.grandchild, keptChild, true) }})
	}
	if r.orphanEnabled() {
		ops = append(ops, keptHandOver{what: fmt.Sprintf("the person's todo that orphans %s", keptChild),
			run: r.orphanChildErr})
	}
	return ops
}

// orphanEnabled is whether a person can orphan the child now: it still stands in the root's tree
// and nobody runs that tree — it lingers after its close, or a person parked it — which is what
// makes a labelled todo on a member of it an orphan's admission rather than a re-entry
// (admit.Admission.orphan).
func (r *keptRun) orphanEnabled() bool {
	return !r.child.handed && (r.root.issue.Lingers() || record.OutOfWorkflow(r.root.issue.Status))
}

// orphanChild is a person setting a parked child of a tree nobody runs back to todo with the
// label: the child is at done, so it carries no run, and admission admits it as a root of its own
// (readmit without carry). What a person decided on it while that tree was nobody's to hear is
// owed to the architect of the root it becomes.
func (r *keptRun) orphanChild() {
	if err := r.orphanChildErr(); err != nil {
		r.fatalf("%v", err)
	}
}

// orphanChildErr is orphanChild without the test's own end, for the steps that run it inside
// another operation's window (raceRow, handOverInsideAHeldTask).
func (r *keptRun) orphanChildErr() error {
	r.child.issue.Phase, r.child.issue.Status = phase.Done, "backlog"
	if err := r.putIssueErr(r.child.issue); err != nil {
		return fmt.Errorf("record the parked child %s: %w", keptChild, err)
	}
	seq := r.child.issue.LastDispatchSeq + 1
	if _, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", fmt.Sprintf("orphan:%s:%d", keptChild, seq),
		intake.DispatchIssue{Key: keptChild, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: "todo",
			Title: r.child.issue.Title, Parent: keptRoot, Rank: r.child.issue.Rank, HandedOver: true},
		r.engine, r.admission); err != nil {
		return fmt.Errorf("apply the person's todo that orphans %s: %w", keptChild, err)
	}
	orphan := r.issueRecord(keptChild)
	if orphan == nil || orphan.Tree != keptChild {
		return fmt.Errorf("the orphaned child = %+v, want a tree of its own", orphan)
	}
	r.child.issue, r.child.handed = *orphan, true
	r.roots = append(r.roots, r.child)
	r.counts.orphans++
	r.tracef("orphan child=%s tree=%s generation=%d phase=%s last-row=%d", keptChild, orphan.Tree, orphan.Generation,
		orphan.Phase, r.lastRow())
	return nil
}

// adoptionTarget is the live tree a person can make the queued root a child of now, nil for none:
// a driven tree other than the queued root itself whose root's tree is live (record.TreeLive),
// which is what admission's sweep of its queue requires of a target (adoptQueuedRoots).
func (r *keptRun) adoptionTarget() *keptTree {
	for _, tree := range r.roots {
		if tree.key == keptQueued {
			continue
		}
		stored := r.issueRecord(tree.key)
		if stored == nil {
			continue
		}
		live := false
		if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
			var err error
			live, err = record.TreeLive(r.ctx, r.records, tx, *stored)
			return err
		}); err != nil {
			r.fatalf("read whether the tree of %s is live: %v", tree.key, err)
		}
		if live {
			return tree
		}
	}
	return nil
}

// adoptQueued is a person making the second root a child of a live tree: admission's sweep of its
// queue moves it into that tree with every record of its own (adoptQueuedRoots, workflow's
// Engine.Adopt), where it takes no slot and no architect of its own ever again. From then on the
// decisions a person made on it while it waited for its slot are owed to that tree's architect.
func (r *keptRun) adoptQueued() {
	target := r.adoptionTarget()
	if target == nil {
		r.fatalf("no live tree for %s to join", keptQueued)
	}
	seq := r.queued.issue.LastDispatchSeq + 1
	if _, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", fmt.Sprintf("adopt:%s:%d", keptQueued, seq),
		intake.DispatchIssue{Key: keptQueued, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated", Status: "todo",
			Title: r.queued.issue.Title, Parent: target.key, Rank: r.queued.issue.Rank, Blockers: []string{keptBlocker},
			HandedOver: true},
		r.engine, r.admission); err != nil {
		r.fatalf("apply the person's re-parenting of %s under %s: %v", keptQueued, target.key, err)
	}
	joined := r.issueRecord(keptQueued)
	if joined == nil {
		r.fatalf("%s has no record after its re-parenting", keptQueued)
	}
	r.queued.issue = *joined
	if joined.Tree != keptQueued {
		r.queued.handed = true
		r.counts.adoptions++
	}
	r.tracef("adopt queued=%s under=%s tree=%s generation=%d phase=%s last-row=%d", keptQueued, target.key,
		joined.Tree, joined.Generation, joined.Phase, r.lastRow())
}

// closeEnabled is whether the root's linger can expire now: it lingers, and this generation's
// close has not run. One close ends the tree for good.
func (r *keptRun) closeEnabled() bool {
	return r.root.issue.Lingers() && !r.closed[r.root.issue.Generation]
}

// closeTree is the linger's expiry, the fact the outbox's linger row applies: the tree closes for
// good, every decision it still owes is named in a line of its own
// (warnKeptDecisionsOfAClosedTree), and each member it left in the workflow is handed to Legion,
// whose status write this run applies as Dispatch's labelled todo. The close must name every
// decision of its tree still owed; one it passes over in silence is one it leaves behind with
// nobody told (owedOfTheClosingTree).
//
// What the close's tree_close rows then do — retiring each claim and running the tree's cleanup
// through its lifecycle — is the tree lifecycle's own machinery, with tests of its own, so those
// rows are taken off with the ones this run does not drive.
func (r *keptRun) closeTree() {
	generation := r.root.issue.Generation
	r.closed[generation] = true
	owed := r.owedOfTheClosingTree()
	mark := r.closeLog.mark()
	if _, err := intake.ApplyFact(r.ctx, r.pool, "linger", fmt.Sprintf("linger-close:%s:%d", keptRoot, generation),
		intake.LingerExpired{Issue: keptRoot, Generation: generation}, r.engine, r.admission); err != nil {
		r.fatalf("expire the linger of %s at generation %d: %v", keptRoot, generation, err)
	}
	logged := r.closeLog.since(mark)
	r.counts.closes++
	r.tracef("close tree=%s generation=%d owed=%s", keptRoot, generation, keptElsewhere(owed))
	for _, ref := range owed {
		if !strings.Contains(logged, ref) {
			r.fatalf("the close of %s said nothing of %s, which a person decided on an issue of that tree and nobody "+
				"in it has been told: a tree closes for good, so its close names every decision it still owes, as the one it "+
				"hands to a child's own tree or as one it leaves standing\n\nthe close logged:\n%s",
				keptRoot, ref, logged)
		}
	}
	// Each member the close hands to Legion is re-admitted by the labelled todo Dispatch then
	// shows for it, which is what carries it into a tree of its own.
	for _, member := range []*keptTree{r.child, r.grandchild, r.queued} {
		if member.handed || !r.handedOverByTheClose(member.key) {
			continue
		}
		parent := keptRoot
		if member.key == keptGrandchild {
			parent = keptChild
		}
		r.applyHandOverEvent(member, parent)
		r.counts.handovers++
		r.tracef("the close handed %s to Legion: tree=%s generation=%d phase=%s", member.key, member.issue.Tree,
			member.issue.Generation, member.issue.Phase)
	}
}

// owedOfTheClosingTree is every tracked decision still owed whose issue stands in the closing
// tree, in reference order: what the close must name, line by line.
func (r *keptRun) owedOfTheClosingTree() []string {
	owed := r.owedRefs()
	trees := r.treesOfTrackedIssues()
	var refs []string
	for _, tracked := range r.tracked {
		if owed[tracked.ref] && trees[tracked.issue] == keptRoot {
			refs = append(refs, tracked.ref)
		}
	}
	return refs
}

// handedOverByTheClose is whether the close queued this member a hand-over status write, which is
// what the linger's end does with a member it leaves in the workflow (workflow's handOver).
func (r *keptRun) handedOverByTheClose(key string) bool {
	var queued bool
	if err := r.pool.QueryRow(r.ctx, `select exists (select 1 from outbox where issue = $1 and kind = $2
		and (payload->>'handOver')::boolean)`, key, string(record.OutboxKindDispatchStatus)).Scan(&queued); err != nil {
		r.fatalf("read whether the close handed %s to Legion: %v", key, err)
	}
	return queued
}

// mirroredBlockers is what the mirror says key waits on, which an event of that issue carries
// again (intake.DispatchIssue.Blockers): the hand-over's own event must not say the issue waits
// on nothing, which would release a member its blocker still holds.
func (r *keptRun) mirroredBlockers(key string) []string {
	var blocked []string
	if err := r.pool.QueryRow(r.ctx, "select blockers from dispatch_issues where key = $1", key).Scan(&blocked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		r.fatalf("read what the mirror says %s waits on: %v", key, err)
	}
	return blocked
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
	over   keptHandOver
}

// raceOps is every operation that can run inside a catch-up's publish now: the ready fact of a
// launch whose agent is up and whose ready the daemon has not told the workflow yet, which writes
// that launch its catch-up; one other due start row of a driven tree — a stale start, whose
// settlement hands over, or one that finds the architect running and applies the ready fact
// itself; the root's re-admission, which is the other thing that supersedes a catch-up
// (catchUpSuperseded reads the root's generation against the catch-up's); and the hand-over of a
// member into a tree of its own, which is the one that moves the debt itself: the catch-up was
// composed while the issue still stood in this tree and publishes once it no longer does, so its
// publish must tell the old architect and leave the row for the tree the issue moved to
// (record.Store.ForgetKeptDecisions). The row the step is publishing is left out, and so is every
// row that touches no kept decision.
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
	for _, over := range r.handOvers() {
		ops = append(ops, keptRaceOp{kind: "handover", over: over, what: over.what})
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
	case "handover":
		over := op.over
		race.run = func() error {
			if err := over.run(); err != nil {
				return err
			}
			r.counts.handedInPublish++
			race.outcome = "the issue left the tree the publishing catch-up was composed for"
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

// awaitingConfirmation is whether the root's architect holds a task that names kept decisions and
// has not been confirmed — queued and held while the root does not hear, or sent and waiting for
// its turn. Its delivery holds those rows and its confirmation is what tells them, so this is the
// window between a held task's queue and its confirmation.
func (r *keptRun) awaitingConfirmation(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	pending := machine.Claim().Pending
	return pending != nil && pending.Generation != 0 && pending.ConfirmedAt.IsZero()
}

// namesKeptDecisions is whether the task the tree's architect holds names a decision of its own,
// which is what makes the window one a decision can be lost in: the counter reports only those.
func (r *keptRun) namesKeptDecisions(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	pending := machine.Claim().Pending
	return pending != nil && len(pending.Kept) > 0
}

// handOverInsideAHeldTask is an issue leaving its tree inside that window, and then the whole of
// what closes it: the hand-over runs, the root is re-admitted when nothing of it runs, so its
// architect hears again, every start row of the root then due is run — which is where the held
// task is narrowed of what has left this tree and sent (supervise's Deps.RootTask) — and the turn
// that confirms it starts at once. The confirmation's own transaction is the first thing to read
// the record the hand-over left, and what it tells is held to the same rule everything else is: a
// row whose issue has left the claim's tree is not told but left kept, for the tree it moved into
// to name (the claim store's tellKeptDecisions). Each piece is an operation the model drives on
// its own; this step runs them back to back so the window is reached rather than waited for.
func (r *keptRun) handOverInsideAHeldTask(over keptHandOver) {
	if err := over.run(); err != nil {
		r.fatalf("%s, inside the window between the root's held task and its confirmation: %v", over.what, err)
	}
	if r.namesKeptDecisions(r.root) {
		r.counts.handedBeforeConfirm++
	}
	r.tracef("hand-over inside a held task: %s; the root runs again and confirms its task now", over.what)
	if r.parkedOrWaiting() {
		r.readmit()
	}
	for _, row := range r.rows(true) {
		if row.Issue != keptRoot || row.Kind != record.OutboxKindSupervise {
			continue
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			continue
		}
		if request, ok := payload.(record.SuperviseRequest); !ok || request.Op != "start" {
			continue
		}
		if runErr := r.runOne(&row, payload); runErr == nil {
			if err := r.finishRow(row); err != nil {
				r.fatalf("finish the start row %d that sends the held task: %v", row.ID, err)
			}
		}
	}
	if r.turnEnabled(r.root) {
		r.turn(r.root)
	}
}

// heldTask is whether the architect's claim holds a task of a run that nobody has sent and nobody
// has acknowledged: a root architect's start task held back because its root has no architect to
// hear it (supervise's sendPending, Deps.RootHears). The rows it names are held by its delivery,
// and its own confirmation is what tells them.
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
// Two things more are driven. What holds a root out of the admission line is finished, as the
// person who closes that Dispatch issue finishes it, and a controller tick is applied so that
// admission promotes every root left waiting (unblockAndPromote): a root nobody promotes has no
// architect, and a decision owed to one is owed to an architect that never runs. And, only where
// the run met it, a catch-up a holder's refusal or the listener's own grant finished undelivered
// while its root heard (noteRefusal) leaves what it named to the row's deletion, and nothing
// names it again while that refusal stands — a fresh catch-up would be refused too. What ends a
// refusal in real life is the architect's next launch, so once nothing else is left to drive,
// each such architect is relaunched once (relaunchAfterRefusal) and that launch's ready names
// every unclaimed decision. Nothing else is relaxed: each is an operation of the machinery,
// driven as every other step's is.
func (r *keptRun) quiesce() {
	r.records.fails.Store(false)
	r.rt.setRefuses(false)
	r.publisher.setMode(keptDelivers)
	r.tracef("quiesce: every failure off, launches open, publisher delivers")
	r.noteBefore()
	if r.parkedOrWaiting() {
		r.readmit()
	}
	r.unblockAndPromote()
	r.noteMoves()
	for range keptQuiesceBound {
		r.step++
		r.snapshotRows()
		r.noteBefore()
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
		r.noteMoves()
		r.observe()
		r.deliverRowsTheRunDoesNotDrive()
	}
	r.fatalf("the rows and deliveries of %s did not drain in %d quiescence steps", strings.Join(r.rootKeys(), ", "), keptQuiesceBound)
}

// unblockAndPromote finishes the Dispatch issue this run's members wait on and applies a
// controller tick, which is the pass admission makes of its own accord: every root the line held
// out — the second root, and a child handed over without its run — takes a slot and is started,
// so each architect a decision is owed to is one that runs. A tick is one fact of the machinery,
// applied as any other.
func (r *keptRun) unblockAndPromote() {
	r.mirror(keptBlocker, "done", 2)
	if _, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", fmt.Sprintf("quiesce-tick:%s", keptRoot),
		intake.ControllerTick{}, r.engine, r.admission); err != nil {
		r.fatalf("promote the roots the line held out: %v", err)
	}
	for _, tree := range r.roots {
		if stored := r.issueRecord(tree.key); stored != nil {
			tree.issue = *stored
		}
	}
	r.tracef("quiesce: %s is finished and the line promoted; last-row=%d", keptBlocker, r.lastRow())
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

// held is the run's verdict once it has quiesced: every decision kept was told exactly once to
// the architect it was owed to, and nothing is left kept anywhere.
//
// Owed to which architect follows the decision's issue. A person deciding on an issue of a tree
// is owed an answer by that tree's architect, so a telling while the issue still stood there pays
// the debt, whatever tree the issue stands in later. An issue that moves into a tree of its own —
// a hand-over at the linger's end, with its run or without it, a person's todo that orphans it,
// or admission's adoption of a queued root into a live tree — takes with it every decision still
// owed inside the tree it leaves: what that tree still keeps of it, and what the unconfirmed task
// held on its architect's claim names of it (noteMoves, owedRefs). From then on only the new
// tree's architect pays it (owedTree, pays); what the move left behind was paid where it stood
// and is named to nobody again.
//
// One telling more is accepted, and only one: a decision carried to the architect of a tree its
// issue has left, by a row composed before it left (leftBefore). Such a row's task or catch-up
// names the reference in text written while the issue was still in that tree, and the row can be
// prompted or published long after — a start whose launch kept failing, a task held on the claim
// until its root heard again. For a decision a move carried on, telling the old architect pays
// nothing of what the new architect is owed, whether it reached that architect before the move or
// after it, and the decision is owed to the new tree all the same, so its architect is told it
// too: a person's decision is never lost, and told twice is the lesser fault. Any other telling
// to another tree's architect, and any second telling to one architect, is a fault.
func (r *keptRun) held() {
	for _, tracked := range r.tracked {
		owedTo, stands := r.owedTree[tracked.ref], r.treeOf(tracked.issue)
		var owed int
		var elsewhere, overheard []string
		for _, telling := range r.told[tracked.ref] {
			if telling.tree == owedTo {
				owed++
				continue
			}
			line := fmt.Sprintf("at step %d by %s, the architect of %s, while %s stood in the tree of %s",
				telling.step, telling.how, telling.tree, tracked.issue, telling.issueTree)
			if r.leftBefore(tracked.issue, telling) {
				overheard = append(overheard, line)
				continue
			}
			elsewhere = append(elsewhere, line)
		}
		if owed == 1 && len(elsewhere) == 0 && len(overheard) <= 1 {
			continue
		}
		r.fatalf("%s, an ask of %s, which stands in the tree of %s and owes its answer to the architect of %s, was told %d times to that "+
			"architect, %d times to another tree's architect (%s) and %d times to the architect of a tree it left by a row composed before it "+
			"left (%s), want once, never, and at most once: a person's decision kept while nobody in its tree could hear it must reach the "+
			"architect of the tree its own issue stood in when it was told, and the architect of the tree it moved into when the move found it "+
			"still kept or still held",
			tracked.ref, tracked.issue, stands, owedTo, owed, len(elsewhere), keptElsewhere(elsewhere),
			len(overheard), keptElsewhere(overheard))
	}
	if left := r.keptAnywhere(); len(left) != 0 {
		r.fatalf("%d decisions are still kept after the run quiesced (%s), want none: each was named by a start or catch-up that never "+
			"reached an architect, or stands under a root nothing starts again, and nothing names them again", len(left), strings.Join(left, ", "))
	}
}

// keptAnywhere is every decision still kept, with the issue it was decided on, the tree that issue
// stands in — the tree that owes it, which the row is read through now that a kept decision is
// keyed by its issue — and the row that claims it: once the run has quiesced there is no architect
// left to tell, so a row standing here is a decision nobody will be told.
func (r *keptRun) keptAnywhere() []string {
	rows, err := r.pool.Query(r.ctx, `select kept.ref, coalesce(owner.tree, ''), kept.issue, coalesce(kept.claimed_by, 0)
		from parked_decisions kept left join issues owner on owner.key = kept.issue order by kept.ref`)
	if err != nil {
		r.fatalf("read the decisions still kept: %v", err)
	}
	defer rows.Close()
	var left []string
	for rows.Next() {
		var ref, tree, issue string
		var claimed int64
		if err := rows.Scan(&ref, &tree, &issue, &claimed); err != nil {
			r.fatalf("scan a decision still kept: %v", err)
		}
		left = append(left, fmt.Sprintf("%s owed-to=%s issue=%s claimed-by=%d", ref, tree, issue, claimed))
	}
	if err := rows.Err(); err != nil {
		r.fatalf("read the decisions still kept: %v", err)
	}
	return left
}

// leftBefore says whether a telling that reached the architect of a tree the decision is not owed
// to is the one such telling the invariant accepts: it went to a tree the decision's issue has
// left, and the row that carried it was composed before the issue left that tree (keptDeparture),
// so its text named the reference while the issue still stood there.
func (r *keptRun) leftBefore(issue string, telling keptTelling) bool {
	if telling.row == 0 {
		return false
	}
	for _, departure := range r.departed[issue] {
		if departure.from == telling.tree && telling.row <= departure.rows {
			return true
		}
	}
	return false
}

func keptElsewhere(elsewhere []string) string {
	if len(elsewhere) == 0 {
		return "none"
	}
	return strings.Join(elsewhere, "; ")
}

// keptRefs names each kept decision and the row that claims it, which the tests of one hand-over
// or one publish report what a root still keeps with.
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
// architect, and the Dispatch and GitHub writes admission and the linger's close queue for a
// promoted or handed-over root — its status write and its issue branch — which go where this run
// drives nobody. The close's own tree_close rows go with them: what they do is retire each claim
// and run the tree's cleanup through its lifecycle, which is that machinery's own, tested there.
// Each has tests of its own.
func (r *keptRun) deliverRowsTheRunDoesNotDrive() {
	if _, err := r.pool.Exec(r.ctx, `delete from outbox where (kind <> $1 and kind <> $2)
		or (kind = $1 and payload->>'op' = 'tree_close')`,
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
// rows were composed before a moment. It is the sequence's own last value, read before each step
// to draw the line between the rows composed while an issue still stood in the tree it is about
// to leave and those composed after (keptDeparture, leftBefore).
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

// waitsForSlot is whether the root waits in `todo` for an admission slot (record.WaitsForSlot),
// which is where a person's decision on it is kept for the start its admission will queue.
func (r *keptRun) waitsForSlot() bool { return r.waitsForItsSlot(r.root) }

// waitsForItsSlot is the same reading for any driven tree, over the record as it stands now.
func (r *keptRun) waitsForItsSlot(tree *keptTree) bool {
	stored := r.issueRecord(tree.key)
	if stored == nil {
		return false
	}
	var waits bool
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		waits, err = record.WaitsForSlot(r.ctx, tx, r.records, *stored)
		return err
	}); err != nil {
		r.fatalf("read whether %s waits for its slot: %v", tree.key, err)
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
