package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// TestMain gives the package's Postgres-backed tests a database each (storetest.Main).
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// laggingDatabaseClock opens a pool on database whose now() runs lag behind this process's clock,
// as a database server's clock may run behind a server process's: a schema of the test's own holds
// a now() answering pg_catalog.now() - lag, and the pool's search_path names that schema ahead of
// pg_catalog, so every unqualified now() a statement on the pool runs reads the lagging clock.
func laggingDatabaseClock(t *testing.T, database *store.Store, lag time.Duration) *store.Pool {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("lagging_clock_%d", time.Now().UnixNano())
	if _, err := database.Pool.Exec(ctx, fmt.Sprintf(`
		create schema %[1]s;
		create function %[1]s.now() returns timestamptz language sql stable as $$ select pg_catalog.now() - interval '%[2]d microseconds' $$;
	`, schema, lag.Microseconds())); err != nil {
		t.Fatalf("make the database's clock lag: %v", err)
	}
	config := database.Pool.Config()
	config.ConnConfig.RuntimeParams["search_path"] = schema + ", public, pg_catalog"
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool on the lagging clock: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return pool
}

// A confirmation's age is the database's own, and Membership reads no clock of its own: with this
// process's clock two hours ahead of the database's, a confirmation the database dates a minute
// short of MembershipMaxAge still stands and asks the issuer nothing, and one it dates a minute
// past it lapses and is refreshed. A process clock in the reckoning would date the first one two
// hours older than the database does and refresh it too.
func TestMembershipAgesAConfirmationByTheDatabasesClock(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	pool := laggingDatabaseClock(t, database, 2*time.Hour)
	people := store.NewPgPeopleStore(pool, testSigningKey, nil)
	if err := people.SignIn(ctx, memberEmail, "refresh-1"); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	var refreshes atomic.Int32
	membership := &Membership{
		People:   people,
		Sessions: &testSessionStore{generations: map[string]int64{memberEmail: 0}},
		Rule:     GroupRule{Group: memberGroup},
		SignIn: refresherFunc(func(context.Context, string) (oidc.Session, error) {
			refreshes.Add(1)
			return oidc.Session{}, fmt.Errorf("oidc: refresh: %w: the test answers none", oidc.ErrIssuerUnavailable)
		}),
	}
	confirmedAgo := func(age time.Duration) {
		t.Helper()
		if _, err := pool.Exec(ctx, `update people set confirmed_at = now() - make_interval(secs => $2) where email = $1`, memberEmail, age.Seconds()); err != nil {
			t.Fatalf("date the confirmation %s back: %v", age, err)
		}
	}

	if err := membership.Confirm(ctx, memberEmail); err != nil || refreshes.Load() != 0 {
		t.Fatalf("Confirm right after the sign-in: err=%v refreshes=%d, want nil and none", err, refreshes.Load())
	}
	confirmedAgo(MembershipMaxAge - time.Minute)
	if err := membership.Confirm(ctx, memberEmail); err != nil || refreshes.Load() != 0 {
		t.Fatalf("Confirm of a confirmation the database dates %s old: err=%v refreshes=%d, want nil and none", MembershipMaxAge-time.Minute, err, refreshes.Load())
	}
	confirmedAgo(MembershipMaxAge + time.Minute)
	if err := membership.Confirm(ctx, memberEmail); !errors.Is(err, ErrSignInUnavailable) || refreshes.Load() != 1 {
		t.Fatalf("Confirm of a confirmation the database dates %s old: err=%v refreshes=%d, want ErrSignInUnavailable after one refresh", MembershipMaxAge+time.Minute, err, refreshes.Load())
	}
}
