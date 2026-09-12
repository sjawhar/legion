package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
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
	// A closed KV handle must remain observable through Ping so /healthz can
	// report the unavailable dependency while NATS reconnects.
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

func TestSessionRegistry_RewatchRestartsStoppedWatcher(t *testing.T) {
	client := setupNATS(t)
	registry, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := registry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for initial cache: %v", err)
	}

	url := client.Conn.ConnectedUrl()
	client.Conn.Close()
	waitFor(t, 5*time.Second, func() bool { return registry.WatchFailed() })
	if err := registry.Ping(); err == nil {
		t.Fatal("stopped watcher should make the registry unhealthy")
	}

	replacement, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect replacement: %v", err)
	}
	defer replacement.Close()
	if err := registry.Rewatch(replacement); err != nil {
		t.Fatalf("rewatch registry: %v", err)
	}
	if err := registry.Ping(); err != nil {
		t.Fatalf("ping after rewatch: %v", err)
	}

	js, err := replacement.JetStream()
	if err != nil {
		t.Fatalf("open replacement JetStream: %v", err)
	}
	kv, err := js.KeyValue(SessionBucket)
	if err != nil {
		t.Fatalf("open replacement session bucket: %v", err)
	}
	entry := SessionEntry{Port: 13381, MachineID: "replacement", Dir: "/replacement"}
	value, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal replacement entry: %v", err)
	}
	if _, err := kv.Put("ses_after_rewatch", value); err != nil {
		t.Fatalf("put replacement entry: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		got, err := registry.Get("ses_after_rewatch")
		return err == nil && got == entry
	})
}

func TestSessionRegistryWatcherEvictsMalformedValue(t *testing.T) {
	logs := captureSessionRegistryLogs(t)
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("OpenSessionRegistry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const sessionID = "ses_malformed"
	if err := reg.Put(sessionID, SessionEntry{Port: 13381, MachineID: "example-host", Dir: "/example"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	revision, err := reg.kv.Put(sessionID, []byte("{"))
	if err != nil {
		t.Fatalf("put malformed value: %v", err)
	}
	waitForSessionEviction(t, reg, sessionID, 5*time.Second)

	for _, want := range []string{
		"level=WARN",
		"key=" + sessionID,
		"revision=" + strconv.FormatUint(revision, 10),
	} {
		waitFor(t, 5*time.Second, func() bool {
			return strings.Contains(logs.String(), want)
		})
	}
	t.Logf("session watcher malformed-value warning: %s", strings.TrimSpace(logs.String()))
}

func TestSessionRegistryPutDoesNotOverwriteNewerWatcherValue(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("OpenSessionRegistry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const sessionID = "ses_revision"
	raw := reg.kv
	reg.kv = &interleavingSessionPutKeyValue{
		KeyValue: raw,
		afterFirstPut: func() {
			remote := SessionEntry{Port: 13382, MachineID: "remote-host", Dir: "/remote", Driving: true}
			buf, err := json.Marshal(remote)
			if err != nil {
				t.Fatalf("marshal remote entry: %v", err)
			}
			if _, err := raw.Put(sessionID, buf); err != nil {
				t.Fatalf("put remote entry: %v", err)
			}
			waitFor(t, 5*time.Second, func() bool {
				got, err := reg.Get(sessionID)
				return err == nil && got.Port == remote.Port
			})
		},
	}
	if err := reg.Put(sessionID, SessionEntry{Port: 13381, MachineID: "local-host", Dir: "/local", Driving: true}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("Get after interleaving: %v", err)
	}
	if got.Port != 13382 || got.MachineID != "remote-host" {
		t.Fatalf("older write-through restored local entry: %+v", got)
	}
}

func TestSessionRegistryDeleteHistoryFailureSuppressesStaleWatcherUpdate(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("OpenSessionRegistry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const sessionID = "ses_delete"
	item := SessionEntry{Port: 13381, MachineID: "example-host", Dir: "/example"}
	if err := reg.Put(sessionID, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entry, err := reg.kv.Get(sessionID)
	if err != nil {
		t.Fatalf("Get seeded entry: %v", err)
	}
	reg.kv = &historyFailSessionKeyValue{
		KeyValue: reg.kv,
		err:      errors.New("injected history failure"),
	}
	if err := reg.Delete(sessionID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reg.mu.Lock()
	reg.cacheSessionLocked(sessionID, item, time.Time{}, entry.Revision())
	reg.mu.Unlock()
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("Get returned a session restored by a stale watcher update")
	}
}

type interleavingSessionPutKeyValue struct {
	natsgo.KeyValue

	puts          int
	afterFirstPut func()
}

func (kv *interleavingSessionPutKeyValue) Put(key string, value []byte) (uint64, error) {
	kv.puts++
	revision, err := kv.KeyValue.Put(key, value)
	if err != nil {
		return 0, err
	}
	if kv.puts == 1 {
		kv.afterFirstPut()
	}
	return revision, nil
}

type historyFailSessionKeyValue struct {
	natsgo.KeyValue

	err error
}

func (kv *historyFailSessionKeyValue) History(string, ...natsgo.WatchOpt) ([]natsgo.KeyValueEntry, error) {
	return nil, kv.err
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for observable state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForSessionEviction(t *testing.T, reg *SessionRegistry, sessionID string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, func() bool {
		_, getErr := reg.Get(sessionID)
		entries, listErr := reg.List()
		return getErr != nil && listErr == nil && len(entries) == 0
	})
}

func captureSessionRegistryLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
