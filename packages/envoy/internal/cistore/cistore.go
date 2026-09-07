// Package cistore holds the ingest-side aggregation of GitHub CI observations.

// Each check_run and check_suite webhook folds into a per-commit State record
// in a JetStream KV bucket via compare-and-swap rather than being published
// raw. A reconcile ticker (see loop.go) emits one checks envelope when the
// current head is quiet and its recorded CI work is complete. All coordination
// state lives in KV, so aggregation is durable, restart-safe, and correct
// across listener replicas; the only in-memory state is a rebuildable WatchAll
// read-cache.
package cistore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
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
	Name       string `json:"name,omitempty"`
	CheckRunID string `json:"check_run_id"`
	URL        string `json:"url"`
	Status     string `json:"status"`     // queued|in_progress|completed
	Conclusion string `json:"conclusion"` // success|failure|... ("" until completed)
	ObservedAt string `json:"observed_at"`
}

// Suite is the last-known state of one GitHub check suite for a commit.
type Suite struct {
	ID         string `json:"id"`
	AppID      string `json:"app_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ObservedAt string `json:"observed_at"`
}

// SettlementClaim is the durable right to publish one settlement generation.
// Claims prevent replicas from publishing the same episode concurrently.
type SettlementClaim struct {
	Hash       string `json:"hash"`
	Generation uint64 `json:"generation"`
	ClaimedAt  int64  `json:"claimed_at"`
}

// State is the aggregated set of checks and suites for one (owner, repo, PR
// number, head SHA).
type State struct {
	Owner             string           `json:"owner"`
	Repo              string           `json:"repo"`
	Number            string           `json:"number"`
	SHA               string           `json:"sha"`
	Checks            map[string]Check `json:"checks"`
	Suites            map[string]Suite `json:"suites"`
	LastEventAt       int64            `json:"last_event_at"`
	InitialGeneration uint64           `json:"initial_generation"`
	Generation        uint64           `json:"generation"`
	SettledEmitted    bool             `json:"settled_emitted"`
	Claim             *SettlementClaim `json:"claim,omitempty"`
}

// UnmarshalJSON maps the retired resettled marker to the generation that
// publishes the equivalent re-settlement envelope.
func (state *State) UnmarshalJSON(data []byte) error {
	type stateAlias State
	*state = State{}
	wire := struct {
		*stateAlias
		Resettled bool `json:"resettled"`
	}{stateAlias: (*stateAlias)(state)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Resettled && state.Generation == 0 {
		state.Generation = 1
	}
	return nil
}

const headRecordKind = "head"

var ErrInvalidHeadSHA = errors.New("cistore: invalid head SHA")

type headRecord struct {
	Kind      string `json:"kind"`
	SHA       string `json:"sha"`
	UpdatedAt string `json:"updated_at"`
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

func headKey(owner, repo, number string) string {
	return "head." + keyCleaner.Replace(owner) + "." +
		keyCleaner.Replace(repo) + "." + keyCleaner.Replace(number)
}

func validHeadSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

// Hash is a stable, order-independent fingerprint of the CI state. It includes
// check-run attempts and check-suite state so a re-run becomes a distinct
// delivery even when it resolves to the same conclusion.
func (s State) Hash() string {
	h := sha256.New()
	checkNames := make([]string, 0, len(s.Checks))
	for name := range s.Checks {
		checkNames = append(checkNames, name)
	}
	sort.Strings(checkNames)
	for _, name := range checkNames {
		check := s.Checks[name]
		h.Write([]byte("check\x00" + name + "\x00" + check.CheckRunID + "\x00" + check.Status + "\x00" + check.Conclusion + "\x01"))
	}
	suiteIDs := make([]string, 0, len(s.Suites))
	for id := range s.Suites {
		suiteIDs = append(suiteIDs, id)
	}
	sort.Strings(suiteIDs)
	for _, id := range suiteIDs {
		suite := s.Suites[id]
		h.Write([]byte("suite\x00" + id + "\x00" + suite.AppID + "\x00" + suite.Status + "\x00" + suite.Conclusion + "\x01"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type Store struct {
	kv             nats.KeyValue
	mu             sync.RWMutex
	cache          map[string]State
	heads          map[string]string
	cacheRevisions map[string]uint64
	readyCh        chan struct{}
	readyOnce      sync.Once
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
	s := &Store{
		kv:             kv,
		cache:          map[string]State{},
		heads:          map[string]string{},
		cacheRevisions: map[string]uint64{},
		readyCh:        make(chan struct{}),
	}
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

		key := entry.Key()
		var malformed error
		s.mu.Lock()
		switch {
		case entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge:
			s.evictCachedLocked(key, entry.Revision())
		default:
			var marker struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(entry.Value(), &marker); err == nil && marker.Kind == headRecordKind {
				var head headRecord
				if err := json.Unmarshal(entry.Value(), &head); err != nil {
					s.evictCachedLocked(key, entry.Revision())
					malformed = err
				} else if !validHeadSHA(head.SHA) {
					s.evictCachedLocked(key, entry.Revision())
					malformed = errors.New("invalid head SHA")
				} else {
					s.cacheHeadLocked(key, head.SHA, entry.Revision())
				}
			} else {
				var st State
				if err := json.Unmarshal(entry.Value(), &st); err != nil {
					s.evictCachedLocked(key, entry.Revision())
					malformed = err
				} else {
					s.cacheStateLocked(key, st, entry.Revision())
				}
			}
		}
		s.mu.Unlock()

		if malformed != nil {
			slog.Warn("cistore watch evicted malformed value",
				slog.String("key", key),
				slog.Uint64("revision", entry.Revision()),
				slog.String("error", malformed.Error()),
			)
		}
	}
	// Updates() closed unexpectedly (e.g. conn lost). The cache will now go stale
	// with no updates; surface it via Ping so the self-health watchdog restarts
	// the listener and rebuilds the cache from durable KV.
	s.setWatchErr(errors.New("cistore: KV watcher stream closed"))
	s.signalReady()
}

func (s *Store) cacheStateLocked(key string, state State, revision uint64) {
	if revision < s.cacheRevisions[key] {
		return
	}
	s.cache[key] = state
	delete(s.heads, key)
	s.cacheRevisions[key] = revision
}

func (s *Store) cacheHeadLocked(key, sha string, revision uint64) {
	if revision < s.cacheRevisions[key] {
		return
	}
	delete(s.cache, key)
	s.heads[key] = sha
	s.cacheRevisions[key] = revision
}

func (s *Store) evictCachedLocked(key string, revision uint64) {
	if revision < s.cacheRevisions[key] {
		return
	}
	delete(s.cache, key)
	delete(s.heads, key)
	s.cacheRevisions[key] = revision
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
func (s *Store) Record(observation contracts.CIObservation) error {
	return s.record(observation)
}

func (s *Store) record(observation contracts.CIObservation) error {
	return s.update(observation.Owner, observation.Repo, observation.Number, observation.SHA, func(st *State) bool {
		if st.Checks == nil {
			st.Checks = map[string]Check{}
		}
		key := observation.CheckName
		if current, ok := st.Checks[key]; ok {
			if checkRunIDIsOlder(observation.CheckRunID, current.CheckRunID) ||
				(observation.CheckRunID == current.CheckRunID && !observationMayReplace(observation.ObservedAt, current.ObservedAt, observation.Status, current.Status)) {
				return false
			}
		}
		next := Check{
			Name:       observation.CheckName,
			CheckRunID: observation.CheckRunID,
			URL:        observation.URL,
			Status:     observation.Status,
			Conclusion: observation.Conclusion,
			ObservedAt: observation.ObservedAt,
		}
		if current, ok := st.Checks[key]; ok && current == next {
			return false
		}
		st.Checks[key] = next
		rearm(st)
		st.LastEventAt = time.Now().UnixMilli()
		return true
	})
}

// RecordSuite folds a check_suite observation into the per-commit state.
func (s *Store) RecordSuite(observation contracts.CIObservation) error {
	return s.update(observation.Owner, observation.Repo, observation.Number, observation.SHA, func(st *State) bool {
		if st.Suites == nil {
			st.Suites = map[string]Suite{}
		}
		next := Suite{
			ID:         observation.SuiteID,
			AppID:      observation.AppID,
			Status:     observation.Status,
			Conclusion: observation.Conclusion,
			ObservedAt: observation.ObservedAt,
		}
		if current, ok := st.Suites[observation.SuiteID]; ok {
			if !observationMayReplace(observation.ObservedAt, current.ObservedAt, observation.Status, current.Status) || current == next {
				return false
			}
		}
		st.Suites[observation.SuiteID] = next
		rearm(st)
		st.LastEventAt = time.Now().UnixMilli()
		return true
	})
}

func (s *Store) update(owner, repo, number, sha string, mutate func(*State) bool) error {
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
		if !mutate(&st) {
			return nil
		}
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
		if time.Now().After(deadline) {
			return errors.New("cistore: record exceeded CAS budget")
		}
		time.Sleep(casBackoff(attempt))
	}
}

func rearm(st *State) {
	if !st.SettledEmitted && (st.Claim == nil || st.Claim.Generation != st.Generation) {
		return
	}
	st.Generation++
	st.SettledEmitted = false
}

func observationMayReplace(incomingAt, storedAt, incomingStatus, storedStatus string) bool {
	incoming, incomingErr := time.Parse(time.RFC3339, incomingAt)
	stored, storedErr := time.Parse(time.RFC3339, storedAt)
	incomingValid := incomingErr == nil
	storedValid := storedErr == nil
	switch {
	case incomingValid && storedValid:
		if incoming.Before(stored) {
			return false
		}
		if incoming.After(stored) {
			return true
		}
		return statusRank(incomingStatus) >= statusRank(storedStatus)
	case !incomingValid && storedValid:
		return false
	case incomingValid:
		return true
	default:
		// A missing timestamp cannot order two observations, so preserve receipt
		// order for two timestamp-less values. A timestamp-less incoming never
		// displaces a stored observation with a usable timestamp.
		return true
	}
}

func statusRank(status string) int {
	if status == "completed" {
		return 1
	}
	return 0
}

func checkRunIDIsOlder(incoming, stored string) bool {
	incomingID, err := strconv.ParseUint(incoming, 10, 64)
	if err != nil {
		return false
	}
	storedID, err := strconv.ParseUint(stored, 10, 64)
	return err == nil && incomingID < storedID
}

// RecordHead persists the current PR head SHA. The WatchAll cache serves Head.
func (s *Store) RecordHead(owner, repo, number, sha, updatedAt string) error {
	if !validHeadSHA(sha) {
		return ErrInvalidHeadSHA
	}
	var incomingAt time.Time
	if updatedAt != "" {
		var err error
		incomingAt, err = time.Parse(time.RFC3339, updatedAt)
		if err != nil {
			return fmt.Errorf("cistore: invalid head updated_at: %w", err)
		}
	}
	key := headKey(owner, repo, number)
	deadline := time.Now().Add(recordBudget)
	for attempt := 0; ; attempt++ {
		entry, getErr := s.kv.Get(key)
		var current headRecord
		var rev uint64
		switch {
		case getErr == nil:
			if err := json.Unmarshal(entry.Value(), &current); err != nil {
				return err
			}
			rev = entry.Revision()
		case errors.Is(getErr, nats.ErrKeyNotFound):
			current = headRecord{Kind: headRecordKind}
		default:
			return getErr
		}
		if current.UpdatedAt != "" && updatedAt != "" {
			storedAt, err := time.Parse(time.RFC3339, current.UpdatedAt)
			if err == nil {
				if incomingAt.Before(storedAt) || (incomingAt.Equal(storedAt) && current.SHA == sha) {
					return nil
				}
			}
		}
		buf, err := json.Marshal(headRecord{Kind: headRecordKind, SHA: sha, UpdatedAt: updatedAt})
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
		if time.Now().After(deadline) {
			return errors.New("cistore: record head exceeded CAS budget")
		}
		time.Sleep(casBackoff(attempt))
	}
}

// Head returns the current PR head SHA when one has been observed.
func (s *Store) Head(owner, repo, number string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sha, ok := s.heads[headKey(owner, repo, number)]
	return sha, ok
}

// casBackoff returns a full-jitter, capped-exponential backoff for CAS retries.
func casBackoff(attempt int) time.Duration {
	base := time.Millisecond << attempt
	if base <= 0 || base > recordBackoffCap {
		base = recordBackoffCap
	}
	return time.Duration(rand.Int64N(int64(base) + 1))
}

// isCASConflict reports whether err is a compare-and-swap revision conflict
// (as opposed to a real transport/storage error). Create returns ErrKeyExists
// when the key already exists; Update returns a "wrong last sequence" API error
// when the revision moved.
func isCASConflict(err error) bool {
	return errors.Is(err, nats.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")
}

func (s *Store) casState(key string, apply func(st *State) (ok bool, err error)) (State, bool, error) {
	deadline := time.Now().Add(recordBudget)
	for attempt := 0; ; attempt++ {
		entry, err := s.kv.Get(key)
		if err != nil {
			return State{}, false, err
		}
		var state State
		if err := json.Unmarshal(entry.Value(), &state); err != nil {
			return State{}, false, err
		}
		ok, err := apply(&state)
		if err != nil {
			return State{}, false, err
		}
		if !ok {
			return state, false, nil
		}
		buf, err := json.Marshal(state)
		if err != nil {
			return State{}, false, err
		}
		revision, err := s.kv.Update(key, buf, entry.Revision())
		if err == nil {
			s.mu.Lock()
			s.cacheStateLocked(key, state, revision)
			s.mu.Unlock()
			return state, true, nil
		}
		if !isCASConflict(err) {
			return State{}, false, err
		}
		if time.Now().After(deadline) {
			return State{}, false, errors.New("cistore: state transition exceeded CAS budget")
		}
		time.Sleep(casBackoff(attempt))
	}
}

// List returns a snapshot copy of the current cached states.
func (s *Store) List() []State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]State, 0, len(s.cache))
	for _, state := range s.cache {
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool {
		return Key(out[i].Owner, out[i].Repo, out[i].Number, out[i].SHA) <
			Key(out[j].Owner, out[j].Repo, out[j].Number, out[j].SHA)
	})
	return out
}

// ClaimSettlement atomically acquires the right to publish a ready state
// generation after re-reading the durable state and head record. It applies
// the debounce window to the durable LastEventAt value, not the cache snapshot.
func (s *Store) ClaimSettlement(key, expectedHash string, expectedGeneration uint64, now int64, debounce time.Duration) (State, bool, error) {
	state, claimed, err := s.casState(key, func(state *State) (bool, error) {
		if state.SettledEmitted ||
			state.Claim != nil ||
			state.Generation != expectedGeneration ||
			state.Hash() != expectedHash ||
			!settlementReady(*state) ||
			state.LastEventAt+debounce.Milliseconds() > now {
			return false, nil
		}
		headMatches, err := s.durableHeadMatches(*state)
		if err != nil || !headMatches {
			return headMatches, err
		}
		state.Claim = &SettlementClaim{
			Hash:       expectedHash,
			Generation: expectedGeneration,
			ClaimedAt:  time.Now().UnixMilli(),
		}
		return true, nil
	})
	if err != nil || !claimed {
		return State{}, false, err
	}
	return state, true, nil
}

// ClaimStillHeld verifies from durable KV that this generation retains the
// settlement right immediately before an external publication.
func (s *Store) ClaimStillHeld(key string, generation uint64) (bool, error) {
	entry, err := s.kv.Get(key)
	if err != nil {
		return false, err
	}
	var state State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return false, err
	}
	headMatches, err := s.durableHeadMatches(state)
	if err != nil || !headMatches {
		return false, err
	}
	return !state.SettledEmitted &&
		state.Generation == generation &&
		state.Claim != nil &&
		state.Claim.Generation == generation &&
		state.Claim.Hash == state.Hash(), nil
}

// ReclaimSettlement releases a claim from a replica that died before publish.
// The caller determines the stale threshold from its configured debounce.
func (s *Store) ReclaimSettlement(key string, generation uint64, staleBefore int64) (bool, error) {
	_, reclaimed, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim == nil ||
			state.Claim.Generation != generation ||
			(state.Generation == generation && state.Claim.ClaimedAt >= staleBefore) {
			return false, nil
		}
		state.Claim = nil
		return true, nil
	})
	return reclaimed, err
}

// ReleaseClaim clears this generation's claim after a failed publication.
func (s *Store) ReleaseClaim(key string, generation uint64) (bool, error) {
	_, released, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim == nil || state.Claim.Generation != generation {
			return false, nil
		}
		state.Claim = nil
		return true, nil
	})
	return released, err
}

// MarkSettled marks a successfully published claim as emitted. A re-arm moves
// Generation, causing this CAS to refuse the obsolete publisher.
func (s *Store) MarkSettled(key string, generation uint64) (bool, error) {
	_, marked, err := s.casState(key, func(state *State) (bool, error) {
		if state.SettledEmitted ||
			state.Generation != generation ||
			state.Claim == nil ||
			state.Claim.Generation != generation ||
			state.Claim.Hash != state.Hash() {
			return false, nil
		}
		state.SettledEmitted = true
		state.Claim = nil
		return true, nil
	})
	return marked, err
}

func (s *Store) durableHeadMatches(state State) (bool, error) {
	entry, err := s.kv.Get(headKey(state.Owner, state.Repo, state.Number))
	if errors.Is(err, nats.ErrKeyNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var head headRecord
	if err := json.Unmarshal(entry.Value(), &head); err != nil || !validHeadSHA(head.SHA) {
		return false, nil
	}
	return head.SHA == state.SHA, nil
}

func settlementReady(st State) bool {
	if !terminal(st) {
		return false
	}
	for _, suite := range st.Suites {
		if suite.Status != "completed" {
			return false
		}
	}
	return true
}

func terminal(st State) bool {
	if len(st.Checks) == 0 {
		return false
	}
	for _, check := range st.Checks {
		switch classify(check) {
		case catRunning, catQueued:
			return false
		}
	}
	return true
}
