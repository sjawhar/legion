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

// KnownSessionRetention is how long a session stays known after its last registration, interest
// change or clean stop: the envoy_known_sessions bucket's TTL, and the window a record's own
// last_seen is read against (KnownSessions.Get). It is well past the 72 hours Envoy keeps a
// session's messages, so a suspended Legion session that resumes under the same id is still known
// when its queued messages are delivered.
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
// its deadline out on its own (Get, MatchEach), so the pass is only there to give the memory
// back, and an hour's delay in doing that changes no answer.
const knownSessionSweepInterval = time.Hour

// knownBucketOpenWait bounds every JetStream request one attempt to open the bucket makes, and so
// the whole attempt: a bucket this listener's NATS user may not open is refused by the server
// dropping the request, which costs the wait in full. The listener opens /v1 behind this attempt,
// so the wait is what a start pays for a bucket it cannot open; 2 seconds is far past a healthy
// open, whose leader has already answered the JetStream requests of every bucket opened before
// this one (cmd/listener/main.go opens the stream, the interest, role, session and CI buckets
// first), and far below the 10 seconds every other store of the listener waits, which no start
// can afford for a store no delivery depends on. An attempt this wait gives up on costs nothing
// but a retry.
const knownBucketOpenWait = 2 * time.Second

// knownBucketServeWait is the wait every bucket call made after the open gets: the cache
// watcher's, and a write's (RecordWithin), which its caller's own context bounds besides
// (bus.Bounded). It is the 10 seconds the listener's other stores give their bucket calls
// (store.Open, OpenSessionRegistry, cistore.Open): the short wait above is for the attempt a
// start waits on, and giving a write or a watch that wait would make a cluster that is merely
// slow look like one that refuses.
const knownBucketServeWait = 10 * time.Second

// knownBucketRetryFirst and knownBucketRetryMax are the waits between the attempts that follow a
// bucket that did not open: the first, then twice the one before it up to the cap, until an
// attempt opens the bucket or the store is stopped. The grant that lets a listener open this
// bucket is applied after the image that reads it, so a listener started before the grant picks
// the bucket up within the cap of the grant arriving, with no restart. Past the cap a denied
// listener asks 12 times an hour, for as long as it runs: one dropped request and one
// permissions violation in the server's log each, and nothing in its own, since only the first
// failure is logged (OpenKnownSessions) and only a success after it (reopenUntilStopped).
const (
	knownBucketRetryFirst = time.Second
	knownBucketRetryMax   = 5 * time.Minute
)

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
	// retryFirst and retryMax are the waits between the attempts to open a bucket that did not
	// open, knownBucketRetryFirst and knownBucketRetryMax unless a test shortens them
	// (withKnownSessionRetry, export_test.go).
	retryFirst time.Duration
	retryMax   time.Duration
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
// (store.TopicMatcher), so every read - one record by id, or the records whose topics match a
// published topic - answers from memory and makes no JetStream call. A publish counts its
// recipients per request against up to ~100,000 records, which neither a per-key Get nor a
// linear scan can serve.
//
// A store whose bucket is not open is the degraded listener: one whose NATS user may not open the
// bucket keeps the store, which answers no record and no match and whose writes do nothing, and
// opens the bucket as soon as an attempt of its own succeeds (reopenUntilStopped). A nil
// *KnownSessions answers the same way, for a listener that could not build a JetStream context at
// all and for a test that builds the handlers without a store, so every method here is safe on a
// nil receiver and no call site carries a nil check.
type KnownSessions struct {
	now func() time.Time
	log *slog.Logger
	// replicas is the copies the bucket is created with, kept for the attempts that follow a
	// bucket that did not open, and retryFirst and retryMax are the waits between them.
	replicas   int
	retryFirst time.Duration
	retryMax   time.Duration

	mu      sync.RWMutex
	records map[string]cachedKnown
	topics  *store.TopicMatcher

	// openMu guards conn and watcher: both are written when the bucket opens, which is at
	// OpenKnownSessions for a listener whose user may open it and at a later attempt for one
	// whose grant arrives while it runs.
	openMu sync.RWMutex
	// conn is the connection the bucket is opened on, the one the store was opened with until a
	// Rewatch moves the store to another.
	conn *nats.Conn
	// watcher feeds the records from the bucket and holds the handle the store writes through,
	// nil until the bucket opens.
	watcher *kvwatch.Watcher

	// stopped ends the sweep goroutine and the attempts to open the bucket; stopOnce closes it
	// once, whichever of StopWatch and EndWatch comes first.
	stopped  chan struct{}
	stopOnce sync.Once
}

// OpenKnownSessions opens the envoy_known_sessions bucket, creating it with a 30-day TTL
// (KnownSessionRetention) on file storage, and starts the watcher that keeps its records in
// memory and the sweep that gives back the memory of the records that aged out.
//
// A bucket that does not open within knownBucketOpenWait - a listener whose NATS user may not
// open it, which is what production runs between the image that reads these records and the grant
// that allows them - is no error and stops no start: the store is returned, logs the one WARN
// below, answers no record and writes nothing, and goes on attempting the bucket in the
// background until one succeeds (reopenUntilStopped). The error is the JetStream context's alone,
// which leaves no store to open anything with; the caller then runs on a nil *KnownSessions,
// which answers as this store does and never opens.
func OpenKnownSessions(conn *nats.Conn, options ...KnownSessionsOption) (*KnownSessions, error) {
	opts := knownSessionsOpts{
		replicas:   1,
		now:        time.Now,
		retryFirst: knownBucketRetryFirst,
		retryMax:   knownBucketRetryMax,
	}
	for _, o := range options {
		o(&opts)
	}
	// A connection that gives no JetStream context can open nothing, now or later. It is this
	// function's one error, read here so the caller is answered it rather than meeting it on
	// every attempt; each attempt builds a context of its own, on the connection it runs against.
	if _, err := knownJetStream(conn); err != nil {
		return nil, err
	}
	s := &KnownSessions{
		now:        opts.now,
		log:        opts.log,
		replicas:   opts.replicas,
		retryFirst: opts.retryFirst,
		retryMax:   opts.retryMax,
		records:    map[string]cachedKnown{},
		topics:     store.NewTopicMatcher(),
		conn:       conn,
		stopped:    make(chan struct{}),
	}
	kv, err := openKnownBucket(conn, opts.replicas)
	if err != nil {
		s.logger().Warn("the known-session bucket is not open; this listener answers no session's 30-day record and writes none until an attempt of its own opens the bucket",
			slog.String("bucket", bus.KnownSessionBucket), slog.String("error", err.Error()))
		go s.reopenUntilStopped()
	} else {
		s.startWatch(kv)
	}
	go s.sweepUntilStopped()
	return s, nil
}

// knownJetStream is the JetStream context one attempt to open the bucket makes its requests
// through: every one of them waits knownBucketOpenWait rather than the 10 seconds nats.go gives
// its own by default.
func knownJetStream(conn *nats.Conn) (nats.JetStreamContext, error) {
	return conn.JetStream(nats.MaxWait(knownBucketOpenWait))
}

// openKnownBucket makes one attempt to open the bucket on conn, creating it with the retention
// and the copies the deployment asks for when it is not there. The attempt waits
// knownBucketOpenWait, so one whose requests the server drops - a user whose grant does not carry
// the bucket - costs that and no more.
//
// The handle it returns is opened again on a context with the wait every other store of the
// listener gives its bucket calls, since the store's writes and its cache watcher go through that
// handle and must not give up on a cluster that is merely slow: a write is bounded by its
// caller's own context (RecordWithin, bus.Bounded), and the open's short wait is for the attempt
// a start waits on, not for the work that follows it.
func openKnownBucket(conn *nats.Conn, replicas int) (bus.KeyValue, error) {
	opening, err := knownJetStream(conn)
	if err != nil {
		return bus.KeyValue{}, err
	}
	if _, err := bus.EnsureKeyValue(opening, &nats.KeyValueConfig{
		Bucket:   bus.KnownSessionBucket,
		TTL:      KnownSessionRetention,
		Replicas: replicas,
		Storage:  nats.FileStorage,
	}); err != nil {
		return bus.KeyValue{}, err
	}
	serving, err := conn.JetStream(nats.MaxWait(knownBucketServeWait))
	if err != nil {
		return bus.KeyValue{}, err
	}
	return bus.OpenKeyValue(serving, bus.KnownSessionBucket)
}

// startWatch installs the watcher of the bucket kv is a handle on, and starts it: the watcher
// fills the records in the background, as the session registry's does, since a synchronous read
// of every key blocks for seconds when the bucket's leader is remote. A store already stopped
// installs nothing, so a bucket that opens during a shutdown leaves no watcher behind: StopWatch
// and EndWatch take openMu to read the watcher, so one installed here is theirs to stop and one
// installed after them is not installed at all.
func (s *KnownSessions) startWatch(kv bus.KeyValue) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	select {
	case <-s.stopped:
		return
	default:
	}
	s.watcher = kvwatch.New("known session store", kv, s.applyWatched, s.resetCache, kvwatch.WithLogger(s.log))
	s.watcher.Start()
}

// reopenUntilStopped attempts the bucket again, waiting retryFirst and then twice the wait before
// it up to retryMax, until an attempt opens it or the store is stopped. Each attempt runs on the
// connection the store holds now, which a Rewatch may have replaced.
//
// An attempt that opens the bucket is not the end of it: the watcher it installs fills the
// records in the background, and until it has, the store holds a cache that answers for none of
// the sessions the bucket knows. So this waits for that first pass before it reports anything,
// which is what keeps Opened false over an unfilled cache - the listener's start gates exactly
// this hazard with WaitForCacheReady before it opens /v1, and nothing gates the attempt that
// opens the bucket later. The one line it logs is that success, after the wait, so the line
// means what it says: the failure was logged when the store opened, and a line per attempt would
// be one every five minutes for the life of a listener whose grant never arrives.
func (s *KnownSessions) reopenUntilStopped() {
	for wait := s.retryFirst; ; wait = min(2*wait, s.retryMax) {
		timer := time.NewTimer(wait)
		select {
		case <-s.stopped:
			timer.Stop()
			return
		case <-timer.C:
		}
		s.openMu.RLock()
		conn := s.conn
		s.openMu.RUnlock()
		kv, err := openKnownBucket(conn, s.replicas)
		if err != nil {
			continue
		}
		s.startWatch(kv)
		watcher := s.watch()
		if watcher == nil {
			// The store stopped while this attempt ran, so startWatch installed nothing.
			return
		}
		if err := watcher.WaitReady(s.untilStopped()); err != nil {
			// The store stopped, or the watch ended before it had delivered the bucket. Opened
			// stays false, which leaves a publish's count out rather than answering it from a
			// cache that holds nothing.
			return
		}
		s.logger().Info("the known-session bucket is open; this listener writes and reads the 30-day records from now on",
			slog.String("bucket", bus.KnownSessionBucket))
		return
	}
}

// untilStopped is a context that ends when the store does, for a background wait that must not
// outlive a shutdown. Its goroutine ends with either.
func (s *KnownSessions) untilStopped() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.stopped:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

// watch is the store's watcher, or nil while its bucket is not open. A nil store has none either,
// so one read of this covers both.
func (s *KnownSessions) watch() *kvwatch.Watcher {
	if s == nil {
		return nil
	}
	s.openMu.RLock()
	defer s.openMu.RUnlock()
	return s.watcher
}

// Opened reports whether the store's bucket is open and its cache holds what the bucket held,
// so its answers are the bucket's. A store whose NATS user may not open the bucket answers false
// until an attempt of its own opens it and the watcher's first pass has filled the records, and
// a nil store for good; both answer no record and write nothing meanwhile. A caller whose own
// answer would mean something narrower than it names then - a publish's recipient count, which
// these records are half of - leaves that answer out instead of sending the live sessions alone
// (cmd/listener/recipients.go).
//
// The readiness is half of this on purpose. A bucket is opened in one request and read in a
// pass over every key it holds, and between the two the store answers for no session the bucket
// knows: a send would be refused `404 no live session` for a session Envoy does know, and a
// publish would count its recipients low, which are the two failures this store exists to
// remove.
func (s *KnownSessions) Opened() bool {
	watcher := s.watch()
	return watcher != nil && watcher.Ready()
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

// MatchEach calls visit with each record whose topics match topic (routing.Match's rules), once
// per record and in no order, looked up in the store's subject index. A record past the retention
// is left out, as Get leaves it out. visit runs under the store's read lock, so it must not call
// back into the store or block. A publish's recipient count reads the store this way: it holds a
// set of the session ids it has counted either way, and the list and the sort Match makes are
// work it would throw away.
func (s *KnownSessions) MatchEach(topic string, visit func(KnownSession)) {
	if s == nil {
		return
	}
	cutoff := s.cutoff()
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.topics.Match(topic, func(key string) {
		if held, ok := s.records[key]; ok && held.record.LastSeen >= cutoff {
			visit(held.record)
		}
	})
}

// Match returns the records MatchEach visits for topic, once each and sorted by session id.
func (s *KnownSessions) Match(topic string) []KnownSession {
	out := []KnownSession{}
	s.MatchEach(topic, func(record KnownSession) { out = append(out, record) })
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
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
// The caller never fails its request over this: a registration, an unsubscribe and a stop each
// answer whatever the write did, and the error is for the call site's WARN. A store whose bucket
// is not open writes nothing and answers nil, as a nil store does: its reads answer no record
// either, so nothing it could write would be read.
func (s *KnownSessions) RecordWithin(ctx context.Context, record KnownSession) error {
	watcher := s.watch()
	if watcher == nil || record.SessionID == "" {
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
	kv := watcher.KV()
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

// Rewatch moves the cache's watcher and the handle the store writes through to conn
// (kvwatch.Watcher.Rewatch), after a reconnect. A store whose bucket is not open has no watcher
// to move: it keeps conn for the attempts that follow, so the attempt that opens the bucket makes
// its requests on the connection the listener holds now.
func (s *KnownSessions) Rewatch(conn *nats.Conn) error {
	if s == nil {
		return nil
	}
	s.openMu.Lock()
	s.conn = conn
	watcher := s.watcher
	s.openMu.Unlock()
	if watcher == nil {
		return nil
	}
	return watcher.Rewatch(conn)
}

// WaitForCacheReady blocks until the watcher has delivered every existing record, or until ctx is
// done. Callers bound ctx and proceed on error: the store answers from whatever it holds. A store
// whose bucket is not open waits for nothing and answers nil: it has no cache to fill, and the
// listener's start must not spend its caller's budget on a bucket it may not open.
func (s *KnownSessions) WaitForCacheReady(ctx context.Context) error {
	watcher := s.watch()
	if watcher == nil {
		return nil
	}
	return watcher.WaitReady(ctx)
}

// StopWatch retires the cache's watcher and the sweep for a shutdown, and stops the attempts to
// open a bucket that is not open.
func (s *KnownSessions) StopWatch() {
	if s == nil {
		return
	}
	s.stopSweep()
	if watcher := s.watch(); watcher != nil {
		watcher.Stop()
	}
}

// EndWatch ends the cache's watcher and the sweep at shutdown, within ctx, while the connection
// still carries requests (kvwatch.Watcher.End), and stops the attempts to open a bucket that is
// not open.
func (s *KnownSessions) EndWatch(ctx context.Context) {
	if s == nil {
		return
	}
	s.stopSweep()
	if watcher := s.watch(); watcher != nil {
		watcher.End(ctx)
	}
}

// stopSweep ends the sweep goroutine and any attempt to open the bucket, and keeps a bucket that
// opens after it from installing a watcher (startWatch).
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
