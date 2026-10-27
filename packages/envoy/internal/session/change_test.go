package session

import (
	"context"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// sessionChange is one id OnChange reported, with what Get answered for it at the time, as the
// relay that follows the cache reads it.
type sessionChange struct {
	sessionID string
	entry     SessionEntry
	live      bool
}

func watchSessionChanges(t *testing.T, r *SessionRegistry) (<-chan sessionChange, func()) {
	t.Helper()
	changes := make(chan sessionChange, 64)
	stop := r.OnChange(func(sessionID string) {
		entry, err := r.Get(sessionID)
		changes <- sessionChange{sessionID: sessionID, entry: entry, live: err == nil}
	})
	t.Cleanup(stop)
	return changes, stop
}

func awaitSessionChange(t *testing.T, changes <-chan sessionChange, want func(sessionChange) bool, what string) sessionChange {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case change := <-changes:
			if want(change) {
				return change
			}
		case <-deadline:
			t.Fatalf("no change reported: %s", what)
		}
	}
}

func TestOnChangeReportsWritesDeletesAndAnotherListenersWrites(t *testing.T) {
	client := setupNATS(t)
	writer, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open the writing registry: %v", err)
	}
	follower, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open the following registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := follower.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("follower cache ready: %v", err)
	}
	changes, stop := watchSessionChanges(t, follower)

	// Another listener's write reaches the follower through its watcher.
	if err := writer.Put("ses_remote", SessionEntry{MachineID: "m2", Delivery: DeliveryConsumer, Epoch: "e1"}); err != nil {
		t.Fatalf("put through the writer: %v", err)
	}
	awaitSessionChange(t, changes, func(c sessionChange) bool {
		return c.sessionID == "ses_remote" && c.live && c.entry.Delivery == DeliveryConsumer && c.entry.Epoch == "e1"
	}, "another listener's write of ses_remote")

	// The follower's own write is reported at once.
	if err := follower.Put("ses_local", SessionEntry{MachineID: "m1"}); err != nil {
		t.Fatalf("put through the follower: %v", err)
	}
	awaitSessionChange(t, changes, func(c sessionChange) bool { return c.sessionID == "ses_local" && c.live },
		"the follower's own write of ses_local")

	// A delete, whoever made it, leaves the session missing when it is reported.
	if err := writer.Delete("ses_remote"); err != nil {
		t.Fatalf("delete through the writer: %v", err)
	}
	awaitSessionChange(t, changes, func(c sessionChange) bool { return c.sessionID == "ses_remote" && !c.live },
		"the delete of ses_remote")

	// Once stopped, nothing more is reported.
	stop()
	if err := follower.Put("ses_after_stop", SessionEntry{MachineID: "m1"}); err != nil {
		t.Fatalf("put after stop: %v", err)
	}
	if _, err := follower.Get("ses_after_stop"); err != nil {
		t.Fatalf("read ses_after_stop: %v", err)
	}
	for {
		select {
		case change := <-changes:
			if change.sessionID == "ses_after_stop" {
				t.Fatalf("a change was reported after stop: %+v", change)
			}
		default:
			return
		}
	}
}

func TestOnChangeReportsALapseOncePruneLapsedFindsIt(t *testing.T) {
	now := time.Now()
	r := &SessionRegistry{
		ttl: 5 * time.Minute,
		cache: map[string]cachedSession{
			"ses_live":   {entry: SessionEntry{MachineID: "m1", Delivery: DeliveryConsumer}, expiresAt: now.Add(time.Minute)},
			"ses_lapsed": {entry: SessionEntry{MachineID: "m1", Delivery: DeliveryConsumer, UpdatedAt: now.Add(-6 * time.Minute).UnixMilli()}, expiresAt: now.Add(-time.Second)},
		},
		cacheRevisions: map[string]uint64{},
	}
	changes, _ := watchSessionChanges(t, r)

	r.PruneLapsed()

	change := awaitSessionChange(t, changes, func(c sessionChange) bool { return c.sessionID == "ses_lapsed" }, "the lapse of ses_lapsed")
	if change.live {
		t.Fatalf("ses_lapsed is still live when its lapse is reported: %+v", change)
	}
	if _, err := r.Get("ses_lapsed"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(ses_lapsed) after its lapse: %v, want ErrKeyNotFound", err)
	}
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].SessionID != "ses_live" {
		t.Fatalf("List after the lapse = %+v, want ses_live alone", entries)
	}
	r.PruneLapsed()
	select {
	case again := <-changes:
		t.Fatalf("a change was reported with nothing lapsed: %+v", again)
	default:
	}
}
