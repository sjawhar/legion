package duty

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// holdName is the name every hold test takes; each test runs on a database of its own.
const holdName = "dispatch-outbox"

// mustTakeHold takes holdName for holder on db and fails the test when it is refused. The hold is
// released when the test ends.
func mustTakeHold(t *testing.T, db Holds, holder uuid.UUID, opts HoldOptions) *Hold {
	t.Helper()
	hold, err := TryHold(context.Background(), db, holdName, holder, opts)
	if err != nil {
		t.Fatalf("take the hold: %v", err)
	}
	if hold == nil {
		t.Fatal("the hold was refused")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	return hold
}

// holdRow reads holdName's row: its holder, token and expiry, on the database's clock.
func holdRow(t *testing.T, db Holds) (holder uuid.UUID, token string, expiresAt time.Time) {
	t.Helper()
	if err := db.QueryRow(context.Background(),
		`select holder, token, expires_at from duty_holds where name = $1`, holdName).Scan(&holder, &token, &expiresAt); err != nil {
		t.Fatalf("read the hold's row: %v", err)
	}
	return holder, token, expiresAt
}

// waitLost waits up to within for hold's Lost to close and fails the test when it does not.
func waitLost(t *testing.T, hold *Hold, within time.Duration) {
	t.Helper()
	select {
	case <-hold.Lost():
	case <-time.After(within):
		t.Fatalf("Lost did not close within %s", within)
	}
}

// Two processes, each with a pool of its own: the second cannot take a hold the first holds live,
// and the first takes its own hold again.
func TestHoldRefusesASecondHolderWhileTheFirstHoldsItLive(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second}
	first := uuid.New()
	mustTakeHold(t, database.Pool, first, opts)

	refused, err := TryHold(context.Background(), second.Pool, holdName, uuid.New(), opts)
	if err != nil {
		t.Fatalf("try the live hold from the second process: %v", err)
	}
	if refused != nil {
		t.Fatal("the second process took a hold the first holds live")
	}
	if holder, _, _ := holdRow(t, database.Pool); holder != first {
		t.Fatalf("the row names %s, want the first holder %s", holder, first)
	}

	mustTakeHold(t, second.Pool, first, opts)
}

// A holder that stops renewing, as a killed process does, lets the hold lapse on the database's
// clock, and a standby that arms one timer to the lapse through Expiry takes it within a second of
// it, measured on that clock: the take-over writes now() + TTL, so its own expiry minus the TTL is
// the moment it ran.
func TestHoldTakesOverWithinASecondAfterTheLapse(t *testing.T) {
	database := storetest.Open(t)
	dying := storetest.SecondProcess(t, database)
	opts := HoldOptions{TTL: 2 * time.Second, Renew: 300 * time.Millisecond, Margin: time.Second}
	ctx := context.Background()

	first, err := TryHold(ctx, dying.Pool, holdName, uuid.New(), opts)
	if err != nil || first == nil {
		t.Fatalf("take the hold on the first process: hold %v, error %v", first, err)
	}
	// Closing the first process's pool waits for its renewal in flight and fails every later one.
	dying.Pool.Close()
	_, _, lapse := holdRow(t, database.Pool)

	standby := uuid.New()
	if refused, err := TryHold(ctx, database.Pool, holdName, standby, opts); err != nil || refused != nil {
		t.Fatalf("the standby's try before the lapse: hold %v, error %v; want neither", refused, err)
	}
	remaining, held, err := Expiry(ctx, database.Pool, holdName)
	if err != nil || !held || remaining <= 0 {
		t.Fatalf("Expiry before the lapse = %s, held %v, error %v; want a positive remainder of a held name", remaining, held, err)
	}
	time.Sleep(remaining)
	var taken *Hold
	for give := time.Now().Add(3 * time.Second); taken == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(give) {
			t.Fatal("the standby did not take the hold within 3 s of its expiry")
		}
		if taken, err = TryHold(ctx, database.Pool, holdName, standby, opts); err != nil {
			t.Fatalf("the standby's try: %v", err)
		}
	}
	t.Cleanup(func() { _ = taken.Release(context.Background()) })

	holder, _, expiresAt := holdRow(t, database.Pool)
	if holder != standby {
		t.Fatalf("the row names %s, want the standby %s", holder, standby)
	}
	after := expiresAt.Add(-opts.TTL).Sub(lapse)
	if after < 0 || after > time.Second {
		t.Fatalf("the standby took the hold %s after the lapse on the database's clock, want within [0, 1s]", after)
	}
	select {
	case <-first.Lost():
	default:
		t.Error("the first holder had not reported its loss when the standby took the hold")
	}
}

// A holder whose renewals cannot land - its row locked by another transaction for longer than a
// renewal waits - reports the loss before the row lapses on the database's clock: the loss
// deadline is Margin ahead of the expiry the last confirmed renewal wrote.
func TestHoldReportsLossBeforeTheDatabaseExpiryWhenRenewalsFail(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second}
	ctx := context.Background()
	holder := uuid.New()
	hold := mustTakeHold(t, database.Pool, holder, opts)

	locker, err := second.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()
	if _, err := locker.Exec(ctx, `select 1 from duty_holds where name = $1 for update`, holdName); err != nil {
		t.Fatalf("lock the hold's row: %v", err)
	}
	if waiting := storetest.WaitForLockWaiters(t, locker, 1, 5*time.Second); waiting < 1 {
		t.Fatal("no renewal queued behind the locked row")
	}

	waitLost(t, hold, 2*opts.TTL)
	var (
		lapsed bool
		named  uuid.UUID
		ahead  int64
	)
	if err := locker.QueryRow(ctx, `
		select clock_timestamp() >= expires_at, holder, (extract(epoch from expires_at - clock_timestamp()) * 1000000)::bigint
		from duty_holds where name = $1`, holdName).Scan(&lapsed, &named, &ahead); err != nil {
		t.Fatalf("read the row once Lost closed: %v", err)
	}
	margin := time.Duration(ahead) * time.Microsecond
	if lapsed {
		t.Fatalf("Lost closed %s after the row lapsed on the database's clock", -margin)
	}
	if named != holder {
		t.Fatalf("the row names %s, want the holder %s: the loss must come from the deadline, not a take-over", named, holder)
	}
	if !time.Now().After(hold.Deadline()) {
		t.Errorf("Lost is closed but the deadline %s is still ahead", time.Until(hold.Deadline()))
	}
	t.Logf("Lost closed %s before the row's expiry on the database's clock", margin)
}

// Release frees the name at once, so a standby takes it on its next try, and wakes every session
// listening on HoldReleasedChannel with the name.
func TestHoldReleaseFreesAtOnceAndNotifiesTheName(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second}
	ctx := context.Background()

	listener, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("open the listening connection: %v", err)
	}
	defer func() { _ = listener.Close(context.Background()) }()
	if _, err := listener.Exec(ctx, `listen `+HoldReleasedChannel); err != nil {
		t.Fatalf("listen: %v", err)
	}

	hold := mustTakeHold(t, database.Pool, uuid.New(), opts)
	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case <-hold.Lost():
	default:
		t.Error("Lost is open after Release")
	}
	if _, held, err := Expiry(ctx, database.Pool, holdName); err != nil || held {
		t.Fatalf("Expiry after Release: held %v, error %v; want no row", held, err)
	}

	waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	notice, err := listener.WaitForNotification(waiting)
	if err != nil {
		t.Fatalf("wait for the release notification: %v", err)
	}
	if notice.Channel != HoldReleasedChannel || notice.Payload != holdName {
		t.Fatalf("notification on %q with %q, want %q with %q", notice.Channel, notice.Payload, HoldReleasedChannel, holdName)
	}

	mustTakeHold(t, second.Pool, uuid.New(), opts)
}

// A renewal that affects no row - the row taken over by another holder after a lapse - closes Lost
// at once, long before the loss deadline, and the holder's Release then leaves the new holder's
// row alone.
func TestHoldZeroRowRenewalClosesLost(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	opts := HoldOptions{TTL: 10 * time.Second, Renew: 200 * time.Millisecond, Margin: time.Second}
	ctx := context.Background()
	hold := mustTakeHold(t, database.Pool, uuid.New(), opts)

	other := uuid.New()
	if _, err := second.Pool.Exec(ctx, `update duty_holds set holder = $2, expires_at = now() + interval '10 seconds' where name = $1`,
		holdName, other); err != nil {
		t.Fatalf("take the row over for another holder: %v", err)
	}
	deadline := hold.Deadline()
	waitLost(t, hold, 3*time.Second)
	if !time.Now().Before(deadline) {
		t.Fatal("Lost closed at the deadline rather than on the zero-row renewal")
	}

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release after the loss: %v", err)
	}
	if holder, _, _ := holdRow(t, database.Pool); holder != other {
		t.Fatalf("the row names %s after the lost holder's Release, want the new holder %s", holder, other)
	}
}

// The token the take records reads back, SetToken replaces it, and SetToken on a row another
// holder has taken changes nothing and reports the loss.
func TestHoldTokenReadsBack(t *testing.T) {
	database := storetest.Open(t)
	opts := HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second, Token: "deliver.first"}
	ctx := context.Background()
	hold := mustTakeHold(t, database.Pool, uuid.New(), opts)

	if _, token, _ := holdRow(t, database.Pool); token != "deliver.first" {
		t.Fatalf("token = %q, want the take's %q", token, "deliver.first")
	}
	if err := hold.SetToken(ctx, "deliver.second"); err != nil {
		t.Fatalf("set the token: %v", err)
	}
	if _, token, _ := holdRow(t, database.Pool); token != "deliver.second" {
		t.Fatalf("token = %q, want %q", token, "deliver.second")
	}

	if _, err := database.Pool.Exec(ctx, `update duty_holds set holder = $2 where name = $1`, holdName, uuid.New()); err != nil {
		t.Fatalf("take the row over for another holder: %v", err)
	}
	if err := hold.SetToken(ctx, "deliver.third"); !errors.Is(err, ErrHoldLost) {
		t.Fatalf("SetToken on another holder's row = %v, want ErrHoldLost", err)
	}
	if _, token, _ := holdRow(t, database.Pool); token != "deliver.second" {
		t.Fatalf("token = %q after a refused SetToken, want %q", token, "deliver.second")
	}
	select {
	case <-hold.Lost():
	default:
		t.Error("Lost is open after SetToken found another holder's row")
	}
}

// Expiry reads expires_at - now() on the database's clock: within 100 ms of the same difference
// read directly, held for a live row, held with a negative remainder for a lapsed one, and not held
// with no row.
func TestHoldExpiryMatchesTheDatabaseClock(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	opts := HoldOptions{TTL: 3 * time.Second, Renew: 500 * time.Millisecond, Margin: time.Second}

	if _, held, err := Expiry(ctx, database.Pool, holdName); err != nil || held {
		t.Fatalf("Expiry with no row: held %v, error %v; want not held", held, err)
	}

	mustTakeHold(t, database.Pool, uuid.New(), opts)
	remaining, held, err := Expiry(ctx, database.Pool, holdName)
	if err != nil || !held {
		t.Fatalf("Expiry of a live hold: held %v, error %v", held, err)
	}
	var expiresAt, now time.Time
	if err := database.Pool.QueryRow(ctx, `select expires_at, now() from duty_holds where name = $1`, holdName).Scan(&expiresAt, &now); err != nil {
		t.Fatalf("read the expiry directly: %v", err)
	}
	direct := expiresAt.Sub(now)
	if gap := (remaining - direct).Abs(); gap > 100*time.Millisecond {
		t.Fatalf("Expiry = %s, expires_at - now() = %s: %s apart, want within 100ms", remaining, direct, gap)
	}

	lapsedExpiry(t, database)
}

// lapsedExpiry rewrites holdName's row to a holder that lapsed two seconds ago, as a killed
// holder's row reads, and checks Expiry reports it held with the negative remainder.
func lapsedExpiry(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `update duty_holds set holder = $2, expires_at = now() - interval '2 seconds' where name = $1`,
		holdName, uuid.New()); err != nil {
		t.Fatalf("rewrite the row as lapsed: %v", err)
	}
	remaining, held, err := Expiry(ctx, database.Pool, holdName)
	if err != nil || !held {
		t.Fatalf("Expiry of a lapsed row: held %v, error %v; want held", held, err)
	}
	if remaining > -2*time.Second+100*time.Millisecond || remaining < -2*time.Second-100*time.Millisecond {
		t.Fatalf("Expiry of a row lapsed 2 s ago = %s, want about -2s", remaining)
	}
}

// Options under which no renewal could be confirmed before the loss deadline are refused before
// any statement is sent.
func TestHoldRefusesOptionsNoRenewalCouldBeConfirmedUnder(t *testing.T) {
	for _, opts := range []HoldOptions{
		{TTL: 6 * time.Second, Renew: 4 * time.Second, Margin: 2 * time.Second},
		{TTL: 2 * time.Second, Renew: time.Second, Margin: 2 * time.Second},
		{TTL: 6 * time.Second, Renew: 0, Margin: 2 * time.Second},
		{TTL: 6 * time.Second, Renew: time.Second, Margin: -time.Second},
	} {
		if hold, err := TryHold(context.Background(), nil, holdName, uuid.New(), opts); err == nil || hold != nil {
			t.Errorf("TryHold with %+v = hold %v, error %v; want a refusal", opts, hold, err)
		}
	}
}
