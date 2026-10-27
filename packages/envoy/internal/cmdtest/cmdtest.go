// Package cmdtest builds a command's binary and runs it as a process a test drives, for the tests
// of more than one command. It is an ordinary package rather than a _test.go file because every
// command is package main, and none can import another's test helpers.
package cmdtest

import (
	"bytes"
	"math/rand/v2"
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
)

// Build builds the main package in the test's working directory, its own command's, into a binary
// named name that is removed when the test ends. ENVOY_TEST_GO_OVERLAY, when set, is passed to go
// build -overlay, so a probe can build the binary from changed sources without editing the tree.
func Build(t testing.TB, name string) string {
	t.Helper()
	return build(t, name)
}

// BuildStamped is Build with the binary stamped with commit as the image build stamps it:
// -ldflags "-X main.buildCommit=<commit>", the variable cmd/dispatch reports on /healthz and
// registers its process under. A test that needs processes serving different builds builds one
// binary per commit.
func BuildStamped(t testing.TB, name, commit string) string {
	t.Helper()
	return build(t, name, "-ldflags", "-X main.buildCommit="+commit)
}

// build is Build with flags passed to go build before the package.
func build(t testing.TB, name string, flags ...string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), name)
	args := []string{"build", "-o", binary}
	if overlay := os.Getenv("ENVOY_TEST_GO_OVERLAY"); overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	args = append(args, flags...)
	args = append(args, ".")
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
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
// Output, and kills it when the test ends if it is still running.
func Start(t testing.TB, name string, cmd *exec.Cmd) *Process {
	t.Helper()
	exited := make(chan struct{})
	process := &Process{Cmd: cmd, Output: &LockedBuffer{}, Exited: exited, name: name}
	cmd.Stdout, cmd.Stderr = process.Output, process.Output
	if err := cmd.Start(); err != nil {
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

// handedOut is every port FreeTCPPort has handed to a test that has not ended yet.
var (
	handedOutMu sync.Mutex
	handedOut   = map[int]bool{}
)

// freePortLow and freePortHigh bound the ports FreeTCPPort hands out: below the range a kernel
// takes ports from for itself (Linux's default 32768-60999, and the 49152-65535 macOS and Windows
// use), for a listener on port 0 and for an outgoing connection's source port alike. So no socket
// the kernel places a port for, a booting process's own connection to its database among them,
// takes one of these while its process boots.
const (
	freePortLow  = 20000
	freePortHigh = 32767
)

// FreeTCPPort is a loopback port, for a process to serve on, that nothing listened on a moment ago
// and that FreeTCPPort hands to no other caller of this binary until the test it went to ends.
//
// The port is free from the moment this returns until its process binds it. In CI run 40162140539
// a test drew a second port in that window and was handed the first again: one process of a pair
// exited on "address already in use" for port 35881 while a health check of that port passed,
// which of the servers that test runs only the pair's other process could have answered. So this
// chooses each port number itself and skips one it handed out before binding anything. A listener
// on port 0 would bind the port before it could be checked, and the process the port went to could
// lose its own bind in that moment.
//
// Only the callers of this function are kept apart: a socket bound by number to a port in this
// range can still take the port before its process binds it, and that process then exits on the
// bind.
func FreeTCPPort(t testing.TB) int {
	t.Helper()
	return freeTCPPortFrom(t, func() int { return freePortLow + rand.IntN(freePortHigh-freePortLow+1) })
}

// freeTCPPortFrom is FreeTCPPort with the port numbers it tries coming from candidate, which a
// test replaces to propose a port it chose.
func freeTCPPortFrom(t testing.TB, candidate func() int) int {
	t.Helper()
	handedOutMu.Lock()
	defer handedOutMu.Unlock()
	var refused error
	for range 1000 {
		port := candidate()
		if handedOut[port] {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			refused = err
			continue
		}
		_ = listener.Close()
		handedOut[port] = true
		t.Cleanup(func() {
			handedOutMu.Lock()
			defer handedOutMu.Unlock()
			delete(handedOut, port)
		})
		return port
	}
	t.Fatalf("pick a port: 1000 tries found none free that this binary has not handed out; the last bind said %v", refused)
	return 0
}

// LockedBuffer is a buffer a process's output, or a logger, writes from more than one goroutine
// while a test reads it.
type LockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *LockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
