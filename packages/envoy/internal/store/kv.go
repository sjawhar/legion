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
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
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

// ExpiredRoleClaimRelease reports the conditional-delete outcome for an
// expired role claim.
type ExpiredRoleClaimRelease uint8

const (
	ExpiredRoleClaimReleased ExpiredRoleClaimRelease = iota
	ExpiredRoleClaimMissing
	ExpiredRoleClaimSuperseded
	ExpiredRoleClaimRetained
)

type Registry struct {
	// kvMu guards roleKV, which Rewatch moves to a replacement connection. The interest bucket's
	// handle is the watcher's (kvwatch.Watcher.KV).
	kvMu                  sync.RWMutex
	roleKV                bus.KeyValue
	log                   *slog.Logger
	now                   func() time.Time
	openedAt              time.Time
	restoredRoleRevisions map[string]uint64
	mu                    sync.RWMutex
	cache                 map[string]Interest
	// cacheRevisions holds the latest KV revision applied for each cache key,
	// including delete tombstones. It prevents a delayed local write-through
	// from replacing a newer watcher update.
	cacheRevisions map[string]uint64
	// watcher feeds the cache from the interest bucket. Once it is ready the cache is consistent
	// with the bucket, and Match answers every "is this session subscribed?" question without
	// falling through.
	watcher *kvwatch.Watcher
}

// OpenOption configures the registry.
type OpenOption func(*openOpts)

type openOpts struct {
	replicas       int
	interestBucket string
	roleBucket     string
	now            func() time.Time
	log            *slog.Logger
}

// WithLogger sets the logger the registry writes through. The listener passes its own, so the
// registry's lines are the JSON records with machine_id that the rest of its output is; the default
// is slog.Default(), which is what every other caller and the tests use. The listener must not set
// that default instead: it also routes the stdlib log package's output into the same handler, and
// the deployed CloudWatch metric filters for publish failures, webhook refusals and dropped stream
// subjects are space-delimited text patterns anchored on that package's date and time prefix
// (agent-c meta/infra/pulumi/components/envoy/listener.py), so JSON there silently stops three
// alarms. No deployed pattern matches a line from this package.
func WithLogger(log *slog.Logger) OpenOption {
	return func(o *openOpts) { o.log = log }
}

// WithReplicas overrides the KV bucket replica count. Use 1 for single-node test NATS.
func WithReplicas(n int) OpenOption {
	return func(o *openOpts) { o.replicas = n }
}

// WithClock sets the registry's clock: the time every interest and role claim records, the reaper's
// staleness window, and a restored role claim's grace window, which starts when Open returns. Tests
// move it to end a window without waiting for it.
func WithClock(now func() time.Time) OpenOption {
	return func(o *openOpts) { o.now = now }
}

func Open(conn *nats.Conn, options ...OpenOption) (*Registry, error) {
	opts := openOpts{replicas: 1, interestBucket: Bucket, roleBucket: RoleBucket, now: time.Now, log: slog.Default()}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: opts.interestBucket, Replicas: opts.replicas, Storage: nats.FileStorage})
	if err != nil {
		return nil, err
	}
	roleKV, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: opts.roleBucket, Replicas: opts.replicas, Storage: nats.FileStorage})
	if err != nil {
		return nil, err
	}
	restoredRoleRevisions, err := roleRevisions(roleKV, opts.log)
	if err != nil {
		return nil, err
	}
	r := &Registry{
		roleKV:                roleKV,
		log:                   opts.log,
		cache:                 map[string]Interest{},
		cacheRevisions:        map[string]uint64{},
		now:                   opts.now,
		openedAt:              opts.now(),
		restoredRoleRevisions: restoredRoleRevisions,
	}
	r.watcher = kvwatch.New("interest registry", kv, r.applyWatched, r.resetCache)
	// No eager load: the watcher populates the cache asynchronously. A synchronous load of N
	// individual kv.Get() calls blocks indefinitely when the KV stream leader is on a remote node.
	r.watcher.Start()
	return r, nil
}

// Ping verifies both KV buckets are reachable through the current NATS
// connection and reports the cache watcher's terminal error, including an
// interest bucket recreated under it (kvwatch.Watcher.Check). /healthz and the
// listener monitor use failures to report an unavailable dependency while NATS
// reconnects.
func (r *Registry) Ping() error {
	if err := r.watcher.Check(); err != nil {
		return err
	}
	if _, err := r.roles().Status(); err != nil {
		return err
	}
	return r.watcher.Err()
}

// WatchErr is the cache watcher's terminal error, or nil while it runs, so the listener can tell a
// rebuildable dead cache from a transient KV timeout.
func (r *Registry) WatchErr() error {
	return r.watcher.Err()
}

// StopWatch retires the cache's watcher for a shutdown (kvwatch.Watcher.Stop).
func (r *Registry) StopWatch() {
	r.watcher.Stop()
}

func (r *Registry) interests() bus.KeyValue {
	return r.watcher.KV()
}

// logger is the logger Open was given, or the default one. A Registry the package's own unit tests
// build as a literal has no logger, and its zero value logging where slog.Default() points is the
// same thing every caller but the listener gets.
func (r *Registry) logger() *slog.Logger {
	if r.log == nil {
		return slog.Default()
	}
	return r.log
}

func (r *Registry) roles() bus.KeyValue {
	r.kvMu.RLock()
	defer r.kvMu.RUnlock()
	return r.roleKV
}

// roleRevisions reads the revision of every role claim kv holds, for the grace a restored claim's
// holder gets to register again. It takes them from one watch over the bucket's existing keys, the
// way the interest, session and CI caches read theirs (internal/kvwatch), so readiness costs one
// pass over the bucket rather than a round trip per stored claim. That is the watch nats.go's
// kv.Keys() runs — MetaOnly over every key — which discards each entry's revision; this keeps it.
// The watch names no key, so a claim whose key this build cannot read (bus.ErrRefused: an earlier
// build stored it past what a read of it may send) gets a revision here; it still gets no grace,
// because ReleaseExpiredRoleClaim, the only reader of these revisions, reads the claim itself
// first and cannot.
//
// The scan keeps the delete markers rather than letting nats.go's IgnoreDeletes drop them, because
// it costs nothing — they are on the wire either way — and the two counts it then has are what a
// restart's log needs, as disjoint fields: restored claims, and the markers it streamed past to
// find them. They must not be reported as one total of "keys": production's 9 claims beside 766
// subjects reads as 757 claims left unrestored, which is the misreading LEGION-360 itself was
// filed on. A marker is not a claim and gets no revision.
//
// A snapshot short of the bucket is missing claims that are in it, and each one it misses is a
// restored holder that loses its grace and is released a session TTL early, so a scan that did not
// reach the end of the bucket fails the caller instead. nats.go ends a scan three ways (kv.go in
// nats.go v1.50.0): it sends a nil entry once it has delivered every existing key (:1096-1099,
// :1140-1142); its idle timer sends the same nil when no entry arrived within the JetStream
// MaxWait, after putting ErrKeyWatcherTimeout on Error() under the watcher's lock (:1145-1156), so
// the error is there to read by the time the nil arrives; and it closes the updates channel with
// no nil at all when the subscription ends first (:1170, which closes Error() at :1171 too), an
// ordered consumer it could not recreate or a closed connection. Only the first is a complete
// scan, and only it logs.
func roleRevisions(kv bus.KeyValue, log *slog.Logger) (map[string]uint64, error) {
	watcher, err := kv.Watch(nats.AllKeys, nats.MetaOnly())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := watcher.Stop(); err != nil {
			log.Warn("role registry could not stop its revision watch", slog.String("error", err.Error()))
		}
	}()
	revisions := map[string]uint64{}
	entries, markers := 0, 0
	for entry := range watcher.Updates() {
		if entry != nil {
			entries++
			if entry.Operation() == nats.KeyValuePut {
				revisions[entry.Key()] = entry.Revision()
			} else {
				markers++
			}
			continue
		}
		select {
		case err := <-watcher.Error():
			if err != nil {
				return nil, fmt.Errorf("role revisions: the bucket's watch stopped after %d entries: %w", entries, err)
			}
		default:
		}
		log.Info("restored role claims", slog.Int("restored", len(revisions)), slog.Int("delete_markers", markers))
		return revisions, nil
	}
	return nil, fmt.Errorf("role revisions: the bucket's watch ended after %d entries, before it had delivered them all", entries)
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

// deleteInterest deletes sessionID's interest at the revision it reads (bus.KeyValue.DeleteAtRead).
func (r *Registry) deleteInterest(sessionID string) error {
	revision := r.cachedRevision(sessionID)
	read, err := r.interests().DeleteAtRead(sessionID)
	if err != nil {
		return err
	}
	revision = max(revision, read)

	entries, err := r.interests().History(sessionID)
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
			r.logger().Warn("registry cache evicted malformed value",
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

// Rewatch reopens both buckets on conn and replaces the cache's watcher with one there
// (kvwatch.Watcher.Rewatch). A NATS server restart loses the watcher's ordered consumer, and
// nats.go replaces it only once it notices the missed heartbeats, up to twenty seconds later;
// until then the cache misses every write another listener makes, and a drain deletes a consumer
// the server no longer has. The listener's reconnect hook calls this, so the cache follows the
// bucket from the reconnect on.
func (r *Registry) Rewatch(conn *nats.Conn) error {
	// The role bucket opens first, so a failure leaves both handles on the previous connection
	// rather than the interests on conn and the roles behind.
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return fmt.Errorf("open role registry JetStream: %w", err)
	}
	roleKV, err := bus.OpenKeyValue(js, r.roles().Bucket())
	if err != nil {
		return fmt.Errorf("open role KV bucket: %w", err)
	}
	if err := r.watcher.Rewatch(conn); err != nil {
		return err
	}
	r.kvMu.Lock()
	r.roleKV = roleKV
	r.kvMu.Unlock()
	return nil
}

// resetCache empties the cache and its revision fence, for a recreated interest bucket.
func (r *Registry) resetCache() {
	r.mu.Lock()
	r.cache = map[string]Interest{}
	r.cacheRevisions = map[string]uint64{}
	r.mu.Unlock()
}

// applyWatched applies one entry the KV watcher delivered to the cache. The revision fence in
// cacheInterestLocked and evictCachedInterestLocked keeps a delayed local write-through and a
// watcher update from undoing each other.
func (r *Registry) applyWatched(entry nats.KeyValueEntry) {
	if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
		r.mu.Lock()
		r.evictCachedInterestLocked(entry.Key(), entry.Revision())
		r.mu.Unlock()
		return
	}

	var item Interest
	if err := json.Unmarshal(entry.Value(), &item); err != nil {
		r.mu.Lock()
		r.evictCachedInterestLocked(entry.Key(), entry.Revision())
		r.mu.Unlock()
		r.logger().Warn("registry watcher evicted malformed value",
			slog.String("key", entry.Key()),
			slog.Uint64("revision", entry.Revision()),
			slog.String("error", err.Error()),
		)
		return
	}
	r.mu.Lock()
	r.cacheInterestLocked(entry.Key(), item, entry.Revision())
	r.mu.Unlock()
}

// WaitForCacheReady blocks until the watcher has delivered every existing interest, or until ctx
// is done. After it returns nil, Match sees every existing subscription in the bucket. Callers
// bound ctx: a watcher that fails to start releases it too, and the caller then serves the
// write-through path while /healthz exposes the unavailable registry.
func (r *Registry) WaitForCacheReady(ctx context.Context) error {
	return r.watcher.WaitReady(ctx)
}

func (r *Registry) Upsert(item Interest, topics []string) (Interest, error) {
	cur, getErr := r.Get(item.SessionID)
	merged, err := mergeForUpsert(cur, getErr, item, topics, r.now().UnixMilli())
	if err != nil {
		return Interest{}, err
	}
	buf, err := json.Marshal(merged)
	if err != nil {
		return Interest{}, err
	}
	revision, err := r.interests().Put(merged.SessionID, buf)
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
	entry, err := r.roles().Get(role)
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

// releaseRoleClaim deletes role's claim when sessionID holds it. A claim this build cannot read
// (bus.ErrRefused) has a holder nothing can tell, so it is left to the role reaper, which deletes
// it.
func (r *Registry) releaseRoleClaim(sessionID, role string) error {
	claim, entry, err := r.roleClaim(role)
	if errors.Is(err, bus.ErrRefused) {
		r.logger().Warn("role registry left a stored claim it cannot read to the role reaper", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
		return nil
	}
	if err != nil {
		return err
	}
	if entry == nil || claim.HolderSessionID != sessionID {
		return nil
	}
	err = r.roles().Delete(role, nats.LastRevision(entry.Revision()))
	if err != nil && !errors.Is(err, nats.ErrKeyExists) && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ReleaseExpiredRoleClaim drops role only when sessionID remains its holder and
// a restored claim has exhausted the session registry's TTL. A listener restart
// therefore gives an existing holder one TTL to register again, while an
// already-expired holder cannot block a new claimant forever.
//
// Its result distinguishes a confirmed deletion from a concurrent removal or
// replacement, so callers never report that a lost conditional delete released
// a claim.
func (r *Registry) ReleaseExpiredRoleClaim(role, sessionID string, sessionTTL time.Duration) (ExpiredRoleClaimRelease, error) {
	claim, entry, err := r.roleClaim(role)
	if err != nil {
		return ExpiredRoleClaimRetained, err
	}
	if entry == nil {
		return ExpiredRoleClaimMissing, nil
	}
	if claim.HolderSessionID != sessionID {
		return ExpiredRoleClaimSuperseded, nil
	}
	r.mu.RLock()
	restoredRevision, restored := r.restoredRoleRevisions[role]
	r.mu.RUnlock()
	if restored && restoredRevision == entry.Revision() && sessionTTL > 0 && r.now().Sub(r.openedAt) < sessionTTL {
		return ExpiredRoleClaimRetained, nil
	}
	err = r.roles().Delete(role, nats.LastRevision(entry.Revision()))
	switch {
	case err == nil:
		return ExpiredRoleClaimReleased, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return ExpiredRoleClaimMissing, nil
	case errors.Is(err, nats.ErrKeyExists):
		return ExpiredRoleClaimSuperseded, nil
	default:
		return ExpiredRoleClaimRetained, err
	}
}

func (r *Registry) releaseAllRoleClaims(sessionID string) error {
	roles, err := r.roles().Keys()
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
	item.UpdatedAt = r.now().UnixMilli()
	buf, err := json.Marshal(item)
	if err != nil {
		return err
	}
	revision, err := r.interests().Put(sessionID, buf)
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
		ClaimedAt:         r.now().UnixMilli(),
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
		_, err = r.roles().Create(role, data)
	} else {
		_, err = r.roles().Update(role, data, entry.Revision())
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
		err := r.removeInterestTopics(oldSessionID, []string{roleTopic})
		if errors.Is(err, bus.ErrRefused) {
			// The old holder is a session an earlier build registered under an id this one cannot
			// write. The claim is written and the caller named nothing too long, so it succeeds; the
			// interest reaper removes the old holder's interest once its session is gone.
			r.logger().Warn("registry role claim left an old holder's interest it cannot write to the reaper",
				slog.String("role", role),
				slog.Int("key_bytes", len(oldSessionID)),
				slog.String("error", err.Error()),
			)
		} else if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
			r.logger().Warn("registry role claim old holder cleanup failed",
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
	entry, err := r.interests().Get(sessionID)
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
	now := r.now().UnixMilli()
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

	reaped := 0
	for _, sid := range stale {
		err := r.deleteInterest(sid)
		if errors.Is(err, bus.ErrRefused) {
			r.logger().Warn("reaper skipped an interest it cannot delete", slog.Int("key_bytes", len(sid)), slog.String("error", err.Error()))
			continue
		}
		if err != nil {
			return 0, err
		}
		reaped++
	}
	return reaped, nil
}

// ReapRoleClaims removes claims whose holders did not re-register before the
// session TTL elapsed. It is intentionally separate from Reap: the interest
// reaper must not tear down a role during a listener restart grace window. A
// claim this build cannot read (bus.ErrRefused: an earlier build stored it past
// what a read of it may send) no build can resolve or write again, so it is
// deleted too; one it cannot delete either is skipped, and the sweep goes on.
func (r *Registry) ReapRoleClaims(isAlive func(string) bool, sessionTTL time.Duration) (int, error) {
	roles, err := r.roles().Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, role := range roles {
		claim, err := r.RoleClaim(role)
		if errors.Is(err, bus.ErrRefused) {
			switch err := r.roles().Delete(role); {
			case err == nil:
				r.logger().Warn("role reaper deleted a claim it cannot read", slog.Int("key_bytes", len(role)))
				reaped++
			case errors.Is(err, bus.ErrRefused):
				r.logger().Warn("role reaper skipped a claim it cannot delete", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
			default:
				return 0, err
			}
			continue
		}
		if err != nil {
			return 0, err
		}
		if claim.HolderSessionID == "" || isAlive(claim.HolderSessionID) {
			continue
		}
		release, err := r.ReleaseExpiredRoleClaim(role, claim.HolderSessionID, sessionTTL)
		if err != nil {
			return 0, err
		}
		if release == ExpiredRoleClaimReleased {
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
				r.logger().Error("reaper cycle failed", slog.String("error", err.Error()))
				continue
			}
			r.logger().Info("reaper cycle", slog.Int("reaped", count))
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
				r.logger().Error("role claim reaper cycle failed", slog.String("error", err.Error()))
				continue
			}
			r.logger().Info("role claim reaper cycle", slog.Int("reaped", count))
		}
	}()
}
