package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// putUnfinished stores a launch the previous daemon began and never recorded, on issue, with the
// role prompt its relaunch reads, and returns its claim and the boot token its generation-1 process
// holds.
func putUnfinished(t *testing.T, cfg config.Config, issue string, role claim.Role) (claim.Token, string) {
	t.Helper()
	project, _ := claim.ProjectToken(cfg.Project)
	token, _ := claim.NewToken(project, issue, role)
	bootToken := "interrupted-" + randomSuffix(t)
	putClaim(t, cfg, supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-1", Issue: issue, Role: role,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken(bootToken),
	})
	writePrompt(t, cfg, token)
	return token, bootToken
}

// newTestShim is the test's shim on conn, closed when the test ends.
func newTestShim(t *testing.T, conn net.Conn) *shim {
	t.Helper()
	t.Cleanup(func() { conn.Close() })
	return &shim{t: t, conn: conn, lines: bufio.NewReader(conn), writer: shimwire.NewWriter(conn)}
}

// registerReady registers the agent a launch started and has it say it is ready, as a relaunched
// worker's agent does once its shim is admitted.
func registerReady(t *testing.T, d *daemon, spec runtime.SpawnSpec) {
	t.Helper()
	session := "ses_" + string(spec.Claim)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: spec.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + string(spec.Claim) + ".jsonl",
		AgentID: "agent", PluginContract: 1,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s at generation %d = %d; body %s", spec.Claim, spec.Generation, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration: %v", err)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: spec.Claim, SessionID: session, Secret: registered.Secret, Generation: spec.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s at generation %d = %d; body %s", spec.Claim, spec.Generation, status, body)
	}
}

// wantRelaunchedReady requires token's claim to be ready at its relaunch's generation, 2, with no
// launch failure charged.
func wantRelaunchedReady(t *testing.T, d *daemon, token claim.Token) {
	t.Helper()
	if c := d.claim(token); c.State != string(supervise.StateReady) || c.Generation != 2 || c.Budgets.LaunchFailures != 0 {
		t.Fatalf("%s is %s at generation %d with %d launch failures, want ready at generation 2 with none",
			token, c.State, c.Generation, c.Budgets.LaunchFailures)
	}
}

// probeClient is a readiness probe's: a second per probe, and no connection kept between probes,
// so none is left open across the daemon's stop.
var probeClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}

// answersHealthz is whether the daemon at base answers /healthz with 200 now, as a probe asks.
func answersHealthz(base string) bool {
	response, err := probeClient.Get(base + "/healthz")
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// clusterRoutedRuntime is the fake runtime launching as an Agent Sandbox relaunch does when the
// daemon runs in the cluster behind a Service (advertise_host): the launch returns only once the
// role's launcher has dialled the daemon's worker stream (sandbox's awaitRoleLauncher), and the
// launcher reaches the daemon only through that Service, which routes to the daemon's pod only once
// the pod is Ready: once the daemon answers /healthz, the path its probes read. A launch whose
// launcher cannot reach the daemon within wait fails as the Sandbox's does. A launch that returns
// has had its launcher start the role's worker, reported on started for the test to play.
type clusterRoutedRuntime struct {
	*fake.Runtime
	base     string
	wait     time.Duration
	timedOut atomic.Int32
	started  chan runtime.SpawnSpec
}

func (r *clusterRoutedRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	deadline := time.Now().Add(r.wait)
	for !answersHealthz(r.base) {
		if time.Now().After(deadline) {
			r.timedOut.Add(1)
			return runtime.Locator{}, fmt.Errorf("launch %s: wait for its role launcher: timed out after %s waiting for role launcher %s",
				spec.Claim, r.wait, spec.Claim)
		}
		select {
		case <-ctx.Done():
			return runtime.Locator{}, fmt.Errorf("launch %s: wait for its role launcher: %w", spec.Claim, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	loc, err := r.Runtime.Spawn(ctx, spec)
	if err == nil {
		r.started <- spec
	}
	return loc, err
}

// A daemon restarted while a launch was in flight answers /healthz before it relaunches that
// launch, and the relaunch registers. A relaunched Sandbox worker reaches the daemon only once it
// answers /healthz (its launcher dials the daemon's Service), and its shim's hello only once
// restoration has closed. Relaunched inside the boot, each attempt waited for a daemon that answers
// only once the boot ends: the boot spent launch_failure_limit × worker_boot_timeout_seconds per
// unfinished launch with its API unanswered, past a Deployment's progress deadline, and the claim
// failed (LEGION-653).
func TestABootAnswersHealthzBeforeItRelaunchesAnUnfinishedLaunchAndTheRelaunchRegisters(t *testing.T) {
	cfg := testConfig(t)
	token, _ := putUnfinished(t, cfg, "LEGION-3", claim.RolePlanner)
	// One wait of the launcher bounds the boot: a boot that waits for the relaunch answers only
	// after launch_failure_limit (3) of them.
	const launcherWait = 5 * time.Second
	rt := &clusterRoutedRuntime{
		Runtime: fake.NewRuntime(), base: "http://127.0.0.1:" + strconv.Itoa(cfg.Port), wait: launcherWait,
		started: make(chan runtime.SpawnSpec, 4),
	}
	var record built

	d := startDaemonWithin(t, cfg, fakeRuntime(rt, &record), quietLogger(), launcherWait)

	if failed := rt.timedOut.Load(); failed != 0 {
		t.Fatalf("the daemon answered /healthz only after %d relaunch attempts timed out waiting for it", failed)
	}
	var spec runtime.SpawnSpec
	select {
	case spec = <-rt.started:
	case <-time.After(2 * launcherWait):
		t.Fatalf("the unfinished launch was never relaunched; the runtime saw %v", rt.Methods())
	}
	if spec.Claim != token || spec.Generation != 2 {
		t.Fatalf("the runtime started %s at generation %d, want %s at generation 2", spec.Claim, spec.Generation, token)
	}
	dialShim(t, record.address, spec.BootToken)
	registerReady(t, d, spec)
	wantRelaunchedReady(t, d, token)
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 1 {
		t.Fatalf("spawns = %d, want the one relaunch", len(spawns))
	}
}

// coldStartRuntime is the fake runtime whose launches each wait in the runtime, as an Agent Sandbox
// relaunch waits minutes for a cold pod (the previous pod to go, workspace-init, the new pod), until
// the test lets them through by closing warm. Each launch reports its spec on entered as its wait
// begins, boot token included, so the test can play the worker its launcher starts.
type coldStartRuntime struct {
	*fake.Runtime
	entered chan runtime.SpawnSpec
	warm    chan struct{}
}

func (r *coldStartRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.entered <- spec
	select {
	case <-ctx.Done():
		return runtime.Locator{}, fmt.Errorf("launch %s: %w", spec.Claim, ctx.Err())
	case <-r.warm:
	}
	return r.Runtime.Spawn(ctx, spec)
}

// A daemon restarted with several launches in flight waits for none of them: it answers /healthz
// while every relaunch still waits on its cold start, the relaunches wait side by side rather than
// one after another, and a relaunched worker's shim is admitted while the others still wait. Each
// then registers. Relaunched one after another inside the boot, every unfinished launch added its
// whole cold start, or its whole launch budget, to the time before the API answered, so two could
// outlast a Deployment's progress deadline whatever worker_boot_timeout_seconds is (LEGION-653).
func TestABootWaitsForNoneOfItsUnfinishedLaunches(t *testing.T) {
	cfg := testConfig(t)
	tokens := []claim.Token{}
	for _, issue := range []string{"LEGION-3", "LEGION-4"} {
		token, _ := putUnfinished(t, cfg, issue, claim.RolePlanner)
		tokens = append(tokens, token)
	}
	rt := &coldStartRuntime{Runtime: fake.NewRuntime(), entered: make(chan runtime.SpawnSpec, 8), warm: make(chan struct{})}
	var warmed sync.Once
	letThrough := func() { warmed.Do(func() { close(rt.warm) }) }
	t.Cleanup(letThrough)
	var record built

	d := startDaemonWithin(t, cfg, fakeRuntime(rt, &record), quietLogger(), 10*time.Second)

	waiting := map[claim.Token]runtime.SpawnSpec{}
	deadline := time.After(10 * time.Second)
	for len(waiting) < len(tokens) {
		select {
		case spec := <-rt.entered:
			waiting[spec.Claim] = spec
		case <-deadline:
			t.Fatalf("%d of %d relaunches wait in the runtime at once, want every one: they are run one after another",
				len(waiting), len(tokens))
		}
	}
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 0 {
		t.Fatalf("%d relaunches left the runtime before it let them through", len(spawns))
	}
	// The first relaunch's worker says hello while both relaunches still wait: its hello is not
	// held behind the other's cold start.
	first := waiting[tokens[0]]
	dialShim(t, record.address, first.BootToken)

	letThrough()
	for _, token := range tokens {
		spec := waiting[token]
		if spec.Generation != 2 {
			t.Fatalf("%s relaunched at generation %d, want 2", token, spec.Generation)
		}
		testwait.Eventually(t, "the relaunch of "+string(token)+" to record its process", func() bool {
			c := d.claim(token)
			return c.State != string(supervise.StateQueued) && c.Locator != nil
		})
		registerReady(t, d, spec)
		wantRelaunchedReady(t, d, token)
	}
}

// stopsEarlierRuntime is the fake runtime whose launch of a claim first ends the claim's earlier
// process the test registered in earlier, as an Agent Sandbox relaunch stops a launcher's earlier
// generation before it starts the next (relaunch.go): the process ends, and its shim's connection
// with it.
type stopsEarlierRuntime struct {
	*fake.Runtime
	mu      sync.Mutex
	earlier map[claim.Token]net.Conn
}

func (r *stopsEarlierRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.mu.Lock()
	if conn, ok := r.earlier[spec.Claim]; ok {
		_ = conn.Close()
		delete(r.earlier, spec.Claim)
	}
	r.mu.Unlock()
	return r.Runtime.Spawn(ctx, spec)
}

// dialShimUntilAcked dials as worker-shim does, again after a refused hello until one is acked,
// and returns the acknowledged connection: a claim's stream slot is free again once the listener
// has read its previous connection's close.
func dialShimUntilAcked(t *testing.T, address, bootToken string) *shim {
	t.Helper()
	var last error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		conn, err := net.Dial("unix", strings.TrimPrefix(address, "unix://"))
		if err != nil {
			last = err
			continue
		}
		s := newTestShim(t, conn)
		if err := s.writer.WriteFrame(shimwire.Hello2{BootToken: bootToken}); err != nil {
			conn.Close()
			last = err
			continue
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("set the read deadline: %v", err)
		}
		line, err := s.lines.ReadBytes('\n')
		if err == nil {
			if frame, decodeErr := shimwire.Decode(line); decodeErr == nil && frame.FrameType() == shimwire.TypeHelloAck {
				return s
			}
		}
		conn.Close()
		last = fmt.Errorf("hello refused: %v", err)
	}
	t.Fatalf("no hello with %s was acked: %v", bootToken, last)
	return nil
}

// What restoration's gate keeps from a hello, the boot still keeps. A hello still waits until every
// stored claim has its machine (TestHelloResolverHoldsAKnownTokenUntilRestoredRatherThanRejectingItAfterTheTimeout),
// but no longer until the boot's relaunches have written their next generation, so the pre-restart
// process of an unfinished launch, still running, can be admitted before its claim relaunches.
// Admitted, it moves nothing: its claim, launch_uncertain until the release, ignores its hello and
// refuses its registration. Its connection holds the claim's stream slot only while its process
// lives, and the relaunch ends that process before it starts the next generation, so the next
// generation's hello is admitted and registers, and the old process's token is refused as stale.
// The store refuses the release at first, which holds the window open: a successful boot
// reconciliation, restoration closed, the release retried at each orphan sweep.
func TestAPreRestartProcessAdmittedBeforeItsRelaunchNeitherMovesItsClaimNorLocksOutTheNext(t *testing.T) {
	cfg := testConfig(t)
	token, oldBootToken := putUnfinished(t, cfg, "LEGION-3", claim.RolePlanner)
	letRelease := refuseRelease(t, cfg, token)
	rt := &stopsEarlierRuntime{Runtime: fake.NewRuntime(), earlier: map[claim.Token]net.Conn{}}
	var record built
	o := fakeRuntime(rt, &record)
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)

	old := dialShim(t, record.address, oldBootToken)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: oldBootToken, SessionID: "old-session", OmpSessionFile: "/sessions/old.jsonl", AgentID: "old-agent", PluginContract: 1,
	}, false)
	if status != http.StatusConflict || !strings.Contains(string(body), "previous launch") {
		t.Fatalf("the pre-restart process's registration = %d %s, want a named 409", status, body)
	}
	// The store, not the listing: the listing's view shows a release in flight as queued, and the
	// refused release is retried at every 20 ms sweep.
	if c := storedClaim(t, cfg, token); c.State != supervise.StateLaunchUncertain || c.Locator != nil || c.Generation != 1 {
		t.Fatalf("the admitted pre-restart hello left the claim stored %s at generation %d with locator %+v, want launch_uncertain at 1 with none",
			c.State, c.Generation, c.Locator)
	}
	rt.mu.Lock()
	rt.earlier[token] = old.conn
	rt.mu.Unlock()

	letRelease()
	testwait.Eventually(t, "the claim to relaunch once the store takes its release", func() bool {
		c := d.claim(token)
		return c.State == string(supervise.StateLaunching) && c.Locator != nil
	})
	spec := lastLaunch(t, rt.Runtime, token)
	if spec.Generation != 2 {
		t.Fatalf("the relaunch is at generation %d, want 2", spec.Generation)
	}
	dialShimUntilAcked(t, record.address, spec.BootToken)
	registerReady(t, d, spec)
	wantRelaunchedReady(t, d, token)

	stale := dialRaw(t, record.address)
	stale.send(shimwire.Hello2{BootToken: oldBootToken})
	stale.closed()
}

// dialRaw connects to the worker stream at address and says nothing yet.
func dialRaw(t *testing.T, address string) *shim {
	t.Helper()
	conn, err := net.Dial("unix", strings.TrimPrefix(address, "unix://"))
	if err != nil {
		t.Fatalf("dial the worker stream %s: %v", address, err)
	}
	return newTestShim(t, conn)
}
