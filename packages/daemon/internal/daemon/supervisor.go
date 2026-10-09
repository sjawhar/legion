package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

var _ api.Supervisor = (*supervisor)(nil)

// supervisor is the daemon's claims: one machine per claim, each fed its events on a goroutine of
// its own. A machine holds its lock through the runtime call a decision makes — a stop waits out
// its grace — so one claim's stop must never hold another claim's hello.
type supervisor struct {
	// ctx is the machines' own work: their timers, their sends, and every event fed to them.
	ctx      context.Context
	deps     supervise.Deps
	store    *store.Store
	project  string
	stateDir string
	log      *slog.Logger
	// restored closes once every stored claim has its machine: a hello waits for it, so a shim
	// reconnecting across a restart is admitted by a claim already supervised.
	restored chan struct{}
	terminal func(supervise.Claim, supervise.ClaimState)

	mu       sync.RWMutex
	machines map[claim.Token]*member
	stopped  bool
	feeding  sync.WaitGroup
	// supervised counts the machines, which are never removed, and deciding is each claim whose
	// machine is handling an event from its inbox now, with the event's type. Both are read without
	// mu, which Create holds through a store write, so the stop's log lines never wait on Postgres.
	supervised atomic.Int64
	decidingMu sync.Mutex
	deciding   map[claim.Token]string
}

// member is one claim's machine, the queue its events wait in, and the tree the claim is of.
type member struct {
	machine *supervise.Machine
	inbox   *inbox
	tree    string
}

func newSupervisor(ctx context.Context, st *store.Store, project, stateDir string, log *slog.Logger) *supervisor {
	return &supervisor{
		ctx: ctx, store: st, project: project, stateDir: stateDir, log: log,
		restored: make(chan struct{}),
		machines: map[claim.Token]*member{}, deciding: map[claim.Token]string{},
	}
}

// Machine is the claim's machine, if the daemon supervises the claim.
func (s *supervisor) Machine(token claim.Token) (*supervise.Machine, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.machines[token]
	if !ok {
		return nil, false
	}
	return m.machine, true
}

// OnTerminal applies one durable-state callback to every current and future machine.
func (s *supervisor) OnTerminal(callback func(supervise.Claim, supervise.ClaimState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminal = callback
	for _, member := range s.machines {
		member.machine.OnTerminal(callback)
	}
}

// Create stores a new claim, queued, and supervises it. An explicit operator prompt is kept
// under the state directory as a test override; ordinary claims have no override and the specs
// composer supplies their role prompt parts. A claim the daemon already supervises is returned as
// it is.
func (s *supervisor) Create(ctx context.Context, c supervise.Claim, rolePrompt string) (*supervise.Machine, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.machines[c.Token]; ok {
		return existing.machine, false, nil
	}
	if s.stopped {
		return nil, false, errors.New("the daemon is stopping")
	}
	if rolePrompt != "" {
		path := rolePromptPath(s.stateDir, c.Token)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, false, fmt.Errorf("keep the role prompt of %s: %w", c.Token, err)
		}
		if err := os.WriteFile(path, []byte(rolePrompt), 0o600); err != nil {
			return nil, false, fmt.Errorf("keep the role prompt of %s: %w", c.Token, err)
		}
	}
	bound, err := s.deps.Store.AdmitClaim(ctx, c)
	if err != nil {
		return nil, false, err
	}
	c = bound
	m, err := supervise.NewMachine(s.ctx, s.deps, c)
	if err != nil {
		return nil, false, err
	}
	s.add(c.Token, m)
	return m, true, nil
}

// Claims is every claim of this daemon's project, as the store holds them.
func (s *supervisor) Claims(ctx context.Context) ([]supervise.Claim, error) {
	claims, err := s.store.Claims(ctx)
	if err != nil {
		return nil, err
	}
	return ofProject(claims, s.project), nil
}

// restore builds the machine of every claim the store held at boot, and reports the claims whose
// launch the last daemon persisted and never finished: launching with no locator, the process —
// if one opened at all — never recorded. Each is put back to queued, which is what it is: nothing
// the daemon knows of runs for it.
func (s *supervisor) restore(ctx context.Context, claims []supervise.Claim) ([]claim.Token, error) {
	var unfinished []claim.Token
	for _, c := range claims {
		if c.State == supervise.StateLaunching && c.Locator == nil {
			c.State = supervise.StateLaunchUncertain
			if err := s.deps.Store.PutClaim(ctx, c); err != nil {
				return nil, err
			}
		}
		if c.State == supervise.StateLaunchUncertain {
			unfinished = append(unfinished, c.Token)
		}
		m, err := supervise.NewMachine(s.ctx, s.deps, c)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.add(c.Token, m)
		s.mu.Unlock()
	}
	return unfinished, nil
}

// add supervises m, starting the goroutine that feeds it. The caller holds mu.
func (s *supervisor) add(token claim.Token, m *supervise.Machine) {
	m.OnTerminal(s.terminal)
	queue := newInbox()
	s.machines[token] = &member{machine: m, inbox: queue, tree: m.Claim().Tree}
	s.supervised.Add(1)
	s.feeding.Add(1)
	go func() {
		defer s.feeding.Done()
		for {
			ev, ok := queue.take()
			if !ok {
				return
			}
			s.decide(token, ev)
			err := m.Handle(s.ctx, ev)
			s.decided(token)
			switch {
			case err == nil:
			case s.ctx.Err() != nil:
				s.log.Info("supervise: the daemon's stop ended a decision; the next boot takes the claim up",
					"claim", token, "event", fmt.Sprintf("%T", ev), "error", err)
			default:
				s.log.Error("supervise: an event failed", "claim", token, "event", fmt.Sprintf("%T", ev), "error", err)
			}
		}
	}()
}

// decide and decided bracket a machine's handling of one of its inbox's events.
func (s *supervisor) decide(token claim.Token, ev supervise.Event) {
	s.decidingMu.Lock()
	defer s.decidingMu.Unlock()
	s.deciding[token] = fmt.Sprintf("%T", ev)
}

func (s *supervisor) decided(token claim.Token) {
	s.decidingMu.Lock()
	defer s.decidingMu.Unlock()
	delete(s.deciding, token)
}

// inDecision is every claim whose machine is handling an event from its inbox now, as
// "<claim> (<event type>)", in claim order.
func (s *supervisor) inDecision() []string {
	s.decidingMu.Lock()
	defer s.decidingMu.Unlock()
	busy := make([]string, 0, len(s.deciding))
	for token, ev := range s.deciding {
		busy = append(busy, fmt.Sprintf("%s (%s)", token, ev))
	}
	slices.Sort(busy)
	return busy
}

// post queues ev for the claim's machine. An event for a claim the daemon does not supervise, or
// one that arrives once supervision is stopping, goes nowhere.
func (s *supervisor) post(token claim.Token, ev supervise.Event) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stopped {
		return
	}
	m, ok := s.machines[token]
	if !ok {
		s.log.Warn("supervise: an event for a claim this daemon does not supervise", "claim", token,
			"event", fmt.Sprintf("%T", ev))
		return
	}
	m.inbox.put(ev)
}

// retree re-points m's claim to tree (supervise.Machine.Retree) and then the tree kept beside it,
// which volumeLost reads. The machine is asked first, under its own lock alone: a machine calls
// volumeLost under its lock, so taking mu around the machine's lock would invert that order.
func (s *supervisor) retree(ctx context.Context, token claim.Token, m *supervise.Machine, tree string) error {
	if err := m.Retree(ctx, tree); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if member, ok := s.machines[token]; ok {
		member.tree = tree
	}
	return nil
}

// volumeLost tells every other claim of c's tree that the tree volume was lost (TreeVolumeLost):
// the sessions they recorded were on it. It is the machines' VolumeLost, called by c's machine
// under its own lock, so it reads no machine — the tree of each is kept beside it — and only queues.
func (s *supervisor) volumeLost(c supervise.Claim) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stopped {
		return
	}
	for token, m := range s.machines {
		if token != c.Token && m.tree == c.Tree {
			m.inbox.put(supervise.TreeVolumeLost{Claim: token})
		}
	}
}

// count is how many claims the daemon supervises.
func (s *supervisor) count() int {
	return int(s.supervised.Load())
}

// halt feeds no machine another event, and waits for nothing.
func (s *supervisor) halt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	for _, m := range s.machines {
		m.inbox.close()
	}
}

// stop feeds no machine another event and waits for every event being handled to finish.
func (s *supervisor) stop() {
	s.halt()
	s.feeding.Wait()
}

// wait blocks until every send any machine started has had its outcome handled.
func (s *supervisor) wait() {
	s.mu.RLock()
	machines := make([]*supervise.Machine, 0, len(s.machines))
	for _, m := range s.machines {
		machines = append(machines, m.machine)
	}
	s.mu.RUnlock()
	for _, m := range machines {
		m.Wait()
	}
}

// helloResolver maps a shim's hello to the launch its boot token was minted for, once every stored
// claim is supervised. A token of a claim this daemon does not supervise — another project's row
// in a shared database — is unknown: its connection would have no machine to report to. The token
// is resolved first, bounded by timeout: the worker-stream port is plaintext and
// reachable from any pod, Hello2 requires only a non-empty string, and holding an unresolved
// connection with no deadline would let any client pin a goroutine and a file descriptor for the
// whole boot wait by sending a garbage token, with no cap on how many could. Only a token
// ClaimByBootTokenHash proves real — a durable Postgres fact independent of supervision — goes on
// to wait for restoration: a live pane reconnecting while the boot's readiness gate still waits
// out an unreachable NATS or Dispatch (daemon.go's run, LEGION-580) is held, not rejected, on
// s.ctx alone with no deadline of its own, since the shim it answers waits for hello_ack the same
// way and supervision.stop cancels s.ctx before the stream closes, releasing every held hello
// rather than leaving one to time out mid-boot naming a credential problem that does not exist.
// The token is resolved again once restoration ends, under a fresh bound: restore (s.start, ahead
// of close(s.restored)) can relaunch a claim that was mid-launch across the restart to a new
// generation before this hello is ever judged, and the first resolve's generation and Stale flag
// are only ever as fresh as the moment they were read. Judging the hold by that first result would
// accept an old generation's shim after its claim has already relaunched, taking the stream slot
// the new generation's own hello then finds "already bound to a live stream".
//
// A store that fails either read answers the hello with that error, never as an unknown token: the
// failure says nothing about the token, and the listener closes the connection unrefused for the
// shim to redial (stream.HelloResolver).
func (s *supervisor) helloResolver(tokens *api.BootTokens, timeout time.Duration) stream.HelloResolver {
	resolve := func(bootToken string) (api.BootToken, bool, error) {
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		return tokens.Resolve(ctx, bootToken)
	}
	return func(bootToken string) (claim.Token, uint64, bool, bool, error) {
		if _, known, err := resolve(bootToken); err != nil || !known {
			return "", 0, false, false, err
		}
		select {
		case <-s.restored:
		case <-s.ctx.Done():
			s.log.Info("worker stream: a hello's boot token was real, but the boot never became ready", "error", s.ctx.Err())
			return "", 0, false, false, nil
		}
		launch, known, err := resolve(bootToken)
		if err != nil || !known {
			return "", 0, false, false, err
		}
		if _, supervised := s.Machine(launch.Claim); !supervised {
			return "", 0, false, false, nil
		}
		return launch.Claim, launch.Generation, launch.Stale, true, nil
	}
}

// superviseEvent is the machine's event for a worker stream event: every agent fact the stream
// reports has one.
func superviseEvent(ev stream.Event) (supervise.Event, error) {
	switch ev := ev.(type) {
	case stream.Hello:
		hello := supervise.StreamHello{Claim: ev.Claim, Generation: ev.Generation}
		if ev.AgentSecrets != nil {
			hello.AgentSecrets = &supervise.AgentSecretsIdentity{Thumbprint: ev.AgentSecrets.Thumbprint, PodToken: ev.AgentSecrets.PodToken}
		}
		return hello, nil
	case stream.TurnStart:
		return supervise.StreamTurnStart{Claim: ev.Claim, DeliveryID: ev.DeliveryID}, nil
	case stream.TurnEnd:
		return supervise.StreamTurnEnd{Claim: ev.Claim}, nil
	case stream.LateRefusal:
		return supervise.StreamLateRefusal{Claim: ev.Claim, DeliveryID: ev.DeliveryID, Error: ev.Error, Replayed: ev.Replayed,
			ConnSequence: ev.ConnSequence}, nil
	case stream.Closed:
		return supervise.StreamClosed{Claim: ev.Claim}, nil
	}
	return nil, fmt.Errorf("worker stream: no supervise event for a %T", ev)
}

// claimOf is the claim a stream event's supervise event is for.
func claimOf(ev supervise.Event) claim.Token {
	switch ev := ev.(type) {
	case supervise.StreamHello:
		return ev.Claim
	case supervise.StreamTurnStart:
		return ev.Claim
	case supervise.StreamTurnEnd:
		return ev.Claim
	case supervise.StreamLateRefusal:
		return ev.Claim
	case supervise.StreamClosed:
		return ev.Claim
	}
	return ""
}

// ofProject is the claims of this daemon's project: a database two legions share holds both.
func ofProject(claims []supervise.Claim, project string) []supervise.Claim {
	var own []supervise.Claim
	for _, c := range claims {
		if c.Project == project {
			own = append(own, c)
		}
	}
	return own
}

// knownClaims is what the orphan sweep is told of: every claim that is not retired, with its
// process when one runs. A suspended or failed claim is known with no locator, so a runtime that
// deletes what belongs to no known claim leaves what such a claim still holds.
func knownClaims(claims []supervise.Claim) []runtime.Known {
	var known []runtime.Known
	for _, c := range claims {
		if c.State != supervise.StateRetired {
			known = append(known, runtime.Known{Claim: c.Token, Locator: c.Locator})
		}
	}
	return known
}

// inbox is one machine's queue of events: unbounded, so neither pump ever waits on a machine that
// is deciding — the worker stream's own queue is unbounded for the same reason — and in order,
// so a claim sees its events as its sources reported them.
type inbox struct {
	mu     sync.Mutex
	events []supervise.Event
	closed bool
	wake   chan struct{}
}

func newInbox() *inbox { return &inbox{wake: make(chan struct{}, 1)} }

func (b *inbox) put(ev supervise.Event) {
	b.mu.Lock()
	b.events = append(b.events, ev)
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// take is the next event, waiting for one; false once the inbox is closed, whatever it still
// holds.
func (b *inbox) take() (supervise.Event, bool) {
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return nil, false
		}
		if len(b.events) > 0 {
			ev := b.events[0]
			b.events = b.events[1:]
			b.mu.Unlock()
			return ev, true
		}
		b.mu.Unlock()
		<-b.wake
	}
}

func (b *inbox) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}
