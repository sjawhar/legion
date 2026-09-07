package cistore

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

type recPub struct {
	mu            sync.Mutex
	items         []contracts.Envelope
	err           error
	beforePublish func()
	onPublish     func(contracts.Envelope)
}

func (p *recPub) Publish(e contracts.Envelope) error {
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return p.err
	}
	beforePublish := p.beforePublish
	onPublish := p.onPublish
	p.mu.Unlock()
	if beforePublish != nil {
		beforePublish()
	}
	p.mu.Lock()
	p.items = append(p.items, e)
	p.mu.Unlock()
	if onPublish != nil {
		onPublish(e)
	}
	return nil
}

func (p *recPub) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

func (p *recPub) last() contracts.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.items[len(p.items)-1]
}

func (p *recPub) all() []contracts.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contracts.Envelope(nil), p.items...)
}

func (p *recPub) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// waitCacheChecks polls the WatchAll cache until the commit shows n checks.
func waitCacheChecks(t *testing.T, s *Store, owner, repo, number, sha string, n int) {
	t.Helper()
	key := Key(owner, repo, number, sha)
	deadline := time.After(5 * time.Second)
	for {
		for _, st := range s.List() {
			if Key(st.Owner, st.Repo, st.Number, st.SHA) == key && len(st.Checks) == n {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("cache never reached %d checks for %s", n, key)
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func waitHead(t *testing.T, s *Store, owner, repo, number, sha string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if head, ok := s.Head(owner, repo, number); ok && head == sha {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("head never reached %s for %s/%s#%s", sha, owner, repo, number)
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func waitCacheSuites(t *testing.T, s *Store, owner, repo, number, sha string, n int) {
	t.Helper()
	key := Key(owner, repo, number, sha)
	deadline := time.After(5 * time.Second)
	for {
		for _, st := range s.List() {
			if Key(st.Owner, st.Repo, st.Number, st.SHA) == key && len(st.Suites) == n {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("cache never reached %d suites for %s", n, key)
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func waitCacheStateMissing(t *testing.T, s *Store, owner, repo, number, sha string) {
	t.Helper()
	key := Key(owner, repo, number, sha)
	deadline := time.After(5 * time.Second)
	for {
		found := false
		for _, st := range s.List() {
			if Key(st.Owner, st.Repo, st.Number, st.SHA) == key {
				found = true
				break
			}
		}
		if !found {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("cache never evicted %s", key)
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func setLastEventAt(t *testing.T, store *Store, owner, repo, number, sha string, at int64) {
	t.Helper()
	key := Key(owner, repo, number, sha)
	entry, err := store.kv.Get(key)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	var state State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	state.LastEventAt = at
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	if _, err := store.kv.Update(key, raw, entry.Revision()); err != nil {
		t.Fatalf("set last event at: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		for _, cached := range store.List() {
			if Key(cached.Owner, cached.Repo, cached.Number, cached.SHA) == key &&
				cached.LastEventAt == at &&
				cached.Generation == state.Generation &&
				cached.SettledEmitted == state.SettledEmitted {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("cache never applied last_event_at %d for %s", at, key)
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func setClaim(t *testing.T, store *Store, owner, repo, number, sha string, claim *SettlementClaim) {
	t.Helper()
	key := Key(owner, repo, number, sha)
	entry, err := store.kv.Get(key)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	var state State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	state.Claim = claim
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	if _, err := store.kv.Update(key, raw, entry.Revision()); err != nil {
		t.Fatalf("set claim: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		for _, cached := range store.List() {
			if Key(cached.Owner, cached.Repo, cached.Number, cached.SHA) != key {
				continue
			}
			if (cached.Claim == nil) == (claim == nil) {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("cache never applied claim for %s", key)
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func TestSummaryTickPublishesOneChecksEnvelopeWithoutSuites(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "800", "https://example-host/checks/800", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("published %d envelopes, want one checks envelope", pub.count())
	}
	env := pub.last()
	if env.Topic != "notifications.github.example-org.example-repo.pr.42.checks" {
		t.Fatalf("topic = %q, want checks", env.Topic)
	}
	if strings.Contains(env.Topic, ".ci") || strings.Contains(env.Topic, ".check.") {
		t.Fatalf("obsolete CI topic published: %q", env.Topic)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(env.Payload), &summary); err != nil {
		t.Fatalf("decode checks payload: %v", err)
	}
	if summary.Kind != "checks" {
		t.Fatalf("payload kind = %q, want checks", summary.Kind)
	}
	t.Logf("topic: %s", env.Topic)
	t.Logf("payload_summary: %s", env.PayloadSummary)
	t.Logf("payload: %s", env.Payload)
}

func TestSummaryTickDoesNotPublishWhenHeadMovesBeforeClaim(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		shaA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	if err := store.RecordHead(owner, repo, pr, shaA, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record initial head: %v", err)
	}
	waitHead(t, store, owner, repo, pr, shaA)
	if err := recordCheck(store, owner, repo, pr, shaA, "build", "800", "https://example.test/800", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, pr, shaA, 1)

	key := Key(owner, repo, pr, shaA)
	originalKV := store.kv
	var moved sync.Once
	store.kv = &interleavingKV{
		KeyValue: originalKV,
		afterGet: func(got string) {
			if got != key {
				return
			}
			moved.Do(func() {
				entry, err := originalKV.Get(headKey(owner, repo, pr))
				if err != nil {
					t.Fatalf("get durable head: %v", err)
				}
				raw, err := json.Marshal(headRecord{Kind: headRecordKind, SHA: shaB, UpdatedAt: "2026-09-07T03:00:01Z"})
				if err != nil {
					t.Fatalf("encode replacement head: %v", err)
				}
				if _, err := originalKV.Update(headKey(owner, repo, pr), raw, entry.Revision()); err != nil {
					t.Fatalf("move durable head: %v", err)
				}
			})
		},
	}
	t.Cleanup(func() { store.kv = originalKV })

	runSummaryTick(store, pub, 0, logging.New("test"))
	if got := pub.count(); got != 0 {
		t.Fatalf("head-raced state published %d envelopes, want none", got)
	}
}
func TestSummaryTickTreatsUnknownHeadAsStateHead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := recordCheck(store, owner, repo, number, sha, "build", "806", "https://example-host/checks/806", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("unknown PR head published %d checks envelopes, want one", pub.count())
	}
	if topic := pub.last().Topic; topic != "notifications.github.example-org.example-repo.pr.42.checks" {
		t.Fatalf("topic = %q, want checks topic", topic)
	}
}

func TestSummaryTickRequiresCompletedSuitesWhenRecorded(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "801", "https://example-host/checks/801", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	if err := recordSuite(store, owner, repo, number, sha, "900", "in_progress", "", "77", ""); err != nil {
		t.Fatalf("record in-progress suite: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	waitCacheSuites(t, store, owner, repo, number, sha, 1)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("in-progress suite allowed %d checks envelopes", pub.count())
	}

	if err := recordSuite(store, owner, repo, number, sha, "900", "completed", "success", "77", ""); err != nil {
		t.Fatalf("complete suite: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("completed suite published %d envelopes, want checks", pub.count())
	}

	if err := recordSuite(store, owner, repo, number, sha, "900", "in_progress", "", "77", ""); err != nil {
		t.Fatalf("reopen suite: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("reopened suite published %d envelopes", pub.count())
	}

	if err := recordSuite(store, owner, repo, number, sha, "900", "completed", "success", "77", ""); err != nil {
		t.Fatalf("recomplete suite: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 2 {
		t.Fatalf("recompleted suite published %d envelopes, want 2", pub.count())
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode re-settled payload: %v", err)
	}
	if !strings.HasSuffix(pub.last().PayloadSummary, " (re-settled)") {
		t.Fatalf("re-settled summary = %q", pub.last().PayloadSummary)
	}
	t.Logf("re-settled topic: %s", pub.last().Topic)
	t.Logf("re-settled payload_summary: %s", pub.last().PayloadSummary)
	t.Logf("re-settled payload: %s", pub.last().Payload)
	if summary.SupersededSettlement != "true" {
		t.Fatalf("superseded_settlement = %q, want true", summary.SupersededSettlement)
	}
}

func TestSummaryTickRearmsGenerationForCheckAndNewSuite(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "802", "https://example-host/checks/802", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("initial checks publish = %d, want 1", got)
	}
	first := pub.last()
	initial := getState(t, store, owner, repo, number, sha)
	gen0 := initial.Generation
	if !initial.SettledEmitted || initial.EmittedCount != 1 {
		t.Fatalf("initial settled state = %+v, want one emitted settlement", initial)
	}

	if err := recordCheck(store, owner, repo, number, sha, "build", "803", "https://example-host/checks/803", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record rerun check: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("check re-arm published %d envelopes, want 2", got)
	}
	second := pub.last()
	if second.DedupeKey == first.DedupeKey {
		t.Fatalf("re-settlement dedupe key = %q, want a new generation key", second.DedupeKey)
	}
	rearmedForCheck := getState(t, store, owner, repo, number, sha)
	gen1 := rearmedForCheck.Generation
	if gen1 <= gen0 || !rearmedForCheck.SettledEmitted || rearmedForCheck.EmittedCount <= initial.EmittedCount {
		t.Fatalf("check re-armed state = %+v, want a newer version and another emitted settlement", rearmedForCheck)
	}

	if err := recordSuite(store, owner, repo, number, sha, "900", "completed", "success", "77", ""); err != nil {
		t.Fatalf("record new suite: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 3 {
		t.Fatalf("new-suite re-arm published %d envelopes, want 3", got)
	}
	third := pub.last()
	if third.DedupeKey == second.DedupeKey {
		t.Fatalf("new-suite dedupe key = %q, want a new generation key", third.DedupeKey)
	}
	rearmedForSuite := getState(t, store, owner, repo, number, sha)
	gen2 := rearmedForSuite.Generation
	if gen2 <= gen1 || !rearmedForSuite.SettledEmitted || rearmedForSuite.EmittedCount <= rearmedForCheck.EmittedCount {
		t.Fatalf("new-suite re-armed state = %+v, want a newer version and another emitted settlement", rearmedForSuite)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(third.Payload), &summary); err != nil {
		t.Fatalf("decode re-settled payload: %v", err)
	}
	if summary.SupersededSettlement != "true" {
		t.Fatalf("superseded_settlement = %q, want true", summary.SupersededSettlement)
	}
	if !strings.HasSuffix(third.PayloadSummary, " (re-settled)") {
		t.Fatalf("re-settled summary = %q", third.PayloadSummary)
	}
}
func TestSummaryTickCarriesGenerationWhenSettlementsShareTheirHighestRun(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "900", "https://example.test/900", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record build: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))

	if err := recordCheck(store, owner, repo, number, sha, "lint", "850", "https://example.test/850", "completed", "failure", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record failed lint: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("settlement count = %d, want 2", got)
	}

	type payload struct {
		CheckRuns            []CheckRunRef `json:"check_runs"`
		Generation           *uint64       `json:"generation"`
		SupersededSettlement string        `json:"superseded_settlement"`
		Failed               StatusGroup   `json:"failed"`
		Passed               StatusGroup   `json:"passed"`
	}
	var first, second payload
	items := pub.all()
	if err := json.Unmarshal([]byte(items[0].Payload), &first); err != nil {
		t.Fatalf("decode first settlement: %v", err)
	}
	if err := json.Unmarshal([]byte(items[1].Payload), &second); err != nil {
		t.Fatalf("decode second settlement: %v", err)
	}
	assertCheckRuns(t, first.CheckRuns, map[string]uint64{"build": 900})
	assertCheckRuns(t, second.CheckRuns, map[string]uint64{"build": 900, "lint": 850})
	if first.Generation == nil || first.SupersededSettlement != "" ||
		first.Passed.Count != 1 || strings.Join(first.Passed.Checks, ",") != "build" || first.Failed.Count != 0 {
		t.Fatalf("first settlement = %+v, want initial green build at run 900", first)
	}
	if second.Generation == nil || *second.Generation <= *first.Generation ||
		second.SupersededSettlement != "true" || second.Failed.Count != 1 || strings.Join(second.Failed.Checks, ",") != "lint" ||
		second.Passed.Count != 1 || strings.Join(second.Passed.Checks, ",") != "build" {
		t.Fatalf("second settlement = %+v, want a newer red lint settlement with max run 900", second)
	}
}

func TestSummaryTickCarriesGenerationForInPlaceCheckRunUpdate(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "900", "https://example.test/900", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record successful build: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))

	if err := recordCheck(store, owner, repo, number, sha, "build", "900", "https://example.test/900", "completed", "failure", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record failed in-place build update: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("settlement count = %d, want 2", got)
	}

	type payload struct {
		CheckRuns            []CheckRunRef `json:"check_runs"`
		Generation           *uint64       `json:"generation"`
		SupersededSettlement string        `json:"superseded_settlement"`
		Failed               StatusGroup   `json:"failed"`
		Passed               StatusGroup   `json:"passed"`
	}
	var first, second payload
	items := pub.all()
	if err := json.Unmarshal([]byte(items[0].Payload), &first); err != nil {
		t.Fatalf("decode first settlement: %v", err)
	}
	if err := json.Unmarshal([]byte(items[1].Payload), &second); err != nil {
		t.Fatalf("decode second settlement: %v", err)
	}
	assertCheckRuns(t, first.CheckRuns, map[string]uint64{"build": 900})
	assertCheckRuns(t, second.CheckRuns, map[string]uint64{"build": 900})
	if first.Generation == nil || first.SupersededSettlement != "" ||
		first.Passed.Count != 1 || strings.Join(first.Passed.Checks, ",") != "build" || first.Failed.Count != 0 {
		t.Fatalf("first settlement = %+v, want green build at run 900", first)
	}
	if second.Generation == nil || *second.Generation <= *first.Generation ||
		second.SupersededSettlement != "true" ||
		second.Failed.Count != 1 || strings.Join(second.Failed.Checks, ",") != "build" || second.Passed.Count != 0 {
		t.Fatalf("second settlement = %+v, want a newer red build at the same attempt set", second)
	}
}
func TestSummaryTickKeepsLegacyFailureAfterFreshCheckArrives(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	legacy := []byte(`{"owner":"example-org","repo":"example-repo","number":"42","sha":"abcdef1234567890abcdef1234567890abcdef12",` +
		`"checks":{"build":{"status":"completed","conclusion":"failure"},"lint":{"status":"in_progress"}}}`)
	if _, err := store.kv.Create(Key(owner, repo, number, sha), legacy); err != nil {
		t.Fatalf("write deployed-listener state: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "lint", "901", "https://example.test/901", "completed", "success", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record fresh lint: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("settlement count = %d, want 1", got)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode settlement: %v", err)
	}
	if maxCheckRunID(summary.CheckRuns) != 901 || summary.Failed.Count != 1 ||
		strings.Join(summary.Failed.Checks, ",") != "build" {
		t.Fatalf("legacy settlement = %+v, want red build with fresh lint run 901", summary)
	}
}

func TestSummaryTickKeepsLatestCheckRunAcrossSuites(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordSuite(store, owner, repo, number, sha, "suite-a", "completed", "failure", "1", ""); err != nil {
		t.Fatalf("record failing suite: %v", err)
	}
	if err := recordSuite(store, owner, repo, number, sha, "suite-b", "completed", "success", "1", ""); err != nil {
		t.Fatalf("record passing suite: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "test", "801", "https://example.test/801", "completed", "failure", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record failing check: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "test", "802", "https://example.test/802", "completed", "success", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record passing check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	waitCacheSuites(t, store, owner, repo, number, sha, 2)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("settlement count = %d, want 1", got)
	}
	state := getState(t, store, owner, repo, number, sha)
	if len(state.Checks) != 1 || state.Checks["test"].CheckRunID != 802 || state.Checks["test"].Conclusion != "success" {
		t.Fatalf("checks = %+v, want the latest successful test run", state.Checks)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode settlement: %v", err)
	}
	assertCheckRuns(t, summary.CheckRuns, map[string]uint64{"test": 802})
	if summary.Failed.Count != 0 || len(summary.Failed.Checks) != 0 {
		t.Fatalf("failed checks = %+v, want none", summary.Failed)
	}
	if summary.Passed.Count != 1 || len(summary.Passed.Checks) != 1 || summary.Passed.Checks[0] != "test" {
		t.Fatalf("passed checks = %+v, want the latest test run", summary.Passed)
	}
	if len(summary.FailingChecks) != 0 {
		t.Fatalf("failing_checks = %+v, want none", summary.FailingChecks)
	}
}

func TestSummaryTickAcceptsNewerRunFromEarlierSuite(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordSuite(store, owner, repo, number, sha, "suite-a", "completed", "success", "1", ""); err != nil {
		t.Fatalf("record suite A: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "test", "100", "https://example.test/100", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record suite A test: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "lint", "101", "https://example.test/101", "completed", "success", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record suite A lint: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 2)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("suite A settlement count = %d, want 1", got)
	}
	var first Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &first); err != nil {
		t.Fatalf("decode suite A settlement: %v", err)
	}
	if maxCheckRunID(first.CheckRuns) != 101 || first.Failed.Count != 0 {
		t.Fatalf("suite A settlement = %+v, want green latest run 101", first)
	}

	if err := recordSuite(store, owner, repo, number, sha, "suite-b", "completed", "success", "1", ""); err != nil {
		t.Fatalf("record suite B: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "test", "200", "https://example.test/200", "completed", "success", "2026-09-07T03:02:00Z"); err != nil {
		t.Fatalf("record suite B test: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "lint", "201", "https://example.test/201", "completed", "success", "2026-09-07T03:03:00Z"); err != nil {
		t.Fatalf("record suite B lint: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("suite B settlement count = %d, want 2", got)
	}
	var second Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &second); err != nil {
		t.Fatalf("decode suite B settlement: %v", err)
	}
	if maxCheckRunID(second.CheckRuns) != 201 || second.Failed.Count != 0 {
		t.Fatalf("suite B settlement = %+v, want green latest run 201", second)
	}

	// This run belongs to the earlier suite A. Its newer GitHub check-run id
	// still replaces suite B's current test run because checks are keyed by name.
	if err := recordCheck(store, owner, repo, number, sha, "test", "300", "https://example.test/300", "queued", "", ""); err != nil {
		t.Fatalf("record suite A rerun: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("queued suite A rerun published %d settlements, want 2", got)
	}
	if err := recordCheck(store, owner, repo, number, sha, "test", "300", "https://example.test/300", "completed", "failure", "2026-09-07T03:04:00Z"); err != nil {
		t.Fatalf("complete suite A rerun: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 3 {
		t.Fatalf("suite A rerun settlement count = %d, want 3", got)
	}
	var third Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &third); err != nil {
		t.Fatalf("decode suite A rerun settlement: %v", err)
	}
	if maxCheckRunID(third.CheckRuns) != 300 || third.Failed.Count != 1 || strings.Join(third.Failed.Checks, ",") != "test" {
		t.Fatalf("suite A rerun settlement = %+v, want failed test at latest run 300", third)
	}
}

func TestSummaryTickIgnoresCancelledRunFromRetargetedSuite(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordSuite(store, owner, repo, number, sha, "suite-a", "completed", "success", "1", ""); err != nil {
		t.Fatalf("record first suite: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "test", "801", "https://example.test/801", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record first test: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "lint", "802", "https://example.test/802", "completed", "success", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record first lint: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 2)
	waitCacheSuites(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("initial settlement count = %d, want 1", got)
	}
	var initialSummary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &initialSummary); err != nil {
		t.Fatalf("decode initial settlement: %v", err)
	}
	if maxCheckRunID(initialSummary.CheckRuns) != 802 {
		t.Fatalf("initial latest_check_run_id = %d, want 802", maxCheckRunID(initialSummary.CheckRuns))
	}
	initial := getState(t, store, owner, repo, number, sha)
	if !initial.SettledEmitted || initial.EmittedCount != 1 {
		t.Fatalf("initial state = %+v, want one emitted settlement", initial)
	}

	if err := recordSuite(store, owner, repo, number, sha, "suite-b", "in_progress", "", "1", ""); err != nil {
		t.Fatalf("record retargeted suite: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "test", "901", "https://example.test/901", "in_progress", "", ""); err != nil {
		t.Fatalf("record retargeted test: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "lint", "902", "https://example.test/902", "in_progress", "", ""); err != nil {
		t.Fatalf("record retargeted lint: %v", err)
	}
	rearmed := getState(t, store, owner, repo, number, sha)
	if rearmed.Generation <= initial.Generation || rearmed.SettledEmitted || rearmed.EmittedCount != initial.EmittedCount {
		t.Fatalf("retargeted state = %+v, want a newer unsettled version without another emission", rearmed)
	}
	if len(rearmed.Checks) != 2 {
		t.Fatalf("retargeted checks = %+v, want latest runs only", rearmed.Checks)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("retargeted in-progress suite published %d settlements, want 1", got)
	}

	if err := recordCheck(store, owner, repo, number, sha, "test", "901", "https://example.test/901", "completed", "success", "2026-09-07T03:02:00Z"); err != nil {
		t.Fatalf("complete retargeted test: %v", err)
	}
	if err := recordCheck(store, owner, repo, number, sha, "lint", "902", "https://example.test/902", "completed", "success", "2026-09-07T03:03:00Z"); err != nil {
		t.Fatalf("complete retargeted lint: %v", err)
	}
	if err := recordSuite(store, owner, repo, number, sha, "suite-b", "completed", "success", "1", ""); err != nil {
		t.Fatalf("complete retargeted suite: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("re-settlement count = %d, want 2", got)
	}
	settled := getState(t, store, owner, repo, number, sha)
	if settled.Generation <= rearmed.Generation || !settled.SettledEmitted || settled.EmittedCount <= initial.EmittedCount {
		t.Fatalf("re-settled state = %+v, want a newer settled version with another emission", settled)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode re-settlement: %v", err)
	}
	if summary.SupersededSettlement != "true" || summary.Failed.Count != 0 || len(summary.FailingChecks) != 0 {
		t.Fatalf("re-settlement = %+v, want a green superseding settlement", summary)
	}
	if maxCheckRunID(summary.CheckRuns) != 902 || maxCheckRunID(summary.CheckRuns) <= maxCheckRunID(initialSummary.CheckRuns) {
		t.Fatalf("re-settlement latest_check_run_id = %d, want greater than %d", maxCheckRunID(summary.CheckRuns), maxCheckRunID(initialSummary.CheckRuns))
	}

	if err := recordCheck(store, owner, repo, number, sha, "test", "801", "https://example.test/801", "completed", "cancelled", ""); err != nil {
		t.Fatalf("record late cancelled test: %v", err)
	}
	afterLate := getState(t, store, owner, repo, number, sha)
	if afterLate.Generation != settled.Generation || !afterLate.SettledEmitted || afterLate.Checks["test"].Conclusion != "success" {
		t.Fatalf("late cancelled run changed settled state: %+v", afterLate)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("late cancelled run published %d settlements, want 2", got)
	}
}

func TestSummaryTickUsesLatestCheckRunIDAfterStateExpiration(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "901", "https://example.test/901", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record first observation: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("first settlement count = %d, want 1", got)
	}
	var first Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &first); err != nil {
		t.Fatalf("decode first settlement: %v", err)
	}
	if maxCheckRunID(first.CheckRuns) != 901 {
		t.Fatalf("first latest_check_run_id = %d, want 901", maxCheckRunID(first.CheckRuns))
	}

	key := Key(owner, repo, number, sha)
	if err := store.kv.Delete(key); err != nil {
		t.Fatalf("delete expired state: %v", err)
	}
	waitCacheStateMissing(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "902", "https://example.test/902", "completed", "success", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record post-expiration observation: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("post-expiration settlement count = %d, want 2", got)
	}
	var second Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &second); err != nil {
		t.Fatalf("decode post-expiration settlement: %v", err)
	}
	if maxCheckRunID(second.CheckRuns) <= maxCheckRunID(first.CheckRuns) {
		t.Fatalf("post-expiration latest_check_run_id = %d, want greater than %d", maxCheckRunID(second.CheckRuns), maxCheckRunID(first.CheckRuns))
	}
}

func TestSummaryTickSkipsNonHeadState(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		oldSHA = "oldabcdef1234"
		head   = "0123456789abcdef0123456789abcdef01234567"
	)

	if err := store.RecordHead(owner, repo, number, head, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, head)
	if err := recordCheck(store, owner, repo, number, oldSHA, "build", "804", "https://example-host/checks/804", "completed", "success", ""); err != nil {
		t.Fatalf("record old check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, oldSHA, 1)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("stale head published %d checks envelopes", pub.count())
	}
}

func TestSummaryTickConcurrentExactlyOnce(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	pub := &recPub{
		beforePublish: func() {
			entered <- struct{}{}
			<-release
		},
	}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "805", "https://example-host/checks/805", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		runSummaryTick(store, pub, time.Second, logging.New("test"))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first replica never reached publish")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		runSummaryTick(store, pub, time.Second, logging.New("test"))
	}()
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		close(release)
		<-firstDone
		t.Fatal("second replica reached publish instead of observing a durable claim")
	}
	close(release)
	<-firstDone

	if got := pub.count(); got != 1 {
		t.Fatalf("replica publishes = %d, want 1", got)
	}
}
func TestSummaryTickRefusesReclaimedClaimWhoseSnapshotWasReplaced(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	replicaA := openStore(t, conn)
	replicaB := openStore(t, conn)
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	if err := replicaA.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, replicaA, owner, repo, number, sha)
	waitHead(t, replicaB, owner, repo, number, sha)
	if err := recordCheck(replicaA, owner, repo, number, sha, "build", "900", "https://example.test/900", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record green build: %v", err)
	}
	waitCacheChecks(t, replicaA, owner, repo, number, sha, 1)
	waitCacheChecks(t, replicaB, owner, repo, number, sha, 1)

	key := Key(owner, repo, number, sha)
	aReady := make(chan struct{})
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	var releaseAOnce, releaseBOnce sync.Once
	t.Cleanup(func() {
		releaseAOnce.Do(func() { close(releaseA) })
		releaseBOnce.Do(func() { close(releaseB) })
	})
	originalKV := replicaA.kv
	var keyGets int
	replicaA.kv = &interleavingKV{
		KeyValue: originalKV,
		beforeGet: func(got string) {
			if got != key {
				return
			}
			keyGets++
			if keyGets == 2 {
				close(aReady)
				<-releaseA
			}
		},
	}
	t.Cleanup(func() { replicaA.kv = originalKV })

	bPublished := make(chan struct{})
	pub := &recPub{onPublish: func(contracts.Envelope) {
		select {
		case <-bPublished:
			return
		default:
			close(bPublished)
			<-releaseB
		}
	}}
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		runSummaryTick(replicaA, pub, 0, logging.New("replica-a"))
	}()
	select {
	case <-aReady:
	case <-time.After(5 * time.Second):
		t.Fatal("replica A never reached claim verification")
	}

	claimed := getState(t, replicaB, owner, repo, number, sha)
	if claimed.Claim == nil {
		t.Fatal("replica A never acquired its green settlement claim")
	}
	reclaimed, err := replicaB.ReclaimSettlement(key, claimed.Claim.Generation, time.Now().Add(time.Second).UnixMilli())
	if err != nil {
		t.Fatalf("reclaim replica A claim: %v", err)
	}
	if !reclaimed {
		t.Fatal("replica B did not reclaim replica A's aged claim")
	}
	if err := recordCheck(replicaB, owner, repo, number, sha, "build", "900", "https://example.test/900", "completed", "failure", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record red in-place build update: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		for _, state := range replicaB.List() {
			if Key(state.Owner, state.Repo, state.Number, state.SHA) == key && state.Checks["build"].Conclusion == "failure" {
				goto replicaBReady
			}
		}
		select {
		case <-deadline:
			t.Fatal("replica B cache never observed the red replacement")
		case <-time.After(15 * time.Millisecond):
		}
	}

replicaBReady:
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		runSummaryTick(replicaB, pub, 0, logging.New("replica-b"))
	}()
	select {
	case <-bPublished:
	case <-time.After(5 * time.Second):
		t.Fatal("replica B never published its red settlement")
	}
	if got := pub.count(); got != 1 {
		t.Fatalf("replica B publishes = %d, want one red settlement before replica A resumes", got)
	}
	var red Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &red); err != nil {
		t.Fatalf("decode red settlement: %v", err)
	}
	if red.Failed.Count != 1 || strings.Join(red.Failed.Checks, ",") != "build" {
		t.Fatalf("replica B settlement = %+v, want red build", red)
	}

	releaseAOnce.Do(func() { close(releaseA) })
	select {
	case <-aDone:
	case <-time.After(5 * time.Second):
		t.Fatal("replica A did not resume")
	}
	releaseBOnce.Do(func() { close(releaseB) })
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("replica B did not finish")
	}

	if got := pub.count(); got != 1 {
		t.Fatalf("replica publishes = %d, want exactly one red settlement", got)
	}
	var published Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &published); err != nil {
		t.Fatalf("decode settlement: %v", err)
	}
	if published.Snapshot == "" || published.Failed.Count != 1 {
		t.Fatalf("settlement = %+v, want the red snapshot with one failing check", published)
	}
	assertCheckRuns(t, published.CheckRuns, map[string]uint64{"build": 900})
}

func TestSummaryTickSkipsClaimRearmedBeforePublish(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "806", "https://example.test/806", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record initial check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	gen0 := getState(t, store, owner, repo, number, sha).Generation

	key := Key(owner, repo, number, sha)
	originalKV := store.kv
	claimUpdated := make(chan struct{})
	releaseClaim := make(chan struct{})
	var claimGate struct {
		sync.Mutex
		blocked bool
	}
	store.kv = &interleavingKV{
		KeyValue: originalKV,
		afterUpdate: func(updatedKey string, _ []byte, _ uint64) {
			claimGate.Lock()
			if updatedKey != key || claimGate.blocked {
				claimGate.Unlock()
				return
			}
			claimGate.blocked = true
			close(claimUpdated)
			claimGate.Unlock()
			<-releaseClaim
		},
	}

	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		runSummaryTick(store, pub, time.Second, logging.New("test"))
	}()
	select {
	case <-claimUpdated:
	case <-time.After(5 * time.Second):
		t.Fatal("summary tick never acquired its settlement claim")
	}
	if err := recordCheck(store, owner, repo, number, sha, "build", "807", "https://example.test/807", "completed", "failure", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record rearming observation: %v", err)
	}
	close(releaseClaim)
	select {
	case <-tickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("summary tick did not resume")
	}
	store.kv = originalKV

	if got := pub.count(); got != 0 {
		t.Fatalf("rearmed claim published %d stale envelopes, want 0", got)
	}
	rearmed := getState(t, store, owner, repo, number, sha)
	if rearmed.Generation <= gen0 || rearmed.SettledEmitted || rearmed.Claim == nil || rearmed.Claim.Generation >= rearmed.Generation {
		t.Fatalf("rearmed state = %+v, want a newer version with an obsolete claim retained", rearmed)
	}

	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("next tick published %d envelopes, want one fresh settlement", got)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode fresh settlement: %v", err)
	}
	if maxCheckRunID(summary.CheckRuns) != 807 {
		t.Fatalf("fresh settlement latest_check_run_id = %d, want 807", maxCheckRunID(summary.CheckRuns))
	}
}
func TestSummaryTickSkipsSettlementWhenDurableHeadMovesBeforePublish(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		oldSHA = "abcdef1234567890abcdef1234567890abcdef12"
		newSHA = "1234567890abcdef1234567890abcdef12345678"
	)
	if err := store.RecordHead(owner, repo, number, oldSHA, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record old head: %v", err)
	}
	waitHead(t, store, owner, repo, number, oldSHA)
	if err := recordCheck(store, owner, repo, number, oldSHA, "build", "901", "https://example.test/901", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record old-head check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, oldSHA, 1)
	setLastEventAt(t, store, owner, repo, number, oldSHA, 0)

	key := Key(owner, repo, number, oldSHA)
	originalKV := store.kv
	claimUpdated := make(chan struct{})
	releaseClaim := make(chan struct{})
	var claimGate struct {
		sync.Mutex
		blocked bool
	}
	store.kv = &interleavingKV{
		KeyValue: originalKV,
		afterUpdate: func(updatedKey string, _ []byte, _ uint64) {
			claimGate.Lock()
			if updatedKey != key || claimGate.blocked {
				claimGate.Unlock()
				return
			}
			claimGate.blocked = true
			close(claimUpdated)
			claimGate.Unlock()
			<-releaseClaim
		},
	}

	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		runSummaryTick(store, pub, time.Second, logging.New("test"))
	}()
	select {
	case <-claimUpdated:
	case <-time.After(5 * time.Second):
		t.Fatal("summary tick never acquired its settlement claim")
	}
	if err := store.RecordHead(owner, repo, number, newSHA, "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record new head: %v", err)
	}
	close(releaseClaim)
	select {
	case <-tickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("summary tick did not resume")
	}
	store.kv = originalKV

	if got := pub.count(); got != 0 {
		t.Fatalf("old head published %d settlements after the durable head moved", got)
	}
	if stale := getState(t, store, owner, repo, number, oldSHA); stale.Claim != nil {
		t.Fatalf("moved-head state retained claim: %+v", stale.Claim)
	}
	waitHead(t, store, owner, repo, number, newSHA)
	if err := recordCheck(store, owner, repo, number, newSHA, "build", "902", "https://example.test/902", "completed", "success", "2026-09-07T03:01:00Z"); err != nil {
		t.Fatalf("record new-head check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, newSHA, 1)
	setLastEventAt(t, store, owner, repo, number, newSHA, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("new head published %d settlements, want 1", got)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode new-head settlement: %v", err)
	}
	if summary.SHA != newSHA {
		t.Fatalf("settled SHA = %q, want %q", summary.SHA, newSHA)
	}
}

func TestSummaryTickPublishesObsoleteSettlementThenSupersedingLatestCheckRunID(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	rearmed := false
	pub := &recPub{onPublish: func(contracts.Envelope) {
		if rearmed {
			return
		}
		rearmed = true
		if err := recordCheck(store,
			owner, repo, number, sha,
			"late-check", "808", "https://example.test/808", "queued", "", "",
		); err != nil {
			t.Fatalf("record queued check during publication: %v", err)
		}
	}}
	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store,
		owner, repo, number, sha,
		"build", "807", "https://example.test/807", "completed", "success", "2026-09-07T03:00:00Z",
	); err != nil {
		t.Fatalf("record initial check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	gen0 := getState(t, store, owner, repo, number, sha).Generation

	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("published %d envelopes, want the obsolete first settlement", got)
	}
	var firstPayload Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &firstPayload); err != nil {
		t.Fatalf("decode first payload: %v", err)
	}
	if maxCheckRunID(firstPayload.CheckRuns) != 807 {
		t.Fatalf("first payload latest_check_run_id = %d, want 807", maxCheckRunID(firstPayload.CheckRuns))
	}
	rearmedState := getState(t, store, owner, repo, number, sha)
	if rearmedState.Generation <= gen0 || rearmedState.SettledEmitted {
		t.Fatalf("post-publication rearmed state = %+v, want a newer unsettled version", rearmedState)
	}

	waitCacheChecks(t, store, owner, repo, number, sha, 2)
	if err := recordCheck(store,
		owner, repo, number, sha,
		"late-check", "808", "https://example.test/808", "completed", "success", "2026-09-07T03:01:00Z",
	); err != nil {
		t.Fatalf("complete queued check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 2)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	runSummaryTick(store, pub, time.Second, logging.New("test"))

	if got := pub.count(); got != 2 {
		t.Fatalf("published %d envelopes, want one re-settlement", got)
	}
	var secondPayload Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &secondPayload); err != nil {
		t.Fatalf("decode re-settled payload: %v", err)
	}
	if maxCheckRunID(secondPayload.CheckRuns) <= maxCheckRunID(firstPayload.CheckRuns) {
		t.Fatalf("re-settled latest_check_run_id = %d, want greater than %d", maxCheckRunID(secondPayload.CheckRuns), maxCheckRunID(firstPayload.CheckRuns))
	}
	if secondPayload.SupersededSettlement != "true" {
		t.Fatalf("superseded settlement = %q, want true", secondPayload.SupersededSettlement)
	}
	if settled := getState(t, store, owner, repo, number, sha); settled.Generation <= gen0 || !settled.SettledEmitted || settled.EmittedCount < 2 {
		t.Fatalf("final state = %+v, want a newer settled version with both emissions recorded", settled)
	}
}
func TestSummaryTickWaitsForChecksThenPublishesOnce(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "810", "https://example.test/810", "in_progress", "", ""); err != nil {
		t.Fatalf("record in-progress check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("in-progress check published %d envelopes", pub.count())
	}
	if err := recordCheck(store, owner, repo, number, sha, "build", "810", "https://example.test/810", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record completed check: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, 0, logging.New("test"))
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("completed check published %d envelopes, want one", pub.count())
	}
}

func TestSummaryTickRetriesSettlementAfterPublishFailure(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "811", "https://example.test/811", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record completed check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	pub.setErr(errors.New("nats unavailable"))
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	failed := getState(t, store, owner, repo, number, sha)
	if failed.SettledEmitted {
		t.Fatal("failed publication marked the state settled")
	}
	if failed.Claim != nil {
		t.Fatalf("failed publication retained claim %+v", failed.Claim)
	}

	pub.setErr(nil)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("retry published %d envelopes, want one", pub.count())
	}
	if !getState(t, store, owner, repo, number, sha).SettledEmitted {
		t.Fatal("successful retry did not mark the state settled")
	}
}

func TestSummaryTickReclaimsStaleClaim(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	debounce := 20 * time.Millisecond
	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "812", "https://example.test/812", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record completed check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	state := getState(t, store, owner, repo, number, sha)
	setClaim(t, store, owner, repo, number, sha, &SettlementClaim{
		Hash:       state.Hash(),
		Generation: state.Generation,
		ClaimedAt:  time.Now().Add(-2*debounce - time.Millisecond).UnixMilli(),
	})

	runSummaryTick(store, pub, debounce, logging.New("test"))
	runSummaryTick(store, pub, debounce, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("stale claim published %d envelopes, want one", got)
	}
	if settled := getState(t, store, owner, repo, number, sha); !settled.SettledEmitted || settled.Claim != nil {
		t.Fatalf("stale claim final state = %+v, want settled with no claim", settled)
	}
}

func TestSummaryTickWaitsForQuietChangedTerminalObservation(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	debounce := time.Second
	if err := store.RecordHead(owner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, store, owner, repo, number, sha)
	if err := recordCheck(store, owner, repo, number, sha, "build", "813", "https://example.test/813", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record initial terminal check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, debounce, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("initial terminal check published %d envelopes, want 1", got)
	}
	first := pub.last()

	if err := recordCheck(store, owner, repo, number, sha, "build", "813", "https://example.test/813-rerendered", "completed", "success", "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record changed terminal check: %v", err)
	}
	runSummaryTick(store, pub, debounce, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("changed terminal check published %d envelopes before debounce, want 1", got)
	}

	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, debounce, logging.New("test"))
	if got := pub.count(); got != 2 {
		t.Fatalf("changed terminal check published %d envelopes after debounce, want 2", got)
	}
	if second := pub.last(); second.DedupeKey == first.DedupeKey {
		t.Fatalf("changed terminal check reused dedupe key %q", second.DedupeKey)
	}
}

func TestSummaryTickPublishesLegacyResettledRecordWithoutTimestamps(t *testing.T) {
	// A record written by a deployed listener carries no timestamps at all;
	// its attempt set is the settlement identity, so it settles like any other.
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	pub := &recPub{}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	legacy := []byte(`{
		"owner":"example-org",
		"repo":"example-repo",
		"number":"42",
		"sha":"abcdef1234567890abcdef1234567890abcdef12",
		"checks":{
			"build":{
				"check_run_id":"814",
				"url":"https://example.test/814",
				"status":"completed",
				"conclusion":"success"
			}
		},
		"resettled":true
	}`)
	if _, err := store.kv.Put(Key(owner, repo, number, sha), legacy); err != nil {
		t.Fatalf("store legacy state: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)

	runSummaryTick(store, pub, 0, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("legacy resettled record published %d envelopes, want 1", got)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode settlement: %v", err)
	}
	if len(summary.CheckRuns) != 1 || summary.CheckRuns[0] != (CheckRunRef{Name: "build", ID: 814}) {
		t.Fatalf("check_runs = %+v, want [{build 814}]", summary.CheckRuns)
	}
}
