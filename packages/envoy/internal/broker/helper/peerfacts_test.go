// packages/envoy/internal/broker/helper/peerfacts_test.go
//go:build linux

package helper

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// forgedSessionID is what the child process says about itself in its own environment, one of the
// two places this test makes it lie about what it is; its argv is the other. A nine-line C
// program on this fleet rewrote its own /proc/<pid>/environ the same way, which is why a claim
// read out of a process's own words is not one the broker may render as the kernel's.
const forgedSessionID = "forged-by-this-process-aaaaaXXXX"

// liarBinary hard-links (or copies) the running test binary into dir under the literal name
// "the-real-binary", so a test can name the executable the kernel must report as a literal path
// while the child process claims a different one in its argv.
func liarBinary(t *testing.T, dir string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "the-real-binary")
	if err := os.Link(self, path); err == nil {
		return path
	}
	src, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, src, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// startLiar runs the test binary again, from path, with an argv[0] that is a lie and an
// environment that claims a session id it was never given, working in workDir. The child
// connects to sock and stays alive until the test kills it.
func startLiar(t *testing.T, path, workDir, sock string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(path, "-test.run=TestPeerFactsChildProcess")
	cmd.Args = []string{"/opt/not-the-real-thing/omp", "-test.run=TestPeerFactsChildProcess"}
	cmd.Path = path
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"PEER_FACTS_CHILD_SOCKET="+sock,
		"OMP_SESSION_ID="+forgedSessionID,
		"ENVOY_SESSION_ID="+forgedSessionID,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// TestPeerFactsChildProcess is the child startLiar runs: it dials the socket its environment
// names and blocks, so the parent can pin it and read the kernel's answers about it. Run
// directly (no socket named), it does nothing.
func TestPeerFactsChildProcess(t *testing.T) {
	sock := os.Getenv("PEER_FACTS_CHILD_SOCKET")
	if sock == "" {
		t.Skip("not the child process of TestPeerFactsAreTheKernelsAnswerNotTheProcesssClaim")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(time.Minute)
}

// TestPeerFactsAreTheKernelsAnswerNotTheProcesssClaim: the facts come from the pinned handle. The
// child runs with an argv[0] of "/opt/not-the-real-thing/omp" and an environment that names a
// session id it invented, and neither reaches the facts: the executable is the binary the kernel
// resolved, the working directory is the one the kernel reports, and the read is stamped with the
// instant it was made.
func TestPeerFactsAreTheKernelsAnswerNotTheProcesssClaim(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	binary := liarBinary(t, dir)
	workDir := filepath.Join(dir, "the-working-directory")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ownCgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	child := startLiar(t, binary, workDir, sock)
	conn, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := PeerOf(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	readAt := time.Unix(1793402651, 0)
	facts, err := peer.Facts(func() time.Time { return readAt })
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if facts.PID != child.Process.Pid {
		t.Fatalf("pid = %d, want the child's %d", facts.PID, child.Process.Pid)
	}
	if facts.Executable != binary {
		t.Fatalf("executable = %q, want the binary the kernel resolved, %q", facts.Executable, binary)
	}
	if facts.WorkingDir != workDir {
		t.Fatalf("working dir = %q, want %q", facts.WorkingDir, workDir)
	}
	if want := strings.TrimSpace(string(ownCgroup)); facts.Cgroup != want {
		t.Fatalf("cgroup = %q, want the cgroup this test's own process runs in, %q", facts.Cgroup, want)
	}
	if !strings.HasPrefix(facts.Cgroup, "0::/") {
		t.Fatalf("cgroup = %q, want the unified hierarchy's line", facts.Cgroup)
	}
	if facts.ReadAt != readAt {
		t.Fatalf("read at = %s, want %s", facts.ReadAt, readAt)
	}
	if facts.StartedAt.Before(before.Add(-2*time.Second)) || facts.StartedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("started at = %s, want the instant the child started, near %s", facts.StartedAt, before)
	}
	if shown := fmt.Sprintf("%+v", facts); strings.Contains(shown, forgedSessionID) ||
		strings.Contains(shown, "not-the-real-thing") {
		t.Fatalf("the facts repeat what the process said about itself: %s", shown)
	}
}

// TestPeerFactsRefuseAProcessTheKernelNoLongerNames: once the pinned process is reaped the handle
// reports no pid, and there is nothing to read. Answering from the pid number at that point would
// read whatever process the kernel has since given that number to.
func TestPeerFactsRefuseAProcessTheKernelNoLongerNames(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	peer, err := PinPID(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.Facts(time.Now); err != nil {
		t.Fatalf("Facts on the live child: %v", err)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for peer.PID() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_, err = peer.Facts(time.Now)
	if err == nil || !strings.Contains(err.Error(), "the pinned process is gone") {
		t.Fatalf("Facts after the child was reaped = %v, want it to refuse", err)
	}
}

// TestAProcDirectoryDescriptorNeverFollowsAReusedPid holds this kernel to what Peer.Facts rests
// on: a /proc/<pid> directory opened while its process was alive answers ESRCH, not another
// process's answers, once that process is gone. Were it otherwise, a pid the kernel reused after
// the open could answer for the reading, which is the whole reason the reading goes through one
// descriptor and not a path built a second time from the number.
func TestAProcDirectoryDescriptorNeverFollowsAReusedPid(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	dirfd, err := unix.Open("/proc/"+strconv.Itoa(child.Process.Pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dirfd)
	if _, err := readLinkAt(dirfd, "exe"); err != nil {
		t.Fatalf("exe through the directory of a live process: %v", err)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	for _, name := range []string{"exe", "cwd"} {
		if _, err := readLinkAt(dirfd, name); !errors.Is(err, unix.ESRCH) {
			t.Fatalf("%s through the directory of a reaped process = %v, want ESRCH", name, err)
		}
	}
	for _, name := range []string{"cgroup", "stat"} {
		if _, err := readFileAt(dirfd, name); !errors.Is(err, unix.ESRCH) {
			t.Fatalf("%s through the directory of a reaped process = %v, want ESRCH", name, err)
		}
	}
}

// TestClockTicksIsTheKernelsOwnAnswer: the start time a fact names is ticks of the kernel's
// USER_HZ, and the helper reads that unit from the auxiliary vector rather than assuming it.
// `getconf CLK_TCK` on this fleet's hosts answers 100, which is what the kernel writes there.
func TestClockTicksIsTheKernelsOwnAnswer(t *testing.T) {
	hz, err := clockTicks()
	if err != nil {
		t.Fatalf("clockTicks: %v", err)
	}
	if hz != 100 {
		t.Fatalf("AT_CLKTCK = %d, want this fleet's 100 (compare `getconf CLK_TCK`)", hz)
	}
}

// TestSignRequestSignsWhatTheKernelSaidAboutTheCaller: the request object the helper hands back
// carries the kernel's answers about the process that asked, read through the pinned handle
// before the ancestry walk releases it, and signed with the session's key — which the asking
// process never holds. The caller here runs with an argv[0] of "/opt/not-the-real-thing/omp" and
// an environment naming a session id it invented; the claims carry neither.
func TestSignRequestSignsWhatTheKernelSaidAboutTheCaller(t *testing.T) {
	r := startRig(t, "")
	dir := shortTempDir(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "child.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	binary := liarBinary(t, dir)
	workDir := filepath.Join(dir, "the-working-directory")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	child := startLiar(t, binary, workDir, filepath.Join(dir, "child.sock"))
	if _, err := ln.AcceptUnix(); err != nil {
		t.Fatal(err)
	}
	r.callerPID.Store(int64(child.Process.Pid))
	if reg := r.call(t, Request{Op: "register", WaitSeconds: 5}); !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register the child: %+v", reg)
	}
	r.callerPID.Store(int64(child.Process.Pid))
	resp := r.call(t, Request{Op: "sign-request", Secrets: []string{"DEEL_API_KEY"}, Reason: "deel sync for ACME-1"})
	if !resp.OK || resp.RequestObject == "" {
		t.Fatalf("sign-request: %+v", resp)
	}
	after := time.Now()
	ro, err := record.VerifyRequestObject(resp.RequestObject, r.srv.Broker.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("VerifyRequestObject: %v", err)
	}
	if ro.Peer == nil {
		t.Fatal("the signed request object carries no kernel facts about the caller")
	}
	if ro.Peer.PID != child.Process.Pid {
		t.Fatalf("peer pid = %d, want the child's %d", ro.Peer.PID, child.Process.Pid)
	}
	if ro.Peer.Executable != binary {
		t.Fatalf("peer executable = %q, want the binary the kernel resolved, %q", ro.Peer.Executable, binary)
	}
	if ro.Peer.WorkingDir != workDir {
		t.Fatalf("peer working dir = %q, want %q", ro.Peer.WorkingDir, workDir)
	}
	if !strings.HasPrefix(ro.Peer.Cgroup, "0::/") {
		t.Fatalf("peer cgroup = %q, want the unified hierarchy's line", ro.Peer.Cgroup)
	}
	if ro.Peer.ReadAt < before.Unix() || ro.Peer.ReadAt > after.Unix() {
		t.Fatalf("peer read at = %d, want between %d and %d", ro.Peer.ReadAt, before.Unix(), after.Unix())
	}
	if ro.Peer.StartedAt < before.Add(-2*time.Second).Unix() || ro.Peer.StartedAt > after.Unix() {
		t.Fatalf("peer started at = %d, want the instant the child started, near %d", ro.Peer.StartedAt, before.Unix())
	}
	if shown := fmt.Sprintf("%+v", *ro.Peer); strings.Contains(shown, forgedSessionID) ||
		strings.Contains(shown, "not-the-real-thing") {
		t.Fatalf("the signed facts repeat what the process said about itself: %s", shown)
	}
}

// TestSignRequestStillSignsWhenTheReadingIsOutsideWhatARecordMayCarry: the broker refuses a peer
// claim whose strings carry a refused rune category, and it refuses the whole request with it.
// A real session working in a directory whose name holds a zero-width joiner must still be able
// to ask for a secret, so the helper leaves the reading out of the request rather than signing
// one the broker will throw away the request over.
func TestSignRequestStillSignsWhenTheReadingIsOutsideWhatARecordMayCarry(t *testing.T) {
	r := startRig(t, "")
	dir := shortTempDir(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "child.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	binary := liarBinary(t, dir)
	workDir := filepath.Join(dir, "zero\u200dwidth")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := startLiar(t, binary, workDir, filepath.Join(dir, "child.sock"))
	if _, err := ln.AcceptUnix(); err != nil {
		t.Fatal(err)
	}
	r.callerPID.Store(int64(child.Process.Pid))
	if reg := r.call(t, Request{Op: "register", WaitSeconds: 5}); !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register the child: %+v", reg)
	}
	r.callerPID.Store(int64(child.Process.Pid))
	resp := r.call(t, Request{Op: "sign-request", Secrets: []string{"DEEL_API_KEY"}, Reason: "deel sync for ACME-1"})
	if !resp.OK || resp.RequestObject == "" {
		t.Fatalf("sign-request from a working directory the broker would refuse: %+v", resp)
	}
	ro, err := record.VerifyRequestObject(resp.RequestObject, r.srv.Broker.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("the request object must still verify: %v", err)
	}
	if ro.Peer != nil {
		t.Fatalf("peer = %+v, want none: the reading is outside what a record may carry", *ro.Peer)
	}
}
