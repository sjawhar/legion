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
	if err := store.Record(owner, repo, number, sha, "build", "800", "https://example-host/checks/800", "completed", "success", ""); err != nil {
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
	if err := store.Record(owner, repo, pr, shaA, "build", "800", "https://example.test/800", "completed", "success", ""); err != nil {
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

	if err := store.Record(owner, repo, number, sha, "build", "806", "https://example-host/checks/806", "completed", "success", ""); err != nil {
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
	if err := store.Record(owner, repo, number, sha, "build", "801", "https://example-host/checks/801", "completed", "success", ""); err != nil {
		t.Fatalf("record check: %v", err)
	}
	if err := store.RecordSuite(owner, repo, number, sha, "900", "in_progress", "", "77", ""); err != nil {
		t.Fatalf("record in-progress suite: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	waitCacheSuites(t, store, owner, repo, number, sha, 1)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("in-progress suite allowed %d checks envelopes", pub.count())
	}

	if err := store.RecordSuite(owner, repo, number, sha, "900", "completed", "success", "77", ""); err != nil {
		t.Fatalf("complete suite: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("completed suite published %d envelopes, want checks", pub.count())
	}

	if err := store.RecordSuite(owner, repo, number, sha, "900", "in_progress", "", "77", ""); err != nil {
		t.Fatalf("reopen suite: %v", err)
	}
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("reopened suite published %d envelopes", pub.count())
	}

	if err := store.RecordSuite(owner, repo, number, sha, "900", "completed", "success", "77", ""); err != nil {
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
	if err := store.Record(owner, repo, number, sha, "build", "802", "https://example-host/checks/802", "completed", "success", ""); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("initial checks publish = %d, want 1", got)
	}
	first := pub.last()
	if state := getState(t, store, owner, repo, number, sha); state.Generation != 0 || !state.SettledEmitted {
		t.Fatalf("initial settled state = %+v, want generation zero and settled", state)
	}

	if err := store.Record(owner, repo, number, sha, "build", "803", "https://example-host/checks/803", "completed", "success", ""); err != nil {
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
	if state := getState(t, store, owner, repo, number, sha); state.Generation != 1 || !state.SettledEmitted {
		t.Fatalf("check re-armed state = %+v, want generation one and settled", state)
	}

	if err := store.RecordSuite(owner, repo, number, sha, "900", "completed", "success", "77", ""); err != nil {
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
	if state := getState(t, store, owner, repo, number, sha); state.Generation != 2 || !state.SettledEmitted {
		t.Fatalf("new-suite re-armed state = %+v, want generation two and settled", state)
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
	if err := store.Record(owner, repo, number, oldSHA, "build", "804", "https://example-host/checks/804", "completed", "success", ""); err != nil {
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
	if err := store.Record(owner, repo, number, sha, "build", "805", "https://example-host/checks/805", "completed", "success", ""); err != nil {
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
	if err := store.Record(owner, repo, number, sha, "build", "810", "https://example.test/810", "in_progress", "", ""); err != nil {
		t.Fatalf("record in-progress check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	runSummaryTick(store, pub, 0, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("in-progress check published %d envelopes", pub.count())
	}
	if err := store.Record(owner, repo, number, sha, "build", "810", "https://example.test/810", "completed", "success", ""); err != nil {
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
	if err := store.Record(owner, repo, number, sha, "build", "811", "https://example.test/811", "completed", "success", ""); err != nil {
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
	if err := store.Record(owner, repo, number, sha, "build", "812", "https://example.test/812", "completed", "success", ""); err != nil {
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
	if err := store.Record(owner, repo, number, sha, "build", "813", "https://example.test/813", "completed", "success", ""); err != nil {
		t.Fatalf("record initial terminal check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	setLastEventAt(t, store, owner, repo, number, sha, 0)
	runSummaryTick(store, pub, debounce, logging.New("test"))
	if got := pub.count(); got != 1 {
		t.Fatalf("initial terminal check published %d envelopes, want 1", got)
	}
	first := pub.last()

	if err := store.Record(owner, repo, number, sha, "build", "813", "https://example.test/813-rerendered", "completed", "success", ""); err != nil {
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

func TestSummaryTickPublishesLegacyResettledAsGenerationOne(t *testing.T) {
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
		t.Fatalf("legacy resettled state published %d envelopes, want 1", got)
	}
	if got := pub.last().DedupeKey; !strings.HasSuffix(got, ".g1") {
		t.Fatalf("legacy resettled dedupe key = %q, want generation one", got)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode legacy resettled summary: %v", err)
	}
	if summary.SupersededSettlement != "true" {
		t.Fatalf("legacy resettled summary = %+v, want superseded settlement", summary)
	}
}
