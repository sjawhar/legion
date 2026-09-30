// Package cistore holds the ingest-side aggregation of GitHub CI observations.

// Each check_run and check_suite webhook folds into a per-commit State record
// in a JetStream KV bucket via compare-and-swap rather than being published
// raw; concurrent observations of one commit share one write (see update). A
// reconcile ticker (see loop.go) emits one checks envelope when a commit is quiet
// and its recorded CI work is complete. All coordination state lives in
// KV, so aggregation is durable, restart-safe, and correct across listener
// replicas; the in-memory state is a rebuildable WatchAll read-cache and the
// queue of observations waiting for their commit's write.
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
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
)

// Bucket is the JetStream KV bucket name for per-commit CI state.
const Bucket = "envoy_ci_state"

// recordBudget bounds how long one write of a commit's record retries. It retries a lost
// compare-and-swap, and a transient KV error (kvErrorLasts), from a fresh read. Concurrent
// observations of one commit within this process are combined into one write (update), so the
// writers that conflict on the KV revision are the few that remain: the summary loop's settlement
// transitions and another listener task during a deploy. Each backs off with jitter before
// retrying, so a bounded time budget lets them serialize rather than a fixed attempt count. The
// transient errors it outlasts are a NATS reconnect (a request refused while the connection
// reconnects, since the reconnect buffer is off, and a request that was in flight when the
// connection went away, given up on at the rewatch that follows the reconnect: kvCall) and a
// JetStream 503 while a server restarts. It cannot outlast a timeout between rewatches, because
// every KV call waits up to the JetStream MaxWait (10 s, Open) first, five times this budget, so a
// call that times out returns after the budget has run out.
// A caller that queues behind a write in progress waits for that write and then its own, so a
// Record returns within two budgets. The webhook handler records a check_run once for each PR it
// lists, one after another, so a delivery listing k PRs can take up to 2k budgets, past the
// listener's 10s HTTP WriteTimeout at three; that needs a write to lose its compare-and-swap for its
// whole budget, and after combining only the summary loop's transitions and another listener
// task's batches write the same record. NOTE: this bounds only the retry loop; a KV call can still
// wait up to the JetStream MaxWait between rewatches (a systemic limit of the legacy nats.go KV
// API, shared with internal/store).
const recordBudget = 2 * time.Second
const recordBackoffCap = 50 * time.Millisecond

// checkRunID is a GitHub check-run id. Records written before ids were
// validated at ingress carry it as a decimal string; new records carry a
// number. Id-less legacy checks omit the field when they are persisted again.
type checkRunID uint64

func (id *checkRunID) UnmarshalJSON(raw []byte) error {
	var n uint64
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		*id = checkRunID(n)
		return nil
	}
	var legacy string
	if err := json.Unmarshal(raw, &legacy); err == nil {
		if n, err := strconv.ParseUint(legacy, 10, 64); err == nil && n > 0 {
			*id = checkRunID(n)
			return nil
		}
	}
	return fmt.Errorf("cistore: check run ID %s is not a positive integer", raw)
}

// Check is the last-known state of a single named check for a commit.
type Check struct {
	Name       string     `json:"name,omitempty"`
	CheckRunID checkRunID `json:"check_run_id,omitempty"`
	URL        string     `json:"url"`
	Status     string     `json:"status"`     // queued|in_progress|completed
	Conclusion string     `json:"conclusion"` // success|failure|... ("" until completed)
	// ObservedAt orders observations of this run (completion, else start).
	ObservedAt string `json:"observed_at"`
}

// UnmarshalJSON reads records written by deployed listeners: their checks
// carry updated_at instead of observed_at. The retired updated_at becomes
// observed_at.
func (check *Check) UnmarshalJSON(data []byte) error {
	type checkAlias Check
	*check = Check{}
	wire := struct {
		*checkAlias
		ObservedAt *string `json:"observed_at"`
		UpdatedAt  string  `json:"updated_at"`
	}{checkAlias: (*checkAlias)(check)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.ObservedAt != nil {
		check.ObservedAt = *wire.ObservedAt
		return nil
	}
	check.ObservedAt = wire.UpdatedAt
	return nil
}

// Suite is the last-known state of one GitHub check suite for a commit.
type Suite struct {
	ID         string `json:"id"`
	AppID      string `json:"app_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ObservedAt string `json:"observed_at"`
}

// SettlementClaim is the durable right to publish one settlement snapshot.
// Claims prevent replicas from publishing the same episode concurrently.
type SettlementClaim struct {
	Hash       string `json:"hash"`
	Generation uint64 `json:"generation"`
	ClaimedAt  int64  `json:"claimed_at"`
}

// State is the aggregated set of checks and suites for one (owner, repo, PR
// number, head SHA).
type State struct {
	Owner          string           `json:"owner"`
	Repo           string           `json:"repo"`
	Number         string           `json:"number"`
	SHA            string           `json:"sha"`
	Checks         map[string]Check `json:"checks"`
	Suites         map[string]Suite `json:"suites"`
	LastEventAt    int64            `json:"last_event_at"`
	Generation     uint64           `json:"generation"`
	EmittedCount   uint64           `json:"emitted_count"`
	SettledEmitted bool             `json:"settled_emitted"`
	Claim          *SettlementClaim `json:"claim,omitempty"`
	// Overflowed says the listener never settles this head again, because the head is past what it
	// can publish: a check was refused because it would take the record past maxRecordBytes or the
	// record's settlement past maxSettlementBytes (write), or NATS refused the settlement itself
	// (markOverflowed, from the summary loop). A head that settled before it overflowed keeps its
	// last settlement; nothing supersedes it.
	Overflowed bool `json:"overflowed,omitempty"`
	// Schema is recordSchema on every record this listener writes (encodeRecord). A record last
	// written by a listener that settled only a pull request's head has none (legacyBacklog). An
	// older listener ignores the field, and its own writes drop it.
	Schema int `json:"schema,omitempty"`
}

// recordSchema stamps every record this listener writes.
const recordSchema = 1

// encodeRecord stamps st with recordSchema and encodes it. Every write of a record goes through
// it, so a record without the stamp was last written by a head-gated listener.
func encodeRecord(st *State) ([]byte, error) {
	st.Schema = recordSchema
	return json.Marshal(st)
}

// handoverGrace is how long past its debounce a record without a schema can still settle, so every
// such record admitted is at most debounce+handoverGrace old. Five minutes is the smallest round
// figure above the startup floor in the table in
// docs/solutions/architecture-patterns/envoy-ci-summary.md, the time a stop-then-start compose
// deploy of a listener with the GitHub webhook route takes from the old listener's SIGTERM to the
// new one's first tick, so a head that finished during a degraded handover still settles; a wider
// grace would only make admitted settlements later. That doc holds the derivation, its constants and the dated measurements.
const handoverGrace = 5 * time.Minute

// legacyBacklog reports whether st is a head-gated listener's leftover: it has no schema and its
// last event is at least debounce plus handoverGrace before now. A record without a schema settles
// only in [debounce, debounce+handoverGrace) after its last event; past that it never settles, and
// it stays settled_emitted false with no schema until the bucket's seven-day TTL expires it, which
// is expected. An observation that changes the record stamps it, and the commit then settles as any
// other; a stamped record is never held back, so a settlement pending across a restart still
// publishes. Seven days after the last head-gated listener stops, no record without a schema
// remains.
//
// A record without a schema whose last_event_at is ahead of this listener's clock publishes once
// wall time reaches its band. Raising ENVOY_CI_DEBOUNCE moves the band's far edge with it, so a
// restart with a larger debounce admits the records whose last event falls in the added slice.
func legacyBacklog(st State, now int64, debounce time.Duration) bool {
	return st.Schema == 0 && now-st.LastEventAt >= (debounce+handoverGrace).Milliseconds()
}

// due reports whether st is waiting to settle at now: unsettled, ready (settlementReady), quiet for
// debounce since its last event, and not a head-gated listener's leftover (legacyBacklog).
func due(st State, now int64, debounce time.Duration) bool {
	return !st.SettledEmitted &&
		settlementReady(st) &&
		now-st.LastEventAt >= debounce.Milliseconds() &&
		!legacyBacklog(st, now, debounce)
}

// heldBack reports whether st would be due but for legacyBacklog.
func heldBack(st State, now int64, debounce time.Duration) bool {
	return !st.SettledEmitted && settlementReady(st) && legacyBacklog(st, now, debounce)
}

// UnmarshalJSON maps the retired resettled marker to the durable fact that
// this record emitted at least one settlement.
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
	if (wire.Resettled || state.SettledEmitted) && state.EmittedCount == 0 {
		state.EmittedCount = 1
	}
	return nil
}

// retiredHeadKind marks the head record an earlier listener kept per pull request under its own
// key, to settle only a pull request's head. Settlements do not depend on the head, so nothing
// writes or reads one; a record left in the bucket waits out its TTL uncached.
const retiredHeadKind = "head"

// keyCleaner writes one segment of a KV key: every key this store builds joins its segments with
// '.', and a KV key's tokens must not be empty, so a '.' in a segment is written '='. A GitHub
// repository name may begin with a dot, end with one or hold two in a row (`sjawhar/.github`), and
// no GitHub owner, name, number or sha holds '=', so the spelling is lossless: `foo.bar` and
// `foo_bar` keep distinct keys, and a segment without a dot is written as it is. The rest guards
// against stray wildcard, space and slash characters.
var keyCleaner = strings.NewReplacer(".", "=", "*", "_", ">", "_", " ", "_", "/", "_")

// Key derives a KV-safe key from the commit identity, each segment through keyCleaner. The key is
// never parsed back (State carries the identity fields), so it only needs to be unique and valid
// per commit.
func Key(owner, repo, number, sha string) string {
	return keyCleaner.Replace(owner) + "." +
		keyCleaner.Replace(repo) + ".pr" +
		keyCleaner.Replace(number) + "." +
		keyCleaner.Replace(sha)
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
		h.Write([]byte("check\x00" + name + "\x00" + strconv.FormatUint(uint64(check.CheckRunID), 10) + "\x00" + check.Status + "\x00" + check.Conclusion + "\x01"))
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
	mu             sync.RWMutex
	cache          map[string]State
	cacheRevisions map[string]uint64
	// watcher feeds the cache and holds the handle the store writes through. The summary loop
	// reads only the cache (no KV fallback), so a dead watcher silently stops summaries; Ping
	// exposes its terminal error to the listener, which re-establishes the watcher or exits for
	// Docker to restart.
	watcher *kvwatch.Watcher

	// rewatchMu guards rewatched, which Rewatch closes and replaces. Rewatch runs on every NATS
	// reconnect and when the listener's self-health rebuilds a watcher, so a write's KV call still
	// waiting when it closes may have been sent on a connection that went away (kvCall).
	rewatchMu sync.Mutex
	rewatched chan struct{}

	// combineMu guards combiners, which holds, for each commit's record, the mutations waiting to
	// be written to it (update).
	combineMu sync.Mutex
	combiners map[string]*keyCombiner

	// logger writes the store's JSON lines, which an alarm can count (write).
	logger *logging.Logger
}

type openOpts struct {
	replicas int
	ttl      time.Duration
	bucket   string
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
// so Open never blocks on per-key Gets. The store and its cache watcher log through logger.
func Open(nc *nats.Conn, logger *logging.Logger, opts ...Option) (*Store, error) {
	o := openOpts{replicas: 1, ttl: 7 * 24 * time.Hour, bucket: Bucket}
	for _, f := range opts {
		f(&o)
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{
		Bucket:   o.bucket,
		Replicas: o.replicas,
		Storage:  nats.FileStorage,
		TTL:      o.ttl,
	})
	if err != nil {
		return nil, err
	}
	s := &Store{
		cache:          map[string]State{},
		cacheRevisions: map[string]uint64{},
		combiners:      map[string]*keyCombiner{},
		rewatched:      make(chan struct{}),
		logger:         logger,
	}
	s.watcher = kvwatch.New("cistore", kv, s.applyWatched, s.resetCache, kvwatch.WithLogger(logger.Slog()))
	s.watcher.Start()
	return s, nil
}

// Ping verifies the KV bucket is reachable and its cache watcher is alive.
func (s *Store) Ping() error {
	if err := s.watcher.Check(); err != nil {
		return err
	}
	return s.watcher.Err()
}

// Rewatch moves the cache's watcher and the handle the store writes through to conn
// (kvwatch.Watcher.Rewatch), and then gives up on every write's KV call still waiting for an
// answer (kvCall), since it runs on every reconnect: their retries take the handle it installed.
// It gives them up even when the move fails, since after a reconnect the connection they were sent
// on went away either way.
func (s *Store) Rewatch(conn *nats.Conn) error {
	err := s.watcher.Rewatch(conn)
	s.rewatchMu.Lock()
	close(s.rewatched)
	s.rewatched = make(chan struct{})
	s.rewatchMu.Unlock()
	return err
}

// resetCache empties the cache and its revision fence, for a recreated CI bucket.
func (s *Store) resetCache() {
	s.mu.Lock()
	s.cache = map[string]State{}
	s.cacheRevisions = map[string]uint64{}
	s.mu.Unlock()
}

// applyWatched applies one entry the KV watcher delivered to the cache.
func (s *Store) applyWatched(entry nats.KeyValueEntry) {
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
		if err := json.Unmarshal(entry.Value(), &marker); err == nil && marker.Kind == retiredHeadKind {
			s.evictCachedLocked(key, entry.Revision())
		} else {
			var st State
			if err := json.Unmarshal(entry.Value(), &st); err != nil {
				s.evictCachedLocked(key, entry.Revision())
				malformed = err
			} else if key != Key(st.Owner, st.Repo, st.Number, st.SHA) {
				// The summary loop addresses a record by the key its identity builds, so a record
				// stored under any other key could never be claimed: it waits out its TTL uncached.
				s.evictCachedLocked(key, entry.Revision())
			} else {
				s.cacheStateLocked(key, st, entry.Revision())
			}
		}
	}
	s.mu.Unlock()

	if malformed != nil {
		s.logger.Warn("cistore watch evicted malformed value",
			slog.String("key", key),
			slog.Uint64("revision", entry.Revision()),
			slog.String("error", malformed.Error()),
		)
	}
}

func (s *Store) cacheStateLocked(key string, state State, revision uint64) {
	if revision < s.cacheRevisions[key] {
		return
	}
	s.cache[key] = state
	s.cacheRevisions[key] = revision
}

func (s *Store) evictCachedLocked(key string, revision uint64) {
	if revision < s.cacheRevisions[key] {
		return
	}
	delete(s.cache, key)
	s.cacheRevisions[key] = revision
}

// WatchErr is the cache watcher's terminal error, or nil while it runs.
func (s *Store) WatchErr() error {
	return s.watcher.Err()
}

// StopWatch retires the cache's watcher for a shutdown (kvwatch.Watcher.Stop).
func (s *Store) StopWatch() {
	s.watcher.Stop()
}

// WaitForCacheReady blocks until the watcher has delivered every existing key, or until ctx is
// done.
func (s *Store) WaitForCacheReady(ctx context.Context) error {
	return s.watcher.WaitReady(ctx)
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
			if checkRunIDIsOlder(observation.CheckRunID, uint64(current.CheckRunID)) ||
				(observation.CheckRunID == uint64(current.CheckRunID) && !observationMayReplace(observation.ObservedAt, current.ObservedAt, observation.Status, current.Status)) {
				return false
			}
		}
		next := Check{
			Name:       observation.CheckName,
			CheckRunID: checkRunID(observation.CheckRunID),
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

func rearm(st *State) {
	if !st.SettledEmitted && (st.Claim == nil || st.Claim.Generation != st.Generation) {
		return
	}
	bumpGeneration(st)
	st.SettledEmitted = false
}

// bumpGeneration advances the delivery identity after a mutation invalidates a
// settlement snapshot. ClaimSettlement and MarkSettled deliberately do not
// call it: claiming reserves the current snapshot, while marking preserves the
// identity of the snapshot that was published.
func bumpGeneration(st *State) {
	st.Generation++
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

func checkRunIDIsOlder(incoming, stored uint64) bool {
	return incoming < stored
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
	kv := s.watcher.KV()
	deadline := time.Now().Add(recordBudget)
	for attempt := 0; ; attempt++ {
		entry, err := kv.Get(key)
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
		buf, err := encodeRecord(&state)
		if err != nil {
			return State{}, false, err
		}
		revision, err := kv.Update(key, buf, entry.Revision())
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
// generation without changing that generation, after re-reading the durable
// state. It applies due to the durable record, not the cache snapshot.
func (s *Store) ClaimSettlement(key, expectedHash string, expectedGeneration uint64, now int64, debounce time.Duration) (State, bool, error) {
	state, claimed, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim != nil ||
			state.Generation != expectedGeneration ||
			state.Hash() != expectedHash ||
			!due(*state, now, debounce) {
			return false, nil
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

// ClaimStillHeld verifies from durable KV that this exact snapshot retains the
// settlement right immediately before an external publication.
func (s *Store) ClaimStillHeld(key string, generation uint64, hash string) (bool, error) {
	kv := s.watcher.KV()
	entry, err := kv.Get(key)
	if err != nil {
		return false, err
	}
	var state State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return false, err
	}
	return !state.SettledEmitted &&
		state.Generation == generation &&
		state.Claim != nil &&
		state.Claim.Generation == generation &&
		state.Claim.Hash == hash &&
		state.Hash() == hash, nil
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
		bumpGeneration(state)
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
		bumpGeneration(state)
		return true, nil
	})
	return released, err
}

// MarkSettled records a successfully published claim without changing its
// generation. A re-arm that happened after publication keeps its newer snapshot
// unsettled, but it must not erase the fact that the obsolete snapshot emitted.
func (s *Store) MarkSettled(key string, generation uint64) (bool, error) {
	_, marked, err := s.casState(key, func(state *State) (bool, error) {
		if state.Claim == nil || state.Claim.Generation != generation {
			return false, nil
		}
		state.EmittedCount++
		if state.Generation == generation && state.Claim.Hash == state.Hash() {
			state.SettledEmitted = true
		}
		state.Claim = nil
		return true, nil
	})
	return marked, err
}

// markOverflowed marks key's record overflowed, so it never settles: NATS refused its settlement,
// which it would refuse on every tick.
func (s *Store) markOverflowed(key string) error {
	_, _, err := s.casState(key, func(state *State) (bool, error) {
		if state.Overflowed {
			return false, nil
		}
		state.Overflowed = true
		return true, nil
	})
	return err
}

// settlementReady reports whether st can settle: every check and suite it holds is terminal, one
// check has a run id, and the record holds every check of its commit, which an overflowed one does
// not.
func settlementReady(st State) bool {
	if st.Overflowed || !terminal(st) {
		return false
	}
	hasCheckRunID := false
	for _, check := range st.Checks {
		if check.CheckRunID > 0 {
			hasCheckRunID = true
			break
		}
	}
	if !hasCheckRunID {
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
