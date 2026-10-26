package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// gate holds one hook of a read until the test opens it. reached closes once the hook is held;
// done closes once the hook has run on, after the gate opened.
type gate struct {
	reached, release, done chan struct{}
	once                   sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{reached: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// pass holds the caller at the gate until it opens, then runs through, and says it ran.
func (g *gate) pass(through func()) {
	close(g.reached)
	<-g.release
	through()
	close(g.done)
}

// waitClosed waits for ch, which names what, failing the test after 10 s.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// holdingCache is the registry's cache with a test's gates in front of its hooks: the next apply
// of a key a test holds, and the next read's completion, wait for the test. A completion held is a
// read that has delivered every record and has not yet swept, the moment a list during a read sees.
type holdingCache struct {
	kvwatch.Cache
	mu         sync.Mutex
	applies    map[string]*gate
	completion *gate
	afterSweep func()
}

func (c *holdingCache) Apply(entry kvwatch.Entry, read kvwatch.Read) {
	c.mu.Lock()
	g := c.applies[entry.Key]
	delete(c.applies, entry.Key)
	c.mu.Unlock()
	if g == nil {
		c.Cache.Apply(entry, read)
		return
	}
	g.pass(func() { c.Cache.Apply(entry, read) })
}

func (c *holdingCache) CompleteRead(read kvwatch.Read) {
	c.mu.Lock()
	g, afterSweep := c.completion, c.afterSweep
	c.completion, c.afterSweep = nil, nil
	c.mu.Unlock()
	if g == nil {
		c.Cache.CompleteRead(read)
		return
	}
	g.pass(func() {
		c.Cache.CompleteRead(read)
		if afterSweep != nil {
			afterSweep()
		}
	})
}

// holdApply holds the next apply of key.
func (c *holdingCache) holdApply(t *testing.T, key string) *gate {
	g := newGate(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applies == nil {
		c.applies = map[string]*gate{}
	}
	c.applies[key] = g
	return g
}

// holdCompletion holds the next read's completion. afterSweep, when not nil, runs right after its
// sweep, before the watcher applies any later record, so it sees the copy the sweep left.
func (c *holdingCache) holdCompletion(t *testing.T, afterSweep func()) *gate {
	g := newGate(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completion, c.afterSweep = g, afterSweep
	return g
}

// holdingRegistry opens a registry on the test's own session bucket whose watcher feeds it
// through a holdingCache, and waits for that watcher's first read to complete.
func holdingRegistry(t *testing.T, client *bus.Client, options ...SessionRegistryOption) (*SessionRegistry, *holdingCache) {
	t.Helper()
	r, err := OpenSessionRegistry(client.Conn, append([]SessionRegistryOption{WithSessionReplicas(1), WithSessionTTL(time.Minute)}, options...)...)
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	waitFor(t, 5*time.Second, r.watcher.Following)
	// The bucket is empty, so once the first read has completed nothing more reaches the cache
	// through the watcher replaced here.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.watcher.End(ctx)
	held := &holdingCache{Cache: sessionCache{r}}
	r.watcher = kvwatch.New("session registry", r.watcher.KV(), held)
	t.Cleanup(r.StopWatch)
	r.watcher.Start()
	waitFor(t, 5*time.Second, r.watcher.Following)
	return r, held
}

// listed is the ids of the sessions List answers.
func listed(t *testing.T, r *SessionRegistry) []string {
	t.Helper()
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return idsOf(entries)
}

// sweptList is the ids of the sessions List answers, for a hook to read right after a sweep. List
// never fails, and a hook runs outside the test's goroutine, where it may not fail the test.
func sweptList(r *SessionRegistry) []string {
	entries, _ := r.List()
	return idsOf(entries)
}

func idsOf(entries []ListEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.SessionID)
	}
	return ids
}

func titleOf(t *testing.T, r *SessionRegistry, sessionID string) string {
	t.Helper()
	entry, err := r.Get(sessionID)
	if err != nil {
		t.Fatalf("Get(%s): %v", sessionID, err)
	}
	return entry.Title
}

func putRaw(t *testing.T, kv natsgo.KeyValue, sessionID string, entry SessionEntry) uint64 {
	t.Helper()
	value, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode %s: %v", sessionID, err)
	}
	revision, err := kv.Put(sessionID, value)
	if err != nil {
		t.Fatalf("put %s: %v", sessionID, err)
	}
	return revision
}

// A heartbeat that lands while the read is under way, before the read applies the older value it
// delivered for the same session, wins: the older value is fenced out, and it still keeps the
// session through the sweep.
func TestAHeartbeatNewerThanTheReadsValueWinsAndIsKept(t *testing.T) {
	client := setupNATS(t)
	r, held := holdingRegistry(t, client)
	putRaw(t, r.watcher.KV(), "ses_beat", SessionEntry{Port: 1, MachineID: "m1", Title: "scanned", UpdatedAt: time.Now().UnixMilli()})
	// The current watcher has applied it, so the held apply below is the new read's.
	waitFor(t, 5*time.Second, func() bool {
		entry, err := r.Get("ses_beat")
		return err == nil && entry.Title == "scanned"
	})

	apply := held.holdApply(t, "ses_beat")
	completion := held.holdCompletion(t, nil)
	if err := r.Rewatch(client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, apply.reached, "the read to deliver ses_beat")
	if err := r.Put("ses_beat", SessionEntry{Port: 1, MachineID: "m1", Title: "heartbeat"}); err != nil {
		t.Fatalf("heartbeat during the read: %v", err)
	}
	apply.open()
	waitClosed(t, completion.reached, "the read to deliver every record")
	if got := titleOf(t, r, "ses_beat"); got != "heartbeat" {
		t.Fatalf("during the read ses_beat = %q, want the heartbeat's value over the older one the read delivered", got)
	}
	completion.open()
	waitClosed(t, completion.done, "the read's sweep")
	if got := titleOf(t, r, "ses_beat"); got != "heartbeat" {
		t.Fatalf("after the read ses_beat = %q, want the heartbeat's value", got)
	}
	if got := listed(t, r); !slices.Equal(got, []string{"ses_beat"}) {
		t.Fatalf("listed after the read = %v, want ses_beat kept", got)
	}
}

// A session registered while the read is under way, after the read delivered every record, is in
// the bucket: the sweep keeps it, before the watcher delivers its record.
func TestASessionRegisteredDuringTheReadIsKeptThroughTheSweep(t *testing.T) {
	client := setupNATS(t)
	r, held := holdingRegistry(t, client)
	if err := r.Put("ses_before", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var swept []string
	completion := held.holdCompletion(t, func() { swept = sweptList(r) })
	if err := r.Rewatch(client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, completion.reached, "the read to deliver every record")
	if err := r.Put("ses_during", SessionEntry{Port: 2, MachineID: "m1"}); err != nil {
		t.Fatalf("register during the read: %v", err)
	}
	completion.open()
	waitClosed(t, completion.done, "the read's sweep")
	if want := []string{"ses_before", "ses_during"}; !slices.Equal(swept, want) {
		t.Fatalf("listed right after the sweep = %v, want %v", swept, want)
	}
	waitFor(t, 5*time.Second, r.watcher.Following)
	if got, want := listed(t, r), []string{"ses_before", "ses_during"}; !slices.Equal(got, want) {
		t.Fatalf("listed after the read = %v, want %v", got, want)
	}
}

// A session deleted while the read is under way, after the read delivered its record, is gone
// once the read completes, and nothing brings it back.
func TestASessionDeletedDuringTheReadStaysGone(t *testing.T) {
	client := setupNATS(t)
	r, held := holdingRegistry(t, client)
	for _, sessionID := range []string{"ses_gone", "ses_kept"} {
		if err := r.Put(sessionID, SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
			t.Fatalf("Put %s: %v", sessionID, err)
		}
	}
	var swept []string
	completion := held.holdCompletion(t, func() { swept = sweptList(r) })
	if err := r.Rewatch(client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, completion.reached, "the read to deliver every record")
	if err := r.Delete("ses_gone"); err != nil {
		t.Fatalf("Delete during the read: %v", err)
	}
	completion.open()
	waitClosed(t, completion.done, "the read's sweep")
	if want := []string{"ses_kept"}; !slices.Equal(swept, want) {
		t.Fatalf("listed right after the sweep = %v, want %v", swept, want)
	}
	waitFor(t, 5*time.Second, r.watcher.Following)
	if _, err := r.Refresh(context.Background(), "ses_gone"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Refresh(ses_gone) after the read = %v, want not found", err)
	}
	if got, want := listed(t, r), []string{"ses_kept"}; !slices.Equal(got, want) {
		t.Fatalf("listed after the read = %v, want %v", got, want)
	}
}

// delayingKV is a session bucket handle whose next Put or Get of a key a test delays goes to the
// bucket at once and answers only once the test opens its gate, so the answer arrives after
// whatever the test does in between.
type delayingKV struct {
	natsgo.KeyValue
	mu         sync.Mutex
	puts, gets map[string]*gate
}

func (kv *delayingKV) delay(t *testing.T, calls *map[string]*gate, key string) *gate {
	g := newGate(t)
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if *calls == nil {
		*calls = map[string]*gate{}
	}
	(*calls)[key] = g
	return g
}

func (kv *delayingKV) take(calls map[string]*gate, key string) *gate {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	g := calls[key]
	delete(calls, key)
	return g
}

func (kv *delayingKV) Put(key string, value []byte) (uint64, error) {
	revision, err := kv.KeyValue.Put(key, value)
	if g := kv.take(kv.puts, key); g != nil {
		g.pass(func() {})
	}
	return revision, err
}

func (kv *delayingKV) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	if g := kv.take(kv.gets, key); g != nil {
		g.pass(func() {})
	}
	return entry, err
}

// A heartbeat or a bucket read that a recreate of the session bucket overtakes is the old
// bucket's: its result reaches the cache after the new bucket's read has completed, and is
// dropped. Applied, its revision would fence out the new bucket's revisions, which number from 1
// again, and the copy would keep the old value past every later write to the new bucket.
func TestAResultFromABucketSinceRecreatedIsDropped(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay func(t *testing.T, kv *delayingKV) *gate
		call  func(r *SessionRegistry) error
	}{
		{
			name:  "a heartbeat",
			delay: func(t *testing.T, kv *delayingKV) *gate { return kv.delay(t, &kv.puts, "ses_a") },
			call: func(r *SessionRegistry) error {
				return r.Put("ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "old"})
			},
		},
		{
			name:  "a bucket read",
			delay: func(t *testing.T, kv *delayingKV) *gate { return kv.delay(t, &kv.gets, "ses_a") },
			call: func(r *SessionRegistry) error {
				_, err := r.Refresh(context.Background(), "ses_a")
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := setupNATS(t)
			r, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
			if err != nil {
				t.Fatalf("open session registry: %v", err)
			}
			t.Cleanup(r.StopWatch)
			delayed := &delayingKV{KeyValue: r.watcher.KV().KeyValue}
			useKV(t, r, delayed)
			// The old bucket's revisions run well past the new bucket's.
			for range 20 {
				putRaw(t, delayed.KeyValue, "ses_filler", SessionEntry{Port: 9, MachineID: "m1"})
			}
			if err := r.Put("ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "old"}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			held := test.delay(t, delayed)
			done := make(chan error, 1)
			go func() { done <- test.call(r) }()
			waitClosed(t, held.reached, "the old bucket to answer the delayed call")

			js, err := client.Conn.JetStream()
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
				t.Fatalf("delete the session bucket: %v", err)
			}
			recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.SessionBucket, TTL: time.Minute})
			putRaw(t, recreated, "ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "new", UpdatedAt: time.Now().UnixMilli()})
			putRaw(t, recreated, "ses_b", SessionEntry{Port: 2, MachineID: "m1", UpdatedAt: time.Now().UnixMilli()})
			if err := r.Rewatch(client.Conn); err != nil {
				t.Fatalf("Rewatch: %v", err)
			}
			waitFor(t, 5*time.Second, func() bool { return r.watcher.Epoch() == 1 && r.watcher.Following() })

			held.open()
			if err := <-done; err != nil {
				t.Fatalf("the delayed call: %v", err)
			}
			if got := titleOf(t, r, "ses_a"); got != "new" {
				t.Fatalf("ses_a = %q after the old bucket's answer, want the new bucket's", got)
			}
			if got, want := listed(t, r), []string{"ses_a", "ses_b"}; !slices.Equal(got, want) {
				t.Fatalf("listed = %v, want the new bucket's sessions %v", got, want)
			}
			putRaw(t, recreated, "ses_a", SessionEntry{Port: 1, MachineID: "m1", Title: "newer", UpdatedAt: time.Now().UnixMilli()})
			waitFor(t, 5*time.Second, func() bool {
				entry, err := r.Get("ses_a")
				return err == nil && entry.Title == "newer"
			})
		})
	}
}

// clock is the registry's injected clock: the real time moved on by what the test advanced it, so
// the creation times the server stamps on records and the registry's own time agree until the test
// moves past a session's TTL without waiting it out.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// While a read is under way the copy cannot see the heartbeats that renewed a session, so a row
// whose TTL passes then stays listed; to one session's caller it is gone, as an expired row always
// is. Once the read completes and the copy follows the bucket again, the row is pruned.
func TestARowThatExpiresDuringAReadStaysListedUntilTheReadCompletes(t *testing.T) {
	client := setupNATS(t)
	now := &clock{}
	r, held := holdingRegistry(t, client, WithSessionClock(now.Now))
	if err := r.Put("ses_old", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	heartbeat, err := r.Get("ses_old")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	completion := held.holdCompletion(t, nil)
	if err := r.Rewatch(client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitClosed(t, completion.reached, "the read to deliver every record")
	now.advance(2 * time.Minute)
	if got := listed(t, r); !slices.Equal(got, []string{"ses_old"}) {
		t.Fatalf("listed during the read = %v, want the expired row kept", got)
	}
	if _, err := r.Get("ses_old"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(ses_old) during the read = %v, want not found", err)
	}
	if got := r.LastSeen("ses_old"); got != heartbeat.UpdatedAt {
		t.Fatalf("LastSeen(ses_old) during the read = %d, want its last heartbeat %d", got, heartbeat.UpdatedAt)
	}

	completion.open()
	waitClosed(t, completion.done, "the read's sweep")
	waitFor(t, 5*time.Second, r.watcher.Following)
	if got := listed(t, r); len(got) != 0 {
		t.Fatalf("listed after the read = %v, want the expired row pruned", got)
	}
	if got := r.LastSeen("ses_old"); got != heartbeat.UpdatedAt {
		t.Fatalf("LastSeen(ses_old) after the read = %d, want its last heartbeat %d", got, heartbeat.UpdatedAt)
	}
}

// A session bucket recreated with another TTL expires its rows by its own: the read of the new
// bucket adopts its MaxAge.
func TestABucketRecreatedWithAnotherTTLMovesTheExpiry(t *testing.T) {
	client := setupNATS(t)
	now := &clock{}
	r, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute), WithSessionClock(now.Now))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	waitFor(t, 5*time.Second, r.watcher.Following)

	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.SessionBucket, TTL: 3 * time.Minute})
	putRaw(t, recreated, "ses_long", SessionEntry{Port: 1, MachineID: "m1", UpdatedAt: time.Now().UnixMilli()})
	if err := r.Rewatch(client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return r.watcher.Epoch() == 1 && r.watcher.Following() })
	if got := r.TTL(); got != 3*time.Minute {
		t.Fatalf("TTL after the recreate = %s, want the new bucket's 3m", got)
	}

	now.advance(2 * time.Minute)
	if _, err := r.Get("ses_long"); err != nil {
		t.Fatalf("Get(ses_long) 2m after its write = %v, want it live under the new bucket's 3m TTL", err)
	}
	now.advance(2 * time.Minute)
	if _, err := r.Get("ses_long"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(ses_long) 4m after its write = %v, want it expired", err)
	}
}
