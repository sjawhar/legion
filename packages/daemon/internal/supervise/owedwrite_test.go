package supervise

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// countingStore is the store double with a tally of every write a decision asked for, the ones
// that failed included. memStore records what it took, which is what a test of the rows reads;
// a test that holds "the write was attempted once, and no second write followed it" has to count
// the calls instead, because the write this change is about is exactly the one the store did not
// take.
type countingStore struct {
	Store
	mu    sync.Mutex
	calls []string
}

func (s *countingStore) record(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, method)
}

func (s *countingStore) PutClaim(ctx context.Context, c Claim) error {
	s.record("PutClaim")
	return s.Store.PutClaim(ctx, c)
}

func (s *countingStore) PutDelivery(ctx context.Context, token claim.Token, d Delivery) error {
	s.record("PutDelivery")
	return s.Store.PutDelivery(ctx, token, d)
}

func (s *countingStore) PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	s.record("PutClaimAndDelivery")
	return s.Store.PutClaimAndDelivery(ctx, c, d)
}

func (s *countingStore) RetireDelivery(ctx context.Context, c Claim, deliveryID string) error {
	s.record("RetireDelivery")
	return s.Store.RetireDelivery(ctx, c, deliveryID)
}

// writes is every write asked for since the machine was built, in order.
func (s *countingStore) writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// mark is how many writes have been asked for now, for a test that reads what one decision asked
// (writes()[mark:]).
func (s *countingStore) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// newCountedHarness is newHarness over a store that counts what each decision asked for. The
// claim is seeded through the double itself, as the spawn route leaves it, so the tally holds the
// machine's own writes and nothing else.
func newCountedHarness(t *testing.T) (*harness, *countingStore) {
	t.Helper()
	h := newBareHarness(t)
	counted := &countingStore{Store: h.store}
	h.deps.Store = counted
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, counted
}

// dialFailure is the error a pool whose address nothing listens on answers a statement with: the
// first row of the measured table in owed.go, built from pgx itself rather than written out, so
// the rule is put to the identity the driver really returns rather than to a hand-made stand-in.
// Port 1 has no listener, so the dial is refused before any server reads anything.
func dialFailure(t *testing.T) error {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("build a pool at an address nothing listens on: %v", err)
	}
	defer pool.Close()
	_, err = pool.Exec(context.Background(), "select 1")
	if err == nil {
		t.Fatal("a statement against 127.0.0.1:1 succeeded; the test needs an address nothing listens on")
	}
	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		t.Fatalf("the dial failure is %T (%v), want a *pgconn.ConnectError", err, err)
	}
	return err
}

// refusingServer listens for Postgres connections and refuses each at its startup message with a
// FATAL error of code and message, as a managed instance that is still starting up answers a new
// connection (57P03) and as one whose password has been rotated answers it (28P01). It returns
// its address.
//
// The shape is carried by hand from refusingServer in packages/envoy's
// internal/dispatch/outbox/lookups_test.go, where the same two steps of the same rule are put to
// the same identity; that is another module, so it cannot be imported and is copied instead.
//
// A listener is what these rows need, rather than a *pgconn.PgError written out here: an error
// made by hand carries no *pgconn.ConnectError around it, so the rule would reach it at its
// SQLSTATE step and the row would prove nothing at all about the step that reads the wrapper
// first. Only a connection pgx itself refused carries the server's answer inside the wrapper,
// which is the identity these two rows exist for.
func refusingServer(t *testing.T, code, message string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				backend := pgproto3.NewBackend(conn, conn)
				for {
					startup, err := backend.ReceiveStartupMessage()
					if err != nil {
						return
					}
					switch startup.(type) {
					case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
						if _, err := conn.Write([]byte("N")); err != nil {
							return
						}
						continue
					}
					break
				}
				backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: message})
				_ = backend.Flush()
			}()
		}
	}()
	return listener.Addr().String()
}

// refusedConnection is the error a pool answers a statement with when the server refused the
// connection itself at its startup message: a *pgconn.ConnectError carrying the server's own
// *pgconn.PgError of code, which is the last two rows of the measured table in owed.go. Both
// halves of that identity are held here, so a row that passes passes on the error the driver
// really builds rather than on whatever this helper happened to return.
func refusedConnection(t *testing.T, code, message string) error {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@"+refusingServer(t, code, message)+"/db?sslmode=disable")
	if err != nil {
		t.Fatalf("build a pool at a server that refuses every connection: %v", err)
	}
	defer pool.Close()
	_, err = pool.Exec(context.Background(), "select 1")
	if err == nil {
		t.Fatalf("a statement against a server refusing with %s succeeded", code)
	}
	var connErr *pgconn.ConnectError
	var pgErr *pgconn.PgError
	if !errors.As(err, &connErr) || !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("the refused connection is %T (%v), want a *pgconn.ConnectError carrying a *pgconn.PgError of code %s", err, err, code)
	}
	return err
}

// brokenConnection is the server answering that the connection under the statement failed: class
// 08, which is an answer about the connection rather than about the statement, and is owed with
// the failures no server ever read.
func brokenConnection() *pgconn.PgError {
	return &pgconn.PgError{Code: "08006", Message: "server closed the connection unexpectedly", Severity: "FATAL"}
}

// serverError is one answer a server gives about something other than the statement it was
// given, written as pgx returns it from a live connection: the code's class is the whole of what
// the rule reads, and each row below says which class it is holding and what that class is in
// the world.
func serverError(code, message string) func(*testing.T) error {
	return func(*testing.T) error {
		return &pgconn.PgError{Code: code, Message: message, Severity: "FATAL"}
	}
}

// The rule for a claim write the store refused is put to the error identities a daemon of this
// cluster meets, each injected on the claim-only write of a turn start — the decision the
// measured incident lost, and one that leaves its state standing in memory whether or not the
// row takes it.
//
// All but two leave the write owed, and the claim's next decision makes it: the row catches up
// with what memory has held all along. The last two are the server reading the statement and
// deciding against it, which it would decide the same way again, so nothing is owed and nothing
// is written again — the write is attempted once and that is the end of it.
//
// What the owed ones share is not a type: one is a pgx type, one is the standard library's, two
// are a server's answer about a connection it refused, carried inside a pgx type, and the rest
// are a server's answer on a live connection. It is that no server judged the statement, which
// is the one question writeOwed asks.
//
// The two connections the server itself refused are the rows this change's order is for, and
// they are built from a listener that speaks the startup message rather than written out, since
// what they hold is the shape pgx wraps such an answer in. The rotated password is the one that
// decides the order: its class is the server judging something, and a rule that read the
// SQLSTATE before the wrapper would drop the write of a claim fact only memory holds for a
// statement no server ever read.
func TestTheOwedWriteErrorsAreTheOnesNoServerAnsweredAbout(t *testing.T) {
	for _, tc := range []struct {
		name string
		// err is built per test rather than shared, so one row's error identity can never be the
		// thing another row passes or fails on.
		err  func(t *testing.T) error
		owed bool
	}{
		{
			// The measured failure to dial: nothing listens at the address, so the statement
			// never reached a server at all and the row is certainly unwritten.
			name: "a pool that could not dial its server",
			err:  dialFailure,
			owed: true,
		},
		{
			// A managed instance answering a new connection while it restarts: the server
			// spoke, but about the connection it would not open, so no statement of this write
			// was ever read. Its class is one the rule owes anyway, which is exactly why it
			// cannot be the only connection the server refused that is held here.
			name: "a restarting server refusing the connection",
			err: func(t *testing.T) error {
				return refusedConnection(t, "57P03", "the database system is starting up")
			},
			owed: true,
		},
		{
			// A rotated password refusing the connection: class 28, which on a live connection
			// is the server judging what it was given, and here is the server refusing to open
			// the connection at all. The write is owed because nothing of it reached a server,
			// and this row is what holds the wrapper being read before the SQLSTATE.
			name: "a rotated password refusing the connection",
			err: func(t *testing.T) error {
				return refusedConnection(t, "28P01", `password authentication failed for user "legion"`)
			},
			owed: true,
		},
		{
			// The write this change's own deadline cut short (Timeouts.ClaimWrite): the wait for
			// an answer ended before any answer came, and the statement may or may not have been
			// applied, which is the same unknown as a connection that died under it.
			name: "a write the deadline cut short",
			err:  func(*testing.T) error { return context.DeadlineExceeded },
			owed: true,
		},
		{
			// Class 08, connection exception: the connection itself is what failed, whatever
			// the server had read of the statement by then, so the write's fate is unknown.
			name: "a connection exception",
			err:  func(*testing.T) error { return brokenConnection() },
			owed: true,
		},
		{
			// Class 40, transaction rollback: the server rolled its own transaction back over a
			// deadlock it broke, so the statement was abandoned rather than judged, and the
			// same write made again can be taken.
			name: "a transaction the server rolled back",
			err:  serverError("40P01", "deadlock detected"),
			owed: true,
		},
		{
			// Class 53, insufficient resources: the connection slots the server had not got for
			// this statement, and will have again once the load that took them goes.
			name: "resources the server had not got",
			err:  serverError("53300", "sorry, too many clients already"),
			owed: true,
		},
		{
			// Class 55, object not in prerequisite state: a lock another session holds, which
			// is an answer about that session rather than about this statement.
			name: "an object another session holds",
			err:  serverError("55P03", "could not obtain lock on row in relation \"claims\""),
			owed: true,
		},
		{
			// Class 57, operator intervention: the shutdown a managed restart of the database
			// sends, which is the measured one. The server answered, but what it says is that
			// it terminated the backend under the statement.
			name: "the backend terminated under the statement",
			err:  serverError("57P01", "terminating connection due to administrator command"),
			owed: true,
		},
		{
			// Class 58, system error: a read the server's own disk refused it, which says
			// nothing whatever about what was asked of it.
			name: "a system error of the server's own",
			err:  serverError("58030", "could not read block 7 in file \"base/16384/2619\""),
			owed: true,
		},
		{
			// A statement the server read and refused: a unique violation is a decision about
			// this very statement, and making the write again buys the same refusal.
			name: "a statement the server refused",
			err: func(*testing.T) error {
				return &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint", Severity: "ERROR"}
			},
			owed: false,
		},
		{
			// A second statement the server read and refused, from a class of its own: a daemon
			// deployed ahead of its migration writes a column the claims table has not got, and
			// the server decides against that statement as plainly as it decides against a
			// duplicate key. Two refusals are held here rather than one because one row holds
			// only its own class: with 23505 alone, a build that owed every class but 23 would
			// pass this table whole while repairing, at every decision for the life of the
			// daemon, a write the server will refuse again.
			name: "a column the server has not got",
			err: func(*testing.T) error {
				return &pgconn.PgError{Code: "42703", Message: `column "turn_started_at" of relation "claims" does not exist`, Severity: "ERROR"}
			},
			owed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, counted := newCountedHarness(t)
			h.reach(StateReady)
			refusal := tc.err(t)
			stored := h.store.load(testToken)
			attempted := counted.mark()

			h.store.fail("PutClaim", refusal)
			if err := h.handle(StreamTurnStart{Claim: testToken}); !errors.Is(err, refusal) {
				t.Fatalf("the turn start returned %v, want the store's refusal %v", err, refusal)
			}
			if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
				t.Fatalf("the failed decision asked for %v, want the one claim write", asked)
			}
			h.wantState(StateWorking)
			if owed := h.m.owedWrite != nil; owed != tc.owed {
				t.Fatalf("the write is owed = %v, want %v", owed, tc.owed)
			}

			// The store takes writes again, and the claim decides once more.
			h.store.fail("PutClaim", nil)
			attempted = counted.mark()
			h.observe(runtime.Alive)

			asked := counted.writes()[attempted:]
			if !tc.owed {
				if len(asked) != 0 {
					t.Fatalf("a write the server refused was asked for again as %v; nothing is owed for it", asked)
				}
				if now := h.store.load(testToken); now.State != stored.State {
					t.Fatalf("the stored claim reads %s, want %s: a refused write is not made again", now.State, stored.State)
				}
				return
			}
			if !slices.Equal(asked, []string{"PutClaim"}) {
				t.Fatalf("the next decision asked for %v, want the one owed claim write", asked)
			}
			if h.m.owedWrite != nil {
				t.Fatal("the write is still owed after the repair made it")
			}
			if now := h.store.load(testToken); now.State != StateWorking {
				t.Fatalf("the stored claim reads %s, want working: the owed write records the turn that started", now.State)
			}
		})
	}
}

// owedTurnStart is the measured decision: a turn the daemon did not prompt, in a claim with no
// task pending, whose claim-only write fails with a broken connection. The claim stands working
// in memory and owes its write.
func owedTurnStart(t *testing.T) (*harness, *countingStore) {
	t.Helper()
	h, counted := newCountedHarness(t)
	h.reach(StateReady)
	h.store.fail("PutClaim", brokenConnection())
	if err := h.handle(StreamTurnStart{Claim: testToken}); err == nil {
		t.Fatal("the turn start whose write failed returned no error")
	}
	h.wantState(StateWorking)
	if h.m.owedWrite == nil {
		t.Fatal("the failed claim write left no mark")
	}
	return h, counted
}

// A turn start whose claim write the database did not take is the incident this change was
// written for: the agent said it was working, the daemon believed it, and the row went on reading
// ready until something else happened to write the claim. These hold what the mark is worth.
func TestTheWriteATurnStartOwedIsMadeAtTheClaimsNextDecision(t *testing.T) {
	// The write is made once and the decision fails with the store's own error: the mark adds
	// nothing to the decision, it only keeps the write from being lost with it. A second write in
	// the tail of the failing decision would be the machine trying to repair under the same
	// outage, where the whole point is that the repair waits for the claim's next decision.
	t.Run("the decision fails after one write and no other", func(t *testing.T) {
		h, counted := newCountedHarness(t)
		h.reach(StateReady)
		attempted := counted.mark()
		h.store.fail("PutClaim", brokenConnection())

		err := h.handle(StreamTurnStart{Claim: testToken})

		if err == nil {
			t.Fatal("the turn start whose write failed returned no error")
		}
		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
			t.Fatalf("the failed decision asked for %v, want the one claim write", asked)
		}
	})

	// Nothing is rolled back: the turn started, the agent's own frame said so, and the claim the
	// machine holds from here on is the working one. The row is behind it, which is what the mark
	// records.
	t.Run("the claim stands working while the write is owed", func(t *testing.T) {
		h, _ := owedTurnStart(t)

		h.wantState(StateWorking)
		if stored := h.store.load(testToken); stored.State != StateReady {
			t.Fatalf("the stored claim reads %s, want ready: the write the store refused changed nothing there", stored.State)
		}
		if h.m.owedWrite == nil {
			t.Fatal("the failed claim write left no mark")
		}
		owed := h.records.of(owedWriteLine)
		if len(owed) != 1 {
			t.Fatalf("the owed write is logged %d times, want once", len(owed))
		}
		if store, ok := owed[0].attr("store"); !ok || store.String() != "PutClaim" {
			t.Fatalf("the owed line names the store method %v, want PutClaim", store)
		}
	})

	// A write of the delivery alone, made while the claim write is still owed, leaves the mark
	// standing. This is why the mark is its own field and not the unsaved flag: a task queued
	// here writes the delivery and clears unsaved, and a claim write carried on that flag would
	// be dropped by this write without a word — the row would stay ready, and the next restart
	// would read a claim whose agent was in a turn as one waiting for work.
	t.Run("a write of the delivery alone leaves the mark standing", func(t *testing.T) {
		h, counted := owedTurnStart(t)
		attempted := counted.mark()

		h.must(RequestDeliver{Claim: testToken, Task: "the task"})

		// The repair at the head of this decision was refused by the same outage; the task's own
		// delivery write went to a table the outage is not on.
		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim", "PutDelivery"}) {
			t.Fatalf("the deliver decision asked for %v, want the owed claim write and the delivery's own", asked)
		}
		if _, ok := h.store.delivery(testToken); !ok {
			t.Fatal("the task's delivery was not written")
		}
		if h.m.unsaved {
			t.Fatal("the delivery reads unsaved after its own write landed")
		}
		if h.m.owedWrite == nil {
			t.Fatal("the delivery's write cleared the mark the claim write left")
		}
	})

	// The repair writes the claim as the machine holds it then, not a copy taken when the write
	// failed. The claim went on deciding while the database was away — the turn it started has
	// ended since — and a copy would write working over an idle claim, which is a claim a restart
	// reads as one whose agent is still at work.
	t.Run("the repair writes the claim the machine holds then", func(t *testing.T) {
		h, counted := owedTurnStart(t)

		_ = h.handle(StreamTurnEnd{Claim: testToken})
		h.wantState(StateIdle)
		if h.m.owedWrite == nil {
			t.Fatal("the turn's end, whose own write the same outage refused, cleared the mark")
		}

		h.store.fail("PutClaim", nil)
		attempted := counted.mark()
		h.observe(runtime.Alive)

		if asked := counted.writes()[attempted:]; !slices.Equal(asked, []string{"PutClaim"}) {
			t.Fatalf("the repairing decision asked for %v, want the one owed claim write", asked)
		}
		if stored := h.store.load(testToken); stored.State != StateIdle {
			t.Fatalf("the repaired row reads %s, want idle: the claim as the machine holds it now", stored.State)
		}
		if h.m.owedWrite != nil {
			t.Fatal("the write is still owed after the repair made it")
		}
	})
}
