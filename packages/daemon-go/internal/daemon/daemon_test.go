package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const testOperatorToken = "operator-bearer-for-daemon-tests"

// testDSN is the devbox and CI Postgres these tests run against. The daemon has no in-memory
// mode: what it does on a boot is what its store recorded.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against. Start one and set it:\n" +
			"  docker run -d --name legion-pg -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 postgres:16\n" +
			"  port=$(docker port legion-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
			"  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:$port/legion go test ./internal/daemon/")
	}
	return dsn
}

// shortTempDir is a state directory short enough for the worker stream's unix socket: a socket
// path is bounded at 108 bytes, and t.TempDir() spells the whole test name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lgd")
	if err != nil {
		t.Fatalf("make a state directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// testConfig is a daemon of its own: its project name is unique, so the boots and claims it
// records are its own, its port is one nothing else holds, and its state directory is its alone.
// Every limit and timeout is the shipped default.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	stateDir := shortTempDir(t)
	tokenFile := filepath.Join(t.TempDir(), "operator-token")
	if err := os.WriteFile(tokenFile, []byte(testOperatorToken+"\n"), 0o600); err != nil {
		t.Fatalf("write the operator token: %v", err)
	}
	port := freePort(t)
	return config.Config{
		Project:                                 "TEST" + randomSuffix(t),
		Port:                                    port,
		Bind:                                    "127.0.0.1",
		PostgresDSN:                             testDSN(t),
		StateDir:                                stateDir,
		Runtime:                                 config.Runtime{Name: "tmux"},
		AdmissionCap:                            4,
		DaemonURL:                               "http://127.0.0.1:" + strconv.Itoa(port),
		WorkerStreamPort:                        port + 1,
		WorkerBootTimeout:                       120 * time.Second,
		WorkerBootRegistrationDeadlineIntervals: 3,
		WorkerRPCTimeout:                        5 * time.Second,
		WorkerStopTimeout:                       10 * time.Second,
		TreeStopTimeout:                         60 * time.Second,
		SlowCommandTimeout:                      300 * time.Second,
		ProbeInterval:                           30 * time.Second,
		LaunchFailureLimit:                      3,
		PromptFailureLimit:                      3,
		PromptRetireLimit:                       2,
		OperatorTokenFile:                       tokenFile,
		EnvoyURL:                                "http://127.0.0.1:9020",
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// stillClock is time that never moves: the machines' timers are armed and never fire, so a test
// sees only what it drove.
type stillClock struct{}

func (stillClock) Now() time.Time { return time.Now() }

func (stillClock) AfterFunc(time.Duration, func()) supervise.Cancel { return stopped{} }

type stopped struct{}

func (stopped) Stop() bool { return true }

// built is what the daemon handed the runtime it built: the connection directory and the address
// every pane's shim dials.
type built struct {
	mu      sync.Mutex
	conns   runtime.Conns
	address string
}

// fakeRuntime is a daemon whose runtime is rt: the real stream listener, store, and machines,
// with nothing launched for real.
func fakeRuntime(rt *fake.Runtime, record *built) overrides {
	return overrides{
		runtime: func(_ context.Context, conns runtime.Conns, address string) (runtime.Runtime, error) {
			record.mu.Lock()
			defer record.mu.Unlock()
			record.conns, record.address = conns, address
			return rt, nil
		},
		clock: stillClock{},
	}
}

// boots is what the store records of cfg's project. The schema is brought forward first, so a test
// that asserts a refused start recorded nothing reads an answer even on a database no daemon has
// migrated yet — a fresh CI service, when that test is the first to touch it.
func boots(t *testing.T, cfg config.Config) (int, time.Time) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	count, firstBootAt, err := st.Boots(ctx, cfg.Project)
	if err != nil {
		t.Fatalf("read the boots: %v", err)
	}
	return count, firstBootAt
}

// A daemon told to stop still owes its store a recorded, stamped boot: the restart count and the
// first boot time are what `legion state` answers from.
func TestRunRecordsEveryBootAndKeepsTheFirstBootTime(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := run(ctx, cfg, quietLogger(), fakeRuntime(fake.NewRuntime(), &built{})); err != nil {
		t.Fatalf("first run: %v", err)
	}
	count, firstBootAt := boots(t, cfg)
	if count != 1 {
		t.Fatalf("boots after one run = %d, want 1", count)
	}
	if firstBootAt.IsZero() {
		t.Fatal("the first boot has no recorded time")
	}

	if err := run(ctx, cfg, quietLogger(), fakeRuntime(fake.NewRuntime(), &built{})); err != nil {
		t.Fatalf("second run: %v", err)
	}
	countAgain, firstBootAgain := boots(t, cfg)
	if countAgain != count+1 {
		t.Fatalf("boots after a restart = %d, want %d", countAgain, count+1)
	}
	if !firstBootAgain.Equal(firstBootAt) {
		t.Fatalf("the first boot time moved on a restart: %s, was %s", firstBootAgain, firstBootAt)
	}
}

// The refusal an operator reads when Postgres is not there names where the daemon went, and
// never how it would have got in.
func TestRunRefusesAnUnreachablePostgresByHostAndNotByPassword(t *testing.T) {
	cfg := testConfig(t)
	cfg.PostgresDSN = "postgres://legion:hunter2@127.0.0.1:1/legion"

	refused := make(chan error, 1)
	go func() {
		refused <- run(context.Background(), cfg, quietLogger(), fakeRuntime(fake.NewRuntime(), &built{}))
	}()

	select {
	case err := <-refused:
		if err == nil {
			t.Fatal("run started without a Postgres to start against")
		}
		if !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Errorf("the refusal does not name the host it could not reach: %v", err)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal leaks the DSN password: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not refuse an unreachable Postgres within one second")
	}
}

// A recorded boot is a boot that served. A daemon whose port is already taken never ran, and a
// row for it would inflate `daemon.boots` — the value `legion state` answers with and the stage
// gate asserts on.
func TestRunRecordsNoBootWhenItCannotTakeItsPort(t *testing.T) {
	cfg := testConfig(t)
	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	occupied, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("occupy %s: %v", address, err)
	}
	defer occupied.Close()

	err = run(context.Background(), cfg, quietLogger(), fakeRuntime(fake.NewRuntime(), &built{}))
	if err == nil {
		t.Fatal("run returned no error although another listener held its port")
	}
	if !strings.Contains(err.Error(), address) {
		t.Errorf("the refusal does not name the address it could not take: %v", err)
	}
	if count, _ := boots(t, cfg); count != 0 {
		t.Fatalf("boots after a start that never served = %d, want 0", count)
	}
}

// The API the daemon serves answers from the store it just migrated: this is the whole of
// `legion state` and of the plugin's read.
func TestRunServesTheStateOfItsOwnBoot(t *testing.T) {
	cfg := testConfig(t)
	d := startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &built{}))

	state := d.state()
	if state.Daemon.Project != cfg.Project {
		t.Errorf("project = %q, want %q", state.Daemon.Project, cfg.Project)
	}
	if state.Daemon.SchemaVersion < 1 {
		t.Errorf("schema version = %d, want the migrated schema", state.Daemon.SchemaVersion)
	}
	if state.Daemon.Boots != 1 {
		t.Errorf("boots = %d, want this daemon's own first boot", state.Daemon.Boots)
	}
	if state.Daemon.StartedAt.IsZero() {
		t.Error("the running daemon reports no start time")
	}
	if state.Admission.Cap != cfg.AdmissionCap {
		t.Errorf("admission cap = %d, want the configured %d", state.Admission.Cap, cfg.AdmissionCap)
	}
	if len(state.Issues) != 0 {
		t.Errorf("issues = %v, want none before a claim is spawned", state.Issues)
	}

	d.stop()
	if _, err := d.client.Get(d.base + "/healthz"); err == nil {
		t.Error("the API still answers after run returned")
	}
}

// daemon is one run of the daemon in the test's process.
type daemon struct {
	t         *testing.T
	cfg       config.Config
	cancel    context.CancelFunc
	done      chan error
	transport *http.Transport
	client    *http.Client
	base      string
	stopped   bool
}

// startDaemon runs the daemon until the test stops it, and returns once it answers /healthz.
func startDaemon(t *testing.T, cfg config.Config, o overrides) *daemon {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// One transport for every request, so the test can close its own connections before it asks
	// the daemon to stop: net/http gives an idle connection five seconds before a Shutdown may
	// close it, and a connection this client dialled is this client's to clean up.
	transport := &http.Transport{}
	d := &daemon{
		t: t, cfg: cfg, cancel: cancel, done: make(chan error, 1), transport: transport,
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second},
		base:   "http://127.0.0.1:" + strconv.Itoa(cfg.Port),
	}
	go func() { d.done <- run(ctx, cfg, quietLogger(), o) }()
	t.Cleanup(d.stop)
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := d.client.Get(d.base + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return d
			}
		}
		select {
		case err := <-d.done:
			d.stopped = true
			t.Fatalf("the daemon exited before it answered /healthz: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never answered /healthz on %s: %v", d.base, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop cancels the daemon and requires it to return cleanly.
func (d *daemon) stop() {
	d.t.Helper()
	if d.stopped {
		return
	}
	d.stopped = true
	d.transport.CloseIdleConnections()
	d.cancel()
	select {
	case err := <-d.done:
		if err != nil {
			d.t.Fatalf("run: %v", err)
		}
	case <-time.After(15 * time.Second):
		d.t.Fatal("run did not return after its context was cancelled")
	}
}

// request sends one request; a nil body sends none. It returns the status and the body.
func (d *daemon) request(method, path string, body any, operator bool) (int, []byte) {
	d.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			d.t.Fatalf("encode the request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, d.base+path, reader)
	if err != nil {
		d.t.Fatalf("build the request: %v", err)
	}
	if operator {
		req.Header.Set("Authorization", "Bearer "+testOperatorToken)
	}
	response, err := d.client.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answered, err := io.ReadAll(response.Body)
	if err != nil {
		d.t.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	return response.StatusCode, answered
}

func (d *daemon) state() api.State {
	d.t.Helper()
	status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		d.t.Fatalf("GET the state = %d; body %s", status, body)
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		d.t.Fatalf("decode the state %s: %v", body, err)
	}
	return state
}

// claim is the operator's view of one claim.
func (d *daemon) claim(token claim.Token) api.OperatorClaim {
	d.t.Helper()
	status, body := d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true)
	if status != http.StatusOK {
		d.t.Fatalf("list the claims = %d; body %s", status, body)
	}
	var list api.OperatorClaims
	if err := json.Unmarshal(body, &list); err != nil {
		d.t.Fatalf("decode the claims %s: %v", body, err)
	}
	for _, c := range list.Claims {
		if c.Token == token {
			return c
		}
	}
	d.t.Fatalf("the daemon lists no claim %s: %s", token, body)
	return api.OperatorClaim{}
}

// spawn has the operator spawn a claim and returns its token.
func (d *daemon) spawn(req api.SpawnRequest) claim.Token {
	d.t.Helper()
	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", req, true)
	if status != http.StatusCreated {
		d.t.Fatalf("spawn = %d; body %s", status, body)
	}
	var spawned api.OperatorClaim
	if err := json.Unmarshal(body, &spawned); err != nil {
		d.t.Fatalf("decode the spawned claim %s: %v", body, err)
	}
	return spawned.Token
}

// eventually waits, boundedly, for what the daemon does on its own goroutines.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lastLaunch is the spec of the claim's latest launch — the boot token its pane carries.
func lastLaunch(t *testing.T, rt *fake.Runtime, token claim.Token) runtime.SpawnSpec {
	t.Helper()
	calls := rt.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if (calls[i].Method == "Spawn" || calls[i].Method == "Resume") && calls[i].Spec.Claim == token {
			return calls[i].Spec
		}
	}
	t.Fatalf("no launch of %s was made; the runtime saw %v", token, rt.Methods())
	return runtime.SpawnSpec{}
}

// shim is the test standing in for `legion worker-shim` on the daemon's worker stream: it dials
// the socket, says hello with a pane's boot token, and speaks the frames the test tells it to.
type shim struct {
	t      *testing.T
	conn   net.Conn
	lines  *bufio.Reader
	writer *shimwire.Writer
}

// dialShim connects to the worker stream at address and says hello with bootToken, returning
// once the daemon's hello_ack arrives.
func dialShim(t *testing.T, address, bootToken string) *shim {
	t.Helper()
	conn, err := net.Dial("unix", strings.TrimPrefix(address, "unix://"))
	if err != nil {
		t.Fatalf("dial the worker stream %s: %v", address, err)
	}
	t.Cleanup(func() { conn.Close() })
	s := &shim{t: t, conn: conn, lines: bufio.NewReader(conn), writer: shimwire.NewWriter(conn)}
	s.send(shimwire.Hello{BootToken: bootToken})
	if frame := s.next(); frame.FrameType() != shimwire.TypeHelloAck {
		t.Fatalf("the daemon answered the hello with %#v, want hello_ack", frame)
	}
	return s
}

func (s *shim) send(frame shimwire.Frame) {
	s.t.Helper()
	if err := s.writer.WriteFrame(frame); err != nil {
		s.t.Fatalf("send %s: %v", frame.FrameType(), err)
	}
}

// next is the next frame the daemon sends.
func (s *shim) next() shimwire.Frame {
	s.t.Helper()
	if err := s.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		s.t.Fatalf("set the read deadline: %v", err)
	}
	line, err := s.lines.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("read a frame from the daemon: %v", err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		s.t.Fatalf("decode %q: %v", line, err)
	}
	return frame
}

// closed waits for the daemon to close the connection.
func (s *shim) closed() {
	s.t.Helper()
	if err := s.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		s.t.Fatalf("set the read deadline: %v", err)
	}
	if line, err := s.lines.ReadBytes('\n'); err == nil {
		s.t.Fatalf("the daemon sent %q where the connection should have closed", line)
	}
}

// prompt answers the negotiation a connection opens with, when the daemon sends one, and returns
// the prompt frame that follows.
func (s *shim) prompt() shimwire.Prompt {
	s.t.Helper()
	frame := s.next()
	if negotiate, ok := frame.(shimwire.NegotiateProtocol); ok {
		s.send(shimwire.Response{ID: negotiate.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		frame = s.next()
	}
	prompt, ok := frame.(shimwire.Prompt)
	if !ok {
		s.t.Fatalf("the daemon sent %#v, want a prompt", frame)
	}
	return prompt
}

// The workflow has to assemble every integration boundary before opening its API. The order in
// this log is the boot sequence operators rely on when an external dependency refuses.
func TestWorkflowBootLogsItsDependencyOrder(t *testing.T) {
	cfg := workflowConfig(t, workflowNATS(t))
	tokens := &workflowTokenRecorder{}
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, logger, overrides{
			runtime:        fakeRuntime(fake.NewRuntime(), &built{}).runtime,
			clock:          stillClock{},
			workflowTokens: tokens,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not answer /healthz: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := tokens.Roles(); len(got) != 2 || got[0] != appauth.Implement || got[1] != appauth.Review {
		t.Fatalf("App token roles = %v, want implement then review", got)
	}
	want := []string{"store", "config", "appauth", "prompts", "worker-bin", "dispatch", "admission", "intake", "outbox", "api"}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var entry struct {
			Message string `json:"msg"`
			Stage   string `json:"stage"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode boot log line %q: %v", line, err)
		}
		if entry.Message == "legion workflow boot stage" {
			got = append(got, entry.Stage)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("workflow boot stages = %v, want %v\nlog:\n%s", got, want, logged.String())
	}
}

// With no notification stream the daemon would boot with no intake, and nothing Dispatch or GitHub
// says would ever reach it. Boot refuses instead, naming the stream, before the API answers.
func TestRunRefusesToBootWithoutTheNotificationStream(t *testing.T) {
	natsURL := workflowNATS(t)
	if err := workflowJetStream(t, natsURL).DeleteStream(context.Background(), "ENVOY_NOTIFICATIONS"); err != nil {
		t.Fatalf("delete the notification stream: %v", err)
	}
	cfg := workflowConfig(t, natsURL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, quietLogger(), overrides{
			runtime: fakeRuntime(fake.NewRuntime(), &built{}).runtime, clock: stillClock{}, workflowTokens: &workflowTokenRecorder{},
		})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "ENVOY_NOTIFICATIONS") {
			t.Fatalf("run = %v, want a boot refusal naming ENVOY_NOTIFICATIONS", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon booted with no notification stream")
	}
}

// Intake ending while the daemon runs (here its durable consumer is deleted) stops the daemon with
// that error, instead of the daemon serving on with nothing reading Dispatch or GitHub.
func TestRunStopsWithTheErrorWhenItsIntakeEnds(t *testing.T) {
	natsURL := workflowNATS(t)
	cfg := workflowConfig(t, natsURL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, quietLogger(), overrides{
			runtime: fakeRuntime(fake.NewRuntime(), &built{}).runtime, clock: stillClock{}, workflowTokens: &workflowTokenRecorder{},
		})
	}()
	stream, err := workflowJetStream(t, natsURL).Stream(context.Background(), "ENVOY_NOTIFICATIONS")
	if err != nil {
		t.Fatalf("open the notification stream: %v", err)
	}
	consumer := "legion-go-" + cfg.Project + "-dispatch"
	eventually(t, "intake pulling from the durable Dispatch consumer", func() bool {
		durable, err := stream.Consumer(context.Background(), consumer)
		if err != nil {
			return false
		}
		info, err := durable.Info(context.Background())
		return err == nil && info.NumWaiting > 0
	})
	if err := stream.DeleteConsumer(context.Background(), consumer); err != nil {
		t.Fatalf("delete %s: %v", consumer, err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), consumer) {
			t.Fatalf("run = %v, want the daemon stopped by its intake naming %s", err, consumer)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon kept running after its intake ended")
	}
}

// workflowConfig is a Stage 3 configuration over natsURL and a Dispatch that lists no issues.
func workflowConfig(t *testing.T, natsURL string) config.Config {
	t.Helper()
	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]any{}); err != nil {
				t.Errorf("write Dispatch issues: %v", err)
			}
		default:
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(dispatchServer.Close)
	cfg := testConfig(t)
	cfg.DispatchURL = dispatchServer.URL
	cfg.DispatchTokenFile = filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte("dispatch-test-token\n"), 0o600); err != nil {
		t.Fatalf("write Dispatch token: %v", err)
	}
	cfg.Projects = map[string]config.Project{cfg.Project: {Repo: "acme/widgets"}}
	cfg.NatsURLs = []string{natsURL}
	return cfg
}

func workflowJetStream(t *testing.T, natsURL string) jetstream.JetStream {
	t.Helper()
	conn, err := nats.Connect(natsURL, nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

func TestRunRefusesMissingRolePromptBundleAtBoot(t *testing.T) {
	cfg := testConfig(t)
	rolesDir := t.TempDir()
	t.Setenv("LEGION_ROLE_PROMPTS_DIR", rolesDir)

	err := run(context.Background(), cfg, quietLogger(), fakeRuntime(fake.NewRuntime(), &built{}))

	if err == nil {
		t.Fatal("run succeeded with no role prompt files")
	}
	for _, want := range []string{rolesDir, "architect-root.md", "controller-root.md", "architect.md", "planner.md", "implementer.md", "tester.md", "reviewer.md", "merger.md", "core/common.md", "core/planner.md", "core/implementer.md", "core/tester.md", "core/reviewer.md", "core/oracle.md", "mechanics/headless.md", "mechanics/interactive.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("boot refusal = %q, want %q", err, want)
		}
	}
}

type workflowTokenRecorder struct {
	mu    sync.Mutex
	roles []appauth.AppRole
}

func (r *workflowTokenRecorder) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roles = append(r.roles, role)
	return appauth.Lease{Token: "workflow-test-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (r *workflowTokenRecorder) Roles() []appauth.AppRole {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]appauth.AppRole(nil), r.roles...)
}

func workflowNATS(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, err := tcnats.Run(ctx, "nats:2.10")
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("start NATS JetStream: %v", err)
	}
	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	var conn *nats.Conn
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err = nats.Connect(url, nats.Timeout(time.Second))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect NATS after its container started: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}
	return url
}
