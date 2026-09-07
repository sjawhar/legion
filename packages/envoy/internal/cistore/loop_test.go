package cistore

import (
	"context"
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

func TestSummaryTickEmitsOnceThenOnChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")

	if err := s.Record("o", "r", "13", "sha13", "build", "", "", "completed", "success"); err != nil {
		t.Fatalf("record build: %v", err)
	}
	if err := s.Record("o", "r", "13", "sha13", "test", "", "", "in_progress", ""); err != nil {
		t.Fatalf("record test: %v", err)
	}
	waitCacheChecks(t, s, "o", "r", "13", "sha13", 2)
	time.Sleep(10 * time.Millisecond) // exceed the 1ms debounce quiet window

	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", pub.count())
	}
	env := pub.last()
	if env.Topic != "notifications.github.o.r.pr.13.ci" {
		t.Errorf("topic = %q", env.Topic)
	}
	if !strings.Contains(env.Payload, "build") || !strings.Contains(env.Payload, "test") {
		t.Errorf("payload should mention both checks: %q", env.Payload)
	}

	// Unchanged set → no re-emit.
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 1 {
		t.Fatalf("unchanged set re-emitted: got %d publishes", pub.count())
	}

	// Changed set → exactly one more emit.
	if err := s.Record("o", "r", "13", "sha13", "lint", "", "", "completed", "failure"); err != nil {
		t.Fatalf("record lint: %v", err)
	}
	waitCacheChecks(t, s, "o", "r", "13", "sha13", 3)
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 2 {
		t.Fatalf("changed set should emit once more: got %d", pub.count())
	}
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 2 {
		t.Fatalf("second unchanged tick re-emitted: got %d", pub.count())
	}
}

func TestSummaryTickRespectsDebounce(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")

	if err := s.Record("o", "r", "1", "sha", "build", "", "", "in_progress", ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	waitCacheChecks(t, s, "o", "r", "1", "sha", 1)
	// Just recorded — a 10s debounce means it is still within the quiet window.
	runSummaryTick(s, pub, 10*time.Second, logger)
	if pub.count() != 0 {
		t.Fatalf("expected no emit within debounce window, got %d", pub.count())
	}
}

func TestSummaryTickPublishFailureDoesNotReemit(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")

	if err := s.Record("o", "r", "2", "sha", "build", "", "", "completed", "success"); err != nil {
		t.Fatalf("record: %v", err)
	}
	waitCacheChecks(t, s, "o", "r", "2", "sha", 1)
	time.Sleep(10 * time.Millisecond)

	// Publish fails, but MarkEmitted already advanced the hash (emit-once favored
	// over at-least-once). The dropped summary is NOT retried on the next tick.
	pub.setErr(errFake)
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 0 {
		t.Fatalf("failed publish should record nothing, got %d", pub.count())
	}
	pub.setErr(nil)
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 0 {
		t.Fatalf("dropped summary must not re-emit after MarkEmitted advanced, got %d", pub.count())
	}
}

func TestSummaryTickConcurrentExactlyOnce(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")

	if err := s.Record("o", "r", "3", "sha", "build", "", "", "in_progress", ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	waitCacheChecks(t, s, "o", "r", "3", "sha", 1)
	time.Sleep(10 * time.Millisecond)

	const loops = 6
	var wg sync.WaitGroup
	for i := 0; i < loops; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSummaryTick(s, pub, time.Millisecond, logger)
		}()
	}
	wg.Wait()
	if pub.count() != 1 {
		t.Fatalf("concurrent ticks must emit exactly once (CAS), got %d", pub.count())
	}
}

func TestStartSummaryLoopBackground(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")

	if err := s.Record("o", "r", "4", "sha", "build", "", "", "completed", "success"); err != nil {
		t.Fatalf("record: %v", err)
	}
	waitCacheChecks(t, s, "o", "r", "4", "sha", 1)

	loopCtx, loopCancel := context.WithCancel(context.Background())
	defer loopCancel()
	StartSummaryLoop(loopCtx, s, pub, 50*time.Millisecond, 10*time.Millisecond, logger)

	deadline := time.After(3 * time.Second)
	for pub.count() == 0 {
		select {
		case <-deadline:
			t.Fatalf("background loop never emitted")
		case <-time.After(20 * time.Millisecond):
		}
	}
	// Give it extra ticks to prove it does not re-emit an unchanged set.
	time.Sleep(200 * time.Millisecond)
	if pub.count() != 2 {
		t.Fatalf("background loop should emit ci and checks.settled exactly once, got %d", pub.count())
	}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake publish error" }

// TestSummaryDedupeKeyDistinctPerPR guards the SEV2 fix: a check_run can attach
// to multiple PRs, so two PRs sharing a head SHA + identical check set must emit
// summaries with DISTINCT DedupeKeys — otherwise a wildcard subscriber's
// (DedupeKey, SessionID) dedupe would suppress the second PR's summary.
func TestSummaryDedupeKeyDistinctPerPR(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")

	if err := s.Record("o", "r", "1", "shaX", "build", "", "", "in_progress", ""); err != nil {
		t.Fatalf("record pr1: %v", err)
	}
	if err := s.Record("o", "r", "2", "shaX", "build", "", "", "in_progress", ""); err != nil {
		t.Fatalf("record pr2: %v", err)
	}
	waitCacheChecks(t, s, "o", "r", "1", "shaX", 1)
	waitCacheChecks(t, s, "o", "r", "2", "shaX", 1)
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(s, pub, time.Millisecond, logger)

	env := pub.all()
	if len(env) != 2 {
		t.Fatalf("expected 2 summaries (one per PR), got %d", len(env))
	}
	if env[0].DedupeKey == env[1].DedupeKey {
		t.Fatalf("PRs sharing a SHA must not share a DedupeKey: %s", env[0].DedupeKey)
	}
	for _, e := range env {
		if !strings.Contains(e.DedupeKey, ".pr.1.") && !strings.Contains(e.DedupeKey, ".pr.2.") {
			t.Fatalf("DedupeKey missing PR identity: %s", e.DedupeKey)
		}
	}
}

func TestSummaryTickSkipsNonHeadState(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")

	if err := s.Record(
		"example-org", "example-repo", "42", "oldabcdef1234",
		"build", "300", "https://example-host/checks/300", "completed", "success",
	); err != nil {
		t.Fatalf("record old head: %v", err)
	}
	if err := s.RecordHead("example-org", "example-repo", "42", "newabcdef1234"); err != nil {
		t.Fatalf("record new head: %v", err)
	}
	waitCacheChecks(t, s, "example-org", "example-repo", "42", "oldabcdef1234", 1)
	waitHead(t, s, "example-org", "example-repo", "42", "newabcdef1234")
	time.Sleep(10 * time.Millisecond)

	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 0 {
		t.Fatalf("non-head state published %d envelopes", pub.count())
	}
	if got := getState(t, s, "example-org", "example-repo", "42", "oldabcdef1234").LastEmitHash; got != "" {
		t.Fatalf("non-head state was marked emitted: %q", got)
	}
}

func TestSummaryTickPublishesSettledOncePerHead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567"
	)

	if err := s.RecordHead(owner, repo, number, sha); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, s, owner, repo, number, sha)
	for _, check := range []struct {
		name, id, url, status, conclusion string
	}{
		{"build", "300", "https://example-host/checks/300", "completed", "success"},
		{"lint", "301", "https://example-host/checks/301", "completed", "failure"},
		{"cancelled-check", "302", "https://example-host/checks/302", "completed", "cancelled"},
	} {
		if err := s.Record(owner, repo, number, sha, check.name, check.id, check.url, check.status, check.conclusion); err != nil {
			t.Fatalf("record %s: %v", check.name, err)
		}
	}
	waitCacheChecks(t, s, owner, repo, number, sha, 3)
	time.Sleep(10 * time.Millisecond)

	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 2 {
		t.Fatalf("initial head state published %d envelopes, want ci and checks.settled", pub.count())
	}
	envelopes := pub.all()
	ci, settled := envelopes[0], envelopes[1]
	if ci.Topic != "notifications.github.example-org.example-repo.pr.42.ci" {
		t.Fatalf("ci topic = %q", ci.Topic)
	}
	if ci.PayloadSummary != "CI for example-org/example-repo#42 @ abcdef1: 1 passed, 1 failed, 0 running, 0 queued, 1 cancelled, 0 skipped" {
		t.Fatalf("ci payload_summary = %q", ci.PayloadSummary)
	}
	var ciSummary Summary
	if err := json.Unmarshal([]byte(ci.Payload), &ciSummary); err != nil {
		t.Fatalf("ci payload must be summary JSON: %v\n%s", err, ci.Payload)
	}
	if !ciSummary.IsHead || ciSummary.Cancelled.Count != 1 {
		t.Fatalf("unexpected ci summary: %+v", ciSummary)
	}
	if settled.Topic != "notifications.github.example-org.example-repo.pr.42.checks.settled" {
		t.Fatalf("settled topic = %q", settled.Topic)
	}
	if settled.PayloadSummary != "checks settled on example-org/example-repo#42 @ abcdef1: 1 passed, 1 failed, 1 cancelled, 0 skipped; failing: lint" {
		t.Fatalf("settled payload_summary = %q", settled.PayloadSummary)
	}
	var settledSummary Summary
	if err := json.Unmarshal([]byte(settled.Payload), &settledSummary); err != nil {
		t.Fatalf("settled payload must be summary JSON: %v\n%s", err, settled.Payload)
	}
	if settledSummary.Kind != "checks_settled" || len(settledSummary.FailingChecks) != 1 ||
		settledSummary.FailingChecks[0].Name != "lint" ||
		settledSummary.FailingChecks[0].URL != "https://example-host/checks/301" {
		t.Fatalf("unexpected settled payload: %+v", settledSummary)
	}
	t.Logf("pr.42.ci payload_summary: %s", ci.PayloadSummary)
	t.Logf("pr.42.ci payload: %s", ci.Payload)
	t.Logf("pr.42.checks.settled payload_summary: %s", settled.PayloadSummary)
	t.Logf("pr.42.checks.settled payload: %s", settled.Payload)

	if !getState(t, s, owner, repo, number, sha).SettledEmitted {
		t.Fatal("settled state was not marked emitted")
	}
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 2 {
		t.Fatalf("no-op tick re-emitted settled state: %d publishes", pub.count())
	}

	const newSHA = "0123456789abcdef"
	if err := s.RecordHead(owner, repo, number, newSHA); err != nil {
		t.Fatalf("record new head: %v", err)
	}
	waitHead(t, s, owner, repo, number, newSHA)
	if err := s.Record(owner, repo, number, newSHA, "build", "303", "https://example-host/checks/303", "completed", "success"); err != nil {
		t.Fatalf("record new head check: %v", err)
	}
	waitCacheChecks(t, s, owner, repo, number, newSHA, 1)
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 4 {
		t.Fatalf("new head did not publish ci and settled envelopes: %d publishes", pub.count())
	}
}

func TestSummaryTickSettlesPreviouslyEmittedTerminalState(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567"
	)

	if err := s.RecordHead(owner, repo, number, sha); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, s, owner, repo, number, sha)
	if err := s.Record(owner, repo, number, sha, "build", "400", "https://example-host/checks/400", "completed", "success"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	waitCacheChecks(t, s, owner, repo, number, sha, 1)
	key := Key(owner, repo, number, sha)
	entry, err := s.kv.Get(key)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	var state State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	state.LastEmitHash = state.Hash()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	if _, err := s.kv.Update(key, raw, entry.Revision()); err != nil {
		t.Fatalf("seed emitted state: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		for _, cached := range s.List() {
			if Key(cached.Owner, cached.Repo, cached.Number, cached.SHA) == key &&
				cached.LastEmitHash == state.LastEmitHash {
				goto cacheReady
			}
		}
		select {
		case <-deadline:
			t.Fatal("cache never reflected seeded emitted state")
		case <-time.After(15 * time.Millisecond):
		}
	}

cacheReady:
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 1 {
		t.Fatalf("previously emitted terminal state published %d envelopes, want settlement only", pub.count())
	}
	if env := pub.last(); env.Topic != "notifications.github.example-org.example-repo.pr.42.checks.settled" {
		t.Fatalf("topic = %q, want checks.settled", env.Topic)
	}
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 1 {
		t.Fatalf("settled state re-emitted: %d publishes", pub.count())
	}
}

func TestSummaryTickDoesNotSettleAfterQueuedWorkArrives(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	logger := logging.New("test")
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567"
	)

	if err := s.RecordHead(owner, repo, number, sha); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, s, owner, repo, number, sha)
	if err := s.Record(owner, repo, number, sha, "build", "500", "https://example-host/checks/500", "completed", "success"); err != nil {
		t.Fatalf("record initial check: %v", err)
	}
	waitCacheChecks(t, s, owner, repo, number, sha, 1)

	var once sync.Once
	pub := &recPub{
		onPublish: func(env contracts.Envelope) {
			if env.Topic != "notifications.github.example-org.example-repo.pr.42.ci" {
				return
			}
			once.Do(func() {
				if err := s.Record(owner, repo, number, sha, "late-check", "501", "https://example-host/checks/501", "queued", ""); err != nil {
					t.Errorf("record queued work: %v", err)
				}
			})
		},
	}
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 1 {
		t.Fatalf("queued work arriving after ci summary allowed %d publishes, want ci only", pub.count())
	}
	waitCacheChecks(t, s, owner, repo, number, sha, 2)

	if err := s.Record(owner, repo, number, sha, "late-check", "501", "https://example-host/checks/501", "completed", "success"); err != nil {
		t.Fatalf("complete late check: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 3 {
		t.Fatalf("terminal state after queued work published %d envelopes, want ci and settlement", pub.count())
	}
}
