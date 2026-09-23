package tmux

// The real-tmux tests. Each test gets a private tmux server of its own (its own socket name under
// its own TMUX_TMPDIR), the real `legion worker-shim` built from cmd/legion, the stand-in OMP built
// from testdata, an httptest daemon serving the two claim routes the stand-in calls, and a
// unix-socket stub standing in for the worker stream listener. They skip, naming tmux, where tmux
// is not on PATH.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

var (
	standinOnce sync.Once
	standinDir  string
	standinErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if standinDir != "" {
		os.RemoveAll(standinDir)
	}
	os.Exit(code)
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not on PATH: the real-tmux runtime tests need a tmux binary")
	}
}

// standins builds the real `legion` (whose worker-shim every pane runs) and the stand-in OMP (as
// `omp`), once per run.
func standins(t *testing.T) (legion, omp string) {
	t.Helper()
	standinOnce.Do(func() {
		if standinDir, standinErr = os.MkdirTemp("", "lgt-bin"); standinErr != nil {
			return
		}
		for name, source := range map[string]string{"legion": "github.com/sjawhar/legion/daemon/cmd/legion", "omp": "testdata/omp-standin.go"} {
			if out, err := exec.Command("go", "build", "-o", filepath.Join(standinDir, name), source).CombinedOutput(); err != nil {
				standinErr = fmt.Errorf("build %s: %v\n%s", source, err, out)
				return
			}
		}
	})
	if standinErr != nil {
		t.Fatal(standinErr)
	}
	return filepath.Join(standinDir, "legion"), filepath.Join(standinDir, "omp")
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

// bootClaim is what a boot token stands for.
type bootClaim struct {
	token      claim.Token
	tree       string
	issue      string
	role       claim.Role
	generation uint64
}

// fakeDaemon serves POST /legion/v1/claims/register and /ready as the real routes will: the boot
// token resolves to a claim or the registration is refused.
type fakeDaemon struct {
	server    *httptest.Server
	mu        sync.Mutex
	claims    map[string]bootClaim
	registers []claim.RegisterRequest
	readies   []claim.ReadyRequest
}

func newFakeDaemon() *fakeDaemon {
	d := &fakeDaemon{claims: map[string]bootClaim{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /legion/v1/claims/register", func(w http.ResponseWriter, req *http.Request) {
		var body claim.RegisterRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c, ok := d.resolve(body.BootToken)
		if !ok {
			w.WriteHeader(claim.InvalidBootToken.Status)
			_ = json.NewEncoder(w).Encode(claim.InvalidBootToken)
			return
		}
		d.mu.Lock()
		d.registers = append(d.registers, body)
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(claim.RegisterResponse{
			ClaimToken: c.token, Tree: c.tree, Issue: c.issue, Role: c.role, Generation: c.generation, Secret: "capability",
		})
	})
	mux.HandleFunc("POST /legion/v1/claims/ready", func(w http.ResponseWriter, req *http.Request) {
		var body claim.ReadyRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.readies = append(d.readies, body)
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	d.server = httptest.NewServer(mux)
	return d
}

func (d *fakeDaemon) resolve(bootToken string) (bootClaim, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.claims[bootToken]
	return c, ok
}

// awaitReady waits for the agent holding bootToken to register and then declare itself ready,
// returning its registration.
func (d *fakeDaemon) awaitReady(t *testing.T, bootToken string) claim.RegisterRequest {
	t.Helper()
	var registration claim.RegisterRequest
	eventually(t, 20*time.Second, "the agent to register and declare ready", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, r := range d.registers {
			if r.BootToken == bootToken {
				registration = r
				for _, ready := range d.readies {
					if ready.SessionID == r.SessionID {
						return true
					}
				}
			}
		}
		return false
	})
	return registration
}

// streamStub stands in for the worker stream listener: it takes a shim's hello, resolves the boot
// token, answers hello_ack, and files the connection under its claim — runtime.Conns over real
// sockets, without internal/stream.
type streamStub struct {
	listener net.Listener
	daemon   *fakeDaemon
	mu       sync.Mutex
	conns    map[claim.Token]*stubConn
}

var _ runtime.Conns = (*streamStub)(nil)

func newStreamStub(t *testing.T, path string, daemon *fakeDaemon) *streamStub {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := &streamStub{listener: listener, daemon: daemon, conns: map[claim.Token]*stubConn{}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.admit(conn)
		}
	}()
	return s
}

func (s *streamStub) admit(conn net.Conn) {
	reader := shimwire.NewReader(conn)
	line, err := reader.ReadLine()
	if err != nil {
		conn.Close()
		return
	}
	frame, err := shimwire.Decode(line)
	hello, ok := frame.(shimwire.Hello)
	if err != nil || !ok {
		conn.Close()
		return
	}
	c, ok := s.daemon.resolve(hello.BootToken)
	if !ok {
		conn.Close()
		return
	}
	stub := &stubConn{
		writer:  shimwire.NewWriter(conn),
		pending: map[string]chan shimwire.Response{},
		events:  make(chan string, 64),
		done:    make(chan struct{}),
	}
	if err := stub.writer.WriteFrame(shimwire.HelloAck{}); err != nil {
		conn.Close()
		return
	}
	s.mu.Lock()
	s.conns[c.token] = stub
	s.mu.Unlock()
	stub.read(reader)
	conn.Close()
	s.mu.Lock()
	if s.conns[c.token] == stub {
		delete(s.conns, c.token)
	}
	s.mu.Unlock()
}

func (s *streamStub) Conn(token claim.Token) (runtime.Conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, ok := s.conns[token]
	return conn, ok
}

// await is the claim's live connection, once its shim has said hello.
func (s *streamStub) await(t *testing.T, token claim.Token) *stubConn {
	t.Helper()
	var conn *stubConn
	eventually(t, 20*time.Second, "the shim of "+string(token)+" to connect", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, ok := s.conns[token]
		if ok {
			select {
			case <-c.done:
				return false
			default:
				conn = c
				return true
			}
		}
		return false
	})
	return conn
}

// stubConn is one shim connection as a runtime.Conn.
type stubConn struct {
	writer  *shimwire.Writer
	mu      sync.Mutex
	pending map[string]chan shimwire.Response
	next    int
	events  chan string
	done    chan struct{}
}

func (c *stubConn) read(reader *shimwire.Reader) {
	defer close(c.done)
	for {
		line, err := reader.ReadLine()
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			return
		}
		switch f := frame.(type) {
		case shimwire.Response:
			c.mu.Lock()
			ch := c.pending[f.ID]
			delete(c.pending, f.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		default:
			select {
			case c.events <- frame.FrameType():
			default:
			}
		}
	}
}

func (c *stubConn) request(ctx context.Context, build func(id string) shimwire.Frame) (shimwire.Response, error) {
	c.mu.Lock()
	c.next++
	id := "req-" + strconv.Itoa(c.next)
	ch := make(chan shimwire.Response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.writer.WriteFrame(build(id)); err != nil {
		return shimwire.Response{}, err
	}
	select {
	case resp := <-ch:
		if !resp.Success {
			return resp, errors.New(resp.Error)
		}
		return resp, nil
	case <-c.done:
		return shimwire.Response{}, errors.New("connection closed")
	case <-ctx.Done():
		return shimwire.Response{}, ctx.Err()
	}
}

func (c *stubConn) Prompt(ctx context.Context, deliveryID, message string) error {
	_, err := c.request(ctx, func(id string) shimwire.Frame {
		return shimwire.Prompt{ID: id, DeliveryID: deliveryID, Message: message}
	})
	return err
}

func (c *stubConn) GetState(ctx context.Context) (runtime.ConnState, error) {
	resp, err := c.request(ctx, func(id string) shimwire.Frame { return shimwire.GetState{ID: id} })
	if err != nil {
		return runtime.ConnState{}, err
	}
	var data shimwire.StateData
	if err := json.Unmarshal(resp.Data, &data); err != nil || data.IsStreaming == nil {
		return runtime.ConnState{}, fmt.Errorf("get_state answered %s", resp.Data)
	}
	return runtime.ConnState{IsStreaming: *data.IsStreaming}, nil
}

func (c *stubConn) Shutdown(context.Context) error { return c.writer.WriteFrame(shimwire.Shutdown{}) }

func (c *stubConn) AdoptWorkingCopy(context.Context, runtime.GitIdentity, time.Duration) error {
	return errors.New("the stream stub does not adopt working copies")
}

func (c *stubConn) awaitEvent(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-c.events:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("no %s frame arrived", want)
		}
	}
}

// rig is one test's private tmux server, daemon, stream stub, and runtime.
type rig struct {
	t            *testing.T
	dir          string
	tmuxDir      string
	stateDir     string
	rolePrompt   string
	instructions string
	legion, omp  string
	project      string
	environ      []string
	daemon       *fakeDaemon
	stream       *streamStub
	rt           *Runtime
	argvMu       sync.Mutex
	argvs        [][]string
}

func newRig(t *testing.T, adjust ...func(*Options)) *rig {
	t.Helper()
	requireTmux(t)
	r := &rig{t: t}
	r.legion, r.omp = standins(t)
	dir, err := os.MkdirTemp("", "lgt")
	if err != nil {
		t.Fatal(err)
	}
	r.dir = dir
	r.tmuxDir = filepath.Join(dir, "tmux")
	r.stateDir = filepath.Join(dir, "state")
	for _, d := range []string{r.tmuxDir, r.stateDir, filepath.Join(dir, "roles")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.rolePrompt = filepath.Join(dir, "roles", "tester.md")
	if err := os.WriteFile(r.rolePrompt, []byte("You are the stand-in tester.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.instructions = filepath.Join(r.stateDir, "deployment-instructions.md")
	if err := os.WriteFile(r.instructions, []byte("# Deployment instructions (demo)\n\nRun the checks.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TMUX_TMPDIR=") {
			r.environ = append(r.environ, entry)
		}
	}
	r.environ = append(r.environ, "TMUX_TMPDIR="+r.tmuxDir)
	r.project = "t" + randomHex(t, 4)
	r.daemon = newFakeDaemon()
	r.stream = newStreamStub(t, filepath.Join(dir, "stream.sock"), r.daemon)
	r.rt = r.newRuntime(adjust...)
	t.Cleanup(func() {
		_, _ = r.tmux("kill-server")
		r.stream.listener.Close()
		r.daemon.server.Close()
		os.RemoveAll(dir)
	})
	return r
}

// newRuntime is a runtime on the rig's server — a second one is a restarted daemon — recording
// every argv it hands tmux.
func (r *rig) newRuntime(adjust ...func(*Options)) *Runtime {
	r.t.Helper()
	opts := Options{
		Project:        r.project,
		StateDir:       r.stateDir,
		StreamAddress:  "unix://" + filepath.Join(r.dir, "stream.sock"),
		DaemonURL:      r.daemon.server.URL,
		EnvoyURL:       "http://127.0.0.1:9020",
		NatsURLs:       []string{"nats://127.0.0.1:4222"},
		OmpInvocation:  shellPath(r.omp),
		StopGrace:      5 * time.Second,
		ProbeInterval:  100 * time.Millisecond,
		AdoptTimeout:   5 * time.Second,
		CommandTimeout: 10 * time.Second,
		Conns:          r.stream,
		Environ:        r.environ,
		Executable:     func() (string, error) { return r.legion, nil },
	}
	for _, f := range adjust {
		f(&opts)
	}
	rt, err := New(opts)
	if err != nil {
		r.t.Fatal(err)
	}
	run := rt.run
	rt.run = func(ctx context.Context, argv []string) (result, error) {
		r.argvMu.Lock()
		r.argvs = append(r.argvs, argv)
		r.argvMu.Unlock()
		return run(ctx, argv)
	}
	return rt
}

func (r *rig) recorded() [][]string {
	r.argvMu.Lock()
	defer r.argvMu.Unlock()
	return slices.Clone(r.argvs)
}

// tmux runs a command against the rig's server as a bystander would: not through the runtime.
func (r *rig) tmux(args ...string) (string, error) {
	cmd := exec.Command("tmux", append([]string{"-L", "legion-" + r.project}, args...)...)
	cmd.Env = r.environ
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *rig) mustTmux(args ...string) string {
	r.t.Helper()
	out, err := r.tmux(args...)
	if err != nil {
		r.t.Fatalf("tmux %q: %v: %s", args, err, out)
	}
	return out
}

// spec is a spawn spec for a claim, with a boot token minted for this launch and known to the
// daemon, a workspace of its own, a PATH distinguishable from the daemon's, and an Envoy secret.
func (r *rig) spec(token, tree, issue string, role claim.Role) runtime.SpawnSpec {
	r.t.Helper()
	bootToken := "boot-" + randomHex(r.t, 8)
	r.daemon.mu.Lock()
	r.daemon.claims[bootToken] = bootClaim{token: claim.Token(token), tree: tree, issue: issue, role: role, generation: 1}
	r.daemon.mu.Unlock()
	workspace := filepath.Join(r.dir, "ws", issue)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		r.t.Fatal(err)
	}
	return runtime.SpawnSpec{
		Claim:      claim.Token(token),
		Project:    r.project,
		Tree:       tree,
		Issue:      issue,
		Role:       role,
		Generation: 1,
		BootToken:  bootToken,
		Env:        map[string]string{"PATH": "/legion-test-path-marker:" + os.Getenv("PATH")},
		Secrets:    map[string]string{"ENVOY_TOKEN": "envoy-" + randomHex(r.t, 8)},
		Prompt: runtime.PromptParts{
			RolePromptPaths:            []string{r.rolePrompt},
			Addressing:                 `Your topic is "notifications.role.` + token + `"; $HOME stays literal.`,
			DeploymentInstructionsPath: r.instructions,
		},
		Workspace: workspace,
	}
}

func eventually(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func probe(t *testing.T, rt *Runtime, loc runtime.Locator) runtime.Observation {
	t.Helper()
	obs, err := rt.Probe(context.Background(), loc)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return obs
}

func panePid(t *testing.T, loc runtime.Locator) int {
	t.Helper()
	inc, err := parseIncarnation(loc.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	return inc.pid
}

func procCmdline(pid int) []string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}

func procEnviron(t *testing.T, pid int) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, entry := range strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00") {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	return env
}

// descendant is the first process under root, walking children breadth first, whose argv[0] is
// named name — how the Stage 2 proof finds a pane's OMP.
func descendant(t *testing.T, root int, name string) int {
	t.Helper()
	found := 0
	eventually(t, 20*time.Second, name+" under pid "+strconv.Itoa(root), func() bool {
		children := map[int][]int{}
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if err != nil {
				continue
			}
			fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
			if ppid, err := strconv.Atoi(fields[1]); err == nil {
				children[ppid] = append(children[ppid], pid)
			}
		}
		queue := []int{root}
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			if argv := procCmdline(pid); len(argv) > 0 && filepath.Base(argv[0]) == name {
				found = pid
				return true
			}
			queue = append(queue, children[pid]...)
		}
		return false
	})
	return found
}

func readReport(t *testing.T, workspace string, pid int) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workspace, fmt.Sprintf("standin-%d.json", pid)))
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func hasKillPane(argvs [][]string) bool {
	for _, argv := range argvs {
		if slices.Contains(argv, "kill-pane") {
			return true
		}
	}
	return false
}

// A spawned agent's whole life on a real server: it opens, registers, is ready, answers a prompt
// through its pane, runs under exactly the environment the runtime promises, stops gracefully when
// asked, resumes as the same session, and — when it will not end itself — is killed after the
// grace.
func TestRealTmuxLifecycle(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	spec := r.spec("legion-t-LEGION-1-architect", "LEGION-1", "LEGION-1", claim.RoleArchitect)

	loc, err := r.rt.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := loc.Validate(); err != nil || loc.Runtime != runtime.RuntimeTmux || loc.Claim != spec.Claim {
		t.Fatalf("Spawn returned %+v (%v)", loc, err)
	}
	registration := r.daemon.awaitReady(t, spec.BootToken)
	if obs := probe(t, r.rt, loc); obs.Kind != runtime.Alive {
		t.Fatalf("Probe after spawn = %+v, want alive", obs)
	}

	// OMP's RPC frames reach the pane's process and come back.
	conn := r.stream.await(t, spec.Claim)
	if err := conn.Prompt(ctx, "delivery-1", "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	conn.awaitEvent(t, shimwire.TypeAgentStart)
	if state, err := conn.GetState(ctx); err != nil || state.IsStreaming {
		t.Fatalf("GetState = (%+v, %v)", state, err)
	}

	// The pane ran `sh -c`, and its OMP runs under the environment the runtime promises.
	pid := panePid(t, loc)
	if argv := procCmdline(pid); len(argv) < 2 || argv[0] != "/bin/sh" || argv[1] != "-c" {
		t.Errorf("the pane's process is %q, want /bin/sh -c", argv)
	}
	omp := descendant(t, pid, "omp")
	env := procEnviron(t, omp)
	secrets := filepath.Join(r.stateDir, "secrets")
	for name, want := range map[string]string{
		"PATH":                   filepath.Join(r.stateDir, "worker-bin") + ":" + spec.Env["PATH"],
		"XDG_CONFIG_HOME":        filepath.Join(r.stateDir, "home", ".config"),
		"XDG_CACHE_HOME":         filepath.Join(r.stateDir, "home", ".cache"),
		"XDG_DATA_HOME":          filepath.Join(r.stateDir, "home", ".local", "share"),
		"XDG_STATE_HOME":         filepath.Join(r.stateDir, "home", ".local", "state"),
		"LEGION_DAEMON_API":      "go",
		"LEGION_TREE":            "LEGION-1",
		"LEGION_ISSUE":           "LEGION-1",
		"LEGION_ROLE":            "architect",
		"LEGION_GENERATION":      "1",
		"LEGION_PROJECT":         r.project,
		"LEGION_DAEMON_URL":      r.daemon.server.URL,
		"LEGION_STATE_DIR":       r.stateDir,
		"LEGION_WORKSPACE":       spec.Workspace,
		"ENVOY_NATS_URL":         "nats://127.0.0.1:4222",
		"ENVOY_URL":              "http://127.0.0.1:9020",
		"GIT_TERMINAL_PROMPT":    "0",
		"LEGION_BOOT_TOKEN_FILE": filepath.Join(secrets, string(spec.Claim)),
		"ENVOY_TOKEN_FILE":       filepath.Join(secrets, string(spec.Claim)+"-envoy_token"),
	} {
		if env[name] != want {
			t.Errorf("OMP's %s = %q, want %q", name, env[name], want)
		}
	}
	for name, value := range env {
		if value == spec.BootToken || value == spec.Secrets["ENVOY_TOKEN"] {
			t.Errorf("OMP's %s carries a secret's value", name)
		}
	}
	for pointer, want := range map[string]string{
		"LEGION_BOOT_TOKEN_FILE": spec.BootToken,
		"ENVOY_TOKEN_FILE":       spec.Secrets["ENVOY_TOKEN"],
	} {
		if info, err := os.Stat(env[pointer]); err != nil {
			t.Errorf("%s file %s: %v", pointer, env[pointer], err)
		} else if info.Mode().Perm() != 0o600 {
			t.Errorf("%s file %s: mode %v, want 0600", pointer, env[pointer], info.Mode().Perm())
		}
		if got, _ := os.ReadFile(env[pointer]); string(got) != want {
			t.Errorf("%s file holds the wrong secret", pointer)
		}
	}
	if info, err := os.Stat(secrets); err != nil {
		t.Errorf("secrets directory: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("secrets directory mode %v, want 0700", info.Mode().Perm())
	}
	for _, argv := range r.recorded() {
		for i, word := range argv {
			if strings.Contains(word, spec.BootToken) || strings.Contains(word, spec.Secrets["ENVOY_TOKEN"]) {
				t.Errorf("a secret reached tmux's argv: %q", argv[3])
			}
			if word == "-e" && i+1 < len(argv) && strings.HasPrefix(argv[i+1], "PATH=") {
				t.Errorf("PATH rode a -e pair: %q", argv[i+1])
			}
		}
	}

	// One --append-system-prompt word, its $(cat)s expanded by the pane's shell, the addressing
	// text literal.
	report := readReport(t, spec.Workspace, omp)
	prompts, _ := report["systemPrompt"].([]any)
	want := "You are the stand-in tester.\n\n" + spec.Prompt.Addressing + "\n\n# Deployment instructions (demo)\n\nRun the checks."
	if len(prompts) != 1 || prompts[0] != want {
		t.Errorf("OMP's system prompt arguments = %q, want exactly one: %q", prompts, want)
	}
	if report["mode"] != "rpc" || report["resumed"] != false {
		t.Errorf("OMP ran with mode %v resumed %v, want rpc and fresh", report["mode"], report["resumed"])
	}

	// Suspend: the shutdown frame ends the agent itself, and nothing is killed.
	before := len(r.recorded())
	if err := r.rt.Suspend(ctx, loc); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if hasKillPane(r.recorded()[before:]) {
		t.Errorf("a graceful stop killed the pane")
	}
	if obs := probe(t, r.rt, loc); obs.Kind != runtime.Gone {
		t.Fatalf("Probe after suspend = %+v, want gone", obs)
	}

	// Resume: the same session, from the file it registered, in a new incarnation.
	resumed := r.spec("legion-t-LEGION-1-architect", "LEGION-1", "LEGION-1", claim.RoleArchitect)
	resumed.ResumeSessionFile = registration.OmpSessionFile
	loc2, err := r.rt.Resume(ctx, loc, resumed)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if loc2.Incarnation == loc.Incarnation {
		t.Errorf("Resume kept the old incarnation %s", loc.Incarnation)
	}
	if again := r.daemon.awaitReady(t, resumed.BootToken); again.SessionID != registration.SessionID {
		t.Errorf("the resumed agent registered as session %s, want %s", again.SessionID, registration.SessionID)
	}
	if report := readReport(t, spec.Workspace, descendant(t, panePid(t, loc2), "omp")); report["resumed"] != true {
		t.Errorf("the resumed OMP was not started with --resume: %v", report["argv"])
	}

	// An agent that will not end itself: the grace runs out and its pane is killed.
	conn2 := r.stream.await(t, spec.Claim)
	if err := conn2.Prompt(ctx, "delivery-2", "ignore SIGTERM"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	before = len(r.recorded())
	if err := r.rt.Stop(ctx, loc2, 500*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !hasKillPane(r.recorded()[before:]) {
		t.Errorf("an agent that ignored its shutdown was not killed")
	}
	if obs := probe(t, r.rt, loc2); obs.Kind != runtime.Gone {
		t.Fatalf("Probe after stop = %+v, want gone", obs)
	}

	// Resuming from a session file that is gone is a refusal, never a fresh agent.
	missing := r.spec("legion-t-LEGION-1-architect", "LEGION-1", "LEGION-1", claim.RoleArchitect)
	missing.ResumeSessionFile = filepath.Join(r.dir, "no-such-session.jsonl")
	if _, err := r.rt.Resume(ctx, loc2, missing); err == nil ||
		err.Error() != "Refusing to start LEGION-1 fresh while resurrecting: recorded OMP session file is missing: "+missing.ResumeSessionFile {
		t.Errorf("Resume from a missing session file = %v", err)
	}
}

// Panes of one issue share its window, while a pane there still verifies; another issue gets a
// window of its own; and one claim never has two processes.
func TestRealTmuxSplitsIntoTheIssueWindow(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	architect, err := r.rt.Spawn(ctx, r.spec("legion-t-LEGION-2-architect", "LEGION-2", "LEGION-2", claim.RoleArchitect))
	if err != nil {
		t.Fatalf("Spawn architect: %v", err)
	}
	tester, err := r.rt.Spawn(ctx, r.spec("legion-t-LEGION-2-tester", "LEGION-2", "LEGION-2", claim.RoleTester))
	if err != nil {
		t.Fatalf("Spawn tester: %v", err)
	}
	other, err := r.rt.Spawn(ctx, r.spec("legion-t-LEGION-3-architect", "LEGION-3", "LEGION-3", claim.RoleArchitect))
	if err != nil {
		t.Fatalf("Spawn on another issue: %v", err)
	}
	if tester.Tmux.Window != architect.Tmux.Window || tester.Tmux.Pane == architect.Tmux.Pane {
		t.Errorf("the tester opened %+v, want a new pane in the architect's window %s", tester.Tmux, architect.Tmux.Window)
	}
	if other.Tmux.Window == architect.Tmux.Window {
		t.Errorf("another issue's pane landed in LEGION-2's window %s", other.Tmux.Window)
	}
	split := false
	for _, argv := range r.recorded() {
		if len(argv) > 5 && argv[3] == "split-window" && argv[5] == architect.Tmux.Window {
			split = true
		}
	}
	if !split {
		t.Errorf("no split-window into %s was recorded", architect.Tmux.Window)
	}
	for _, loc := range []runtime.Locator{architect, tester, other} {
		if obs := probe(t, r.rt, loc); obs.Kind != runtime.Alive {
			t.Errorf("Probe %s = %+v, want alive", loc.Claim, obs)
		}
	}
	if windows := r.mustTmux("list-windows", "-F", "#{window_id} #{window_name} #{@legion_owner}"); !strings.Contains(windows, architect.Tmux.Window+" legion-2 legion-"+r.project) {
		t.Errorf("the issue's window is not named for it and marked as the daemon's:\n%s", windows)
	}

	// One claim, one process: a second spawn of a live claim is refused, and a resume waits for
	// the previous incarnation to go — and refuses when it does not.
	if _, err := r.rt.Spawn(ctx, r.spec("legion-t-LEGION-2-tester", "LEGION-2", "LEGION-2", claim.RoleTester)); err == nil ||
		!strings.Contains(err.Error(), "is still running in pane "+tester.Tmux.Pane) {
		t.Errorf("a second spawn of a live claim = %v", err)
	}
	impatient := r.newRuntime(func(o *Options) { o.StopGrace = 300 * time.Millisecond })
	resume := r.spec("legion-t-LEGION-2-tester", "LEGION-2", "LEGION-2", claim.RoleTester)
	resume.ResumeSessionFile = r.rolePrompt
	if _, err := impatient.Resume(ctx, tester, resume); err == nil ||
		!strings.Contains(err.Error(), "previous incarnation "+tester.Incarnation+" is still running") {
		t.Errorf("a resume over a live incarnation = %v", err)
	}
}

// A tmux server that does not answer proves nothing about a pane: Probe says Uncertain, and a stop
// that cannot confirm its process gone is an error, never a success.
func TestRealTmuxUncertainOnABrokenSocket(t *testing.T) {
	r := newRig(t, func(o *Options) { o.CommandTimeout = time.Second })
	socketDir := filepath.Join(r.tmuxDir, fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A socket that accepts and never says anything: a server not responding.
	hung, err := net.Listen("unix", filepath.Join(socketDir, r.rt.Socket()))
	if err != nil {
		t.Fatal(err)
	}
	defer hung.Close()
	var held []net.Conn
	var heldMu sync.Mutex
	go func() {
		for {
			conn, err := hung.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, conn)
			heldMu.Unlock()
		}
	}()
	defer func() {
		heldMu.Lock()
		defer heldMu.Unlock()
		for _, conn := range held {
			conn.Close()
		}
	}()
	loc := runtime.Locator{
		Runtime:     runtime.RuntimeTmux,
		Claim:       "legion-t-LEGION-9-architect",
		Incarnation: strconv.Itoa(os.Getpid()) + ":1",
		Tmux:        &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
	}
	ctx := context.Background()

	obs := probe(t, r.rt, loc)
	if obs.Kind != runtime.Uncertain || !strings.Contains(obs.Detail, "cannot verify pane %1: list-panes -t %1 timed out") {
		t.Errorf("Probe on a hung server = %+v, want uncertain naming the timeout", obs)
	}
	if err := r.rt.Stop(ctx, loc, 0); err == nil || !strings.Contains(err.Error(), "cannot verify pane %1") {
		t.Errorf("Stop on a hung server = %v, want a refusal", err)
	}
	if err := r.rt.ReconcileOrphans(ctx, []runtime.Locator{loc}, 0); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("ReconcileOrphans on a hung server = %v, want the listing's failure", err)
	}
	// The sweep reports the same, and keeps the process watched.
	sweepCtx, stop := context.WithCancel(ctx)
	defer stop()
	observations, err := r.rt.Observe(sweepCtx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case obs := <-observations:
			if obs.Kind != runtime.Uncertain || obs.Locator.Incarnation != loc.Incarnation {
				t.Errorf("sweep %d reported %+v, want uncertain", i, obs)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("sweep %d reported nothing", i)
		}
	}
}

// The scrub removes what the pane environment does not carry — including a multi-line value set
// the way tmux prints raw to a UTF-8 client — and keeps every allow-listed name.
func TestRealTmuxScrubRemovesAPlantedVariableAndKeepsTheAllowList(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	loc, err := r.rt.Spawn(ctx, r.spec("legion-t-LEGION-5-architect", "LEGION-5", "LEGION-5", claim.RoleArchitect))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	session := r.rt.Socket()
	r.mustTmux("set-environment", "-g", "PLANTED_TOKEN", "stale-secret")
	r.mustTmux("set-environment", "-g", "PLANTED_PEM", "-----BEGIN KEY-----\nMIIabc=\nHOME;=x\n\"; export HOME;\n-----END KEY-----")
	r.mustTmux("set-environment", "-t", session, "SSH_AUTH_SOCK", "/tmp/agent.sock")
	globalsBefore, _, err := r.rt.environmentNames(ctx, globalTable)
	if err != nil {
		t.Fatalf("read the global table: %v", err)
	}

	removed, err := r.rt.ScrubServerEnvironment(ctx)
	if err != nil {
		t.Fatalf("ScrubServerEnvironment: %v", err)
	}
	for _, name := range []string{"PLANTED_PEM", "PLANTED_TOKEN", "SSH_AUTH_SOCK"} {
		if !slices.Contains(removed, name) {
			t.Errorf("the scrub did not remove %s (removed %q)", name, removed)
		}
	}
	for _, name := range removed {
		if _, allowed := r.rt.paneEnv[name]; allowed {
			t.Errorf("the scrub removed allow-listed %s", name)
		}
	}
	globals, _, err := r.rt.environmentNames(ctx, globalTable)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range globals {
		if _, allowed := r.rt.paneEnv[name]; !allowed && !tmuxOwnGlobals[name] {
			t.Errorf("the global table still holds %s", name)
		}
	}
	for _, name := range globalsBefore {
		if _, allowed := r.rt.paneEnv[name]; allowed && !slices.Contains(globals, name) {
			t.Errorf("the scrub lost allow-listed %s", name)
		}
	}
	for _, name := range []string{"HOME", "PATH", "XDG_CONFIG_HOME"} {
		if !slices.Contains(globals, name) {
			t.Errorf("the global table lacks allow-listed %s after the scrub", name)
		}
	}
	sessionNames, _, err := r.rt.environmentNames(ctx, sessionTable(session))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sessionNames, "SSH_AUTH_SOCK") {
		t.Errorf("the session table still holds SSH_AUTH_SOCK")
	}
	if obs := probe(t, r.rt, loc); obs.Kind != runtime.Alive {
		t.Errorf("the pane did not survive the scrub: %+v", obs)
	}
}

// A crash between new-window creating its pane and the daemon recording that pane's locator leaves
// the pane running but no runtime record. The pane must mark its own window before the report gets
// back to the daemon, so a restarted runtime can reap it at boot (grace zero).
func TestRealTmuxReconcilesAWindowWhoseReportWasLost(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec := r.spec("lost-report", "LEGION-1", "LEGION-9", claim.RoleReviewer)
	base := r.rt.run
	dropped := false
	r.rt.run = func(ctx context.Context, argv []string) (result, error) {
		res, err := base(ctx, argv)
		if err == nil && !dropped && verb(argv) == "new-window" {
			dropped = true
			return res, errors.New("the new-window report was lost")
		}
		return res, err
	}
	if _, err := r.rt.Spawn(ctx, spec); err == nil || !strings.Contains(err.Error(), "new-window report was lost") {
		t.Fatalf("Spawn after a lost report = %v, want the injected loss", err)
	}
	r.rt.run = base
	panes := func() string {
		return r.mustTmux("list-panes", "-a", "-F", "#{pane_id}\t#{window_id}\t#{@legion_owner}\t#{pane_start_command}")
	}
	eventually(t, 10*time.Second, "the unrecorded worker-shim pane to start", func() bool {
		return strings.Contains(panes(), "worker-shim")
	})
	if !strings.Contains(panes(), "sleep 3600") {
		t.Fatalf("the private server has no unmarked bootstrap pane before reconciliation:\n%s", panes())
	}
	if err := r.rt.ReconcileOrphans(ctx, nil, 0); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if listing := panes(); strings.Contains(listing, "worker-shim") {
		t.Fatalf("the lost-report worker pane survived reconciliation:\n%s", listing)
	} else if !strings.Contains(listing, "sleep 3600") {
		t.Fatalf("reconciliation reaped the unmarked private-server bootstrap pane too:\n%s", listing)
	}
}

// What a crash between opening a pane and recording it leaves — a worker-shim pane in a live
// window, a window marked as the daemon's — is reaped once idle past the grace; a window without
// the marker is a human's and stays. A restarted daemon told of a live pane watches it.
func TestRealTmuxReconcileOrphans(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	live, err := r.rt.Spawn(ctx, r.spec("legion-t-LEGION-4-architect", "LEGION-4", "LEGION-4", claim.RoleArchitect))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	session := r.rt.Socket()
	orphanPane := r.mustTmux("split-window", "-d", "-t", live.Tmux.Window, "-P", "-F", "#{pane_id}", "/bin/sh", "-c", "exec sleep 300 # legion worker-shim")
	orphanWindow := r.mustTmux("new-window", "-d", "-t", session, "-P", "-F", "#{window_id}", "/bin/sh", "-c", "exec sleep 300")
	r.mustTmux("set-option", "-w", "-t", orphanWindow, "@legion_owner", session)
	humans := r.mustTmux("new-window", "-d", "-t", session, "-P", "-F", "#{window_id}", "sleep 300")
	panes := func() string { return r.mustTmux("list-panes", "-a", "-F", "#{pane_id} #{window_id}") }

	if err := r.rt.ReconcileOrphans(ctx, nil, time.Hour); err != nil {
		t.Fatalf("ReconcileOrphans within the grace: %v", err)
	}
	if listing := panes(); !strings.Contains(listing, orphanPane+" ") || !strings.Contains(listing, orphanWindow) {
		t.Fatalf("something was reaped within the grace:\n%s", listing)
	}

	if err := r.rt.ReconcileOrphans(ctx, nil, 0); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	listing := panes()
	if strings.Contains(listing, orphanPane+" ") {
		t.Errorf("the orphaned worker-shim pane %s survived:\n%s", orphanPane, listing)
	}
	if strings.Contains(listing, orphanWindow) {
		t.Errorf("the orphaned marked window %s survived:\n%s", orphanWindow, listing)
	}
	if !strings.Contains(listing, humans) {
		t.Errorf("the unmarked window %s was reaped:\n%s", humans, listing)
	}
	if obs := probe(t, r.rt, live); obs.Kind != runtime.Alive {
		t.Errorf("the recorded pane did not survive: %+v", obs)
	}

	restarted := r.newRuntime()
	if err := restarted.ReconcileOrphans(ctx, []runtime.Locator{live}, 0); err != nil {
		t.Fatalf("ReconcileOrphans after a restart: %v", err)
	}
	sweepCtx, stop := context.WithCancel(ctx)
	defer stop()
	observations, err := restarted.Observe(sweepCtx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case obs := <-observations:
		if obs.Kind != runtime.Alive || obs.Locator.Incarnation != live.Incarnation {
			t.Errorf("the restarted daemon's sweep reported %+v, want the known pane alive", obs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the restarted daemon's sweep reported nothing")
	}
}

// The sweep reports a watched process alive, then gone once its pane is killed from outside — and
// then never again, since a gone incarnation cannot come back.
func TestRealTmuxObserveReportsGoneOnce(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	loc, err := r.rt.Spawn(ctx, r.spec("legion-t-LEGION-6-architect", "LEGION-6", "LEGION-6", claim.RoleArchitect))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	observations, err := r.rt.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs := <-observations; obs.Kind != runtime.Alive {
		t.Fatalf("first sweep = %+v, want alive", obs)
	}
	r.mustTmux("kill-pane", "-t", loc.Tmux.Pane)
	deadline := time.After(10 * time.Second)
	for gone := false; !gone; {
		select {
		case obs := <-observations:
			gone = obs.Kind == runtime.Gone
			if !gone && obs.Kind != runtime.Alive {
				t.Fatalf("sweep reported %+v", obs)
			}
		case <-deadline:
			t.Fatal("the sweep never reported the killed pane gone")
		}
	}
	select {
	case obs := <-observations:
		t.Errorf("the sweep reported a gone incarnation again: %+v", obs)
	case <-time.After(time.Second):
	}
}

// A pane id or a pid is never taken as proof of the recorded process. A locator whose pane now runs
// another process — another pid, the same pid started at another moment, or something that is not
// OMP — is NotRecordedProcess, and stopping it kills nothing: the pane's real occupant survives.
func TestRealTmuxNeverKillsAPaneThatIsNotTheRecordedProcess(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	live, err := r.rt.Spawn(ctx, r.spec("legion-t-LEGION-7-architect", "LEGION-7", "LEGION-7", claim.RoleArchitect))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	inc, err := parseIncarnation(live.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	bystander := strings.Fields(r.mustTmux("new-window", "-d", "-t", r.rt.Socket(), "-P", "-F", "#{window_id} #{pane_id} #{pane_pid}", "exec sleep 300"))
	bystanderPid, _ := strconv.Atoi(bystander[2])
	bystanderTicks, alive, err := r.rt.startTicks(bystanderPid)
	if err != nil || !alive {
		t.Fatalf("read the bystander's start ticks: %v", err)
	}
	stale := func(window, pane string, pid int, ticks uint64) runtime.Locator {
		return runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       "legion-t-LEGION-7-architect",
			Incarnation: incarnation{pid: pid, ticks: ticks}.String(),
			Tmux:        &runtime.TmuxLocator{Window: window, Pane: pane},
		}
	}
	for _, tc := range []struct {
		name   string
		loc    runtime.Locator
		detail string
	}{
		{
			name:   "another pid in the pane",
			loc:    stale(live.Tmux.Window, live.Tmux.Pane, inc.pid+100000, inc.ticks),
			detail: fmt.Sprintf("pane %s now runs pid %d (recorded pid %d start %d)", live.Tmux.Pane, inc.pid, inc.pid+100000, inc.ticks),
		},
		{
			name:   "the same pid started at another moment",
			loc:    stale(live.Tmux.Window, live.Tmux.Pane, inc.pid, inc.ticks+1),
			detail: fmt.Sprintf("pane %s runs pid %d started at %d (recorded pid %d start %d)", live.Tmux.Pane, inc.pid, inc.ticks, inc.pid, inc.ticks+1),
		},
		{
			name:   "a process that is not OMP",
			loc:    stale(bystander[0], bystander[1], bystanderPid, bystanderTicks),
			detail: fmt.Sprintf("pane %s pid %d is not running OMP (recorded pid %d start %d)", bystander[1], bystanderPid, bystanderPid, bystanderTicks),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := probe(t, r.rt, tc.loc)
			if obs.Kind != runtime.NotRecordedProcess || obs.Detail != tc.detail {
				t.Errorf("Probe = %+v, want not_recorded_process: %s", obs, tc.detail)
			}
			before := len(r.recorded())
			if err := r.rt.Stop(ctx, tc.loc, 0); err != nil {
				t.Errorf("Stop: %v", err)
			}
			if hasKillPane(r.recorded()[before:]) {
				t.Errorf("Stop killed a pane that is not the recorded process")
			}
		})
	}
	if obs := probe(t, r.rt, live); obs.Kind != runtime.Alive {
		t.Errorf("the live agent did not survive: %+v", obs)
	}
	if panes := r.mustTmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, bystander[1]) {
		t.Errorf("the bystander's pane %s was killed:\n%s", bystander[1], panes)
	}
}

// A provider key reaches a pane's OMP as the daemon resolved it at boot — the secretsd key's value
// under the variable OMP reads, through the daemon-held file the real shim is pointed at — and
// reaches nothing else: not the shim's own environment, not under the secretsd key's own name, and
// no pane ever sees the secret store's config or age identity, even when the daemon's own
// environment names them.
func TestRealTmuxProviderKeysReachOMPAndNothingElse(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bin := t.TempDir()
	stub := "#!/bin/sh\ncase \"$2:$3\" in\n" +
		"  LEGION_TEST_PROVIDER_SECRET:--no-request) echo '{\"key\":\"LEGION_TEST_PROVIDER_SECRET\",\"tier\":\"agent\"}' ;;\n" +
		"  LEGION_TEST_PROVIDER_SECRET:--value) echo 'stub-provider-value' ;;\n" +
		"  *) exit 9 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "secrets"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	ageIdentity := filepath.Join(t.TempDir(), "keys.txt")
	daemonEnviron := append(slices.Clone(r.environ),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"SOPS_AGE_KEY_FILE="+ageIdentity,
		"SECRETSD_CONFIG=/home/legion/.config/secretsd/config.toml",
	)
	dir, err := config.MaterializeProviderKeys([]config.ProviderKey{{Env: "LEGION_TEST_PROVIDER_ENV", Secret: "LEGION_TEST_PROVIDER_SECRET"}}, r.stateDir, daemonEnviron,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("MaterializeProviderKeys: %v", err)
	}
	r.environ = append(r.environ, "SOPS_AGE_KEY_FILE="+ageIdentity, "SECRETSD_CONFIG=/home/legion/.config/secretsd/config.toml")
	rt := r.newRuntime(func(o *Options) { o.ProviderEnvDir = dir })
	spec := r.spec("legion-t-LEGION-8-architect", "LEGION-8", "LEGION-8", claim.RoleArchitect)

	loc, err := rt.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	r.daemon.awaitReady(t, spec.BootToken)
	shim := descendant(t, panePid(t, loc), "legion")
	omp := descendant(t, shim, "omp")

	ompEnv, shimEnv := procEnviron(t, omp), procEnviron(t, shim)
	if ompEnv["LEGION_TEST_PROVIDER_ENV"] != "stub-provider-value" {
		t.Errorf("OMP's LEGION_TEST_PROVIDER_ENV has %d bytes, want the resolved value", len(ompEnv["LEGION_TEST_PROVIDER_ENV"]))
	}
	if _, ok := ompEnv["LEGION_TEST_PROVIDER_SECRET"]; ok {
		t.Errorf("OMP's environment carries the secretsd key's name; it must carry the variable OMP reads")
	}
	if _, ok := shimEnv["LEGION_TEST_PROVIDER_ENV"]; ok {
		t.Errorf("the shim's own environment carries the provider key; only OMP's may")
	}
	for _, env := range []map[string]string{ompEnv, shimEnv} {
		for _, name := range []string{"SOPS_AGE_KEY_FILE", "SOPS_AGE_KEY", "SECRETSD_CONFIG"} {
			if _, ok := env[name]; ok {
				t.Errorf("a pane process carries %s", name)
			}
		}
		for name, value := range env {
			if value == ageIdentity {
				t.Errorf("a pane process's %s names the daemon's age identity", name)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(ompEnv["XDG_CONFIG_HOME"], "sops", "age", "keys.txt")); !os.IsNotExist(err) {
		t.Errorf("an age identity is readable under the pane's XDG_CONFIG_HOME (stat: %v)", err)
	}
	flagged := false
	for _, argv := range r.recorded() {
		for _, word := range argv {
			if strings.Contains(word, "--provider-env-dir "+dir+" --") {
				flagged = true
			}
			if strings.Contains(word, "stub-provider-value") {
				t.Errorf("a provider key's value reached tmux's argv")
			}
		}
	}
	if !flagged {
		t.Errorf("no pane command carried --provider-env-dir %s", dir)
	}
}

// A Go pane must reach the Dispatch server its daemon is configured for. Without these two
// variables, OMP's Dispatch client falls back to the operator's ~/.config/opencode/envoy.json and
// a scratch proof writes to that deployment instead.
func TestRealTmuxOMPGetsTheConfiguredDispatchURLAndTokenFile(t *testing.T) {
	ctx := context.Background()
	token := "dispatch-" + randomHex(t, 8)
	r := newRig(t, func(o *Options) {
		file, err := WriteDispatchTokenFile(o.StateDir, token)
		if err != nil {
			t.Fatalf("WriteDispatchTokenFile: %v", err)
		}
		o.DispatchURL = "http://127.0.0.1:18766"
		o.DispatchTokenFile = file
	})
	spec := r.spec("legion-t-LEGION-9-architect", "LEGION-9", "LEGION-9", claim.RoleArchitect)

	loc, err := r.rt.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	r.daemon.awaitReady(t, spec.BootToken)
	env := procEnviron(t, descendant(t, panePid(t, loc), "omp"))
	wantFile := filepath.Join(r.stateDir, "secrets", "dispatch-token")
	if env["DISPATCH_URL"] != "http://127.0.0.1:18766" {
		t.Errorf("OMP's DISPATCH_URL = %q, want the configured Dispatch URL", env["DISPATCH_URL"])
	}
	if env["DISPATCH_TOKEN_FILE"] != wantFile {
		t.Errorf("OMP's DISPATCH_TOKEN_FILE = %q, want %q", env["DISPATCH_TOKEN_FILE"], wantFile)
	}
	if _, ok := env["DISPATCH_TOKEN"]; ok {
		t.Errorf("OMP's environment carries DISPATCH_TOKEN; the token must stay in its file")
	}
	if info, err := os.Stat(wantFile); err != nil {
		t.Errorf("Dispatch token file: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("Dispatch token file mode %v, want 0600", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(wantFile); string(got) != token {
		t.Errorf("Dispatch token file holds the wrong value")
	}
	for _, argv := range r.recorded() {
		for _, word := range argv {
			if strings.Contains(word, token) {
				t.Errorf("the Dispatch token reached tmux's argv")
			}
		}
	}
}
