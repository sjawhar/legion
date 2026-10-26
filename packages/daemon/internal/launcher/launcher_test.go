package launcher

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const launcherChildEnv = "LEGION_LAUNCHER_TEST_CHILD"

// TestLauncherChild is the worker-shim stand-in the launcher starts: it records what its
// credential pointer reads and whether its generation's session socket was already bound when it
// ran, starts a descendant in its process group, and waits.
func TestLauncherChild(t *testing.T) {
	if os.Getenv(launcherChildEnv) != "1" {
		return
	}
	marker := os.Getenv("LEGION_LAUNCHER_MARKER")
	if marker == "" {
		os.Exit(2)
	}
	seen := "unreadable"
	if body, err := os.ReadFile(os.Getenv("ENVOY_TOKEN_FILE")); err == nil {
		seen = string(body)
	}
	socket := "unbound"
	if _, err := os.Stat(os.Getenv("LEGION_LAUNCHER_SOCKET")); err == nil {
		socket = "bound"
	}
	child := exec.Command("/bin/sh", "-c", "exec sleep 600")
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(marker, []byte(strconv.Itoa(child.Process.Pid)+" "+socket+" "+seen), 0o600); err != nil {
		os.Exit(4)
	}
	// Wait on the descendant rather than block in select{}: a goroutine parked forever with nothing
	// else outstanding is the Go runtime's deadlock, fatal under gVisor, while a blocking wait is not.
	_ = child.Wait()
	os.Exit(0)
}

// rig is one launcher under test and the daemon end of its connection.
type rig struct {
	t        *testing.T
	listener net.Listener
	conn     net.Conn
	reader   *bufio.Reader
	writer   *shimwire.Writer
	private  string
	cancel   context.CancelFunc
	result   chan error
	hello    shimwire.LauncherHello
	latest   shimwire.LauncherState
	// stderr is what the launcher and its children wrote to Config.Stderr.
	stderr *lockedBuffer
}

// lockedBuffer is a bytes.Buffer the launcher and its children can write while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newSessionRig(t, Config{})
}

// newSessionRig runs a launcher that carries cfg's session database and dialer, the fake standing
// in for the Cloud SQL connector, besides the fields every rig's launcher is given.
func newSessionRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	// The private directory's path is kept short deliberately: a generation's session socket is
	// bound under it, and a unix socket's path has barely a hundred bytes, where t.TempDir()'s
	// carries the whole of the test's name.
	private, err := os.MkdirTemp("", "lch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(private) })
	ctx, cancel := context.WithCancel(context.Background())
	g := &rig{t: t, listener: listener, private: private, cancel: cancel, result: make(chan error, 1), stderr: &lockedBuffer{}}
	cfg.Connect, cfg.Token, cfg.Sandbox = "tcp://"+listener.Addr().String(), "launcher-token", "legion-legion-legion-208"
	cfg.Role, cfg.PodUID, cfg.PrivateDir, cfg.Stderr = "tester", "pod-1", g.private, g.stderr
	go func() {
		g.result <- Run(ctx, cfg)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-g.result:
		case <-time.After(5 * time.Second):
			t.Error("the launcher did not leave after its context ended")
		}
	})
	g.accept()
	return g
}

// accept takes the launcher's next connection through its hello, acknowledgement and state.
func (g *rig) accept() shimwire.LauncherState {
	g.t.Helper()
	conn, err := g.listener.Accept()
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = conn.Close() })
	g.conn, g.reader, g.writer = conn, bufio.NewReader(conn), shimwire.NewWriter(conn)
	hello, ok := g.next().(shimwire.LauncherHello)
	if !ok || hello.Token != "launcher-token" || hello.Role != "tester" || hello.PodUID != "pod-1" || hello.LauncherID == "" {
		g.t.Fatalf("hello = %#v", hello)
	}
	if g.hello.LauncherID != "" && hello.LauncherID != g.hello.LauncherID {
		g.t.Fatalf("reconnect hello is launcher %s, want %s again", hello.LauncherID, g.hello.LauncherID)
	}
	g.hello = hello
	g.send(shimwire.LauncherHelloAck{})
	state, ok := g.next().(shimwire.LauncherState)
	if !ok {
		g.t.Fatalf("after the acknowledgement: %#v, want the launcher's state", state)
	}
	return state
}

func (g *rig) next() shimwire.Frame {
	g.t.Helper()
	if err := g.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		g.t.Fatal(err)
	}
	line, err := g.reader.ReadBytes('\n')
	if err != nil {
		g.t.Fatal(err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		g.t.Fatal(err)
	}
	if state, ok := frame.(shimwire.LauncherState); ok {
		g.latest = state
	}
	return frame
}

func (g *rig) send(frame shimwire.Frame) {
	g.t.Helper()
	if err := g.writer.WriteFrame(frame); err != nil {
		g.t.Fatal(err)
	}
}

// answer sends command and returns its answer, skipping state frames.
func (g *rig) answer(command shimwire.Frame) shimwire.Frame {
	g.t.Helper()
	g.send(command)
	return g.reply()
}

// reply is the next answer the launcher sends, skipping state frames.
func (g *rig) reply() shimwire.Frame {
	g.t.Helper()
	for {
		frame := g.next()
		if _, state := frame.(shimwire.LauncherState); !state {
			return frame
		}
	}
}

func (g *rig) start(command shimwire.LauncherStart) shimwire.LauncherStartResult {
	g.t.Helper()
	result, ok := g.answer(command).(shimwire.LauncherStartResult)
	if !ok {
		g.t.Fatalf("start answer is %#v", result)
	}
	return result
}

func (g *rig) stop(generation uint64) {
	g.t.Helper()
	id := "stop-" + strconv.FormatUint(generation, 10)
	if got, ok := g.answer(shimwire.LauncherStop{ID: id, Generation: generation, GraceMs: 1000}).(shimwire.LauncherStopResult); !ok || !got.OK || got.ID != id {
		g.t.Fatalf("stop result = %#v", got)
	}
}

// generationDir is where the launcher keeps generation's credentials and binds its session socket.
func (g *rig) generationDir(generation uint64) string {
	return generationDir(g.private, generation)
}

// childStart is a start of the stand-in child for generation, reading its Envoy credential
// through the pointer the daemon names, looking for its generation's session socket where the
// launcher binds it, and publishing both to marker.
func (g *rig) childStart(generation uint64, marker string, files map[string]string) shimwire.LauncherStart {
	g.t.Helper()
	self, err := os.Executable()
	if err != nil {
		g.t.Fatal(err)
	}
	dir := g.generationDir(generation)
	return shimwire.LauncherStart{
		ID: "start-" + strconv.FormatUint(generation, 10), Generation: generation,
		Argv: []string{self, "-test.run=^TestLauncherChild$"},
		Env: []string{launcherChildEnv + "=1", "LEGION_LAUNCHER_MARKER=" + marker,
			"ENVOY_TOKEN_FILE=" + filepath.Join(dir, "ENVOY_TOKEN"),
			"LEGION_LAUNCHER_SOCKET=" + filepath.Join(dir, ompsessions.SocketName)},
		Files: files,
	}
}

// observed is what the stand-in child saw when it ran: the pid of the descendant it started,
// whether its generation's session socket was already bound, and what its credential pointer read.
type observed struct {
	descendant int
	socket     string
	credential string
}

// published waits for a marker of the form `<pid> <rest>` and returns the pid and the rest,
// verbatim. The stand-in child and the namespace tests' shell scripts both write such markers.
func published(t *testing.T, marker string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(marker); err == nil {
			pid, rest, _ := strings.Cut(string(body), " ")
			if n, err := strconv.Atoi(pid); err == nil {
				return n, rest
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the child never published its marker")
	return 0, ""
}

// publishedAll waits for the stand-in child's `<pid> <socket> <credential>` marker and reports
// the whole of it.
func publishedAll(t *testing.T, marker string) observed {
	t.Helper()
	descendant, rest := published(t, marker)
	socket, credential, _ := strings.Cut(rest, " ")
	return observed{descendant: descendant, socket: socket, credential: credential}
}

func gone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// A started generation's credentials are written owner-only into its own fresh directory, its
// child reads them through the pointer the daemon named, a lost daemon connection keeps the child
// and the launcher reports it again, and a stop ends the whole process group and the directory.
func TestAGenerationsCredentialsLiveExactlyAsLongAsItsChild(t *testing.T) {
	g := newRig(t)
	marker := filepath.Join(t.TempDir(), "marker")
	files := map[string]string{"LEGION_BOOT_TOKEN": "boot-7", "ENVOY_TOKEN": "envoy-7"}
	if got := g.start(g.childStart(7, marker, files)); !got.OK || got.RunningGeneration != 7 {
		t.Fatalf("start result = %#v", got)
	}
	child := publishedAll(t, marker)
	descendant := child.descendant
	if child.credential != "envoy-7" {
		t.Fatalf("the child read %q through ENVOY_TOKEN_FILE, want its generation's credential", child.credential)
	}
	dir := filepath.Join(g.private, "g7")
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("generation directory %s: %v, %v", dir, info, err)
	}
	for name, want := range files {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v, %v; want an owner-only regular file", name, info, err)
		}
		if body, _ := os.ReadFile(filepath.Join(dir, name)); string(body) != want {
			t.Fatalf("%s holds the wrong bytes", name)
		}
	}

	_ = g.conn.Close()
	if state := g.accept(); state.Child == nil || state.Child.Generation != 7 {
		t.Fatalf("state after reconnect = %#v, want generation 7 still running", state)
	}
	if err := syscall.Kill(descendant, 0); err != nil {
		t.Fatalf("the reconnect stopped descendant %d: %v", descendant, err)
	}

	g.stop(7)
	if !gone(descendant) {
		t.Fatalf("the launcher left descendant %d running", descendant)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generation 7's credentials outlived it: %v", err)
	}
}

// The launcher writes one line when the daemon acknowledges its hello, and one more at each later
// acknowledgement, naming the daemon it reached and the launcher it is: a connected launcher's log
// reads apart from one still dialing. A connection the daemon ends before acknowledging the hello
// writes none.
func TestTheLauncherLogsOneLineForEachAcknowledgedHello(t *testing.T) {
	g := newRig(t)
	connected := func() []string {
		var lines []string
		for _, line := range strings.Split(g.stderr.String(), "\n") {
			if strings.HasPrefix(line, "launcher: connected to the daemon") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	address := g.listener.Addr().String()
	if lines := connected(); len(lines) != 1 || !strings.Contains(lines[0], address) || !strings.Contains(lines[0], g.hello.LauncherID) {
		t.Fatalf("after the first acknowledgement the launcher logged %q, want one line naming %s and launcher %s", lines, address, g.hello.LauncherID)
	}

	_ = g.conn.Close()
	unacknowledged, err := g.listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if err := unacknowledged.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(unacknowledged).ReadBytes('\n'); err != nil {
		t.Fatalf("the hello of the connection the daemon ends unacknowledged: %v", err)
	}
	_ = unacknowledged.Close()
	g.accept()
	if lines := connected(); len(lines) != 2 {
		t.Fatalf("after a dropped and an acknowledged hello the launcher logged %q, want a second line for the acknowledged one alone", lines)
	}
	if !strings.Contains(g.stderr.String(), "launcher: daemon connection: hello acknowledgement:") {
		t.Fatalf("the launcher logged %q, want the unacknowledged hello's reconnect", g.stderr.String())
	}
}

// Nothing already in the private directory is followed or kept: a symlink the previous agent
// planted at the next generation's path toward the shared workspace receives no credential, and an
// earlier generation's directory a crashed launcher left is removed.
func TestAPlantedPathCannotRedirectCredentials(t *testing.T) {
	g := newRig(t)
	workspace := t.TempDir()
	if err := os.Symlink(workspace, filepath.Join(g.private, "g8")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(g.private, "g3"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.private, "g3", "ENVOY_TOKEN"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(8, marker, map[string]string{"ENVOY_TOKEN": "envoy-8"})); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	published(t, marker)
	if entries, _ := os.ReadDir(workspace); len(entries) != 0 {
		t.Fatalf("a credential reached the planted link's target: %v", entries)
	}
	if info, err := os.Lstat(filepath.Join(g.private, "g8")); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("g8 is %v (%v), want a fresh directory", info, err)
	}
	if _, err := os.Lstat(filepath.Join(g.private, "g3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an earlier generation's credentials survived: %v", err)
	}
	g.stop(8)
}

// A replayed start of the live generation answers as before and rewrites nothing; the same id with
// another payload, credentials included, is refused before anything is written; and a generation
// that already ran is never started again.
func TestStartsAreIdempotentAndRefusedBeforeWriting(t *testing.T) {
	g := newRig(t)
	marker := filepath.Join(t.TempDir(), "marker")
	start := g.childStart(9, marker, map[string]string{"ENVOY_TOKEN": "envoy-9"})
	if got := g.start(start); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	published(t, marker)
	credential := filepath.Join(g.private, "g9", "ENVOY_TOKEN")
	if err := os.WriteFile(credential, []byte("touched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := g.start(start); !got.OK || got.RunningGeneration != 9 {
		t.Fatalf("replay result = %#v, want the first answer", got)
	}
	if body, _ := os.ReadFile(credential); string(body) != "touched" {
		t.Fatal("a replayed start rewrote the running generation's credentials")
	}
	changed := start
	changed.Files = map[string]string{"ENVOY_TOKEN": "envoy-other"}
	if got := g.start(changed); got.OK || !strings.Contains(got.Error, "different payload") || strings.Contains(got.Error, "envoy-other") {
		t.Fatalf("changed replay result = %#v, want a refusal that names no credential", got)
	}
	if body, _ := os.ReadFile(credential); string(body) != "touched" {
		t.Fatal("a refused start wrote credentials")
	}
	g.stop(9)
	again := g.childStart(9, marker, map[string]string{"ENVOY_TOKEN": "envoy-9"})
	again.ID = "start-9-again"
	if got := g.start(again); got.OK || !strings.Contains(got.Error, "already exited") {
		t.Fatalf("restart of an exited generation = %#v", got)
	}
	if _, err := os.Lstat(filepath.Join(g.private, "g9")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused stale start wrote its directory: %v", err)
	}
}

// toldRecreated starts generation with env (resuming resume, "" for none), returns the
// WorkspaceRecreatedVariable value its child was started with, and stops it.
func (g *rig) toldRecreated(generation uint64, env []string, resume string) string {
	g.t.Helper()
	told := filepath.Join(g.t.TempDir(), "told")
	command := shimwire.LauncherStart{
		ID: "start-" + strconv.FormatUint(generation, 10), Generation: generation,
		Argv: []string{"/bin/sh", "-c", `printf '%s' "$` + WorkspaceRecreatedVariable + `" > "$TOLD.tmp" && mv "$TOLD.tmp" "$TOLD"; exec sleep 600`},
		Env:  append([]string{"TOLD=" + told}, env...), ResumeFile: resume,
	}
	if got := g.start(command); !got.OK {
		g.t.Fatalf("start of generation %d = %#v", generation, got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if body, err := os.ReadFile(told); err == nil {
			g.stop(generation)
			return string(body)
		}
		if time.Now().After(deadline) {
			g.t.Fatalf("generation %d's child never said what it was told", generation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newWorkspace is a directory shaped as a workspace for the creation record (workspace.Created).
func newWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testInstance is the Cloud SQL instance a test's session database sits in. This repository is
// public, so it is a placeholder, as is every login and database name beside it.
const testInstance = "example-project:us-central1:example-instance"

// testDatabase is the session database a launcher under test proxies its generations to.
var testDatabase = ompsessions.Database{Instance: testInstance, Name: "legion_sessions", User: "legion-sessions@example-project.iam"}

// sessionDialer stands in for the Cloud SQL connector: every connection is a fresh dial of addr,
// which the test owns, and check is the credential work the connector does before each of them, so
// a test can refuse one dial and allow the next. While stalling (stallAt), every dial reaches a
// silent upstream instead and stalls in a real TLS handshake, which, like the connector's own, ends
// only when the dial's context does.
type sessionDialer struct {
	addr  string
	check func() error

	mu     sync.Mutex
	dials  []string
	silent string
}

func (d *sessionDialer) Dial(ctx context.Context, instance string) (net.Conn, error) {
	d.mu.Lock()
	d.dials = append(d.dials, instance)
	silent := d.silent
	d.mu.Unlock()
	if silent != "" {
		return stalledDial(ctx, silent)
	}
	if d.check != nil {
		if err := d.check(); err != nil {
			return nil, err
		}
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.addr)
}

// stallAt makes every dial from now on stall against the silent upstream at addr; "" ends that.
func (d *sessionDialer) stallAt(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.silent = addr
}

func (d *sessionDialer) Close() error { return nil }

// build is the Config.SessionDialer a test passes: every generation of its launcher dials through
// this very fake.
func (d *sessionDialer) build(context.Context) (sessionproxy.Dialer, error) { return d, nil }

// stalledDial opens a real connection to addr, a silent upstream, and runs a real TLS handshake
// over it, which nothing ever answers, so it ends only when ctx does.
func stalledDial(ctx context.Context, addr string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	client := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "x"})
	if err := client.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

// silentUpstream is an instance that never answers: a listener that accepts every connection and
// never writes to it, holding each one open until the test ends.
func silentUpstream(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return listener.Addr().String()
}

// idleUpstream is a listener that accepts a connection and closes it, which is all a proof dial
// and a bound-socket check need: no Postgres speaks over it.
func idleUpstream(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

// A session database named only in part is refused before the launcher does anything at all, and
// the refusal names each flag that is missing: all three open the database, and a pod that proxied
// to half of one would run an agent against nothing.
func TestAPartlyNamedSessionDatabaseIsRefusedBeforeTheLauncherStarts(t *testing.T) {
	for name, tc := range map[string]struct {
		database ompsessions.Database
		want     string
	}{
		"the instance alone": {ompsessions.Database{Instance: testInstance},
			"launcher: the session database is named in part: no --session-database, no --session-user"},
		"no login": {ompsessions.Database{Instance: testInstance, Name: testDatabase.Name},
			"launcher: the session database is named in part: no --session-user"},
		"no instance": {ompsessions.Database{Name: testDatabase.Name, User: testDatabase.User},
			"launcher: the session database is named in part: no --session-instance"},
	} {
		t.Run(name, func(t *testing.T) {
			// The connect address is one nothing listens on: a refused configuration must answer
			// before the launcher dials anything.
			err := Run(context.Background(), Config{
				Connect: "tcp://127.0.0.1:1", Token: "launcher-token", Sandbox: "legion-legion-legion-208",
				Role: "tester", PodUID: "pod-1", PrivateDir: t.TempDir(), SessionDatabase: tc.database,
			})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Run = %v, want %q", err, tc.want)
			}
		})
	}
}

// The child of a generation with a session database finds its socket already bound: the proxy is
// up, and has proven one connection to the instance, before anything the agent runs can ask Oh My
// Pi for its session (INFRA-271).
func TestAChildFindsItsGenerationsSessionSocketAlreadyBound(t *testing.T) {
	dialer := &sessionDialer{addr: idleUpstream(t)}
	g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build})
	marker := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(4, marker, nil)); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	if seen := publishedAll(t, marker); seen.socket != "bound" {
		t.Fatalf("the child found its session socket %s, want it already bound", seen.socket)
	}
	g.stop(4)
}

// A generation whose session database cannot be reached fails loudly rather than running an agent
// against something else: the refusal names the generation and the instance, no child runs, the
// generation's directory is gone, and nothing is left listening where its socket would have been.
func TestAGenerationWhoseSessionDatabaseRefusesIsNotStarted(t *testing.T) {
	dialer := &sessionDialer{addr: idleUpstream(t), check: func() error { return errors.New("the instance refused the login") }}
	g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build})
	marker := filepath.Join(t.TempDir(), "marker")
	got := g.start(g.childStart(5, marker, nil))
	if got.OK || !strings.Contains(got.Error, "generation 5") || !strings.Contains(got.Error, testInstance) {
		t.Fatalf("start result = %#v, want a refusal naming generation 5 and %s", got, testInstance)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a generation whose session database refused it started its child")
	}
	dir := g.generationDir(5)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused generation's directory survived: %v", err)
	}
	if conn, err := net.Dial("unix", filepath.Join(dir, ompsessions.SocketName)); err == nil {
		_ = conn.Close()
		t.Fatal("something is still listening at the refused generation's session socket")
	}
}

// A generation whose session database never answers is refused while the daemon still waits: the
// proof dial ends at half the launcher's stop grace, which the daemon's wait on a start always
// outlasts, and the refusal names the generation, the instance and the bound, with nothing of the
// generation left on disk or bound. The command loop is free again: the Stop the daemon sends after
// a start it gave up on, already queued behind that start, is read and obeyed, and the next
// generation, once the instance answers, starts and is served.
func TestAGenerationWhoseSessionDatabaseNeverAnswersIsRefusedWhileTheDaemonWaits(t *testing.T) {
	dialer := &sessionDialer{addr: idleUpstream(t)}
	dialer.stallAt(silentUpstream(t))
	const grace = 400 * time.Millisecond
	g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build, StopGrace: grace})
	marker := filepath.Join(t.TempDir(), "marker")

	began := time.Now()
	g.send(g.childStart(1, marker, nil))
	stop := shimwire.LauncherStop{ID: "stop-1", Generation: 1, GraceMs: 1000}
	g.send(stop)
	got, ok := g.reply().(shimwire.LauncherStartResult)
	if waited := time.Since(began); waited >= grace {
		t.Fatalf("the start was answered %s after it was sent, past the %s stop grace", waited, grace)
	}
	if !ok || got.OK || !strings.Contains(got.Error, "generation 1") || !strings.Contains(got.Error, testInstance) ||
		!strings.Contains(got.Error, "no answer within 200ms") {
		t.Fatalf("start result = %#v, want a refusal naming generation 1, %s and its 200ms bound", got, testInstance)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a generation whose session database never answered started its child")
	}
	dir := g.generationDir(1)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused generation's directory survived: %v", err)
	}
	if conn, err := net.Dial("unix", filepath.Join(dir, ompsessions.SocketName)); err == nil {
		_ = conn.Close()
		t.Fatal("something is still listening at the refused generation's session socket")
	}
	if stopped, ok := g.reply().(shimwire.LauncherStopResult); !ok || !stopped.OK || stopped.ID != stop.ID {
		t.Fatalf("stop result = %#v, want the queued stop obeyed", stopped)
	}

	dialer.stallAt("")
	next := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(2, next, nil)); !got.OK {
		t.Fatalf("start of generation 2 = %#v", got)
	}
	if seen := publishedAll(t, next); seen.socket != "bound" {
		t.Fatalf("generation 2's child found its session socket %s, want it already bound", seen.socket)
	}
	conn, err := net.Dial("unix", filepath.Join(g.generationDir(2), ompsessions.SocketName))
	if err != nil {
		t.Fatalf("dial generation 2's session socket: %v", err)
	}
	_ = conn.Close()
	g.stop(2)
}

// A session database is held for exactly one generation and nothing between: while the generation
// runs its socket is served and its connection file sits owner-only in its own directory, and once
// its child is gone both are too and that address is dead. The generation after it gets a
// directory, a socket and a file of its own.
func TestASessionDatabaseIsHeldForItsGenerationAndNothingBetween(t *testing.T) {
	dialer := &sessionDialer{addr: idleUpstream(t)}
	g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build})
	running := func(generation uint64) (socket, file string) {
		t.Helper()
		marker := filepath.Join(t.TempDir(), "marker")
		if got := g.start(g.childStart(generation, marker, nil)); !got.OK {
			t.Fatalf("start of generation %d = %#v", generation, got)
		}
		publishedAll(t, marker)
		dir := g.generationDir(generation)
		socket, file = filepath.Join(dir, ompsessions.SocketName), filepath.Join(dir, ompsessions.DSNFileName)
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatalf("dial generation %d's session socket: %v", generation, err)
		}
		_ = conn.Close()
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("generation %d's connection file: %v, %v; want an owner-only regular file", generation, info, err)
		}
		return socket, file
	}
	ended := func(generation uint64, socket, file string) {
		t.Helper()
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			t.Fatalf("generation %d's session socket is still served after it ended", generation)
		}
		if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("generation %d's connection file outlived it: %v", generation, err)
		}
	}

	first, firstFile := running(6)
	g.stop(6)
	ended(6, first, firstFile)
	second, secondFile := running(7)
	g.stop(7)
	ended(7, second, secondFile)
}

// On the container's shutdown a generation's session database outlives its child and no longer: a
// wedged agent that ignores SIGTERM is still served through its stop grace, is killed at the grace,
// and only then is its proxy closed and its directory removed, before Run returns. The proxy's
// lifetime is the child's plus the generation's own cleanup, bounded by the grace, never the
// container's context.
func TestOnTheContainersShutdownASessionDatabaseEndsOnceItsWedgedChildIsKilled(t *testing.T) {
	dialer := &sessionDialer{addr: idleUpstream(t)}
	const grace = 500 * time.Millisecond
	g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build, StopGrace: grace})
	ready := filepath.Join(t.TempDir(), "ready")
	// An ignored signal stays ignored across fork and exec, so neither the shell nor its sleeps
	// answer SIGTERM: only the SIGKILL at the grace ends the generation.
	start := shimwire.LauncherStart{
		ID: "start-1", Generation: 1,
		Argv: []string{"/bin/sh", "-c", `trap '' TERM; printf '%s ' "$$" > "$READY.tmp" && mv "$READY.tmp" "$READY"; while :; do sleep 0.05; done`},
		Env:  []string{"READY=" + ready},
	}
	if got := g.start(start); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	pid, _ := published(t, ready)
	dir := g.generationDir(1)
	socket := filepath.Join(dir, ompsessions.SocketName)
	upstreamDials := func() int {
		dialer.mu.Lock()
		defer dialer.mu.Unlock()
		return len(dialer.dials)
	}
	before := upstreamDials()

	g.cancel()
	stopped := time.Now()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("the session socket refused a dial during the wedged child's stop grace: %v", err)
	}
	_ = conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for upstreamDials() == before {
		if time.Now().After(deadline) {
			t.Fatal("the proxy accepted a connection during the stop grace but never dialed the upstream for it")
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-g.result:
		// The rig's cleanup reads the result too.
		g.result <- err
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want the container's shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the wedged child's stop grace")
	}
	if waited := time.Since(stopped); waited < grace {
		t.Fatalf("Run returned %s after the shutdown, inside the %s grace a child ignoring SIGTERM is owed", waited, grace)
	}
	if !gone(pid) {
		t.Fatalf("the wedged child %d outlived its stop grace", pid)
	}
	if conn, err := net.Dial("unix", socket); err == nil {
		_ = conn.Close()
		t.Fatal("the session socket is still served after Run returned")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the generation's directory outlived Run: %v", err)
	}
}

// echoUpstream is an instance that answers every connection by echoing it back, and holds it open
// until its far end closes it or the test ends, as a database holds an idle session.
func echoUpstream(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
			go func() {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return listener.Addr().String()
}

// holdSessionSocket connects the test process itself to a generation's session socket, a holder
// outside the generation's process group as a setsid'd psql would be, shows by a round trip that
// the connection reaches the upstream, and returns it held open.
func holdSessionSocket(t *testing.T, socket string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the generation's session socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through the session socket: %v", err)
	}
	echoed := make([]byte, len("ping"))
	if _, err := io.ReadFull(conn, echoed); err != nil || string(echoed) != "ping" {
		t.Fatalf("the upstream echoed %q, %v; want ping", echoed, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	return conn
}

// expectHeldClosed fails unless the proxy has closed a held connection: a read on it ends with no
// bytes, within a bound and not for want of time.
func expectHeldClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	var timeout net.Error
	if n != 0 || err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("the held connection read %q, %v; want it closed", buf[:n], err)
	}
}

// A socket holder outside the generation's process group cannot hold the generation open on the
// container's shutdown. Off PID 1 the launcher's signals reach only the child's process group and
// nothing reaps the rest, so the holder outlives the child; the proxy closes its connection once
// sessionDrain is over, and Run returns within the stop grace and the drain, with the socket no
// longer served, the generation's directory gone and the holder's connection closed under it.
func TestASocketHolderOutsideTheGenerationCannotHoldItOpenOnTheContainersShutdown(t *testing.T) {
	dialer := &sessionDialer{addr: echoUpstream(t)}
	const grace = 300 * time.Millisecond
	g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build, StopGrace: grace})
	marker := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(1, marker, nil)); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	publishedAll(t, marker)
	dir := g.generationDir(1)
	socket := filepath.Join(dir, ompsessions.SocketName)
	held := holdSessionSocket(t, socket)

	g.cancel()
	bound := grace + sessionDrain + 3*time.Second
	select {
	case err := <-g.result:
		// The rig's cleanup reads the result too.
		g.result <- err
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want the container's shutdown", err)
		}
	case <-time.After(bound):
		t.Fatalf("Run did not return within %s of the shutdown while a holder outside the generation kept its session socket open", bound)
	}
	if conn, err := net.Dial("unix", socket); err == nil {
		_ = conn.Close()
		t.Fatal("the session socket is still served after Run returned")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the generation's directory outlived Run: %v", err)
	}
	expectHeldClosed(t, held)
	if logged := g.stderr.String(); !strings.Contains(logged, "still open after "+sessionDrain.String()) {
		t.Fatalf("the launcher logged %q, want the connection closed after the drain named", logged)
	}
}

// A socket holder outside the generation's process group cannot hold up the daemon's stop either:
// the stop is answered within its grace and the drain, the holder's connection is closed under it,
// and the next generation starts and is served.
func TestASocketHolderOutsideTheGenerationCannotHoldUpTheDaemonsStop(t *testing.T) {
	dialer := &sessionDialer{addr: echoUpstream(t)}
	g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build, StopGrace: 300 * time.Millisecond})
	marker := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(1, marker, nil)); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	publishedAll(t, marker)
	dir := g.generationDir(1)
	held := holdSessionSocket(t, filepath.Join(dir, ompsessions.SocketName))

	began := time.Now()
	// g.stop sends the stop with a one-second grace.
	g.stop(1)
	if waited, bound := time.Since(began), time.Second+sessionDrain+3*time.Second; waited > bound {
		t.Fatalf("the stop was answered %s after it was sent, past %s", waited, bound)
	}
	expectHeldClosed(t, held)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stopped generation's directory survived: %v", err)
	}

	next := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(2, next, nil)); !got.OK {
		t.Fatalf("start of generation 2 = %#v", got)
	}
	if seen := publishedAll(t, next); seen.socket != "bound" {
		t.Fatalf("generation 2's child found its session socket %s, want it already bound", seen.socket)
	}
	// Generation 2's socket carries a round trip to the upstream: the proxy serves it.
	_ = holdSessionSocket(t, filepath.Join(g.generationDir(2), ompsessions.SocketName)).Close()
	g.stop(2)
}

// The connection file a generation's agent reads is the agent's own spelling of that generation's
// endpoint — the socket as the URL's path, where the launcher's own client names its directory —
// and carries no password at all: the login travels in the proxy's certificate exchange.
func TestTheSessionConnectionFileIsTheAgentsSpellingAndCarriesNoPassword(t *testing.T) {
	dialer := &sessionDialer{addr: idleUpstream(t)}
	g := newSessionRig(t, Config{SessionDatabase: testDatabase, SessionDialer: dialer.build})
	marker := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(3, marker, nil)); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	publishedAll(t, marker)
	dir := g.generationDir(3)
	body, err := os.ReadFile(filepath.Join(dir, ompsessions.DSNFileName))
	if err != nil {
		t.Fatal(err)
	}
	want := ompsessions.Endpoint{SocketDir: dir, Database: testDatabase.Name, User: testDatabase.User}.AgentURL()
	if string(body) != want {
		t.Fatalf("the connection file holds %q, want %q", body, want)
	}
	parsed, err := url.Parse(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if password, held := parsed.User.Password(); held {
		t.Fatalf("the connection file carries a password: %q", password)
	}
	if parsed.User.Username() != testDatabase.User {
		t.Fatalf("the connection file signs in as %q, want %q", parsed.User.Username(), testDatabase.User)
	}
	g.stop(3)
}
