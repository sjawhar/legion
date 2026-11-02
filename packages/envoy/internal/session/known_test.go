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

	"github.com/sjawhar/envoy/internal/bus"
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

// matchedIDs is the session ids Match answers for topic.
func matchedIDs(known *KnownSessions, topic string) []string {
	ids := []string{}
	for _, record := range known.Match(topic) {
		ids = append(ids, record.SessionID)
	}
	return ids
}

// A record written to the bucket is answered from memory by id and by topic, with the topics it
// was written with - a team topic no stream subject captures among them.
func TestKnownSessionsAnswerByIDAndByTopic(t *testing.T) {
	known := openKnown(t, time.Now)
	topics := []string{"notifications.agent.ses_known", "notifications.team.deploy-squad"}
	if err := known.RecordWithin(context.Background(), KnownSession{
		SessionID:        "ses_known",
		Title:            "a known session",
		Capabilities:     []string{"aside"},
		Topics:           topics,
		ClientInstanceID: "inst_one",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	record, ok := known.Get("ses_known")
	if !ok {
		t.Fatal("Get answered no record for the session just written")
	}
	if record.Title != "a known session" || record.ClientInstanceID != "inst_one" || !slices.Equal(record.Topics, topics) {
		t.Fatalf("record = %#v, want the written title, instance and topics", record)
	}
	if record.LastSeen <= 0 {
		t.Fatalf("record.LastSeen = %d, want the write's own stamp", record.LastSeen)
	}
	if got := matchedIDs(known, "notifications.team.deploy-squad"); !slices.Equal(got, []string{"ses_known"}) {
		t.Errorf("Match of the team topic = %v, want the session", got)
	}
	if got := matchedIDs(known, "notifications.team.other"); len(got) != 0 {
		t.Errorf("Match of an unfollowed topic = %v, want none", got)
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
		t.Errorf("Match answered %v for a record older than the retention", got)
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
	if got := known.Match("notifications.team.deploy-squad"); len(got) != 0 {
		t.Errorf("Match on a nil store = %v, want none", got)
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
	for known.watch() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if known.watch() == nil {
		t.Fatalf("the store did not open the bucket within 30s of the grant:\n%s", logged())
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

// knownSessionMemoryBound is what the records and the subject index of 100,000 sessions may cost
// in memory. Each session here carries its agent subject, which no other session shares, and one
// of ten team topics, which is the shape the store holds in production: one unique pattern per
// session plus a handful of shared ones. The measurement on this machine (linux/amd64, go1.27)
// was 89,254,392 bytes, 85.1 MiB, 892 bytes a record, and it moved by under 200 KiB over three
// runs. The bound is 128 MiB: room for the allocator's own slack and for a Go release that grows
// a map's header, while still failing a change that makes a record or an index node cost half as
// much again.
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
			SessionID:        sessionID,
			Title:            fmt.Sprintf("session %d of the memory measurement", i),
			Capabilities:     []string{"aside"},
			Topics:           []string{"notifications.agent." + sessionID, fmt.Sprintf("notifications.team.squad-%d", i%10)},
			ClientInstanceID: fmt.Sprintf("inst_%016d", i%1000),
			LastSeen:         now,
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
