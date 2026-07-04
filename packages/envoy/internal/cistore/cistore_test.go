package cistore

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// connectNATS spins up a throwaway JetStream-enabled NATS server via
// testcontainers, mirroring internal/store/kv_test.go.
func connectNATS(t *testing.T) (*natsgo.Conn, func()) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("failed to start NATS: %v", err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		ctr.Terminate(ctx)
		t.Fatalf("failed to get NATS URI: %v", err)
	}
	conn, err := natsgo.Connect(uri)
	if err != nil {
		ctr.Terminate(ctx)
		t.Fatalf("failed to connect: %v", err)
	}
	cleanup := func() {
		conn.Close()
		ctr.Terminate(ctx)
	}
	return conn, cleanup
}

func openStore(t *testing.T, conn *natsgo.Conn) *Store {
	t.Helper()
	st, err := Open(conn, WithReplicas(1), WithTTL(time.Hour))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("cache not ready: %v", err)
	}
	return st
}

// getState reads the durable state directly (bypassing the eventually-consistent
// cache) so assertions are deterministic right after a write.
func getState(t *testing.T, s *Store, owner, repo, number, sha string) State {
	t.Helper()
	entry, err := s.kv.Get(Key(owner, repo, number, sha))
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	var st State
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return st
}

func TestRecordAccumulatesChecks(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := s.Record("sjawhar", "legion", "42", "abc123", "build", "completed", "success"); err != nil {
		t.Fatalf("record build: %v", err)
	}
	before := getState(t, s, "sjawhar", "legion", "42", "abc123")
	if err := s.Record("sjawhar", "legion", "42", "abc123", "test", "in_progress", ""); err != nil {
		t.Fatalf("record test: %v", err)
	}
	st := getState(t, s, "sjawhar", "legion", "42", "abc123")

	if len(st.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d: %+v", len(st.Checks), st.Checks)
	}
	if st.Checks["build"].Conclusion != "success" {
		t.Errorf("build conclusion = %q, want success", st.Checks["build"].Conclusion)
	}
	if st.Checks["test"].Status != "in_progress" {
		t.Errorf("test status = %q, want in_progress", st.Checks["test"].Status)
	}
	if st.LastEventAt < before.LastEventAt {
		t.Errorf("LastEventAt regressed: %d < %d", st.LastEventAt, before.LastEventAt)
	}
	if st.Owner != "sjawhar" || st.Repo != "legion" || st.Number != "42" || st.SHA != "abc123" {
		t.Errorf("identity mismatch: %+v", st)
	}
}

func TestRecordConcurrentNoLostUpdate(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		name := "check-" + string(rune('a'+i))
		go func() {
			defer wg.Done()
			errs <- s.Record("o", "r", "1", "sha", name, "completed", "success")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent record failed: %v", err)
		}
	}

	st := getState(t, s, "o", "r", "1", "sha")
	if len(st.Checks) != n {
		t.Fatalf("expected %d checks after concurrent records (CAS retry), got %d", n, len(st.Checks))
	}
}

func TestHashStableAndOrderIndependent(t *testing.T) {
	a := State{Checks: map[string]Check{
		"x": {Status: "completed", Conclusion: "success"},
		"y": {Status: "in_progress"},
	}}
	b := State{Checks: map[string]Check{
		"y": {Status: "in_progress"},
		"x": {Status: "completed", Conclusion: "success"},
	}}
	if a.Hash() != b.Hash() {
		t.Fatalf("hash should be order-independent: %s != %s", a.Hash(), b.Hash())
	}
	c := State{Checks: map[string]Check{
		"x": {Status: "completed", Conclusion: "failure"},
		"y": {Status: "in_progress"},
	}}
	if a.Hash() == c.Hash() {
		t.Fatalf("hash should change when a conclusion changes")
	}
}

func TestListReflectsRecords(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := s.Record("o", "r", "7", "sha7", "build", "completed", "success"); err != nil {
		t.Fatalf("record: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		list := s.List()
		if len(list) == 1 && list[0].Number == "7" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("List did not reflect record in time: %+v", s.List())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestMarkEmittedCAS(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := s.Record("o", "r", "5", "sha5", "build", "completed", "success"); err != nil {
		t.Fatalf("record: %v", err)
	}
	key := Key("o", "r", "5", "sha5")
	h := getState(t, s, "o", "r", "5", "sha5").Hash()

	ok, err := s.MarkEmitted(key, h, 0)
	if err != nil {
		t.Fatalf("mark emitted: %v", err)
	}
	if !ok {
		t.Fatalf("first MarkEmitted should succeed")
	}
	// Re-marking the same hash is a no-op (already emitted) — must not report success.
	ok, err = s.MarkEmitted(key, h, 0)
	if err != nil {
		t.Fatalf("second mark emitted err: %v", err)
	}
	if ok {
		t.Fatalf("re-marking the same hash should return false (already emitted)")
	}
	// A new hash after a state change emits again.
	if err := s.Record("o", "r", "5", "sha5", "test", "completed", "failure"); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	h2 := getState(t, s, "o", "r", "5", "sha5").Hash()
	if h2 == h {
		t.Fatalf("hash should have changed after new check")
	}
	ok, err = s.MarkEmitted(key, h2, 0)
	if err != nil {
		t.Fatalf("mark emitted h2: %v", err)
	}
	if !ok {
		t.Fatalf("MarkEmitted for changed hash should succeed")
	}
}

// TestMarkEmittedRejectsStaleHash covers the SEV1 race: a Record lands after the
// summary loop rendered from a stale cache snapshot but before MarkEmitted. The
// CAS must refuse to stamp (and publish) the now-stale hash.
func TestMarkEmittedRejectsStaleHash(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := s.Record("o", "r", "8", "sha8", "build", "in_progress", ""); err != nil {
		t.Fatalf("record build: %v", err)
	}
	key := Key("o", "r", "8", "sha8")
	stale := getState(t, s, "o", "r", "8", "sha8").Hash()

	// A new check lands after `stale` was computed but before MarkEmitted.
	if err := s.Record("o", "r", "8", "sha8", "test", "in_progress", ""); err != nil {
		t.Fatalf("record test: %v", err)
	}
	ok, err := s.MarkEmitted(key, stale, 0)
	if err != nil {
		t.Fatalf("mark emitted: %v", err)
	}
	if ok {
		t.Fatalf("MarkEmitted must reject a hash that no longer matches fresh state")
	}
	if got := getState(t, s, "o", "r", "8", "sha8").LastEmitHash; got == stale {
		t.Fatalf("stale hash was wrongly persisted as LastEmitHash")
	}
}

// TestMarkEmittedRespectsDebounceReopen: a duplicate check re-observation leaves
// the hash unchanged but advances LastEventAt, reopening the quiet window.
// MarkEmitted must defer while still inside the debounce window.
func TestMarkEmittedRespectsDebounceReopen(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := s.Record("o", "r", "9", "sha9", "build", "completed", "success"); err != nil {
		t.Fatalf("record: %v", err)
	}
	key := Key("o", "r", "9", "sha9")
	h := getState(t, s, "o", "r", "9", "sha9").Hash()

	if err := s.Record("o", "r", "9", "sha9", "build", "completed", "success"); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	ok, err := s.MarkEmitted(key, h, time.Hour)
	if err != nil {
		t.Fatalf("mark emitted: %v", err)
	}
	if ok {
		t.Fatalf("MarkEmitted must defer while within the debounce window")
	}
}
