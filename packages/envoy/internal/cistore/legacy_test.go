package cistore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
)

// putHeadGatedRecord stores the record a head-gated listener leaves for a commit it did not
// settle: every check and suite completed, unsettled, last observed at lastEventAt, and no schema,
// so it encodes to the bytes that listener wrote. edit adjusts it first.
func putHeadGatedRecord(t *testing.T, s *Store, h head, lastEventAt int64, edit func(*State)) {
	t.Helper()
	record := State{
		Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha,
		Checks: map[string]Check{
			"build": {Name: "build", CheckRunID: 700, URL: "https://example-host/runs/700", Status: "completed", Conclusion: "success", ObservedAt: "2026-09-25T10:00:00Z"},
		},
		Suites: map[string]Suite{
			"9001": {ID: "9001", AppID: "15368", Status: "completed", Conclusion: "success", ObservedAt: "2026-09-25T10:00:00Z"},
		},
		LastEventAt: lastEventAt,
		Generation:  2,
	}
	if edit != nil {
		edit(&record)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode the head-gated record: %v", err)
	}
	if _, err := s.watcher.KV().Put(h.key(), raw); err != nil {
		t.Fatalf("store the head-gated record: %v", err)
	}
	waitCached(t, s, h.key(), func(st State) bool { return st.LastEventAt == lastEventAt })
}

// waitCached polls the cache until key's record satisfies ok.
func waitCached(t *testing.T, s *Store, key string, ok func(State) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		for _, st := range s.List() {
			if Key(st.Owner, st.Repo, st.Number, st.SHA) == key && ok(st) {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("cache never reached the expected record for %s", key)
		case <-time.After(15 * time.Millisecond):
		}
	}
}

// storedSchema is the schema field of key's stored record as the bucket holds it, 0 when absent.
func storedSchema(t *testing.T, s *Store, key string) (int, uint64) {
	t.Helper()
	entry, err := s.watcher.KV().Get(key)
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	var fields struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(entry.Value(), &fields); err != nil {
		t.Fatalf("decode the stored record: %v", err)
	}
	return fields.Schema, entry.Revision()
}

var legacyHead = head{"example-org", "example-repo", "42", "1e9ac1000000000000000000000000000000beef"}

// The backlog a head-gated listener leaves - commits it never settled because they were not their
// pull request's head, some days old - is not published when this listener takes over, including a
// record whose claim the old listener left behind, which the tick must not reclaim into a
// settlement either.
func TestAHeadGatedListenersUnsettledRecordPastTheGraceIsNotSettled(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	debounce := 50 * time.Millisecond
	old := time.Now().Add(-65 * time.Hour).UnixMilli()
	claimed := head{legacyHead.owner, legacyHead.repo, "43", "c1a1c1000000000000000000000000000000beef"}
	putHeadGatedRecord(t, store, legacyHead, old, nil)
	putHeadGatedRecord(t, store, claimed, old, func(st *State) {
		st.Claim = &SettlementClaim{Hash: "stale", Generation: st.Generation, ClaimedAt: old}
	})
	_, before := storedSchema(t, store, legacyHead.key())
	_, beforeClaimed := storedSchema(t, store, claimed.key())

	runSummaryTick(store, pub, debounce, logging.New("test"))
	runSummaryTick(store, pub, debounce, logging.New("test"))

	if got := pub.count(); got != 0 {
		t.Fatalf("the head-gated backlog published %d settlements, want none: %+v", got, pub.all())
	}
	if _, after := storedSchema(t, store, legacyHead.key()); after != before {
		t.Fatalf("the backlog record was written (revision %d -> %d), want it left as it was", before, after)
	}
	if _, after := storedSchema(t, store, claimed.key()); after != beforeClaimed {
		t.Fatalf("the backlog record's stale claim was reclaimed (revision %d -> %d), want it left as it was", beforeClaimed, after)
	}
	cached := getState(t, store, legacyHead.owner, legacyHead.repo, legacyHead.number, legacyHead.sha)
	if _, claimedNow, err := store.ClaimSettlement(legacyHead.key(), cached.Hash(), cached.Generation, time.Now().UnixMilli(), debounce); err != nil || claimedNow {
		t.Fatalf("ClaimSettlement on the durable backlog record = (%v, %v), want refused", claimedNow, err)
	}
}

// A new check run on a commit the head-gated listener left unsettled stamps its record through the
// ordinary record path, and the commit then settles once, carrying the new run.
func TestANewCheckRunOnAHeadGatedRecordSettlesItOnce(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	debounce := 20 * time.Millisecond
	putHeadGatedRecord(t, store, legacyHead, time.Now().Add(-30*time.Hour).UnixMilli(), nil)
	runSummaryTick(store, pub, debounce, logging.New("test"))
	if got := pub.count(); got != 0 {
		t.Fatalf("the untouched backlog record published %d settlements, want none", got)
	}

	if err := record(store, legacyHead.check("lint", 701, "completed", "success", "2026-09-28T05:00:00Z")); err != nil {
		t.Fatalf("record the new check run: %v", err)
	}
	if schema, _ := storedSchema(t, store, legacyHead.key()); schema != recordSchema {
		t.Fatalf("stored schema after the new check run = %d, want %d", schema, recordSchema)
	}
	waitCacheChecks(t, store, legacyHead.owner, legacyHead.repo, legacyHead.number, legacyHead.sha, 2)
	time.Sleep(2 * debounce)
	runSummaryTick(store, pub, debounce, logging.New("test"))
	runSummaryTick(store, pub, debounce, logging.New("test"))

	if got := pub.count(); got != 1 {
		t.Fatalf("the touched record published %d settlements, want 1", got)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode the settlement: %v", err)
	}
	assertCheckRuns(t, summary.CheckRuns, map[string]uint64{"build": 700, "lint": 701})
	if !getState(t, store, legacyHead.owner, legacyHead.repo, legacyHead.number, legacyHead.sha).SettledEmitted {
		t.Fatal("the touched record was not marked settled")
	}
}

// A head the head-gated listener had not yet settled when it was replaced - its debounce ran out
// in the handover - still settles.
func TestAHeadGatedRecordInsideTheGraceSettles(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	debounce := 50 * time.Millisecond
	putHeadGatedRecord(t, store, legacyHead, time.Now().Add(-10*time.Second).UnixMilli(), nil)

	runSummaryTick(store, pub, debounce, logging.New("test"))
	runSummaryTick(store, pub, debounce, logging.New("test"))

	if got := pub.count(); got != 1 {
		t.Fatalf("the handover head published %d settlements, want 1", got)
	}
	if st := getState(t, store, legacyHead.owner, legacyHead.repo, legacyHead.number, legacyHead.sha); !st.SettledEmitted {
		t.Fatalf("the handover head's record = %+v, want settled", st)
	}
}

// A record this listener wrote that was still waiting to settle when it restarted settles after
// the restart however long it waited: the grace applies only to records without the stamp.
func TestAStampedRecordPendingAcrossARestartSettles(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	before := openStore(t, conn)
	if err := record(before, legacyHead.check("build", 702, "completed", "failure", "2026-09-25T10:00:00Z")); err != nil {
		t.Fatalf("record the check run: %v", err)
	}
	waitCacheChecks(t, before, legacyHead.owner, legacyHead.repo, legacyHead.number, legacyHead.sha, 1)
	setLastEventAt(t, before, legacyHead.owner, legacyHead.repo, legacyHead.number, legacyHead.sha, time.Now().Add(-72*time.Hour).UnixMilli())
	before.StopWatch()

	after := openStore(t, conn)
	pub := &recPub{}
	runSummaryTick(after, pub, 50*time.Millisecond, logging.New("test"))
	runSummaryTick(after, pub, 50*time.Millisecond, logging.New("test"))

	if got := pub.count(); got != 1 {
		t.Fatalf("the stamped pending record published %d settlements after the restart, want 1", got)
	}
}

// A settled record is never published again, stamped or not, recent or old.
func TestASettledRecordIsNotPublishedAgain(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	debounce := 20 * time.Millisecond
	now := time.Now()
	settled := func(st *State) { st.SettledEmitted = true; st.EmittedCount = 1 }
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "50", "01d5e7000000000000000000000000000000beef"}, now.Add(-65*time.Hour).UnixMilli(), settled)
	putHeadGatedRecord(t, store, head{"example-org", "example-repo", "51", "4ece47000000000000000000000000000000beef"}, now.Add(-10*time.Second).UnixMilli(), settled)

	stamped := head{"example-org", "example-repo", "52", "57a3be000000000000000000000000000000beef"}
	if err := record(store, stamped.check("build", 703, "completed", "success", "2026-09-28T05:00:00Z")); err != nil {
		t.Fatalf("record the check run: %v", err)
	}
	waitCacheChecks(t, store, stamped.owner, stamped.repo, stamped.number, stamped.sha, 1)
	time.Sleep(2 * debounce)
	runSummaryTick(store, pub, debounce, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("the stamped record's first settlement published %d envelopes, want 1", got)
	}
	setLastEventAt(t, store, stamped.owner, stamped.repo, stamped.number, stamped.sha, now.Add(-72*time.Hour).UnixMilli())

	runSummaryTick(store, pub, debounce, logging.New("test"))
	runSummaryTick(store, pub, debounce, logging.New("test"))

	if got := pub.count(); got != 1 {
		t.Fatalf("settled records published %d envelopes in all, want only the stamped record's first", got)
	}
}
