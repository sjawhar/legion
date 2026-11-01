package session

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/store"
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
	seen := 0
	known.Each(func(KnownSession) { seen++ })
	if seen != 1 || known.Count() != 1 {
		t.Errorf("Each visited %d and Count = %d, want one record each", seen, known.Count())
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

// A record whose last_seen is past the 30-day retention answers as no record, by id, by topic and
// in a walk, before the sweep has run; the sweep then gives its memory back.
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
	known.Each(func(record KnownSession) { t.Errorf("Each visited %q, a record older than the retention", record.SessionID) })
	if known.Count() != 0 {
		t.Errorf("Count = %d, want none", known.Count())
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

// Every method is safe on a nil store, the listener that could not open the bucket: no record, no
// match, no walk, and writes that do nothing.
func TestNilKnownSessionsAnswersNothingAndPanicsNowhere(t *testing.T) {
	var known *KnownSessions
	if record, ok := known.Get("ses_any"); ok || record.SessionID != "" {
		t.Errorf("Get on a nil store = %#v, %v, want no record", record, ok)
	}
	if got := known.Match("notifications.team.deploy-squad"); len(got) != 0 {
		t.Errorf("Match on a nil store = %v, want none", got)
	}
	known.Each(func(record KnownSession) { t.Errorf("Each on a nil store visited %#v", record) })
	if known.Count() != 0 {
		t.Errorf("Count on a nil store = %d, want 0", known.Count())
	}
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_any"}); err != nil {
		t.Errorf("RecordWithin on a nil store = %v, want nil", err)
	}
	if err := known.TouchWithin(context.Background(), "ses_any"); err != nil {
		t.Errorf("TouchWithin on a nil store = %v, want nil", err)
	}
	if err := known.Ping(); err != nil {
		t.Errorf("Ping on a nil store = %v, want nil", err)
	}
	if err := known.WatchErr(); err != nil {
		t.Errorf("WatchErr on a nil store = %v, want nil", err)
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

	if known.Count() != knownSessionMemoryRecords {
		t.Fatalf("the store holds %d records, want %d", known.Count(), knownSessionMemoryRecords)
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
