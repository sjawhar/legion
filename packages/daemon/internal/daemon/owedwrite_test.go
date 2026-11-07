package daemon

// What a database that has stopped answering costs one claim, and what it must not cost the rest
// of the daemon (INFRA-1132). The parts only a real daemon over a real store can show are here:
// the claim-write deadline measured on a decision of a supervised machine, the other claims and
// the readers that go on while one claim's write waits it out, the mark's loss with the process
// that held it, and the owed report arriving at the daemon's own durable-state path rather than
// at the machine's callback. supervise's own tests hold the rule itself.
//
// Each rig is a supervisor of this package's own kind — a real store over a database of the
// test's own (isolatedOutboxPool), claims seeded and restored as a boot restores them — whose
// deps carry a store a test can take away and put back (switchStore). The store is switchable
// rather than rebuilt because supervise.NewMachine copies the Deps it is given: a machine reads
// the store it was built with for the whole of its life, so taking the database away under a
// live claim means handing that one store another target.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	owedProject = "legion"
	owedIssue   = "LEGION-1132"
	// owedPod is the pod every role of the issue runs in, as a sandbox locator records it. Each
	// seeded claim records a process: a live claim restored with none is taken up as
	// launch_uncertain (supervisor.restore), which is not the claim these tests are about.
	owedPod = "0f2d4a6b-8c1e-4f33-9a57-1d2b3c4e5f60"
	// owedWriteFailedLine and owedWriteMadeLine are the two lines supervise writes about a claim
	// write the database did not take: the Warn where the write failed or a repair of it failed
	// again, and the Info where the repair made it. They are read here as an operator reads them,
	// because the mark itself is memory internal/daemon cannot see.
	owedWriteFailedLine = "supervise: a claim write the database did not take"
	owedWriteMadeLine   = "supervise: a claim write the database did not take was made"
)

// loggedMessage is how one of those lines is found in a machine's log: slog's message field,
// whole. The made line begins with the failed one, so a search for the failed line's text alone
// finds the made line too.
func loggedMessage(line string) string { return `"msg":"` + line + `"` }

// owedRole is one claim a rig seeds: the role, and the state the store holds it in.
type owedRole struct {
	role  claim.Role
	state supervise.ClaimState
}

// switchStore is the store a rig puts in a supervisor's deps: every call goes to whatever target
// is in place at the moment of the call, so a test takes the database away under a live claim and
// puts it back without rebuilding a machine.
type switchStore struct {
	mu     sync.Mutex
	target supervise.Store
}

func (s *switchStore) use(target supervise.Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = target
}

func (s *switchStore) now() supervise.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target
}

func (s *switchStore) AdmitClaim(ctx context.Context, c supervise.Claim) (supervise.Claim, error) {
	return s.now().AdmitClaim(ctx, c)
}

func (s *switchStore) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	return s.now().CheckLaunch(ctx, c)
}

func (s *switchStore) PutClaim(ctx context.Context, c supervise.Claim) error {
	return s.now().PutClaim(ctx, c)
}

func (s *switchStore) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	return s.now().PutDelivery(ctx, token, d)
}

func (s *switchStore) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	return s.now().PutClaimAndDelivery(ctx, c, d)
}

func (s *switchStore) RetireDelivery(ctx context.Context, c supervise.Claim, deliveryID string) error {
	return s.now().RetireDelivery(ctx, c, deliveryID)
}

// claimWriteFault is a fault on every claim write of one claim: the writes of that token answer
// fault, and everything else goes to the store behind it. One claim's writes alone are touched,
// so what another claim's machine does meanwhile is the real store's own answer rather than a
// second fault.
type claimWriteFault struct {
	supervise.Store
	token claim.Token
	fault func(context.Context) error
}

func (s *claimWriteFault) PutClaim(ctx context.Context, c supervise.Claim) error {
	if c.Token == s.token {
		return s.fault(ctx)
	}
	return s.Store.PutClaim(ctx, c)
}

func (s *claimWriteFault) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	if c.Token == s.token {
		return s.fault(ctx)
	}
	return s.Store.PutClaimAndDelivery(ctx, c, d)
}

func (s *claimWriteFault) RetireDelivery(ctx context.Context, c supervise.Claim, deliveryID string) error {
	if c.Token == s.token {
		return s.fault(ctx)
	}
	return s.Store.RetireDelivery(ctx, c, deliveryID)
}

// refusing is the fault of a server that answers the write with err.
func refusing(behind supervise.Store, token claim.Token, err error) *claimWriteFault {
	return &claimWriteFault{Store: behind, token: token, fault: func(context.Context) error { return err }}
}

// blocked is the fault of a database that has stopped answering: the write goes to a pool pointed
// at a listener this test opens and never accepts from, so the kernel takes the connection and no
// server ever answers the startup message. The write returns only when something ends its wait,
// which is the whole point — what ends it is Timeouts.ClaimWrite, through the context the machine
// derives for the write.
//
// pgxpool.New dials nothing, so the pool is built at once and the first statement is where the
// wait begins.
func blocked(t *testing.T, behind supervise.Store, token claim.Token) *claimWriteFault {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the database that never answers: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf("postgres://legion@%s/legion?sslmode=disable", listener.Addr().String()))
	if err != nil {
		t.Fatalf("open a pool on the database that never answers: %v", err)
	}
	t.Cleanup(pool.Close)
	return &claimWriteFault{Store: behind, token: token, fault: func(ctx context.Context) error {
		_, err := pool.Exec(ctx, "select 1")
		return err
	}}
}

// brokenConnection is the server saying the connection under the statement failed: class 08,
// which is an answer about the connection and not about the statement, and so one the rule owes
// (supervise's writeOwed). It is what a claim write meets when the database goes away under it.
func brokenConnection() *pgconn.PgError {
	return &pgconn.PgError{Code: "08006", Message: "server closed the connection unexpectedly", Severity: "FATAL"}
}

// owedWriteRig is one supervision over a real store: the claims seeded in a database of the
// test's own, restored as a boot restores them, each machine's events driven by hand.
type owedWriteRig struct {
	t   *testing.T
	ctx context.Context
	// pool is the test's own connection to the same database, for reading what the store holds
	// without asking the supervision.
	pool   *pgxpool.Pool
	st     *store.Store
	sup    *supervisor
	store  *switchStore
	tokens map[claim.Role]claim.Token
	// logs is what the machines wrote, which is where a write the database did not take is
	// reported: the mark itself is supervise's own memory, and the log line is what internal/daemon
	// and an operator read it by.
	logs  *syncBuffer
	write time.Duration
}

// newOwedWriteRig seeds each role's claim in a database of its own and supervises them, with
// write as every machine's claim-write bound.
func newOwedWriteRig(t *testing.T, write time.Duration, roles ...owedRole) *owedWriteRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	dsn := pool.Config().ConnString()
	tokens := map[claim.Role]claim.Token{}
	claims := make([]supervise.Claim, 0, len(roles))
	for _, role := range roles {
		token := mustClaimToken(t, owedIssue, role.role)
		tokens[role.role] = token
		claims = append(claims, supervise.Claim{
			Token: token, Project: owedProject, Tree: owedIssue, Issue: owedIssue, Role: role.role,
			Generation: 1, State: role.state, PluginContract: api.DaemonAPIVersion,
			Session: "ses_" + string(role.role), SessionFile: "/sessions/" + string(role.role) + ".jsonl",
			Locator: podUpdateLocator(token, owedIssue, owedPod),
		})
	}
	seedWorkflowTree(t, dsn, owedProject, record.Issue{
		Key: owedIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: owedIssue, Title: "The claim write the database did not take",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
	}, claims...)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	rig := &owedWriteRig{t: t, ctx: ctx, pool: pool, st: st, tokens: tokens, write: write}
	rig.boot()
	return rig
}

// boot builds the supervision over the store the rig holds and takes up every claim the store
// holds now, which is what a daemon's boot does. It is called again by the restart test, so a
// second supervision is this one and not a test's own arrangement; the rig's handles read the
// newest from here on.
func (r *owedWriteRig) boot() {
	r.t.Helper()
	logs := &syncBuffer{}
	switcher := &switchStore{target: r.st}
	sup := newSupervisor(r.ctx, r.st, owedProject, r.t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: fake.NewRuntime(), Conns: fake.NewConns(), Store: switcher, Specs: outboxSpecs{},
		Clock: stillClock{}, Log: slog.New(slog.NewJSONHandler(logs, nil)),
		Limits: supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second,
			Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond, ClaimWrite: r.write},
		PluginContract: api.DaemonAPIVersion,
	}
	r.t.Cleanup(sup.stop)
	stored, err := sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	if _, err := sup.restore(r.ctx, stored); err != nil {
		r.t.Fatalf("restore the stored claims: %v", err)
	}
	r.sup, r.store, r.logs = sup, switcher, logs
}

func (r *owedWriteRig) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, ok := r.tokens[role]
	if !ok {
		r.t.Fatalf("no claim was seeded for the %s", role)
	}
	return token
}

func (r *owedWriteRig) machine(role claim.Role) *supervise.Machine {
	r.t.Helper()
	m, ok := r.sup.Machine(r.token(role))
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	return m
}

// handle drives one event into a role's machine and returns what the decision answered, as the
// daemon's own sources reach a machine (supervisor.add's feeding).
func (r *owedWriteRig) handle(role claim.Role, ev supervise.Event) error {
	r.t.Helper()
	return r.machine(role).Handle(r.ctx, ev)
}

// turnStart is the agent's own turn start: the decision the measured incident lost, and one whose
// state stands in memory whether or not the row takes it.
func (r *owedWriteRig) turnStart(role claim.Role) supervise.Event {
	r.t.Helper()
	return supervise.StreamTurnStart{Claim: r.token(role)}
}

// nudge is an event that drives a decision and writes nothing of its own: a prompt result naming
// a delivery the claim does not hold, which the fence drops before the table. The repair at the
// head of Handle still runs, so what a test measures around it is the owed write alone.
func (r *owedWriteRig) nudge(role claim.Role) supervise.Event {
	r.t.Helper()
	return supervise.StreamPromptResult{Claim: r.token(role), DeliveryID: "no delivery of this claim", AgentInvoked: true}
}

// held is the claim as its machine holds it.
func (r *owedWriteRig) held(role claim.Role) supervise.ClaimState {
	r.t.Helper()
	return r.machine(role).Claim().State
}

// stored is the claim's state as the database holds it, read on the test's own connection
// (storedClaimState).
func (r *owedWriteRig) stored(role claim.Role) supervise.ClaimState {
	r.t.Helper()
	return storedClaimState(r.t, r.pool, r.token(role))
}

// refuse puts a store in place that answers role's claim writes with err.
func (r *owedWriteRig) refuse(role claim.Role, err error) {
	r.t.Helper()
	r.store.use(refusing(r.st, r.token(role), err))
}

// block puts a store in place whose claim writes for role never answer.
func (r *owedWriteRig) block(role claim.Role) {
	r.t.Helper()
	r.store.use(blocked(r.t, r.st, r.token(role)))
}

// answers puts the real store back: the database is reachable again.
func (r *owedWriteRig) answers() {
	r.t.Helper()
	r.store.use(r.st)
}

// owedTurnStart is the measured incident over a real store: the agent's turn started, the claim
// write failed, and the claim stands working in memory over a row that still reads what it was.
func (r *owedWriteRig) owedTurnStart(role claim.Role) {
	r.t.Helper()
	before := r.stored(role)
	if err := r.handle(role, r.turnStart(role)); err == nil {
		r.t.Fatalf("the %s's turn start, whose claim write the store refused, returned no error", role)
	}
	if got := r.held(role); got != supervise.StateWorking {
		r.t.Fatalf("the %s stands %s after its turn started, want working: nothing is put back", role, got)
	}
	if got := r.stored(role); got != before {
		r.t.Fatalf("the %s's row reads %s, want the %s the refused write left", role, got, before)
	}
}

// The claim-write deadline is what ends a decision's wait on a database that has stopped
// answering. With a claim's write already owed and its store blocked — a pool on a listener that
// never answers — the next decision's head is the repair, and the repair is the write: the
// decision returns at the bound and not at the length of the outage, which has no end of its own
// here. The write it could not make is still owed, because a write a deadline cut short is no
// server's refusal, and it lands at the first decision after the database answers again.
//
// Without the bound the machine's lock is held for as long as the connection's own patience,
// which is what every other claim's event and every read of this claim then waits for
// (TestNoDecisionWaitsOnTheClaimWhoseWriteTheDatabaseHasNotTaken).
func TestTheRepairDeadlineEndsADecisionsWaitOnADatabaseThatHasStoppedAnswering(t *testing.T) {
	// The bound a decision is measured against, and the bound that catches a wait which is not
	// bounded at all: the blocked store never answers, so an unbounded decision returns at no
	// time this test could name.
	const claimWrite = 250 * time.Millisecond
	const unbounded = 10 * time.Second
	rig := newOwedWriteRig(t, claimWrite, owedRole{claim.RoleArchitect, supervise.StateReady})
	rig.refuse(claim.RoleArchitect, brokenConnection())
	rig.owedTurnStart(claim.RoleArchitect)

	rig.block(claim.RoleArchitect)
	rig.logs.Reset()
	began := time.Now()
	err := rig.handle(claim.RoleArchitect, rig.nudge(claim.RoleArchitect))
	elapsed := time.Since(began)

	if err != nil {
		t.Fatalf("the decision under the blocked store = %v, want the dropped event's own nil: a repair that fails must not drop the event behind it", err)
	}
	if elapsed < claimWrite {
		t.Fatalf("the decision returned after %s, sooner than the %s bound: its write was never put to the store", elapsed, claimWrite)
	}
	if elapsed > unbounded {
		t.Fatalf("the decision returned after %s, past the %s bound: it waited on the database rather than on the deadline", elapsed, unbounded)
	}
	// Still owed: the repair's own failure says so, in the line an operator counts.
	if logs := rig.logs.String(); !strings.Contains(logs, loggedMessage(owedWriteFailedLine)) || !strings.Contains(logs, `"owed":true`) {
		t.Fatalf("the repair the deadline cut short logged %s, want %q with owed true: a write a deadline cut short is no refusal, and is owed", logs, owedWriteFailedLine)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the row reads %s while the database is away, want the ready row the first failure left", got)
	}

	// The database answers again, and the claim's next decision makes the write it has owed all
	// along: the row catches up with what memory has held since the turn started.
	rig.answers()
	rig.logs.Reset()
	if err := rig.handle(claim.RoleArchitect, rig.nudge(claim.RoleArchitect)); err != nil {
		t.Fatalf("the decision after the database answered again = %v, want none", err)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateWorking {
		t.Fatalf("the row reads %s after the repair, want the working claim the machine holds", got)
	}
	if logs := rig.logs.String(); !strings.Contains(logs, loggedMessage(owedWriteMadeLine)) {
		t.Fatalf("the repair that landed logged %s, want %q", logs, owedWriteMadeLine)
	}
}

// What the bound buys the rest of the daemon. One claim's writes never answer, so its machine is
// in a decision for the whole of this test; everything else goes on inside the deadline. Another
// claim's turn start is applied and reaches its own row, a read that waits on the held machine's
// lock is answered rather than held for the outage, and the supervisor's stop returns.
//
// This is the whole worth of bounding the write: the machine's lock is what an event of this
// claim, a read of it, and the stop all wait on, and a decision that waits out the connection's
// own patience holds every one of them for the length of the outage.
func TestNoDecisionWaitsOnTheClaimWhoseWriteTheDatabaseHasNotTaken(t *testing.T) {
	const claimWrite = 500 * time.Millisecond
	// answered is what each wait here is measured against: several claim-write bounds, and far
	// less than the outage, which never ends. The daemon's own stop allows its whole shutdown 15
	// seconds (daemon.stop), so a stop inside this is a stop inside that.
	const answered = 10 * time.Second
	rig := newOwedWriteRig(t, claimWrite,
		owedRole{claim.RoleArchitect, supervise.StateReady},
		owedRole{claim.RoleImplementer, supervise.StateReady})
	rig.refuse(claim.RoleArchitect, brokenConnection())
	rig.owedTurnStart(claim.RoleArchitect)
	rig.block(claim.RoleArchitect)

	// The architect's machine decides for the rest of this test: every one of these events drives
	// a decision whose head is the repair, and every repair waits its bound out against a store
	// that never answers.
	architect := rig.token(claim.RoleArchitect)
	const decisions = 20
	for range decisions {
		rig.sup.post(architect, rig.nudge(claim.RoleArchitect))
	}
	testwait.Eventually(t, "the architect's machine to be deciding under the outage", func() bool {
		return rig.machine(claim.RoleArchitect).Deciding() != nil
	})

	// The implementer's own turn start is applied while that goes on, and its row takes it.
	began := time.Now()
	implementer := rig.token(claim.RoleImplementer)
	rig.sup.post(implementer, supervise.StreamTurnStart{Claim: implementer})
	testwait.Eventually(t, "the implementer's turn start to reach its row", func() bool {
		return rig.stored(claim.RoleImplementer) == supervise.StateWorking
	})
	if waited := time.Since(began); waited > answered {
		t.Fatalf("the implementer's turn start took %s, past %s: one claim's outage is holding another claim's decision", waited, answered)
	}

	// A read that waits on the held machine's lock — the registration route's read of the claim
	// (internal/api/claims.go), which is Machine.Claim() — is answered inside the deadline.
	read := make(chan time.Duration, 1)
	// The machine is taken here, on the test's own goroutine: a helper that fails the test must
	// not run on another.
	machine := rig.machine(claim.RoleArchitect)
	go func() {
		began := time.Now()
		_ = machine.Claim()
		read <- time.Since(began)
	}()
	select {
	case waited := <-read:
		if waited > answered {
			t.Fatalf("the read of the architect's claim waited %s, past %s: it is held for the outage rather than for the deadline", waited, answered)
		}
	case <-time.After(answered):
		t.Fatalf("the read of the architect's claim was not answered within %s: it is held for the outage", answered)
	}

	// The outage is still on: the architect's row is what the failed write left, and its claim
	// still stands working in memory alone.
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the architect's row reads %s, want the ready row its failed write left: its database answered after all", got)
	}
	if got := rig.held(claim.RoleArchitect); got != supervise.StateWorking {
		t.Fatalf("the architect stands %s, want working", got)
	}

	// The stop returns though the claim it waits for is in a write that never answers: what it
	// waits out is that decision's deadline.
	testwait.Eventually(t, "the architect's machine to be deciding again", func() bool {
		return rig.machine(claim.RoleArchitect).Deciding() != nil
	})
	stopped := make(chan time.Duration, 1)
	go func() {
		began := time.Now()
		rig.sup.stop()
		stopped <- time.Since(began)
	}()
	select {
	case waited := <-stopped:
		if waited > answered {
			t.Fatalf("the supervisor's stop took %s, past %s: it waits the outage out", waited, answered)
		}
	case <-time.After(answered):
		t.Fatalf("the supervisor's stop did not return within %s: it waits the outage out", answered)
	}
}

// The mark is memory and nothing else: a daemon that stops with a write owed loses it, and loses
// the reports owed with it. That is the spec's own answer — the loss is exactly what a crash
// between a decision and its write has always cost, and no part of this change makes a claim fact
// durable that was not durable before — and the point of holding it is that nothing is wedged by
// it. The next boot reads the stored row, takes the claim up from it, and the claim decides
// again.
func TestAnOwedWriteAcrossARestartIsLostAndTheStoredClaimIsTakenUp(t *testing.T) {
	const claimWrite = 250 * time.Millisecond
	rig := newOwedWriteRig(t, claimWrite, owedRole{claim.RoleArchitect, supervise.StateReady})
	rig.refuse(claim.RoleArchitect, brokenConnection())
	rig.owedTurnStart(claim.RoleArchitect)

	// The daemon stops with the write owed, and the supervision is built again over the same
	// store, as the next boot builds it.
	rig.sup.stop()
	rig.boot()

	// The claim is taken up from the row the failed write left, not from the working claim the
	// stopped daemon held: the mark went with the process that held it.
	if got := rig.held(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the restored architect stands %s, want the ready row the failed write left", got)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the architect's row reads %s after the restart, want ready: nothing made the lost write", got)
	}

	// And it decides again: the claim is a claim like any other, with nothing owed and nothing
	// held over from the daemon that stopped.
	if err := rig.handle(claim.RoleArchitect, rig.turnStart(claim.RoleArchitect)); err != nil {
		t.Fatalf("the restored architect's turn start = %v, want none", err)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateWorking {
		t.Fatalf("the restored architect's row reads %s after a turn started, want working", got)
	}
}

// The report a decision owed reaches the daemon itself, and not only the machine's callback. A
// claim whose ready the store did not take tells the daemon nothing at the time — the fact the
// daemon would act on is one the row does not hold — and once the repair has made the write, the
// report travels the daemon's own path: Machine.OnDurableState to supervisor.durableState to
// workflowRuntime.claimDurableState, which applies the claim's ready as a workflow fact
// (applyTerminal). The fact's own row is what this reads, because that row is what the daemon
// did with the report: written in the transaction that applies the fact, keyed on the claim and
// the generation the report named (terminalEventID), and never written by anything else.
func TestTheOwedReportsReachTheDaemonOnceTheRepairLands(t *testing.T) {
	const claimWrite = 250 * time.Millisecond
	rig := newOwedWriteRig(t, claimWrite, owedRole{claim.RoleArchitect, supervise.StateRegistered})
	// The daemon's own durable-state path over the rig's real pool, as the workflow installs it
	// on every machine of its supervisor (workflowRuntime.attach).
	workflow := &workflowRuntime{pool: rig.pool, records: record.NewStore(), log: quietLogger()}
	rig.sup.OnDurableState(workflow.claimDurableState)
	architect := rig.machine(claim.RoleArchitect).Claim()
	readyFact := terminalEventID(architect, supervise.StateReady)

	// The agent says it is ready and the store refuses the claim write: the claim stands ready in
	// memory, its row still reads registered, and the daemon has not been told.
	rig.refuse(claim.RoleArchitect, brokenConnection())
	if err := rig.handle(claim.RoleArchitect, supervise.RequestReady{
		Claim: architect.Token, Generation: architect.Generation, Session: architect.Session,
	}); err == nil {
		t.Fatal("the ready whose claim write the store refused returned no error")
	}
	if got := rig.held(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the architect stands %s after it said it was ready, want ready", got)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateRegistered {
		t.Fatalf("the architect's row reads %s, want the registered row the refused write left", got)
	}
	if rig.applied(readyFact) {
		t.Fatalf("the daemon applied %s while the claim write was owed: it was told a claim fact the store does not hold", readyFact)
	}

	// The database answers again, and the claim's next decision makes the owed write and the
	// report behind it. The daemon learns what it would have learnt had the write been taken,
	// only later.
	rig.answers()
	if err := rig.handle(claim.RoleArchitect, rig.nudge(claim.RoleArchitect)); err != nil {
		t.Fatalf("the decision after the database answered again = %v, want none", err)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the architect's row reads %s after the repair, want ready", got)
	}
	if !rig.applied(readyFact) {
		t.Fatalf("the daemon never applied %s: the report the owed write skipped never reached claimDurableState", readyFact)
	}
}

// applied is whether the daemon has applied the supervision fact of this event id, which
// applyTerminal writes in the fact's own transaction (intake.ApplyFact).
func (r *owedWriteRig) applied(eventID string) bool {
	r.t.Helper()
	var applied bool
	if err := r.pool.QueryRow(r.ctx,
		"select exists (select 1 from processed_events where source = $1 and event_id = $2)",
		terminalSource, eventID).Scan(&applied); err != nil {
		r.t.Fatalf("read whether the daemon applied %s: %v", eventID, err)
	}
	return applied
}
