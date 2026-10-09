package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// stopBound is how long run may take to return once its context is cancelled, given the stop's
// budget: the budget, then the stamp's and the store close's own bounds (stopBudget).
func stopBound(budget time.Duration) time.Duration {
	return budget + stampTimeout + storeCloseTimeout
}

// stoppedWaiting is the line the stop logs when its budget runs out with work still running.
const stoppedWaiting = "legion daemon stopped waiting for its work"

// stallingRuntime is a fake runtime, except that once stalling is set every launch waits in the
// runtime as an Agent Sandbox relaunch waits on a busy cluster — for the previous pod to go, for the
// tree's other pods to finish workspace-init, for the new pod — far longer than a pod's grace. Once
// stallsSuspend is set every suspension waits the same way, as a Sandbox suspension waits out its
// pod's termination grace. Each wait ends with the call's context, as every Sandbox wait does,
// unless ignoresContext makes it a call that answers to none; release, at the test's end, ends it
// either way.
type stallingRuntime struct {
	*fake.Runtime
	ignoresContext bool
	stalling       atomic.Bool
	stallsSuspend  atomic.Bool
	entered        chan claim.Token
	release        chan struct{}
}

func newStallingRuntime(t *testing.T, base *fake.Runtime, ignoresContext bool) *stallingRuntime {
	t.Helper()
	r := &stallingRuntime{
		Runtime: base, ignoresContext: ignoresContext,
		entered: make(chan claim.Token, 64), release: make(chan struct{}),
	}
	t.Cleanup(func() { close(r.release) })
	return r
}

func (r *stallingRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if r.stalling.Load() {
		if err := r.stall(ctx, "launch", spec.Claim); err != nil {
			return runtime.Locator{}, err
		}
	}
	return r.Runtime.Spawn(ctx, spec)
}

func (r *stallingRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if r.stalling.Load() {
		if err := r.stall(ctx, "launch", spec.Claim); err != nil {
			return runtime.Locator{}, err
		}
	}
	return r.Runtime.Resume(ctx, prev, spec)
}

func (r *stallingRuntime) Suspend(ctx context.Context, loc runtime.Locator) error {
	if r.stallsSuspend.Load() {
		if err := r.stall(ctx, "suspend", loc.Claim); err != nil {
			return err
		}
	}
	return r.Runtime.Suspend(ctx, loc)
}

// stall is one call waiting in the runtime: it says which claim's call waits on entered, then waits
// for the call's context, unless the runtime ignores it, or for the test's end.
func (r *stallingRuntime) stall(ctx context.Context, call string, token claim.Token) error {
	r.entered <- token
	ended := ctx.Done()
	if r.ignoresContext {
		ended = nil
	}
	select {
	case <-ended:
		return fmt.Errorf("%s %s: %w", call, token, ctx.Err())
	case <-r.release:
		return nil
	}
}

// bootReconcileStall is the stalling runtime whose boot orphan reconciliation (grace zero) waits on
// its context, as a Sandbox sweep waits on a cluster API that does not answer or a tmux reconcile on
// a wedged server.
type bootReconcileStall struct {
	*stallingRuntime
	reconciling chan struct{}
}

func (r *bootReconcileStall) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	if grace != 0 {
		return r.stallingRuntime.ReconcileOrphans(ctx, known, grace)
	}
	select {
	case r.reconciling <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// superviseMany starts a daemon over rt supervising 36 ready claims across 14 trees, the daemon
// LEGION-650 records, then has the first worker of each of the first relaunching trees lose its
// process, and returns once every one of their relaunches waits in the runtime.
func superviseMany(t *testing.T, cfg config.Config, o overrides, rt *stallingRuntime, logs *syncBuffer, relaunching int) (*daemon, []claim.Token) {
	t.Helper()
	d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(logs, nil)))
	workerRoles := []claim.Role{claim.RoleImplementer, claim.RoleReviewer}
	var all, firstWorkers []claim.Token
	for tree := 1; tree <= 14; tree++ {
		issue := fmt.Sprintf("LEGION-%d", tree)
		all = append(all, d.spawn(api.SpawnRequest{Tree: issue, Issue: issue, Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}))
		workers := 1
		if tree <= 8 {
			workers = 2
		}
		for _, role := range workerRoles[:workers] {
			all = append(all, d.spawn(api.SpawnRequest{Tree: issue, Issue: issue, Role: role, Prompt: "Reply ready and wait."}))
		}
		firstWorkers = append(firstWorkers, all[len(all)-workers])
	}
	if len(all) != 36 {
		t.Fatalf("the daemon supervises %d claims, want 36", len(all))
	}
	for _, token := range all {
		readyClaim(t, d, rt.Runtime, token)
	}

	// Every locator is read before the first relaunch, through the listing, as the operator reads
	// them.
	relaunched := firstWorkers[:relaunching]
	locators := make([]runtime.Locator, 0, relaunching)
	for _, token := range relaunched {
		locators = append(locators, *d.claim(token).Locator)
	}
	rt.stalling.Store(true)
	for i, token := range relaunched {
		rt.Emit(runtime.Observation{Locator: locators[i], Kind: runtime.Gone,
			Detail: "pod " + string(token) + " Failed: init container workspace-init terminated (Error, exit code 1)"})
	}
	waiting := map[claim.Token]bool{}
	deadline := time.After(10 * time.Second)
	for len(waiting) < relaunching {
		select {
		case token := <-rt.entered:
			waiting[token] = true
		case <-deadline:
			t.Fatalf("%d of %d relaunches reached the runtime; log:\n%s", len(waiting), relaunching, logs)
		}
	}
	return d, relaunched
}

// runLogging runs the daemon on cfg with its log written to the returned buffer, without waiting for
// it to serve, and returns what stops it: cancel, as SIGTERM does, and the channel run returns on.
func runLogging(t *testing.T, cfg config.Config, o overrides) (context.CancelFunc, chan error, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil)), o) }()
	return cancel, done, logs
}

// stopRun cancels a daemon's context, as SIGTERM does, and waits up to bound for run to return on
// done, failing the test with what the daemon logged meanwhile if it does not. It returns how long
// the stop took and that log.
func stopRun(t *testing.T, cancel context.CancelFunc, done <-chan error, logs *syncBuffer, bound time.Duration) (time.Duration, string) {
	t.Helper()
	mark := len(logs.String())
	began := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(bound):
		t.Fatalf("run had not returned %s after its context was cancelled; it logged:\n%s", bound, logs.String()[mark:])
	}
	return time.Since(began), logs.String()[mark:]
}

// stopWithin is stopRun for a daemon startDaemonLogging started.
func stopWithin(t *testing.T, d *daemon, logs *syncBuffer, bound time.Duration) (time.Duration, string) {
	t.Helper()
	d.stopped = true
	d.transport.CloseIdleConnections()
	return stopRun(t, d.cancel, d.done, logs, bound)
}

// lastBootStopped is the stopped_at of the newest boot of cfg's project, nil while none is stamped.
func lastBootStopped(t *testing.T, cfg config.Config) *time.Time {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	var stoppedAt *time.Time
	if err := st.Pool().QueryRow(ctx, "select stopped_at from daemon_boot where project = $1 order by id desc limit 1",
		cfg.Project).Scan(&stoppedAt); err != nil {
		t.Fatalf("read the newest boot of %s: %v", cfg.Project, err)
	}
	return stoppedAt
}

// logLine is the last line of log whose message is msg, or "" when there is none.
func logLine(log, msg string) string {
	line := ""
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, `"msg":"`+msg+`"`) {
			line = l
		}
	}
	return line
}

// logsStopping requires the stop's log to say the daemon is stopping before it says it stopped, and
// returns the line that says it is stopping.
func logsStopping(t *testing.T, stopLog string) string {
	t.Helper()
	stopping := strings.Index(stopLog, `"msg":"legion daemon stopping"`)
	stopped := strings.Index(stopLog, `"msg":"legion daemon stopped"`)
	if stopping < 0 || stopped < 0 || stopping > stopped {
		t.Fatalf("the stop logged no line saying the daemon is stopping before the one saying it stopped:\n%s", stopLog)
	}
	return logLine(stopLog, "legion daemon stopping")
}

// namesDeciding requires line to name each claim as deciding an event of type event, the way the
// stop's lines list the decisions in flight.
func namesDeciding(t *testing.T, line, event string, tokens ...claim.Token) {
	t.Helper()
	for _, token := range tokens {
		if want := fmt.Sprintf("%s (%s)", token, event); !strings.Contains(line, want) {
			t.Errorf("the line does not name %q among the decisions in flight:\n%s", want, line)
		}
	}
}

// neededNoBudget requires the stop to have needed none of its budget: every decision in flight
// ended with the stop's cancellation.
func neededNoBudget(t *testing.T, stopLog string) {
	t.Helper()
	if logLine(stopLog, stoppedWaiting) != "" {
		t.Errorf("the stop waited out its budget although every decision in flight answers to its cancellation:\n%s", stopLog)
	}
}

// leftDeciding requires the stop to have stopped waiting at its budget, naming each of tokens as
// deciding an event of type event among the decisions it left running.
func leftDeciding(t *testing.T, stopLog, event string, tokens ...claim.Token) {
	t.Helper()
	left := logLine(stopLog, stoppedWaiting)
	if left == "" {
		t.Fatalf("the stop did not say it stopped waiting for its work:\n%s", stopLog)
	}
	namesDeciding(t, left, event, tokens...)
}

// neverStarted requires a daemon the stop caught mid-boot never to say it started: the signal cut
// its boot short, and a started line after the stopping one would tell the operator otherwise.
func neverStarted(t *testing.T, stopLog string) {
	t.Helper()
	if logLine(stopLog, "legion daemon started") != "" {
		t.Errorf("a daemon the stop caught mid-boot said it started:\n%s", stopLog)
	}
}

// A daemon supervising many claims, told to stop while relaunches wait on the cluster, says it is
// stopping and which claims are deciding, ends those relaunches with the stop instead of waiting
// them out, and stamps its boot, well inside a pod's termination grace (LEGION-650).
func TestRunStopsInsideAPodGraceWhileRelaunchesWaitOnTheCluster(t *testing.T) {
	cfg := testConfig(t)
	rt := newStallingRuntime(t, fake.NewRuntime(), false)
	logs := &syncBuffer{}
	d, relaunching := superviseMany(t, cfg, fakeRuntime(rt, &built{}), rt, logs, 3)

	took, stopLog := stopWithin(t, d, logs, stopBound(stopBudget))

	t.Logf("stopped in %s with %d relaunches waiting in the runtime", took, len(relaunching))
	stopping := logsStopping(t, stopLog)
	for _, token := range relaunching {
		namesDeciding(t, stopping, "supervise.RuntimeObservation", token)
	}
	neededNoBudget(t, stopLog)
	if stopped := lastBootStopped(t, cfg); stopped == nil {
		t.Fatalf("the boot was not stamped stopped; the stop logged:\n%s", stopLog)
	}
}

// A launch that does not answer to its context — a call into a process that will not end — is not
// waited on past the stop's budget: the daemon names the claims it left deciding, stamps its boot
// and returns, and the next boot's re-adoption and orphan sweep take those claims up.
func TestRunStampsItsBootWithoutWaitingOutALaunchThatIgnoresTheStop(t *testing.T) {
	cfg := testConfig(t)
	rt := newStallingRuntime(t, fake.NewRuntime(), true)
	logs := &syncBuffer{}
	o := fakeRuntime(rt, &built{})
	o.stopBudget = time.Second
	d, relaunching := superviseMany(t, cfg, o, rt, logs, 3)

	took, stopLog := stopWithin(t, d, logs, stopBound(o.stopBudget))

	t.Logf("stopped in %s with %d relaunches that ignore the stop", took, len(relaunching))
	logsStopping(t, stopLog)
	leftDeciding(t, stopLog, "supervise.RuntimeObservation", relaunching...)
	if stopped := lastBootStopped(t, cfg); stopped == nil {
		t.Fatalf("the boot was not stamped stopped; the stop logged:\n%s", stopLog)
	}
}

// The operator's claim listing answers while relaunches wait on the cluster, and shows each such
// claim as its decision last recorded it: launching, at its next generation, with no process yet.
// A machine holds its lock through the runtime call its decision makes, and the listing reads every
// machine, so a daemon with a relaunch always in flight never answered it (LEGION-650).
func TestTheOperatorListingAnswersWhileRelaunchesWaitOnTheCluster(t *testing.T) {
	const listingBound = 5 * time.Second
	cfg := testConfig(t)
	rt := newStallingRuntime(t, fake.NewRuntime(), false)
	logs := &syncBuffer{}
	d, relaunching := superviseMany(t, cfg, fakeRuntime(rt, &built{}), rt, logs, 3)
	d.client.Timeout = listingBound

	began := time.Now()
	status, body := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true)
	took := time.Since(began)

	if status != http.StatusOK {
		t.Fatalf("list the claims = %d; body %s", status, body)
	}
	var list api.OperatorClaims
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode the claims %s: %v", body, err)
	}
	t.Logf("listed %d claims in %s with %d relaunches waiting in the runtime", len(list.Claims), took, len(relaunching))
	if len(list.Claims) != 36 {
		t.Fatalf("the listing holds %d claims, want 36", len(list.Claims))
	}
	for _, token := range relaunching {
		for _, c := range list.Claims {
			if c.Token == token && (c.State != "launching" || c.Generation != 2 || c.Locator != nil) {
				t.Errorf("%s is listed %s at generation %d with locator %+v, want launching at generation 2 with none yet",
					token, c.State, c.Generation, c.Locator)
			}
		}
	}
}

// A daemon told to stop while its boot relaunches a launch the previous daemon left unrecorded
// stops inside the same bound, boot included: the boot's relaunches run one after another before
// the API serves, each waiting in the runtime. One that answers to its context ends with the stop;
// one that answers to none is not waited on past the stop's budget, and the stop names it
// (LEGION-650).
func TestRunStopsInsideAPodGraceWhileItsBootRelaunchesAnUnfinishedLaunch(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ignoresContext bool
	}{
		{name: "a relaunch that ends with the stop"},
		{name: "a relaunch that ignores the stop", ignoresContext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			project, _ := claim.ProjectToken(cfg.Project)
			token, _ := claim.NewToken(project, "LEGION-4", claim.RoleReviewer)
			putClaim(t, cfg, supervise.Claim{
				Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-4", Role: claim.RoleReviewer,
				Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
			})
			writePrompt(t, cfg, token)
			rt := newStallingRuntime(t, fake.NewRuntime(), tc.ignoresContext)
			rt.stalling.Store(true)
			o := fakeRuntime(rt, &built{})
			o.stopBudget = time.Second
			cancel, done, logs := runLogging(t, cfg, o)
			select {
			case <-rt.entered:
			case err := <-done:
				t.Fatalf("run returned %v before its boot relaunched the unfinished launch; log:\n%s", err, logs)
			case <-time.After(30 * time.Second):
				t.Fatalf("the boot never relaunched the unfinished launch; log:\n%s", logs)
			}

			took, stopLog := stopRun(t, cancel, done, logs, stopBound(o.stopBudget))

			t.Logf("stopped in %s while its boot relaunched %s", took, token)
			namesDeciding(t, logsStopping(t, stopLog), "supervise.RequestSpawn", token)
			neverStarted(t, stopLog)
			if tc.ignoresContext {
				leftDeciding(t, stopLog, "supervise.RequestSpawn", token)
			} else {
				neededNoBudget(t, stopLog)
			}
			if stopped := lastBootStopped(t, cfg); stopped == nil {
				t.Fatalf("the boot was not stamped stopped; the stop logged:\n%s", stopLog)
			}
		})
	}
}

// A daemon told to stop while its boot reconciles orphans ends that reconciliation with the stop:
// the boot's own steps (restoring the claims, stopping a launched controller, the orphan sweep that
// must succeed before an unfinished launch is relaunched) wait on the cluster or the tmux server,
// and the stop's budget bounds a boot as it bounds a daemon that serves (LEGION-650).
func TestRunStopsInsideAPodGraceWhileItsBootReconcilesOrphans(t *testing.T) {
	cfg := testConfig(t)
	rt := &bootReconcileStall{stallingRuntime: newStallingRuntime(t, fake.NewRuntime(), false), reconciling: make(chan struct{}, 1)}
	o := fakeRuntime(rt, &built{})
	o.stopBudget = time.Second
	cancel, done, logs := runLogging(t, cfg, o)
	select {
	case <-rt.reconciling:
	case err := <-done:
		t.Fatalf("run returned %v before its boot reconciled orphans; log:\n%s", err, logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("the boot never reconciled orphans; log:\n%s", logs)
	}

	took, stopLog := stopRun(t, cancel, done, logs, stopBound(o.stopBudget))

	t.Logf("stopped in %s while its boot reconciled orphans", took)
	logsStopping(t, stopLog)
	neverStarted(t, stopLog)
	neededNoBudget(t, stopLog)
	if stopped := lastBootStopped(t, cfg); stopped == nil {
		t.Fatalf("the boot was not stamped stopped; the stop logged:\n%s", stopLog)
	}
}

// quietStop requires the stop to have logged nothing at WARN or ERROR: a rollout's stop cuts
// decisions short by design, and the next boot takes their claims up.
func quietStop(t *testing.T, stopLog string) {
	t.Helper()
	for _, line := range strings.Split(stopLog, "\n") {
		if strings.Contains(line, `"level":"ERROR"`) || strings.Contains(line, `"level":"WARN"`) {
			t.Errorf("the stop logged: %s", line)
		}
	}
}

// storedClaim is token's claim as the store holds it, as the next boot reads it.
func storedClaim(t *testing.T, cfg config.Config, token claim.Token) supervise.Claim {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	claims, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

// operatorRequest sends one operator request from a goroutine of its own and answers its status on
// the returned channel, -1 when it got none.
func operatorRequest(d *daemon, method, path string, body []byte) <-chan int {
	answered := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(method, d.base+path, bytes.NewReader(body))
		if err != nil {
			answered <- -1
			return
		}
		req.Header.Set("Authorization", "Bearer "+testOperatorToken)
		resp, err := d.client.Do(req)
		if err != nil {
			answered <- -1
			return
		}
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	return answered
}

// An operator's spawn waiting in the runtime when the daemon is told to stop runs until the API's
// drain ends, then ends with it inside the stop's budget, and the stop names it among the decisions
// in flight: the operator, claims and controller routes run their decisions past a client that
// hangs up, never past the drain. The failure the cut leaves is the stop's, logged at Info
// (LEGION-650).
func TestRunEndsAnOperatorSpawnInFlightWithTheDrainAndNamesIt(t *testing.T) {
	cfg := testConfig(t)
	rt := newStallingRuntime(t, fake.NewRuntime(), false)
	logs := &syncBuffer{}
	o := fakeRuntime(rt, &built{})
	o.stopBudget = 3 * time.Second
	d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(logs, nil)))
	rt.stalling.Store(true)
	body, err := json.Marshal(api.SpawnRequest{Tree: "LEGION-9", Issue: "LEGION-9", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."})
	if err != nil {
		t.Fatal(err)
	}
	answered := operatorRequest(d, http.MethodPost, "/legion/v1/operator/claims", body)
	var spawning claim.Token
	select {
	case spawning = <-rt.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("the operator's spawn never reached the runtime; log:\n%s", logs)
	}

	took, stopLog := stopWithin(t, d, logs, stopBound(o.stopBudget))

	t.Logf("stopped in %s with the operator's spawn of %s waiting in the runtime; it was answered %d", took, spawning, <-answered)
	namesDeciding(t, logsStopping(t, stopLog), "supervise.RequestSpawn", spawning)
	neededNoBudget(t, stopLog)
	quietStop(t, stopLog)
	if stopped := lastBootStopped(t, cfg); stopped == nil {
		t.Fatalf("the boot was not stamped stopped; the stop logged:\n%s", stopLog)
	}
}

// exitingSuspend is the fake runtime whose suspension has sent its shutdown frame and waits for the
// process to exit, as tmux's stop does (awaitNotRunning): it says which claim's suspension waits on
// stopping, and returns once the test lets the process exit, or with its context.
type exitingSuspend struct {
	*fake.Runtime
	stopping chan claim.Token
	exit     chan struct{}
}

func (r *exitingSuspend) Suspend(ctx context.Context, loc runtime.Locator) error {
	r.stopping <- loc.Claim
	select {
	case <-ctx.Done():
		return fmt.Errorf("suspend %s: %w", loc.Claim, ctx.Err())
	case <-r.exit:
	}
	return r.Runtime.Suspend(ctx, loc)
}

// An operator's suspension whose shutdown frame went out before the daemon was told to stop, and
// whose process exits a moment later, inside the API's drain, is recorded: the operator is answered
// and the store holds the claim suspended. Cut short at the signal, it would leave the claim stored
// live, which the next boot finds dead and relaunches, undoing the operator's request (LEGION-650).
func TestRunRecordsAnOperatorSuspensionWhoseProcessExitsWithinTheDrain(t *testing.T) {
	cfg := testConfig(t)
	rt := &exitingSuspend{Runtime: fake.NewRuntime(), stopping: make(chan claim.Token, 4), exit: make(chan struct{})}
	logs := &syncBuffer{}
	d := startDaemonLogging(t, cfg, fakeRuntime(rt, &built{}), slog.New(slog.NewJSONHandler(logs, nil)))
	token := d.spawn(api.SpawnRequest{Tree: "LEGION-9", Issue: "LEGION-9", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."})
	readyClaim(t, d, rt.Runtime, token)
	answered := operatorRequest(d, http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil)
	select {
	case <-rt.stopping:
	case <-time.After(10 * time.Second):
		t.Fatalf("the operator's suspension never reached the runtime; log:\n%s", logs)
	}

	d.stopped = true
	d.transport.CloseIdleConnections()
	mark := len(logs.String())
	d.cancel()
	testwait.Eventually(t, "the daemon to say it is stopping", func() bool {
		return strings.Contains(logs.String()[mark:], `"msg":"legion daemon stopping"`)
	})
	// The halt runs in microseconds; the agent, already told to shut down, exits a moment after.
	time.Sleep(100 * time.Millisecond)
	close(rt.exit)
	select {
	case err := <-d.done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(stopBound(stopBudget)):
		t.Fatalf("run had not returned %s after its context was cancelled; it logged:\n%s", stopBound(stopBudget), logs.String()[mark:])
	}
	stopLog := logs.String()[mark:]

	if status := <-answered; status != http.StatusOK {
		t.Errorf("the operator's suspension was answered %d, want 200; the stop logged:\n%s", status, stopLog)
	}
	if c := storedClaim(t, cfg, token); c.State != supervise.StateSuspended || c.Locator != nil {
		t.Errorf("the store holds %s with locator %+v, want suspended with none; the stop logged:\n%s", c.State, c.Locator, stopLog)
	}
	neededNoBudget(t, stopLog)
	quietStop(t, stopLog)
}
