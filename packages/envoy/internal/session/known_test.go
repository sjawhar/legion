package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// openKnown opens the known-session store on the package's shared NATS server, with a clock the
// test moves.
func openKnown(t *testing.T, now func() time.Time) *KnownSessions {
	t.Helper()
	client := setupNATS(t)
	known, err := OpenKnownSessions(client.Conn, WithKnownSessionReplicas(1), WithKnownSessionClock(now))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	return known
}

// matchedIDs is the session ids MatchEach visits for topic, sorted.
func matchedIDs(known *KnownSessions, topic string) []string {
	ids := []string{}
	known.MatchEach(topic, func(record KnownSession) { ids = append(ids, record.SessionID) })
	slices.Sort(ids)
	return ids
}

// A record written to the bucket is answered from memory by id and by topic, with the topics it
// was written with - a team topic no stream subject captures among them.
func TestKnownSessionsAnswerByIDAndByTopic(t *testing.T) {
	known := openKnown(t, time.Now)
	topics := []string{"notifications.agent.ses_known", "notifications.team.deploy-squad"}
	if err := known.RecordWithin(context.Background(), KnownSession{
		SessionID:    "ses_known",
		Title:        "a known session",
		Capabilities: []string{"aside"},
		Topics:       topics,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	record, ok := known.Get("ses_known")
	if !ok {
		t.Fatal("Get answered no record for the session just written")
	}
	if record.Title != "a known session" || !slices.Equal(record.Topics, topics) {
		t.Fatalf("record = %#v, want the written title and topics", record)
	}
	if record.LastSeen <= 0 {
		t.Fatalf("record.LastSeen = %d, want the write's own stamp", record.LastSeen)
	}
	if got := matchedIDs(known, "notifications.team.deploy-squad"); !slices.Equal(got, []string{"ses_known"}) {
		t.Errorf("MatchEach of the team topic = %v, want the session", got)
	}
	if got := matchedIDs(known, "notifications.team.other"); len(got) != 0 {
		t.Errorf("MatchEach of an unfollowed topic = %v, want none", got)
	}
}

// TouchWithin moves a record's last_seen and keeps its content; a session the store knows nothing
// of writes nothing.
func TestKnownSessionsTouchKeepsTheRecordAndMovesLastSeen(t *testing.T) {
	clock := time.Now()
	known := openKnown(t, func() time.Time { return clock })
	topics := []string{"notifications.team.deploy-squad"}
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_touch", Topics: topics}); err != nil {
		t.Fatalf("record: %v", err)
	}
	first, _ := known.Get("ses_touch")
	clock = clock.Add(KnownSessionRefreshAge + time.Minute)
	if err := known.TouchWithin(context.Background(), "ses_touch"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	moved, ok := known.Get("ses_touch")
	if !ok {
		t.Fatal("the touched record is gone")
	}
	if moved.LastSeen != clock.UnixMilli() || moved.LastSeen == first.LastSeen {
		t.Errorf("last_seen = %d, want the clock's %d (was %d)", moved.LastSeen, clock.UnixMilli(), first.LastSeen)
	}
	if !slices.Equal(moved.Topics, topics) {
		t.Errorf("topics = %v, want the record's own %v", moved.Topics, topics)
	}
	if err := known.TouchWithin(context.Background(), "ses_unknown"); err != nil {
		t.Fatalf("touch of a session with no record: %v", err)
	}
	if _, ok := known.Get("ses_unknown"); ok {
		t.Error("a touch of a session with no record wrote one")
	}
}

// A record whose last_seen is past the 30-day retention answers as no record, by id and by topic,
// before the sweep has run; the sweep then gives its memory back.
func TestKnownSessionsAnswerNoRecordPastTheRetention(t *testing.T) {
	clock := time.Now()
	known := openKnown(t, func() time.Time { return clock })
	if err := known.RecordWithin(context.Background(), KnownSession{
		SessionID: "ses_old",
		Topics:    []string{"notifications.team.deploy-squad"},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	clock = clock.Add(KnownSessionRetention + time.Hour)
	if _, ok := known.Get("ses_old"); ok {
		t.Error("Get answered a record older than the retention")
	}
	if got := matchedIDs(known, "notifications.team.deploy-squad"); len(got) != 0 {
		t.Errorf("MatchEach answered %v for a record older than the retention", got)
	}
	known.mu.RLock()
	held := len(known.records)
	known.mu.RUnlock()
	if held != 1 {
		t.Fatalf("the store holds %d records before the sweep, want the one it has not swept yet", held)
	}
	known.sweep()
	known.mu.RLock()
	held = len(known.records)
	known.mu.RUnlock()
	if held != 0 {
		t.Errorf("the store holds %d records after the sweep, want none", held)
	}
}

// Every method is safe on a nil store, the listener that could not build a JetStream context: no
// record, no match, and writes that do nothing.
func TestNilKnownSessionsAnswersNothingAndPanicsNowhere(t *testing.T) {
	var known *KnownSessions
	if record, ok := known.Get("ses_any"); ok || record.SessionID != "" {
		t.Errorf("Get on a nil store = %#v, %v, want no record", record, ok)
	}
	known.MatchEach("notifications.team.deploy-squad", func(record KnownSession) {
		t.Errorf("MatchEach on a nil store visited %#v", record)
	})
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_any"}); err != nil {
		t.Errorf("RecordWithin on a nil store = %v, want nil", err)
	}
	if err := known.TouchWithin(context.Background(), "ses_any"); err != nil {
		t.Errorf("TouchWithin on a nil store = %v, want nil", err)
	}
	if err := known.Rewatch(nil); err != nil {
		t.Errorf("Rewatch on a nil store = %v, want nil", err)
	}
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Errorf("WaitForCacheReady on a nil store = %v, want nil", err)
	}
	known.StopWatch()
	known.EndWatch(context.Background())
}

// knownBucketGrant is the publish grant a NATS user needs to open the known-session bucket,
// watch it and write it. A listener's user carries these once the grant that follows this image
// is applied; until then it carries none of them, which is the listener below.
func knownBucketGrant() []string {
	stream := "KV_" + bus.KnownSessionBucket
	return []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO." + stream,
		"$JS.API.STREAM.CREATE." + stream,
		"$JS.API.DIRECT.GET." + stream + ".>",
		"$JS.API.STREAM.MSG.GET." + stream,
		"$KV." + bus.KnownSessionBucket + ".>",
		"$JS.API.CONSUMER.CREATE." + stream + ".>",
		"$JS.API.CONSUMER.DELETE." + stream + ".>",
		"$JS.FC." + stream + ".>",
	}
}

// A store whose NATS user may not open the bucket opens all the same, within one attempt's wait:
// it stops no start, waits for no cache, answers no record and writes nothing, and says so once.
// When the grant arrives - production applies it after the image that reads these records - an
// attempt of its own opens the bucket, with no restart, and the store writes and reads from then
// on. The denial is the server's own: the user's publish grant names none of the bucket's
// subjects, so the request is dropped as a denied JetStream request is in production, and the
// attempt costs the whole wait rather than ending on a refusal.
func TestAKnownSessionStoreOpensItsDeniedBucketOnceTheGrantArrives(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "nothing.this.store.uses")
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant does not carry the bucket: %v", err)
	}
	t.Cleanup(conn.Close)
	var logs bytes.Buffer
	var logsMu sync.Mutex
	logged := func() string {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.String()
	}

	started := time.Now()
	known, err := OpenKnownSessions(conn,
		WithKnownSessionReplicas(1),
		WithKnownSessionLogger(slog.New(slog.NewTextHandler(lockedWriter{&logs, &logsMu}, nil))),
		// The production schedule's first wait is a second and its cap five minutes, which a
		// test would spend waiting; every attempt it makes is the same.
		withKnownSessionRetry(100*time.Millisecond, 500*time.Millisecond))
	opening := time.Since(started)
	if err != nil {
		t.Fatalf("open the store over a bucket its user may not open: %v", err)
	}
	if known == nil {
		t.Fatal("no store over a bucket its user may not open; the listener has nothing to answer reads with")
	}
	t.Cleanup(known.StopWatch)
	if opening > 2*knownBucketOpenWait {
		t.Fatalf("the open took %s, want it bounded by the %s one attempt waits: the listener's start waits here before it opens /v1",
			opening, knownBucketOpenWait)
	}
	t.Logf("the open over a denied bucket took %s", opening)

	waited := time.Now()
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReady()
	if err := known.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("WaitForCacheReady over a bucket that is not open: %v, want nil", err)
	}
	if elapsed := time.Since(waited); elapsed > time.Second {
		t.Fatalf("the cache gate waited %s on a bucket that is not open, want it to wait for nothing", elapsed)
	}

	const denied = "ses_denied_bucket"
	topic := "notifications.team.deploy-squad"
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: denied, Topics: []string{topic}}); err != nil {
		t.Fatalf("RecordWithin over a bucket that is not open: %v, want nil", err)
	}
	if record, ok := known.Get(denied); ok {
		t.Errorf("Get over a bucket that is not open answered %#v, want no record", record)
	}
	if got := matchedIDs(known, topic); len(got) != 0 {
		t.Errorf("Match over a bucket that is not open answered %v, want none", got)
	}
	if warnings := strings.Count(logged(), "level=WARN"); warnings != 1 {
		t.Fatalf("the store logged %d WARN lines over a bucket it may not open, want the one that names it:\n%s", warnings, logged())
	}

	grant.Grant(t, knownBucketGrant()...)

	deadline := time.Now().Add(30 * time.Second)
	for !known.Opened() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !known.Opened() {
		t.Fatalf("the store did not open the bucket within 30s of the grant:\n%s", logged())
	}
	// Opened is the watcher's first pass delivered, and the line is written by the goroutine that
	// waited for it, so the two are not ordered: wait for the line before counting it.
	for !strings.Contains(logged(), "the known-session bucket is open") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: denied, Topics: []string{topic}}); err != nil {
		t.Fatalf("record once the grant arrived: %v", err)
	}
	record, ok := known.Get(denied)
	if !ok {
		t.Fatal("the store holds no record of the session written once the grant arrived")
	}
	if !slices.Equal(record.Topics, []string{topic}) {
		t.Errorf("the record's topics are %v, want the written %q", record.Topics, topic)
	}
	if warnings := strings.Count(logged(), "level=WARN"); warnings != 1 {
		t.Errorf("the store logged %d WARN lines, want the one the first failed open wrote and none per attempt:\n%s", warnings, logged())
	}
	if opens := strings.Count(logged(), "the known-session bucket is open"); opens != 1 {
		t.Errorf("the store logged the open %d times, want one line:\n%s", opens, logged())
	}
}

// A bucket opened after the start answers the records it already holds from the moment the store
// reports it open. The store is what the send route reads to tell a session Envoy knows from one
// it does not (cmd/listener/api.go, sendHandler) and what a publish counts its recipients
// against, so a store that reports an open bucket over a cache no watcher has filled answers
// `404 no live session` for a session the bucket knows and counts a publish's recipients low -
// the two failures this change removes, arriving at the moment the grant lands. The listener's
// start keeps that from happening by waiting on WaitForCacheReady before it opens /v1
// (cmd/listener/main.go); nothing waits on the attempt that opens the bucket later, so the
// store itself must not answer before its cache is filled.
func TestABucketOpenedAfterTheStartAnswersTheRecordsItHolds(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, knownBucketGrant()...)
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant carries the bucket: %v", err)
	}
	t.Cleanup(conn.Close)

	// The records a listener started before the grant finds waiting for it. Enough of them that
	// one watcher pass over the bucket is work: a cache filled before the store answers is the
	// whole question, and a handful of records would be filled whether it waits or not.
	const records = 2000
	const topic = "notifications.team.late-open"
	filling, err := OpenKnownSessions(conn, WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the store that fills the bucket: %v", err)
	}
	for index := range records {
		id := fmt.Sprintf("ses_late_open_%04d", index)
		if err := filling.RecordWithin(context.Background(), KnownSession{SessionID: id, Topics: []string{topic}}); err != nil {
			t.Fatalf("write the record of %s: %v", id, err)
		}
	}
	if err := filling.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the filling store's cache did not become ready: %v", err)
	}
	if held := len(matchedIDs(filling, topic)); held != records {
		t.Fatalf("the bucket holds %d records, want the %d written: this test proves nothing about a cache filled before the store answers", held, records)
	}
	filling.StopWatch()

	// The grant goes away, as it is away on every listener started before it is applied.
	grant.Grant(t, "nothing.this.store.uses")
	known, err := OpenKnownSessions(conn,
		WithKnownSessionReplicas(1),
		withKnownSessionRetry(100*time.Millisecond, 500*time.Millisecond))
	if err != nil {
		t.Fatalf("open the store over a bucket its user may not open: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if known.Opened() {
		t.Fatal("the store reports its bucket open while its user may not open it")
	}

	grant.Grant(t, knownBucketGrant()...)
	deadline := time.Now().Add(60 * time.Second)
	for !known.Opened() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !known.Opened() {
		t.Fatal("the store did not open the bucket within 60s of the grant")
	}
	if answered := len(matchedIDs(known, topic)); answered != records {
		t.Fatalf("the store answered %d of the %d records the bucket holds at the moment it reported its bucket open; until its cache is filled a send to each of the other sessions is refused 404 no live session and a publish counts them for nothing",
			answered, records)
	}
	last := fmt.Sprintf("ses_late_open_%04d", records-1)
	if _, ok := known.Get(last); !ok {
		t.Fatalf("the store answers no record for %s, the last the bucket was given, at the moment it reported its bucket open", last)
	}
}

// A store whose first scan nats.go's idle timer ended holds only the records that scan
// delivered, so it is no answer: Opened reads the watcher's completeness
// (kvwatch.Watcher.Complete), not its readiness, which such a scan releases too, and until a
// scan has delivered every record a publish's count is left out rather than counted against part
// of the bucket. The watcher scans the bucket again (startWatch's kvwatch.WithRescan), and the
// store reports its bucket open once that scan completes.
func TestAStoreWhoseFirstScanTimedOutOpensOnlyOnceItsRescanCompletes(t *testing.T) {
	client := setupNATS(t)
	const topic = "notifications.team.timed-out-scan"
	ids := []string{"ses_timeout_1", "ses_timeout_2", "ses_timeout_3"}
	filling, err := OpenKnownSessions(client.Conn, WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the store that fills the bucket: %v", err)
	}
	for _, id := range ids {
		if err := filling.RecordWithin(context.Background(), KnownSession{SessionID: id, Topics: []string{topic}}); err != nil {
			t.Fatalf("write the record of %s: %v", id, err)
		}
	}
	filling.StopWatch()
	raw, err := client.JS().KeyValue(bus.KnownSessionBucket)
	if err != nil {
		t.Fatalf("open the known-session bucket: %v", err)
	}

	// The store's own watcher, over a handle whose first watch stalls after one entry the way
	// that makes nats.go's idle timer give up on the scan.
	known := &KnownSessions{
		now:     time.Now,
		records: map[string]cachedKnown{},
		topics:  store.NewTopicMatcher(),
		stopped: make(chan struct{}),
	}
	t.Cleanup(known.StopWatch)
	known.startWatch(bus.KeyValue{KeyValue: kvwatchtest.TimingOutFirst(raw, 1)})
	watcher := known.watch()

	deadline := time.Now().Add(30 * time.Second)
	for !watcher.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !watcher.Ready() {
		t.Fatal("the cache never became ready, so this test says nothing about a scan the idle timer ended")
	}
	if known.Opened() {
		t.Fatal("the store reports its bucket open over a first scan the idle timer ended: a publish would count its recipients against the one record that scan delivered")
	}

	for !known.Opened() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !known.Opened() {
		t.Fatal("the store never reported its bucket open: nothing completed a scan after the idle timer ended the first")
	}
	if got := matchedIDs(known, topic); !slices.Equal(got, ids) {
		t.Fatalf("the store answered %v at the moment it reported its bucket open, want every record the bucket holds, %v", got, ids)
	}
}

// knownOverGrant opens a store whose bucket is open, on a server whose one user's grant the test
// takes away and gives back (testnats.NkeyGrant): while it is away every request the store makes
// is dropped by the server, so a move to a recovered connection fails as it fails against a
// cluster that refuses this listener - a grant rolled back, or a bucket deleted under its
// watcher - and succeeds again once it is back. The retry's waits are the test's, since the
// production schedule's first wait is a second and its cap five minutes.
func knownOverGrant(t *testing.T, first, longest time.Duration) (*KnownSessions, *nats.Conn, *testnats.NkeyGrant, func() string) {
	t.Helper()
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, knownBucketGrant()...)
	conn, err := testnats.ConnectSeed(t, grant.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant carries the bucket: %v", err)
	}
	t.Cleanup(conn.Close)
	var logs bytes.Buffer
	var logsMu sync.Mutex
	logged := func() string {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.String()
	}
	known, err := OpenKnownSessions(conn,
		WithKnownSessionReplicas(1),
		WithKnownSessionLogger(slog.New(slog.NewTextHandler(lockedWriter{&logs, &logsMu}, nil))),
		withKnownSessionRetry(first, longest))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if err := known.WaitForCacheReady(context.Background()); err != nil {
		t.Fatalf("the known-session cache did not become ready: %v", err)
	}
	if !known.Opened() {
		t.Fatal("the store does not report its bucket open before the grant is taken away")
	}
	return known, conn, grant, logged
}

// repairRunning reports whether the store's one background repair is running.
func repairRunning(known *KnownSessions) bool {
	known.repairMu.Lock()
	defer known.repairMu.Unlock()
	return known.repairing
}

// watchGeneration is the generation of the store's watcher (kvwatch.Watcher.Complete): a move
// that succeeded installs a watcher and moves it on, and a move that failed leaves it where it
// was. It is how a test tells a repair the store made from one it did not.
func watchGeneration(known *KnownSessions) uint64 {
	generation, _ := known.watch().Complete()
	return generation
}

// awaitKnown waits for what to hold, failing the test when it has not within the wait.
func awaitKnown(t *testing.T, within time.Duration, what string, holds func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !holds() {
		if time.Now().After(deadline) {
			t.Fatalf("%s within %s", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A move to a recovered connection that failed is repaired by the store itself, on its own
// backoff. Nothing else repairs it: the listener's reconnect hook answers nil over the error,
// since a cache no delivery reads must not report the listener degraded, so the bus counts the
// connection event as covered and runs no hook again until the next one, which can be days away
// (internal/bus/recovery.go); the store is kept out of listenerCaches, so the self-health
// rebuild passes it by; and the attempts that open a bucket that never opened are not this.
// Until the move is made the watcher and the write handle are on a connection the listener has
// left, so every record written is lost and a publish's recipient count is left out.
func TestAFailedMoveToARecoveredConnectionIsRepairedByTheStore(t *testing.T) {
	known, conn, grant, logged := knownOverGrant(t, 200*time.Millisecond, time.Second)
	before := watchGeneration(known)

	grant.Grant(t, "nothing.this.store.uses")
	if err := known.Rewatch(conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a move that failed")
	}
	if moved := watchGeneration(known); moved != before {
		t.Fatalf("the watcher moved to generation %d over a move that failed, want it left at %d", moved, before)
	}
	if !repairRunning(known) {
		t.Fatal("no repair runs after a move that failed: nothing else moves the watcher off the connection the listener has left")
	}

	// The cluster takes this listener back, and nothing asks the store to move again: no
	// connection event, no reconnect hook, no rebuild. The store's own retry is the whole repair.
	grant.Grant(t, knownBucketGrant()...)
	awaitKnown(t, 60*time.Second, "the store's own repair did not move the watcher", func() bool {
		return watchGeneration(known) > before
	})
	awaitKnown(t, 60*time.Second, "the repair did not retire once it had nothing left to repair", func() bool {
		return !repairRunning(known)
	})
	if lines := strings.Count(logged(), "the known-session cache is back"); lines != 1 {
		t.Errorf("the repair logged %d lines, want the one that reports the move it made:\n%s", lines, logged())
	}

	// The cache the repair left behind is the bucket's again: it reports the bucket open once the
	// moved watcher's scan has delivered every record, and the records written through it answer.
	awaitKnown(t, 60*time.Second, "the repaired store did not report its bucket open", known.Opened)
	const repaired = "ses_after_the_repair"
	const topic = "notifications.team.repaired-move"
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: repaired, Topics: []string{topic}}); err != nil {
		t.Fatalf("record through the repaired store: %v", err)
	}
	if got := matchedIDs(known, topic); !slices.Equal(got, []string{repaired}) {
		t.Errorf("the repaired store answered %v for the topic of the record just written, want %q", got, repaired)
	}
}

// The move the next connection event makes ends the retry, and the retry moves nothing after it:
// the connection event's move is the newer one, and a retry that moved the watcher again would
// put it on the handle of a connection the store has already left. The waits here are long
// enough that the connection event lands between the retry being armed and its first attempt,
// which is the window this guards.
func TestAMoveThatSucceedsEndsTheStoresRetry(t *testing.T) {
	known, conn, grant, logged := knownOverGrant(t, 5*time.Second, 5*time.Second)
	before := watchGeneration(known)

	grant.Grant(t, "nothing.this.store.uses")
	if err := known.Rewatch(conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a move that failed")
	}
	if !repairRunning(known) {
		t.Fatal("no repair runs after a move that failed")
	}

	grant.Grant(t, knownBucketGrant()...)
	if err := known.Rewatch(conn); err != nil {
		t.Fatalf("the move of the next connection event, once the cluster took this listener back: %v", err)
	}
	moved := watchGeneration(known)
	if moved <= before {
		t.Fatalf("the move that succeeded left the watcher at generation %d, want it past %d", moved, before)
	}
	awaitKnown(t, 60*time.Second, "the retry did not end after a move that succeeded", func() bool {
		return !repairRunning(known)
	})
	if again := watchGeneration(known); again != moved {
		t.Errorf("the retry moved the watcher again, to generation %d: the move the connection event made, at %d, is the newer one", again, moved)
	}
	if strings.Contains(logged(), "the known-session cache is back") {
		t.Errorf("the retry reported a repair the connection event had already made:\n%s", logged())
	}
}

// A stop ends the retry wherever it is, and a store that stopped repairs nothing afterwards: its
// watcher is retired for the drain, and a move it made would arm a watcher nothing ends.
func TestAStopEndsTheStoresRetry(t *testing.T) {
	known, conn, grant, _ := knownOverGrant(t, 200*time.Millisecond, time.Second)
	before := watchGeneration(known)

	grant.Grant(t, "nothing.this.store.uses")
	if err := known.Rewatch(conn); err == nil {
		t.Fatal("the move succeeded while the user's grant named none of the bucket's subjects; this test says nothing about a move that failed")
	}
	if moved := watchGeneration(known); moved != before {
		t.Fatalf("the watcher moved to generation %d over a move that failed, want it left at %d", moved, before)
	}
	if !repairRunning(known) {
		t.Fatal("no repair runs after a move that failed")
	}

	known.StopWatch()
	awaitKnown(t, 60*time.Second, "the retry did not end at the stop", func() bool {
		return !repairRunning(known)
	})
	// The stop retires the watcher and moves the generation on itself (kvwatch.Watcher.Stop), so
	// a move made after it is one past this.
	retired := watchGeneration(known)

	// The cluster takes the listener back, past several of the retry's waits: a store that
	// stopped arms nothing, and moves nothing.
	grant.Grant(t, knownBucketGrant()...)
	time.Sleep(2 * time.Second)
	if repairRunning(known) {
		t.Error("a repair was armed after the stop")
	}
	if moved := watchGeneration(known); moved != retired {
		t.Errorf("a stopped store moved its watcher to generation %d, want it left at %d", moved, retired)
	}
}

// knownSessionMemoryBound is what the records and the subject index of 100,000 sessions may cost
// in memory. Each session here carries its agent subject, which no other session shares, and one
// of ten team topics, which is the shape the store holds in production: one unique pattern per
// session plus a handful of shared ones. The measurement on this machine (linux/amd64, go1.27)
// was 84,756,680 bytes, 80.8 MiB, 847 bytes a record. The bound is 128 MiB: room for the
// allocator's own slack and for a Go release that grows a map's header, while still failing a
// change that makes a record or an index node cost half as much again.
const knownSessionMemoryBound = 128 << 20

// knownSessionMemoryRecords is the number of records the bound above is measured over: the order
// of magnitude a publish count must answer against (~100,000 sessions in 30 days).
const knownSessionMemoryRecords = 100_000

func TestKnownSessionsMemoryForAHundredThousandRecords(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	known := &KnownSessions{
		now:     time.Now,
		records: make(map[string]cachedKnown),
		topics:  store.NewTopicMatcher(),
	}
	now := time.Now().UnixMilli()
	known.mu.Lock()
	for i := range knownSessionMemoryRecords {
		sessionID := fmt.Sprintf("ses_%024d", i)
		known.applyLocked(sessionID, KnownSession{
			SessionID:    sessionID,
			Title:        fmt.Sprintf("session %d of the memory measurement", i),
			Capabilities: []string{"aside"},
			Topics:       []string{"notifications.agent." + sessionID, fmt.Sprintf("notifications.team.squad-%d", i%10)},
			LastSeen:     now,
		}, uint64(i+1))
	}
	known.mu.Unlock()

	runtime.GC()
	runtime.ReadMemStats(&after)
	held := after.HeapAlloc - before.HeapAlloc
	runtime.KeepAlive(known)

	known.mu.RLock()
	stored := len(known.records)
	known.mu.RUnlock()
	if stored != knownSessionMemoryRecords {
		t.Fatalf("the store holds %d records, want %d", stored, knownSessionMemoryRecords)
	}
	if got := matchedIDs(known, "notifications.team.squad-3"); len(got) != knownSessionMemoryRecords/10 {
		t.Fatalf("the team topic matched %d records, want %d", len(got), knownSessionMemoryRecords/10)
	}
	t.Logf("%d records and their subject index hold %d bytes (%.1f MiB), %d bytes each",
		knownSessionMemoryRecords, held, float64(held)/(1<<20), held/knownSessionMemoryRecords)
	if held > knownSessionMemoryBound {
		t.Errorf("%d records hold %d bytes, above the %d-byte bound", knownSessionMemoryRecords, held, knownSessionMemoryBound)
	}
}
