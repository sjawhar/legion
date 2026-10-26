package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// endingGetKeyValue ends the request (end) when the claim's cleanup reads key's interest record,
// and answers that read only after the request has seen its end, so the request stops waiting
// before the cleanup's first bucket call answers.
type endingGetKeyValue struct {
	natsgo.KeyValue

	key string
	end context.CancelFunc
}

func (kv *endingGetKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.key {
		kv.end()
		time.Sleep(100 * time.Millisecond)
	}
	return kv.KeyValue.Get(key)
}

// endingWriteKeyValue ends the request when the claim is written, and answers the write only after
// the request has seen its end, so the claim's write does not answer within it. With land set the
// write lands in the bucket all the same; without it, it never does. Each Get after the write
// counts in getsAfterWrite.
type endingWriteKeyValue struct {
	natsgo.KeyValue

	end            context.CancelFunc
	land           bool
	written        atomic.Bool
	getsAfterWrite atomic.Int32
}

func (kv *endingWriteKeyValue) Update(key string, value []byte, last uint64) (uint64, error) {
	var revision uint64
	err := errors.New("injected claim write that does not land")
	if kv.land {
		revision, err = kv.KeyValue.Update(key, value, last)
	}
	kv.written.Store(true)
	kv.end()
	time.Sleep(100 * time.Millisecond)
	return revision, err
}

func (kv *endingWriteKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	if kv.written.Load() {
		kv.getsAfterWrite.Add(1)
	}
	return entry, err
}

// rawTopics returns sessionID's topics as the interest bucket holds them, and false when it holds
// no record for sessionID.
func rawTopics(t *testing.T, kv natsgo.KeyValue, sessionID string) ([]string, bool) {
	t.Helper()
	entry, err := kv.Get(sessionID)
	if errors.Is(err, natsgo.ErrKeyNotFound) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read %s's interest record: %v", sessionID, err)
	}
	var item Interest
	if err := json.Unmarshal(entry.Value(), &item); err != nil {
		t.Fatalf("decode %s's interest record: %v", sessionID, err)
	}
	return item.Topics, true
}

// A claim's two cleanups - the old holder's loss of the role's topic once the claim lands, and the
// rollback of the claimant's topic once it does not - run under their own bound, detached from
// the request that made the claim, so a request that ended before they answer still has them
// done. The claim's own answer is unchanged: the request ended, so it is the error it was. The
// control is a claim whose write did not answer within the request but landed all the same: its
// claimant keeps the topic, since the rollback reads the claim again first.
func TestAClaimsCleanupsOutliveTheRequestThatMadeIt(t *testing.T) {
	const (
		oldSession = "ses_old"
		newSession = "ses_new"
		role       = "legion-controller"
		roleTopic  = "notifications.role.legion-controller"
		otherTopic = "notifications.slack.>"
	)
	seed := func(t *testing.T) (*Registry, natsgo.KeyValue) {
		t.Helper()
		conn, cleanup := connectNATS(t)
		t.Cleanup(cleanup)
		reg, kv := coldRegistry(t, conn)
		putInterest(t, kv, Interest{SessionID: oldSession, MachineID: "example-host", Topics: []string{roleTopic, otherTopic}})
		if _, err := reg.roleKV.Put(role, []byte(oldSession)); err != nil {
			t.Fatalf("seed role claim: %v", err)
		}
		return reg, kv
	}

	t.Run("the old holder's topic", func(t *testing.T) {
		reg, kv := seed(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reg.watcher = kvwatch.New("interest registry", bus.KeyValue{KeyValue: &endingGetKeyValue{KeyValue: kv, key: oldSession, end: cancel}}, reg.applyWatched, reg.resetCache)
		if _, err := reg.SetRoleWithPrevious(ctx, newSession, "example-host", role, "", false); !errors.Is(err, context.Canceled) {
			t.Fatalf("a claim whose request ended during its cleanup returned %v, want the request's end", err)
		}
		assertRoleHolder(t, reg, role, newSession)
		waitFor(t, 5*time.Second, func() bool {
			topics, _ := rawTopics(t, kv, oldSession)
			return slices.Equal(topics, []string{otherTopic})
		})
		topics, _ := rawTopics(t, kv, oldSession)
		t.Logf("the old holder's topics after the request ended: %v", topics)
	})

	t.Run("a failed claim's rollback", func(t *testing.T) {
		reg, kv := seed(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reg.roleKV = bus.KeyValue{KeyValue: &endingWriteKeyValue{KeyValue: reg.roleKV, end: cancel}}
		if _, err := reg.SetRoleWithPrevious(ctx, newSession, "example-host", role, "", false); !errors.Is(err, context.Canceled) {
			t.Fatalf("a claim whose write did not answer within its request returned %v, want the request's end", err)
		}
		assertRoleHolder(t, reg, role, oldSession)
		waitFor(t, 5*time.Second, func() bool {
			_, held := rawTopics(t, kv, newSession)
			return !held
		})
		if topics, _ := rawTopics(t, kv, oldSession); !slices.Equal(topics, []string{roleTopic, otherTopic}) {
			t.Fatalf("the holder's topics = %v, want them untouched", topics)
		}
	})

	t.Run("control: a claim that landed after its request ended", func(t *testing.T) {
		reg, kv := seed(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		roles := &endingWriteKeyValue{KeyValue: reg.roleKV, end: cancel, land: true}
		reg.roleKV = bus.KeyValue{KeyValue: roles}
		if _, err := reg.SetRoleWithPrevious(ctx, newSession, "example-host", role, "", false); !errors.Is(err, context.Canceled) {
			t.Fatalf("a claim whose write did not answer within its request returned %v, want the request's end", err)
		}
		// The holder read the request ended skips, so the first read after the write is the
		// rollback's; a rollback that does not read removes the topic instead.
		waitFor(t, 5*time.Second, func() bool {
			topics, _ := rawTopics(t, kv, newSession)
			return roles.getsAfterWrite.Load() > 0 || !slices.Contains(topics, roleTopic)
		})
		time.Sleep(200 * time.Millisecond)
		assertRoleHolder(t, reg, role, newSession)
		if topics, held := rawTopics(t, kv, newSession); !slices.Equal(topics, []string{roleTopic}) {
			t.Fatalf("the claimant whose claim landed has topics %v (record held: %t), want %s kept", topics, held, roleTopic)
		}
	})
}

// waitCached waits until reg's cache holds sessionID at revision or later.
func waitCached(t *testing.T, reg *Registry, sessionID string, revision uint64) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return reg.cachedRevision(sessionID) >= revision })
}

// putRaw writes item to the interest bucket directly and waits for the registry's cache to hold it.
func putRaw(t *testing.T, reg *Registry, item Interest) {
	t.Helper()
	buf, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal %s's interest: %v", item.SessionID, err)
	}
	revision, err := reg.interests().Put(item.SessionID, buf)
	if err != nil {
		t.Fatalf("write %s's interest: %v", item.SessionID, err)
	}
	waitCached(t, reg, item.SessionID, revision)
}

// A claim's cleanup that fails all the same leaves a role topic on a record whose role's claim
// names another session or none, and the role reaper removes it. A claim writes its claimant's
// topic before the claim itself, so the reaper removes only a topic two consecutive passes found
// so on the same revision of the record. The negative controls: a session that holds its role's
// claim keeps the topic; one whose claim lands between the passes keeps it; one whose record is
// written between the passes keeps it until a pass after that write.
func TestTheRoleReaperRemovesARoleTopicWhoseClaimNamesAnotherSessionOrNone(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg := openRegistry(t, conn)
	const (
		other      = "notifications.slack.>"
		holder     = "ses_holder"
		superseded = "ses_superseded"
		unclaimed  = "ses_unclaimed"
		late       = "ses_late"
		rewritten  = "ses_rewritten"
	)
	topic := func(role string) string { return "notifications.role." + role }
	for _, item := range []Interest{
		{SessionID: holder, MachineID: "m1", Topics: []string{topic("r")}},
		{SessionID: superseded, MachineID: "m1", Topics: []string{other, topic("r")}},
		{SessionID: unclaimed, MachineID: "m1", Topics: []string{topic("q")}},
		{SessionID: late, MachineID: "m1", Topics: []string{topic("l")}},
		{SessionID: rewritten, MachineID: "m1", Topics: []string{topic("w")}},
	} {
		putRaw(t, reg, item)
	}
	if _, err := reg.roleKV.Put("r", []byte(holder)); err != nil {
		t.Fatalf("seed r's claim: %v", err)
	}
	alive := func(string) bool { return true }
	pass := func(n int) {
		t.Helper()
		if reaped, err := reg.ReapRoleClaims(alive, 0); err != nil || reaped != 0 {
			t.Fatalf("role reaper pass %d = %d, %v; want no claim reaped", n, reaped, err)
		}
	}
	want := func(n int, sessionID string, topics ...string) {
		t.Helper()
		got, held := rawTopics(t, reg.interests(), sessionID)
		if len(topics) == 0 {
			if held {
				t.Fatalf("after pass %d %s's record holds %v, want it deleted", n, sessionID, got)
			}
			return
		}
		if !slices.Equal(got, topics) {
			t.Fatalf("after pass %d %s's topics = %v, want %v", n, sessionID, got, topics)
		}
	}

	pass(1)
	want(1, holder, topic("r"))
	want(1, superseded, other, topic("r"))
	want(1, unclaimed, topic("q"))
	want(1, late, topic("l"))
	want(1, rewritten, topic("w"))

	if _, err := reg.roleKV.Put("l", []byte(late)); err != nil {
		t.Fatalf("land l's claim between the passes: %v", err)
	}
	written, err := reg.Upsert(Interest{SessionID: rewritten}, []string{other})
	if err != nil {
		t.Fatalf("write %s's record between the passes: %v", rewritten, err)
	}
	if !slices.Contains(written.Topics, topic("w")) {
		t.Fatalf("%s's rewrite holds %v, want %s kept", rewritten, written.Topics, topic("w"))
	}

	pass(2)
	want(2, holder, topic("r"))
	want(2, superseded, other)
	want(2, unclaimed)
	want(2, late, topic("l"))
	want(2, rewritten, topic("w"), other)

	pass(3)
	want(3, holder, topic("r"))
	want(3, late, topic("l"))
	want(3, rewritten, other)

	removed := strings.Count(logs.String(), "role reaper removed a role topic whose claim names another session or none")
	if removed != 3 {
		t.Fatalf("the reaper logged %d removals, want 3:\n%s", removed, logs.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, "role reaper removed") {
			t.Log(line)
		}
	}
}

// claimLandingKeyValue writes the claim of role naming holder just before the get of role's claim
// numbered at answers, so the claim lands between a read of it and the next.
type claimLandingKeyValue struct {
	natsgo.KeyValue

	role, holder string
	at           int32
	gets         atomic.Int32
}

func (kv *claimLandingKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.role && kv.gets.Add(1) == kv.at {
		if _, err := kv.KeyValue.Put(kv.role, []byte(kv.holder)); err != nil {
			return nil, err
		}
	}
	return kv.KeyValue.Get(key)
}

// A claim whose interest write the second pass met and whose claim landed only after that pass read
// it loses the topic to the pass's removal; the reaper reads the claim again after removing and
// puts the topic back.
func TestTheRoleReaperPutsBackATopicWhoseClaimLandsAsItIsRemoved(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg := openRegistry(t, conn)
	const (
		sessionID = "ses_claiming"
		roleTopic = "notifications.role.p"
	)
	putRaw(t, reg, Interest{SessionID: sessionID, MachineID: "m1", Dir: "/work", Topics: []string{roleTopic}})
	// The first pass reads p's claim once and the second once before removing, so the third read is
	// the one after the removal.
	reg.roleKV = bus.KeyValue{KeyValue: &claimLandingKeyValue{KeyValue: reg.roleKV, role: "p", holder: sessionID, at: 3}}
	for n := 1; n <= 2; n++ {
		if _, err := reg.ReapRoleClaims(func(string) bool { return true }, 0); err != nil {
			t.Fatalf("role reaper pass %d: %v", n, err)
		}
	}
	assertRoleHolder(t, reg, "p", sessionID)
	if topics, _ := rawTopics(t, reg.interests(), sessionID); !slices.Equal(topics, []string{roleTopic}) {
		t.Fatalf("the claimant's topics = %v, want %s put back", topics, roleTopic)
	}
	item, err := reg.Get(context.Background(), sessionID)
	if err != nil || item.MachineID != "m1" || item.Dir != "/work" {
		t.Fatalf("the claimant's record = %+v, %v; want its machine and dir kept", item, err)
	}
	for _, want := range []string{"role reaper removed a role topic", "role reaper put back a role topic whose claim landed as it was removed"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the reaper's log does not say %q:\n%s", want, logs.String())
		}
	}
}
