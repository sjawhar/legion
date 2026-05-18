package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

var (
	sharedNATSOnce sync.Once
	sharedNATSURI  string
	sharedNATSErr  error
)

func sharedTestNATSURI(t *testing.T) string {
	t.Helper()
	sharedNATSOnce.Do(func() {
		ctx := context.Background()
		ctr, err := tcnats.Run(ctx, "nats:2.10")
		if err != nil {
			sharedNATSErr = err
			return
		}
		sharedNATSURI, sharedNATSErr = ctr.ConnectionString(ctx)
	})
	if sharedNATSErr != nil {
		t.Fatalf("failed to start shared NATS: %v", sharedNATSErr)
	}
	return sharedNATSURI
}

func clearSessionBucket(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("failed to open JetStream: %v", err)
	}
	// Delete the entire bucket so OpenSessionRegistry can recreate it with the
	// correct TTL. Merely clearing keys preserves the original bucket config,
	// which causes TTL-sensitive tests to inherit the wrong TTL.
	if err := js.DeleteKeyValue(SessionBucket); err != nil && !errors.Is(err, natsgo.ErrBucketNotFound) && !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("failed to delete session bucket: %v", err)
	}
}

func setupNATS(t *testing.T) *bus.Client {
	t.Helper()
	client, err := bus.Connect([]string{sharedTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(func() { client.Conn.Close() })
	clearSessionBucket(t, client.Conn)
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

func TestSessionRegistry_NilPutReturnsErrNoKV(t *testing.T) {
	var reg *SessionRegistry
	err := reg.Put("ses_test", SessionEntry{Port: 1234})
	if err != ErrNoKV {
		t.Fatalf("expected ErrNoKV, got: %v", err)
	}
}

func TestSessionRegistry_NilGetReturnsErrNoKV(t *testing.T) {
	var reg *SessionRegistry
	_, err := reg.Get("ses_test")
	if err != ErrNoKV {
		t.Fatalf("expected ErrNoKV, got: %v", err)
	}
}

func TestSessionRegistry_NilDeleteReturnsErrNoKV(t *testing.T) {
	var reg *SessionRegistry
	err := reg.Delete("ses_test")
	if err != ErrNoKV {
		t.Fatalf("expected ErrNoKV, got: %v", err)
	}
}


func TestSessionRegistry_Ping_Healthy(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open: %v", err)
	}
	if err := reg.Ping(); err != nil {
		t.Fatalf("Ping on healthy conn returned error: %v", err)
	}
}

func TestSessionRegistry_Ping_ClosedConnReturnsError(t *testing.T) {
	// Regression for the sami listener stuck-after-recovery scenario — the
	// session registry's KV handle is bound to the original *nats.Conn. After
	// bus recovery replaces the conn, this Ping must surface the broken state
	// so the listener can self-terminate.
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open: %v", err)
	}
	if err := reg.Ping(); err != nil {
		t.Fatalf("sanity: Ping before close: %v", err)
	}

	client.Conn.Close()

	if err := reg.Ping(); err == nil {
		t.Fatal("Ping after conn close should return error")
	}
}

func TestSessionRegistry_Ping_NilReceiver(t *testing.T) {
	var reg *SessionRegistry
	if err := reg.Ping(); err != ErrNoKV {
		t.Fatalf("expected ErrNoKV, got %v", err)
	}
}