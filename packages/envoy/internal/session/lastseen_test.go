package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
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

	onA := a.LastSeen(context.Background(), sessionID)
	if onA == 0 {
		t.Fatalf("pod a reports no last-seen time for %s, which it registered %s ago", sessionID, time.Since(written.Created()))
	}
	b := lastSeenPod(t, "b", ttl)
	if onB := b.LastSeen(context.Background(), sessionID); onB != onA {
		t.Fatalf("pod b, started after %s lapsed, reports last seen %d; pod a reports %d", sessionID, onB, onA)
	}
	if want := written.Created().UnixMilli(); onA != want {
		t.Fatalf("last seen = %d, want the server's timestamp of the registration write, %d", onA, want)
	}
}

// Every pod answers a lapsed session's last-seen time out of the bucket's `seen.` record alone,
// so a pod that watched the session answers exactly what a pod that never saw it answers for as
// long as the window lasts, and nothing once the record is gone. The bucket is sized to keep the
// record past that window (listenerStateMaxAge), so a live deployment never reaches the second
// case; this is what it would answer if it did. A pod that kept an answer of its own would go on
// reporting the session after the bucket's record was gone, and two pods would answer
// GET /v1/interests/ differently for one session.
func TestEveryPodAnswersAlikeOnceTheBucketsLastSeenRecordIsGone(t *testing.T) {
	const (
		sessionID = "ses_outlives_record"
		ttl       = 2 * time.Second
	)
	a := lastSeenPod(t, "a", ttl, WithLastSeenRetention(time.Minute))
	if err := a.Put(sessionID, SessionEntry{Port: 4243, MachineID: "box-a"}); err != nil {
		t.Fatalf("register %s through pod a: %v", sessionID, err)
	}
	deadline := time.Now().Add(ttl + 10*time.Second)
	for a.LastSeen(context.Background(), sessionID) == 0 || func() bool { _, err := a.Get(sessionID); return err == nil }() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not lapse on pod a with a last-seen time within %s", sessionID, ttl+10*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
	b := lastSeenPod(t, "b", ttl, WithLastSeenRetention(time.Minute))
	if onA, onB := a.LastSeen(context.Background(), sessionID), b.LastSeen(context.Background(), sessionID); onA != onB {
		t.Fatalf("pod a reports %s last seen %d while the record stands; pod b, started after it lapsed, reports %d", sessionID, onA, onB)
	}
	// What the bucket's MaxAge would do to the record, were the window longer than the bucket
	// keeps it.
	if err := a.seen.Delete(lastSeenKey(sessionID)); err != nil {
		t.Fatalf("remove %s's seen record: %v", sessionID, err)
	}
	onA, onB := a.LastSeen(context.Background(), sessionID), b.LastSeen(context.Background(), sessionID)
	if onA != onB {
		t.Fatalf("pod a, which watched %s, reports last seen %d once the bucket's record is gone; pod b reports %d", sessionID, onA, onB)
	}
	if onA != 0 {
		t.Fatalf("pods report %s last seen %d with no record in the bucket, want none", sessionID, onA)
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

// A registration through a connection holding exactly the listener's grant
// (testnats.ListenerGrant, the grant envoy-infra renders) records the session's last-seen time,
// with no WARN: the registration write is read back by key, a direct get on the key's subject,
// which the grant allows. A read by revision is a direct get on the session stream's bare subject,
// which the grant refuses, so the read waited out its budget and no record was written.
func TestARegistrationUnderTheListenersGrantRecordsItsLastSeen(t *testing.T) {
	const sessionID = "ses_granted"
	seed, public := testnats.User(t)
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: testnats.ListenerGrant(t, "box-a")})
	conn, err := testnats.ConnectSeed(t, granted.URL(), seed)
	if err != nil {
		t.Fatalf("connect as the listener's user: %v", err)
	}
	t.Cleanup(conn.Close)
	var logs syncBuffer
	reg, err := OpenSessionRegistry(conn, WithSessionReplicas(1), WithLastSeenBucket(listenerStateBucket(t, conn)),
		WithSessionLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatalf("open the session registry under the listener's grant: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	if err := reg.Put(sessionID, SessionEntry{Port: 4242, MachineID: "box-a"}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}

	admin := testnats.Connect(t, granted.AdminURL)
	t.Cleanup(admin.Close)
	js, err := admin.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	sessions, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	written, err := sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's registration: %v", sessionID, err)
	}
	state, err := js.KeyValue(bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	entry, err := state.Get(lastSeenKey(sessionID))
	if err != nil {
		t.Fatalf("read %s's last-seen record: %v\nregistry log:\n%s", sessionID, err, logs.String())
	}
	var record seenRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		t.Fatalf("decode %s's last-seen record %q: %v", sessionID, entry.Value(), err)
	}
	if want := written.Created().UnixMilli(); record.At != want {
		t.Fatalf("last seen = %d, want the server's timestamp of the registration write, %d", record.At, want)
	}
	if got := logs.String(); strings.Contains(got, "level=WARN") {
		t.Fatalf("the registry warned under the listener's grant:\n%s", got)
	}
}

// A registration write that a newer write replaced before its read back records nothing: the read
// by key finds the newer revision, and that write records its own time. The newer write's own
// read back records it.
func TestALastSeenReadBackThatFindsANewerWriteRecordsNothing(t *testing.T) {
	const sessionID = "ses_replaced"
	client := setupNATS(t)
	var logs syncBuffer
	state := listenerStateBucket(t, client.Conn)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithLastSeenBucket(state),
		WithSessionLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	value, err := encodeSessionEntry(SessionEntry{Port: 4242, MachineID: "box-a"})
	if err != nil {
		t.Fatalf("encode the registration: %v", err)
	}
	kv := reg.watcher.KV()
	older, err := kv.Put(sessionID, value)
	if err != nil {
		t.Fatalf("write the older registration: %v", err)
	}
	newer, err := kv.Put(sessionID, value)
	if err != nil {
		t.Fatalf("write the newer registration: %v", err)
	}

	reg.recordSeen(context.Background(), kv, sessionID, older)
	if entry, err := state.Get(lastSeenKey(sessionID)); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the replaced write recorded %v, %v; want no last-seen record", entry, err)
	}
	reg.recordSeen(context.Background(), kv, sessionID, newer)
	entry, err := state.Get(lastSeenKey(sessionID))
	if err != nil {
		t.Fatalf("read the newer write's last-seen record: %v", err)
	}
	written, err := kv.Get(sessionID)
	if err != nil {
		t.Fatalf("read the newer registration: %v", err)
	}
	var record seenRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil || record.At != written.Created().UnixMilli() {
		t.Fatalf("last-seen record %q (%v), want the newer write's timestamp %d", entry.Value(), err, written.Created().UnixMilli())
	}
	if got := logs.String(); strings.Contains(got, "level=WARN") {
		t.Fatalf("the registry warned:\n%s", got)
	}
}
