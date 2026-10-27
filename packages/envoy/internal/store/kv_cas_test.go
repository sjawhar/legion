package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// podRegistries opens the interest registry twice on one pair of buckets, each over its own NATS
// connection, as two listener pods of one machine do, and returns both once their caches are warm.
func podRegistries(t *testing.T) (a, b *Registry) {
	t.Helper()
	open := func(name string) *Registry {
		t.Helper()
		conn, cleanup := connectNATS(t)
		t.Cleanup(cleanup)
		reg, err := Open(conn, WithReplicas(1))
		if err != nil {
			t.Fatalf("open pod %s's registry: %v", name, err)
		}
		t.Cleanup(reg.StopWatch)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := reg.WaitForCacheReady(ctx); err != nil {
			t.Fatalf("warm pod %s's cache: %v", name, err)
		}
		return reg
	}
	return open("a"), open("b")
}

// storedJSON reads key from kv itself, as raw JSON keys.
func storedJSON(t *testing.T, kv natsgo.KeyValue, key string) map[string]json.RawMessage {
	t.Helper()
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatalf("read %s from %s: %v", key, kv.Bucket(), err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &stored); err != nil {
		t.Fatalf("decode %s's stored value %s: %v", key, entry.Value(), err)
	}
	return stored
}

func storedTopics(t *testing.T, reg *Registry, sessionID string) []string {
	t.Helper()
	var topics []string
	if err := json.Unmarshal(storedJSON(t, reg.interests().KeyValue, sessionID)["topics"], &topics); err != nil {
		t.Fatalf("decode %s's stored topics: %v", sessionID, err)
	}
	slices.Sort(topics)
	return topics
}

// A topic one pod adds is kept by a heartbeat or an unsubscribe on a pod whose cache trails the
// bucket: each write is a compare-and-set on the revision the writer read, so a stale copy's write
// fails and is merged again onto what the other pod stored.
func TestATopicAddedOnOnePodSurvivesAWriteOnAPodWhoseCacheTrails(t *testing.T) {
	const sessionID = "ses_two_pods"
	agent := "notifications.agent." + sessionID
	t.Run("heartbeat", func(t *testing.T) {
		a, b := podRegistries(t)
		if _, err := b.Upsert(Interest{SessionID: sessionID, MachineID: "box-a"}, []string{agent}); err != nil {
			t.Fatalf("subscribe through pod B: %v", err)
		}
		b.StopWatch()
		if _, err := a.Upsert(Interest{SessionID: sessionID, MachineID: "box-a"}, []string{"notifications.team.added"}); err != nil {
			t.Fatalf("add a topic through pod A: %v", err)
		}
		if _, err := b.Upsert(Interest{SessionID: sessionID, MachineID: "box-a"}, []string{agent}); err != nil {
			t.Fatalf("heartbeat through pod B: %v", err)
		}
		if got, want := storedTopics(t, a, sessionID), []string{agent, "notifications.team.added"}; !slices.Equal(got, want) {
			t.Fatalf("stored topics after pod B's stale heartbeat = %v, want %v", got, want)
		}
	})
	t.Run("unsubscribe", func(t *testing.T) {
		a, b := podRegistries(t)
		if _, err := b.Upsert(Interest{SessionID: sessionID, MachineID: "box-a"}, []string{agent, "notifications.team.leaving"}); err != nil {
			t.Fatalf("subscribe through pod B: %v", err)
		}
		b.StopWatch()
		if _, err := a.Upsert(Interest{SessionID: sessionID, MachineID: "box-a"}, []string{"notifications.team.added"}); err != nil {
			t.Fatalf("add a topic through pod A: %v", err)
		}
		if err := b.Remove(context.Background(), sessionID, []string{agent, "notifications.team.leaving"}); err != nil {
			t.Fatalf("unsubscribe through pod B: %v", err)
		}
		if got, want := storedTopics(t, a, sessionID), []string{"notifications.team.added"}; !slices.Equal(got, want) {
			t.Fatalf("stored topics after pod B's stale unsubscribe = %v, want %v", got, want)
		}
	})
}

// conflictingInterestKV is an interest bucket another writer races on every write: before each
// Create, Update or conditional Delete it writes the key itself, so the expected revision is stale.
type conflictingInterestKV struct {
	natsgo.KeyValue
	writes int
}

func (kv *conflictingInterestKV) race(key string) {
	_, _ = kv.KeyValue.Put(key, []byte(`{"session_id":"`+key+`","machine_id":"box-b","topics":["notifications.team.racer"]}`))
}

func (kv *conflictingInterestKV) Create(key string, value []byte) (uint64, error) {
	kv.writes++
	kv.race(key)
	return kv.KeyValue.Create(key, value)
}

func (kv *conflictingInterestKV) Update(key string, value []byte, last uint64) (uint64, error) {
	kv.writes++
	kv.race(key)
	return kv.KeyValue.Update(key, value, last)
}

// An interest write that finds the stored revision moved under it every time gives up after five
// attempts with ErrWriteConflict, which the listener answers as a 503 to retry.
func TestAnInterestWriteThatLosesEveryRaceIsAConflict(t *testing.T) {
	a, _ := podRegistries(t)
	const sessionID = "ses_raced"
	if _, err := a.Upsert(Interest{SessionID: sessionID, MachineID: "box-a"}, []string{"notifications.agent.ses_raced"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	racing := &conflictingInterestKV{KeyValue: a.interests().KeyValue}
	useKV(t, a, racing)
	_, err := a.Upsert(Interest{SessionID: sessionID, MachineID: "box-a"}, []string{"notifications.team.more"})
	if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("Upsert while every write races = %v, want ErrWriteConflict", err)
	}
	if racing.writes != 5 {
		t.Fatalf("Upsert made %d write attempts, want 5", racing.writes)
	}
}

// An interest and a role claim carry the fields their build knows; a newer build's field, stored by
// a pod of that build, is kept across a heartbeat, an unsubscribe and a role claim through this one.
func TestInterestAndClaimWritesKeepFieldsThisBuildDoesNotKnow(t *testing.T) {
	a, _ := podRegistries(t)
	const sessionID = "ses_newer_build"
	interests, roles := a.interests().KeyValue, a.roles().KeyValue
	if _, err := interests.Put(sessionID, []byte(`{"session_id":"ses_newer_build","machine_id":"box-a","topics":["notifications.agent.ses_newer_build","notifications.team.gone"],"future_field":[1,2]}`)); err != nil {
		t.Fatalf("store a newer build's interest: %v", err)
	}
	if _, err := roles.Put("keeper", []byte(`{"holder_session_id":"ses_before","claimed_at":1,"future_claim_field":"kept"}`)); err != nil {
		t.Fatalf("store a newer build's claim: %v", err)
	}
	pollMatch(t, a, "box-a", "notifications.team.gone", 1, 5*time.Second)

	if _, err := a.Upsert(Interest{SessionID: sessionID, MachineID: "box-a"}, []string{"notifications.team.new"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if got := string(storedJSON(t, interests, sessionID)["future_field"]); got != "[1,2]" {
		t.Fatalf("interest future_field after a heartbeat = %q, want [1,2]", got)
	}
	if err := a.Remove(context.Background(), sessionID, []string{"notifications.team.gone"}); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if got := string(storedJSON(t, interests, sessionID)["future_field"]); got != "[1,2]" {
		t.Fatalf("interest future_field after an unsubscribe = %q, want [1,2]", got)
	}
	if _, err := a.SetRole(sessionID, "box-a", "keeper", false); err != nil {
		t.Fatalf("claim keeper: %v", err)
	}
	if got := string(storedJSON(t, interests, sessionID)["future_field"]); got != "[1,2]" {
		t.Fatalf("interest future_field after a role claim = %q, want [1,2]", got)
	}
	claim := storedJSON(t, roles, "keeper")
	if got := string(claim["future_claim_field"]); got != `"kept"` {
		t.Fatalf("claim future_claim_field after a role claim = %q, want \"kept\"; stored %v", got, claim)
	}
	if got := string(claim["holder_session_id"]); got != `"`+sessionID+`"` {
		t.Fatalf("claim holder after a role claim = %s, want %s", got, sessionID)
	}
}

// beforeDeleteKeyValue runs before ahead of the first Delete, then deletes as asked.
type beforeDeleteKeyValue struct {
	natsgo.KeyValue
	before func()
}

func (kv *beforeDeleteKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	if before := kv.before; before != nil {
		kv.before = nil
		before()
	}
	return kv.KeyValue.Delete(key, opts...)
}

// The role reaper deletes a claim this build cannot read (one an earlier build stored under a key
// past the read bound) at the revision its scan saw, so a claim an earlier build writes there in the
// meantime is kept rather than deleted with the one the reaper judged.
func TestTheRoleReaperDeletesAClaimItCannotReadAtItsRevision(t *testing.T) {
	_, unreadable := testnats.LegacyKeys(bus.RoleClaimBucket, "r")
	stale := time.Now().Add(-time.Hour).UnixMilli()
	claim := func(holder string, at int64) []byte {
		value, err := json.Marshal(RoleClaim{HolderSessionID: holder, ClaimedAt: at})
		if err != nil {
			t.Fatalf("encode claim: %v", err)
		}
		return value
	}
	registry, _, rawRoles := openRegistryOver(t, func(_ natsgo.JetStreamContext, _, rawRoles natsgo.KeyValue) {
		if _, err := rawRoles.Put(unreadable, claim("ses_gone", stale)); err != nil {
			t.Fatalf("store the unreadable claim: %v", err)
		}
	})
	registry.roleKV = bus.KeyValue{KeyValue: &beforeDeleteKeyValue{KeyValue: rawRoles, before: func() {
		if _, err := rawRoles.Put(unreadable, claim("ses_fresh", time.Now().UnixMilli())); err != nil {
			t.Fatalf("rewrite the claim between the reaper's scan and its delete: %v", err)
		}
	}}}
	if reaped, err := registry.ReapRoleClaims(func(string) bool { return false }, 0); err != nil || reaped != 0 {
		t.Fatalf("reap role claims = %d, %v; want none: the claim the scan saw was rewritten", reaped, err)
	}
	entry, err := rawRoles.Get(unreadable)
	if err != nil {
		t.Fatalf("the claim rewritten before the reaper's delete: %v, want it kept", err)
	}
	var kept RoleClaim
	if err := json.Unmarshal(entry.Value(), &kept); err != nil || kept.HolderSessionID != "ses_fresh" {
		t.Fatalf("the claim after the reaper = %s (%v), want ses_fresh's", entry.Value(), err)
	}
}
