package launcher

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
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
	select {}
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
}

func newRig(t *testing.T) *rig {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	g := &rig{t: t, listener: listener, private: t.TempDir(), cancel: cancel, result: make(chan error, 1)}
	go func() {
		g.result <- Run(ctx, Config{
			Connect: "tcp://" + listener.Addr().String(), Token: "launcher-token", Sandbox: "legion-legion-legion-208",
			Role: "tester", PodUID: "pod-1", PrivateDir: g.private,
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
