package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/store"
)

// KnownSessionRetention is how long a session stays known after its last registration, refollow or
// clean stop: the envoy_known_sessions bucket's TTL, and the window a record's own last_seen is
// read against (KnownSessions.Get). It is well past the 72 hours Envoy keeps a session's messages,
// so a suspended Legion session that resumes under the same id is still known when its queued
// messages are delivered.
const KnownSessionRetention = 30 * 24 * time.Hour

// KnownSessionRefreshAge is how young a stored record must be for a write of the same content to
// be skipped (KnownSessions.RecordWithin): a session heartbeats every 90 seconds, so an unchanged
// record is rewritten at most once per this, whatever the number of heartbeats and however many
// listeners serve the session. A record's last_seen therefore lags the session's last heartbeat by
// less than this, which nothing reads as liveness: the session entry (SessionBucket) is the store
// that answers that.
const KnownSessionRefreshAge = 10 * time.Minute

// knownSessionSweepInterval is how often the store drops the records that have aged out of
// KnownSessionRetention. One periodic pass is the whole expiry machinery here, rather than a timer
// per record as the session registry arms (expiry.go): that registry holds the live sessions, a
// few thousand at most, each with a 5-minute deadline a read must not overshoot, while this store
// holds up to ~100,000 records whose deadline is 30 days away and whose only reader is a publish
// count. 100,000 armed timers would cost more than the records do; a read filters a record past
// its deadline out on its own (Get, Match, Each), so the pass is only there to give the memory
// back, and an hour's delay in doing that changes no answer.
const knownSessionSweepInterval = time.Hour

// KnownSession is one record of the envoy_known_sessions bucket, keyed by session id: a session
// that registered within KnownSessionRetention, whether it is registered now or not. Its Topics
// are the topics the registration stored (store.Interest.Topics), not the session consumer's
// filter: that filter keeps only the agent subject and the topics the stream captures
// (cmd/listener/session_delivery.go, sessionConsumerFilter), so a record built from it would count
// no subscriber of notifications.team.<name>, which no stream subject captures.
//
// The record holds nothing else, and in particular not whether the session declared the presence
// check: a session with no entry in the session registry is stopped without being asked, so
// nothing would read such a field.
type KnownSession struct {
	SessionID        string   `json:"session_id"`
	Title            string   `json:"title"`
	Capabilities     []string `json:"capabilities"`
	Topics           []string `json:"topics"`
	ClientInstanceID string   `json:"client_instance_id"`
	// LastSeen is when the record was last written, in unix milliseconds, as
	// SessionEntry.UpdatedAt is.
	LastSeen int64 `json:"last_seen"`
}

// cachedKnown is one cached record and the bucket revision it came from, so a watch event the
// store's own write already applied does not put an older value back.
type cachedKnown struct {
	record   KnownSession
	revision uint64
}

type KnownSessionsOption func(*knownSessionsOpts)

type knownSessionsOpts struct {
	replicas int
	log      *slog.Logger
	now      func() time.Time
}

// WithKnownSessionReplicas sets the copies the bucket is created with, and raised to, as the
// deployment's replica count says (bus.EnsureKeyValue).
func WithKnownSessionReplicas(n int) KnownSessionsOption {
	return func(o *knownSessionsOpts) { o.replicas = n }
}

// WithKnownSessionLogger sets the logger the store and its cache watcher write through. The
// listener passes its own, so their lines are JSON records with machine_id, as the session
// registry's are (WithSessionLogger).
func WithKnownSessionLogger(log *slog.Logger) KnownSessionsOption {
	return func(o *knownSessionsOpts) { o.log = log }
}

// WithKnownSessionClock sets the clock the store stamps each record's last_seen with and reads
// both the refresh age and the retention against; time.Now unless a test sets it. One clock drives
// all three, so a test that drives heartbeats through the listener's own write path controls the
// throttle with it.
func WithKnownSessionClock(now func() time.Time) KnownSessionsOption {
	return func(o *knownSessionsOpts) { o.now = now }
}

// KnownSessions is a cache-backed view of the envoy_known_sessions bucket: every session that
// registered through any listener within KnownSessionRetention, keyed by session id. A long-lived
// watcher (kvwatch) keeps the records in memory with a subject index of their topics
// (store.TopicMatcher), so every read - one record by id, the whole set, or the records whose
// topics match a published topic - answers from memory and makes no JetStream call. A publish
// counts its recipients per request against up to ~100,000 records, which neither a per-key Get
// nor a linear scan can serve.
//
// A nil *KnownSessions is the degraded listener: one whose NATS user may not open the bucket
// starts without the store and keeps running, so every method here is safe on a nil receiver and
// answers as an empty store does - no record, no match, and a write that does nothing. Call sites
// therefore carry no nil check of their own.
type KnownSessions struct {
	now func() time.Time
	log *slog.Logger

	mu      sync.RWMutex
	records map[string]cachedKnown
	topics  *store.TopicMatcher

	// watcher feeds the records from the bucket and holds the handle the store writes through.
	watcher *kvwatch.Watcher

	// stopped ends the sweep goroutine; stopOnce closes it once, whichever of StopWatch and
	// EndWatch comes first.
	stopped  chan struct{}
	stopOnce sync.Once
}

// OpenKnownSessions opens the envoy_known_sessions bucket, creating it with a 30-day TTL
// (KnownSessionRetention) on file storage, and starts the watcher that keeps its records in
// memory and the sweep that gives back the memory of the records that aged out. Its error is the
// bucket's: the listener logs it once and runs without the store (a nil *KnownSessions), since no
// delivery depends on these records.
func OpenKnownSessions(conn *nats.Conn, options ...KnownSessionsOption) (*KnownSessions, error) {
	opts := knownSessionsOpts{replicas: 1, now: time.Now}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{
		Bucket:   bus.KnownSessionBucket,
		TTL:      KnownSessionRetention,
		Replicas: opts.replicas,
		Storage:  nats.FileStorage,
	})
	if err != nil {
		return nil, err
	}
	s := &KnownSessions{
		now:     opts.now,
		log:     opts.log,
		records: map[string]cachedKnown{},
		topics:  store.NewTopicMatcher(),
		stopped: make(chan struct{}),
	}
	s.watcher = kvwatch.New("known session store", kv, s.applyWatched, s.resetCache, kvwatch.WithLogger(opts.log))
	// The watcher fills the records in the background, as the session registry's does: a
	// synchronous read of every key blocks for seconds when the bucket's leader is remote.
	s.watcher.Start()
	go s.sweepUntilStopped()
	return s, nil
}

// logger is the logger WithKnownSessionLogger set, or the default one.
func (s *KnownSessions) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

// Get returns sessionID's record, and whether the store holds one that has not aged out of
// KnownSessionRetention. It reads memory alone and makes no bucket call, so a caller on a request
// path pays nothing for it; a record past the retention answers as no record, whether or not the
// sweep has dropped it yet.
func (s *KnownSessions) Get(sessionID string) (KnownSession, bool) {
	if s == nil {
		return KnownSession{}, false
	}
	cutoff := s.cutoff()
	s.mu.RLock()
	defer s.mu.RUnlock()
	held, ok := s.records[sessionID]
	if !ok || held.record.LastSeen < cutoff {
		return KnownSession{}, false
	}
	return held.record, true
}

// Match returns, once each and sorted by session id, the records whose topics match topic
// (routing.Match's rules), looked up in the store's subject index. A record past the retention is
// left out, as Get leaves it out.
func (s *KnownSessions) Match(topic string) []KnownSession {
	out := []KnownSession{}
	if s == nil {
		return out
	}
	cutoff := s.cutoff()
	s.mu.RLock()
	var seen map[string]struct{}
	s.topics.Match(topic, func(key string) {
		if _, dup := seen[key]; dup {
			return
		}
		if seen == nil {
			seen = map[string]struct{}{}
		}
		seen[key] = struct{}{}
		if held, ok := s.records[key]; ok && held.record.LastSeen >= cutoff {
			out = append(out, held.record)
		}
	})
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// Each calls visit with every record the store holds that has not aged out, in no order. visit
// runs under the store's read lock, so it must not call back into the store or block.
func (s *KnownSessions) Each(visit func(KnownSession)) {
	if s == nil {
		return
	}
	cutoff := s.cutoff()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, held := range s.records {
		if held.record.LastSeen >= cutoff {
			visit(held.record)
		}
	}
}

// Count is how many records the store holds that have not aged out.
func (s *KnownSessions) Count() int {
	if s == nil {
		return 0
	}
	cutoff := s.cutoff()
	s.mu.RLock()
	defer s.mu.RUnlock()
	live := 0
	for _, held := range s.records {
		if held.record.LastSeen >= cutoff {
			live++
		}
	}
	return live
}

// RecordWithin writes record, stamped with the store's clock, its bucket write waiting no longer
// than ctx (bus.Bounded): an HTTP route passes its request's own context. A record whose content
// equals the one the store holds is not written while that one is younger than
// KnownSessionRefreshAge, so a session's 90-second heartbeat costs one write per refresh age
// rather than one per heartbeat. What that skip compares is the stored record as the watcher
// delivered it, or as this listener's own write left it, not a note this listener kept of what it
// sent, so the throttle holds across every listener that serves the session, exactly as
// store.Registry.UpsertWithin's does for an interest row.
//
// The caller never fails its request over this: a registration, a refollow and a stop each answer
// whatever the write did, and the error is for the call site's WARN.
func (s *KnownSessions) RecordWithin(ctx context.Context, record KnownSession) error {
	if s == nil || record.SessionID == "" {
		return nil
	}
	now := s.now()
	record.LastSeen = now.UnixMilli()
	buf, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if held, ok := s.Get(record.SessionID); ok && now.UnixMilli()-held.LastSeen < KnownSessionRefreshAge.Milliseconds() {
		// What the skip asks is whether this write would store the record already there, so it
		// compares the two encodings rather than the fields one by one: a field added to
		// KnownSession, and the difference between a null and an empty list, are then covered by
		// the encoding itself. The stored record is stamped with this write's last_seen, the one
		// field a write of unchanged content would move.
		unchanged := held
		unchanged.LastSeen = record.LastSeen
		stored, err := json.Marshal(unchanged)
		if err != nil {
			return err
		}
		if bytes.Equal(buf, stored) {
			return nil
		}
	}
	kv := s.watcher.KV()
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.Put(record.SessionID, buf) })
	if !answered {
		return fmt.Errorf("write the known-session record of %s: %w", record.SessionID, err)
	}
	if err != nil {
		return err
	}
	// Write through, as the session registry's Put does: the next heartbeat then finds this
	// record whether or not the watcher has delivered it back yet, so the throttle holds from the
	// first write rather than from the first watch event.
	s.mu.Lock()
	s.applyLocked(record.SessionID, record, revision)
	s.mu.Unlock()
	return nil
}

// TouchWithin moves sessionID's record to now, keeping its content, which is what a clean stop
// writes: the session stopped, and a publish must still count it as a recipient of the topics it
// followed. A session the store knows nothing of writes nothing, since a stop carries no topics of
// its own to record. The write goes through RecordWithin, so the refresh-age throttle holds here
// too.
func (s *KnownSessions) TouchWithin(ctx context.Context, sessionID string) error {
	if s == nil {
		return nil
	}
	held, ok := s.Get(sessionID)
	if !ok {
		return nil
	}
	return s.RecordWithin(ctx, held)
}

// Ping reports whether the bucket is reachable and the cache watcher still runs
// (kvwatch.Watcher.Check), in one round trip.
func (s *KnownSessions) Ping() error {
	if s == nil {
		return nil
	}
	if err := s.watcher.Check(); err != nil {
		return err
	}
	return s.watcher.Err()
}

// Rewatch moves the cache's watcher and the handle the store writes through to conn
// (kvwatch.Watcher.Rewatch), after a reconnect.
func (s *KnownSessions) Rewatch(conn *nats.Conn) error {
	if s == nil {
		return nil
	}
	return s.watcher.Rewatch(conn)
}

// WatchErr is the cache watcher's terminal error, or nil while it runs.
func (s *KnownSessions) WatchErr() error {
	if s == nil {
		return nil
	}
	return s.watcher.Err()
}

// WaitForCacheReady blocks until the watcher has delivered every existing record, or until ctx is
// done. Callers bound ctx and proceed on error: the store answers from whatever it holds.
func (s *KnownSessions) WaitForCacheReady(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.watcher.WaitReady(ctx)
}

// StopWatch retires the cache's watcher and the sweep for a shutdown.
func (s *KnownSessions) StopWatch() {
	if s == nil {
		return
	}
	s.stopSweep()
	s.watcher.Stop()
}

// EndWatch ends the cache's watcher and the sweep at shutdown, within ctx, while the connection
// still carries requests (kvwatch.Watcher.End).
func (s *KnownSessions) EndWatch(ctx context.Context) {
	if s == nil {
		return
	}
	s.stopSweep()
	s.watcher.End(ctx)
}

func (s *KnownSessions) stopSweep() {
	s.stopOnce.Do(func() { close(s.stopped) })
}

// cutoff is the earliest last_seen a record may carry and still answer: anything older has aged
// out of KnownSessionRetention.
func (s *KnownSessions) cutoff() int64 {
	return s.now().Add(-KnownSessionRetention).UnixMilli()
}

// applyWatched applies one entry the watcher delivered. A revision the store already holds, the
// echo of its own write among them, changes nothing.
func (s *KnownSessions) applyWatched(entry nats.KeyValueEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held, ok := s.records[entry.Key()]; ok && entry.Revision() <= held.revision {
		return
	}
	if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
		s.dropLocked(entry.Key())
		return
	}
	var record KnownSession
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		s.dropLocked(entry.Key())
		s.logger().Warn("known session store evicted a malformed record",
			slog.String("key", entry.Key()), slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
		return
	}
	s.applyLocked(entry.Key(), record, entry.Revision())
}

// applyLocked holds key's record and keeps the subject index in step with it. Callers hold s.mu
// for writing.
func (s *KnownSessions) applyLocked(key string, record KnownSession, revision uint64) {
	s.records[key] = cachedKnown{record: record, revision: revision}
	s.topics.Set(key, record.Topics)
}

// dropLocked forgets key. Callers hold s.mu for writing.
func (s *KnownSessions) dropLocked(key string) {
	delete(s.records, key)
	s.topics.Remove(key)
}

// resetCache empties the records and the index, for a recreated bucket whose revisions start at 1
// again.
func (s *KnownSessions) resetCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = map[string]cachedKnown{}
	s.topics = store.NewTopicMatcher()
}

// sweepUntilStopped runs the one periodic pass until the store is stopped.
func (s *KnownSessions) sweepUntilStopped() {
	ticker := time.NewTicker(knownSessionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

// sweep drops every record that has aged out of KnownSessionRetention, giving back the strings and
// topic lists it held and the index nodes its topics alone kept. A JetStream TTL expiry is
// delivered as no watch event, so nothing else would ever drop them. The map keeps the buckets it
// grew to, which the records written next reuse.
func (s *KnownSessions) sweep() {
	cutoff := s.cutoff()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, held := range s.records {
		if held.record.LastSeen < cutoff {
			s.dropLocked(key)
		}
	}
}
