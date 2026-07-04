// Package cistore holds the ingest-side aggregation of GitHub check_run events.
//
// Each check_run webhook folds into a per-commit State record in a JetStream KV
// bucket via compare-and-swap, instead of being published raw to pr.<n>.ci. A
// reconcile ticker (see loop.go) emits one rendered, debounced summary per
// commit once its checks have been quiet for the debounce window. All
// coordination state lives in KV so the aggregation is durable, restart-safe,
// and correct across multiple listener replicas; the only in-memory state is a
// rebuildable WatchAll read-cache.
package cistore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Bucket is the JetStream KV bucket name for per-commit CI state.
const Bucket = "envoy_ci_state"

// recordBudget bounds how long Record retries its compare-and-swap loop under
// contention. Concurrent writers (parallel webhook handlers, multiple replicas)
// racing on the same commit conflict on the KV revision; each backs off with
// jitter before retrying, so a bounded time budget lets them serialize rather
// than a fixed attempt count that can starve when many checks for one SHA land
// at once. Kept well under the listener's 10s HTTP WriteTimeout because Record
// runs synchronously in the webhook handler and a check_run can fan out over
// several PRs sequentially. NOTE: this bounds only the retry loop; a single
// hung KV call can still block up to the JetStream MaxWait (a systemic limit of
// the legacy nats.go KV API, shared with internal/store).
const recordBudget = 2 * time.Second
const recordBackoffCap = 50 * time.Millisecond

// Check is the last-known state of a single named check for a commit.
type Check struct {
	Status     string `json:"status"`     // queued|in_progress|completed
	Conclusion string `json:"conclusion"` // success|failure|... ("" until completed)
	UpdatedAt  int64  `json:"updated_at"`
}

// State is the aggregated set of checks for one (owner, repo, PR number, head SHA).
type State struct {
	Owner        string           `json:"owner"`
	Repo         string           `json:"repo"`
	Number       string           `json:"number"`
	SHA          string           `json:"sha"`
	Checks       map[string]Check `json:"checks"`
	LastEventAt  int64            `json:"last_event_at"`
	LastEmitHash string           `json:"last_emit_hash"`
}

// keyCleaner replaces characters that are invalid in a NATS KV key. GitHub
// owner/repo/number/sha are already within the valid KV charset
// ([-/_=.a-zA-Z0-9]); this only guards against stray wildcard/space/slash chars.
// Crucially it does NOT collapse '.', so repos like `foo.bar` and `foo_bar` map
// to distinct keys (collapsing them risked a cross-repo collision).
var keyCleaner = strings.NewReplacer("*", "_", ">", "_", " ", "_", "/", "_")

// Key derives a KV-safe key from the commit identity. '.' is a legal KV key
// character (unlike in a NATS subject), so segment dots are preserved; the key
// is never parsed back (State carries the identity fields), so the dot
// separators only need to yield a unique, valid key per commit.
func Key(owner, repo, number, sha string) string {
	return keyCleaner.Replace(owner) + "." +
		keyCleaner.Replace(repo) + ".pr" +
		keyCleaner.Replace(number) + "." +
		keyCleaner.Replace(sha)
}

// Hash is a stable, order-independent fingerprint of the check set
// (name+status+conclusion). It decides whether a newly-observed state is worth
// emitting: an unchanged hash since the last emit means nothing user-visible
// changed, so the summary loop stays quiet.
func (s State) Hash() string {
	names := make([]string, 0, len(s.Checks))
	for n := range s.Checks {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		c := s.Checks[n]
		h.Write([]byte(n + "\x00" + c.Status + "\x00" + c.Conclusion + "\x01"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Store is a KV-backed CI state registry with an in-memory WatchAll read-cache.
type Store struct {
	kv        nats.KeyValue
	mu        sync.RWMutex
	cache     map[string]State
	readyCh   chan struct{}
	readyOnce sync.Once
	// watchErr is non-nil once the WatchAll watcher fails to start or its update
	// stream ends. The summary loop reads only the cache (no KV fallback), so a
	// dead watcher silently stops/staleness summaries; surfacing it via Ping lets
	// self-health restart the listener and rebuild the cache from durable KV.
	watchErr error
}

type openOpts struct {
	replicas int
	ttl      time.Duration
}

// Option configures Open.
type Option func(*openOpts)

// WithReplicas overrides the KV bucket replica count. Use 1 for single-node test NATS.
func WithReplicas(n int) Option {
	return func(o *openOpts) {
		if n > 0 {
			o.replicas = n
		}
	}
}

// WithTTL sets the per-key expiry so stale per-commit state auto-cleans.
func WithTTL(d time.Duration) Option {
	return func(o *openOpts) {
		if d > 0 {
			o.ttl = d
		}
	}
}

// Open connects (or creates) the CI-state KV bucket and starts the WatchAll
// cache. Mirrors store.Open: the cache is populated asynchronously by watch()
// so Open never blocks on per-key Gets.
func Open(nc *nats.Conn, opts ...Option) (*Store, error) {
	o := openOpts{replicas: 1, ttl: 7 * 24 * time.Hour}
	for _, f := range opts {
		f(&o)
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:   Bucket,
			Replicas: o.replicas,
			Storage:  nats.FileStorage,
			TTL:      o.ttl,
		})
	}
	if err != nil {
		return nil, err
	}
	s := &Store{kv: kv, cache: map[string]State{}, readyCh: make(chan struct{})}
	go s.watch()
	return s, nil
}

// Ping verifies the KV bucket is reachable. Used by the listener self-health watchdog.
func (s *Store) Ping() error {
	if _, err := s.kv.Status(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.watchErr
}

func (s *Store) watch() {
	w, err := s.kv.WatchAll()
	if err != nil {
		s.setWatchErr(err)
		slog.Error("cistore watch failed", slog.String("error", err.Error()))
		s.signalReady()
		return
	}
	for entry := range w.Updates() {
		if entry == nil {
			// WatchAll emits a nil sentinel once the initial scan of existing
			// keys is delivered — treat that as cache-ready.
			s.signalReady()
			continue
		}
		s.mu.Lock()
		if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
			delete(s.cache, entry.Key())
		} else {
			var st State
			if err := json.Unmarshal(entry.Value(), &st); err == nil {
				s.cache[entry.Key()] = st
			}
		}
		s.mu.Unlock()
	}
	// Updates() closed unexpectedly (e.g. conn lost). The cache will now go stale
	// with no updates; surface it via Ping so the self-health watchdog restarts
	// the listener and rebuilds the cache from durable KV.
	s.setWatchErr(errors.New("cistore: KV watcher stream closed"))
	s.signalReady()
}

func (s *Store) setWatchErr(err error) {
	s.mu.Lock()
	s.watchErr = err
	s.mu.Unlock()
}

func (s *Store) signalReady() {
	s.readyOnce.Do(func() {
		if s.readyCh != nil {
			close(s.readyCh)
		}
	})
}

// WaitForCacheReady blocks until watch() has finished its initial scan of
// existing KV entries, or until the context is cancelled.
func (s *Store) WaitForCacheReady(ctx context.Context) error {
	if s == nil || s.readyCh == nil {
		return nil
	}
	select {
	case <-s.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Record folds one check observation into the per-commit state via CAS.
// It retries on revision conflict so concurrent writers (or replicas) racing on
// the same commit never lose an update.
func (s *Store) Record(owner, repo, number, sha, checkName, status, conclusion string) error {
	key := Key(owner, repo, number, sha)
	deadline := time.Now().Add(recordBudget)
	for attempt := 0; ; attempt++ {
		entry, getErr := s.kv.Get(key)
		var st State
		var rev uint64
		switch {
		case getErr == nil:
			if err := json.Unmarshal(entry.Value(), &st); err != nil {
				return err
			}
			rev = entry.Revision()
		case errors.Is(getErr, nats.ErrKeyNotFound):
			st = State{Owner: owner, Repo: repo, Number: number, SHA: sha}
		default:
			return getErr
		}
		if st.Checks == nil {
			st.Checks = map[string]Check{}
		}
		now := time.Now().UnixMilli()
		st.Checks[checkName] = Check{Status: status, Conclusion: conclusion, UpdatedAt: now}
		st.LastEventAt = now
		buf, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if rev == 0 {
			if _, err := s.kv.Create(key, buf); err == nil {
				return nil
			} else if !errors.Is(err, nats.ErrKeyExists) {
				return err
			}
		} else {
			if _, err := s.kv.Update(key, buf, rev); err == nil {
				return nil
			} else if !isCASConflict(err) {
				return err
			}
		}
		// Conflict — another writer advanced the revision. Back off with jitter
		// so racing writers serialize instead of thundering, then retry with
		// fresh state until the budget expires.
		if time.Now().After(deadline) {
			return errors.New("cistore: record exceeded CAS budget")
		}
		time.Sleep(casBackoff(attempt))
	}
}

// casBackoff returns a full-jitter, capped-exponential backoff for CAS retries.
func casBackoff(attempt int) time.Duration {
	base := time.Millisecond << attempt
	if base <= 0 || base > recordBackoffCap {
		base = recordBackoffCap
	}
	return time.Duration(rand.Int63n(int64(base) + 1))
}

// isCASConflict reports whether err is a compare-and-swap revision conflict
// (as opposed to a real transport/storage error). Create returns ErrKeyExists
// when the key already exists; Update returns a "wrong last sequence" API error
// when the revision moved.
func isCASConflict(err error) bool {
	return errors.Is(err, nats.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")
}

// List returns a snapshot copy of the current cached states.
func (s *Store) List() []State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]State, 0, len(s.cache))
	for _, st := range s.cache {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		return Key(out[i].Owner, out[i].Repo, out[i].Number, out[i].SHA) <
			Key(out[j].Owner, out[j].Repo, out[j].Number, out[j].SHA)
	})
	return out
}

// MarkEmitted claims the right to emit the summary for `hash` on a commit, via a
// compare-and-swap that stamps LastEmitHash. It reads fresh from KV so it is
// correct even when the caller acted on a slightly-stale WatchAll cache, and
// re-validates the caller's decision against that fresh state before committing.
//
// Returns (false, nil) — not an error — when emitting would be wrong:
//   - the entry already carries this hash (already emitted);
//   - the fresh check set no longer hashes to `hash` (a Record landed after the
//     caller rendered → its summary is stale, skip it);
//   - the commit is no longer past the debounce window (that same late Record
//     reopened the quiet window → too early to emit);
//   - the revision moved under a concurrent writer (CAS conflict).
//
// These guards are what make emit-once AND debounce hold against the
// eventually-consistent read-cache: a stale/premature summary can never win the
// CAS. On success the durable state's hash still equals `hash`, so the caller's
// already-rendered summary (which depends only on the hashed check set + stable
// identity) faithfully represents what was marked.
func (s *Store) MarkEmitted(key, hash string, debounce time.Duration) (bool, error) {
	entry, err := s.kv.Get(key)
	if err != nil {
		return false, err
	}
	var st State
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		return false, err
	}
	if st.LastEmitHash == hash {
		return false, nil // already emitted this exact check set
	}
	if st.Hash() != hash {
		return false, nil // set changed since the caller rendered; that summary is stale
	}
	if time.Now().UnixMilli()-st.LastEventAt < debounce.Milliseconds() {
		return false, nil // a later event reopened the debounce window; too early
	}
	st.LastEmitHash = hash
	buf, err := json.Marshal(st)
	if err != nil {
		return false, err
	}
	if _, err := s.kv.Update(key, buf, entry.Revision()); err != nil {
		if isCASConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
