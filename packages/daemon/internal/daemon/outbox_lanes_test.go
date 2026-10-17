package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The lanes (outbox_lanes.go) as the daemon runs them: woken by the commit that writes a row, by
// the timer armed for a row due later, and by the sweep; one issue's lifecycle rows one at a time
// and its board rows beside them; a running row's lease renewed under it; and a stop that joins
// every lane. Every test here drives Run on a held clock (heldClock) and waits on what it can
// observe — Dispatch writes, publishes, Spawn calls, the rows left, the log — never on time.

// runLanes starts runner.Run on clock and waits for its boot sweep to arm the first sweep timer,
// so every row a test writes after it is one the boot sweep never saw. It returns the stop that
// cancels Run and waits for it to return, run at cleanup too; the stop first runs every release
// given, since a lane blocked in a gated call would hold Run's join forever.
func runLanes(t *testing.T, runner *outbox, clock *heldClock, releases ...func()) func() {
	t.Helper()
	runner.now, runner.clock = clock.Now, clock
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			for _, release := range releases {
				release()
			}
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	testwait.Eventually(t, "Run armed its sweep", func() bool {
		live, _ := clock.armed(outboxLease)
		return live == 1
	})
	return stop
}

// fireSweep fires the one sweep timer Run armed and waits for Run to arm the next, which it does
// once the sweep has woken every lane with a row.
func fireSweep(t *testing.T, clock *heldClock) {
	t.Helper()
	_, before := clock.armed(outboxLease)
	if fired := clock.fire(outboxLease); fired != 1 {
		t.Fatalf("sweep timers fired = %d, want the one Run armed", fired)
	}
	testwait.Eventually(t, "the sweep ran and the next was armed", func() bool {
		live, total := clock.armed(outboxLease)
		return live == 1 && total == before+1
	})
}

// liveLanes is how many lanes the registry runs: the one internal reading these tests make, for a
// lane's exit.
func liveLanes(runner *outbox) int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return len(runner.lanes)
}

// gate holds a fake's call until the test opens it, counting the calls that reached it.
type gate struct {
	mu      sync.Mutex
	entered int
	release chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{release: make(chan struct{})} }

// enter counts the call and holds it until the gate is open.
func (g *gate) enter() {
	g.mu.Lock()
	g.entered++
	g.mu.Unlock()
	<-g.release
}

// entries is how many calls reached the gate, held or let through.
func (g *gate) entries() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.entered
}

// open lets every call through, the held ones and every later one; opening twice is once.
func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// gatedSpawn is the fake runtime with its Spawn behind a gate: a start blocked in it is a lane
// blocked on the runtime, as a Sandbox launch waits on the cluster for its pod. The fake records
// the call only once it is let through, so its Spawn calls are the launches that completed.
type gatedSpawn struct {
	*fake.Runtime
	gate *gate
}

func (g *gatedSpawn) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	g.gate.enter()
	return g.Runtime.Spawn(ctx, spec)
}

// gatedPublisher is holderPublisher with every publish behind a gate: a notice blocked in it is a
// lane blocked on the listener.
type gatedPublisher struct {
	*holderPublisher
	gate *gate
}

func (g *gatedPublisher) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	g.gate.enter()
	return g.holderPublisher.Publish(ctx, topic, message, payload, key)
}

// postingDispatch is Dispatch as a message post meets it: the bodies posted, by issue, and no
// message read back, so each row posts once. It is read while a lane posts.
type postingDispatch struct {
	mu     sync.Mutex
	posted map[string][]string
}

func (d *postingDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}
func (d *postingDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	return dispatch.Issue{Key: key}, nil
}
func (d *postingDispatch) SetStatus(context.Context, string, string) error {
	return errors.New("postingDispatch takes no status")
}
func (d *postingDispatch) HandOver(context.Context, dispatch.Issue, string) error {
	return errors.New("postingDispatch takes no hand-over")
}
func (d *postingDispatch) PostMessage(_ context.Context, key, body string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.posted == nil {
		d.posted = map[string][]string{}
	}
	d.posted[key] = append(d.posted[key], body)
	return nil
}
func (d *postingDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *postingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *postingDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}

// postsTo is how many messages were posted on issue.
func (d *postingDispatch) postsTo(issue string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.posted[issue])
}

// laneBoard is one issue's Dispatch board as a lane writes it: it refuses the first failures
// writes, applies each after, and keeps the statuses that landed in order. It is read while the
// lane writes, which boardDispatch is not made for.
type laneBoard struct {
	mu       sync.Mutex
	status   string
	failures int
	landed   []string
}

func (d *laneBoard) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}
func (d *laneBoard) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return dispatch.Issue{Key: key, Status: d.status}, nil
}
func (d *laneBoard) SetStatus(_ context.Context, _ string, status string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failures > 0 {
		d.failures--
		return errors.New("dispatch unavailable")
	}
	d.status = status
	d.landed = append(d.landed, status)
	return nil
}
func (d *laneBoard) HandOver(context.Context, dispatch.Issue, string) error {
	return errors.New("laneBoard takes no hand-over")
}
func (d *laneBoard) PostMessage(context.Context, string, string) error { return nil }
func (d *laneBoard) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *laneBoard) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *laneBoard) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}

// statuses is each status that landed, in order.
func (d *laneBoard) statuses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.landed...)
}

// enqueuing is an intake handler that writes rows inside the fact's transaction, as the workflow
// does; through the wake-decorated projectRecords, the commit wakes each row's lane.
type enqueuing struct {
	records record.Store
	rows    []record.OutboxRow
}

func (h enqueuing) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	for _, row := range h.rows {
		if err := h.records.Enqueue(ctx, tx, row); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

// commitRows writes rows through records in one fact's transaction (intake.ApplyFact), under
// eventID, which must be new to the database.
func commitRows(t *testing.T, pool *pgxpool.Pool, records record.Store, eventID string, rows ...record.OutboxRow) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "lanes-test", eventID, intake.DaemonBooted{Boot: 1}, enqueuing{records: records, rows: rows}); err != nil {
		t.Fatalf("commit %s: %v", eventID, err)
	}
}

// wakingRecords is the store the workflow writes through (projectRecords): every row it writes
// wakes the row's lane once the write commits.
func wakingRecords(runner *outbox) projectRecords {
	return projectRecords{Store: record.NewStore(), project: "LEGION", wake: runner.wake, log: quietLogger()}
}

// microClock is a held clock at a microsecond, the precision the rows' next_at is stored at, so
// the wait a lane arms for a later row is exactly the duration the test fires.
func microClock() *heldClock {
	return &heldClock{at: time.Now().UTC().Truncate(time.Microsecond)}
}

// outboxIDs is every row's id, in id order.
func outboxIDs(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select id from outbox order by id")
	if err != nil {
		t.Fatalf("list outbox rows: %v", err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// outboxRowsOf is how many rows issue has left.
func outboxRowsOf(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where issue = $1", issue).Scan(&rows); err != nil {
		t.Fatalf("count the outbox rows of %s: %v", issue, err)
	}
	return rows
}

// rowState is what a row's claim, renewal and settle left on it.
type rowState struct {
	leaseToken string
	leaseUntil *time.Time
	attempts   int
	nextAt     time.Time
}

func rowStateOf(t *testing.T, pool *pgxpool.Pool, id int64) rowState {
	t.Helper()
	var state rowState
	if err := pool.QueryRow(context.Background(), "select lease_token, lease_until, attempts, next_at from outbox where id = $1", id).
		Scan(&state.leaseToken, &state.leaseUntil, &state.attempts, &state.nextAt); err != nil {
		t.Fatalf("read outbox row %d: %v", id, err)
	}
	return state
}

// spawnedRoles is the role of each launch the fake runtime recorded, in order.
func spawnedRoles(rt *fake.Runtime) []claim.Role {
	calls := rt.CallsOf("Spawn")
	roles := make([]claim.Role, len(calls))
	for i, call := range calls {
		roles[i] = call.Spec.Role
	}
	return roles
}

// laneTree is one root issue of its own tree whose lifecycle lane a test holds inside its
// architect's start: the issue, the outbox running on a supervisor whose runtime gates every
// Spawn, the clock, the store the test writes rows through as the workflow does, the Dispatch
// client its board rows write and the publisher its notices reach, and how many workspaces its
// starts provisioned.
type laneTree struct {
	pool       *pgxpool.Pool
	root       record.Issue
	runner     *outbox
	records    record.Store
	clock      *heldClock
	spawn      *gatedSpawn
	rt         *fake.Runtime
	client     *blockingDispatch
	publisher  *holderPublisher
	provisions atomic.Int32
}

// startLaneTree records the tree, builds the outbox on a gated runtime with the log given (quiet
// when nil) and starts its lanes.
func startLaneTree(t *testing.T, log *slog.Logger) *laneTree {
	t.Helper()
	tree := &laneTree{pool: isolatedOutboxPool(t), clock: microClock()}
	tree.root = record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, tree.pool, record.NewStore(), tree.root)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	tree.rt = rt
	tree.spawn = &gatedSpawn{Runtime: rt, gate: newGate()}
	sup.deps.Runtime = tree.spawn
	tree.client = &blockingDispatch{issue: dispatch.Issue{Key: tree.root.Key, Status: "todo"}}
	tree.publisher = &holderPublisher{}
	if log == nil {
		log = quietLogger()
	}
	tree.runner = &outbox{
		pool: tree.pool, dispatchProject: "LEGION", records: record.NewStore(), supervisor: sup, tokens: outboxTokens{}, project: "legion",
		stateDir: t.TempDir(), dispatch: tree.client, notices: tree.publisher, log: log,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			tree.provisions.Add(1)
			return workspace.Workspace{}, nil
		},
		remove: func(context.Context, workspace.Workspace) error { return nil },
	}
	tree.records = wakingRecords(tree.runner)
	runLanes(t, tree.runner, tree.clock, tree.spawn.gate.open)
	return tree
}

// start is a start of role on the root, due now.
func (tree *laneTree) start(t *testing.T, role claim.Role) record.OutboxRow {
	t.Helper()
	return mustOutboxRow(t, tree.root.Key, record.SuperviseRequest{Op: "start", Tree: tree.root.Tree, Role: role, Generation: tree.root.Generation}, tree.clock.Now())
}

// commit writes rows as one committed fact, which wakes their lanes.
func (tree *laneTree) commit(t *testing.T, eventID string, rows ...record.OutboxRow) {
	t.Helper()
	commitRows(t, tree.pool, tree.records, eventID, rows...)
}

// holdArchitectStart commits the architect's start and waits until its lane is inside the gated
// Spawn: the lifecycle lane is blocked on the runtime from here until the gate opens.
func (tree *laneTree) holdArchitectStart(t *testing.T) {
	t.Helper()
	tree.commit(t, "architect-start", tree.start(t, claim.RoleArchitect))
	testwait.Eventually(t, "the architect's start reached Spawn", func() bool { return tree.spawn.gate.entries() == 1 })
}

// blocked fails the test unless the architect's start is still the one call held in Spawn and
// no launch has gone through.
func (tree *laneTree) blocked(t *testing.T, when string) {
	t.Helper()
	if entries, spawned := tree.spawn.gate.entries(), spawnedRoles(tree.rt); entries != 1 || len(spawned) != 0 {
		t.Fatalf("%s: %d calls reached Spawn and %v went through, want the architect's start alone, still held", when, entries, spawned)
	}
}

// A row's commit is what runs it: the fact that writes a dispatch_message row through the
// workflow's store (projectRecords) wakes the row's lane as it commits, and the message is posted
// with no drain called and no timer fired. A row written through the bare store wakes nothing and
// waits for the sweep: it is still queued once a row committed after it has posted, and posts
// when the sweep's timer fires.
func TestACommittedRowRunsOnItsLaneWithoutASweep(t *testing.T) {
	pool := isolatedOutboxPool(t)
	clock := microClock()
	client := &postingDispatch{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: record.NewStore(), dispatch: client, log: quietLogger()}
	runLanes(t, runner, clock)

	// The unwoken row goes first, so the woken row's post is the lanes running while it waited.
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-209", record.MessagePost{Body: "written outside the workflow"}, clock.Now()))
	commitRows(t, pool, wakingRecords(runner), "message", mustOutboxRow(t, "LEGION-208", record.MessagePost{Body: "committed by a fact"}, clock.Now()))
	testwait.Eventually(t, "the committed row's message was posted", func() bool {
		return client.postsTo("LEGION-208") == 1 && outboxRowsOf(t, pool, "LEGION-208") == 0
	})
	if _, sweeps := clock.armed(outboxLease); sweeps != 1 {
		t.Fatalf("sweep timers armed = %d, want the one Run armed at boot and never fired", sweeps)
	}
	if posts, left := client.postsTo("LEGION-209"), outboxRowsOf(t, pool, "LEGION-209"); posts != 0 || left != 1 {
		t.Fatalf("the row written outside the workflow: %d posts with %d rows left, want none and the row still queued", posts, left)
	}

	fireSweep(t, clock)
	testwait.Eventually(t, "the sweep ran the unwoken row", func() bool {
		return client.postsTo("LEGION-209") == 1 && outboxRows(t, pool) == 0
	})
}

// A row due later runs at its moment and not before: the commit wakes its lane, which finds
// nothing due and arms one timer for the wait, and the row runs when that timer fires on the
// clock, with no sweep in between.
func TestARowDueLaterRunsWhenItsLanesTimerFires(t *testing.T) {
	pool := isolatedOutboxPool(t)
	clock := microClock()
	client := &postingDispatch{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: record.NewStore(), dispatch: client, log: quietLogger()}
	runLanes(t, runner, clock)

	commitRows(t, pool, wakingRecords(runner), "later", mustOutboxRow(t, "LEGION-208", record.MessagePost{Body: "due in an hour"}, clock.Now().Add(time.Hour)))
	testwait.Eventually(t, "the lane armed its wait for the hour", func() bool {
		live, total := clock.armed(time.Hour)
		return live == 1 && total == 1
	})
	if posts, left := client.postsTo("LEGION-208"), outboxRows(t, pool); posts != 0 || left != 1 {
		t.Fatalf("before the hour: %d posts with %d rows left, want none and the row waiting", posts, left)
	}

	clock.advance(time.Hour)
	if fired := clock.fire(time.Hour); fired != 1 {
		t.Fatalf("hour timers fired = %d, want the lane's", fired)
	}
	testwait.Eventually(t, "the row ran at its hour", func() bool {
		return client.postsTo("LEGION-208") == 1 && outboxRows(t, pool) == 0
	})
	if _, sweeps := clock.armed(outboxLease); sweeps != 1 {
		t.Fatalf("sweep timers armed = %d, want the one Run armed at boot and never fired", sweeps)
	}
}

// A notice waiting for a later attempt is released by its architect's ready (releaseWaiting): the
// expedite that makes it due wakes its lane as it commits, and the notice is published with no
// timer fired — neither the lane's wait for the hour it was due at nor the sweep.
func TestAReleasedNoticeRunsOnItsLaneWhenTheReleaseCommits(t *testing.T) {
	pool := isolatedOutboxPool(t)
	noticeTree(t, pool, record.NewStore(), false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architect := architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	clock := microClock()
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", notices: publisher, supervisor: sup, project: "legion"}
	runner.records = wakingRecords(runner)
	runLanes(t, runner, clock)

	commitRows(t, pool, runner.records, "held-notice", mustOutboxRow(t, "LEGION-2",
		record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, clock.Now().Add(time.Hour)))
	testwait.Eventually(t, "the lane armed its wait for the hour", func() bool {
		live, _ := clock.armed(time.Hour)
		return live == 1
	})
	if _, delivered := publisher.snapshot(); len(delivered) != 0 {
		t.Fatalf("delivered %+v before the release, want nothing", delivered)
	}

	if err := runner.releaseWaiting(context.Background(), architect); err != nil {
		t.Fatalf("release the architect's notices: %v", err)
	}
	testwait.Eventually(t, "the released notice was published and its lane dropped its wait", func() bool {
		_, delivered := publisher.snapshot()
		live, _ := clock.armed(time.Hour)
		return len(delivered) == 1 && outboxRows(t, pool) == 0 && live == 0
	})
	_, delivered := publisher.snapshot()
	if got := deliveredKinds(t, delivered); fmt.Sprint(got) != "[pr-blocked to LEGION-1]" {
		t.Fatalf("delivered %v, want the held pr-blocked at the root architect", got)
	}
	_, hours := clock.armed(time.Hour)
	_, sweeps := clock.armed(outboxLease)
	if hours != 1 || sweeps != 1 {
		t.Fatalf("hour timers armed = %d and sweep timers armed = %d, want one each and neither fired", hours, sweeps)
	}
}

// A READY merge GitHub refuses for good tells the architect as the refusal commits: the
// ready_merge row's finish and its merge-refused notice are one transaction through the workflow's
// store (projectRecords), whose commit wakes the notice's board lane, so the notice is published
// with no sweep fired and no row written outside a commit scope. Written unscoped, the wake comes
// before the commit and the lane finds nothing unless the commit wins the race, so the write's
// ERROR line (wakeOnCommit) is the witness the test holds, beside the publish and the quiet sweep.
func TestAREADYMergeRefusalsNoticeRunsOnTheBoardLaneAsTheRefusalCommits(t *testing.T) {
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") },
		refusedAnswer("Squash merges are not allowed on this repository."))
	pool := isolatedOutboxPool(t)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-208", supervise.StateWorking)
	clock := microClock()
	publisher := &holderPublisher{}
	var logged syncBuffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	runner := mergeRunner(pool, nil, github.url, log, clock.Now)
	runner.supervisor, runner.notices = sup, publisher
	runner.records = projectRecords{Store: record.NewStore(), project: "LEGION", wake: runner.wake, log: log}
	seedReadyMerge(t, pool, runner.records, "LEGION-208", awaitingREADY)
	runLanes(t, runner, clock)

	commitRows(t, pool, runner.records, "ready", mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock.Now()))
	testwait.Eventually(t, "the merge-refused notice was published and both rows finished", func() bool {
		_, delivered := publisher.snapshot()
		return len(delivered) == 1 && outboxRows(t, pool) == 0
	})
	_, delivered := publisher.snapshot()
	if kind := delivered[0].payload.(record.Notice).Kind; kind != "merge-refused" || delivered[0].topic != architectTopic(t, "LEGION-208") {
		t.Fatalf("delivered %s to %s, want merge-refused to LEGION-208's architect", kind, delivered[0].topic)
	}
	if merges, _ := github.merged(); merges != 1 {
		t.Fatalf("merge submissions = %d, want the one GitHub refused", merges)
	}
	if _, sweeps := clock.armed(outboxLease); sweeps != 1 {
		t.Fatalf("sweep timers armed = %d, want the one Run armed and never fired", sweeps)
	}
	if strings.Contains(logged.String(), "written outside a commit scope") {
		t.Fatalf("the refusal's notice was written outside a commit scope:\n%s", logged.String())
	}
}

// A start blocked on the runtime holds only its own issue's lifecycle rows. Another issue's status
// write made while it blocks lands at once; the issue's own second lifecycle row, written while
// its start blocks, waits — the lanes run on around it — and runs once the start has returned.
func TestAStartBlockedOnTheRuntimeHoldsOnlyItsOwnIssuesLifecycleLane(t *testing.T) {
	tree := startLaneTree(t, nil)
	tree.holdArchitectStart(t)

	status := func(t *testing.T) record.OutboxRow {
		t.Helper()
		return mustOutboxRow(t, "LEGION-209", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, tree.clock.Now())
	}
	tree.commit(t, "other-status", status(t))
	testwait.Eventually(t, "the other issue's status landed", func() bool {
		return tree.client.count() == 1 && outboxRowsOf(t, tree.pool, "LEGION-209") == 0
	})
	tree.blocked(t, "after the other issue's status landed")

	tree.commit(t, "own-implementer", tree.start(t, claim.RoleImplementer))
	// Another write of the other issue lands meanwhile, so the lanes ran while the row waited.
	tree.commit(t, "other-status-again", status(t))
	testwait.Eventually(t, "the other issue's second status landed", func() bool {
		return tree.client.count() == 2 && outboxRowsOf(t, tree.pool, "LEGION-209") == 0
	})
	ids := outboxIDs(t, tree.pool)
	if len(ids) != 2 {
		t.Fatalf("outbox rows = %v, want the blocked start and the implementer's start behind it", ids)
	}
	if held := rowStateOf(t, tree.pool, ids[1]); held.leaseToken != "" || tree.provisions.Load() != 1 {
		t.Fatalf("the implementer's start is leased %q after %d provisions, want unleased behind the blocked start, which provisioned once", held.leaseToken, tree.provisions.Load())
	}
	tree.blocked(t, "with the implementer's start behind it")

	tree.spawn.gate.open()
	testwait.Eventually(t, "both starts ran", func() bool { return outboxRows(t, tree.pool) == 0 })
	if got := spawnedRoles(tree.rt); fmt.Sprint(got) != "[architect implementer]" || tree.provisions.Load() != 2 {
		t.Fatalf("launched %v after %d provisions, want the architect then the implementer, each provisioned", got, tree.provisions.Load())
	}
}

// A fact of an issue whose lifecycle lane is blocked commits as any fact does: the wake its row
// registers returns at once rather than waiting on the lane, so the fact is applied within a
// bound, and the board row it wrote runs beside the blocked start.
func TestACommitOnABlockedLifecycleLanesIssueReturnsAtOnceAndItsBoardRowRuns(t *testing.T) {
	tree := startLaneTree(t, nil)
	tree.holdArchitectStart(t)

	began := time.Now()
	tree.commit(t, "own-status", mustOutboxRow(t, tree.root.Key, record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, tree.clock.Now()))
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the fact took %s to commit with the issue's lifecycle lane blocked, want it back within 2s", took)
	}
	testwait.Eventually(t, "the issue's status landed beside its blocked start", func() bool {
		return tree.client.count() == 1 && outboxRowsOf(t, tree.pool, tree.root.Key) == 1
	})
	tree.blocked(t, "after the issue's status landed")

	tree.spawn.gate.open()
	testwait.Eventually(t, "the start ran", func() bool { return outboxRows(t, tree.pool) == 0 })
}

// One issue's status writes reach Dispatch in the order the workflow made them when the lane
// runs them: the newer row waits behind the older while it backs off, and once the backoff's
// timer fires the older lands and then the newer, nothing drained by hand.
func TestTheLaneWritesOneIssuesStatusesInOrderWhenTheOlderFailsFirst(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	clock := microClock()
	now := clock.Now()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "in_progress", Status: "testing"}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "testing", Status: "needs_review"}, now))
	board := &laneBoard{status: "in_progress", failures: 1}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: board, log: quietLogger()}
	runLanes(t, runner, clock)

	backoff := retryBackoff(0)
	testwait.Eventually(t, "the older row failed and its lane armed its backoff", func() bool {
		live, _ := clock.armed(backoff)
		return live == 1
	})
	if landed, left := board.statuses(), outboxRows(t, pool); len(landed) != 0 || left != 2 {
		t.Fatalf("while the older row backs off: landed %v with %d rows left, want nothing landed and both rows kept", landed, left)
	}

	clock.advance(backoff)
	if fired := clock.fire(backoff); fired != 1 {
		t.Fatalf("backoff timers fired = %d, want the lane's", fired)
	}
	testwait.Eventually(t, "both statuses landed", func() bool { return outboxRows(t, pool) == 0 })
	if got := board.statuses(); fmt.Sprint(got) != "[testing needs_review]" {
		t.Fatalf("statuses landed %v, want testing then needs_review", got)
	}
}

// Two notices of one tree for one architect, from two issues, reach it in the order they were
// written though they run on two lanes: the newer finds the older unfinished and waits
// (errNoticeWaits) on its first backoff, and once the older has gone and that backoff's timer
// fires, the newer is published — after the older, never before it.
func TestANewerNoticeOnAnotherLaneWaitsBehindTheOlderAndFollowsItAfterOneBackoff(t *testing.T) {
	pool := isolatedOutboxPool(t)
	noticeTree(t, pool, record.NewStore(), false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	clock := microClock()
	publisher := &gatedPublisher{holderPublisher: &holderPublisher{}, gate: newGate()}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", notices: publisher, supervisor: sup, project: "legion"}
	runner.records = wakingRecords(runner)
	runLanes(t, runner, clock, publisher.gate.open)

	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}
	commitRows(t, pool, runner.records, "older", mustOutboxRow(t, "LEGION-2", notice, clock.Now()))
	testwait.Eventually(t, "the older notice reached the publisher", func() bool { return publisher.gate.entries() == 1 })
	commitRows(t, pool, runner.records, "newer", mustOutboxRow(t, "LEGION-3", notice, clock.Now()))
	backoff := retryBackoff(0)
	testwait.Eventually(t, "the newer notice waited and its lane armed the backoff", func() bool {
		live, _ := clock.armed(backoff)
		return live == 1
	})
	if _, delivered := publisher.snapshot(); len(delivered) != 0 || publisher.gate.entries() != 1 || outboxRows(t, pool) != 2 {
		t.Fatalf("while the older notice is held in the publisher: delivered %+v, %d publishes, %d rows; want none, the older's alone, both rows",
			delivered, publisher.gate.entries(), outboxRows(t, pool))
	}

	publisher.gate.open()
	testwait.Eventually(t, "the older notice was published and finished", func() bool {
		_, delivered := publisher.snapshot()
		return len(delivered) == 1 && outboxRows(t, pool) == 1
	})
	clock.advance(backoff)
	if fired := clock.fire(backoff); fired != 1 {
		t.Fatalf("backoff timers fired = %d, want the newer notice's lane's", fired)
	}
	testwait.Eventually(t, "the newer notice followed", func() bool { return outboxRows(t, pool) == 0 })
	_, delivered := publisher.snapshot()
	messages := make([]string, len(delivered))
	for i, published := range delivered {
		messages[i] = published.message
	}
	want := []string{noticeSummary(notice.Kind, "LEGION-2"), noticeSummary(notice.Kind, "LEGION-3")}
	if fmt.Sprint(messages) != fmt.Sprint(want) {
		t.Fatalf("published %v, want %v: the older first, then the newer", messages, want)
	}
}

// One issue's lifecycle rows run one at a time, in id order: with two starts due, the second's
// provision and launch begin only once the first has returned from the runtime.
func TestALifecycleLaneRunsOneRowAtATimeInIdOrder(t *testing.T) {
	tree := startLaneTree(t, nil)
	tree.commit(t, "two-starts", tree.start(t, claim.RoleArchitect), tree.start(t, claim.RoleImplementer))
	testwait.Eventually(t, "the architect's start reached Spawn", func() bool { return tree.spawn.gate.entries() == 1 })
	ids := outboxIDs(t, tree.pool)
	if len(ids) != 2 {
		t.Fatalf("outbox rows = %v, want the two starts", ids)
	}
	if second := rowStateOf(t, tree.pool, ids[1]); second.leaseToken != "" || tree.provisions.Load() != 1 {
		t.Fatalf("the second start is leased %q after %d provisions, want it unleased behind the first, which provisioned once", second.leaseToken, tree.provisions.Load())
	}
	tree.blocked(t, "with the second start due behind the first")

	tree.spawn.gate.open()
	testwait.Eventually(t, "both starts ran", func() bool { return outboxRows(t, tree.pool) == 0 })
	if got := spawnedRoles(tree.rt); fmt.Sprint(got) != "[architect implementer]" || tree.spawn.gate.entries() != 2 || tree.provisions.Load() != 2 {
		t.Fatalf("launched %v through %d Spawn calls after %d provisions, want the architect then the implementer, two of each", got, tree.spawn.gate.entries(), tree.provisions.Load())
	}
}

// An issue's notice runs on its board lane beside its blocked lifecycle lane: the notice is
// published to the issue's architect while that architect's start is still held in Spawn.
func TestAnIssuesNoticePublishesWhileItsLifecycleRowBlocksOnTheRuntime(t *testing.T) {
	tree := startLaneTree(t, nil)
	tree.holdArchitectStart(t)

	tree.commit(t, "own-notice", mustOutboxRow(t, tree.root.Key, record.Notice{Kind: "design-approved", Version: 2}, tree.clock.Now()))
	testwait.Eventually(t, "the notice was published beside the blocked start", func() bool {
		_, delivered := tree.publisher.snapshot()
		return len(delivered) == 1 && outboxRowsOf(t, tree.pool, tree.root.Key) == 1
	})
	if _, delivered := tree.publisher.snapshot(); delivered[0].topic != architectTopic(t, tree.root.Key) {
		t.Fatalf("the notice went to %s, want the issue's architect's role topic", delivered[0].topic)
	}
	tree.blocked(t, "after the notice was published")

	tree.spawn.gate.open()
	testwait.Eventually(t, "the start ran", func() bool { return outboxRows(t, tree.pool) == 0 })
}

// A row that runs long keeps its lease: every third of the lease a tick moves the lease a whole
// lease ahead, so the sweep that comes while it runs — at the very moment the first lease would
// have lapsed — claims nothing again, and the row finishes once, with one write.
func TestALaneRenewsItsRunningRowsLeaseAndNoSweepClaimsItAgain(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	clock := microClock()
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}, entered: entered, release: release}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, log: quietLogger()}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, clock.Now()))
	runLanes(t, runner, clock, func() { released.Do(func() { close(release) }) })
	<-entered

	id := outboxIDs(t, pool)[0]
	claimed := rowStateOf(t, pool, id)
	if claimed.leaseToken == "" || claimed.leaseUntil == nil || !claimed.leaseUntil.Equal(clock.Now().Add(outboxLease)) {
		t.Fatalf("the running row is leased %q until %v, want a lease of %s from its claim", claimed.leaseToken, claimed.leaseUntil, outboxLease)
	}
	lease := *claimed.leaseUntil
	renewEvery := outboxLease / 3
	for tick := range 6 {
		clock.advance(renewEvery)
		if fired := clock.fire(renewEvery); fired != 1 {
			t.Fatalf("tick %d: renewal timers fired = %d, want the running row's", tick, fired)
		}
		renewed := rowStateOf(t, pool, id)
		if renewed.leaseUntil == nil || !renewed.leaseUntil.After(lease) || !renewed.leaseUntil.Equal(clock.Now().Add(outboxLease)) {
			t.Fatalf("tick %d: lease_until = %v after %v, want it moved a whole lease ahead of now", tick, renewed.leaseUntil, lease)
		}
		lease = *renewed.leaseUntil
		if tick == 2 {
			// The sweep at the moment the first lease would have lapsed.
			fireSweep(t, clock)
		}
		if state := rowStateOf(t, pool, id); client.count() != 1 || state.leaseToken != claimed.leaseToken {
			t.Fatalf("tick %d: %d writes, lease token %q; want the one write under the one lease %q", tick, client.count(), state.leaseToken, claimed.leaseToken)
		}
	}

	released.Do(func() { close(release) })
	testwait.Eventually(t, "the row finished and its renewal stopped", func() bool {
		live, _ := clock.armed(renewEvery)
		return outboxRows(t, pool) == 0 && live == 0
	})
	if got := client.count(); got != 1 {
		t.Fatalf("status writes = %d, want the one", got)
	}
}

// A row whose runner died mid-run — leased by a token nobody holds — runs again once the lease has
// lapsed, on the sweep after the lapse, with no restart: the sweep before the lapse starts its
// lane, which finds the row another's and exits.
func TestTheSweepAfterALeasesLapseStartsItsLaneAgainWithoutARestart(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	clock := microClock()
	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, log: quietLogger()}
	runLanes(t, runner, clock)

	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, clock.Now()))
	if _, err := pool.Exec(context.Background(), "update outbox set lease_token = 'a-runner-that-died', lease_until = $1", clock.Now().Add(outboxLease/2)); err != nil {
		t.Fatalf("leave a lease nobody holds on the row: %v", err)
	}

	fireSweep(t, clock)
	testwait.Eventually(t, "the lane the sweep started found the row leased and exited", func() bool { return liveLanes(runner) == 0 })
	if got, left := client.count(), outboxRows(t, pool); got != 0 || left != 1 {
		t.Fatalf("before the lease lapsed: %d writes with %d rows left, want none and the leased row", got, left)
	}

	clock.advance(outboxLease)
	fireSweep(t, clock)
	testwait.Eventually(t, "the row ran after its lease lapsed", func() bool {
		return client.count() == 1 && outboxRows(t, pool) == 0
	})
}

// The daemon's stop joins its lanes: Run, its context cancelled while a lane holds a row blocked in
// Dispatch, returns only once that lane has. The row's write failed on the stop, so the row is
// retried with its lease cleared, on a settle the stop's context does not cut short; until the
// block lifts the row is still the lane's, under its lease.
func TestRunReturnsOnlyAfterItsLanesAndSettlesTheRowAStopInterrupted(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	clock := microClock()
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}, entered: entered, release: release, fail: errors.New("dispatch: context canceled")}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: clock.Now, clock: clock, log: quietLogger()}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, clock.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	defer func() {
		released.Do(func() { close(release) })
		cancel()
		<-done
	}()
	<-entered
	id := outboxIDs(t, pool)[0]

	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while its lane held a row blocked in Dispatch")
	default:
	}
	if leased := rowStateOf(t, pool, id); leased.leaseToken == "" || leased.attempts != 0 {
		t.Fatalf("before the block lifted the row is leased %q with %d attempts, want still the lane's under its lease", leased.leaseToken, leased.attempts)
	}

	released.Do(func() { close(release) })
	<-done
	retried := rowStateOf(t, pool, id)
	if retried.leaseToken != "" || retried.attempts != 1 || !retried.nextAt.Equal(clock.Now().Add(retryBackoff(0))) {
		t.Fatalf("after Run returned the row is leased %q with %d attempts, due %v; want it retried unleased on its first backoff, due %v",
			retried.leaseToken, retried.attempts, retried.nextAt, clock.Now().Add(retryBackoff(0)))
	}
	if got := client.count(); got != 1 {
		t.Fatalf("status writes = %d, want the one the stop interrupted", got)
	}
}

// heldWarnings is each line in the JSON log saying a row is held behind its issue's running
// lifecycle row, as (row, waitsOn).
func heldWarnings(t *testing.T, logged string) [][2]int64 {
	t.Helper()
	var warnings [][2]int64
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("read log line %q: %v", line, err)
		}
		if entry["msg"] != "outbox row held behind its issue's running lifecycle row" {
			continue
		}
		row, _ := entry["row"].(float64)
		waitsOn, _ := entry["waitsOn"].(float64)
		warnings = append(warnings, [2]int64{int64(row), int64(waitsOn)})
	}
	return warnings
}

// A lifecycle row that runs long is what an operator looks at when the issue's next start or stop
// does not happen: its lease renewal logs each lifecycle row held behind it, once, when that row
// has waited heldWarnAfter, naming the held row and the running one — and nothing before then.
func TestARunningLifecycleRowLogsEachRowHeldBehindItOnceItHasWaitedLong(t *testing.T) {
	var logged syncBuffer
	tree := startLaneTree(t, slog.New(slog.NewJSONHandler(&logged, nil)))
	tree.holdArchitectStart(t)
	tree.commit(t, "own-implementer", tree.start(t, claim.RoleImplementer))
	ids := outboxIDs(t, tree.pool)
	if len(ids) != 2 {
		t.Fatalf("outbox rows = %v, want the running start and the one held behind it", ids)
	}
	running, held := ids[0], ids[1]

	renewEvery := outboxLease / 3
	tick := func() {
		t.Helper()
		tree.clock.advance(renewEvery)
		if fired := tree.clock.fire(renewEvery); fired != 1 {
			t.Fatalf("renewal timers fired = %d, want the running row's", fired)
		}
	}
	ticks := int(heldWarnAfter / renewEvery)
	for range ticks - 1 {
		tick()
	}
	if got := heldWarnings(t, logged.String()); len(got) != 0 {
		t.Fatalf("held rows logged %v after %s, want none before %s", got, heldWarnAfter-renewEvery, heldWarnAfter)
	}
	tick()
	if got := heldWarnings(t, logged.String()); len(got) != 1 || got[0] != [2]int64{held, running} {
		t.Fatalf("held rows logged %v after %s, want row %d held behind row %d, once", got, heldWarnAfter, held, running)
	}
	tick()
	if got := heldWarnings(t, logged.String()); len(got) != 1 {
		t.Fatalf("held rows logged %v after %s, want the one line, not one per tick", got, heldWarnAfter+renewEvery)
	}
	if lease := rowStateOf(t, tree.pool, running); lease.leaseUntil == nil || !lease.leaseUntil.Equal(tree.clock.Now().Add(outboxLease)) {
		t.Fatalf("the running row's lease_until = %v, want renewed to a lease ahead of now", lease.leaseUntil)
	}
	tree.blocked(t, "after the held row was logged")

	tree.spawn.gate.open()
	testwait.Eventually(t, "both starts ran", func() bool { return outboxRows(t, tree.pool) == 0 })
}
