package supervise

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

// brokenConnection is the server answering that the connection under the statement failed: class
// 08, which is an answer about the connection rather than about the statement, and is owed with
// the failures no server ever read.
func brokenConnection() *pgconn.PgError {
	return &pgconn.PgError{Code: "08006", Message: "server closed the connection unexpectedly", Severity: "FATAL"}
}

// The rule for a claim write the store refused is put to the five error identities a daemon of
// this cluster actually meets, each injected on the claim-only write of a turn start — the
// decision the measured incident lost, and one that leaves its state standing in memory whether
// or not the row takes it.
//
// Four of them leave the write owed, and the claim's next decision makes it: the row catches up
// with what memory has held all along. The fifth is the server reading the statement and
// deciding against it, which it would decide the same way again, so nothing is owed and nothing
// is written again — the write is attempted once and that is the end of it.
//
// What the four share is not a type: one is a pgx type, one is the standard library's, and two
// are server answers. It is that no server answered about the statement, which is the one
// question writeOwed asks.
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
			// The shutdown a managed restart of the database sends: the server answered, but
			// what it says is that it terminated the backend under the statement, not that it
			// read the statement and decided against it.
			name: "the backend terminated under the statement",
			err: func(*testing.T) error {
				return &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command", Severity: "FATAL"}
			},
			owed: true,
		},
		{
			// A connection exception: the connection itself is what failed, whatever the server
			// had read of the statement by then, so the write's fate is unknown and it is owed.
			name: "a connection exception",
			err:  func(*testing.T) error { return brokenConnection() },
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
			// A statement the server read and refused: a unique violation is a decision about
			// this very statement, and making the write again buys the same refusal.
			name: "a statement the server refused",
			err: func(*testing.T) error {
				return &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint", Severity: "ERROR"}
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
		if h.m.owedWrite.method != "PutClaim" {
			t.Fatalf("the mark names the store method %q, want PutClaim", h.m.owedWrite.method)
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
