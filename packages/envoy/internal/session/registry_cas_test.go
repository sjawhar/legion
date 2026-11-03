package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// podRegistries opens the session registry twice on one bucket, each over its own NATS connection,
// as two listener pods of one machine do, and returns both once their caches are warm.
func podRegistries(t *testing.T) (a, b *SessionRegistry) {
	t.Helper()
	open := func(name string) *SessionRegistry {
		t.Helper()
		client := setupNATS(t)
		reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
		if err != nil {
			t.Fatalf("open pod %s's session registry: %v", name, err)
		}
		t.Cleanup(reg.StopWatch)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := reg.WaitForCacheReady(ctx); err != nil {
			t.Fatalf("warm pod %s's session cache: %v", name, err)
		}
		return reg
	}
	return open("a"), open("b")
}

// storedSession reads sessionID's registration from the session bucket itself, as raw JSON keys.
func storedSession(t *testing.T, reg *SessionRegistry, sessionID string) map[string]json.RawMessage {
	t.Helper()
	entry, err := reg.watcher.KV().Get(sessionID)
	if err != nil {
		t.Fatalf("read %s from the session bucket: %v", sessionID, err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &stored); err != nil {
		t.Fatalf("decode %s's stored registration %s: %v", sessionID, entry.Value(), err)
	}
	return stored
}

// A heartbeat on a pod whose cache trails the bucket must not write onto its stale copy: pod B read
// the session, a pod of a newer build then stored a field this build does not know, and B's next
// heartbeat, merged onto B's stale copy, would drop it. The write is a compare-and-set on the
// revision the bucket holds, so it merges onto what that pod wrote instead.
func TestAStoredFieldThisBuildDoesNotKnowSurvivesAHeartbeatOnAPodWhoseCacheTrails(t *testing.T) {
	a, b := podRegistries(t)
	const sessionID = "ses_contended"
	if err := b.Put(sessionID, SessionEntry{MachineID: "box-a", Dir: "/work"}); err != nil {
		t.Fatalf("the session registers through pod B: %v", err)
	}
	b.StopWatch()
	if _, err := a.watcher.KV().Put(sessionID, []byte(`{"machine_id":"box-a","dir":"/work","future_field":{"kept":true}}`)); err != nil {
		t.Fatalf("a newer build's pod stores the registration: %v", err)
	}
	if err := b.Put(sessionID, SessionEntry{MachineID: "box-a", Dir: "/work", Title: "planner"}); err != nil {
		t.Fatalf("heartbeat through pod B: %v", err)
	}
	stored := storedSession(t, a, sessionID)
	if got := string(stored["future_field"]); got != `{"kept":true}` {
		t.Fatalf("future_field after pod B's stale heartbeat = %q, want {\"kept\":true}; stored %v", got, stored)
	}
	if got := string(stored["title"]); got != `"planner"` {
		t.Fatalf("title after pod B's stale heartbeat = %s, want the heartbeat's", got)
	}
}

// conflictingSessionKV is a session bucket another writer races on every write: before each Create
// or Update it writes the key itself, so the write's expected revision is always stale.
type conflictingSessionKV struct {
	natsgo.KeyValue
	writes int
}

func (kv *conflictingSessionKV) race(key string) {
	_, _ = kv.KeyValue.Put(key, []byte(`{"port":7,"machine_id":"box-b"}`))
}

func (kv *conflictingSessionKV) Create(key string, value []byte) (uint64, error) {
	kv.writes++
	kv.race(key)
	return kv.KeyValue.Create(key, value)
}

func (kv *conflictingSessionKV) Update(key string, value []byte, last uint64) (uint64, error) {
	kv.writes++
	kv.race(key)
	return kv.KeyValue.Update(key, value, last)
}

// A registration write that finds the stored revision moved under it every time gives up after five
// attempts with ErrWriteConflict, which the listener answers as a 503 to retry, rather than looping
// or overwriting the other writer.
func TestASessionWriteThatLosesEveryRaceIsAConflict(t *testing.T) {
	a, _ := podRegistries(t)
	const sessionID = "ses_raced"
	if err := a.Put(sessionID, SessionEntry{Port: 7, MachineID: "box-a"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	racing := &conflictingSessionKV{KeyValue: a.watcher.KV().KeyValue}
	useKV(t, a, racing)
	err := a.Put(sessionID, SessionEntry{Port: 7, MachineID: "box-a"})
	if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("Put while every write races = %v, want ErrWriteConflict", err)
	}
	if racing.writes != 5 {
		t.Fatalf("Put made %d write attempts, want 5", racing.writes)
	}
}

// A registration carries the fields its build knows; a newer build's field, stored by a pod of that
// build, is kept across a heartbeat through this one rather than flapping away every 90 s during a
// roll.
func TestAHeartbeatKeepsARegistrationFieldThisBuildDoesNotKnow(t *testing.T) {
	a, _ := podRegistries(t)
	const sessionID = "ses_newer_build"
	if _, err := a.watcher.KV().Put(sessionID, []byte(`{"port":4242,"machine_id":"box-a","dir":"/work","future_field":{"kept":true}}`)); err != nil {
		t.Fatalf("store a newer build's registration: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := a.Get(sessionID)
		return err == nil
	})
	if err := a.Put(sessionID, SessionEntry{Port: 4242, MachineID: "box-a", Dir: "/work", Title: "planner"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	stored := storedSession(t, a, sessionID)
	if got := string(stored["future_field"]); got != `{"kept":true}` {
		t.Fatalf("future_field after the heartbeat = %q, want {\"kept\":true}; stored %v", got, stored)
	}
	if got := string(stored["title"]); got != `"planner"` {
		t.Fatalf("title after the heartbeat = %s, want the heartbeat's", got)
	}
}
