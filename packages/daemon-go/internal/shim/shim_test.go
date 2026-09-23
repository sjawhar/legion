package shim_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The fake OMP child is this test binary, re-executed with fakeOMPEnv set: a Go program that
// speaks enough of OMP's RPC frames for the bridge to have something real on the other side of
// its pipes.
const fakeOMPEnv = "LEGION_SHIM_FAKE_OMP"

const (
	bootToken = "boot-token-for-the-pane"
	grace     = 3 * time.Second
	waitLimit = 10 * time.Second
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeOMPEnv) == "1" {
		os.Exit(fakeOMP())
	}
	os.Exit(m.Run())
}

// fakeOMP answers a prompt with OMP's own sequence — the ack, then the turn — records every line
// it reads, and reports on its stdout what the tests need to see from inside the child: that it
// started, that a SIGTERM reached it, and a burst of frames on SIGUSR1.
func fakeOMP() int {
	var mu sync.Mutex
	emit := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = os.Stdout.WriteString(line + "\n")
	}
	if err := os.WriteFile(os.Getenv("FAKE_OMP_MARKER"), nil, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake omp: write the marker: %v\n", err)
		return 3
	}
	record, err := os.OpenFile(os.Getenv("FAKE_OMP_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake omp: open the log: %v\n", err)
		return 3
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGUSR1)
	if os.Getenv("FAKE_OMP_IGNORE_SIGTERM") == "1" {
		signal.Notify(signals, syscall.SIGTERM)
	}
	go func() {
		for sig := range signals {
			switch sig {
			case syscall.SIGTERM:
				emit(`{"type":"fake_sigterm"}`)
			case syscall.SIGUSR1:
				for n := range 1200 {
					emit(fmt.Sprintf(`{"type":"fake_frame","n":%d}`, n))
				}
				emit(`{"type":"agent_end"}`)
			}
		}
	}()

	emit(fmt.Sprintf(`{"type":"fake_ready","pid":%d}`, os.Getpid()))
	if os.Getenv("FAKE_OMP_HUGE_LINE") == "1" {
		emit(`{"type":"fake_huge","pad":"` + strings.Repeat("x", 2<<20) + `"}`)
	}

	lines := bufio.NewScanner(os.Stdin)
	lines.Buffer(make([]byte, 64<<10), 2<<20)
	for lines.Scan() {
		line := lines.Bytes()
		_, _ = record.Write(append(bytes.Clone(line), '\n'))
		var frame struct{ Type, ID, Message string }
		_ = json.Unmarshal(line, &frame)
		switch frame.Type {
		case "prompt":
			emit(fmt.Sprintf(`{"type":"response","id":%q,"command":"prompt","success":true}`, frame.ID))
			emit(`{"type":"agent_start"}`)
			if frame.Message != "hold" {
				emit(`{"type":"agent_end"}`)
			}
		case "fake_end_turn":
			emit(`{"type":"agent_end"}`)
		case "get_state":
			emit(fmt.Sprintf(`{"type":"response","id":%q,"command":"get_state","success":true,"data":{"isStreaming":false}}`, frame.ID))
		}
	}
	return 0
}

// omp is one fake OMP child's configuration and what it leaves behind.
type omp struct {
	marker, log string
	env         []string
}

func newOMP(t *testing.T, env ...string) *omp {
	t.Helper()
	dir := t.TempDir()
	o := &omp{marker: filepath.Join(dir, "started"), log: filepath.Join(dir, "stdin.ndjson")}
	o.env = append(os.Environ(), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+o.marker, "FAKE_OMP_LOG="+o.log)
	o.env = append(o.env, env...)
	return o
}

func (o *omp) started() bool {
	_, err := os.Stat(o.marker)
	return err == nil
}

// received is every line the child read on its stdin, decoded.
func (o *omp) received(t *testing.T) []shimwire.Frame {
	t.Helper()
	body, err := os.ReadFile(o.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the fake OMP's stdin record: %v", err)
	}
	var frames []shimwire.Frame
	for line := range bytes.Lines(body) {
		frame, err := shimwire.Decode(line)
		if err != nil {
			t.Fatalf("decode a line the fake OMP read: %v", err)
		}
		frames = append(frames, frame)
	}
	return frames
}

// fakeClock hands every wait the shim asks for to the test, which fires it — the backoff between
// dials and the grace before a kill are then steps of the test rather than time it spends.
type fakeClock struct {
	waits    chan clockWait
	mu       sync.Mutex
	released bool
}

type clockWait struct {
	d    time.Duration
	fire chan time.Time
}

func newClock() *fakeClock { return &fakeClock{waits: make(chan clockWait, 64)} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	w := clockWait{d: d, fire: make(chan time.Time, 1)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		w.fire <- time.Time{}
		return w.fire
	}
	c.waits <- w
	return w.fire
}

// next is the shim's next wait, which must be of d.
func (c *fakeClock) next(t *testing.T, d time.Duration) clockWait {
	t.Helper()
	select {
	case w := <-c.waits:
		if w.d != d {
			t.Fatalf("the shim waited %v, want %v", w.d, d)
		}
		return w
	case <-time.After(waitLimit):
		t.Fatalf("the shim never waited %v", d)
		return clockWait{}
	}
}

func (w clockWait) elapse() { w.fire <- time.Time{} }

// release fires every wait, pending and future, so a test's cleanup is never stuck behind one.
func (c *fakeClock) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = true
	for {
		select {
		case w := <-c.waits:
			w.fire <- time.Time{}
		default:
			return
		}
	}
}

func (c *fakeClock) idle(t *testing.T) {
	t.Helper()
	select {
	case w := <-c.waits:
		t.Fatalf("the shim waited %v; it should not have waited at all", w.d)
	default:
	}
}

// syncBuffer is the shim's log: what the pane shows.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// count is how many log lines contain substr.
func (b *syncBuffer) count(substr string) int {
	n := 0
	for line := range strings.Lines(b.String()) {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func (b *syncBuffer) await(t *testing.T, substr string, n int) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for b.count(substr) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the shim never logged %q %d time(s); log:\n%s", substr, n, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitLine waits for n log lines that are exactly line: the pane's one-line frame summaries.
func (b *syncBuffer) awaitLine(t *testing.T, line string, n int) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		got := 0
		for l := range strings.Lines(b.String()) {
			if strings.TrimSpace(l) == line {
				got++
			}
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shim never logged the line %q %d time(s); log:\n%s", line, n, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// running is one shim.Run in a goroutine.
type running struct {
	cancel context.CancelFunc
	done   chan struct{}
	code   int
	err    error
	log    *syncBuffer
}

func run(t *testing.T, cfg shim.Config, clock *fakeClock) *running {
	t.Helper()
	r := &running{done: make(chan struct{}), log: &syncBuffer{}}
	cfg.Log = r.log
	cfg.Clock = clock
	var ctx context.Context
	ctx, r.cancel = context.WithCancel(context.Background())
	go func() {
		r.code, r.err = shim.Run(ctx, cfg)
		close(r.done)
	}()
	t.Cleanup(func() {
		clock.release()
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(waitLimit):
			t.Errorf("the shim never exited; log:\n%s", r.log)
		}
	})
	return r
}

func (r *running) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(waitLimit):
		t.Fatalf("the shim never exited; log:\n%s", r.log)
	}
	if r.err != nil {
		t.Fatalf("shim.Run: %v; log:\n%s", r.err, r.log)
	}
	return r.code
}

func (r *running) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// stub is the daemon's worker stream listener reduced to what a test drives by hand: it accepts,
// and every frame on the connection is the test's to read or write.
type stub struct {
	ln   net.Listener
	path string
}

func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "s")
}

func listen(t *testing.T, path string) *stub {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return &stub{ln: ln, path: path}
}

func (s *stub) accept(t *testing.T) *peer {
	t.Helper()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := s.ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	select {
	case conn := <-accepted:
		t.Cleanup(func() { _ = conn.Close() })
		return &peer{conn: conn, r: shimwire.NewReader(conn), w: shimwire.NewWriter(conn)}
	case <-time.After(waitLimit):
		t.Fatal("the shim never dialled the listener")
		return nil
	}
}

type peer struct {
	conn net.Conn
	r    *shimwire.Reader
	w    *shimwire.Writer
}

func (p *peer) next(t *testing.T) shimwire.Frame {
	t.Helper()
	if err := p.conn.SetReadDeadline(time.Now().Add(waitLimit)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	line, err := p.r.ReadLine()
	if err != nil {
		t.Fatalf("read a frame from the shim: %v", err)
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		t.Fatalf("decode a frame from the shim: %v", err)
	}
	return frame
}

func (p *peer) expect(t *testing.T, want shimwire.Frame) {
	t.Helper()
	if got := p.next(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("the shim sent %#v, want %#v", got, want)
	}
}

// expectRaw reads the next frame, which must be an unmodelled one of type typ.
func (p *peer) expectRaw(t *testing.T, typ string) shimwire.Raw {
	t.Helper()
	frame := p.next(t)
	raw, ok := frame.(shimwire.Raw)
	if !ok || raw.Type != typ {
		t.Fatalf("the shim sent %#v, want a %s frame", frame, typ)
	}
	return raw
}

func (p *peer) send(t *testing.T, frame shimwire.Frame) {
	t.Helper()
	if err := p.w.WriteFrame(frame); err != nil {
		t.Fatalf("write a %s frame to the shim: %v", frame.FrameType(), err)
	}
}

func (p *peer) sendLine(t *testing.T, line string) {
	t.Helper()
	if err := p.w.WriteLine([]byte(line)); err != nil {
		t.Fatalf("write %s to the shim: %v", line, err)
	}
}

// expectHello reads the connection's first frame, which must be the hello carrying the pane's
// boot token.
func (p *peer) expectHello(t *testing.T) {
	t.Helper()
	p.expect(t, shimwire.Hello{BootToken: bootToken})
}

// open is the daemon's side of a successful connect: the hello read and acked, and the child's
// first frame — proof it was spawned and bridged — read back. It returns the child's pid.
func (p *peer) open(t *testing.T) int {
	t.Helper()
	p.expectHello(t)
	p.send(t, shimwire.HelloAck{})
	return readyPID(t, p.expectRaw(t, "fake_ready"))
}

func readyPID(t *testing.T, raw shimwire.Raw) int {
	t.Helper()
	var ready struct{ PID int }
	if err := json.Unmarshal(raw.JSON, &ready); err != nil || ready.PID == 0 {
		t.Fatalf("the fake OMP's ready frame %s carries no pid: %v", raw.JSON, err)
	}
	return ready.PID
}

func config(t *testing.T, path string, o *omp) shim.Config {
	return shim.Config{
		Network:   "unix",
		Address:   path,
		BootToken: bootToken,
		Argv:      []string{os.Args[0]},
		Env:       o.env,
		Grace:     grace,
	}
}

func frameTypes(frames []shimwire.Frame) []string {
	types := make([]string, len(frames))
	for i, frame := range frames {
		types[i] = frame.FrameType()
	}
	return types
}

// OMP is spawned only once the daemon has accepted the pane's hello: a frame that arrives before
// the ack is ignored — never buffered for a child that does not exist yet — and the child exists
// from the ack on.
func TestOMPIsSpawnedOnlyAfterTheHelloAck(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	sh := run(t, config(t, path, child), newClock())

	p := daemon.accept(t)
	p.expectHello(t)
	p.send(t, shimwire.Prompt{ID: "early", DeliveryID: "d0", Message: "not yet"})
	sh.log.await(t, "ignoring a frame received before hello_ack", 1)
	if child.started() || sh.log.count("[worker-shim] spawned ") != 0 {
		t.Fatalf("OMP was spawned before the daemon acked the hello; log:\n%s", sh.log)
	}

	p.send(t, shimwire.HelloAck{})
	p.expectRaw(t, "fake_ready")
	p.send(t, shimwire.GetState{ID: "probe"})
	if raw := p.next(t).(shimwire.Response); raw.ID != "probe" {
		t.Fatalf("the probe was answered as %#v", raw)
	}
	if got := frameTypes(child.received(t)); !reflect.DeepEqual(got, []string{shimwire.TypeGetState}) {
		t.Fatalf("OMP read %v; the pre-ack prompt must never reach it", got)
	}
}

// A daemon that refuses the hello — closing the stream, as it does for an unknown or stale boot
// token — never gets an OMP spawned: the shim redials, re-sending the hello each time, with a
// backoff that doubles from 200 ms to its 5 s cap. A dial that finds no listener counts the same.
func TestARefusedHelloIsRedialledWithBackoffAndNeverSpawnsOMP(t *testing.T) {
	path := socketPath(t)
	child := newOMP(t)
	clock := newClock()
	sh := run(t, config(t, path, child), clock)

	// No listener yet: the daemon is not up.
	clock.next(t, 200*time.Millisecond).elapse()
	last := clock.next(t, 400*time.Millisecond)
	daemon := listen(t, path)
	last.elapse()
	for _, want := range []time.Duration{800, 1600, 3200, 5000, 5000, 5000} {
		p := daemon.accept(t)
		p.expectHello(t)
		_ = p.conn.Close()
		clock.next(t, want*time.Millisecond).elapse()
	}
	if child.started() || sh.log.count("[worker-shim] spawned ") != 0 {
		t.Fatalf("OMP was spawned although the daemon never acked a hello; log:\n%s", sh.log)
	}

	// The shim's own SIGTERM with nothing spawned: it exits as the signal would have.
	daemon.accept(t).expectHello(t)
	sh.cancel()
	if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("the shim exited %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	if child.started() || sh.log.count("[worker-shim] spawned ") != 0 {
		t.Fatalf("OMP was spawned by a shim told to stop before any ack; log:\n%s", sh.log)
	}
}

// An acked connection clears the backoff: a stream that drops after its ack is redialled at once,
// and the next refusal waits 200 ms again, not where the earlier failures had left the backoff.
func TestAnAckedConnectionResetsTheBackoff(t *testing.T) {
	path := socketPath(t)
	child := newOMP(t)
	clock := newClock()
	run(t, config(t, path, child), clock)

	clock.next(t, 200*time.Millisecond).elapse()
	clock.next(t, 400*time.Millisecond).elapse()
	last := clock.next(t, 800*time.Millisecond)
	daemon := listen(t, path)
	last.elapse()
	p := daemon.accept(t)
	p.open(t)

	_ = p.conn.Close()
	p = daemon.accept(t)
	p.expectHello(t)
	clock.idle(t)
	_ = p.conn.Close()
	clock.next(t, 200*time.Millisecond)
}

// What OMP writes while the daemon is away waits in a backlog of the newest 1000 frames and is
// replayed in order, ahead of anything newer, once the next connection's hello is acked. The
// hello is re-sent on that connection; a stream that dropped after its ack is redialled at once,
// with no backoff; and overflowing the backlog is logged once for the shim's life.
func TestFramesFromADroppedConnectionReplayInOrderAfterTheNextAck(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	clock := newClock()
	sh := run(t, config(t, path, child), clock)
	p := daemon.accept(t)
	pid := p.open(t)

	for round := 1; round <= 2; round++ {
		_ = p.conn.Close()
		sh.log.await(t, "reconnecting", round)
		// 1200 frames and the end of a turn, all while no daemon is connected: the oldest 201
		// cannot be kept.
		if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
			t.Fatalf("signal the fake OMP: %v", err)
		}
		sh.log.awaitLine(t, "agent_end", round)

		p = daemon.accept(t)
		p.expectHello(t)
		p.send(t, shimwire.HelloAck{})
		for want := 201; want < 1200; want++ {
			var frame struct{ N int }
			if err := json.Unmarshal(p.expectRaw(t, "fake_frame").JSON, &frame); err != nil {
				t.Fatalf("decode a replayed frame: %v", err)
			}
			if frame.N != want {
				t.Fatalf("round %d replayed frame %d where frame %d was due", round, frame.N, want)
			}
		}
		p.expect(t, shimwire.AgentEnd{})
	}
	if got := sh.log.count("dropping oldest frames beyond the last 1000"); got != 1 {
		t.Fatalf("the backlog overflow was logged %d times, want once for the shim's life; log:\n%s", got, sh.log)
	}
	clock.idle(t)
}

// The daemon's `shutdown` is the shim's to act on, never OMP's to read: the child is sent
// SIGTERM, and killed if it is still running when the grace runs out. The shim's own SIGTERM —
// the pane or the pod being stopped — ends the child the same way. The bridge keeps working
// through the grace, so whatever OMP says on its way out still reaches the daemon.
func TestShutdownSendsSIGTERMThenKillsTheChildAfterTheGrace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(*testing.T, *running, *peer)
	}{
		{"the daemon's shutdown frame", func(t *testing.T, _ *running, p *peer) { p.send(t, shimwire.Shutdown{}) }},
		{"the shim's own SIGTERM", func(_ *testing.T, sh *running, _ *peer) { sh.cancel() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t, "FAKE_OMP_IGNORE_SIGTERM=1")
			clock := newClock()
			sh := run(t, config(t, path, child), clock)
			p := daemon.accept(t)
			p.open(t)

			tc.trigger(t, sh, p)
			p.expectRaw(t, "fake_sigterm")
			expired := clock.next(t, grace)
			p.send(t, shimwire.GetState{ID: "during-grace"})
			if got := p.next(t).(shimwire.Response); got.ID != "during-grace" {
				t.Fatalf("the bridge answered %#v during the grace", got)
			}
			if sh.exited() {
				t.Fatal("the shim exited before the grace ran out")
			}

			expired.elapse()
			if code := sh.wait(t); code != 128+int(syscall.SIGKILL) {
				t.Fatalf("the shim exited %d, want %d (the child killed)", code, 128+int(syscall.SIGKILL))
			}
			if got := frameTypes(child.received(t)); !reflect.DeepEqual(got, []string{shimwire.TypeGetState}) {
				t.Fatalf("OMP read %v; shutdown must never be forwarded", got)
			}
		})
	}
}

// A child that honours SIGTERM ends within the grace, and the shim exits with its status.
func TestShutdownOfAChildThatHonoursSIGTERMExitsWithItsStatus(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	clock := newClock()
	sh := run(t, config(t, path, child), clock)
	p := daemon.accept(t)
	p.open(t)

	p.send(t, shimwire.Shutdown{})
	clock.next(t, grace)
	if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("the shim exited %d, want %d", code, 128+int(syscall.SIGTERM))
	}
}

// fakeJJ writes a `jj` that records how it was run and then does what mode says, at the path the
// test hands the shim as LEGION_JJ_PATH, and puts a decoy `jj` first on PATH that fails naming
// itself: the shim runs the jj the daemon resolved at boot, never a PATH lookup.
func fakeJJ(t *testing.T) (path, record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "jj-invocation")
	path = filepath.Join(t.TempDir(), "jj")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_JJ_RECORD"
printf 'JJ_USER=%s\nJJ_EMAIL=%s\n' "$JJ_USER" "$JJ_EMAIL" >> "$FAKE_JJ_RECORD"
case "$FAKE_JJ_MODE" in
fail) echo "Error: the working copy is stale" >&2; exit 3 ;;
hang) exec sleep 30 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake jj: %v", err)
	}
	decoys := t.TempDir()
	decoy := "#!/bin/sh\necho 'the jj on PATH ran' >&2\nexit 97\n"
	if err := os.WriteFile(filepath.Join(decoys, "jj"), []byte(decoy), 0o700); err != nil {
		t.Fatalf("write the decoy jj: %v", err)
	}
	t.Setenv("PATH", decoys+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path, record
}

// environWithout is os.Environ() less the named variables: the shim under test must not inherit
// one from whatever environment the tests happen to run in.
func environWithout(names ...string) []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(names, name)
	})
}

// `adopt-working-copy` is answered by the shim itself: the one shared adoption command, run by
// the jj the pane's environment names in LEGION_JJ_PATH on the workspace it names in
// LEGION_WORKSPACE — never a PATH lookup, never the directory the shim happens to run in — under
// the identity the daemon named and within its budget, and OMP never sees the frame. A shim
// missing either variable refuses, naming it. A failure answers `ok: false` with the command's
// report rather than taking the agent down.
func TestAdoptWorkingCopyRunsTheSharedMetaeditAndAnswersTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string
		noWorkspace bool
		noJJPath    bool
		request     shimwire.AdoptWorkingCopy
		ok          bool
		errorHas    []string
		ranJJ       bool
	}{
		{
			name:    "a clean run",
			request: shimwire.AdoptWorkingCopy{ID: "a1", JJUser: "legion-implementer[bot]", JJEmail: "implementer@example.test", TimeoutMs: 5000},
			ok:      true,
			ranJJ:   true,
		},
		{
			name:     "jj fails",
			mode:     "fail",
			request:  shimwire.AdoptWorkingCopy{ID: "a2", JJUser: "u", JJEmail: "u@example.test", TimeoutMs: 5000},
			errorHas: []string{"Command failed (exit 3)", "jj metaedit", "the working copy is stale"},
			ranJJ:    true,
		},
		{
			name:     "jj outlives the budget",
			mode:     "hang",
			request:  shimwire.AdoptWorkingCopy{ID: "a3", JJUser: "u", JJEmail: "u@example.test", TimeoutMs: 200},
			errorHas: []string{"Command timed out after 0.2 s", "jj metaedit"},
			ranJJ:    true,
		},
		{
			name:     "a request without an identity",
			request:  shimwire.AdoptWorkingCopy{ID: "a4", TimeoutMs: 5000},
			errorHas: []string{"no jj identity"},
		},
		{
			name:        "a shim with no LEGION_WORKSPACE",
			noWorkspace: true,
			request:     shimwire.AdoptWorkingCopy{ID: "a5", JJUser: "u", JJEmail: "u@example.test", TimeoutMs: 5000},
			errorHas:    []string{"LEGION_WORKSPACE is not set"},
		},
		{
			name:     "a shim with no LEGION_JJ_PATH",
			noJJPath: true,
			request:  shimwire.AdoptWorkingCopy{ID: "a6", JJUser: "u", JJEmail: "u@example.test", TimeoutMs: 5000},
			errorHas: []string{"LEGION_JJ_PATH is not set"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jj, record := fakeJJ(t)
			path := socketPath(t)
			daemon := listen(t, path)
			child := newOMP(t)
			cfg := config(t, path, child)
			// The workspace is a directory of its own: not the test process's working
			// directory, which is the shim's.
			workspace := t.TempDir()
			cfg.Env = append(environWithout("LEGION_WORKSPACE", "LEGION_JJ_PATH"), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
				"FAKE_OMP_LOG="+child.log, "FAKE_JJ_RECORD="+record, "FAKE_JJ_MODE="+tc.mode)
			if !tc.noWorkspace {
				cfg.Env = append(cfg.Env, "LEGION_WORKSPACE="+workspace)
			}
			if !tc.noJJPath {
				cfg.Env = append(cfg.Env, "LEGION_JJ_PATH="+jj)
			}
			run(t, cfg, newClock())
			p := daemon.accept(t)
			p.open(t)

			p.send(t, tc.request)
			got, ok := p.next(t).(shimwire.AdoptWorkingCopyResult)
			if !ok || got.ID != tc.request.ID || got.OK != tc.ok {
				t.Fatalf("the shim answered %#v, want a result for %s with ok=%v", got, tc.request.ID, tc.ok)
			}
			for _, want := range tc.errorHas {
				if !strings.Contains(got.Error, want) {
					t.Fatalf("the result's error %q does not mention %q", got.Error, want)
				}
			}

			invocation, err := os.ReadFile(record)
			if !tc.ranJJ {
				if err == nil {
					t.Fatalf("jj ran for a request the shim should have refused: %s", invocation)
				}
			} else {
				if err != nil {
					t.Fatalf("jj never ran: %v", err)
				}
				want := strings.Join([]string{
					"metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", workspace,
					"JJ_USER=" + tc.request.JJUser, "JJ_EMAIL=" + tc.request.JJEmail,
				}, "\n") + "\n"
				if string(invocation) != want {
					t.Fatalf("jj ran as\n%s\nwant\n%s", invocation, want)
				}
			}

			p.send(t, shimwire.GetState{ID: "after"})
			p.next(t)
			if got := frameTypes(child.received(t)); !reflect.DeepEqual(got, []string{shimwire.TypeGetState}) {
				t.Fatalf("OMP read %v; adopt-working-copy is the shim's own", got)
			}
		})
	}
}

// The delivery dedupe end to end: the daemon retrying a prompt with the same deliveryId reaches
// OMP once, and every repeat is answered by the shim with the replay rules of internal/shimwire
// — the ack, a start carrying the deliveryId once the turn began, and its end once it is over.
func TestARepeatedDeliveryReachesOMPOnceAndIsAnsweredLocally(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	run(t, config(t, path, child), newClock())
	p := daemon.accept(t)
	p.open(t)
	ack := func(id string) shimwire.Response {
		return shimwire.Response{ID: id, Command: shimwire.TypePrompt, Success: true}
	}

	p.send(t, shimwire.Prompt{ID: "r1", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r1"))
	p.expect(t, shimwire.AgentStart{})

	// Mid-turn: the ack and the start, which the daemon can fence on by its deliveryId.
	p.send(t, shimwire.Prompt{ID: "r2", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r2"))
	p.expect(t, shimwire.AgentStart{DeliveryID: "d1"})

	p.sendLine(t, `{"type":"fake_end_turn"}`)
	p.expect(t, shimwire.AgentEnd{})

	// After the turn: the end is replayed too, or the daemon would wait for one already sent.
	p.send(t, shimwire.Prompt{ID: "r3", DeliveryID: "d1", Message: "hold"})
	p.expect(t, ack("r3"))
	p.expect(t, shimwire.AgentStart{DeliveryID: "d1"})
	p.expect(t, shimwire.AgentEnd{})

	// A new delivery is OMP's again.
	p.send(t, shimwire.Prompt{ID: "r4", DeliveryID: "d2", Message: "the next task"})
	p.expect(t, ack("r4"))
	p.expect(t, shimwire.AgentStart{})
	p.expect(t, shimwire.AgentEnd{})

	var prompts []string
	for _, frame := range child.received(t) {
		if prompt, ok := frame.(shimwire.Prompt); ok {
			prompts = append(prompts, prompt.ID)
		}
	}
	if !reflect.DeepEqual(prompts, []string{"r1", "r4"}) {
		t.Fatalf("OMP was prompted with %v, want [r1 r4]: d1 once, d2 once", prompts)
	}
}

// OMP chunks any frame over the plain-frame limit, so a longer line on its stdout is a broken
// child: nothing honest can be forwarded from mid-line. The shim says so and ends the child
// rather than bridging a stream it can no longer frame.
func TestAnOverlongLineFromOMPEndsTheChild(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t, "FAKE_OMP_HUGE_LINE=1")
	clock := newClock()
	sh := run(t, config(t, path, child), clock)
	daemon.accept(t).open(t)

	clock.next(t, grace)
	if code := sh.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("the shim exited %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	if sh.log.count("exceeds the plain-frame limit") != 1 {
		t.Fatalf("the shim did not say why it ended the child; log:\n%s", sh.log)
	}
}
