package cistore

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

// headGatedState is State as the head-gated listeners decode and write it: legion 1ad2466c, and
// 523f0ebd (the parent of #1526), byte for byte in State, its UnmarshalJSON, Check, Check's
// UnmarshalJSON, Suite and SettlementClaim; legion 9de053a2, the listener production runs, has the
// same shape less Overflowed (liveHeadGatedState). Check, Suite and SettlementClaim are those
// types unchanged, so this reuses them.
type headGatedState struct {
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
	Overflowed     bool             `json:"overflowed,omitempty"`
}

func (state *headGatedState) UnmarshalJSON(data []byte) error {
	type stateAlias headGatedState
	*state = headGatedState{}
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

// liveHeadGatedState is State at legion 9de053a2.
type liveHeadGatedState struct {
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
}

func (state *liveHeadGatedState) UnmarshalJSON(data []byte) error {
	type stateAlias liveHeadGatedState
	*state = liveHeadGatedState{}
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

// The summary loop reports the backlog it holds back: its gauge carries each tick's count of held
// records, settled ones excluded, and the first tick that holds any logs that count once.
func TestTheHeldBacklogIsCountedAndLoggedOnce(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	debounce := 20 * time.Millisecond
	old := time.Now().Add(-65 * time.Hour).UnixMilli()
	touched := head{legacyHead.owner, legacyHead.repo, "44", "7ac4ed000000000000000000000000000000beef"}
	putHeadGatedRecord(t, store, legacyHead, old, nil)
	putHeadGatedRecord(t, store, touched, old, nil)
	putHeadGatedRecord(t, store, head{legacyHead.owner, legacyHead.repo, "45", "5e7713000000000000000000000000000000beef"}, old,
		func(st *State) { st.SettledEmitted = true; st.EmittedCount = 1 })
	met := metrics.New()
	backlog := &backlogHeld{gauge: met.NewGauge("envoy_ci_legacy_records_held", "held")}
	var logs bytes.Buffer
	logger := logging.NewWithWriter("test", &logs)
	gauge := func() string {
		recorder := httptest.NewRecorder()
		met.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
		for _, line := range strings.Split(recorder.Body.String(), "\n") {
			if strings.HasPrefix(line, "envoy_ci_legacy_records_held ") {
				return line
			}
		}
		return ""
	}
	const logLine = "checks held back a head-gated listener's unsettled records"

	backlog.observe(runSummaryTick(store, pub, debounce, logger), logger)
	backlog.observe(runSummaryTick(store, pub, debounce, logger), logger)
	if got := gauge(); got != "envoy_ci_legacy_records_held 2" {
		t.Fatalf("gauge after two ticks over the backlog = %q, want 2 held", got)
	}
	if n := strings.Count(logs.String(), logLine); n != 1 || !strings.Contains(logs.String(), `"records":2`) {
		t.Fatalf("held-backlog log lines = %d, want one naming 2 records:\n%s", n, logs.String())
	}

	if err := record(store, touched.check("lint", 706, "completed", "success", "2026-09-28T05:00:00Z")); err != nil {
		t.Fatalf("record the new check run: %v", err)
	}
	waitCacheChecks(t, store, touched.owner, touched.repo, touched.number, touched.sha, 2)
	time.Sleep(2 * debounce)
	backlog.observe(runSummaryTick(store, pub, debounce, logger), logger)
	if got := gauge(); got != "envoy_ci_legacy_records_held 1" || pub.count() != 1 {
		t.Fatalf("after the touched record settled: gauge %q, %d settlements, want 1 held and 1 settlement", got, pub.count())
	}
	if n := strings.Count(logs.String(), logLine); n != 1 {
		t.Fatalf("held-backlog log lines = %d after a later tick, want still one", n)
	}
}

// A head-gated listener - 1ad2466c, or 9de053a2, which production runs - reads a stamped record as
// the record it is: the stamp is a field neither decodes, and neither mistakes the record for its
// per-pull-request head record (it tells those apart by `kind`). Its own write of the record drops
// the stamp, so a record it touched after a rollback reads as its own again.
// It decodes records the current encoder writes, so an encoder change that breaks a rollback turns
// it red.
func TestAHeadGatedListenerDecodesAStampedRecord(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	if err := record(store, legacyHead.check("build", 704, "completed", "success", "2026-09-28T05:00:00Z")); err != nil {
		t.Fatalf("record the check run: %v", err)
	}
	if err := record(store, legacyHead.suite("9002", "completed", "success", "2026-09-28T05:00:00Z")); err != nil {
		t.Fatalf("record the suite: %v", err)
	}
	waitCacheSuites(t, store, legacyHead.owner, legacyHead.repo, legacyHead.number, legacyHead.sha, 1)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("published %d settlements, want 1", pub.count())
	}
	entry, err := store.watcher.KV().Get(legacyHead.key())
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	raw := entry.Value()
	if schema, _ := storedSchema(t, store, legacyHead.key()); schema != 1 {
		t.Fatalf("stored schema = %d, want 1: %s", schema, raw)
	}
	current := getState(t, store, legacyHead.owner, legacyHead.repo, legacyHead.number, legacyHead.sha)

	var marker struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil || marker.Kind == retiredHeadKind {
		t.Fatalf("a head-gated listener's head-record check reads kind %q (%v), want a commit record", marker.Kind, err)
	}
	var rollback headGatedState
	if err := json.Unmarshal(raw, &rollback); err != nil {
		t.Fatalf("1ad2466c's State decodes the stamped record: %v", err)
	}
	var live liveHeadGatedState
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatalf("9de053a2's State decodes the stamped record: %v", err)
	}
	for name, got := range map[string]headGatedState{
		"1ad2466c": rollback,
		"9de053a2": {Owner: live.Owner, Repo: live.Repo, Number: live.Number, SHA: live.SHA, Checks: live.Checks, Suites: live.Suites, LastEventAt: live.LastEventAt, Generation: live.Generation, EmittedCount: live.EmittedCount, SettledEmitted: live.SettledEmitted, Claim: live.Claim},
	} {
		if got.Owner != current.Owner || got.Repo != current.Repo || got.Number != current.Number || got.SHA != current.SHA ||
			got.LastEventAt != current.LastEventAt || got.Generation != current.Generation ||
			got.EmittedCount != current.EmittedCount || got.SettledEmitted != current.SettledEmitted ||
			got.Claim != nil || len(got.Checks) != 1 || got.Checks["build"] != current.Checks["build"] ||
			len(got.Suites) != 1 || got.Suites["9002"] != current.Suites["9002"] {
			t.Fatalf("%s decodes %+v, want the record %+v", name, got, current)
		}
	}

	rewritten, err := json.Marshal(rollback)
	if err != nil {
		t.Fatalf("encode through 1ad2466c's State: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rewritten, &fields); err != nil {
		t.Fatalf("decode the rolled-back write: %v", err)
	}
	if _, stamped := fields["schema"]; stamped {
		t.Fatalf("the rolled-back write kept the stamp: %s", rewritten)
	}
	var reread State
	if err := json.Unmarshal(rewritten, &reread); err != nil {
		t.Fatalf("decode the rolled-back write: %v", err)
	}
	if !reread.SettledEmitted || reread.Hash() != current.Hash() {
		t.Fatalf("the rolled-back write reads back as %+v, want the settled record", reread)
	}
}
