package store

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
)

// mintCall is one token a fakeMinter minted: the endpoint and user it was asked for.
type mintCall struct{ endpoint, user string }

// fakeMinter stands in for RDS: it answers token as every connection's password and records what
// each mint was asked for. A test signs in with it to a role whose password is token, so a
// connection that did not sign in with the minted token is refused by Postgres itself.
type fakeMinter struct {
	mu    sync.Mutex
	token string
	calls []mintCall
}

func (m *fakeMinter) mint(_ context.Context, endpoint, user string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, mintCall{endpoint, user})
	return m.token, nil
}

func (m *fakeMinter) set(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = token
}

func (m *fakeMinter) minted() []mintCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mintCall(nil), m.calls...)
}

// passwordRole creates a login role on the BROKER_TEST_DATABASE_URL server whose password is
// password, and a database of its own the role may create in, both dropped when t ends, and
// returns the role's name and a URL that signs in as it to that database with no password, as an
// IAM-form URL does. Postgres asks the role for a password over TCP (the test server's pg_hba),
// so only a connection that brings one signs in. The database is its own rather than the
// server's shared one because these rows create tables and hold locks on them, which no other
// test on the server has any business seeing (dispatch://INFRA-1075).
func passwordRole(t *testing.T, password string) (role, databaseURL string) {
	t.Helper()
	raw := testDatabaseURL(t)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random role name: %v", err)
	}
	name := hex.EncodeToString(suffix[:])
	role = "broker_iam_test_" + name
	adminExec(t, raw, "create role "+role+" login password '"+password+"'")
	// The role owns nothing outside the database below, which goes first, so dropping the role
	// is all its cleanup owes.
	t.Cleanup(func() { adminExec(t, raw, "drop role "+role) })
	// The database is a file copy of template1 (docs/testing.md, "A database a Go test
	// creates"), and its drop forces off whatever connection a failed row left on it, so the
	// role that owns nothing in it can then be dropped too. Cleanups run last-registered-first,
	// so the database goes before the role.
	database := "broker_iam_db_" + name
	adminExec(t, raw, "create database "+database+" strategy file_copy")
	t.Cleanup(func() { adminExec(t, raw, "drop database "+database+" with (force)") })
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse BROKER_TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + database
	adminExec(t, parsed.String(), "grant all on schema public to "+role)
	parsed.User = url.User(role)
	return role, parsed.String()
}

// signedInWith is databaseURL carrying password, for a connection that brings a password of its
// own rather than a minted token: it signs in before the rotation below and owns what it locks,
// where the server's own user owns nothing the role created.
func signedInWith(t *testing.T, databaseURL, password string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(parsed.User.Username(), password)
	return parsed.String()
}

// endpointOf is the host:port pgx dials for databaseURL, which RDS signs a token for.
func endpointOf(t *testing.T, databaseURL string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort(parsed.Hostname(), cmp.Or(parsed.Port(), "5432"))
}

// TestOpenSignsEveryNewConnectionInWithAFreshlyMintedToken pins the IAM sign-in: with a token
// minter, every new pooled connection signs in with a token minted for it, for the URL's
// host:port and user, rather than one minted at startup, since an RDS token is good for 15
// minutes and a pool opens connections for as long as the broker runs. The role's password is
// the fake token, so a connection that brought anything else would be refused; and the same URL
// opened without the minter is refused, so the token is what signed in.
func TestOpenSignsEveryNewConnectionInWithAFreshlyMintedToken(t *testing.T) {
	ctx := context.Background()
	role, databaseURL := passwordRole(t, "fake-iam-token-1")
	minter := &fakeMinter{token: "fake-iam-token-1"}

	s, err := Open(ctx, databaseURL, WithTokenMinter(minter.mint))
	if err != nil {
		t.Fatalf("Open with the token minter: %v", err)
	}
	t.Cleanup(s.Pool.Close)
	want := mintCall{endpoint: endpointOf(t, databaseURL), user: role}
	if got := minter.minted(); len(got) != 1 || got[0] != want {
		t.Fatalf("Open minted %+v, want one token for %+v", got, want)
	}

	first, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the pool's first connection: %v", err)
	}
	defer first.Release()
	second, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a second connection while the first is held: %v", err)
	}
	defer second.Release()
	var signedInAs string
	if err := second.QueryRow(ctx, "select current_user").Scan(&signedInAs); err != nil {
		t.Fatalf("read the second connection's user: %v", err)
	}
	if signedInAs != role {
		t.Fatalf("second connection signed in as %q, want %q", signedInAs, role)
	}
	if got := minter.minted(); len(got) != 2 || got[1] != want {
		t.Fatalf("after a second connection the minter was asked for %+v, want a second token for %+v", got, want)
	}

	if s, err := Open(ctx, databaseURL); err == nil {
		s.Pool.Close()
		t.Fatal("Open without the token minter signed in to a role that has a password; the test proves nothing")
	} else if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("Open without the token minter = %v, want Postgres to refuse the password", err)
	}
}

// TestMigrateLockWatchSignsInWithAFreshToken pins the lock watch's own connection to the pool's
// sign-in. The watch dials while a migration waits for a lock, from the configuration of the
// connection the migration runs on, which may have signed in up to the pool's connection lifetime
// before, longer than an RDS token lasts. Here the role's password moves on after the migration's
// connection signed in, as a token's expiry would: a watch that reused that connection's token is
// refused and cannot say which lock the migration waited for, while one that mints its own names
// the lock and its holder.
func TestMigrateLockWatchSignsInWithAFreshToken(t *testing.T) {
	ctx := context.Background()
	raw := testDatabaseURL(t)
	role, databaseURL := passwordRole(t, "fake-iam-token-1")
	holderURL := signedInWith(t, databaseURL, "fake-iam-token-1")

	minter := &fakeMinter{token: "fake-iam-token-1"}
	s, err := Open(ctx, databaseURL, WithTokenMinter(minter.mint))
	if err != nil {
		t.Fatalf("Open with the token minter: %v", err)
	}
	t.Cleanup(s.Pool.Close)
	if _, err := s.Pool.Exec(ctx, "create table guard_held (id integer)"); err != nil {
		t.Fatalf("create the held table: %v", err)
	}
	holder, err := pgx.Connect(ctx, holderURL)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder's transaction: %v", err)
	}
	defer held.Rollback(ctx)
	if _, err := held.Exec(ctx, "lock table guard_held in access share mode"); err != nil {
		t.Fatalf("hold the table: %v", err)
	}

	adminExec(t, raw, "alter role "+role+" password 'fake-iam-token-2'")
	minter.set("fake-iam-token-2")
	if got := len(minter.minted()); got != 1 {
		t.Fatalf("the minter minted %d tokens before the migration, want 1: the migration must run on the connection that signed in with the token since rotated", got)
	}

	err = s.migrate(ctx, fstest.MapFS{
		"migrations/0001_widen_held.up.sql": {Data: []byte("alter table guard_held add column wider integer")},
	})
	if err == nil {
		t.Fatal("migrate applied a migration whose lock another transaction held")
	}
	t.Logf("migrate gave up: %v", err)
	holderPID := strconv.FormatUint(uint64(holder.PgConn().PID()), 10)
	for _, want := range []string{"AccessExclusiveLock", "guard_held", "pid " + holderPID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("migrate error = %q, want it to name %q: the lock watch did not sign in", err, want)
		}
	}
	if got := len(minter.minted()); got < 2 {
		t.Errorf("the minter minted %d tokens, want the lock watch to have minted its own", got)
	}
}
