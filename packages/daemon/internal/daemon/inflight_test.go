package daemon

import (
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
)

// stopBound is the share of a pod's 30-second termination grace a daemon's stop may take: a third,
// so a daemon on a loaded node still stamps its boot and exits before it is killed.
const stopBound = 10 * time.Second

// stallingRuntime is the fake runtime, except that once stalling is set every launch waits in the
// runtime as an Agent Sandbox relaunch waits on a busy cluster — for the previous pod to go, for the
// tree's other pods to finish workspace-init, for the new pod — far longer than a pod's grace: a
// relaunch on the production cluster took 169 s on 2026-10-08. Once stallsSuspend is set every
// suspension waits the same way, as a Sandbox suspension waits out its pod's termination grace.
// The wait ends with the call's context, as every Sandbox wait does, unless ignoresContext makes it
// a call that answers to none; release, at the test's end, ends it either way.
type stallingRuntime struct {
	*fake.Runtime
	ignoresContext bool
	stalling       atomic.Bool
	stallsSuspend  atomic.Bool
	entered        chan claim.Token
	release        chan struct{}
}

func newStallingRuntime(t *testing.T, ignoresContext bool) *stallingRuntime {
	t.Helper()
	r := &stallingRuntime{
		Runtime: fake.NewRuntime(), ignoresContext: ignoresContext,
		entered: make(chan claim.Token, 64), release: make(chan struct{}),
	}
	t.Cleanup(func() { close(r.release) })
	return r
}

func (r *stallingRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := r.stall(ctx, spec.Claim); err != nil {
		return runtime.Locator{}, err
	}
	return r.Runtime.Spawn(ctx, spec)
}

func (r *stallingRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := r.stall(ctx, spec.Claim); err != nil {
		return runtime.Locator{}, err
	}
	return r.Runtime.Resume(ctx, prev, spec)
}

func (r *stallingRuntime) Suspend(ctx context.Context, loc runtime.Locator) error {
	if r.stallsSuspend.Load() {
		r.entered <- loc.Claim
		select {
		case <-ctx.Done():
			return fmt.Errorf("suspend %s: %w", loc.Claim, ctx.Err())
		case <-r.release:
		}
	}
	return r.Runtime.Suspend(ctx, loc)
}

func (r *stallingRuntime) stall(ctx context.Context, token claim.Token) error {
	if !r.stalling.Load() {
		return nil
	}
	r.entered <- token
	ended := ctx.Done()
	if r.ignoresContext {
		ended = nil
	}
	select {
	case <-ended:
		return fmt.Errorf("launch %s: %w", token, ctx.Err())
	case <-r.release:
		return nil
	}
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

	// Every locator is read before the first relaunch: a machine holds its lock through the
	// runtime call its decision makes, and the operator's listing reads every machine.
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

// stopWithin cancels the daemon's context, as SIGTERM does, and waits up to bound for run to
// return, failing the test with what the daemon logged meanwhile if it does not. It returns how
// long the stop took and that log.
func stopWithin(t *testing.T, d *daemon, logs *syncBuffer, bound time.Duration) (time.Duration, string) {
	t.Helper()
	d.stopped = true
	d.transport.CloseIdleConnections()
	mark := len(logs.String())
	began := time.Now()
	d.cancel()
	select {
	case err := <-d.done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(bound):
		t.Fatalf("run had not returned %s after its context was cancelled; it logged:\n%s", bound, logs.String()[mark:])
	}
	return time.Since(began), logs.String()[mark:]
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

// logsStopping requires the stop's log to say the daemon is stopping before it says it stopped.
func logsStopping(t *testing.T, stopLog string) {
	t.Helper()
	stopping := strings.Index(stopLog, `"msg":"legion daemon stopping"`)
	stopped := strings.Index(stopLog, `"msg":"legion daemon stopped"`)
	if stopping < 0 || stopped < 0 || stopping > stopped {
		t.Fatalf("the stop logged no line saying the daemon is stopping before the one saying it stopped:\n%s", stopLog)
	}
}

// A daemon supervising many claims, told to stop while relaunches wait on the cluster, says it is
// stopping, ends those relaunches with the stop instead of waiting them out, and stamps its boot,
// well inside a pod's termination grace (LEGION-650: such a daemon logged nothing for 90 s after
// SIGTERM and was killed, its boot never stamped).
func TestRunStopsInsideAPodGraceWhileRelaunchesWaitOnTheCluster(t *testing.T) {
	cfg := testConfig(t)
	rt := newStallingRuntime(t, false)
	logs := &syncBuffer{}
	d, relaunching := superviseMany(t, cfg, fakeRuntime(rt, &built{}), rt, logs, 3)

	took, stopLog := stopWithin(t, d, logs, stopBound)

	t.Logf("stopped in %s with %d relaunches waiting in the runtime", took, len(relaunching))
	logsStopping(t, stopLog)
	if stopped := lastBootStopped(t, cfg); stopped == nil {
		t.Fatalf("the boot was not stamped stopped; the stop logged:\n%s", stopLog)
	}
}

// A launch that does not answer to its context — a call into a process that will not end — is not
// waited on past the stop's budget: the daemon names the claims it left deciding, stamps its boot
// and returns, and the next boot's re-adoption and orphan sweep take those claims up.
func TestRunStampsItsBootWithoutWaitingOutALaunchThatIgnoresTheStop(t *testing.T) {
	cfg := testConfig(t)
	rt := newStallingRuntime(t, true)
	logs := &syncBuffer{}
	o := fakeRuntime(rt, &built{})
	o.stopBudget = time.Second
	d, relaunching := superviseMany(t, cfg, o, rt, logs, 3)

	took, stopLog := stopWithin(t, d, logs, stopBound)

	t.Logf("stopped in %s with %d relaunches that ignore the stop", took, len(relaunching))
	logsStopping(t, stopLog)
	left := ""
	for _, line := range strings.Split(stopLog, "\n") {
		if strings.Contains(line, `"msg":"legion daemon stopped waiting for its work"`) {
			left = line
		}
	}
	for _, token := range relaunching {
		if !strings.Contains(left, string(token)) {
			t.Errorf("the stop did not name %s among the claims it left deciding; the stop logged:\n%s", token, stopLog)
		}
	}
	if stopped := lastBootStopped(t, cfg); stopped == nil {
		t.Fatalf("the boot was not stamped stopped; the stop logged:\n%s", stopLog)
	}
}

// The operator's claim listing answers while relaunches wait on the cluster, and shows each such
// claim as its decision last recorded it: launching, at its next generation, with no process yet.
// A machine holds its lock through the runtime call its decision makes, and the listing reads every
// machine, so a daemon with a relaunch always in flight answered it never (LEGION-650: the cluster
// daemon's listing sent no byte in 60 s, three tries, while GET /legion/v1/state answered in 0.3 s).
func TestTheOperatorListingAnswersWhileRelaunchesWaitOnTheCluster(t *testing.T) {
	const listingBound = 5 * time.Second
	cfg := testConfig(t)
	rt := newStallingRuntime(t, false)
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
// the API serves, each waiting in the runtime, and the stop cuts them short as it cuts any
// decision short (LEGION-650).
func TestRunStopsInsideAPodGraceWhileItsBootRelaunchesAnUnfinishedLaunch(t *testing.T) {
	cfg := testConfig(t)
	project, _ := claim.ProjectToken(cfg.Project)
	token, _ := claim.NewToken(project, "LEGION-4", claim.RoleReviewer)
	putClaim(t, cfg, supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-4", Role: claim.RoleReviewer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	})
	writePrompt(t, cfg, token)
	rt := newStallingRuntime(t, false)
	rt.stalling.Store(true)
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil)), fakeRuntime(rt, &built{})) }()
	select {
	case <-rt.entered:
	case err := <-done:
		t.Fatalf("run returned %v before its boot relaunched the unfinished launch; log:\n%s", err, logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("the boot never relaunched the unfinished launch; log:\n%s", logs)
	}

	mark := len(logs.String())
	began := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(stopBound):
		t.Fatalf("run had not returned %s after its context was cancelled mid-boot; it logged:\n%s", stopBound, logs.String()[mark:])
	}
	t.Logf("stopped in %s while its boot relaunched %s", time.Since(began), token)
	logsStopping(t, logs.String()[mark:])
	if stopped := lastBootStopped(t, cfg); stopped == nil {
		t.Fatalf("the boot was not stamped stopped; the stop logged:\n%s", logs.String()[mark:])
	}
}
