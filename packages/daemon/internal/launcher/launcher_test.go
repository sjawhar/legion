package launcher

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const launcherChildEnv = "LEGION_LAUNCHER_TEST_CHILD"

// TestLauncherChild is the worker-shim stand-in the launcher starts: it records what its
// credential pointer reads, starts a descendant in its process group, and waits.
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
	child := exec.Command("/bin/sh", "-c", "exec sleep 600")
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(marker, []byte(strconv.Itoa(child.Process.Pid)+" "+seen), 0o600); err != nil {
		os.Exit(4)
	}
	// Wait on the descendant rather than block in select{}: a goroutine parked forever with nothing
	// else outstanding is the Go runtime's deadlock, fatal under gVisor, while a blocking wait is not.
	_ = child.Wait()
	os.Exit(0)
}

// rig is one single-role launcher under test and the daemon end of its connection.
type rig struct {
	t        *testing.T
	listener net.Listener
	private  string
	cancel   context.CancelFunc
	result   chan error
	// roleConn is the daemon end of the role's current connection, which accept replaces.
	*roleConn
	// stderr is what the launcher and its children wrote to Config.Stderr.
	stderr *lockedBuffer
}

// roleConn is the daemon end of one role's launcher connection: the frames it reads, the hello
// that opened it and the last state the role reported on it.
type roleConn struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
	writer *shimwire.Writer
	hello  shimwire.LauncherHello
	latest shimwire.LauncherState
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	g := &rig{t: t, listener: listener, private: t.TempDir(), cancel: cancel, result: make(chan error, 1), stderr: &lockedBuffer{}}
	go func() {
		g.result <- Run(ctx, Config{
			Connect: "tcp://" + listener.Addr().String(), Token: "launcher-token", Sandbox: "legion-legion-legion-208",
			Role: "tester", PodUID: "pod-1", PrivateDir: g.private, Stderr: g.stderr,
		})
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
	hello := shimwire.LauncherHello{}
	if g.roleConn != nil {
		hello = g.hello
	}
	conn := acceptRole(g.t, g.listener)
	if conn.hello.Token != "launcher-token" || conn.hello.Role != "tester" || conn.hello.PodUID != "pod-1" {
		g.t.Fatalf("hello = %#v", conn.hello)
	}
	if hello.LauncherID != "" && conn.hello.LauncherID != hello.LauncherID {
		g.t.Fatalf("reconnect hello is launcher %s, want %s again", conn.hello.LauncherID, hello.LauncherID)
	}
	g.roleConn = conn
	return conn.acknowledge()
}

// acceptRole takes one launcher connection up to its hello: the daemon end of the role that
// hello names, which has not been acknowledged yet.
func acceptRole(t *testing.T, listener net.Listener) *roleConn {
	t.Helper()
	if tcp, ok := listener.(*net.TCPListener); ok {
		if err := tcp.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &roleConn{t: t, conn: conn, reader: bufio.NewReader(conn), writer: shimwire.NewWriter(conn)}
	hello, ok := c.next().(shimwire.LauncherHello)
	if !ok || hello.Role == "" || hello.LauncherID == "" {
		t.Fatalf("hello = %#v", hello)
	}
	c.hello = hello
	return c
}

// acknowledge admits the connection's hello and reads the launcher's first state on it.
func (g *roleConn) acknowledge() shimwire.LauncherState {
	g.t.Helper()
	g.send(shimwire.LauncherHelloAck{})
	state, ok := g.next().(shimwire.LauncherState)
	if !ok {
		g.t.Fatalf("after the acknowledgement: %#v, want the launcher's state", state)
	}
	return state
}

func (g *roleConn) next() shimwire.Frame {
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

func (g *roleConn) send(frame shimwire.Frame) {
	g.t.Helper()
	if err := g.writer.WriteFrame(frame); err != nil {
		g.t.Fatal(err)
	}
}

// answer sends command and returns its answer, skipping state frames.
func (g *roleConn) answer(command shimwire.Frame) shimwire.Frame {
	g.t.Helper()
	g.send(command)
	for {
		frame := g.next()
		if _, state := frame.(shimwire.LauncherState); !state {
			return frame
		}
	}
}

func (g *roleConn) start(command shimwire.LauncherStart) shimwire.LauncherStartResult {
	g.t.Helper()
	result, ok := g.answer(command).(shimwire.LauncherStartResult)
	if !ok {
		g.t.Fatalf("start answer is %#v", result)
	}
	return result
}

func (g *roleConn) stop(generation uint64) {
	g.t.Helper()
	id := "stop-" + strconv.FormatUint(generation, 10)
	if got, ok := g.answer(shimwire.LauncherStop{ID: id, Generation: generation, GraceMs: 1000}).(shimwire.LauncherStopResult); !ok || !got.OK || got.ID != id {
		g.t.Fatalf("stop result = %#v", got)
	}
}

// childStart is a start of the stand-in child for generation, reading its Envoy credential
// through the pointer the daemon names, and publishing its descendant's pid to marker.
func (g *rig) childStart(generation uint64, marker string, files map[string]string) shimwire.LauncherStart {
	g.t.Helper()
	self, err := os.Executable()
	if err != nil {
		g.t.Fatal(err)
	}
	dir := filepath.Join(g.private, "g"+strconv.FormatUint(generation, 10))
	return shimwire.LauncherStart{
		ID: "start-" + strconv.FormatUint(generation, 10), Generation: generation,
		Argv:  []string{self, "-test.run=^TestLauncherChild$"},
		Env:   []string{launcherChildEnv + "=1", "LEGION_LAUNCHER_MARKER=" + marker, "ENVOY_TOKEN_FILE=" + filepath.Join(dir, "ENVOY_TOKEN")},
		Files: files,
	}
}

// published waits for the child's marker: its descendant's pid and what its credential read.
func published(t *testing.T, marker string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(marker); err == nil {
			pid, seen, _ := strings.Cut(string(body), " ")
			if n, err := strconv.Atoi(pid); err == nil {
				return n, seen
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the child never published its marker")
	return 0, ""
}

// gone says whether a process has ended. A process whose parent never reaps it stays a zombie,
// which still takes a signal, so the state its /proc entry reports decides: in a container whose
// PID 1 is the test's own stand-in rather than a reaping init — the probe pod runs `sleep` —
// every ended descendant the launcher killed is a zombie until the container exits.
func gone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if zombie(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// zombie says whether pid has ended and is only waiting to be reaped: the third field of
// /proc/<pid>/stat, after the command in parentheses, which may itself hold spaces.
func zombie(pid int) bool {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	_, after, ok := strings.Cut(string(body), ") ")
	return ok && strings.HasPrefix(after, "Z")
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
	descendant, seen := published(t, marker)
	if seen != "envoy-7" {
		t.Fatalf("the child read %q through ENVOY_TOKEN_FILE, want its generation's credential", seen)
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

// A resume naming a session file the tree volume no longer holds is refused before anything
// starts: no child runs and no generation directory is written, so a missing retained session
// never becomes a fresh agent (LEGION-462). A later generation naming a session file that exists
// starts normally in the same launcher.
func TestStartRefusesAMissingResumeFile(t *testing.T) {
	g := newRig(t)
	marker := filepath.Join(t.TempDir(), "marker")
	absent := filepath.Join(t.TempDir(), "sessions", "absent.marker")
	refused := g.childStart(7, marker, nil)
	refused.ResumeFile = absent
	got := g.start(refused)
	if got.OK {
		t.Fatalf("start result = %#v, want a refusal: a missing resume file must never become a fresh agent", got)
	}
	want := "resume session file " + absent + ": stat " + absent
	if !strings.Contains(got.Error, want) {
		t.Fatalf("refusal = %q, want it to quote the missing session file %q", got.Error, want)
	}
	if _, err := os.Lstat(filepath.Join(g.private, "g7")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused resume wrote its generation directory: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused resume started the child: its marker was published")
	}

	resumeFile := filepath.Join(t.TempDir(), "session")
	if err := os.WriteFile(resumeFile, []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := g.childStart(8, marker, nil)
	fixed.ResumeFile = resumeFile
	if got := g.start(fixed); !got.OK || got.RunningGeneration != 8 {
		t.Fatalf("start with an existing resume file = %#v, want generation 8 to run", got)
	}
	published(t, marker)
	g.stop(8)
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

// A resumed generation is told whether its workspace (LEGION_WORKSPACE) was recreated since its
// session was last written, which the Legion plugin tells the agent on its first turn: "true" once
// provisioning's creation record is later than the session file's last write, "false" when it is
// earlier or there is none. A fresh generation resumes nothing and is told "false", and so is every
// generation whatever the container's own environment says, so no other value reaches the child. A
// record that is empty or not an instant, which an init container killed mid-write can leave, only
// decides the notice: the resume starts, told "false", and the launcher logs the record.
func TestAResumeIsToldWhetherItsWorkspaceWasRecreatedSinceItsSessionWasWritten(t *testing.T) {
	t.Setenv(WorkspaceRecreatedVariable, "true")
	g := newRig(t)
	session := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	written := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(session, written, written); err != nil {
		t.Fatal(err)
	}
	dir := newWorkspace(t)
	env := []string{"LEGION_WORKSPACE=" + dir}

	if got := g.toldRecreated(1, env, session); got != "false" {
		t.Errorf("a resume in a workspace with no creation record was told %q, want false", got)
	}
	if err := workspace.RecordCreated(dir, written.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := g.toldRecreated(2, env, session); got != "false" {
		t.Errorf("a resume in a workspace created before its session's last write was told %q, want false", got)
	}
	if err := workspace.RecordCreated(dir, written.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := g.toldRecreated(3, env, session); got != "true" {
		t.Errorf("a resume in a workspace created after its session's last write was told %q, want true", got)
	}
	if got := g.toldRecreated(4, env, ""); got != "false" {
		t.Errorf("a fresh generation was told %q, want false", got)
	}
	if got := g.toldRecreated(5, nil, session); got != "false" {
		t.Errorf("a resume with no workspace was told %q, want false", got)
	}

	for i, record := range []string{"soon\n", ""} {
		if err := os.WriteFile(filepath.Join(dir, ".jj", "legion-created"), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := g.toldRecreated(uint64(6+i), env, session); got != "false" {
			t.Errorf("a resume in a workspace whose creation record is %q was told %q, want false", record, got)
		}
	}
	if logged := g.stderr.String(); !strings.Contains(logged, filepath.Join(dir, ".jj", "legion-created")) ||
		!strings.Contains(logged, "the workspace is taken as not recreated") {
		t.Errorf("the launcher logged %q, want the unreadable creation record named", logged)
	}
}

// roleToken is the launcher token a roles directory holds for role.
func roleToken(role string) string { return "launcher-token-" + role }

// fleet is one multi-role launcher under test — an issue pod's one container, which serves every
// resident role — and the daemon end of every role's connection.
type fleet struct {
	t        *testing.T
	listener net.Listener
	// rolesDir is the container-level per-role root, home the directory each role's own home
	// overlay covers, and fixed the root its own directories are bound back under.
	rolesDir, home, fixed string
	cancel                context.CancelFunc
	result                chan error
	stderr                *lockedBuffer
	conns                 map[string]*roleConn
}

// newFleet starts a launcher over a roles directory shaped as an issue pod's one container gives
// it — every role's launcher token, gh bundle, private, state and agent-secrets directory — and
// takes every role's connection through its hello, acknowledgement and state. A generation of
// such a launcher runs in namespaces of its own, so a test that starts one calls
// requireRoleNamespaces first.
func newFleet(t *testing.T, grace time.Duration, roles ...string) *fleet {
	t.Helper()
	scratch := namespaceScratch(t)
	f := &fleet{
		t: t, rolesDir: filepath.Join(scratch, "roles"), home: filepath.Join(scratch, "home"),
		fixed: filepath.Join(scratch, "fixed"), result: make(chan error, 1), stderr: &lockedBuffer{},
		conns: map[string]*roleConn{},
	}
	for _, name := range []string{RoleGHDir, RolePrivateDir, RoleStateDir, RoleAgentSecretsDir} {
		if err := os.MkdirAll(filepath.Join(f.fixed, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{f.home, filepath.Join(scratch, "home-mounts")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range roles {
		for _, name := range []string{RoleLauncherDir, RoleGHDir, RolePrivateDir, RoleStateDir, RoleAgentSecretsDir} {
			if err := os.MkdirAll(filepath.Join(f.rolesDir, role, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(f.rolesDir, role, RoleLauncherDir, LauncherTokenFile), []byte(roleToken(role)+"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	useRoleLayout(t, f.home, filepath.Join(scratch, "home-mounts"), f.fixed)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.listener = listener
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() {
		f.result <- Run(ctx, Config{
			Connect: "tcp://" + listener.Addr().String(), Sandbox: "legion-legion-legion-208", PodUID: "pod-1",
			RolesDir: f.rolesDir, Roles: roles, Stdout: f.stderr, Stderr: f.stderr, StopGrace: grace,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.result:
		case <-time.After(30 * time.Second):
			t.Errorf("the launcher did not leave after its context ended; its log: %s", f.stderr.String())
		}
	})
	for range roles {
		conn := acceptRole(t, listener)
		if _, seen := f.conns[conn.hello.Role]; seen {
			t.Fatalf("role %s opened two connections", conn.hello.Role)
		}
		f.conns[conn.hello.Role] = conn
	}
	for _, conn := range f.conns {
		conn.acknowledge()
	}
	return f
}

// role is the daemon end of role's connection.
func (f *fleet) role(name string) *roleConn {
	f.t.Helper()
	conn, ok := f.conns[name]
	if !ok {
		f.t.Fatalf("role %s never connected", name)
	}
	return conn
}

// roleTmp is what the role wrote at /tmp/<name> inside its namespaces, as the container sees it:
// its own temporary directory on the roles directory's disk volume.
func (f *fleet) roleTmp(role, name string) string {
	return filepath.Join(f.rolesDir, RoleHomesDir, role, "tmp", name)
}

// awaitExit reads frames until the role reports generation's exit.
func (g *roleConn) awaitExit(generation uint64) shimwire.LauncherExit {
	g.t.Helper()
	for g.latest.LastExit == nil || g.latest.LastExit.Generation != generation {
		g.next()
	}
	return *g.latest.LastExit
}

// waitForRoleFile is path's content once the role writes it, naming the launcher's log — every
// role's output, role-init's refusals among it — when it never does.
func (f *fleet) waitForRoleFile(path string) string {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
			return string(body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("%s was never written; the launcher's log: %s", path, f.stderr.String())
	return ""
}

// An issue pod's one container holds one authenticated connection per resident role, each with
// that role's own launcher token read from that role's own directory, so the daemon tells the
// roles apart on the worker stream exactly as one container per role did. Each role is its own
// launcher: its id is its own and outlives its own reconnects, and one role's lost connection
// leaves every other role serving.
func TestAMultiRoleLauncherConnectsOncePerRoleWithThatRolesToken(t *testing.T) {
	f := newFleet(t, 2*time.Second, "tester", "reviewer")
	for _, role := range []string{"tester", "reviewer"} {
		hello := f.role(role).hello
		if hello.Token != roleToken(role) || hello.Sandbox != "legion-legion-legion-208" || hello.PodUID != "pod-1" {
			t.Fatalf("role %s hello = %#v, want its own token for this sandbox and pod", role, hello)
		}
	}
	if f.role("tester").hello.LauncherID == f.role("reviewer").hello.LauncherID {
		t.Fatal("the two roles claimed one launcher id; each role's connection is its own launcher")
	}

	first := f.role("tester").hello.LauncherID
	_ = f.role("tester").conn.Close()
	again := acceptRole(t, f.listener)
	if again.hello.Role != "tester" || again.hello.LauncherID != first || again.hello.Token != roleToken("tester") {
		t.Fatalf("the reconnect hello is %#v, want tester as launcher %s again", again.hello, first)
	}
	again.acknowledge()
	f.conns["tester"] = again
	// The reviewer's connection carried on through the tester's: it still answers.
	f.role("reviewer").stop(1)
}

// A stop reaches the role it names and nothing else: the stopped role's processes end, and every
// other role's child keeps running in its own namespaces.
func TestAStopOfOneRolesGenerationLeavesTheOtherRoleRunning(t *testing.T) {
	requireRoleNamespaces(t)
	f := newFleet(t, 10*time.Second, "tester", "reviewer")
	const beating = `while :; do /bin/date +%s%N > /tmp/beat.tmp && /bin/mv /tmp/beat.tmp /tmp/beat; /bin/sleep 0.05; done`
	for i, role := range []string{"tester", "reviewer"} {
		got := f.role(role).start(shimwire.LauncherStart{
			ID: "start-" + role, Generation: uint64(i + 1), Argv: []string{"/bin/sh", "-c", beating},
		})
		if !got.OK {
			t.Fatalf("start of %s = %#v; the launcher's log: %s", role, got, f.stderr.String())
		}
		f.waitForRoleFile(f.roleTmp(role, "beat"))
	}

	f.role("tester").stop(1)
	stopped := f.waitForRoleFile(f.roleTmp("tester", "beat"))
	running := f.waitForRoleFile(f.roleTmp("reviewer", "beat"))
	time.Sleep(500 * time.Millisecond)
	if again := f.waitForRoleFile(f.roleTmp("tester", "beat")); again != stopped {
		t.Fatalf("the tester's processes outlived its stop: its beat moved from %s to %s", stopped, again)
	}
	if again := f.waitForRoleFile(f.roleTmp("reviewer", "beat")); again == running {
		t.Fatalf("the tester's stop ended the reviewer's child too: its beat stayed at %s", running)
	}
	f.role("reviewer").stop(2)
}

// A role's exit is the shim's own, not its namespace init's: the init reports the status it
// waited on, so an exit code and a kill both reach the daemon as the role's.
func TestARolesExitIsReportedWithTheShimsOwnStatus(t *testing.T) {
	requireRoleNamespaces(t)
	f := newFleet(t, 10*time.Second, "tester", "reviewer")
	if got := f.role("tester").start(shimwire.LauncherStart{
		ID: "start-tester", Generation: 1, Argv: []string{"/bin/sh", "-c", "exit 9"},
	}); !got.OK {
		t.Fatalf("start = %#v; the launcher's log: %s", got, f.stderr.String())
	}
	if exit := f.role("tester").awaitExit(1); exit.Code != 9 || exit.Signal != "" {
		t.Fatalf("the tester's exit = %#v, want the shim's own code 9 and no signal; the launcher's log: %s", exit, f.stderr.String())
	}
	if got := f.role("reviewer").start(shimwire.LauncherStart{
		ID: "start-reviewer", Generation: 1, Argv: []string{"/bin/sh", "-c", "kill -9 $$"},
	}); !got.OK {
		t.Fatalf("start = %#v; the launcher's log: %s", got, f.stderr.String())
	}
	if exit := f.role("reviewer").awaitExit(1); exit.Signal != "killed" {
		t.Fatalf("the reviewer's exit = %#v, want the shim's own kill", exit)
	}
}

// The container's SIGTERM stops every role at once, each given the whole stop grace its own
// container had: both roles' handlers run to the end, and the launcher leaves in one grace rather
// than one per role.
func TestTheContainersStopEndsEveryRoleWithinOneStopGrace(t *testing.T) {
	requireRoleNamespaces(t)
	f := newFleet(t, 6*time.Second, "tester", "reviewer")
	const handler = `trap '/bin/sleep 1.5; printf done > /tmp/finished; exit 0' TERM
printf ready > /tmp/ready
while :; do /bin/sleep 600 & wait; done
`
	for i, role := range []string{"tester", "reviewer"} {
		got := f.role(role).start(shimwire.LauncherStart{
			ID: "start-" + role, Generation: uint64(i + 1), Argv: []string{"/bin/sh", "-c", handler},
		})
		if !got.OK {
			t.Fatalf("start of %s = %#v; the launcher's log: %s", role, got, f.stderr.String())
		}
		f.waitForRoleFile(f.roleTmp(role, "ready"))
	}

	started := time.Now()
	f.cancel()
	err := <-f.result
	f.result <- err
	elapsed := time.Since(started)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the launcher returned %v, want its context's end", err)
	}
	for _, role := range []string{"tester", "reviewer"} {
		if body, readErr := os.ReadFile(f.roleTmp(role, "finished")); readErr != nil || string(body) != "done" {
			t.Fatalf("role %s was cut short at the container's stop: %q, %v; the launcher's log: %s", role, body, readErr, f.stderr.String())
		}
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("the container's stop took %s for two roles whose handlers take 1.5s each: the roles are stopped one after another, not together", elapsed)
	}
}

// An issue pod's role names its session under its own home, which the container holds under the
// home mounts directory (layout.go's HomeMountsDir): the launcher, outside every role's namespaces,
// reads the session there. A file at the role's own path in the container's view is not the
// session — the home carries no mount there — so a resume that only it would satisfy is refused,
// naming the session as the role knows it.
func TestARolesResumeIsReadWhereTheContainerHoldsItsHome(t *testing.T) {
	f := newFleet(t, 2*time.Second, "tester")
	session := filepath.Join(f.home, "sessions", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(session), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := f.role("tester").start(shimwire.LauncherStart{
		ID: "start-1", Generation: 1, Argv: []string{"/bin/true"}, ResumeFile: session,
	})
	held := containerPath(session)
	if held == session || !strings.HasPrefix(held, roleHomeMounts+"/") {
		t.Fatalf("the container holds %s at %s, want it under the home mounts %s", session, held, roleHomeMounts)
	}
	if got.OK || !strings.Contains(got.Error, "resume session file "+session+": stat "+session) {
		t.Fatalf("start = %#v, want a refusal naming %s: the session is read under the home mounts, at %s, where nothing is", got, session, held)
	}
}
