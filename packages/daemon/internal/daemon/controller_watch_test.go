package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// syncBuffer is a log sink a daemon writes while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// notRegisteredLine is what the daemon logs, at most once per worker boot timeout, while no live
// controller holds the project's controller record. It is the whole line under `controller:
// operator` and the leading phrase of the line under `controller: daemon`, so one log query on it
// counts either mode.
const notRegisteredLine = "controller not registered; run legion controller start"

// noHolderLine is the Prober's line, once per sweep in either mode, while the Envoy role registry
// holds the controller role for nobody.
const noHolderLine = "controller liveness: the controller role has no live holder; the controller is gone"

// loggedLines decodes each line of the daemon's JSON log that holds text.
func loggedLines(t *testing.T, logs, text string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, text) {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("decode the log line %q: %v", line, err)
		}
		lines = append(lines, fields)
	}
	return lines
}

// recordControllerSession registers session as the project's controller in the store before the
// daemon boots, as a registration an earlier boot took leaves the record.
func recordControllerSession(t *testing.T, cfg config.Config, project, session string) {
	t.Helper()
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	generation, err := st.MintController(context.Background(), project, []byte("capability"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RegisterController(context.Background(), project, generation, session, []byte("secret"), time.Now()); err != nil || !ok {
		t.Fatalf("register the controller: %v %v", ok, err)
	}
}

// envoyRoleRegistry answers the controller role lookup: session holds the role, seen now, when
// alive is true, and nobody does otherwise. It counts the lookups, one per sweep that reads a
// registered controller.
func envoyRoleRegistry(t *testing.T, session string, alive bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var lookups atomic.Int64
	envoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, body := http.StatusNotFound, any(map[string]any{})
		if alive {
			code, body = http.StatusOK, map[string]any{"holder": session, "last_seen": time.Now().UnixMilli()}
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
		lookups.Add(1)
	}))
	t.Cleanup(envoy.Close)
	return envoy, &lookups
}

// Under `controller: operator` the daemon launches no controller, under either runtime, so it says
// when none is registered, or when the Envoy role registry says the registered one is gone, and how
// to start one — once per worker boot timeout, and never about a controller the registry holds
// alive. The line names the mode.
func TestTheDaemonSaysWhenNoControllerIsRegistered(t *testing.T) {
	const session = "ses_controller"
	for _, tc := range []struct {
		name     string
		register bool
		alive    bool
		want     int
	}{
		{"no controller has registered", false, false, 1},
		{"the registered session holds the role", true, true, 0},
		{"the role has no live holder", true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			envoy, lookups := envoyRoleRegistry(t, session, tc.alive)
			cfg.EnvoyURL = envoy.URL
			project, err := claim.ProjectToken(cfg.Project)
			if err != nil {
				t.Fatal(err)
			}
			if tc.register {
				recordControllerSession(t, cfg, project, session)
			}
			logs := &syncBuffer{}
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.orphanSweep = 20 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil)), o) }()
			awaitHealthz(t, cfg, done)
			// The first sweep runs as the daemon starts serving, and a due line comes from it. The
			// count then waits out further sweeps, where a second line would show: a registered
			// controller is looked up in the role registry on every sweep, so three more lookups are
			// three more sweeps; with none registered nothing is looked up, and ten intervals pass.
			if tc.want > 0 {
				testwait.Eventually(t, "the not-registered line", func() bool { return strings.Contains(logs.String(), notRegisteredLine) })
			}
			if tc.register {
				seen := lookups.Load()
				testwait.Eventually(t, "three more sweeps", func() bool { return lookups.Load() >= seen+3 })
			} else {
				time.Sleep(10 * o.orphanSweep)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("run: %v", err)
			}
			lines := loggedLines(t, logs.String(), notRegisteredLine)
			if len(lines) != tc.want {
				t.Fatalf("%q was logged %d times over many sweeps, want %d\n%s", notRegisteredLine, len(lines), tc.want, logs.String())
			}
			for _, line := range lines {
				if line["msg"] != notRegisteredLine || line["mode"] != string(config.ControllerLaunchOperator) {
					t.Errorf("the not-registered line = %v, want exactly %q with mode operator", line, notRegisteredLine)
				}
			}
		})
	}
}

// checkDaemonModeLine fails the test unless line is the not-registered line of a daemon that launches
// its own controller: the operator's text leading this mode's remedy, the mode, and the state of the
// controller's claim as the store holds it, which is never a live controller's (empty before the
// daemon has created the claim).
func checkDaemonModeLine(t *testing.T, line map[string]any) {
	t.Helper()
	msg, _ := line["msg"].(string)
	state, isString := line["claimState"].(string)
	live := state == string(supervise.StateReady) || state == string(supervise.StateWorking) || state == string(supervise.StateIdle)
	if !strings.HasPrefix(msg, notRegisteredLine) || !strings.Contains(msg, "controller: operator") ||
		line["mode"] != string(config.ControllerLaunchDaemon) || !isString || live {
		t.Errorf("the not-registered line = %v, want %q leading a remedy naming controller: operator, mode daemon, and the state of a claim that is not live",
			line, notRegisteredLine)
	}
}

// Under `controller: daemon` the daemon launches the controller itself, and the same sweep watches
// it on the same cadence: while its launch has not registered, or the Envoy role registry says the
// session it registered is gone (a launch that died, its claim relaunching or waiting out the
// keeper's backoff), the daemon logs the operator's line with the remedy this mode has, and the
// Prober its no-holder line on every sweep, so a log query on either line counts both modes. The
// lines name the mode, and the not-registered line the state of the controller's claim, so a
// reader tells a launch in flight or a backoff from a death. A controller that holds the role is
// not reported, neither the session recorded before its launch registers nor the launch once it has.
func TestTheDaemonSaysWhenItsOwnControllerIsNotRegistered(t *testing.T) {
	const session = "ses_controller"
	for _, tc := range []struct {
		name string
		// recorded is session registered before this boot, as a launch an earlier boot ran leaves
		// the record.
		recorded bool
		// alive is the Envoy role registry holding the role for session.
		alive bool
		// register is this boot's launch registering as session, as a resumed controller does.
		register     bool
		wantWarn     int
		wantNoHolder bool
	}{
		{name: "its launch has not registered", wantWarn: 1},
		{name: "the session it registered has no live holder", recorded: true, wantWarn: 1, wantNoHolder: true},
		{name: "its launch registered and holds the role", recorded: true, alive: true, register: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.ControllerLaunch = config.ControllerLaunchDaemon
			envoy, lookups := envoyRoleRegistry(t, session, tc.alive)
			cfg.EnvoyURL = envoy.URL
			project, err := claim.ProjectToken(cfg.Project)
			if err != nil {
				t.Fatal(err)
			}
			if tc.recorded {
				recordControllerSession(t, cfg, project, session)
			}
			logs := &syncBuffer{}
			rt := fake.NewRuntime()
			var record built
			o := fakeRuntime(rt, &record)
			o.orphanSweep = 20 * time.Millisecond
			d := startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(logs, nil)))
			launch := controllerLaunched(t, rt, claim.ControllerToken(project))
			if tc.register {
				// Before the launch registers, sweeps read the recorded session alive and say nothing.
				testwait.Eventually(t, "three sweeps before the launch registers", func() bool { return lookups.Load() >= 3 })
				if got := strings.Count(logs.String(), notRegisteredLine); got != 0 {
					t.Fatalf("%q was logged %d times while the recorded session held the role, want 0\n%s", notRegisteredLine, got, logs.String())
				}
				registeredLaunch(t, d, &record, launch, session)
				// The registration writes the record in two statements, a fresh capability and then
				// the session, so a sweep that reads it between them says once that none is
				// registered. Two more lookups put a whole sweep after the registration; the count
				// starts there, and a sweep reading the registered session alive has cleared the
				// once-per-boot-timeout wait, so a wrong verdict after it would show at once.
				seen := lookups.Load()
				testwait.Eventually(t, "a whole sweep after the registration", func() bool { return lookups.Load() >= seen+2 })
				logs.Reset()
			}
			// As above: a due line comes from the first sweep, and the count waits out further
			// sweeps where a second would show.
			if tc.wantWarn > 0 {
				testwait.Eventually(t, "the not-registered line", func() bool { return strings.Contains(logs.String(), notRegisteredLine) })
			}
			if tc.recorded {
				seen := lookups.Load()
				testwait.Eventually(t, "three more sweeps", func() bool { return lookups.Load() >= seen+3 })
			} else {
				time.Sleep(10 * o.orphanSweep)
			}
			d.stop()
			warns := loggedLines(t, logs.String(), notRegisteredLine)
			if len(warns) != tc.wantWarn {
				t.Fatalf("%q was logged %d times over many sweeps, want %d\n%s", notRegisteredLine, len(warns), tc.wantWarn, logs.String())
			}
			for _, line := range warns {
				checkDaemonModeLine(t, line)
			}
			noHolder := loggedLines(t, logs.String(), noHolderLine)
			if tc.wantNoHolder != (len(noHolder) >= 3) || !tc.wantNoHolder && len(noHolder) > 0 {
				t.Fatalf("%q was logged %d times over many sweeps; want it on every sweep: %t\n%s", noHolderLine, len(noHolder), tc.wantNoHolder, logs.String())
			}
			for _, line := range noHolder {
				if line["mode"] != string(config.ControllerLaunchDaemon) {
					t.Errorf("the no-holder line = %v, want mode daemon", line)
				}
			}
		})
	}
}

// stalledLaunches is a runtime whose launches of the controller do not return until release closes,
// as a launch whose Sandbox never shows its pod waits out its bound, holding the controller's
// machine all the while. started is set once such a launch has begun.
type stalledLaunches struct {
	*fake.Runtime
	started *atomic.Bool
	release chan struct{}
}

func (r stalledLaunches) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.Role == claim.RoleController {
		r.started.Store(true)
		select {
		case <-r.release:
		case <-ctx.Done():
			return runtime.Locator{}, ctx.Err()
		}
	}
	return r.Runtime.Spawn(ctx, spec)
}

// The sweep never waits on the controller's launch: while a launch of the daemon's own controller
// hangs, holding its machine, the daemon still says on its cadence that no controller is registered.
// The machines' timers never fire here (stillClock), so a short worker boot timeout spaces the line
// a few sweeps apart and nothing else; one line after the launch began shows the sweep did not wait.
func TestTheDaemonSaysItsControllerIsNotRegisteredWhileItsLaunchHangs(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	cfg.WorkerBootTimeout = 100 * time.Millisecond
	envoy, _ := envoyRoleRegistry(t, "ses_controller", false)
	cfg.EnvoyURL = envoy.URL
	stalled := stalledLaunches{Runtime: fake.NewRuntime(), started: &atomic.Bool{}, release: make(chan struct{})}
	o := fakeRuntime(stalled.Runtime, &built{})
	build := o.runtime
	o.runtime = func(ctx context.Context, conns runtime.Conns, address string, apps appauth.Tokens, removable func(ctx context.Context, tree, exclude string) ([]runtime.RemovableWorkspace, error)) (runtime.Runtime, error) {
		if _, err := build(ctx, conns, address, apps, removable); err != nil {
			return nil, err
		}
		return stalled, nil
	}
	o.orphanSweep = 20 * time.Millisecond
	logs := &syncBuffer{}
	startDaemonLogging(t, cfg, o, slog.New(slog.NewJSONHandler(logs, nil)))
	// Registered after the daemon's own cleanup, so it runs first and the launch returns before the
	// daemon is stopped.
	t.Cleanup(func() { close(stalled.release) })
	testwait.Eventually(t, "the controller's launch to begin", stalled.started.Load)
	mark := len(logs.String())
	testwait.Eventually(t, "a not-registered line after the launch began", func() bool {
		return strings.Contains(logs.String()[mark:], notRegisteredLine)
	})
	for _, line := range loggedLines(t, logs.String(), notRegisteredLine) {
		checkDaemonModeLine(t, line)
	}
}
