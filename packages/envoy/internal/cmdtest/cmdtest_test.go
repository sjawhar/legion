package cmdtest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/listenfd"
)

const (
	// heldPortChildEnv makes this test binary run heldPortChild instead of its tests.
	heldPortChildEnv = "CMDTEST_HELD_PORT_CHILD"
	// heldPortFD is the variable Pass names the child's inherited socket in.
	heldPortFD = "CMDTEST_LISTEN_FD"
)

func TestMain(m *testing.M) {
	if os.Getenv(heldPortChildEnv) == "1" {
		os.Exit(heldPortChild())
	}
	os.Exit(m.Run())
}

// heldPortChild is the process TestHeldPortStaysHeldUntilItsProcessServes starts. It waits for a
// line on standard input, then serves one connection on the socket it inherited, answering its own
// pid, and exits once the test closes that connection, so the test is the one to close first.
func heldPortChild() int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "held-port child: "+format+"\n", args...)
		return 1
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		return fail("wait for the line to serve: %v", err)
	}
	socket, err := listenfd.Inspect(heldPortFD, os.Getenv(heldPortFD))
	if err != nil {
		return fail("%v", err)
	}
	listener, err := listenfd.Adopt(socket)
	if err != nil {
		return fail("%v", err)
	}
	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		return fail("accept: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%d\n", os.Getpid()); err != nil {
		return fail("answer: %v", err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return fail("wait for the test to close the connection: %v", err)
	}
	return 0
}

// A held port stays bound from HoldPort until the process it was passed to serves on it, with only
// that process holding it once Start returns: another program's bind in the meantime is refused,
// the process serves on the socket, and the port is free again once it exits.
func TestHeldPortStaysHeldUntilItsProcessServes(t *testing.T) {
	held := HoldPort(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), heldPortChildEnv+"=1")
	held.Pass(cmd, heldPortFD)
	serve, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child := Start(t, "the held-port child", cmd)

	// The child is waiting for its line and has not served, so only its inherited copy can hold the
	// port.
	competitor, err := net.Listen("tcp", held.Addr)
	if err == nil {
		_ = competitor.Close()
		t.Fatalf("another listener bound %s after Start, before the child served; want address already in use", held.Addr)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("another listener's bind of %s before the child served: %v; want address already in use", held.Addr, err)
	}

	if _, err := io.WriteString(serve, "serve\n"); err != nil {
		t.Fatalf("tell the child to serve: %v\n%s", err, child.Output.String())
	}
	_ = serve.Close()
	conn, err := net.Dial("tcp", held.Addr)
	if err != nil {
		t.Fatalf("dial %s: %v\n%s", held.Addr, err, child.Output.String())
	}
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	answer, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read the answer on %s: %v\n%s", held.Addr, err, child.Output.String())
	}
	if got, want := strings.TrimSpace(answer), strconv.Itoa(cmd.Process.Pid); got != want {
		_ = conn.Close()
		t.Fatalf("%s answered pid %q; want the child's %s", held.Addr, got, want)
	}
	_ = conn.Close()
	child.WaitExit(t, "the test closed its connection")
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("the child exited %d:\n%s", code, child.Output.String())
	}

	// The test's cleanup has not run yet, so a bind that succeeds now shows Start closed the test's
	// copy, and the child released its own on exit.
	rebound, err := net.Listen("tcp", held.Addr)
	if err != nil {
		t.Fatalf("bind %s after the child exited: %v; want it free", held.Addr, err)
	}
	_ = rebound.Close()
}

// probeProgram is the main package the build tests build, printing the line its source names, so
// that a binary built from one source and a binary built from another tell themselves apart.
const probeProgram = `package main

import "fmt"

func main() { fmt.Println(%q) }
`

// brokenProgram is a main package that does not compile, so that a go build of it fails.
const brokenProgram = "package main\n\nthis is not Go\n"

// probeModule writes a module holding probeProgram printing line, and makes its directory the
// test's working directory, which is the package Build builds. It returns the directory, for a
// test that rewrites the sources. GOWORK is off because the module is the test's own, not one of
// this repository's workspace.
func probeModule(t *testing.T, line string) string {
	t.Helper()
	dir := t.TempDir()
	writeProbe(t, filepath.Join(dir, "go.mod"), "module probe\n\ngo 1.27\n")
	writeProbe(t, filepath.Join(dir, "main.go"), fmt.Sprintf(probeProgram, line))
	t.Setenv("GOWORK", "off")
	t.Chdir(dir)
	return dir
}

func writeProbe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runProbe runs the binary and returns the line it printed.
func runProbe(t *testing.T, binary string) string {
	t.Helper()
	out, err := exec.Command(binary).CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", binary, err, out)
	}
	return strings.TrimSpace(string(out))
}

// probeBinary is what TestBuildBuildsEachBinaryOncePerTestProcess built, read by the test after
// it, which runs the binary once that test has ended.
var probeBinary string

// A test process builds each binary once: a second caller gets the first caller's binary and no
// second go build runs, which sources that no longer compile show, and a binary of another name
// is a build of its own.
func TestBuildBuildsEachBinaryOncePerTestProcess(t *testing.T) {
	dir := probeModule(t, "built once")
	first := Build(t, "probe")
	if line := runProbe(t, first); line != "built once" {
		t.Fatalf("the binary printed %q; want %q", line, "built once")
	}

	// A go build of this package fails from here on, so a caller that ran one would fail the
	// test rather than answer a binary's path.
	writeProbe(t, filepath.Join(dir, "main.go"), brokenProgram)
	if again := Build(t, "probe"); again != first {
		t.Fatalf("the second call answered %s; want the first call's %s", again, first)
	}
	probeBinary = first
}

// The binary a test built outlives that test, because the tests after it run it.
func TestABinaryAnEarlierTestBuiltIsStillThere(t *testing.T) {
	if probeBinary == "" {
		t.Skip("TestBuildBuildsEachBinaryOncePerTestProcess did not run")
	}
	if line := runProbe(t, probeBinary); line != "built once" {
		t.Fatalf("the binary printed %q; want %q", line, "built once")
	}
}

// Callers that ask for one binary at the same time all get that one finished binary: each runs
// it, so none was handed a path to a build still being written.
func TestBuildHandsConcurrentCallersOneFinishedBinary(t *testing.T) {
	probeModule(t, "built for all")
	paths := make([]string, 8)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			paths[i] = Build(t, "concurrent-probe")
		}()
	}
	close(start)
	wg.Wait()
	for i, path := range paths {
		if path != paths[0] {
			t.Fatalf("caller %d got %s; want caller 0's %s", i, path, paths[0])
		}
		if line := runProbe(t, path); line != "built for all" {
			t.Fatalf("caller %d ran a binary printing %q; want %q", i, line, "built for all")
		}
	}
}

// A build that failed fails every caller with that one failure, and is never answered as a
// binary a later caller can run.
func TestABuildThatFailedFailsEveryCallerTheSameWay(t *testing.T) {
	dir := probeModule(t, "never built")
	writeProbe(t, filepath.Join(dir, "main.go"), brokenProgram)
	first := buildFailure(t, "broken-probe")
	if !strings.Contains(first, "build broken-probe: ") {
		t.Fatalf("the build failed with %q; want a failure naming the binary", first)
	}
	if second := buildFailure(t, "broken-probe"); second != first {
		t.Fatalf("the second caller failed with %q; want the first caller's %q", second, first)
	}
}

// buildFailure runs Build on a stand-in for a test and returns what it failed with. Build ends a
// failed build's caller as Fatal ends a test, so the call runs on a goroutine of its own.
func buildFailure(t *testing.T, name string) string {
	t.Helper()
	stub := &fatalTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		binary := Build(stub, name)
		stub.failure = fmt.Sprintf("the build answered %s instead of failing", binary)
	}()
	<-done
	return stub.failure
}

// fatalTB is the test a failing build is given: it records what the build failed with and ends
// the goroutine the call runs on, as testing's own Fatal ends a test's.
type fatalTB struct {
	testing.TB
	failure string
}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatal(args ...any) {
	f.failure = fmt.Sprint(args...)
	runtime.Goexit()
}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.failure = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
