package session

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

func setupNATS(t *testing.T) *bus.Client {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("failed to start NATS: %v", err)
	}
	t.Cleanup(func() { ctr.Terminate(ctx) })
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get NATS URI: %v", err)
	}
	client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { client.Conn.Close() })
	return client
}

func TestSessionRegistry_PutAndGet(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	if err := reg.Put("ses_test", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	got, err := reg.Get("ses_test")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Port != 13381 {
		t.Fatalf("expected port 13381, got %d", got.Port)
	}
	if got.MachineID != "test-machine" {
		t.Fatalf("expected machine_id test-machine, got %s", got.MachineID)
	}
}

func TestSessionRegistry_GetNotFound(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	_, err = reg.Get("ses_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestSessionRegistry_TTLExpiry(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(2*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	if err := reg.Put("ses_expiry", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	if _, err := reg.Get("ses_expiry"); err != nil {
		t.Fatalf("expected entry to exist immediately: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if _, err := reg.Get("ses_expiry"); err != nil {
			return
		}
	}
	t.Fatal("session entry did not expire within 5s (TTL was 2s)")
}

func TestSessionRegistry_PutRefreshesTTL(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(2*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	if err := reg.Put("ses_refresh", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)
	if err := reg.Put("ses_refresh", entry); err != nil {
		t.Fatalf("refresh put failed: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)

	if _, err := reg.Get("ses_refresh"); err != nil {
		t.Fatalf("expected entry to still exist after TTL refresh, got: %v", err)
	}
}

func TestSessionRegistry_Delete(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test"}
	reg.Put("ses_del", entry)

	if err := reg.Delete("ses_del"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	_, err = reg.Get("ses_del")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}
