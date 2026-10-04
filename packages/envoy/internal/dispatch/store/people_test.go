package store

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

func TestPgPeopleStoreKeepsEachPersonsMembership(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, "signing-key", nil)
	email := "person-" + randomDatabaseSuffix(t) + "@d.example"

	if _, found, err := people.Membership(ctx, email); err != nil || found {
		t.Fatalf("membership before any sign-in: found=%t err=%v, want not found", found, err)
	}
	signedIn := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if err := people.SignIn(ctx, email, "refresh-1", signedIn); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	// Recording a person who already signed in through the pool keeps their membership.
	if err := people.Record(ctx, email); err != nil {
		t.Fatalf("record: %v", err)
	}
	membership, found, err := people.Membership(ctx, email)
	if err != nil || !found || membership.RefreshToken != "refresh-1" || !membership.ConfirmedAt.Equal(signedIn) {
		t.Fatalf("membership after sign-in = %#v found=%t err=%v, want refresh-1 confirmed %s", membership, found, err, signedIn)
	}

	confirmed := signedIn.Add(90 * time.Minute)
	if err := people.Confirm(ctx, email, "refresh-2", confirmed); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	membership, _, err = people.Membership(ctx, email)
	if err != nil || membership.RefreshToken != "refresh-2" || !membership.ConfirmedAt.Equal(confirmed) {
		t.Fatalf("membership after confirm = %#v err=%v, want refresh-2 confirmed %s", membership, err, confirmed)
	}

	if err := people.End(ctx, email); err != nil {
		t.Fatalf("end: %v", err)
	}
	membership, found, err = people.Membership(ctx, email)
	if err != nil || !found || membership.RefreshToken != "" || !membership.ConfirmedAt.IsZero() {
		t.Fatalf("membership after end = %#v found=%t err=%v, want the person kept with neither", membership, found, err)
	}
}

// People are named by lowercase email; the table refuses any other spelling, so no writer can
// make one person two rows.
func TestPeopleTableRefusesAnEmailThatIsNotLowercase(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, "signing-key", nil)
	for _, email := range []string{"Sami@d.example", ""} {
		if err := people.Record(ctx, email); err == nil {
			t.Errorf("Record(%q) was accepted", email)
		}
	}
}

// storedRefreshToken is people.refresh_token for email as the table holds it, as a backup copies
// it; nil when the row holds none.
func storedRefreshToken(t *testing.T, database *Store, email string) *string {
	t.Helper()
	var stored *string
	if err := database.Pool.QueryRow(context.Background(), `select refresh_token from people where email = $1`, email).Scan(&stored); err != nil {
		t.Fatalf("read the stored refresh token of %s: %v", email, err)
	}
	return stored
}

// captureLogs sends slog's default logger to a buffer until t ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// revokingPool is a sign-in pool on this machine with Dispatch's app client and the revocation
// endpoint its discovery names, and the code flow a people store revokes through, as main hands it
// the one it discovers.
func revokingPool(t *testing.T) (*oidctest.Issuer, *oidc.CodeFlow) {
	t.Helper()
	pool := oidctest.New(t)
	pool.EnableCodeFlow(pool.PublishKey(t, "pool-key"), "dispatch-client", "client-secret")
	flow, err := oidc.NewCodeFlow(context.Background(), pool.URL(), "dispatch-client", "client-secret")
	if err != nil {
		t.Fatalf("discover the sign-in pool: %v", err)
	}
	return pool, flow
}

// storePlainRefreshToken stores token for email in plain text with the statement the release
// before sealing signs a person in with, as a task of that release still does during a roll or
// after a rollback.
func storePlainRefreshToken(t *testing.T, database *Store, email, token string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into people (email, refresh_token, confirmed_at) values ($1, $2, $3)
		on conflict (email) do update
		set signed_in_at = now(), refresh_token = excluded.refresh_token, confirmed_at = excluded.confirmed_at
	`, email, token, time.Now()); err != nil {
		t.Fatalf("store the refresh token of %s in plain text: %v", email, err)
	}
}

// migratedEmptyStore is a database of the test's own at the current schema, so a sweep of people
// meets only the rows the test writes.
func migratedEmptyStore(t *testing.T) *Store {
	t.Helper()
	database := openEmptyTestStore(t)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return database
}

// retireLines counts the lines logging a refresh token the pool did not revoke.
func retireLines(logs *bytes.Buffer) int {
	return strings.Count(logs.String(), "the sign-in pool did not revoke a refresh token stored in plain text")
}

// The row a backup copies never holds the refresh token: a sign-in and a confirmation each store it
// sealed under the signing key, in the versioned format, and only a store under that key opens it.
func TestPgPeopleStoreSealsTheRefreshTokenAtRest(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	people := NewPgPeopleStore(database.Pool, "signing-key", nil)
	email := "sealed-" + randomDatabaseSuffix(t) + "@d.example"
	confirmed := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, write := range []struct {
		name  string
		write func(token string) error
	}{
		{"sign-in", func(token string) error { return people.SignIn(ctx, email, token, confirmed) }},
		{"confirmation", func(token string) error { return people.Confirm(ctx, email, token, confirmed) }},
	} {
		token := "refresh-" + write.name + "-" + randomDatabaseSuffix(t)
		if err := write.write(token); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		stored := storedRefreshToken(t, database, email)
		if stored == nil {
			t.Fatalf("after the %s the row holds no refresh token", write.name)
		}
		if strings.Contains(*stored, token) || !strings.HasPrefix(*stored, "v1:") {
			t.Fatalf("after the %s the row holds %q, want the token sealed in the v1: format", write.name, *stored)
		}
		membership, found, err := people.Membership(ctx, email)
		if err != nil || !found || membership.RefreshToken != token || !membership.ConfirmedAt.Equal(confirmed) {
			t.Fatalf("membership after the %s = %#v found=%t err=%v, want %s confirmed %s", write.name, membership, found, err, token, confirmed)
		}
	}
}

// A refresh token that does not open is no refresh token: one sealed under another signing key (the
// key rotated), one sealed for another person (a value moved between rows), and one stored in plain
// text. Each read answers none, so the person signs in again, and logs that, naming the person and
// never the value; the read writes nothing and asks the pool nothing. The person's next sign-in
// replaces the value, revoking one in plain text first; a sealed value is never sent to the pool.
func TestPgPeopleStoreReadsARefreshTokenThatDoesNotOpenAsNone(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	confirmed := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for name, unopened := range map[string]struct {
		store   func(t *testing.T, people *PgPeopleStore, email, token string)
		revoked bool
	}{
		"sealed under another signing key": {store: func(t *testing.T, _ *PgPeopleStore, email, token string) {
			if err := NewPgPeopleStore(database.Pool, "rotated-signing-key", nil).SignIn(ctx, email, token, confirmed); err != nil {
				t.Fatal(err)
			}
		}},
		"sealed for another person": {store: func(t *testing.T, people *PgPeopleStore, email, token string) {
			other := "other-" + email
			if err := people.SignIn(ctx, other, token, confirmed); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Pool.Exec(ctx, `insert into people (email, refresh_token, confirmed_at) select $1, refresh_token, confirmed_at from people where email = $2`, email, other); err != nil {
				t.Fatal(err)
			}
		}},
		"stored in plain text": {store: func(t *testing.T, _ *PgPeopleStore, email, token string) {
			storePlainRefreshToken(t, database, email, token)
		}, revoked: true},
	} {
		t.Run(name, func(t *testing.T) {
			pool, flow := revokingPool(t)
			people := NewPgPeopleStore(database.Pool, "signing-key", flow)
			email := "unopened-" + randomDatabaseSuffix(t) + "@d.example"
			token := "refresh-" + randomDatabaseSuffix(t)
			unopened.store(t, people, email, token)
			stored := storedRefreshToken(t, database, email)
			if stored == nil {
				t.Fatal("the row holds no refresh token to read")
			}
			logs := captureLogs(t)
			for read := range 2 {
				membership, found, err := people.Membership(ctx, email)
				if err != nil || !found || membership.RefreshToken != "" || !membership.ConfirmedAt.IsZero() {
					t.Fatalf("read %d: membership = %#v found=%t err=%v, want the person with no refresh token or confirmation", read, membership, found, err)
				}
			}
			if kept := storedRefreshToken(t, database, email); kept == nil || *kept != *stored {
				t.Errorf("the row holds %v after the reads, want the value the reads found, unchanged", kept)
			}
			if got := pool.RevocationRequests(); len(got) != 0 {
				t.Errorf("the reads sent %q to the sign-in pool, want nothing", got)
			}
			out := logs.String()
			if count := strings.Count(out, "a stored refresh token did not open"); count != 2 || !strings.Contains(out, email) {
				t.Errorf("logged %d lines naming the unopened token, want one per read naming %s:\n%s", count, email, out)
			}
			if strings.Contains(out, token) || strings.Contains(out, *stored) {
				t.Errorf("the log carries the stored value:\n%s", out)
			}
			if err := people.SignIn(ctx, email, "refresh-again", confirmed); err != nil {
				t.Fatal(err)
			}
			if membership, _, err := people.Membership(ctx, email); err != nil || membership.RefreshToken != "refresh-again" {
				t.Errorf("membership after signing in again = %#v err=%v, want refresh-again", membership, err)
			}
			var want []string
			if unopened.revoked {
				want = []string{token}
			}
			if got := pool.RevocationRequests(); !reflect.DeepEqual(got, want) {
				t.Errorf("signing in again sent %q to the sign-in pool, want %q", got, want)
			}
		})
	}
}

// A boot retires every refresh token people holds in plain text: it revokes each at the sign-in
// pool, then forgets it with its confirmation, keeping the person. A sealed token is never sent to
// the pool and stays, as does a person holding none, and a second boot finds nothing to do.
func TestRetirePlainRefreshTokensRevokesEachThenForgetsIt(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	pool, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, "signing-key", flow)
	storePlainRefreshToken(t, database, "plain-1@d.example", "plain-token-1")
	storePlainRefreshToken(t, database, "plain-2@d.example", "plain-token-2")
	if err := people.SignIn(ctx, "sealed@d.example", "sealed-token", time.Now()); err != nil {
		t.Fatal(err)
	}
	sealed := storedRefreshToken(t, database, "sealed@d.example")
	if err := people.Record(ctx, "recorded@d.example"); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	retired, failed, err := people.RetirePlainRefreshTokens(ctx)
	if err != nil || retired != 2 || failed != 0 {
		t.Fatalf("RetirePlainRefreshTokens = %d retired, %d failed, %v; want 2, 0", retired, failed, err)
	}
	if got, want := pool.RevocationRequests(), []string{"plain-token-1", "plain-token-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the sign-in pool was sent %q to revoke, want %q: each plain token once, nothing sealed", got, want)
	}
	for _, email := range []string{"plain-1@d.example", "plain-2@d.example", "recorded@d.example"} {
		membership, found, err := people.Membership(ctx, email)
		if err != nil || !found || membership.RefreshToken != "" || !membership.ConfirmedAt.IsZero() {
			t.Errorf("%s after the boot = %#v found=%t err=%v, want the person kept with no refresh token or confirmation", email, membership, found, err)
		}
	}
	if kept := storedRefreshToken(t, database, "sealed@d.example"); kept == nil || *kept != *sealed {
		t.Errorf("the sealed row holds %v after the boot, want its sealed value unchanged", kept)
	}
	if membership, _, err := people.Membership(ctx, "sealed@d.example"); err != nil || membership.RefreshToken != "sealed-token" {
		t.Errorf("the sealed sign-in after the boot = %#v err=%v, want sealed-token", membership, err)
	}
	if strings.Contains(logs.String(), "plain-token") {
		t.Errorf("the log carries a token:\n%s", logs.String())
	}

	if retired, failed, err := people.RetirePlainRefreshTokens(ctx); err != nil || retired != 0 || failed != 0 {
		t.Errorf("a second boot = %d retired, %d failed, %v; want nothing to do", retired, failed, err)
	}
	if got := pool.RevocationRequests(); len(got) != 2 {
		t.Errorf("after a second boot the sign-in pool was sent %q, want nothing more", got)
	}
}

// A token the sign-in pool does not revoke stays in its row, for the next boot to try again: the
// boot logs one ERROR naming the person and the pool's HTTP status, never the value, and counts
// it. The next boot the pool answers retires it.
func TestRetirePlainRefreshTokensLeavesATokenThePoolDidNotRevoke(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	pool, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, "signing-key", flow)
	email := "plain@d.example"
	storePlainRefreshToken(t, database, email, "plain-token")
	pool.FailRevocation(http.StatusServiceUnavailable, "")

	logs := captureLogs(t)
	if retired, failed, err := people.RetirePlainRefreshTokens(ctx); err != nil || retired != 0 || failed != 1 {
		t.Fatalf("a boot the pool refuses = %d retired, %d failed, %v; want 0, 1", retired, failed, err)
	}
	if kept := storedRefreshToken(t, database, email); kept == nil || *kept != "plain-token" {
		t.Fatalf("the row holds %v after a revocation the pool refused, want the token kept for the next attempt", kept)
	}
	out := logs.String()
	if retireLines(logs) != 1 || !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "email="+email) || !strings.Contains(out, "status=503") {
		t.Errorf("logged, want one ERROR naming %s and status=503:\n%s", email, out)
	}
	if strings.Contains(out, "plain-token") {
		t.Errorf("the log carries the token:\n%s", out)
	}

	pool.FailRevocation(0, "")
	if retired, failed, err := people.RetirePlainRefreshTokens(ctx); err != nil || retired != 1 || failed != 0 {
		t.Fatalf("the next boot = %d retired, %d failed, %v; want 1, 0", retired, failed, err)
	}
	if kept := storedRefreshToken(t, database, email); kept != nil {
		t.Errorf("the row holds %q after the pool revoked it, want it forgotten", *kept)
	}
}

// The retirement runs at every boot, not once: a token a task of the earlier release stores in
// plain text after this release booted (during the roll, or after a rollback) is retired at the
// next boot.
func TestRetirePlainRefreshTokensRetiresATokenStoredAfterTheLastBoot(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	pool, flow := revokingPool(t)
	people := NewPgPeopleStore(database.Pool, "signing-key", flow)
	boot := func() (retired, failed int) {
		t.Helper()
		if err := database.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		retired, failed, err := people.RetirePlainRefreshTokens(ctx)
		if err != nil {
			t.Fatalf("RetirePlainRefreshTokens: %v", err)
		}
		return retired, failed
	}
	if retired, failed := boot(); retired != 0 || failed != 0 {
		t.Fatalf("the first boot = %d retired, %d failed, want nothing to do", retired, failed)
	}
	email := "rolled-back@d.example"
	storePlainRefreshToken(t, database, email, "stored-after-the-boot")
	if retired, failed := boot(); retired != 1 || failed != 0 {
		t.Errorf("the next boot = %d retired, %d failed, want 1, 0", retired, failed)
	}
	if kept := storedRefreshToken(t, database, email); kept != nil {
		t.Errorf("after the next boot the row holds %q, want it forgotten", *kept)
	}
	if got := pool.RevocationRequests(); !reflect.DeepEqual(got, []string{"stored-after-the-boot"}) {
		t.Errorf("the sign-in pool was sent %q, want the token stored after the first boot", got)
	}
}

// A sign-in replaces the refresh token a row holds; one in plain text is retired first, so the
// replacement never forgets a token the pool still accepts without revoking it. When the pool does
// not revoke it, the sign-in logs that, naming the person and the status and never the value, and
// proceeds: the person just signed in at the pool.
func TestPgPeopleStoreSignInRetiresThePlainTokenItReplaces(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, status := range []int{0, http.StatusBadRequest} {
		pool, flow := revokingPool(t)
		people := NewPgPeopleStore(database.Pool, "signing-key", flow)
		email := "replaced-" + randomDatabaseSuffix(t) + "@d.example"
		plain := "plain-" + randomDatabaseSuffix(t)
		storePlainRefreshToken(t, database, email, plain)
		pool.FailRevocation(status, "invalid_request")
		logs := captureLogs(t)
		if err := people.SignIn(ctx, email, "refresh-fresh", time.Now()); err != nil {
			t.Fatalf("pool answering %d: sign in: %v", status, err)
		}
		if got := pool.RevocationRequests(); !reflect.DeepEqual(got, []string{plain}) {
			t.Errorf("pool answering %d: the sign-in sent %q to revoke, want the plain token it replaces", status, got)
		}
		if membership, _, err := people.Membership(ctx, email); err != nil || membership.RefreshToken != "refresh-fresh" {
			t.Errorf("pool answering %d: membership = %#v err=%v, want the new sign-in's refresh-fresh", status, membership, err)
		}
		out := logs.String()
		if wantLines := map[bool]int{true: 0, false: 1}[status == 0]; retireLines(logs) != wantLines ||
			(wantLines == 1 && (!strings.Contains(out, "email="+email) || !strings.Contains(out, "status=400"))) {
			t.Errorf("pool answering %d: logged, want %d ERROR lines naming %s and the status:\n%s", status, wantLines, email, out)
		}
		if strings.Contains(out, plain) {
			t.Errorf("pool answering %d: the log carries the token:\n%s", status, out)
		}
	}
}
