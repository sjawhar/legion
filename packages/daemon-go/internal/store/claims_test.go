package store

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// at is a time Postgres stores exactly: timestamptz keeps microseconds.
func at(minute int) time.Time {
	return time.Date(2026, 9, 22, 18, minute, 7, 123456000, time.UTC)
}

func tmuxClaim(token claim.Token) supervise.Claim {
	return supervise.Claim{
		Token:       token,
		Project:     "legion",
		Tree:        "LEGION-208",
		Issue:       "LEGION-209",
		Role:        claim.RoleImplementer,
		Generation:  4,
		Session:     "ses_implementer",
		SessionFile: "/state/sessions/implementer.jsonl",
		Locator: &runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       token,
			Incarnation: "31001:900001",
			Tmux:        &runtime.TmuxLocator{Window: "@3", Pane: "%41"},
		},
		State:           supervise.StateIdle,
		Budgets:         supervise.Budgets{LaunchFailures: 1, Deaths: 2, PromptFailures: 2, PromptRetires: 1},
		BootTokenHash:   supervise.HashBootToken("boot-" + string(token)),
		CapabilityHash:  []byte{0xca, 0xfe, 0x01},
		UncertainStreak: 3,
		// The two columns a restart reads to fence a stop and to attribute a completion: a daemon
		// that rebuilt this claim without them would suspend a run that had already replaced the
		// one its stop names, and refuse the completion of the worker it is waiting on.
		LastStartRow:      7,
		ServingGeneration: 4,
	}
}

// sameClaim compares two claims field by field, with the delivery's times compared as instants:
// Postgres hands a timestamptz back in the session's zone, not the one it was written in.
func sameClaim(t *testing.T, got, want supervise.Claim) {
	t.Helper()
	gotPending, wantPending := got.Pending, want.Pending
	got.Pending, want.Pending = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claim read back differs:\n got %+v\nwant %+v", got, want)
	}
	if (gotPending == nil) != (wantPending == nil) {
		t.Fatalf("pending delivery read back = %+v, want %+v", gotPending, wantPending)
	}
	if gotPending == nil {
		return
	}
	if gotPending.ID != wantPending.ID || gotPending.Task != wantPending.Task ||
		gotPending.Phase != wantPending.Phase || gotPending.Generation != wantPending.Generation {
		t.Errorf("pending delivery read back = %+v, want %+v", *gotPending, *wantPending)
	}
	for _, field := range []struct {
		name      string
		got, want time.Time
	}{
		{"QueuedAt", gotPending.QueuedAt, wantPending.QueuedAt},
		{"DeliveredAt", gotPending.DeliveredAt, wantPending.DeliveredAt},
		{"ConfirmedAt", gotPending.ConfirmedAt, wantPending.ConfirmedAt},
	} {
		if !field.got.Equal(field.want) {
			t.Errorf("pending delivery %s = %v, want %v", field.name, field.got, field.want)
		}
	}
}

func onlyClaim(t *testing.T, store *Store) supervise.Claim {
	t.Helper()
	claims, err := store.Claims(context.Background())
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d rows, want 1: %+v", len(claims), claims)
	}
	return claims[0]
}

func TestAClaimRoundTripsWithItsLocatorAndDelivery(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	want := tmuxClaim("legion-LEGION-209-implementer")
	want.Pending = &supervise.Delivery{
		ID:          "delivery-1",
		Task:        "implement the plan",
		Phase:       phase.Implementing,
		Generation:  4,
		QueuedAt:    at(1),
		DeliveredAt: at(2),
		Interrupted: true,
	}

	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if err := store.PutDelivery(ctx, want.Token, *want.Pending); err != nil {
		t.Fatalf("put delivery: %v", err)
	}

	sameClaim(t, onlyClaim(t, store), want)
}

// The phase a task was queued for is what says the task is still the work to do, and it has to
// survive the path that rewrites the delivery: a prompt refused or unanswered is re-put under a
// new id, and a restart then reads the delivery back from this table. A phase lost on either side
// of that round trip brings back the drop this column exists to make: the daemon would send a
// finished phase's task to a worker that had moved on, silently, after every restart.
func TestARePutDeliveryKeepsThePhaseItWasQueuedFor(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-211-implementer")
	queued := supervise.Delivery{ID: "outbox:42", Task: "Continue. Phase: implementing.", Phase: phase.Implementing, QueuedAt: at(1)}
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if err := store.PutDelivery(ctx, c.Token, queued); err != nil {
		t.Fatalf("put delivery: %v", err)
	}

	// What promptFailed does: the same task under a new id, unconfirmed.
	rotated := queued
	rotated.ID = "S4YXQ2F7TDNX"
	if err := store.PutDelivery(ctx, c.Token, rotated); err != nil {
		t.Fatalf("re-put the delivery under its new id: %v", err)
	}

	read := onlyClaim(t, store)
	if read.Pending == nil {
		t.Fatal("the restarted daemon read no pending delivery")
	}
	if read.Pending.ID != rotated.ID || read.Pending.Phase != phase.Implementing {
		t.Fatalf("delivery read back = %+v, want %s queued for %s", read.Pending, rotated.ID, phase.Implementing)
	}
}

func TestAClaimWithNothingOptionalRoundTripsAsNothing(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	want := supervise.Claim{
		Token:   "legion-LEGION-210-architect",
		Project: "legion",
		Tree:    "LEGION-210",
		Issue:   "LEGION-210",
		Role:    claim.RoleArchitect,
		State:   supervise.StateQueued,
	}

	if err := store.PutClaim(ctx, want); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	sameClaim(t, onlyClaim(t, store), want)
}

func TestPutClaimOverwritesTheClaimItNames(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	first := tmuxClaim("legion-LEGION-209-tester")
	if err := store.PutClaim(ctx, first); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	second := first
	second.Generation = 5
	second.State = supervise.StateSuspended
	second.Locator = nil
	second.Budgets = supervise.Budgets{}
	second.UncertainStreak = 0
	if err := store.PutClaim(ctx, second); err != nil {
		t.Fatalf("put claim again: %v", err)
	}

	sameClaim(t, onlyClaim(t, store), second)
}

// A locator is checked where it is read back: a row nothing could act on is refused by name rather
// than loaded as a process the daemon believes in.
func TestClaimsRefusesAStoredLocatorThatDoesNotValidate(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	stored := tmuxClaim("legion-LEGION-209-reviewer")
	if err := store.PutClaim(ctx, stored); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	if _, err := store.pool.Exec(ctx,
		`update claims set locator = '{"runtime":"tmux","claim":"legion-LEGION-209-reviewer","incarnation":"1:2"}' where token = $1`,
		string(stored.Token),
	); err != nil {
		t.Fatalf("write a locator with no tmux member: %v", err)
	}

	_, err := store.Claims(ctx)
	if err == nil {
		t.Fatal("claims loaded a locator with no tmux member")
	}
	for _, want := range []string{"legion-LEGION-209-reviewer", "no tmux member"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

func TestClaimByBootTokenHashFindsTheClaimCarryingIt(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	implementer := tmuxClaim("legion-LEGION-209-implementer")
	tester := tmuxClaim("legion-LEGION-209-tester")
	for _, c := range []supervise.Claim{implementer, tester} {
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("put claim %s: %v", c.Token, err)
		}
	}

	got, ok, err := store.ClaimByBootTokenHash(ctx, supervise.HashBootToken("boot-legion-LEGION-209-tester"))
	if err != nil {
		t.Fatalf("claim by boot token hash: %v", err)
	}
	if !ok {
		t.Fatal("no claim found for the tester's boot token hash")
	}
	sameClaim(t, got, tester)

	_, ok, err = store.ClaimByBootTokenHash(ctx, supervise.HashBootToken("a token no launch minted"))
	if err != nil {
		t.Fatalf("claim by an unknown boot token hash: %v", err)
	}
	if ok {
		t.Error("an unknown boot token hash found a claim")
	}
}

func TestADeliveryIsPutConfirmedAndRetired(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-merger")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	delivery := supervise.Delivery{ID: "delivery-7", Task: "merge-ready check", QueuedAt: at(3)}
	if err := store.PutDelivery(ctx, c.Token, delivery); err != nil {
		t.Fatalf("put delivery: %v", err)
	}
	c.Pending = &delivery
	sameClaim(t, onlyClaim(t, store), c)

	delivery.DeliveredAt = at(4)
	delivery.ConfirmedAt = at(5)
	if err := store.PutDelivery(ctx, c.Token, delivery); err != nil {
		t.Fatalf("put the confirmed delivery: %v", err)
	}
	c.Pending = &delivery
	sameClaim(t, onlyClaim(t, store), c)

	c.Pending = nil
	if err := store.RetireDelivery(ctx, c, delivery.ID); err != nil {
		t.Fatalf("retire delivery: %v", err)
	}
	sameClaim(t, onlyClaim(t, store), c)
}

// Retiring is fenced on the delivery id: a caller retiring a delivery the claim no longer holds
// is told so, and the delivery the claim does hold stays.
func TestRetireDeliveryRefusesADeliveryTheClaimDoesNotHold(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-planner")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	delivery := supervise.Delivery{ID: "delivery-new", Task: "plan it", QueuedAt: at(6)}
	if err := store.PutDelivery(ctx, c.Token, delivery); err != nil {
		t.Fatalf("put delivery: %v", err)
	}

	// The claim the refused retire carries is rolled back with it: the delivery and the run the
	// claim serves move together or not at all.
	stale := c
	stale.ServingGeneration = 99
	err := store.RetireDelivery(ctx, stale, "delivery-old")
	if err == nil {
		t.Fatal("retired a delivery the claim does not hold")
	}
	if !strings.Contains(err.Error(), "delivery-old") {
		t.Errorf("refusal does not name the delivery it refused: %v", err)
	}
	c.Pending = &delivery
	sameClaim(t, onlyClaim(t, store), c)
}

func TestPutDeliveryRefusesAClaimTheStoreDoesNotHold(t *testing.T) {
	store := migratedStore(t)
	err := store.PutDelivery(context.Background(), "legion-LEGION-404-tester",
		supervise.Delivery{ID: "delivery-1", Task: "t", QueuedAt: at(7)})
	if err == nil {
		t.Fatal("put a delivery for a claim the store does not hold")
	}
}

// A confirmation writes the claim and its delivery together. The delivery write failing must take
// the claim's with it: a stored claim recorded as working whose delivery never landed is a task
// the shim has already answered and nothing sends again.
func TestPutClaimAndDeliveryWritesBothOrNeither(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-212-tester")
	c.State = supervise.StateIdle
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	working := c
	working.State = supervise.StateWorking
	// A generation no bigint holds: the delivery's write is refused, inside the transaction that
	// carries the claim's.
	confirmed := supervise.Delivery{ID: "delivery-1", Task: "test it", QueuedAt: at(1),
		DeliveredAt: at(2), ConfirmedAt: at(3), Generation: math.MaxUint64}
	if err := store.PutClaimAndDelivery(ctx, working, confirmed); err == nil {
		t.Fatal("wrote a delivery whose generation no bigint holds")
	}

	stored := onlyClaim(t, store)
	if stored.State != supervise.StateIdle || stored.Pending != nil {
		t.Fatalf("claim read back = %s with pending %+v, want the idle row it was, unchanged",
			stored.State, stored.Pending)
	}
}
