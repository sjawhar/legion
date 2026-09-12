package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/routing"
)

const Bucket = "envoy_interests"
const RoleBucket = "envoy_roles"

// RoleClaim is the durable ownership record for a role lane. Role claims are
// independent of interest records: a listener restart can temporarily age an
// interest out before its still-running holder re-registers.
type RoleClaim struct {
	HolderSessionID   string `json:"holder_session_id"`
	ClaimedAt         int64  `json:"claimed_at"`
	PreviousSessionID string `json:"previous_session_id"`
}

type Registry struct {
	kv                    nats.KeyValue
	roleKV                nats.KeyValue
	openedAt              time.Time
	restoredRoleRevisions map[string]uint64
	mu                    sync.RWMutex
	cache                 map[string]Interest
	// cacheRevisions holds the latest KV revision applied for each cache key,
	// including delete tombstones. It prevents a delayed local write-through
	// from replacing a newer watcher update.
	cacheRevisions map[string]uint64
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
	restoredRoleRevisions, err := roleRevisions(roleKV)
	if err != nil {
		return nil, err
	}
	r := &Registry{
		kv:                    kv,
		roleKV:                roleKV,
		cache:                 map[string]Interest{},
		cacheRevisions:        map[string]uint64{},
		readyCh:               make(chan struct{}),
		openedAt:              time.Now(),
		restoredRoleRevisions: restoredRoleRevisions,
	}
	// Skip eager load — watch() populates cache asynchronously via KV watcher.
	// The synchronous load() did N individual kv.Get() calls that block indefinitely
	// when the KV stream leader is on a remote node.
	go r.watch()
	return r, nil
}

// Ping verifies both KV buckets are reachable through the current NATS
// connection. /healthz and the listener monitor use failures to report an
// unavailable dependency while NATS reconnects.
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

func roleRevisions(kv nats.KeyValue) (map[string]uint64, error) {
	revisions := map[string]uint64{}
	keys, err := kv.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return revisions, nil
	}
	if err != nil {
		return nil, err
	}
	for _, role := range keys {
		entry, err := kv.Get(role)
		if errors.Is(err, nats.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		revisions[role] = entry.Revision()
	}
	return revisions, nil
}

func (r *Registry) cachedRevision(sessionID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheRevisions[sessionID]
}

func (r *Registry) cacheInterestLocked(sessionID string, item Interest, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	if r.cache == nil {
		r.cache = map[string]Interest{}
	}
	if r.cacheRevisions == nil {
		r.cacheRevisions = map[string]uint64{}
	}
	r.cache[sessionID] = item
	r.cacheRevisions[sessionID] = revision
}

func (r *Registry) evictCachedInterestLocked(sessionID string, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	delete(r.cache, sessionID)
	if r.cacheRevisions == nil {
		r.cacheRevisions = map[string]uint64{}
	}
	r.cacheRevisions[sessionID] = revision
}

func (r *Registry) deleteInterest(sessionID string) error {
	revision := r.cachedRevision(sessionID)
	entry, err := r.kv.Get(sessionID)
	deleteOpts := []nats.DeleteOpt{}
	if err == nil {
		if entry.Revision() > revision {
			revision = entry.Revision()
		}
		deleteOpts = append(deleteOpts, nats.LastRevision(entry.Revision()))
	} else if !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	if err := r.kv.Delete(sessionID, deleteOpts...); err != nil {
		return err
	}

	entries, err := r.kv.History(sessionID)
	if err == nil && len(entries) > 0 {
		latest := entries[len(entries)-1]
		if latest.Operation() == nats.KeyValueDelete || latest.Operation() == nats.KeyValuePurge {
			r.mu.Lock()
			r.evictCachedInterestLocked(sessionID, latest.Revision())
			r.mu.Unlock()
			return nil
		}

		var item Interest
		if err := json.Unmarshal(latest.Value(), &item); err != nil {
			r.mu.Lock()
			r.evictCachedInterestLocked(sessionID, latest.Revision())
			r.mu.Unlock()
			slog.Warn("registry cache evicted malformed value",
				slog.String("key", latest.Key()),
				slog.Uint64("revision", latest.Revision()),
				slog.String("error", err.Error()),
			)
			return nil
		}
		r.mu.Lock()
		r.cacheInterestLocked(sessionID, item, latest.Revision())
		r.mu.Unlock()
		return nil
	}

	// KeyValue.Delete does not return its revision. A successful delete must
	// still fence off any watcher update from the preceding revision while the
	// delete marker remains pending.
	r.mu.Lock()
	r.evictCachedInterestLocked(sessionID, revision+1)
	r.mu.Unlock()
	return nil
}

func (r *Registry) watch() {
	w, err := r.kv.WatchAll()
	if err != nil {
		slog.Error("registry watch failed", slog.String("error", err.Error()))
		// Unblock callers of WaitForCacheReady even on watcher failure — they'd
		// rather see the empty-cache symptom than hang. /healthz then exposes
		// the unavailable registry while NATS retries its connection.
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
		if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
			r.mu.Lock()
			r.evictCachedInterestLocked(entry.Key(), entry.Revision())
			r.mu.Unlock()
			continue
		}

		var item Interest
		if err := json.Unmarshal(entry.Value(), &item); err != nil {
			r.mu.Lock()
			r.evictCachedInterestLocked(entry.Key(), entry.Revision())
			r.mu.Unlock()
			slog.Warn("registry watcher evicted malformed value",
				slog.String("key", entry.Key()),
				slog.Uint64("revision", entry.Revision()),
				slog.String("error", err.Error()),
			)
			continue
		}
		r.mu.Lock()
		r.cacheInterestLocked(entry.Key(), item, entry.Revision())
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
// Callers should set a bounded timeout: WatchAll() on a healthy cluster
// completes in milliseconds, but the watcher may legitimately fail to start
// (e.g., bucket misconfigured). The caller logs the failure and serves the
// write-through path; /healthz exposes the unavailable registry.
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
	revision, err := r.kv.Put(merged.SessionID, buf)
	if err != nil {
		return Interest{}, err
	}
	r.mu.Lock()
	r.cacheInterestLocked(merged.SessionID, merged, revision)
	r.mu.Unlock()
	return merged, nil
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

func decodeRoleClaim(value []byte) (RoleClaim, error) {
	var claim RoleClaim
	err := json.Unmarshal(value, &claim)
	if err == nil {
		if strings.TrimSpace(claim.HolderSessionID) == "" {
			return RoleClaim{}, fmt.Errorf("role claim has no holder")
		}
		return claim, nil
	}
	if json.Valid(value) {
		return RoleClaim{}, fmt.Errorf("decode role claim: %w", err)
	}
	// Accept bare session-ID records so persisted claims remain routable;
	// a later claim normalizes the row to the structured format.
	holder := strings.TrimSpace(string(value))
	if holder == "" {
		return RoleClaim{}, fmt.Errorf("decode legacy role claim: empty holder")
	}
	return RoleClaim{HolderSessionID: holder}, nil
}

func (r *Registry) roleClaim(role string) (RoleClaim, nats.KeyValueEntry, error) {
	entry, err := r.roleKV.Get(role)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return RoleClaim{}, nil, nil
	}
	if err != nil {
		return RoleClaim{}, nil, err
	}
	claim, err := decodeRoleClaim(entry.Value())
	if err != nil {
		return RoleClaim{}, nil, err
	}
	return claim, entry, nil
}

func (r *Registry) releaseRoleClaims(sessionID string, topics []string) error {
	for _, topic := range topics {
		if !strings.HasPrefix(topic, contracts.RoleTopicPrefix) {
			continue
		}
		role := strings.TrimPrefix(topic, contracts.RoleTopicPrefix)
		if role == "" || strings.ContainsAny(role, "*>") {
			continue
		}
		if err := r.releaseRoleClaim(sessionID, role); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) releaseRoleClaim(sessionID, role string) error {
	claim, entry, err := r.roleClaim(role)
	if err != nil {
		return err
	}
	if entry == nil || claim.HolderSessionID != sessionID {
		return nil
	}
	err = r.roleKV.Delete(role, nats.LastRevision(entry.Revision()))
	if err != nil && !errors.Is(err, nats.ErrKeyExists) && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ReleaseExpiredRoleClaim drops role only when sessionID remains its holder and
// a restored claim has exhausted the session registry's TTL. A listener restart
// therefore gives an existing holder one TTL to register again, while an
// already-expired holder cannot block a new claimant forever.
func (r *Registry) ReleaseExpiredRoleClaim(role, sessionID string, sessionTTL time.Duration) (bool, error) {
	claim, entry, err := r.roleClaim(role)
	if err != nil {
		return false, err
	}
	if entry == nil || claim.HolderSessionID != sessionID {
		return false, nil
	}
	r.mu.RLock()
	restoredRevision, restored := r.restoredRoleRevisions[role]
	r.mu.RUnlock()
	if restored && restoredRevision == entry.Revision() && sessionTTL > 0 && time.Since(r.openedAt) < sessionTTL {
		return false, nil
	}
	err = r.roleKV.Delete(role, nats.LastRevision(entry.Revision()))
	if err != nil && !errors.Is(err, nats.ErrKeyExists) && !errors.Is(err, nats.ErrKeyNotFound) {
		return false, err
	}
	return true, nil
}

func (r *Registry) releaseAllRoleClaims(sessionID string) error {
	roles, err := r.roleKV.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, role := range roles {
		if err := r.releaseRoleClaim(sessionID, role); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) Remove(sessionID string, topics []string) error {
	if len(topics) == 0 {
		item, err := r.Get(sessionID)
		if err == nil {
			if err := r.releaseRoleClaims(sessionID, item.Topics); err != nil {
				return err
			}
		} else if !errors.Is(err, nats.ErrKeyNotFound) {
			return err
		}
		if err := r.releaseAllRoleClaims(sessionID); err != nil {
			return err
		}
	} else if err := r.releaseRoleClaims(sessionID, topics); err != nil {
		return err
	}
	return r.removeInterestTopics(sessionID, topics)
}

func (r *Registry) removeInterestTopics(sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry).
	if len(topics) == 0 {
		return r.deleteInterest(sessionID)
	}
	item, err := r.Get(sessionID)
	if err != nil {
		return err
	}
	item = Remove(item, topics)
	if len(item.Topics) == 0 {
		return r.deleteInterest(sessionID)
	}
	item.UpdatedAt = time.Now().UnixMilli()
	buf, err := json.Marshal(item)
	if err != nil {
		return err
	}
	revision, err := r.kv.Put(sessionID, buf)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cacheInterestLocked(sessionID, item, revision)
	r.mu.Unlock()
	return nil
}

// ErrRoleHeld reports a soft claim that found the role held by another session.
// The holder is carried on the error so callers can report who has it.
type ErrRoleHeld struct {
	Role   string
	Holder string
}

func (e *ErrRoleHeld) Error() string {
	return fmt.Sprintf("role %s is held by %s", e.Role, e.Holder)
}

// SetRole makes sessionID the holder of role. A hard claim (soft=false) is
// last-claim-wins: it takes the role from whoever holds it. A soft claim
// succeeds only if the role is unheld, already held by sessionID, or held by
// a session listed in supersedable; any other holder returns *ErrRoleHeld
// and leaves the claim untouched. Callers decide what "supersedable" means —
// the listener passes the ids it has established are no longer live, so a
// resumed session can recover a role its dead predecessor held without ever
// taking one from a live peer.
func (r *Registry) SetRole(sessionID, machineID, role string, soft bool, supersedable ...string) (Interest, error) {
	return r.SetRoleWithPrevious(sessionID, machineID, role, "", soft, supersedable...)
}

// SetRoleWithPrevious records the session ID that a successful soft claim
// continues. The predecessor is provenance only after the listener has
// evaluated the existing soft-claim rules.
func (r *Registry) SetRoleWithPrevious(sessionID, machineID, role, previousSessionID string, soft bool, supersedable ...string) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + role
	oldClaim, entry, err := r.roleClaim(role)
	if err != nil {
		return Interest{}, err
	}

	oldSessionID := oldClaim.HolderSessionID
	if soft && oldSessionID != "" && oldSessionID != sessionID && !slices.Contains(supersedable, oldSessionID) {
		return Interest{}, &ErrRoleHeld{Role: role, Holder: oldSessionID}
	}

	item, err := r.Upsert(Interest{SessionID: sessionID, MachineID: machineID}, []string{roleTopic})
	if err != nil {
		return Interest{}, err
	}

	claim := RoleClaim{
		HolderSessionID:   sessionID,
		ClaimedAt:         time.Now().UnixMilli(),
		PreviousSessionID: "",
	}
	if soft {
		claim.PreviousSessionID = previousSessionID
	}
	data, err := json.Marshal(claim)
	if err != nil {
		return Interest{}, err
	}
	if entry == nil {
		_, err = r.roleKV.Create(role, data)
	} else {
		_, err = r.roleKV.Update(role, data, entry.Revision())
	}
	if err != nil {
		holder, holderErr := r.RoleHolder(role)
		if holderErr == nil && holder == sessionID {
			return item, nil
		}
		if rollbackErr := r.removeInterestTopics(sessionID, []string{roleTopic}); rollbackErr != nil {
			return Interest{}, errors.Join(err, holderErr, rollbackErr)
		}
		if holderErr != nil {
			return Interest{}, errors.Join(err, holderErr)
		}
		// A soft claim that lost the CAS race lost to a concurrent claimant;
		// report that as held rather than as a storage failure.
		if soft && holder != "" {
			return Interest{}, &ErrRoleHeld{Role: role, Holder: holder}
		}
		return Interest{}, err
	}

	if oldSessionID != "" && oldSessionID != sessionID {
		if err := r.removeInterestTopics(oldSessionID, []string{roleTopic}); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
			slog.Warn("registry role claim old holder cleanup failed",
				slog.String("role", role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", sessionID),
				slog.String("error", err.Error()),
			)
			return Interest{}, err
		}
	}
	return item, nil
}

// RoleClaim returns the durable claim for role. An unclaimed role returns a
// zero-value claim without an error.
func (r *Registry) RoleClaim(role string) (RoleClaim, error) {
	claim, _, err := r.roleClaim(role)
	return claim, err
}

// RoleHolder returns the authoritative session ID for role. An unclaimed role
// returns an empty holder without an error.
func (r *Registry) RoleHolder(role string) (string, error) {
	claim, err := r.RoleClaim(role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
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
		r.cacheInterestLocked(sessionID, fresh, entry.Revision())
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
// grace window. A role claim is intentionally not an interest: it remains
// available for its holder to re-register after a listener restart, and the
// role delivery path drops it if that holder never becomes live again.
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
		if err := r.deleteInterest(sid); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// ReapRoleClaims removes claims whose holders did not re-register before the
// session TTL elapsed. It is intentionally separate from Reap: the interest
// reaper must not tear down a role during a listener restart grace window.
func (r *Registry) ReapRoleClaims(isAlive func(string) bool, sessionTTL time.Duration) (int, error) {
	roles, err := r.roleKV.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, role := range roles {
		claim, err := r.RoleClaim(role)
		if err != nil {
			return 0, err
		}
		if claim.HolderSessionID == "" || isAlive(claim.HolderSessionID) {
			continue
		}
		removed, err := r.ReleaseExpiredRoleClaim(role, claim.HolderSessionID, sessionTTL)
		if err != nil {
			return 0, err
		}
		if removed {
			reaped++
		}
	}
	return reaped, nil
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

// StartRoleClaimReaper expires role holders that fail to re-register after a
// listener restart. It is independent of interest reaping because a role is a
// point-to-point route rather than a general subscription.
func (r *Registry) StartRoleClaimReaper(isAlive func(string) bool, interval, sessionTTL time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			count, err := r.ReapRoleClaims(isAlive, sessionTTL)
			if err != nil {
				slog.Error("role claim reaper cycle failed", slog.String("error", err.Error()))
				continue
			}
			slog.Info("role claim reaper cycle", slog.Int("reaped", count))
		}
	}()
}
