package store

import (
	"context"
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
	if _, err := store.Pool.Exec(ctx, "truncate users"); err != nil {
		t.Fatalf("truncate users: %v", err)
	}

	users := NewPgUserStore(store.Pool)
	want := &auth.User{
		Login: "sjawhar",
		Tokens: auth.Tokens{
			AccessToken:      "access",
			RefreshToken:     "refresh",
			AccessExpiresAt:  time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC).UnixMilli(),
			RefreshExpiresAt: time.Date(2027, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli(),
			GithubLogin:      "sjawhar",
		},
	}
	if err := users.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := users.Read(want.Login)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatal("read returned nil")
	}
	if *got != *want {
		t.Errorf("round trip: got %+v, want %+v", got, want)
	}

	if err := users.Remove(want.Login); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got, err = users.Read(want.Login)
	if err != nil {
		t.Fatalf("read after remove: %v", err)
	}
	if got != nil {
		t.Errorf("read after remove: got %+v, want nil", got)
	}
}
