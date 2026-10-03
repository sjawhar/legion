package store

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// The controller record's whole life: a mint issues generation 1 with nothing registered; a
// registration at that generation records the session; a second mint moves to generation 2 and
// clears it; and a registration still carrying generation 1 — the capability the second mint
// replaced — writes nothing.
func TestControllerMintRegisterAndRotate(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)

	if _, found, err := store.Controller(ctx, "demo"); err != nil || found {
		t.Fatalf("Controller before any mint = %v, %v, want no record", found, err)
	}

	first, err := store.MintController(ctx, "demo", []byte("capability-1"))
	if err != nil || first != 1 {
		t.Fatalf("first mint = %d, %v, want generation 1", first, err)
	}
	record, found, err := store.Controller(ctx, "demo")
	if err != nil || !found {
		t.Fatalf("Controller after the mint = %v, %v", found, err)
	}
	if record.Generation != 1 || !bytes.Equal(record.CapabilityHash, []byte("capability-1")) || record.Registered() {
		t.Fatalf("record after the mint = %+v, want generation 1, its hash, and no session", record)
	}

	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	registered, err := store.RegisterController(ctx, "demo", 1, "ses_one", []byte("secret-1"), at)
	if err != nil || !registered {
		t.Fatalf("register at generation 1 = %v, %v, want it written", registered, err)
	}
	record, _, err = store.Controller(ctx, "demo")
	if err != nil {
		t.Fatalf("Controller after the registration: %v", err)
	}
	if record.Session != "ses_one" || !bytes.Equal(record.SecretHash, []byte("secret-1")) || !record.RegisteredAt.Equal(at) {
		t.Fatalf("record after the registration = %+v, want ses_one, its secret hash, and %s", record, at)
	}

	second, err := store.MintController(ctx, "demo", []byte("capability-2"))
	if err != nil || second != 2 {
		t.Fatalf("second mint = %d, %v, want generation 2", second, err)
	}
	record, _, err = store.Controller(ctx, "demo")
	if err != nil {
		t.Fatalf("Controller after the second mint: %v", err)
	}
	if record.Generation != 2 || !bytes.Equal(record.CapabilityHash, []byte("capability-2")) || record.Registered() ||
		record.SecretHash != nil || !record.RegisteredAt.IsZero() {
		t.Fatalf("record after the second mint = %+v, want generation 2 and the registration cleared", record)
	}

	registered, err = store.RegisterController(ctx, "demo", 1, "ses_stale", []byte("secret-stale"), at)
	if err != nil || registered {
		t.Fatalf("register at the replaced generation 1 = %v, %v, want nothing written", registered, err)
	}
	if record, _, _ := store.Controller(ctx, "demo"); record.Registered() {
		t.Fatalf("record after a stale registration = %+v, want no session", record)
	}
}

// Each project has a controller of its own: two daemons sharing a database never read or rotate
// the other's.
func TestControllerIsPerProject(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	if _, err := store.MintController(ctx, "alpha", []byte("alpha-capability")); err != nil {
		t.Fatalf("mint alpha: %v", err)
	}
	if generation, err := store.MintController(ctx, "beta", []byte("beta-capability")); err != nil || generation != 1 {
		t.Fatalf("mint beta = %d, %v, want its own generation 1", generation, err)
	}
	alpha, _, err := store.Controller(ctx, "alpha")
	if err != nil || alpha.Generation != 1 || !bytes.Equal(alpha.CapabilityHash, []byte("alpha-capability")) {
		t.Fatalf("alpha after beta's mint = %+v, %v, want alpha's own record", alpha, err)
	}
}
