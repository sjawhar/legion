package store

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
)

// The kept launcher credential's whole life: nothing kept at first; a save is read back byte for
// byte, its expiry to the microsecond; a renewal's save beside a next key replaces it; another
// project, or the same project pointed at another broker, keeps nothing; forgetting another
// credential's id drops nothing, and forgetting this one's drops it.
func TestAgentSecretsCredentialSaveLoadForget(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	const broker = "https://broker.internal.example"

	if _, found, err := store.AgentSecretsCredential(ctx, "demo", broker); err != nil || found {
		t.Fatalf("before any save = %v, %v, want nothing kept", found, err)
	}

	expires := time.Date(2026, 10, 18, 2, 0, 0, 123456000, time.UTC)
	first := agentsecrets.Kept{CredentialID: "cred-1", PrivateKey: []byte("key-1"), ExpiresAt: expires}
	if err := store.SaveAgentSecretsCredential(ctx, "demo", broker, first); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, found, err := store.AgentSecretsCredential(ctx, "demo", broker)
	if err != nil || !found || got.CredentialID != "cred-1" || !bytes.Equal(got.PrivateKey, []byte("key-1")) || got.NextPrivateKey != nil || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("after the save = %+v, %v, %v; want %+v", got, found, err, first)
	}

	renewing := agentsecrets.Kept{CredentialID: "cred-1", PrivateKey: []byte("key-1"), NextPrivateKey: []byte("key-2"), ExpiresAt: expires}
	if err := store.SaveAgentSecretsCredential(ctx, "demo", broker, renewing); err != nil {
		t.Fatalf("save beside a next key: %v", err)
	}
	if got, _, _ := store.AgentSecretsCredential(ctx, "demo", broker); !bytes.Equal(got.NextPrivateKey, []byte("key-2")) {
		t.Fatalf("after the save beside a next key = %+v, want next key key-2", got)
	}

	for _, other := range []struct{ project, broker string }{{"other", broker}, {"demo", "https://other-broker.internal.example"}} {
		if _, found, err := store.AgentSecretsCredential(ctx, other.project, other.broker); err != nil || found {
			t.Fatalf("project %s at %s = %v, %v, want nothing kept", other.project, other.broker, found, err)
		}
	}

	if err := store.ForgetAgentSecretsCredential(ctx, "demo", broker, "cred-older"); err != nil {
		t.Fatalf("forget another credential: %v", err)
	}
	if _, found, _ := store.AgentSecretsCredential(ctx, "demo", broker); !found {
		t.Fatal("forgetting another credential's id dropped the kept one")
	}
	if err := store.ForgetAgentSecretsCredential(ctx, "demo", broker, "cred-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, found, err := store.AgentSecretsCredential(ctx, "demo", broker); err != nil || found {
		t.Fatalf("after forgetting it = %v, %v, want nothing kept", found, err)
	}

	unknown := agentsecrets.Kept{CredentialID: "cred-3", PrivateKey: []byte("key-3")}
	if err := store.SaveAgentSecretsCredential(ctx, "demo", broker, unknown); err != nil {
		t.Fatalf("save with no expiry: %v", err)
	}
	if got, _, _ := store.AgentSecretsCredential(ctx, "demo", broker); !got.ExpiresAt.IsZero() {
		t.Fatalf("a credential saved with no expiry reads back expiring at %s, want the zero time", got.ExpiresAt)
	}
}

// The renewal winner replaces the kept key only while that row still holds the key it renewed
// from. A second daemon carrying the same old key may first write its candidate next key, then
// lose the broker renewal and clear that candidate; both writes compare against the old key, so
// neither can overwrite the winner.
func TestCompareAndSaveAgentSecretsCredentialKeepsTheWinner(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	const (
		project = "demo"
		broker  = "https://broker.internal.example"
	)
	old := agentsecrets.Kept{CredentialID: "cred-1", PrivateKey: []byte("old-key")}
	if err := store.SaveAgentSecretsCredential(ctx, project, broker, old); err != nil {
		t.Fatal(err)
	}
	loserPreflight := agentsecrets.Kept{CredentialID: "cred-1", PrivateKey: old.PrivateKey, NextPrivateKey: []byte("loser-next")}
	saved, err := store.CompareAndSaveAgentSecretsCredential(ctx, project, broker, old.PrivateKey, loserPreflight)
	if err != nil || !saved {
		t.Fatalf("loser preflight compare-and-save = %v, %v; want saved before the broker race", saved, err)
	}
	winner := agentsecrets.Kept{CredentialID: "cred-1", PrivateKey: []byte("winner-key")}
	saved, err = store.CompareAndSaveAgentSecretsCredential(ctx, project, broker, old.PrivateKey, winner)
	if err != nil || !saved {
		t.Fatalf("winner compare-and-save = %v, %v; want saved", saved, err)
	}
	loserCleanup := agentsecrets.Kept{CredentialID: "cred-1", PrivateKey: old.PrivateKey}
	saved, err = store.CompareAndSaveAgentSecretsCredential(ctx, project, broker, old.PrivateKey, loserCleanup)
	if err != nil || saved {
		t.Fatalf("loser cleanup compare-and-save = %v, %v; want not saved", saved, err)
	}
	got, found, err := store.AgentSecretsCredential(ctx, project, broker)
	if err != nil || !found || !bytes.Equal(got.PrivateKey, winner.PrivateKey) || got.NextPrivateKey != nil {
		t.Fatalf("kept credential after the race = %+v, %v, %v; want the winner with no next key", got, found, err)
	}
}
