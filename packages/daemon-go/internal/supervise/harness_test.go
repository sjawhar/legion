package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

const (
	testToken   claim.Token = "legion-LEGION-209-implementer"
	rootToken   claim.Token = "legion-LEGION-208-architect"
	testBoot                = 120 * time.Second
	testRPC                 = 5 * time.Second
	testProbe               = 30 * time.Second
	intervals               = 3
	deadline                = testBoot * intervals
	session                 = "ses_implementer"
	sessionFile             = "/state/sessions/implementer.jsonl"
)

var epoch = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// fakeClock is time that moves only when the test says so. Advance fires every timer that falls
// due, in deadline order, synchronously and outside the clock's lock — a timer's callback is a
// machine's Handle, which may arm the next timer.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	armed  int
}

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	order   int
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: epoch} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Cancel {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed++
	timer := &fakeTimer{clock: c, at: c.now.Add(d), order: c.armed, f: f}
	c.timers = append(c.timers, timer)
	return timer
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// Advance moves time forward by d, firing what falls due on the way.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	until := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, timer := range c.timers {
			if timer.stopped || timer.fired || timer.at.After(until) {
				continue
			}
			if next == nil || timer.at.Before(next.at) || (timer.at.Equal(next.at) && timer.order < next.order) {
				next = timer
			}
		}
		if next == nil {
			c.now = until
			c.mu.Unlock()
			return
		}
		next.fired = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}

// dropAll is every armed timer dying with the process that armed it.
func (c *fakeClock) dropAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		timer.stopped = true
	}
}

// Live is how many timers are armed and neither fired nor stopped.
func (c *fakeClock) Live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	live := 0
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired {
			live++
		}
	}
	return live
}

// memStore is the store as a double: what was written, in order, and a way to fail a write.
type memStore struct {
	mu         sync.Mutex
	claims     map[claim.Token]Claim
	deliveries map[claim.Token]Delivery
	puts       []Claim
	failures   map[string]error
	// onPut, when set, runs on every PutClaim before it is recorded.
	onPut func(Claim)
}

func newMemStore() *memStore {
	return &memStore{
		claims:     map[claim.Token]Claim{},
		deliveries: map[claim.Token]Delivery{},
		failures:   map[string]error{},
	}
}

func (s *memStore) PutClaim(_ context.Context, c Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failures["PutClaim"]; err != nil {
		return err
	}
	if s.onPut != nil {
		s.onPut(c)
	}
	c = copyClaim(c)
	c.Pending = nil
	s.claims[c.Token] = c
	s.puts = append(s.puts, c)
	return nil
}

func (s *memStore) PutDelivery(_ context.Context, token claim.Token, d Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failures["PutDelivery"]; err != nil {
		return err
	}
	if _, ok := s.claims[token]; !ok {
		return fmt.Errorf("put delivery %s: no claim %s", d.ID, token)
	}
	s.deliveries[token] = d
	return nil
}

func (s *memStore) RetireDelivery(_ context.Context, token claim.Token, deliveryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failures["RetireDelivery"]; err != nil {
		return err
	}
	held, ok := s.deliveries[token]
	if !ok || held.ID != deliveryID {
		return fmt.Errorf("retire delivery %s on %s: the claim holds no such delivery", deliveryID, token)
	}
	delete(s.deliveries, token)
	return nil
}

func (s *memStore) fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method] = err
}

// load is the claim as Claims would read it back after a restart: the row and its delivery.
func (s *memStore) load(token claim.Token) Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := copyClaim(s.claims[token])
	if d, ok := s.deliveries[token]; ok {
		c.Pending = &d
	}
	return c
}

func (s *memStore) delivery(token claim.Token) (Delivery, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[token]
	return d, ok
}

func (s *memStore) history() []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Claim(nil), s.puts...)
}

// testSpecs answers every launch with the same environment, or with the error it was told to, and
// records the claim each launch was built for.
type testSpecs struct {
	err    error
	mu     sync.Mutex
	claims []Claim
}

func (s *testSpecs) SpawnSpec(_ context.Context, c Claim) (runtime.SpawnSpec, error) {
	s.mu.Lock()
	s.claims = append(s.claims, copyClaim(c))
	s.mu.Unlock()
	if s.err != nil {
		return runtime.SpawnSpec{}, s.err
	}
	return runtime.SpawnSpec{
		Env: map[string]string{"LEGION_ISSUE": c.Issue},
	}, nil
}

// last is the claim the latest launch was built for.
func (s *testSpecs) last(t *testing.T) Claim {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.claims) == 0 {
		t.Fatal("no launch was built")
	}
	return s.claims[len(s.claims)-1]
}

// logBuffer is a log handler's destination that tests read from.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines is every log line containing all of the fragments.
func (b *logBuffer) lines(fragments ...string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var matched []string
	for _, line := range strings.Split(b.buf.String(), "\n") {
		all := line != ""
		for _, fragment := range fragments {
			all = all && strings.Contains(line, fragment)
		}
		if all {
			matched = append(matched, line)
		}
	}
	return matched
}

type harness struct {
	t     *testing.T
	ctx   context.Context
	token claim.Token
	rt    *fake.Runtime
	conns *fake.Conns
	conn  *fake.Conn
	store *memStore
	clock *fakeClock
	specs *testSpecs
	logs  *logBuffer
	deps  Deps
	m     *Machine
}

func testLimits() Limits { return Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2} }

func testTimeouts() Timeouts {
	return Timeouts{
		Boot:                  testBoot,
		RegistrationIntervals: intervals,
		RPC:                   testRPC,
		Probe:                 testProbe,
	}
}

func queuedClaim() Claim {
	return Claim{
		Token:   testToken,
		Project: "legion",
		Tree:    "LEGION-208",
		Issue:   "LEGION-209",
		Role:    claim.RoleImplementer,
		State:   StateQueued,
	}
}

// rootClaim is the tree's root claim: the architect of the issue the tree is named for.
func rootClaim() Claim {
	return Claim{
		Token:   rootToken,
		Project: "legion",
		Tree:    "LEGION-208",
		Issue:   "LEGION-208",
		Role:    claim.RoleArchitect,
		State:   StateQueued,
	}
}

// newHarness is a machine over a queued claim the store already holds, as the spawn route would
// leave it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOf(t, queuedClaim())
}

// newHarnessOf is newHarness over the queued claim c; the walks drive c's token.
func newHarnessOf(t *testing.T, c Claim) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.token = c.Token
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h
}

func newBareHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{
		t:     t,
		ctx:   ctx,
		token: testToken,
		rt:    fake.NewRuntime(),
		conns: fake.NewConns(),
		conn:  fake.NewConn(),
		store: newMemStore(),
		clock: newFakeClock(),
		specs: &testSpecs{},
		logs:  &logBuffer{},
	}
	h.rt.Now = h.clock.Now
	h.deps = Deps{
		Runtime:  h.rt,
		Conns:    h.conns,
		Store:    h.store,
		Specs:    h.specs,
		Clock:    h.clock,
		Log:      slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Limits:   testLimits(),
		Timeouts: testTimeouts(),
	}
	return h
}

// start builds the machine over c, as a daemon does for a claim it created or loaded.
func (h *harness) start(c Claim) {
	h.t.Helper()
	m, err := NewMachine(h.ctx, h.deps, c)
	if err != nil {
		h.t.Fatalf("new machine: %v", err)
	}
	h.m = m
}

// restart is a daemon restart: the machine is dropped with whatever it held in memory — its
// timers die with it — and a new one is built from what the store read back.
func (h *harness) restart() {
	h.t.Helper()
	h.m.Wait()
	h.clock.dropAll()
	h.start(h.store.load(h.token))
}

func (h *harness) handle(ev Event) error {
	err := h.m.Handle(h.ctx, ev)
	h.m.Wait()
	return err
}

func (h *harness) must(ev Event) {
	h.t.Helper()
	if err := h.handle(ev); err != nil {
		h.t.Fatalf("handle %T: %v", ev, err)
	}
}

func (h *harness) advance(d time.Duration) {
	h.clock.Advance(d)
	h.m.Wait()
}

func (h *harness) claim() Claim { return h.m.Claim() }

func (h *harness) state() ClaimState { return h.m.Claim().State }

func (h *harness) wantState(want ClaimState) {
	h.t.Helper()
	if got := h.state(); got != want {
		h.t.Fatalf("state = %s, want %s", got, want)
	}
}

func (h *harness) locator() runtime.Locator {
	h.t.Helper()
	loc := h.claim().Locator
	if loc == nil {
		h.t.Fatal("the claim has no locator")
	}
	return *loc
}

func (h *harness) generation() uint64 { return h.claim().Generation }

func (h *harness) observe(kind runtime.ObservationKind) {
	h.t.Helper()
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: kind, At: h.clock.Now()}})
}

func (h *harness) prompts() []fake.Prompt { return h.conn.Prompts() }

func (h *harness) wantPrompts(n int) []fake.Prompt {
	h.t.Helper()
	prompts := h.prompts()
	if len(prompts) != n {
		h.t.Fatalf("prompts sent = %d (%+v), want %d", len(prompts), prompts, n)
	}
	return prompts
}

func (h *harness) pending() Delivery {
	h.t.Helper()
	p := h.claim().Pending
	if p == nil {
		h.t.Fatal("the claim has no pending delivery")
	}
	return *p
}

func (h *harness) budgets() Budgets { return h.claim().Budgets }

func (h *harness) wantBudgets(want Budgets) {
	h.t.Helper()
	if got := h.budgets(); got != want {
		h.t.Fatalf("budgets = %+v, want %+v", got, want)
	}
}

func (h *harness) calls(method string) []fake.Call { return h.rt.CallsOf(method) }

func (h *harness) wantCalls(method string, n int) []fake.Call {
	h.t.Helper()
	calls := h.calls(method)
	if len(calls) != n {
		h.t.Fatalf("%s calls = %d, want %d (all calls: %v)", method, len(calls), n, h.rt.Methods())
	}
	return calls
}

// launch spawns the queued claim.
func (h *harness) launch() {
	h.t.Helper()
	h.must(RequestSpawn{Claim: h.token})
	h.wantState(StateLaunching)
}

// connect is the shim's hello, with its connection in the directory.
func (h *harness) connect() {
	h.t.Helper()
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation()})
}

func (h *harness) register() {
	h.t.Helper()
	h.must(RequestRegister{
		Claim:          h.token,
		Generation:     h.generation(),
		Session:        session,
		SessionFile:    sessionFile,
		CapabilityHash: []byte("capability"),
	})
}

func (h *harness) ready() {
	h.t.Helper()
	h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: session})
}

// relaunched walks a claim the machine has just relaunched back to ready.
func (h *harness) relaunched() {
	h.t.Helper()
	h.wantState(StateLaunching)
	h.connect()
	h.register()
	h.ready()
}

// reach walks the claim from where it is to state through the events that lead there. Working
// and idle are reached through a delivered task, so they carry one: working holds it confirmed,
// idle has retired it.
func (h *harness) reach(state ClaimState) {
	h.t.Helper()
	steps := []ClaimState{StateQueued, StateLaunching, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle}
	if state == StateSuspended {
		h.reach(StateIdle)
		h.must(RequestSuspend{Claim: h.token})
		h.wantState(StateSuspended)
		return
	}
	from, target := slices.Index(steps, h.state()), slices.Index(steps, state)
	if from < 0 || target < from {
		h.t.Fatalf("reach: no walk from %s to %s", h.state(), state)
	}
	for _, step := range steps[from+1 : target+1] {
		switch step {
		case StateLaunching:
			h.launch()
		case StateShimConnected:
			h.connect()
		case StateRegistered:
			h.register()
		case StateReady:
			h.ready()
		case StateWorking:
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
			h.must(StreamTurnStart{Claim: h.token})
		case StateIdle:
			h.must(StreamTurnEnd{Claim: h.token})
		}
		h.wantState(step)
	}
}

// gatedConn holds every prompt at the door until the test lets it through, so a test can land a
// stream event while a send is still in flight.
type gatedConn struct {
	*fake.Conn
	entered chan string
	release chan struct{}
}

func newGatedConn() *gatedConn {
	return &gatedConn{Conn: fake.NewConn(), entered: make(chan string, 8), release: make(chan struct{}, 8)}
}

func (g *gatedConn) Prompt(ctx context.Context, deliveryID, message string) error {
	g.entered <- deliveryID
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.Conn.Prompt(ctx, deliveryID, message)
}

// gatedRuntime holds Suspend until the test lets it through.
type gatedRuntime struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
}

func (g *gatedRuntime) Suspend(ctx context.Context, loc runtime.Locator) error {
	g.entered <- struct{}{}
	<-g.release
	return g.Runtime.Suspend(ctx, loc)
}

func waitFor(t *testing.T, what string, ch <-chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return ""
	}
}

var errBoom = errors.New("boom")
