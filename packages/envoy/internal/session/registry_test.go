package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// setupNATS connects t to the package's shared NATS server, in the JetStream account testnats.URL
// hands t, where no earlier test made a session bucket.
func setupNATS(t *testing.T) *bus.Client {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("failed to connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestSessionEntry_HostOrMachine(t *testing.T) {
	reported := SessionEntry{MachineID: "example-listener", Host: "example-host-b"}
	if got := reported.HostOrMachine(); got != "example-host-b" {
		t.Fatalf("HostOrMachine() = %q, want the reported host", got)
	}

	legacy := SessionEntry{MachineID: "example-listener"}
	if got := legacy.HostOrMachine(); got != "example-listener" {
		t.Fatalf("HostOrMachine() = %q, want the fallback to MachineID for a session with no reported host", got)
	}
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

// The stored entry keeps the plugin a session registered through and that plugin's build, so the
// session list read back from the bucket names them.
func TestSessionRegistry_PutAndGetKeepsThePluginAndItsVersion(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{MachineID: "test-machine", Plugin: "@sjawhar/claude-envoy", PluginVersion: "afe10b2f3e1b"}
	if err := reg.Put("ses_plugin", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	got, err := reg.Get("ses_plugin")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Plugin != "@sjawhar/claude-envoy" || got.PluginVersion != "afe10b2f3e1b" {
		t.Fatalf("got %+v, want plugin @sjawhar/claude-envoy at afe10b2f3e1b", got)
	}
}

// The stored entry keeps the client instance that registered the session, so a soft claim naming
// a predecessor can read whose instance that predecessor belongs to.
func TestSessionRegistry_PutAndGetKeepsTheClientInstanceID(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	entry := SessionEntry{Port: 13381, MachineID: "test-machine", ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"}
	if err := reg.Put("ses_instance", entry); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	got, err := reg.Get("ses_instance")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.ClientInstanceID != "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a" {
		t.Fatalf("got %+v, want the registered client instance id", got)
	}
}

// A re-registration carrying no client instance id clears the one stored: the handler builds the
// entry fresh from each subscribe body, so a client that predates the field (or one that stopped
// sending it) leaves the session with the empty id that keeps the old soft-claim rule, rather
// than with another instance's id it no longer belongs to.
func TestSessionRegistry_PutWithoutClientInstanceIDClearsTheStoredOne(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	registered := SessionEntry{Port: 13381, MachineID: "test-machine", ClientInstanceID: "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"}
	if err := reg.Put("ses_instance_cleared", registered); err != nil {
		t.Fatalf("first put failed: %v", err)
	}

	// The same process heartbeating again (same port, so no claim arbitration intervenes),
	// this time with no id in its payload.
	if err := reg.Put("ses_instance_cleared", SessionEntry{Port: 13381, MachineID: "test-machine"}); err != nil {
		t.Fatalf("second put failed: %v", err)
	}

	got, err := reg.Get("ses_instance_cleared")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.ClientInstanceID != "" {
		t.Fatalf("got %+v, want the stored client instance id cleared by a registration without one", got)
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

	if err := reg.Delete(context.Background(), "ses_del"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	_, err = reg.Get("ses_del")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

// A deregistration that names the instance that registered the session deletes it, as the
// unconditional Delete does: the /clear handoff this guards is a client deregistering its own
// session before it claims that session's role.
func TestSessionRegistryDeleteIfInstance_DeletesTheSameInstancesEntry(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_same"
	putForInstanceDelete(t, reg, sessionID, "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a")

	outcome, err := reg.DeleteIfInstance(context.Background(), sessionID, "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a")
	if err != nil {
		t.Fatalf("DeleteIfInstance: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIfInstance = %s, want %s for an entry its own client instance deregistered", outcome, EntryDeleted)
	}
	if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("read the deleted key: %v, want nats.ErrKeyNotFound", err)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("the cache still answers with the deleted entry")
	}
}

// The hole this closes: another process resumed the session id and registered under its own
// instance, so the earlier instance's deregistration must leave that live entry alone — and still
// answer, since there is nothing for it to retry.
func TestSessionRegistryDeleteIfInstance_KeepsAnotherInstancesEntry(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_other"
	putForInstanceDelete(t, reg, sessionID, "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2")

	outcome, err := reg.DeleteIfInstance(context.Background(), sessionID, "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a")
	if err != nil {
		t.Fatalf("DeleteIfInstance: %v", err)
	}
	if outcome != EntryKeptForAnotherInstance {
		t.Fatalf("DeleteIfInstance = %s, want %s for another client instance's live entry", outcome, EntryKeptForAnotherInstance)
	}
	stored, err := reg.Get(sessionID)
	if err != nil {
		t.Fatalf("Get the kept entry: %v", err)
	}
	if stored.ClientInstanceID != "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2" {
		t.Fatalf("got %+v, want the other client instance's entry kept as it was", stored)
	}
}

// An entry a client that predates the field registered carries no instance id, and a client that
// names one still deregisters it: that client is the only one that can be deregistering it.
func TestSessionRegistryDeleteIfInstance_DeletesAnEntryThatNamesNoInstance(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_absent_stored"
	putForInstanceDelete(t, reg, sessionID, "")

	outcome, err := reg.DeleteIfInstance(context.Background(), sessionID, "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a")
	if err != nil {
		t.Fatalf("DeleteIfInstance: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIfInstance = %s, want %s for an entry that carries no client instance id", outcome, EntryDeleted)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("the cache still answers with the deleted entry")
	}
}

// A caller that names no instance id compares nothing, so it deletes whatever is stored, as the
// route does for a client that sends no id of its own.
func TestSessionRegistryDeleteIfInstance_WithNoInstanceIDDeletesWhateverIsStored(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_absent_requested"
	putForInstanceDelete(t, reg, sessionID, "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2")

	outcome, err := reg.DeleteIfInstance(context.Background(), sessionID, "")
	if err != nil {
		t.Fatalf("DeleteIfInstance: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIfInstance = %s, want %s for a request naming no client instance id", outcome, EntryDeleted)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("the cache still answers with the deleted entry")
	}
}

// A key that is not there is already gone, which is what the caller asked for: deleted, no error,
// as Delete is idempotent today.
func TestSessionRegistryDeleteIfInstance_AbsentKeyAnswersDeleted(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)

	outcome, err := reg.DeleteIfInstance(context.Background(), "ses_instance_missing", "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a")
	if err != nil {
		t.Fatalf("DeleteIfInstance: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIfInstance = %s, want %s for an absent key", outcome, EntryDeleted)
	}
}

// The deleting instance's own heartbeat lands between the read and the delete, so JetStream
// refuses the delete with wrong last sequence: the sequence runs once more and the entry goes,
// where answering "kept" would leave the predecessor alive and make the claim that follows a 409.
func TestSessionRegistryDeleteIfInstance_RetriesOnceAfterALostCompareAndSwap(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const (
		sessionID  = "ses_instance_race"
		instanceID = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	putForInstanceDelete(t, reg, sessionID, instanceID)
	heartbeat, err := json.Marshal(SessionEntry{
		Port:             13381,
		MachineID:        "test-machine",
		ClientInstanceID: instanceID,
		UpdatedAt:        time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("encode the interleaved heartbeat: %v", err)
	}
	race := &interleavingSessionGetKeyValue{KeyValue: reg.watcher.KV().Legacy(), key: sessionID, value: heartbeat, interleaves: 1}
	useKV(t, reg, race)

	outcome, err := reg.DeleteIfInstance(context.Background(), sessionID, instanceID)
	if err != nil {
		t.Fatalf("DeleteIfInstance: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIfInstance = %s, want %s where its own heartbeat only lost it one compare-and-swap", outcome, EntryDeleted)
	}
	if got := race.reads(); got != 2 {
		t.Fatalf("read the entry %d times, want the one retry (2)", got)
	}
	if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("read the deleted key: %v, want nats.ErrKeyNotFound", err)
	}
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("the cache still answers with the deleted entry")
	}
}

// A value this build cannot read names no instance even when its raw JSON carries one:
// json.Unmarshal fills the fields it decoded before the type error, so the id another client
// instance wrote would keep the entry if the half-decoded value were trusted. The branch drops
// it, and the deregistration is not blocked by a record no reader can act on.
func TestSessionRegistryDeleteIfInstance_DeletesAMalformedValueThatNamesAnotherInstance(t *testing.T) {
	logs := captureSessionRegistryLogs(t)
	reg := openRegistryForInstanceDelete(t)
	const sessionID = "ses_instance_malformed"
	if _, err := reg.watcher.KV().Put(sessionID, []byte(`{"client_instance_id":"b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2","port":"not a port"}`)); err != nil {
		t.Fatalf("put the malformed value: %v", err)
	}

	outcome, err := reg.DeleteIfInstance(context.Background(), sessionID, "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a")
	if err != nil {
		t.Fatalf("DeleteIfInstance: %v", err)
	}
	if outcome != EntryDeleted {
		t.Fatalf("DeleteIfInstance = %s, want %s for a value this build cannot read", outcome, EntryDeleted)
	}
	if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("read the deleted key: %v, want nats.ErrKeyNotFound", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(logs.String(), "session registry deleting a malformed value for a named client instance")
	})
}

// Both conditional deletes lose to a write that lands under them, and the answer says only that:
// the entry stayed. It is not the mismatch's outcome, because the write that won the race can be
// this very client instance's heartbeat, and the route's reason for a mismatch would then be
// false.
func TestSessionRegistryDeleteIfInstance_KeepsTheEntryAfterTwoLostCompareAndSwaps(t *testing.T) {
	reg := openRegistryForInstanceDelete(t)
	const (
		sessionID  = "ses_instance_race_lost"
		instanceID = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	putForInstanceDelete(t, reg, sessionID, instanceID)
	heartbeat, err := json.Marshal(SessionEntry{
		Port:             13381,
		MachineID:        "test-machine",
		ClientInstanceID: instanceID,
		UpdatedAt:        time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("encode the interleaved heartbeat: %v", err)
	}
	race := &interleavingSessionGetKeyValue{KeyValue: reg.watcher.KV().Legacy(), key: sessionID, value: heartbeat, interleaves: 2}
	useKV(t, reg, race)

	outcome, err := reg.DeleteIfInstance(context.Background(), sessionID, instanceID)
	if err != nil {
		t.Fatalf("DeleteIfInstance: %v", err)
	}
	if outcome != EntryKeptAfterLostRace {
		t.Fatalf("DeleteIfInstance = %s, want %s after both compare-and-swaps lost", outcome, EntryKeptAfterLostRace)
	}
	if got := race.reads(); got != 2 {
		t.Fatalf("read the entry %d times, want the one retry (2)", got)
	}
	if _, err := reg.watcher.KV().Get(sessionID); err != nil {
		t.Fatalf("read the entry that stayed: %v, want it still stored", err)
	}
}

// openRegistryForInstanceDelete opens a registry whose cache has caught up with its bucket, so a
// DeleteIfInstance test reads what it seeded and nothing else.
func openRegistryForInstanceDelete(t *testing.T) *SessionRegistry {
	t.Helper()
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
	return reg
}

// putForInstanceDelete registers sessionID for instanceID, which is empty for a client that
// predates the field.
func putForInstanceDelete(t *testing.T, reg *SessionRegistry, sessionID, instanceID string) {
	t.Helper()
	entry := SessionEntry{Port: 13381, MachineID: "test-machine", Dir: "/test", ClientInstanceID: instanceID}
	if err := reg.Put(sessionID, entry); err != nil {
		t.Fatalf("Put %s: %v", sessionID, err)
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

	client.Close()

	if err := reg.Ping(); err == nil {
		t.Fatal("Ping after conn close should return error")
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
	client.Close()
	waitFor(t, 5*time.Second, func() bool { return registry.WatchErr() != nil })
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
	kv, err := js.KeyValue(bus.SessionBucket)
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
		return err == nil && reflect.DeepEqual(got, entry)
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
	revision, err := reg.watcher.KV().Put(sessionID, []byte("{"))
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
	raw := reg.watcher.KV()
	useKV(t, reg, &interleavingSessionPutKeyValue{
		KeyValue: raw.Legacy(),
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
	})
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
	entry, err := reg.watcher.KV().Get(sessionID)
	if err != nil {
		t.Fatalf("Get seeded entry: %v", err)
	}
	useKV(t, reg, &historyFailSessionKeyValue{
		KeyValue: reg.watcher.KV().Legacy(),
		err:      errors.New("injected history failure"),
	})
	if err := reg.Delete(context.Background(), sessionID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	sessionCache{reg}.Apply(kvwatch.LegacyEntry(entry), kvwatch.Read{})
	if _, err := reg.Get(sessionID); err == nil {
		t.Fatal("Get returned a session restored by a stale watcher update")
	}
}

// useKV rebuilds the registry's watcher over kv, a wrapper of its bucket handle that watches
// through the handle's own jetstream one, so the registry writes through kv from here on. The
// replaced watcher is stopped first.
func useKV(t *testing.T, r *SessionRegistry, kv natsgo.KeyValue) {
	t.Helper()
	base := r.watcher.KV()
	r.watcher.Stop()
	r.watcher = kvwatch.New("session registry", bus.NewKeyValue(kv, base.WatchHandle(), base.Streams()), sessionCache{r})
	r.watcher.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the rebuilt watcher: %v", err)
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

// interleavingSessionGetKeyValue writes value under key after each of the first interleaves reads
// of key, so whatever that reader does next loses its compare-and-swap: the revision it read is
// no longer the last one by the time it deletes. One interleaved write is the heartbeat
// DeleteIfInstance retries past; two leave it no attempt that can land. Reads of any other key,
// and reads that did not answer, pass straight through and are not counted.
type interleavingSessionGetKeyValue struct {
	natsgo.KeyValue

	key         string
	value       []byte
	interleaves int

	mu   sync.Mutex
	gets int
}

func (kv *interleavingSessionGetKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	if key != kv.key || err != nil {
		return entry, err
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.gets++
	if kv.gets > kv.interleaves {
		return entry, nil
	}
	if _, err := kv.KeyValue.Put(key, kv.value); err != nil {
		return nil, err
	}
	return entry, nil
}

// reads is how often the key was read through this bucket, one per DeleteIfInstance attempt.
func (kv *interleavingSessionGetKeyValue) reads() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.gets
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
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
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

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }
