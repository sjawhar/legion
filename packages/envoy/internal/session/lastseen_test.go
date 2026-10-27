package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// listenerStateBucket opens the listener state bucket over conn, as the listener's lease does.
func listenerStateBucket(t *testing.T, conn *natsgo.Conn) bus.KeyValue {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(js, &natsgo.KeyValueConfig{Bucket: bus.ListenerStateBucket, Replicas: 1, Storage: natsgo.FileStorage, TTL: 10 * time.Minute})
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	return kv
}

// lastSeenPod opens one listener pod's session registry over a NATS connection of its own, with
// the listener state bucket opened over that connection too, and returns it once its cache is warm.
func lastSeenPod(t *testing.T, name string, ttl time.Duration, options ...SessionRegistryOption) *SessionRegistry {
	t.Helper()
	client := setupNATS(t)
	options = append([]SessionRegistryOption{WithSessionReplicas(1), WithSessionTTL(ttl), WithLastSeenBucket(listenerStateBucket(t, client.Conn))}, options...)
	reg, err := OpenSessionRegistry(client.Conn, options...)
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

// A session that heartbeated only through pod A and then expired is reported with one last-seen
// time by every pod: pod B, started after the session lapsed on a connection of its own, answers
// what pod A answers. Before, the last-seen time lived only in the memory of a pod whose cache had
// held the session, so pod B had none.
func TestEveryPodReportsALapsedSessionsLastSeenAlike(t *testing.T) {
	const (
		sessionID = "ses_lapsing"
		ttl       = 3 * time.Second
	)
	a := lastSeenPod(t, "a", ttl)
	if err := a.Put(sessionID, SessionEntry{Port: 4242, MachineID: "box-a"}); err != nil {
		t.Fatalf("register %s through pod a: %v", sessionID, err)
	}
	written, err := a.watcher.KV().Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's registration: %v", sessionID, err)
	}
	// The registration expires when the session bucket's MaxAge removes it.
	deadline := time.Now().Add(ttl + 10*time.Second)
	for {
		if _, err := a.watcher.KV().Get(sessionID); errors.Is(err, natsgo.ErrKeyNotFound) {
			break
		} else if err != nil {
			t.Fatalf("read %s while it expires: %v", sessionID, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's registration was not removed within %s of its write", sessionID, ttl+10*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := a.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("pod a's cache still holds the expired %s: %v", sessionID, err)
	}

	onA := a.LastSeen(sessionID)
	if onA == 0 {
		t.Fatalf("pod a reports no last-seen time for %s, which it registered %s ago", sessionID, time.Since(written.Created()))
	}
	b := lastSeenPod(t, "b", ttl)
	if onB := b.LastSeen(sessionID); onB != onA {
		t.Fatalf("pod b, started after %s lapsed, reports last seen %d; pod a reports %d", sessionID, onB, onA)
	}
	if want := written.Created().UnixMilli(); onA != want {
		t.Fatalf("last seen = %d, want the server's timestamp of the registration write, %d", onA, want)
	}
}

// failingPutKV is a listener state bucket whose every Put fails.
type failingPutKV struct {
	natsgo.KeyValue
}

var errSeenPut = errors.New("injected last-seen put failure")

func (failingPutKV) Put(string, []byte) (uint64, error) { return 0, errSeenPut }

// syncBuffer is a log sink the registry writes to from its own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A last-seen record the bucket refuses is a WARN, not a failed registration: the session is
// registered all the same.
func TestARegistrationWhoseLastSeenRecordFailsIsStillWritten(t *testing.T) {
	client := setupNATS(t)
	var logs syncBuffer
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1),
		WithLastSeenBucket(bus.KeyValue{KeyValue: failingPutKV{KeyValue: listenerStateBucket(t, client.Conn).KeyValue}}),
		WithSessionLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	if err := reg.Put("ses_unrecorded", SessionEntry{Port: 4242, MachineID: "box-a"}); err != nil {
		t.Fatalf("register with a failing last-seen record: %v, want the registration written", err)
	}
	if _, err := reg.watcher.KV().Get("ses_unrecorded"); err != nil {
		t.Fatalf("read the registration from the session bucket: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "session last-seen not recorded") || !strings.Contains(got, errSeenPut.Error()) {
		t.Fatalf("no WARN naming the failed last-seen record:\n%s", got)
	}
}
