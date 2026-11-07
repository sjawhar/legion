// Package cmdtest builds a command's binary and runs it as a process a test drives, for the tests
// of more than one command. It is an ordinary package rather than a _test.go file because every
// command is package main, and none can import another's test helpers.
package cmdtest

import (
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

	"github.com/sjawhar/envoy/internal/logtest"
)

// Build builds the main package in the test's working directory, its own command's, into a binary
// named name that is removed when the test ends.
func Build(t testing.TB, name string) string {
	t.Helper()
	return BuildCommand(t, Command{Name: name})
}

// BuildStamped is Build with the binary stamped with commit as the image build stamps it:
// -ldflags "-X main.buildCommit=<commit>", the variable cmd/dispatch reports on /healthz and
// registers its process under. A test that needs processes serving different builds builds one
// binary per commit.
func BuildStamped(t testing.TB, name, commit string) string {
	t.Helper()
	return BuildCommand(t, Command{Name: name, Flags: []string{"-ldflags", "-X main.buildCommit=" + commit}})
}

// Command is a binary to build: Name is what the binary is called under the test's own
// directory, Dir the directory go build runs in (the test's own when empty), Target what it
// builds ("." when empty), Flags the flags before the target, and Env the whole environment of
// the go command (the process's when nil).
type Command struct {
	Name   string
	Dir    string
	Target string
	Flags  []string
	Env    []string
}

// built is the binary each test has already built, by builtKey. go build relinks the whole
// binary into every -o path it is given - about a second apiece on a warm build cache, minutes on
// a loaded machine - and a row that starts five processes of one command asked for five of them
// (INFRA-985), so a test builds each command once and hands every process of that test the same
// binary. The key holds the test itself, so no binary outlives the TempDir it was written into,
// and the entry goes when the test ends.
var built sync.Map

// builtKey is one test's build of one command: the test, and everything that decides what go
// build writes - the command's name, directory, target, flags and environment, and the overlay.
type builtKey struct {
	t    testing.TB
	args string
}

// builtBinary is one memoised build: once makes it, path is what it wrote, empty where it failed.
type builtBinary struct {
	once sync.Once
	path string
}

// BuildCommand builds c and returns the binary's path. The binary is removed when the test ends,
// and ENVOY_TEST_GO_OVERLAY, when set, is passed to go build -overlay, so a probe can build it
// from changed sources without editing the tree. A test that asks for the same build again is
// handed the one it already built (built).
func BuildCommand(t testing.TB, c Command) string {
	t.Helper()
	overlay := os.Getenv("ENVOY_TEST_GO_OVERLAY")
	// The count of the flags stands between them and the environment, so a value that moves from
	// one of the two to the other is another key rather than the same one.
	parts := append([]string{c.Name, c.Dir, c.Target, overlay, strconv.Itoa(len(c.Flags))}, c.Flags...)
	key := builtKey{t: t, args: strings.Join(append(parts, c.Env...), "\x00")}
	entry, loaded := built.LoadOrStore(key, &builtBinary{})
	if !loaded {
		t.Cleanup(func() { built.Delete(key) })
	}
	binary := entry.(*builtBinary)
	binary.once.Do(func() { binary.path = runBuild(t, c, overlay) })
	if binary.path == "" {
		t.Fatalf("build %s: an earlier build of it in this test failed", c.Name)
	}
	return binary.path
}

// runBuild is one go build of c, into a binary named c.Name under a directory of this test's
// own. It returns the empty path where the build failed, having failed the test.
func runBuild(t testing.TB, c Command, overlay string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), c.Name)
	args := []string{"build", "-o", binary}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	args = append(args, c.Flags...)
	target := c.Target
	if target == "" {
		target = "."
	}
	args = append(args, target)
	cmd := exec.Command("go", args...)
	cmd.Dir, cmd.Env = c.Dir, c.Env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("build %s: %v\n%s", c.Name, err, out)
		return ""
	}
	return binary
}

// Process is a binary a test started, with its combined output.
type Process struct {
	Cmd    *exec.Cmd
	Output *LockedBuffer
	// Exited is closed once the process has exited and Cmd.ProcessState is set.
	Exited <-chan struct{}
	// name is what failures call the process.
	name string
}

// Start starts cmd, which failures call name, with its standard output and error captured in
// Output, and kills it when the test ends if it is still running. It closes every file in
// cmd.ExtraFiles once cmd.Start returns, whether the process started or not (CloseExtraFiles).
func Start(t testing.TB, name string, cmd *exec.Cmd) *Process {
	t.Helper()
	exited := make(chan struct{})
	process := &Process{Cmd: cmd, Output: &LockedBuffer{}, Exited: exited, name: name}
	cmd.Stdout, cmd.Stderr = process.Output, process.Output
	err := cmd.Start()
	CloseExtraFiles(cmd)
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	return process
}

// CloseExtraFiles closes the test's copy of every file in cmd.ExtraFiles. Call it once cmd.Start
// returns, whether the process started or not: a started process holds its own copies from then
// on, so a socket a HeldPort passed it is released when it exits.
func CloseExtraFiles(cmd *exec.Cmd) {
	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}
}

// Terminate sends the process SIGTERM, as a deploy stops it, and returns when it was sent.
func (p *Process) Terminate(t testing.TB) time.Time {
	t.Helper()
	signalled := time.Now()
	if err := p.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM %s: %v", p.name, err)
	}
	return signalled
}

// WaitExit waits up to 30 s for the process to exit, and fails the test with its output when it is
// still running; after names what it was waiting on.
func (p *Process) WaitExit(t testing.TB, after string) {
	t.Helper()
	select {
	case <-p.Exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s was still running 30s after %s:\n%s", p.name, after, p.Output.String())
	}
}

// WaitForOutput waits up to 30 s for each of lines to appear in the process's output, and fails the
// test when the process exits first or a line never appears.
func (p *Process) WaitForOutput(t testing.TB, lines ...string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for _, line := range lines {
		for !strings.Contains(p.Output.String(), line) {
			select {
			case <-p.Exited:
				// Exited closes only once the output is whole, so this read is final.
				if strings.Contains(p.Output.String(), line) {
					continue
				}
				t.Fatalf("%s exited before it logged %q:\n%s", p.name, line, p.Output.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never logged %q:\n%s", p.name, line, p.Output.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// HeldPort is a loopback TCP port the kernel chose, held by a listening socket from the moment
// HoldPort returns until the process the test passes it to serves on it, so no other program can
// take the port in between.
type HeldPort struct {
	// Port is the port the kernel chose.
	Port int
	// Addr is the address the socket listens on, 127.0.0.1:Port.
	Addr string
	// file holds the listening socket until Start has handed it to the process.
	file *os.File
}

// HoldPort opens a listening socket on a loopback port the kernel chooses, for the test to hand to
// the process it starts with Pass. The test's copy closes once Start has started that process, or
// when the test ends.
func HoldPort(t testing.TB) *HeldPort {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	// The file is a descriptor of its own for the socket, so the socket stays listening once the
	// listener is closed.
	file, err := listener.(*net.TCPListener).File()
	_ = listener.Close()
	if err != nil {
		t.Fatalf("hold a loopback port: take the listening socket's descriptor: %v", err)
	}
	// A File closed twice closes its descriptor once, so this is safe after Start has closed it.
	t.Cleanup(func() { _ = file.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	return &HeldPort{Port: addr.Port, Addr: addr.String(), file: file}
}

// Pass hands the held socket to cmd: it adds the socket to cmd.ExtraFiles and sets envName to the
// descriptor number the process inherits it as. It appends to cmd.Env, or to this process's
// environment when cmd.Env is nil, so call it after cmd.Env is assigned. Start then closes the
// test's copy.
func (h *HeldPort) Pass(cmd *exec.Cmd, envName string) {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// ExtraFiles entry i is descriptor 3+i in the process.
	cmd.Env = append(cmd.Env, envName+"="+strconv.Itoa(3+len(cmd.ExtraFiles)))
	cmd.ExtraFiles = append(cmd.ExtraFiles, h.file)
}

// LockedBuffer is a buffer a process's output, or a logger, writes from more than one goroutine
// while a test reads it. Its body is logtest.Buffer's.
type LockedBuffer = logtest.Buffer
