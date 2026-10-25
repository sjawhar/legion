package reread

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// delivered is how long a notification that must arrive is given, and the only wait in this file.
// It is also what bounds the two notifications that must NOT reach the reread function: each is
// sent before a good one, and Postgres delivers to one listening session in the order the sends
// committed, so the good one arriving proves the listener was alive, the channel delivering and
// the earlier notification already handled - and the reread function having heard only the good
// one's name proves the earlier one was dropped. Two seconds is far longer than a local round
// trip and short enough to fail a hung listener inside any test timeout.
const delivered = 2 * time.Second

// listener opens a store on a database of its own and runs a reread listener against it under
// sender, returning that store and the channel its reread function reports each name on. The
// database is deliberately unmigrated: the fan-out reads no table.
//
// Each test gets a database rather than a schema (storetest.DatabaseURL, not storetest.URL)
// because Channel is one constant and a Postgres channel is database-wide: on a shared database
// each of these tests would hear the others' sends.
func listener(t *testing.T, sender string) (*store.Store, <-chan string) {
	t.Helper()
	st, err := store.Open(context.Background(), storetest.DatabaseURL(t))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(st.Pool.Close)

	refreshed := make(chan string, 4)
	listening := make(chan struct{})
	l := &Listener{
		Sender: sender,
		Dial:   st.Listener,
		Refresh: func(_ context.Context, name string) error {
			refreshed <- name
			return nil
		},
		connected: sync.OnceFunc(func() { close(listening) }),
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	select {
	case <-listening:
	case <-time.After(delivered):
		t.Fatal("the listener did not listen on its channel")
	}
	return st, refreshed
}

// next answers the name the listener next rereads, failing t when none arrives.
func next(t *testing.T, refreshed <-chan string) string {
	t.Helper()
	select {
	case name := <-refreshed:
		return name
	case <-time.After(delivered):
		t.Fatal("no secret was reread")
		return ""
	}
}

// TestARereadAnotherPodSentIsRunHere is the fan-out's whole point: a notification one pod's
// notifier sends reaches the listener another pod runs against the same database, which rereads
// the secret it names.
func TestARereadAnotherPodSentIsRunHere(t *testing.T) {
	st, refreshed := listener(t, "pod-a")
	notify := Notifier(st.Pool, "pod-b")
	if err := notify(context.Background(), "AGENT_SECRET_ONE"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if name := next(t, refreshed); name != "AGENT_SECRET_ONE" {
		t.Fatalf("reread %q, want AGENT_SECRET_ONE", name)
	}
}

// TestAPodDropsTheNotificationItSentItself pins the sender filter. Postgres delivers a
// notification to every session listening on the channel, including one the sending process
// holds, so a pod hears its own reread come back; running it again would be a second read of the
// secret store for nothing.
func TestAPodDropsTheNotificationItSentItself(t *testing.T) {
	st, refreshed := listener(t, "pod-a")
	own, other := Notifier(st.Pool, "pod-a"), Notifier(st.Pool, "pod-b")
	if err := own(context.Background(), "OWN_REREAD"); err != nil {
		t.Fatalf("notify as the listener's own sender: %v", err)
	}
	if err := other(context.Background(), "ANOTHER_PODS_REREAD"); err != nil {
		t.Fatalf("notify as another sender: %v", err)
	}
	if name := next(t, refreshed); name != "ANOTHER_PODS_REREAD" {
		t.Fatalf("reread %q, want ANOTHER_PODS_REREAD alone: the listener's own notification must be dropped", name)
	}
}

// TestAnUnreadablePayloadIsDroppedAndTheListenerGoesOn pins that a payload this build cannot read
// - anything another writer of the database sends on the channel - costs the listener nothing but
// the one line it logs.
func TestAnUnreadablePayloadIsDroppedAndTheListenerGoesOn(t *testing.T) {
	st, refreshed := listener(t, "pod-a")
	ctx := context.Background()
	for _, payload := range []string{"", "not json at all", `{"sender":"pod-b"}`} {
		if _, err := st.Pool.Exec(ctx, `select pg_notify($1, $2)`, Channel, payload); err != nil {
			t.Fatalf("send %q: %v", payload, err)
		}
	}
	if err := Notifier(st.Pool, "pod-b")(ctx, "AFTER_THE_BAD_ONES"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if name := next(t, refreshed); name != "AFTER_THE_BAD_ONES" {
		t.Fatalf("reread %q, want AFTER_THE_BAD_ONES alone: an unreadable payload must be dropped", name)
	}
}
