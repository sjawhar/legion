package store

import (
	"context"
	"testing"
)

func TestPgSessionStoreAdvancesGenerationOnRevocation(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sessions := NewPgSessionStore(database.Pool)
	login := "session-" + randomDatabaseSuffix(t)

	generation, err := sessions.EnsureSession(ctx, login)
	if err != nil {
		t.Fatalf("ensure initial generation: %v", err)
	}
	if generation != 0 {
		t.Fatalf("initial generation = %d, want 0", generation)
	}
	if err := sessions.RevokeSessions(ctx, login); err != nil {
		t.Fatalf("revoke sessions: %v", err)
	}
	generation, found, err := sessions.CurrentSessionGeneration(ctx, login)
	if err != nil {
		t.Fatalf("read revoked generation: %v", err)
	}
	if !found || generation != 1 {
		t.Fatalf("revoked generation = %d, found=%t, want 1, true", generation, found)
	}
}
