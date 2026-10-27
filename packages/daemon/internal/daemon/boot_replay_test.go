package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// terminalClaims is n claims of project, each its own issue's planner at generation 1, every
// other one ready and the rest failed, as a boot reads them.
func terminalClaims(t *testing.T, project string, n int) []supervise.Claim {
	t.Helper()
	claims := make([]supervise.Claim, n)
	for i := range claims {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		token, err := claim.NewToken(project, issue, claim.RolePlanner)
		if err != nil {
			t.Fatal(err)
		}
		state := supervise.StateReady
		if i%2 == 1 {
			state = supervise.StateFailed
		}
		claims[i] = supervise.Claim{Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RolePlanner, Generation: 1, State: state, Failure: "worker exited"}
	}
	return claims
}

// A boot replays the ready or failure fact of every stored claim in either state, and nearly all
// were applied when they happened. Those duplicates must not queue on the fact lock behind a live
// fact: a boot over hundreds of them, each waiting there in turn on its budget, ran out of it and
// exited with `serialize fact supervise/…: context deadline exceeded`. Here a live fact holds
// the lock for the whole replay; the replay of facts already applied finishes anyway and applies
// nothing again, and a claim's fact no daemon applied is still applied, once the lock is free.
func TestTheBootsReplayOfAppliedTerminalFactsWaitsBehindNoLiveFact(t *testing.T) {
	pool := isolatedOutboxPool(t)
	handler := &outboxFactHandler{}
	w := &workflowRuntime{pool: pool, handlers: []intake.Handler{handler}, log: quietLogger()}
	claims := terminalClaims(t, "legion", 300)
	if err := w.replayTerminal(context.Background(), claims, bootTimeout); err != nil {
		t.Fatalf("the first replay: %v", err)
	}
	if got := len(handler.facts()); got != len(claims) {
		t.Fatalf("the first replay applied %d facts, want %d", got, len(claims))
	}

	live, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A failed assertion below must not leave the lock held: the pool's close at cleanup waits for
	// this connection.
	defer func() { _ = live.Rollback(context.Background()) }()
	if err := intake.SerializeFacts(context.Background(), live); err != nil {
		t.Fatalf("take the fact lock as a live fact does: %v", err)
	}
	started := time.Now()
	err = w.replayTerminal(context.Background(), claims, 5*time.Second)
	took := time.Since(started)
	if err != nil {
		t.Fatalf("the replay behind a live fact holding the lock = %v after %s, want it done without the lock", err, took)
	}
	if got := len(handler.facts()); got != len(claims) {
		t.Fatalf("the replay of applied facts applied %d more, want none", got-len(claims))
	}
	if err := live.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The next boot's claims mix every state a boot reads: a working claim, a launch the boot left
	// uncertain and the controller's ready, none of them a workflow fact, then the 300 applied, then
	// one ready claim no daemon applied, which alone is applied.
	project := "legion"
	working, _ := claim.NewToken(project, "LEGION-901", claim.RoleImplementer)
	uncertain, _ := claim.NewToken(project, "LEGION-902", claim.RoleTester)
	mixed := []supervise.Claim{
		{Token: working, Project: project, Tree: "LEGION-901", Issue: "LEGION-901", Role: claim.RoleImplementer, Generation: 1, State: supervise.StateWorking},
		{Token: uncertain, Project: project, Tree: "LEGION-902", Issue: "LEGION-902", Role: claim.RoleTester, Generation: 1, State: supervise.StateLaunchUncertain},
		{Token: claim.ControllerToken(project), Project: project, Role: claim.RoleController, Generation: 1, State: supervise.StateReady},
	}
	next := terminalClaims(t, project, len(claims)+1)
	mixed = append(mixed, next...)
	if err := w.replayTerminal(context.Background(), mixed, bootTimeout); err != nil {
		t.Fatalf("the replay with one claim's fact unapplied: %v", err)
	}
	facts := handler.facts()
	if len(facts) != len(claims)+1 {
		t.Fatalf("the replay with one claim's fact unapplied applied %d, want exactly that one", len(facts)-len(claims))
	}
	last := next[len(next)-1]
	if want := (intake.ClaimReady{Issue: last.Issue, Role: last.Role, Launch: last.Generation}); facts[len(facts)-1] != want {
		t.Fatalf("the replay applied %#v, want %#v", facts[len(facts)-1], want)
	}
}

// slowFactHandler is a workflow handler each of whose facts takes delay to apply, as a handler's
// reads and writes take time on a loaded database, and that counts the facts it applied.
type slowFactHandler struct {
	delay time.Duration
	mu    sync.Mutex
	seen  map[intake.Fact]int
}

func (h *slowFactHandler) Apply(ctx context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	select {
	case <-time.After(h.delay):
	case <-ctx.Done():
		return intake.Result{}, ctx.Err()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen[fact]++
	return intake.Result{}, nil
}

func (h *slowFactHandler) applied(fact intake.Fact) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[fact]
}

// A boot must not die on the number of terminal facts no daemon applied. Those it replays one at a
// time, each taking the fact lock, and each may wait there behind live facts: here 100 restored
// claims fail while the replay runs, and their failures (terminal) apply beside it. The 300
// replayed facts and the 100 live ones take several times the replay's budget between them; the
// replay still finishes, since the budget is each fact's own, and every fact, replayed or live, is
// applied exactly once.
func TestAReplayOfHundredsOfUnappliedFactsBesideLiveFailuresOutlastsItsBudget(t *testing.T) {
	const budget = 5 * time.Second
	pool := isolatedOutboxPool(t)
	handler := &slowFactHandler{delay: 20 * time.Millisecond, seen: map[intake.Fact]int{}}
	w := &workflowRuntime{pool: pool, handlers: []intake.Handler{handler}, log: quietLogger(), failed: make(chan error, 1)}
	replayed := terminalClaims(t, "legion", 300)
	failing := make([]supervise.Claim, 100)
	for i := range failing {
		issue := fmt.Sprintf("LEGION-%d", 1001+i)
		token, err := claim.NewToken("legion", issue, claim.RoleTester)
		if err != nil {
			t.Fatal(err)
		}
		failing[i] = supervise.Claim{Token: token, Project: "legion", Tree: issue, Issue: issue, Role: claim.RoleTester, Generation: 1, State: supervise.StateFailed, Failure: "pane exited"}
	}

	var live sync.WaitGroup
	live.Add(len(failing))
	started := time.Now()
	for _, c := range failing {
		go func() {
			defer live.Done()
			w.terminal(c, supervise.StateFailed)
		}()
	}
	err := w.replayTerminal(context.Background(), replayed, budget)
	took := time.Since(started)
	live.Wait()
	if err != nil {
		t.Fatalf("the replay of %d unapplied facts beside %d live failures = %v after %s, want it finished", len(replayed), len(failing), err, took)
	}
	if took <= budget {
		t.Fatalf("the replay took %s, within its budget of %s, so it proves nothing about outlasting it", took, budget)
	}
	select {
	case err := <-w.failed:
		t.Fatalf("a live failure failed to apply: %v", err)
	default:
	}
	for _, c := range replayed {
		var fact intake.Fact = intake.ClaimReady{Issue: c.Issue, Role: c.Role, Launch: c.Generation}
		if c.State == supervise.StateFailed {
			fact = intake.ClaimFailed{Issue: c.Issue, Role: c.Role, Reason: c.Failure}
		}
		if got := handler.applied(fact); got != 1 {
			t.Fatalf("replayed %#v was applied %d times, want once", fact, got)
		}
	}
	for _, c := range failing {
		if fact := (intake.ClaimFailed{Issue: c.Issue, Role: c.Role, Reason: c.Failure}); handler.applied(fact) != 1 {
			t.Fatalf("live %#v was applied %d times, want once", fact, handler.applied(fact))
		}
	}
}

// bootReconcileHeld is the fake runtime holding the boot's orphan reconciliation, the step of
// supervision.start before the terminal replay and the release of held hellos, until every shim
// the test runs has sent its hello, so no hello arrives only after the boot has settled.
type bootReconcileHeld struct {
	*fake.Runtime
	once  sync.Once
	hello <-chan struct{}
}

func (r *bootReconcileHeld) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	r.once.Do(func() {
		select {
		case <-r.hello:
		case <-ctx.Done():
		}
	})
	return r.Runtime.ReconcileOrphans(ctx, known, grace)
}

// A restart brings every live shim back at once. Each admitted hello costs the database a second
// resolve of its boot token and its machine a claim write, and each machine that then fails applies
// a fact under the fact lock, so a boot whose terminal replay ran after hellos were admitted
// replayed behind all of that, on its 30 s budget, and exited when the budget ran out. The boot
// settles first: no hello is acknowledged until the replay has applied every stored claim's ready
// fact. Here every one of 200 ready claims' shims has sent its hello before the boot's orphan
// reconciliation returns, and each, as its hello_ack arrives, finds all 200 facts applied.
func TestABootReplaysEveryTerminalFactBeforeItAdmitsAnyHello(t *testing.T) {
	const n = 200
	cfg := workflowConfig(t, workflowNATS(t))
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	bootTokens := make([]string, n)
	events := make([]string, n)
	for i := range n {
		issue := fmt.Sprintf("%s-%d", cfg.Project, i+1)
		token, err := claim.NewToken(project, issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		bootTokens[i] = "live-" + randomSuffix(t)
		c := supervise.Claim{
			Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleImplementer, Generation: 1,
			State: supervise.StateReady, Session: "ses_" + string(token), SessionFile: "/sessions/" + string(token) + ".jsonl",
			PluginContract: api.DaemonAPIVersion, BootTokenHash: supervise.HashBootToken(bootTokens[i]),
			Locator: &runtime.Locator{
				Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: fmt.Sprintf("4242:%d", i+1),
				Tmux: &runtime.TmuxLocator{Window: fmt.Sprintf("@%d", i+1), Pane: fmt.Sprintf("%%%d", i+1)},
			},
		}
		if _, err := st.OpenTreeLifecycle(ctx, project, issue, treelifecycle.AuthorityOperator); err != nil {
			t.Fatalf("open the tree of %s: %v", token, err)
		}
		if _, err := st.AdmitClaim(ctx, c); err != nil {
			t.Fatalf("put %s: %v", token, err)
		}
		events[i] = terminalEventID(c, c.State)
	}

	// The 200 shims count through a few connections of their own: a pool sized by the host's CPUs
	// would open one per shim and can exhaust the test server's max_connections.
	counting, err := pgxpool.ParseConfig(cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	counting.MaxConns = 4
	reader, err := pgxpool.NewWithConfig(ctx, counting)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	hello := make(chan struct{})
	var record built
	o := fakeRuntime(&bootReconcileHeld{Runtime: fake.NewRuntime(), hello: hello}, &record)
	o.workflowTokens = &workflowTokenRecorder{}
	type acked struct {
		conn    net.Conn
		applied int
		err     error
	}
	results := make(chan acked, n)
	var said sync.WaitGroup
	said.Add(n)
	go func() {
		said.Wait()
		close(hello)
	}()
	go func() {
		var address string
		for deadline := time.Now().Add(60 * time.Second); address == ""; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				// The daemon never built its runtime; startDaemonWithin fails the test.
				return
			}
			record.mu.Lock()
			address = record.address
			record.mu.Unlock()
		}
		for _, bootToken := range bootTokens {
			go func() {
				conn, applied, err := helloCountingApplied(address, bootToken, &said, reader, events)
				results <- acked{conn: conn, applied: applied, err: err}
			}()
		}
	}()

	startDaemonWithin(t, cfg, o, quietLogger(), 60*time.Second)

	var conns []net.Conn
	t.Cleanup(func() {
		for _, conn := range conns {
			conn.Close()
		}
	})
	var early []int
	for range n {
		got := <-results
		if got.conn != nil {
			conns = append(conns, got.conn)
		}
		if got.err != nil {
			t.Fatalf("a shim's hello: %v", got.err)
		}
		if got.applied != n {
			early = append(early, got.applied)
		}
	}
	if len(early) != 0 {
		t.Fatalf("%d of %d hellos were acknowledged before the boot replayed every terminal fact (facts applied at each of those acks: %v), want none",
			len(early), n, early)
	}
}

// helloCountingApplied dials the worker stream at address as a shim does, says hello with
// bootToken, marks said, and waits for the daemon's hello_ack; then it counts how many of events
// the store holds as applied at that moment.
func helloCountingApplied(address, bootToken string, said *sync.WaitGroup, reader *pgxpool.Pool, events []string) (net.Conn, int, error) {
	conn, err := net.Dial("unix", strings.TrimPrefix(address, "unix://"))
	if err != nil {
		said.Done()
		return nil, 0, fmt.Errorf("dial the worker stream %s: %w", address, err)
	}
	err = shimwire.NewWriter(conn).WriteFrame(shimwire.Hello2{BootToken: bootToken})
	said.Done()
	if err != nil {
		return conn, 0, fmt.Errorf("say hello: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(45 * time.Second)); err != nil {
		return conn, 0, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return conn, 0, fmt.Errorf("read the hello's answer: %w", err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		return conn, 0, fmt.Errorf("decode %q: %w", line, err)
	}
	if frame.FrameType() != shimwire.TypeHelloAck {
		return conn, 0, fmt.Errorf("the daemon answered the hello with %#v, want hello_ack", frame)
	}
	var applied int
	if err := reader.QueryRow(context.Background(), `select count(*) from processed_events where source = $1 and event_id = any($2::text[])`,
		terminalSource, events).Scan(&applied); err != nil {
		return conn, 0, fmt.Errorf("count the applied terminal facts: %w", err)
	}
	return conn, applied, nil
}
