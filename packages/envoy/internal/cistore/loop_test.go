package cistore

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

type recPub struct {
	mu    sync.Mutex
	items []contracts.Envelope
	err   error
}

func (p *recPub) Publish(e contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.items = append(p.items, e)
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

func TestSummaryTickEmitsOnceThenOnChange(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	pub := &recPub{}
	logger := logging.New("test")

	if err := s.Record("o", "r", "13", "sha13", "build", "completed", "success"); err != nil {
		t.Fatalf("record build: %v", err)
	}
	if err := s.Record("o", "r", "13", "sha13", "test", "in_progress", ""); err != nil {
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
	if !strings.Contains(env.PayloadSummary, "build") || !strings.Contains(env.PayloadSummary, "test") {
		t.Errorf("summary should mention both checks: %q", env.PayloadSummary)
	}

	// Unchanged set → no re-emit.
	runSummaryTick(s, pub, time.Millisecond, logger)
	if pub.count() != 1 {
		t.Fatalf("unchanged set re-emitted: got %d publishes", pub.count())
	}

	// Changed set → exactly one more emit.
	if err := s.Record("o", "r", "13", "sha13", "lint", "completed", "failure"); err != nil {
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

	if err := s.Record("o", "r", "1", "sha", "build", "in_progress", ""); err != nil {
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

	if err := s.Record("o", "r", "2", "sha", "build", "completed", "success"); err != nil {
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

	if err := s.Record("o", "r", "3", "sha", "build", "completed", "success"); err != nil {
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

	if err := s.Record("o", "r", "4", "sha", "build", "completed", "success"); err != nil {
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
	if pub.count() != 1 {
		t.Fatalf("background loop should emit exactly once for a stable set, got %d", pub.count())
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

	if err := s.Record("o", "r", "1", "shaX", "build", "completed", "success"); err != nil {
		t.Fatalf("record pr1: %v", err)
	}
	if err := s.Record("o", "r", "2", "shaX", "build", "completed", "success"); err != nil {
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
