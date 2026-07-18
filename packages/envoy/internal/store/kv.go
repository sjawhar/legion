package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/routing"
)

const Bucket = "envoy_interests"
const RoleBucket = "envoy_roles"

type Registry struct {
	kv     nats.KeyValue
	roleKV nats.KeyValue
	mu     sync.RWMutex
	cache  map[string]Interest
	// readyCh is closed when watch() finishes its initial scan of existing KV
	// entries (signalled by the nil sentinel WatchAll() emits after delivering
	// the current value of each existing key). After readyCh is closed, the
	// cache is consistent with the durable KV state and Match() can answer
	// every "is this session subscribed?" question without falling through.
	readyCh   chan struct{}
	readyOnce sync.Once
}

// OpenOption configures the registry.
type OpenOption func(*openOpts)

type openOpts struct{ replicas int }

// WithReplicas overrides the KV bucket replica count. Use 1 for single-node test NATS.
func WithReplicas(n int) OpenOption {
	return func(o *openOpts) { o.replicas = n }
}

func Open(conn *nats.Conn, options ...OpenOption) (*Registry, error) {
	opts := openOpts{replicas: 1}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := openBucket(js, Bucket, opts.replicas)
	if err != nil {
		return nil, err
	}
	roleKV, err := openBucket(js, RoleBucket, opts.replicas)
	if err != nil {
		return nil, err
	}
	r := &Registry{kv: kv, roleKV: roleKV, cache: map[string]Interest{}, readyCh: make(chan struct{})}
	// Skip eager load — watch() populates cache asynchronously via KV watcher.
	// The synchronous load() did N individual kv.Get() calls that block indefinitely
	// when the KV stream leader is on a remote node.
	go r.watch()
	return r, nil
}

// Ping verifies the KV bucket is reachable via the underlying NATS connection.
// Returns nil on success. Used by /healthz so the listener can self-terminate
// (and let restart policy bring it back) when the KV-backed JetStream context
// is broken — e.g. after the NATS connection drops and only the main subject
// subscription gets re-established by the bus recovery path.
func (r *Registry) Ping() error {
	if _, err := r.kv.Status(); err != nil {
		return err
	}
	if _, err := r.roleKV.Status(); err != nil {
		return err
	}
	return nil
}

func openBucket(js nats.JetStreamContext, bucket string, replicas int) (nats.KeyValue, error) {
	kv, err := js.KeyValue(bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Replicas: replicas, Storage: nats.FileStorage})
	}
	return kv, err
}

func (r *Registry) watch() {
	w, err := r.kv.WatchAll()
	if err != nil {
		slog.Error("registry watch failed", slog.String("error", err.Error()))
		// Unblock callers of WaitForCacheReady even on watcher failure — they'd
		// rather see the empty-cache symptom than hang. Self-health watchdog
		// (added in #608) will catch a persistently broken registry.
		r.signalReady()
		return
	}
	for entry := range w.Updates() {
		if entry == nil {
			// NATS KV WatchAll() emits a nil sentinel after delivering the
			// current value of each existing key. Treat that as "initial scan
			// complete" and unblock cache-readiness gates.
			r.signalReady()
			continue
		}
		r.mu.Lock()
		if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
			delete(r.cache, entry.Key())
		} else {
			var item Interest
			if err := json.Unmarshal(entry.Value(), &item); err == nil {
				r.cache[entry.Key()] = item
			}
		}
		r.mu.Unlock()
	}
}

// signalReady closes readyCh exactly once, unblocking any callers of
// WaitForCacheReady. Safe to call from multiple code paths (success / error).
func (r *Registry) signalReady() {
	r.readyOnce.Do(func() {
		if r.readyCh != nil {
			close(r.readyCh)
		}
	})
}

// WaitForCacheReady blocks until watch() has finished its initial scan of
// existing KV entries, or until the context is cancelled. After this returns
// nil, registry.Match sees every existing subscription in the bucket.
//
// Callers should set a bounded timeout: WatchAll() on a healthy NATS cluster
// completes in milliseconds, but the watcher may legitimately fail to start
// (e.g., bucket misconfigured). The caller is responsible for deciding what to
// do with a non-nil error — typically log + proceed (fail open) so the listener
// can still answer the new-subscription path while the self-health watchdog
// arranges a restart.
func (r *Registry) WaitForCacheReady(ctx context.Context) error {
	if r == nil || r.readyCh == nil {
		return nil
	}
	select {
	case <-r.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Registry) Upsert(item Interest, topics []string) (Interest, error) {
	cur, getErr := r.Get(item.SessionID)
	merged, err := mergeForUpsert(cur, getErr, item, topics, time.Now().UnixMilli())
	if err != nil {
		return Interest{}, err
	}
	buf, err := json.Marshal(merged)
	if err != nil {
		return Interest{}, err
	}
	_, err = r.kv.Put(merged.SessionID, buf)
	return merged, err
}

// mergeForUpsert computes the Interest to persist by reconciling the requested
// item/topics with the current cached/durable state returned by r.Get.
//
// Critical invariant: a transient KV error (anything other than ErrKeyNotFound)
// MUST be returned so the caller refuses to overwrite durable state with the
// partial heartbeat payload. Silently treating transient errors as "no prior
// state" is what caused Atlas's pr.11416.> subscription to disappear: when r.Get
// failed transiently during a 2-minute heartbeat, the original implementation
// merged only the agent topic and Put a truncated state to KV, which then
// propagated to the in-memory cache via watch() and made all subsequent github
// events fall through registry.Match with "no matching interests".
//
// ErrKeyNotFound is the genuinely-new-session case — proceed with the passed-in
// item and topics.
func mergeForUpsert(cur Interest, getErr error, item Interest, topics []string, now int64) (Interest, error) {
	if getErr != nil && !errors.Is(getErr, nats.ErrKeyNotFound) {
		return Interest{}, getErr
	}
	if getErr == nil {
		item = cur
	}
	item.MachineID = first(item.MachineID, curValue(cur.MachineID))
	item.Dir = first(item.Dir, curValue(cur.Dir))
	item.UpdatedAt = now
	item = Merge(item, topics)
	sort.Strings(item.Topics)
	return item, nil
}

func (r *Registry) Remove(sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry)
	if len(topics) == 0 {
		return r.kv.Delete(sessionID)
	}
	item, err := r.Get(sessionID)
	if err != nil {
		return err
	}
	item = Remove(item, topics)
	if len(item.Topics) == 0 {
		return r.kv.Delete(sessionID)
	}
	item.UpdatedAt = time.Now().UnixMilli()
	buf, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = r.kv.Put(sessionID, buf)
	return err
}

func (r *Registry) SetRole(sessionID, machineID, role string) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + role
	entry, err := r.roleKV.Get(role)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return Interest{}, err
	}
	if err == nil {
		oldSessionID := string(entry.Value())
		if oldSessionID != "" && oldSessionID != sessionID {
			if removeErr := r.Remove(oldSessionID, []string{roleTopic}); removeErr != nil && !errors.Is(removeErr, nats.ErrKeyNotFound) {
				return Interest{}, removeErr
			}
		}
	}

	item, err := r.Upsert(Interest{SessionID: sessionID, MachineID: machineID}, []string{roleTopic})
	if err != nil {
		return Interest{}, err
	}
	if _, err := r.roleKV.Put(role, []byte(sessionID)); err != nil {
		return Interest{}, err
	}
	return item, nil
}

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache.
func (r *Registry) Get(sessionID string) (Interest, error) {
	r.mu.RLock()
	item, ok := r.cache[sessionID]
	r.mu.RUnlock()
	if ok {
		return item, nil
	}
	entry, err := r.kv.Get(sessionID)
	if err != nil {
		return Interest{}, err
	}
	var fresh Interest
	err = json.Unmarshal(entry.Value(), &fresh)
	if err == nil {
		r.mu.Lock()
		r.cache[sessionID] = fresh
		r.mu.Unlock()
	}
	return fresh, err
}

func (r *Registry) Match(machineID string, topic string) []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Interest{}
	for _, item := range r.cache {
		if item.MachineID != machineID {
			continue
		}
		for _, pattern := range item.Topics {
			if routing.Match(pattern, topic) {
				out = append(out, item)
				break
			}
		}
	}
	return out
}

// List returns all cached interests sorted by SessionID for deterministic output.
func (r *Registry) List() []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Interest, 0, len(r.cache))
	for _, item := range r.cache {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

func first(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func curValue(value string) string {
	return value
}

// Reap cross-references the interest cache with a session liveness check and
// deletes interests whose sessions are dead AND whose UpdatedAt exceeds the
// grace window. The isAlive function should return true when the session exists.
// Returns the number of reaped interests.
func (r *Registry) Reap(isAlive func(string) bool, graceWindow time.Duration) (int, error) {
	now := time.Now().UnixMilli()
	graceMs := graceWindow.Milliseconds()

	r.mu.RLock()
	var stale []string
	for sid, item := range r.cache {
		if isAlive(sid) {
			continue // session still alive
		}
		if now-item.UpdatedAt <= graceMs {
			continue // within grace window
		}
		stale = append(stale, sid)
	}
	r.mu.RUnlock()

	for _, sid := range stale {
		if err := r.kv.Delete(sid); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// StartReaper runs Reap in a background goroutine at the given interval.
func (r *Registry) StartReaper(isAlive func(string) bool, interval, graceWindow time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			count, err := r.Reap(isAlive, graceWindow)
			if err != nil {
				slog.Error("reaper cycle failed", slog.String("error", err.Error()))
				continue
			}
			slog.Info("reaper cycle", slog.Int("reaped", count))
		}
	}()
}
