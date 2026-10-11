// Package shim is `legion worker-shim`: the bridge between one phase worker's `omp --mode rpc`
// and the daemon's worker stream. The shim always dials the daemon — the stream is the daemon's
// listener, never the shim's — authenticates with the pane's boot token, and spawns OMP only
// once the daemon has acked it. From then on it carries OMP's stdio NDJSON to the stream and
// back, answers the frames that are its own (`shutdown`, `adopt-working-copy`, a repeated
// delivery), and outlives any one connection: a dropped stream is redialled, and what OMP says
// meanwhile waits in a bounded backlog.
//
// The behaviour was ported from the TypeScript shim's `--connect` mode, which LEGION-223 removed; a
// `worker-shim.ts:N` citation at a rule names its source in this repository's history. The wire
// itself is internal/shimwire's.
package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const (
	// backlogLimit is how many of OMP's newest frames wait for a daemon that is not connected
	// (BACKLOG_LIMIT, worker-shim.ts).
	backlogLimit = 1000
	// A refused or failed connect is retried after reconnectBase, doubling to reconnectCap
	// (RECONNECT_BASE_MS and RECONNECT_CAP_MS, and cmdWorkerShimConnect's retry, worker-shim.ts).
	reconnectBase = 200 * time.Millisecond
	reconnectCap  = 5 * time.Second
	// DefaultGrace is how long a SIGTERMed child has to exit before it is killed: the shipped
	// shim's default, half the daemon's default `worker_stop_timeout_seconds`
	// (DEFAULT_TERMINATE_STDIN_GRACE_MS, worker-shim.ts).
	DefaultGrace = 5 * time.Second
	// DefaultStopGrace is the stop grace a shim assumes when its runtime names none
	// (Config.StopGrace): the daemon's default `worker_stop_timeout_seconds`, which is also what a
	// pod's launcher gets as its own stop grace (internal/runtime/sandbox's launcherCommand passes
	// both the same value, the pod's terminationGracePeriodSeconds).
	DefaultStopGrace = 10 * time.Second
	// drainTimeout bounds reading OMP's stdout after OMP has exited. What OMP wrote is in the pipe
	// by then; only a process it started and left holding the pipe keeps it open past this.
	drainTimeout = time.Second
	// adoptionWaitDelay bounds the wait for jj's stderr once jj has been killed at its budget.
	adoptionWaitDelay = time.Second
)

// warmUpDrain is how long Run waits for an in-flight CodeGraph warm-up once the shim's loop has
// ended, before it returns without it: what is left of the role's stop grace once the agent's own
// grace and the stdout drain are spent, so the whole stop — the agent SIGTERMed and, ignoring it,
// killed at Grace; its stdout drained for drainTimeout; the warm-up's codegraph child SIGTERMed
// and, ignoring it, killed at workspace's codegraphStopGrace; the lease released — fits inside the
// grace after which the launcher SIGKILLs the shim (endGeneration, internal/launcher/process.go),
// which would leave the lease behind. With the defaults, 10 s − 5 s − 1 s = 4 s, against a measured
// 0.1–0.3 s and a codegraph kill at 2 s. A runtime whose stop grace leaves nothing waits not at all.
func warmUpDrain(stopGrace, grace time.Duration) time.Duration {
	return max(stopGrace-grace-drainTimeout, 0)
}

// Clock is the time the shim waits on: the backoff between dials and the grace before a kill.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config is one shim's resolved flags. Every refusal belongs to the readers in config.go and has
// happened before Run: Run dials, and spawns once acked.
type Config struct {
	// Network and Address are the daemon's worker stream, as ParseAddress returns them.
	Network, Address string
	// BootToken is the pane's boot token, the hello's only field.
	BootToken string
	// Argv is the wrapped OMP invocation.
	Argv []string
	// Env is the shim's own environment (os.Environ()). OMP and the adoption command inherit it,
	// and the adoption rewrites the workspace its LEGION_WORKSPACE names.
	Env []string
	// ProviderEnv is NAME=value pairs for OMP's environment only (--provider-env-dir).
	ProviderEnv []string
	// AgentSecrets is the pod's enrollment with the secrets broker (agentsecrets.go); nil on a
	// tmux pane, which is never enrolled.
	AgentSecrets *AgentSecrets
	// WarmCodegraph, when set, is started once with the workspace the shim's LEGION_WORKSPACE
	// names, on the first line the wrapped process writes: Oh My Pi's `ready` frame comes once its
	// extensions are loaded and its RPC loop serves, so the index build the pod needs starts
	// behind the launch, never on it (`legion worker-shim --warm-codegraph`,
	// workspace.WarmCodegraphIndex). It runs in its own goroutine under a context the shim ends
	// when its loop ends — the wrapped process has exited — and it must return once that context
	// ends, its build stopped and its lease released, since Run waits for it (bounded by
	// warmUpDrain) before returning the agent's exit status: a stop mid-build then leaves no lease
	// behind for the role's relaunch to read as a live build. Nil on a tmux pane, whose workspace
	// the daemon warms itself, and on the controller, which has no workspace: nothing runs.
	WarmCodegraph func(ctx context.Context, dir string)
	// Log receives the shim's own lines and its one-line frame summaries: what the pane shows.
	Log io.Writer
	// Grace is how long a SIGTERMed child has before it is killed; zero is DefaultGrace.
	Grace time.Duration
	// StopGrace is how long the shim's runtime gives the whole shim to exit after its stop signal
	// before it is killed — a pod's terminationGracePeriodSeconds, which the launcher passes as
	// `--stop-grace`; zero is DefaultStopGrace. Run keeps its wait for the warm-up (warmUpDrain)
	// inside it.
	StopGrace time.Duration
	// Clock is the time the shim waits on; nil is the real one.
	Clock Clock
}

// Run bridges until the wrapped process exits and returns its exit status: its code, or 128 plus
// the signal that ended it. ctx is the shim's own termination — the signal that stops a pane or
// a pod — which ends the child the way the daemon's `shutdown` does; with no child spawned yet,
// the shim exits 143 as that signal would have. The error is the shim's own failure to start
// the child, never the child's.
func Run(ctx context.Context, cfg Config) (int, error) {
	s := &shim{cfg: cfg, clock: cfg.Clock, grace: cfg.Grace, stopGrace: cfg.StopGrace, childExited: make(chan struct{})}
	if s.clock == nil {
		s.clock = realClock{}
	}
	if s.grace == 0 {
		s.grace = DefaultGrace
	}
	if s.stopGrace == 0 {
		s.stopGrace = DefaultStopGrace
	}
	out := cfg.Log
	if out == nil {
		out = io.Discard
	}
	s.log = log.New(out, "", 0)
	s.out.log = s.log
	s.loop, s.stop = context.WithCancel(context.Background())
	s.warm, s.stopWarm = context.WithCancel(s.loop)
	// Deferred in this order so that they run in reverse: the loop ends, the renewer with it, and
	// the warm-up — ended with the loop — is waited for last, inside the stop grace.
	defer s.drainWarmUp()
	defer s.renewers.Wait()
	defer s.stop()
	go s.watch(ctx)
	return s.run()
}

type shim struct {
	cfg       Config
	clock     Clock
	grace     time.Duration
	stopGrace time.Duration
	log       *log.Logger
	dedupe    shimwire.Dedupe
	out       outbox

	// order makes a dedupe decision and the daemon-bound frames it implies one step, as they are
	// in the single-threaded shim this is ported from: a start replayed for a retried delivery
	// never overtakes OMP's own start on its way to the daemon.
	order sync.Mutex

	// loop ends with the shim: every dial, connection, and backoff is bound to it.
	loop context.Context
	stop context.CancelFunc
	// warm is the warm-up's context, a child of loop that also ends the moment the child is told
	// to stop (terminate): the build has nothing to wait for once the agent is on its way out, and
	// ending it then, not at the agent's exit, keeps the whole stop inside the stop grace even when
	// the agent spends its own grace ignoring SIGTERM.
	warm     context.Context
	stopWarm context.CancelFunc

	mu          sync.Mutex
	child       *exec.Cmd
	stdin       *shimwire.Writer
	exiting     bool // the shim is ending before any child was spawned; none will be
	terminating bool
	childExited chan struct{}
	thumbprint  string // agent-secrets keygen's thumbprint, generated once per shim
	renewing    bool   // the agent-secrets renewer goroutine has been started

	// renewers is the agent-secrets renewer goroutine (agentsecrets.go's startRenewer), reaped by
	// Run's deferred Wait before it returns.
	renewers sync.WaitGroup

	// warmed guards cfg.WarmCodegraph: called on the child's first line and never again, not for
	// a later frame and not when a redialled connection finds the child already running. warmUps
	// is that one goroutine, which Run waits for (drainWarmUp) before returning.
	warmed  sync.Once
	warmUps sync.WaitGroup

	once sync.Once
	code int
	err  error
}

// run dials until the shim ends. A connection that was acked and then dropped is redialled at
// once; a dial that fails, or a stream the daemon closes before its ack — a refused hello — is
// retried with backoff (cmdWorkerShimConnect's dial loop, worker-shim.ts).
func (s *shim) run() (int, error) {
	failures := 0
	for s.loop.Err() == nil {
		acked, err := s.connect()
		if s.loop.Err() != nil {
			break
		}
		if acked {
			failures = 0
			s.log.Printf("[worker-shim] daemon stream %s closed (%v); reconnecting", s.cfg.Address, err)
			continue
		}
		delay := min(reconnectBase<<min(failures, 5), reconnectCap)
		failures++
		s.log.Printf("[worker-shim] daemon stream %s unavailable (%v); retrying in %dms", s.cfg.Address, err, delay.Milliseconds())
		select {
		case <-s.clock.After(delay):
		case <-s.loop.Done():
		}
	}
	return s.code, s.err
}

// connect is one connection: the hello, the wait for its ack, the child spawned on the first
// ack, and the daemon's frames until the stream drops. It reports whether the daemon acked.
func (s *shim) connect() (acked bool, err error) {
	identity, err := s.identity()
	if err != nil {
		// A pod that cannot generate its key or read its token registers as nothing: the shim ends
		// naming the failure, before any dial (finish cancels the loop, so run returns 1 and the
		// error), and the daemon observes the pod gone.
		s.finish(1, err)
		return false, err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(s.loop, s.cfg.Network, s.cfg.Address)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	defer context.AfterFunc(s.loop, func() { _ = conn.Close() })()

	w := shimwire.NewWriter(conn)
	if err := w.WriteFrame(shimwire.Hello2{BootToken: s.cfg.BootToken, AgentSecrets: identity}); err != nil {
		return false, fmt.Errorf("send hello: %w", err)
	}
	r := shimwire.NewReader(conn)
	for {
		line, err := r.ReadLine()
		if err != nil {
			return false, fmt.Errorf("stream closed before hello_ack: %w", err)
		}
		if frame, err := shimwire.Decode(line); err == nil && frame.FrameType() == shimwire.TypeHelloAck {
			break
		}
		// Nothing the daemon sends before its ack is meant for a child that does not exist yet
		// (cmdWorkerShimConnect's onLine before hello_ack, worker-shim.ts).
		s.log.Printf("[worker-shim] ignoring a frame received before hello_ack: %.80s", line)
	}
	// Spawned here, before the next line is read: the daemon's first RPC frame may share a read
	// with the ack, and it must find a child to go to (cmdWorkerShimConnect's ack, worker-shim.ts).
	if !s.spawnOnce() {
		// The shim is ending — told to stop before any child, or unable to start one — and
		// finish settles its status; nothing is left to bridge.
		<-s.loop.Done()
		return true, nil
	}
	s.out.attach(conn, w)
	defer s.out.detach(conn)
	for {
		line, err := r.ReadLine()
		if err != nil {
			return true, err
		}
		s.fromDaemon(line)
	}
}

// fromDaemon routes one daemon frame: the two the shim answers itself, a delivery the dedupe
// answers, and everything else to OMP unchanged (createShimBridge's onLine, worker-shim.ts).
func (s *shim) fromDaemon(line []byte) {
	frame, err := shimwire.Decode(line)
	if err != nil {
		if typ := frameType(line); typ == shimwire.TypeAdoptWorkingCopy || typ == shimwire.TypeAgentSecretsEnrollment {
			// The daemon is its only sender, and it is never OMP's to read.
			s.log.Printf("[worker-shim] refusing a malformed %s frame: %v", typ, err)
			return
		}
		s.toChild(line)
		return
	}
	switch f := frame.(type) {
	case shimwire.Shutdown:
		s.terminate("the daemon's shutdown")
		return
	case shimwire.AdoptWorkingCopy:
		go s.adopt(f)
		return
	case shimwire.AgentSecretsEnrollment:
		s.enroll(f)
		return
	}
	s.order.Lock()
	forward, replies := s.dedupe.DaemonFrame(frame)
	for _, reply := range replies {
		s.toDaemon(reply)
	}
	s.order.Unlock()
	if forward {
		s.toChild(line)
	}
}

func frameType(line []byte) string {
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line, &head)
	return head.Type
}

func (s *shim) toChild(line []byte) {
	if err := s.stdin.WriteLine(line); err != nil {
		s.log.Printf("[worker-shim] could not forward a daemon frame to the wrapped process: %v", err)
	}
}

func (s *shim) toDaemon(frame shimwire.Frame) {
	line, err := json.Marshal(frame)
	if err != nil {
		s.log.Printf("[worker-shim] could not encode a %s frame: %v", frame.FrameType(), err)
		return
	}
	s.out.send(line)
}

// spawnOnce starts the wrapped process unless it already runs or the shim is ending. A process
// that cannot be started ends the shim with that failure.
func (s *shim) spawnOnce() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child != nil {
		return true
	}
	if s.exiting {
		return false
	}
	cmd := exec.Command(s.cfg.Argv[0], s.cfg.Argv[1:]...)
	cmd.Env = append(slices.Clone(s.cfg.Env), s.cfg.ProviderEnv...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.exiting = true
		s.finish(1, fmt.Errorf("spawn %s: %w", s.cfg.Argv[0], err))
		return false
	}
	// OMP's stdout is a pipe of the shim's own rather than exec's, so the process's exit and the
	// end of its output are two facts the shim waits on separately (see reap).
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		s.exiting = true
		s.finish(1, fmt.Errorf("spawn %s: %w", s.cfg.Argv[0], err))
		return false
	}
	cmd.Stdout = childStdout
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = childStdout.Close()
		s.exiting = true
		s.finish(1, fmt.Errorf("spawn %s: %w", s.cfg.Argv[0], err))
		return false
	}
	_ = childStdout.Close()
	s.child = cmd
	s.stdin = shimwire.NewWriter(stdin)
	s.log.Printf("[worker-shim] spawned %s (pid %d)", s.cfg.Argv[0], cmd.Process.Pid)
	pumped := make(chan struct{})
	go s.pump(stdout, pumped)
	go s.reap(cmd, stdout, pumped)
	return true
}

// pump carries OMP's stdout to the daemon, line by line and unchanged, after the dedupe has seen
// each frame; the answers the dedupe owes other requests follow the frame that settled them
// (createShimBridge's forwardToSocket, worker-shim.ts). The child's first line is also when the
// workspace's CodeGraph warm-up starts (Config.WarmCodegraph), in its own goroutine so the pump
// never waits on it.
func (s *shim) pump(stdout *os.File, done chan<- struct{}) {
	defer close(done)
	defer stdout.Close()
	r := shimwire.NewReader(stdout)
	for {
		line, err := r.ReadLine()
		if errors.Is(err, shimwire.ErrLineTooLong) {
			// OMP chunks every frame over the limit; a longer line is a child whose stream can no
			// longer be framed, and nothing from mid-line can be forwarded honestly.
			s.log.Printf("[worker-shim] the wrapped process broke its framing (%v); ending it", err)
			s.terminate("broken framing")
			_, _ = io.Copy(io.Discard, stdout)
			return
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				s.log.Printf("[worker-shim] reading the wrapped process's stdout: %v", err)
			}
			return
		}
		s.warmed.Do(s.warmCodegraph)
		line = bytes.Clone(line)
		frame, err := shimwire.Decode(line)
		s.order.Lock()
		var owed []shimwire.Frame
		if err == nil {
			owed = s.dedupe.AgentFrame(frame)
		}
		s.out.send(line)
		for _, f := range owed {
			s.toDaemon(f)
		}
		s.order.Unlock()
		if err == nil {
			if summary := summarize(frame); summary != "" {
				s.log.Print(summary)
			}
		}
	}
}

// warmCodegraph starts cfg.WarmCodegraph for the workspace LEGION_WORKSPACE names, once the
// wrapped process has written its first line (pump), under s.warm: the warm-up's build ends when
// the child is told to stop or has exited, and Run waits for its return (drainWarmUp). The command
// refuses --warm-codegraph without the variable (cmd/legion/worker_shim.go), so an empty value
// here is a Config built by hand.
func (s *shim) warmCodegraph() {
	if s.cfg.WarmCodegraph == nil {
		return
	}
	dir := envValue(s.cfg.Env, "LEGION_WORKSPACE")
	if dir == "" {
		s.log.Printf("[worker-shim] no CodeGraph warm-up: LEGION_WORKSPACE is unset")
		return
	}
	s.warmUps.Add(1)
	go func() {
		defer s.warmUps.Done()
		s.cfg.WarmCodegraph(s.warm, dir)
	}()
}

// drainWarmUp waits for the warm-up warmCodegraph started, if any, to return — its codegraph
// child ended by s.warm's end, its lease released — before Run returns the agent's exit status,
// so a role stopped mid-build leaves no lease its relaunch would read as a live build. The wait is
// bounded by warmUpDrain, what the role's stop grace leaves: a warm-up still running past it is
// logged and left, so the shim's exit still reaches the daemon before the launcher's kill.
func (s *shim) drainWarmUp() {
	drained := make(chan struct{})
	go func() {
		s.warmUps.Wait()
		close(drained)
	}()
	bound := warmUpDrain(s.stopGrace, s.grace)
	select {
	case <-drained:
	case <-time.After(bound):
		s.log.Printf("[worker-shim] the CodeGraph warm-up is still running %v after the wrapped process exited; leaving it", bound)
	}
}

// reap waits for the wrapped process, lets the pump forward what it wrote before exiting, and
// ends the shim with its status.
func (s *shim) reap(cmd *exec.Cmd, stdout *os.File, pumped <-chan struct{}) {
	_ = cmd.Wait()
	close(s.childExited)
	select {
	case <-pumped:
	case <-time.After(drainTimeout):
		s.log.Printf("[worker-shim] the wrapped process exited and something it started still holds its stdout; closing it")
		_ = stdout.Close()
		<-pumped
	}
	s.finish(exitStatus(cmd.ProcessState), nil)
}

func exitStatus(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}

// watch is the shim's own termination: ctx ending ends the child as `shutdown` does, and with no
// child yet ends the shim (cmdWorkerShimConnect's onTerminate callback, worker-shim.ts).
func (s *shim) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		if !s.terminate("signalled to stop") {
			s.log.Print("[worker-shim] signalled to stop before the wrapped process was spawned; exiting")
			s.finish(128+int(syscall.SIGTERM), nil)
		}
	case <-s.loop.Done():
	}
}

// terminate SIGTERMs the child and kills it if it is still running when the grace runs out. The
// bridge keeps running meanwhile, so what OMP says on its way out still reaches the daemon. The
// warm-up's context ends here too (s.warm): its build has nothing left to serve once the agent is
// on its way out, and ending it now rather than at the agent's exit keeps the stop inside the stop
// grace whatever the agent does with its own. It reports false, having done nothing but settle
// that no child will be spawned, when none was.
func (s *shim) terminate(reason string) bool {
	s.mu.Lock()
	child := s.child
	switch {
	case child == nil:
		s.exiting = true
		s.mu.Unlock()
		return false
	case s.terminating:
		s.mu.Unlock()
		return true
	}
	s.terminating = true
	s.mu.Unlock()

	s.log.Printf("[worker-shim] %s: sending SIGTERM to the wrapped process", reason)
	s.stopWarm()
	_ = child.Process.Signal(syscall.SIGTERM)
	expired := s.clock.After(s.grace)
	go func() {
		select {
		case <-s.childExited:
		case <-expired:
			s.log.Printf("[worker-shim] the wrapped process is still running %v after SIGTERM; killing it", s.grace)
			_ = child.Process.Kill()
		}
	}()
	return true
}

func (s *shim) finish(code int, err error) {
	s.once.Do(func() {
		s.code, s.err = code, err
		s.stop()
	})
}

// adopt answers `adopt-working-copy` (the adoption branch of createShimBridge's onLine,
// worker-shim.ts). A request the shim cannot act on is answered with the reason when it names an
// id to answer.
func (s *shim) adopt(request shimwire.AdoptWorkingCopy) {
	result := shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: true}
	if err := request.Validate(); err != nil {
		if request.ID == "" {
			s.log.Printf("[worker-shim] refusing an adopt-working-copy frame: %v", err)
			return
		}
		result.OK, result.Error = false, err.Error()
	} else if err := s.runAdoption(request); err != nil {
		result.OK, result.Error = false, err.Error()
	}
	s.toDaemon(result)
}

// adoptionArgs is the one adoption command both executors run (adoptWorkingCopyCommand,
// packages/workspace/src/workspace.ts): the working copy's author becomes the identity in
// the command's environment, and only while it is undescribed — a described working copy is a
// previous phase's work and keeps its author.
func adoptionArgs(jj, dir string) []string {
	return []string{jj, "metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", dir}
}

// describedArgs prints the working copy's commit id when it is described, and nothing otherwise.
func describedArgs(jj, dir string) []string {
	return []string{jj, "log", "-r", `@ ~ description(exact:"")`, "--no-graph", "-T", "commit_id", "-R", dir}
}

// runAdoption runs the adoption with PATH's jj — the one the pane's own shell runs, since no
// runtime pins a jj on a pane or pod — on the workspace the shim's environment names in
// LEGION_WORKSPACE, which the runtime sets on every pane, under the requested identity and
// budget. Every role of an issue shares the workspace, so a working copy the previous role left
// described (its pushed commit) is kept as it is, and the incoming role starts on a fresh one of
// its own (`jj new`): otherwise its work would land in the previous role's commit, authored by the
// previous role's App. One read decides which of the two changes the working copy needs, and only
// that one runs. A failure is reported the way the shipped runner does (commandFailure,
// packages/workspace/src/workspace.ts): the daemon decides what a failed adoption means. A shim
// without LEGION_WORKSPACE was not started by a runtime, and refuses (runWorkingCopyAdoption,
// worker-shim.ts); a PATH without jj refuses naming jj and the PATH searched.
func (s *shim) runAdoption(request shimwire.AdoptWorkingCopy) error {
	workspace := envValue(s.cfg.Env, "LEGION_WORKSPACE")
	if workspace == "" {
		return errors.New("worker-shim: LEGION_WORKSPACE is not set; no workspace to adopt")
	}
	jj, err := exec.LookPath("jj")
	if err != nil {
		return fmt.Errorf("worker-shim: jj is not on PATH (%s); no jj to adopt the working copy with", os.Getenv("PATH"))
	}
	budget := time.Duration(request.TimeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	env := append(slices.Clone(s.cfg.Env), "JJ_USER="+request.JJUser, "JJ_EMAIL="+request.JJEmail)
	started := time.Now()
	described, err := runAdoptionCommand(ctx, budget, started, env, describedArgs(jj, workspace))
	if err != nil {
		return err
	}
	change := adoptionArgs(jj, workspace)
	if described != "" {
		change = []string{jj, "new", "-R", workspace}
	}
	_, err = runAdoptionCommand(ctx, budget, started, env, change)
	return err
}

// runAdoptionCommand runs one adoption command within the adoption's budget and returns its
// trimmed standard output.
func runAdoptionCommand(ctx context.Context, budget time.Duration, started time.Time, env, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = adoptionWaitDelay
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(stdout.String()), nil
	}
	command := strings.Join(args, " ")
	report := strings.TrimSpace(stderr.String())
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		message := fmt.Sprintf("Command timed out after %s s (ran %.1f s): %s",
			strconv.FormatFloat(budget.Seconds(), 'f', -1, 64), time.Since(started).Seconds(), command)
		if report != "" {
			message += "\n" + report
		}
		return "", errors.New(message)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", fmt.Errorf("Command failed (exit %d): %s\n%s", exit.ExitCode(), command, report)
	}
	return "", fmt.Errorf("run %s: %w", command, err)
}

// envValue is name's value in env, the last assignment winning as it does for a process started
// with env; empty when env assigns none.
func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
}

// summarize is the one line the pane shows for a frame worth a human's glance, or nothing
// (summarizeShimFrame, worker-shim.ts).
func summarize(frame shimwire.Frame) string {
	switch f := frame.(type) {
	case shimwire.AgentStart:
		return shimwire.TypeAgentStart
	case shimwire.AgentEnd:
		return shimwire.TypeAgentEnd
	case shimwire.Response:
		if !f.Success {
			return strings.TrimSpace("error " + f.Command + " " + f.Error)
		}
	case shimwire.Raw:
		switch f.Type {
		case "tool_execution_start":
			var event struct {
				ToolName string          `json:"toolName"`
				Args     json.RawMessage `json:"args"`
			}
			_ = json.Unmarshal(f.JSON, &event)
			tool := event.ToolName
			if tool == "" {
				tool = "unknown"
			}
			var args bytes.Buffer
			if json.Compact(&args, event.Args) != nil || args.String() == "null" {
				args.Reset()
				args.WriteString("{}")
			}
			return "tool_execution_start " + tool + " " + truncate(args.String(), 80)
		case "error":
			var event struct {
				Message *string `json:"message"`
			}
			if json.Unmarshal(f.JSON, &event) == nil && event.Message != nil {
				return "error " + *event.Message
			}
			return "error " + string(f.JSON)
		case "extension_error":
			// An extension that failed leaves the agent running without what it gives it; this
			// line is the one place a pod's log says so (Oh My Pi reports it on its RPC stream only).
			var event struct {
				ExtensionPath string `json:"extensionPath"`
				Event         string `json:"event"`
				Error         string `json:"error"`
			}
			_ = json.Unmarshal(f.JSON, &event)
			return "extension_error " + event.ExtensionPath + " " + event.Event + ": " + event.Error
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if runes := []rune(s); len(runes) > n {
		return string(runes[:n])
	}
	return s
}

// outbox is every frame bound for the daemon: written to the connection when one is attached,
// held in the backlog of the newest backlogLimit frames when none is, and replayed in order —
// ahead of anything newer — when the next connection is acked (createShimBridge's forward and
// onConnect, worker-shim.ts).
type outbox struct {
	mu      sync.Mutex
	conn    net.Conn
	w       *shimwire.Writer
	backlog [][]byte
	dropped bool
	log     *log.Logger
}

func (o *outbox) send(line []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.w != nil {
		if o.w.WriteLine(line) == nil {
			return
		}
		// The stream broke under this frame, so the daemon never got it whole: it waits with the
		// rest for the next connection. Closing the connection is what tells its reader.
		_ = o.conn.Close()
		o.conn, o.w = nil, nil
	}
	o.backlog = append(o.backlog, line)
	if len(o.backlog) > backlogLimit {
		o.backlog[0] = nil
		o.backlog = o.backlog[1:]
		if !o.dropped {
			o.dropped = true
			o.log.Printf("[worker-shim] no daemon client connected; dropping oldest frames beyond the last %d", backlogLimit)
		}
	}
}

// attach replays the backlog on a newly acked connection and makes it the one frames go to. A
// connection that breaks mid-replay is closed, and what it did not take waits for the next.
func (o *outbox) attach(conn net.Conn, w *shimwire.Writer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(o.backlog) > 0 {
		if w.WriteLine(o.backlog[0]) != nil {
			_ = conn.Close()
			return
		}
		o.backlog[0] = nil
		o.backlog = o.backlog[1:]
	}
	o.backlog = nil
	o.conn, o.w = conn, w
}

func (o *outbox) detach(conn net.Conn) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conn == conn {
		o.conn, o.w = nil, nil
	}
}
