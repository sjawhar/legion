package stream_test

// The shim–listener integration test: the real `legion worker-shim` binary, built from
// cmd/legion by this test, dials the real Listener — over a unix socket and over TCP — and
// bridges a fake Oh My Pi, which is this test binary re-executed. Neither end is a stand-in: the
// hello, its ack, the spawn that waits for the ack, the prompt and its turn, the shim's dedupe of a
// repeated delivery, and the reconnect across a daemon restart all run through the code that ships.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// fakeOMPEnv makes this test binary the fake OMP: the shim passes its own environment to the
// process it wraps, so the variable set on the shim reaches the child.
const fakeOMPEnv = "LEGION_STREAM_FAKE_OMP"

const (
	integrationClaim      claim.Token = "legion-integration-legion-1-implementer"
	integrationToken                  = "integration-boot-token"
	integrationGeneration uint64      = 4
	// gapTurns is how many turns the fake OMP reports while no daemon is listening.
	gapTurns = 25
	// integrationWait bounds every wait on the two real processes; each step takes
	// milliseconds when it works, and the shim's redial backoff is 200 ms doubling.
	integrationWait = 15 * time.Second
)

var (
	legionOnce sync.Once
	legionDir  string
	legionErr  error
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeOMPEnv) == "1" {
		os.Exit(fakeOMP())
	}
	code := m.Run()
	if legionDir != "" {
		os.RemoveAll(legionDir)
	}
	os.Exit(code)
}

// fakeOMP speaks the RPC frames the listener sends as Oh My Pi does — the negotiation answered, a
// prompt acknowledged and then its turn reported — records every line it reads, and on SIGUSR1
// reports gapTurns turns of its own. Those turns carry `deliveryId` markers, which OMP's own
// agent_start never does, so the order in which they reach the listener is observable.
func fakeOMP() int {
	var mu sync.Mutex
	emit := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = os.Stdout.WriteString(line + "\n")
	}
	record, err := os.OpenFile(os.Getenv("FAKE_OMP_STDIN"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake omp: open the stdin record: %v\n", err)
		return 3
	}
	pidFile := os.Getenv("FAKE_OMP_PID")
	if err := os.WriteFile(pidFile+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake omp: write the pid: %v\n", err)
		return 3
	}
	if err := os.Rename(pidFile+".tmp", pidFile); err != nil {
		fmt.Fprintf(os.Stderr, "fake omp: publish the pid: %v\n", err)
		return 3
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	go func() {
		for range signals {
			for n := range gapTurns {
				emit(fmt.Sprintf(`{"type":"agent_start","deliveryId":"gap-%d"}`, n))
				emit(`{"type":"agent_end"}`)
			}
		}
	}()

	lines := bufio.NewScanner(os.Stdin)
	lines.Buffer(make([]byte, 64<<10), 2<<20)
	for lines.Scan() {
		line := lines.Bytes()
		_, _ = record.Write(append(bytes.Clone(line), '\n'))
		var frame struct{ Type, ID string }
		_ = json.Unmarshal(line, &frame)
		switch frame.Type {
		case shimwire.TypeNegotiateProtocol, shimwire.TypePrompt:
			emit(fmt.Sprintf(`{"type":"response","id":%q,"command":%q,"success":true}`, frame.ID, frame.Type))
			if frame.Type == shimwire.TypePrompt {
				emit(`{"type":"agent_start"}`)
				emit(`{"type":"agent_end"}`)
			}
		}
	}
	return 0
}

// buildLegion builds cmd/legion once per test binary: the shim under test is the real command.
func buildLegion(t *testing.T) string {
	t.Helper()
	legionOnce.Do(func() {
		if legionDir, legionErr = os.MkdirTemp("", "legion-it-bin"); legionErr != nil {
			return
		}
		out, err := exec.Command("go", "build", "-o", filepath.Join(legionDir, "legion"),
			"github.com/sjawhar/legion/daemon/cmd/legion").CombinedOutput()
		if err != nil {
			legionErr = fmt.Errorf("go build cmd/legion: %v\n%s", err, out)
		}
	})
	if legionErr != nil {
		t.Fatal(legionErr)
	}
	return filepath.Join(legionDir, "legion")
}

// lockedBuffer collects one process's output or one listener's log while the test reads it.
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

// lines is every complete line written so far.
func (b *lockedBuffer) lines() []string {
	text := b.String()
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		return strings.Split(text[:i], "\n")
	}
	return nil
}

// gate is the hello resolver: it refuses the boot token until opened, and counts every hello
// that named it.
type gate struct {
	mu     sync.Mutex
	open   bool
	hellos int
}

func (g *gate) resolve(bootToken string) (claim.Token, uint64, bool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if bootToken != integrationToken {
		return "", 0, false, false
	}
	g.hellos++
	return integrationClaim, integrationGeneration, false, g.open
}

func (g *gate) state() (open bool, hellos int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open, g.hellos
}

func (g *gate) opened() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = true
}

// daemonEnd is one real listener and the log it writes.
type daemonEnd struct {
	listener *stream.Listener
	cancel   context.CancelFunc
	log      *lockedBuffer
}

func listen(t *testing.T, addr string, g *gate) *daemonEnd {
	t.Helper()
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := stream.Listen(ctx, addr, g.resolve, stream.Options{
		RPCTimeout: integrationWait,
		Log:        slog.New(slog.NewTextHandler(log, nil)),
	})
	if err != nil {
		cancel()
		t.Fatalf("Listen(%q): %v", addr, err)
	}
	d := &daemonEnd{listener: listener, cancel: cancel, log: log}
	t.Cleanup(cancel)
	return d
}

// next is the listener's next event.
func (d *daemonEnd) next(t *testing.T) stream.Event {
	t.Helper()
	select {
	case event, ok := <-d.listener.Events():
		if !ok {
			t.Fatal("Events() closed while an event was expected")
		}
		return event
	case <-time.After(integrationWait):
		t.Fatal("no event from the listener within the wait")
		return nil
	}
}

func (d *daemonEnd) expect(t *testing.T, want ...stream.Event) {
	t.Helper()
	for i, w := range want {
		if got := d.next(t); got != w {
			t.Fatalf("event %d of %d = %#v, want %#v", i+1, len(want), got, w)
		}
	}
}

// close ends the listener — the daemon going away — and returns every event it had not yet handed
// out, through the channel's close.
func (d *daemonEnd) close(t *testing.T) []stream.Event {
	t.Helper()
	d.cancel()
	var rest []stream.Event
	deadline := time.After(integrationWait)
	for {
		select {
		case event, ok := <-d.listener.Events():
			if !ok {
				return rest
			}
			rest = append(rest, event)
		case <-deadline:
			t.Fatal("Events() did not close after the listener's context ended")
			return nil
		}
	}
}

// warnings is every line the listener logged other than the rejected hellos this test causes.
func (d *daemonEnd) warnings() []string {
	var other []string
	for _, line := range d.log.lines() {
		if !strings.Contains(line, "worker-stream: rejected hello (unknown boot token)") {
			other = append(other, line)
		}
	}
	return other
}

// eventually polls cond until it holds, failing the test with what it was waiting for.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(integrationWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func count(lines []string, want string) int {
	n := 0
	for _, line := range lines {
		if line == want {
			n++
		}
	}
	return n
}

// childFrames is every line the fake OMP read on its stdin, as the frame type and the delivery id
// a prompt carries.
func childFrames(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the fake OMP's stdin record: %v", err)
	}
	var frames []string
	for line := range bytes.Lines(body) {
		frame, err := shimwire.Decode(line)
		if err != nil {
			t.Fatalf("decode a line the fake OMP read: %v", err)
		}
		entry := frame.FrameType()
		if prompt, ok := frame.(shimwire.Prompt); ok {
			entry += " " + prompt.DeliveryID
		}
		frames = append(frames, entry)
	}
	return frames
}

// The real shim against the real listener, over both address families the listener speaks.
func TestTheRealShimBridgesAFakeOMPToTheRealListener(t *testing.T) {
	legion := buildLegion(t)
	for _, family := range []string{"unix", "tcp"} {
		t.Run(family, func(t *testing.T) {
			addr := "tcp://127.0.0.1:0"
			if family == "unix" {
				dir, err := os.MkdirTemp("", "lit")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(dir) })
				addr = "unix://" + filepath.Join(dir, "worker-stream.sock")
			}
			bridge(t, legion, addr)
		})
	}
}

func bridge(t *testing.T, legion, addr string) {
	work := t.TempDir()
	tokenFile := filepath.Join(work, "boot-token")
	if err := os.WriteFile(tokenFile, []byte(integrationToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdinRecord := filepath.Join(work, "omp-stdin.ndjson")
	pidFile := filepath.Join(work, "omp.pid")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	g := &gate{}
	first := listen(t, addr, g)
	// The address the shim is told, and the one the restarted daemon binds again: under TCP it is
	// the port the kernel chose for the first listener.
	addr = first.listener.Addr()

	shimOut := &lockedBuffer{}
	shim := exec.Command(legion, "worker-shim", "--connect", addr, "--boot-token-file", tokenFile, "--", self)
	shim.Env = append(os.Environ(), fakeOMPEnv+"=1", "FAKE_OMP_STDIN="+stdinRecord, "FAKE_OMP_PID="+pidFile)
	shim.Stdout = shimOut
	shim.Stderr = shimOut
	// The fake OMP writes its stderr into the shim's, so a test that fails with the child alive
	// would otherwise wait on that pipe after the shim is gone.
	shim.WaitDelay = time.Second
	if err := shim.Start(); err != nil {
		t.Fatalf("start the shim: %v", err)
	}
	// exited is closed once the shim has been reaped; waitErr is what Wait returned.
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = shim.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = shim.Process.Kill()
			if pid, err := os.ReadFile(pidFile); err == nil {
				if childPid, err := strconv.Atoi(string(pid)); err == nil {
					_ = syscall.Kill(childPid, syscall.SIGKILL)
				}
			}
			<-exited
		}
		if t.Failed() {
			t.Logf("the shim's output:\n%s", shimOut.String())
		}
	})
	childStarted := func() bool {
		_, err := os.Stat(pidFile)
		return err == nil
	}

	// hello → refused while the token resolves to nothing: the shim redials with its hello, and
	// never spawns OMP without an ack.
	eventually(t, "two hellos refused", func() bool { _, hellos := g.state(); return hellos >= 2 })
	if childStarted() {
		t.Fatal("the shim spawned OMP before any hello was acked")
	}

	// hello → hello_ack → spawn.
	g.opened()
	first.expect(t, stream.Hello{Claim: integrationClaim, Generation: integrationGeneration})
	eventually(t, "the shim to spawn OMP after the ack", childStarted)
	ctx := context.Background()
	await, cancelAwait := context.WithTimeout(ctx, integrationWait)
	defer cancelAwait()
	conn, err := first.listener.Await(await, integrationClaim)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}

	// A prompt is acknowledged, and its turn is observed through Events().
	if err := conn.Prompt(ctx, "delivery-1", "the first task"); err != nil {
		t.Fatalf("Prompt(delivery-1): %v", err)
	}
	first.expect(t, stream.TurnStart{Claim: integrationClaim}, stream.TurnEnd{Claim: integrationClaim})

	// The same delivery again: the shim acknowledges it itself and replays the turn it already
	// saw, naming the delivery; OMP is not prompted a second time.
	if err := conn.Prompt(ctx, "delivery-1", "the first task"); err != nil {
		t.Fatalf("Prompt(delivery-1) repeated: %v", err)
	}
	first.expect(t, stream.TurnStart{Claim: integrationClaim, DeliveryID: "delivery-1"}, stream.TurnEnd{Claim: integrationClaim})
	if got, want := childFrames(t, stdinRecord), []string{"negotiate_protocol", "prompt delivery-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the fake OMP read %q, want %q: a repeated delivery reached it", got, want)
	}

	// The daemon goes away. The connection's Closed is the last thing the listener reports.
	if rest := first.close(t); !reflect.DeepEqual(rest, []stream.Event{stream.Closed{Claim: integrationClaim}}) {
		t.Fatalf("after the listener closed, its last events were %#v, want only Closed", rest)
	}
	if lines := first.warnings(); len(lines) != 0 {
		t.Fatalf("the first listener logged %q", lines)
	}
	eventually(t, "the shim to notice the stream closed", func() bool {
		return strings.Contains(shimOut.String(), "daemon stream "+strings.SplitN(addr, "://", 2)[1]+" closed")
	})

	// OMP reports turns while no daemon is listening: the shim holds them.
	pid, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPid, err := strconv.Atoi(string(pid))
	if err != nil {
		t.Fatalf("the fake OMP's pid %q: %v", pid, err)
	}
	startsBefore := count(shimOut.lines(), shimwire.TypeAgentStart)
	endsBefore := count(shimOut.lines(), shimwire.TypeAgentEnd)
	if err := syscall.Kill(childPid, syscall.SIGUSR1); err != nil {
		t.Fatalf("signal the fake OMP: %v", err)
	}
	eventually(t, "the shim to read every turn OMP reported during the gap", func() bool {
		lines := shimOut.lines()
		return count(lines, shimwire.TypeAgentStart) == startsBefore+gapTurns &&
			count(lines, shimwire.TypeAgentEnd) == endsBefore+gapTurns
	})
	_, hellosBefore := g.state()

	// The daemon is back on the same address: the shim redials, says hello again, and the turns
	// from the gap arrive in the order OMP reported them, ahead of anything newer.
	second := listen(t, addr, g)
	second.expect(t, stream.Hello{Claim: integrationClaim, Generation: integrationGeneration})
	if _, hellos := g.state(); hellos <= hellosBefore {
		t.Fatalf("the restarted listener resolved no new hello (%d before, %d after)", hellosBefore, hellos)
	}
	for n := range gapTurns {
		second.expect(t,
			stream.TurnStart{Claim: integrationClaim, DeliveryID: fmt.Sprintf("gap-%d", n)},
			stream.TurnEnd{Claim: integrationClaim})
	}

	// The new connection carries a new delivery to OMP.
	await, cancelAwait = context.WithTimeout(ctx, integrationWait)
	defer cancelAwait()
	conn, err = second.listener.Await(await, integrationClaim)
	if err != nil {
		t.Fatalf("Await after the restart: %v", err)
	}
	if err := conn.Prompt(ctx, "delivery-2", "the second task"); err != nil {
		t.Fatalf("Prompt(delivery-2): %v", err)
	}
	second.expect(t, stream.TurnStart{Claim: integrationClaim}, stream.TurnEnd{Claim: integrationClaim})
	want := []string{"negotiate_protocol", "prompt delivery-1", "negotiate_protocol", "prompt delivery-2"}
	if got := childFrames(t, stdinRecord); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fake OMP read %q, want %q", got, want)
	}

	// Shutdown ends OMP through the shim, which exits with OMP's status: SIGTERM, 128+15.
	if err := conn.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	second.expect(t, stream.Closed{Claim: integrationClaim})
	select {
	case <-exited:
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) || exit.ExitCode() != 128+int(syscall.SIGTERM) {
			t.Fatalf("the shim exited with %v, want exit status 143 (its OMP ended by SIGTERM)", waitErr)
		}
	case <-time.After(integrationWait):
		t.Fatal("the shim did not exit after the daemon's shutdown")
	}
	if got := childFrames(t, stdinRecord); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the shutdown the fake OMP had read %q, want %q: the shutdown frame reached it", got, want)
	}
	if rest := second.close(t); len(rest) != 0 {
		t.Fatalf("after the shim exited the listener reported %#v", rest)
	}
	if lines := second.warnings(); len(lines) != 0 {
		t.Fatalf("the restarted listener logged %q", lines)
	}
}
