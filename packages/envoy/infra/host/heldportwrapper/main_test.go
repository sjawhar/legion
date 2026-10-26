package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// The wrapper's real-process tests: the wrapper this directory builds, run as the held-port image
// runs it, against the socket server the live tests use (pulumitest.HoldPort) and the real
// envoy-listener of this branch.

// The wrapper takes the socket the test holds and becomes the listener, which serves on it: the
// listener runs in the wrapper's process, and of the socket it holds only its own close-on-exec
// copy, so nothing it starts inherits the port.
func TestTheListenerServesTheHeldSocketInTheWrappersProcess(t *testing.T) {
	skipOffLinux(t)
	wrapper := cmdtest.Build(t, "envoy-listener-heldport")
	listener := buildListener(t)
	held := pulumitest.HoldPort(t)
	port := held.Addr().(*net.TCPAddr).Port
	process := startWrapper(t, wrapper, port, listener)

	if addr, want := listeningAddr(t, process), held.Addr().String(); addr != want {
		t.Fatalf("the listener logged that it listens on %s, not on the held %s:\n%s", addr, want, process.Output.String())
	}
	requireAnswers(t, process, port)

	pid := process.Cmd.Process.Pid
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		t.Fatalf("read what process %d runs: %v", pid, err)
	}
	want, err := filepath.EvalSymlinks(listener)
	if err != nil {
		t.Fatal(err)
	}
	if exe != want {
		t.Fatalf("the wrapper's process %d runs %s, not the listener %s: the wrapper did not execute it in its place", pid, exe, want)
	}

	socket := "socket:[" + listeningInode(t, port) + "]"
	holders := descriptorsOf(t, pid, socket)
	if len(holders) != 1 {
		t.Fatalf("the listener holds the socket on descriptors %v, want exactly one: its own serving copy", holders)
	}
	if holders[0] == 3 {
		t.Fatal("the listener still holds the socket on descriptor 3, the one it inherited, rather than only its own copy")
	}
	if !closeOnExec(t, pid, holders[0]) {
		t.Fatalf("the listener's copy of the socket, descriptor %d, is not close-on-exec, so a process it starts inherits the port", holders[0])
	}
}

// With no test serving the abstract socket, the wrapper exits 1 at once, naming the socket it
// asked, and the port stays the test's alone.
func TestWithNoSocketServedTheWrapperExitsNamingIt(t *testing.T) {
	skipOffLinux(t)
	wrapper := cmdtest.Build(t, "envoy-listener-heldport")
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	port := held.Addr().(*net.TCPAddr).Port
	process := startWrapper(t, wrapper, port, filepath.Join(t.TempDir(), "never-executed"))
	requireFailedNaming(t, process, pulumitest.HeldPortSocket(port))
	requireTheTestsAlone(t, held)
}

// A listener binary that does not exist ends the wrapper with exit 1, naming the socket, and leaves
// no process holding the socket it received: the port is the test's alone again.
func TestATargetThatCannotRunLeavesTheSocketTheTestsAlone(t *testing.T) {
	skipOffLinux(t)
	wrapper := cmdtest.Build(t, "envoy-listener-heldport")
	held := pulumitest.HoldPort(t)
	port := held.Addr().(*net.TCPAddr).Port
	missing := filepath.Join(t.TempDir(), "no-such-listener")
	process := startWrapper(t, wrapper, port, missing)
	requireFailedNaming(t, process, pulumitest.HeldPortSocket(port))
	if output := process.Output.String(); !strings.Contains(output, "execute "+missing) {
		t.Errorf("the wrapper did not say it failed to execute %s:\n%s", missing, output)
	}
	requireTheTestsAlone(t, held)
}

func skipOffLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("the held-port wrapper runs on linux only, and this is %s", runtime.GOOS)
	}
}

// buildListener builds packages/envoy/cmd/listener from this branch as Envoy's own jobs do, in the
// workspace of the repository's root go.work, and returns the binary.
func buildListener(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "envoy-listener")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = filepath.Join("..", "..", "..", "cmd", "listener")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build envoy-listener: %v\n%s", err, out)
	}
	return binary
}

// startWrapper runs the wrapper as the held-port image runs it on the container's PORT, executing
// target, with a NATS URL that nothing serves: the listener serves before it connects, so its
// /healthz answers "starting" while its connect waits on a socket that never answers.
func startWrapper(t *testing.T, wrapper string, port int, target string) *cmdtest.Process {
	t.Helper()
	nats, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a NATS address nothing serves: %v", err)
	}
	t.Cleanup(func() { _ = nats.Close() })
	cmd := exec.Command(wrapper)
	cmd.Env = []string{
		"PORT=" + strconv.Itoa(port),
		"HELDPORT_WRAPPER_EXEC=" + target,
		"ENVOY_MACHINE_ID=heldport-wrapper-test",
		"NATS_URLS=nats://" + nats.Addr().String(),
	}
	return cmdtest.Start(t, "the wrapper", cmd)
}

// listeningRecord matches the listener's JSON "envoy-listener listening" line, capturing its addr.
var listeningRecord = regexp.MustCompile(`"msg":"envoy-listener listening".*"addr":"([^"]*)"`)

// listeningAddr waits for the listener's "envoy-listener listening" line and returns its addr.
func listeningAddr(t *testing.T, process *cmdtest.Process) string {
	t.Helper()
	process.WaitForOutput(t, `"msg":"envoy-listener listening"`)
	match := listeningRecord.FindStringSubmatch(process.Output.String())
	if match == nil {
		t.Fatalf("the listener's listening line carries no addr:\n%s", process.Output.String())
	}
	return match[1]
}

// requireAnswers waits up to 30 s for /healthz on port to answer at all, and fails the test when
// the process exits first or it never does.
func requireAnswers(t *testing.T, process *cmdtest.Process, port int) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	deadline := time.Now().Add(30 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		var response *http.Response
		if response, err = client.Get(url); err == nil {
			_ = response.Body.Close()
			return
		}
		select {
		case <-process.Exited:
			t.Fatalf("the process exited before /healthz answered on %d:\n%s", port, process.Output.String())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("/healthz never answered on %d: %v\n%s", port, err, process.Output.String())
}

// requireFailedNaming waits up to 10 s for the wrapper to exit, and fails the test unless it exited
// 1 naming socket.
func requireFailedNaming(t *testing.T, process *cmdtest.Process, socket string) {
	t.Helper()
	select {
	case <-process.Exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the wrapper was still running 10s after it started:\n%s", process.Output.String())
	}
	output := process.Output.String()
	if code := process.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("the wrapper exited %d, want 1:\n%s", code, output)
	}
	if !strings.Contains(output, socket) {
		t.Errorf("the wrapper's refusal does not name %s:\n%s", socket, output)
	}
}

// requireTheTestsAlone fails the test when a process other than the test's holds the socket held
// listens on, then closes the test's copy and requires a bind of its address to succeed: nothing
// kept the port.
func requireTheTestsAlone(t *testing.T, held net.Listener) {
	t.Helper()
	addr := held.Addr().String()
	socket := "socket:[" + listeningInode(t, held.Addr().(*net.TCPAddr).Port) + "]"
	processes, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range processes {
		pid, err := strconv.Atoi(filepath.Base(dir))
		if err != nil || pid == os.Getpid() {
			continue
		}
		if fds := descriptorsOf(t, pid, socket); len(fds) > 0 {
			t.Errorf("process %d holds the socket on %s on descriptors %v", pid, addr, fds)
		}
	}
	if err := held.Close(); err != nil {
		t.Fatalf("close the test's copy of the socket on %s: %v", addr, err)
	}
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("bind %s once the test closed its copy: %v", addr, err)
	}
	_ = again.Close()
}

// listeningInode returns the inode of the socket listening on 127.0.0.1:port, from /proc/net/tcp.
func listeningInode(t *testing.T, port int) string {
	t.Helper()
	file, err := os.Open("/proc/net/tcp")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// local_address is the IPv4 address as the host's byte order prints it (127.0.0.1 is 0100007F
	// on the little-endian machines the tests run on) and the port, in hexadecimal; st 0A is LISTEN,
	// and the tenth field is the inode.
	local := fmt.Sprintf("0100007F:%04X", port)
	var inodes []string
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) > 9 && fields[1] == local && fields[3] == "0A" {
			inodes = append(inodes, fields[9])
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if len(inodes) != 1 {
		t.Fatalf("/proc/net/tcp lists %d sockets listening on 127.0.0.1:%d, want exactly one", len(inodes), port)
	}
	return inodes[0]
}

// descriptorsOf returns the descriptors of process pid that are socket, read from /proc/<pid>/fd:
// none for a process that has exited or whose descriptors this test may not read.
func descriptorsOf(t *testing.T, pid int, socket string) []int {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var fds []int
	for _, entry := range entries {
		if target, err := os.Readlink(filepath.Join(dir, entry.Name())); err == nil && target == socket {
			fd, err := strconv.Atoi(entry.Name())
			if err != nil {
				t.Fatalf("%s lists %q", dir, entry.Name())
			}
			fds = append(fds, fd)
		}
	}
	return fds
}

// closeOnExec reports whether descriptor fd of process pid is close-on-exec, from the octal flags
// line of /proc/<pid>/fdinfo/<fd>.
func closeOnExec(t *testing.T, pid, fd int) bool {
	t.Helper()
	info, err := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%d", pid, fd))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(info), "\n") {
		if value, found := strings.CutPrefix(line, "flags:"); found {
			flags, err := strconv.ParseUint(strings.TrimSpace(value), 8, 64)
			if err != nil {
				t.Fatalf("fdinfo of descriptor %d of process %d has flags %q", fd, pid, value)
			}
			return flags&syscall.O_CLOEXEC != 0
		}
	}
	t.Fatalf("fdinfo of descriptor %d of process %d has no flags line:\n%s", fd, pid, info)
	return false
}
