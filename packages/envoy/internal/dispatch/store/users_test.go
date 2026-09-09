package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

func TestPgUserStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	login := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + randomDatabaseSuffix(t)
	t.Cleanup(func() {
		if _, err := store.Pool.Exec(ctx, "delete from users where login = $1", login); err != nil {
			t.Errorf("delete test user: %v", err)
		}
	})

	users := NewPgUserStore(store.Pool)
	want := &auth.User{
		Login: login,
		Tokens: auth.Tokens{
			AccessToken:      "access",
			RefreshToken:     "refresh",
			AccessExpiresAt:  time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC).UnixMilli(),
			RefreshExpiresAt: time.Date(2027, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli(),
			GithubLogin:      login,
		},
	}
	if err := users.Write(ctx, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := users.Read(ctx, want.Login)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatal("read returned nil")
	}
	if *got != *want {
		t.Errorf("round trip: got %+v, want %+v", got, want)
	}

	if err := users.Remove(ctx, want.Login); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got, err = users.Read(ctx, want.Login)
	if err != nil {
		t.Fatalf("read after remove: %v", err)
	}
	if got != nil {
		t.Errorf("read after remove: got %+v, want nil", got)
	}
}

func TestPgUserStoreHonorsContextCancellation(t *testing.T) {
	store := openTestStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	users := NewPgUserStore(store.Pool)
	user := &auth.User{Login: "canceled-context", Tokens: auth.Tokens{AccessToken: "access", RefreshToken: "refresh"}}

	if _, err := users.Read(ctx, user.Login); !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want context canceled", err)
	}
	if err := users.Write(ctx, user); !errors.Is(err, context.Canceled) {
		t.Fatalf("write error = %v, want context canceled", err)
	}
	if err := users.Remove(ctx, user.Login); !errors.Is(err, context.Canceled) {
		t.Fatalf("remove error = %v, want context canceled", err)
	}
}
