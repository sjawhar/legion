package cistore

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

type recPub struct {
	mu        sync.Mutex
	items     []contracts.Envelope
	err       error
	onPublish func(contracts.Envelope)
}

func (p *recPub) Publish(e contracts.Envelope) error {
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return p.err
	}
	p.items = append(p.items, e)
	onPublish := p.onPublish
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
	time.Sleep(10 * time.Millisecond)

	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
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
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
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
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("in-progress suite allowed %d checks envelopes", pub.count())
	}

	if err := store.RecordSuite(owner, repo, number, sha, "900", "completed", "success", "77", ""); err != nil {
		t.Fatalf("complete suite: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("completed suite published %d envelopes, want checks", pub.count())
	}

	if err := store.RecordSuite(owner, repo, number, sha, "900", "in_progress", "", "77", ""); err != nil {
		t.Fatalf("reopen suite: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("reopened suite published %d envelopes", pub.count())
	}

	if err := store.RecordSuite(owner, repo, number, sha, "900", "completed", "success", "77", ""); err != nil {
		t.Fatalf("recomplete suite: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
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

func TestSummaryTickRearmsChecks(t *testing.T) {
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
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
	if pub.count() != 1 {
		t.Fatalf("initial checks publish = %d, want 1", pub.count())
	}

	if err := store.Record(owner, repo, number, sha, "build", "803", "https://example-host/checks/803", "completed", "success", ""); err != nil {
		t.Fatalf("record rerun check: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
	if pub.count() != 2 {
		t.Fatalf("rearmed checks publish = %d, want 2", pub.count())
	}
	var summary Summary
	if err := json.Unmarshal([]byte(pub.last().Payload), &summary); err != nil {
		t.Fatalf("decode re-settled payload: %v", err)
	}
	if summary.SupersededSettlement != "true" {
		t.Fatalf("superseded_settlement = %q, want true", summary.SupersededSettlement)
	}
	if !strings.HasSuffix(pub.last().PayloadSummary, " (re-settled)") {
		t.Fatalf("re-settled summary = %q", pub.last().PayloadSummary)
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
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
	if pub.count() != 0 {
		t.Fatalf("stale head published %d checks envelopes", pub.count())
	}
}

func TestSummaryTickConcurrentExactlyOnce(t *testing.T) {
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
	if err := store.Record(owner, repo, number, sha, "build", "805", "https://example-host/checks/805", "completed", "success", ""); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, store, owner, repo, number, sha, 1)
	time.Sleep(10 * time.Millisecond)

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSummaryTick(store, pub, time.Millisecond, logging.New("test"))
		}()
	}
	wg.Wait()
	if pub.count() != 1 {
		t.Fatalf("concurrent checks publishes = %d, want 1", pub.count())
	}
}
