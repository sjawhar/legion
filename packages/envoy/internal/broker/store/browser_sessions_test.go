package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// A session is found by its secret's hash, with the login and expiry it was made with, until its
// logout deletes it; a lookup by the hash of another secret finds nothing.
func TestBrowserSessionIsFoundByItsHashUntilItsLogout(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := time.Now()
	hash := sha256.Sum256([]byte("session secret " + t.Name()))
	expires := now.Add(time.Hour).Truncate(time.Microsecond)
	if err := st.CreateBrowserSession(ctx, hash, "ada@example.com", expires); err != nil {
		t.Fatalf("CreateBrowserSession: %v", err)
	}

	login, gotExpires, err := st.BrowserSession(ctx, hash, now)
	if err != nil || login != "ada@example.com" || !gotExpires.Equal(expires) {
		t.Fatalf("BrowserSession = %q, %v, %v; want ada@example.com, %v", login, gotExpires, err, expires)
	}
	if _, _, err := st.BrowserSession(ctx, sha256.Sum256([]byte("another secret")), now); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("BrowserSession(another secret's hash) error = %v, want pgx.ErrNoRows", err)
	}

	if err := st.DeleteBrowserSession(ctx, hash); err != nil {
		t.Fatalf("DeleteBrowserSession: %v", err)
	}
	if _, _, err := st.BrowserSession(ctx, hash, now); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("BrowserSession after the logout: error = %v, want pgx.ErrNoRows", err)
	}
	if err := st.DeleteBrowserSession(ctx, hash); err != nil {
		t.Fatalf("DeleteBrowserSession of a session already gone: %v", err)
	}
}

// A session whose expiry has come is absent from the lookup though its row is still there, at the
// instant of its expiry as well as after it.
func TestBrowserSessionPastItsExpiryIsAbsent(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte("session secret " + t.Name()))
	expires := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	if err := st.CreateBrowserSession(ctx, hash, "ada@example.com", expires); err != nil {
		t.Fatalf("CreateBrowserSession: %v", err)
	}
	for _, at := range []time.Time{expires, expires.Add(time.Second)} {
		if login, _, err := st.BrowserSession(ctx, hash, at); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("BrowserSession at %v, its expiry %v = %q, %v; want pgx.ErrNoRows", at, expires, login, err)
		}
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `select count(*) from browser_sessions`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("browser_sessions rows = %d (%v), want the expired one still there", rows, err)
	}
}

// The sweep deletes every session whose expiry has come, and answers how many, and leaves a live
// one signed in.
func TestDeleteExpiredBrowserSessionsDeletesOnlyTheExpired(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	sessions := []struct {
		secret  string
		expires time.Time
	}{
		{"long gone", now.Add(-time.Hour)},
		{"expiring now", now},
		{"live", now.Add(time.Hour)},
	}
	for _, s := range sessions {
		if err := st.CreateBrowserSession(ctx, sha256.Sum256([]byte(s.secret)), "ada@example.com", s.expires); err != nil {
			t.Fatalf("CreateBrowserSession(%s): %v", s.secret, err)
		}
	}

	deleted, err := st.DeleteExpiredBrowserSessions(ctx, now)
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteExpiredBrowserSessions = %d, %v; want 2", deleted, err)
	}
	if login, _, err := st.BrowserSession(ctx, sha256.Sum256([]byte("live")), now); err != nil || login != "ada@example.com" {
		t.Fatalf("the live session after the sweep = %q, %v; want ada@example.com", login, err)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `select count(*) from browser_sessions`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("browser_sessions rows after the sweep = %d (%v), want only the live one", rows, err)
	}
	if deleted, err := st.DeleteExpiredBrowserSessions(ctx, now); err != nil || deleted != 0 {
		t.Fatalf("a second sweep = %d, %v; want 0", deleted, err)
	}
}
