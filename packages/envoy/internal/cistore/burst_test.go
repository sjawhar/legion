package cistore

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// writtenState is one state the store wrote to a record, at the revision the write created.
type writtenState struct {
	revision uint64
	state    State
}

// writeKV is the CI bucket seen through a store whose writes a test can slow, hold or fail, and
// whose every successful write it records. Reads and watches pass straight through.
type writeKV struct {
	natsgo.KeyValue
	delay time.Duration

	mu      sync.Mutex
	act     func(call int) error
	calls   int
	written map[string][]writtenState
}

func (k *writeKV) Create(key string, value []byte) (uint64, error) {
	return k.write(key, value, func() (uint64, error) { return k.KeyValue.Create(key, value) })
}

func (k *writeKV) Update(key string, value []byte, last uint64) (uint64, error) {
	return k.write(key, value, func() (uint64, error) { return k.KeyValue.Update(key, value, last) })
}

func (k *writeKV) write(key string, value []byte, do func() (uint64, error)) (uint64, error) {
	time.Sleep(k.delay)
	k.mu.Lock()
	act := k.act
	k.calls++
	call := k.calls
	k.mu.Unlock()
	if act != nil {
		if err := act(call); err != nil {
			return 0, err
		}
	}
	revision, err := do()
	if err == nil {
		var state State
		if json.Unmarshal(value, &state) == nil {
			k.mu.Lock()
			k.written[key] = append(k.written[key], writtenState{revision, state})
			k.mu.Unlock()
		}
	}
	return revision, err
}

// onWrite makes every write attempt from here on first call act with its number, counting from 1;
// an error act returns is returned instead of writing. A nil act clears it.
func (k *writeKV) onWrite(act func(call int) error) {
	k.mu.Lock()
	k.act = act
	k.calls = 0
	k.mu.Unlock()
}

// writeCalls is how many write attempts there have been since the last onWrite.
func (k *writeKV) writeCalls() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls
}

// holdNextWrite makes the next write report on entered and wait until release is called. Later
// writes pass.
func (k *writeKV) holdNextWrite() (entered <-chan struct{}, release func()) {
	reached := make(chan struct{})
	gate := make(chan struct{})
	k.onWrite(func(call int) error {
		if call == 1 {
			close(reached)
			<-gate
		}
		return nil
	})
	return reached, func() { close(gate) }
}

// recv waits up to 10 s for ch, failing the test when it never comes, so a regression in the
// hand-over fails the test instead of hanging it.
func recv(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// states returns every state written to key, in revision order.
func (k *writeKV) states(key string) []State {
	k.mu.Lock()
	written := append([]writtenState(nil), k.written[key]...)
	k.mu.Unlock()
	sort.Slice(written, func(i, j int) bool { return written[i].revision < written[j].revision })
	states := make([]State, len(written))
	for i, w := range written {
		states[i] = w.state
	}
	return states
}

// wrapStore opens a store on the shared NATS and routes its writes through a writeKV.
func wrapStore(t testing.TB, conn *natsgo.Conn, delay time.Duration) (*Store, *writeKV) {
	t.Helper()
	s := openStore(t, conn)
	kv := &writeKV{KeyValue: s.watcher.KV(), delay: delay, written: map[string][]writtenState{}}
	useKV(t, s, kv)
	return s, kv
}

// record records one observation of a head as the webhook handler does: a check when CheckName is
// set, a suite otherwise.
func record(s *Store, observation contracts.CIObservation) error {
	if observation.CheckName == "" {
		return s.RecordSuite(observation)
	}
	return s.Record(observation)
}

type head struct{ owner, repo, number, sha string }

func (h head) key() string { return Key(h.owner, h.repo, h.number, h.sha) }

func (h head) check(name string, runID uint64, status, conclusion, observedAt string) contracts.CIObservation {
	return contracts.CIObservation{
		Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha,
		CheckName: name, CheckRunID: runID, URL: fmt.Sprintf("https://example-host/runs/%d", runID),
		Status: status, Conclusion: conclusion, ObservedAt: observedAt,
	}
}

func (h head) suite(id, status, conclusion, observedAt string) contracts.CIObservation {
	return contracts.CIObservation{
		Owner: h.owner, Repo: h.repo, Number: h.number, SHA: h.sha,
		SuiteID: id, AppID: "15368", Status: status, Conclusion: conclusion, ObservedAt: observedAt,
	}
}

// queued is how many callers wait to be written to key.
func queued(s *Store, key string) int {
	s.combineMu.Lock()
	defer s.combineMu.Unlock()
	if c := s.combiners[key]; c != nil {
		return len(c.queue)
	}
	return 0
}

func waitQueued(t *testing.T, s *Store, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for queued(s, key) != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d callers queued for %s, want %d", queued(s, key), key, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitAll waits up to 10 s for every call started on wg, failing the test if one hangs.
func waitAll(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: a caller never returned", what)
	}
}

// recordAsOneBatch records observations[0] while its write is held, queues observations[1:] behind
// it in that order, and then lets the write through, so observations[1:] are written as one batch
// in arrival order. observations[0] must change the record, or there is no write to hold.
func recordAsOneBatch(t *testing.T, s *Store, kv *writeKV, observations []contracts.CIObservation) []error {
	t.Helper()
	key := Key(observations[0].Owner, observations[0].Repo, observations[0].Number, observations[0].SHA)
	entered, release := kv.holdNextWrite()
	errs := make([]error, len(observations))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[0] = record(s, observations[0])
	}()
	recv(t, entered, "the held write of the first observation")
	for i := 1; i < len(observations); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = record(s, observations[i])
		}()
		waitQueued(t, s, key, i)
	}
	release()
	waitAll(t, &wg, "recordAsOneBatch")
	kv.onWrite(nil)
	return errs
}

func requireNoErrors(t *testing.T, what string, errs []error) {
	t.Helper()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("%s: observation %d: %v", what, i, err)
		}
	}
}

// requireGenerationFencesSnapshots holds the record's identity rules over every state written to
// key: generation never decreases, it increases whenever the snapshot changes, and no generation
// is ever written with two different snapshots.
func requireGenerationFencesSnapshots(t *testing.T, kv *writeKV, key string) {
	t.Helper()
	states := kv.states(key)
	snapshots := map[uint64]string{}
	for i, state := range states {
		hash := state.Hash()
		if seen, ok := snapshots[state.Generation]; ok && seen != hash {
			t.Fatalf("%s: generation %d written with snapshots %s and %s", key, state.Generation, seen, hash)
		}
		snapshots[state.Generation] = hash
		if i == 0 {
			continue
		}
		previous := states[i-1]
		if state.Generation < previous.Generation {
			t.Fatalf("%s: generation fell from %d to %d at write %d", key, previous.Generation, state.Generation, i)
		}
		if hash != previous.Hash() && state.Generation == previous.Generation {
			t.Fatalf("%s: snapshot changed at write %d without a new generation (%d)", key, i, state.Generation)
		}
	}
}

// settle makes the head's record quiet and runs one summary tick, returning what it published.
func settle(t *testing.T, s *Store, h head, pub *recPub) []Summary {
	t.Helper()
	before := pub.count()
	setLastEventAt(t, s, h.owner, h.repo, h.number, h.sha, 0)
	runSummaryTick(s, pub, time.Second, logging.New("test"))
	var published []Summary
	for _, envelope := range pub.all()[before:] {
		var summary Summary
		if err := json.Unmarshal([]byte(envelope.Payload), &summary); err != nil {
			t.Fatalf("decode settlement: %v", err)
		}
		published = append(published, summary)
	}
	return published
}

// A burst folded into one write leaves the record, and the settlements it publishes, as the same
// observations written one at a time in arrival order do. The waves carry every kind of
// observation a burst mixes: a run superseded by a newer id, an older id arriving late, an
// in_progress arriving after its own completion, two timestamp-less observations of one run whose
// receipt order decides, two same-time completions of one run where the later one wins, exact
// duplicates, and suites. generation counts writes, not observations, so the batched record's
// numbers are lower; within each record it still never decreases, rises with every snapshot
// change, and never carries two snapshots.
func TestBatchedObservationsSettleAsSerialWritesInArrivalOrder(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s, kv := wrapStore(t, conn, 0)
	serial := head{"example-org", "example-repo", "41", "1111111111111111111111111111111111111111"}
	batched := head{"example-org", "example-repo", "42", "2222222222222222222222222222222222222222"}
	waves := func(h head) [][]contracts.CIObservation {
		return [][]contracts.CIObservation{
			{
				h.check("build", 100, "in_progress", "", "2026-09-24T01:00:00Z"),
				h.check("build", 100, "completed", "success", "2026-09-24T01:05:00Z"),
				h.check("lint", 101, "completed", "failure", "2026-09-24T01:04:00Z"),
				h.check("test", 102, "completed", "success", "2026-09-24T01:06:00Z"),
				h.check("test", 102, "in_progress", "", "2026-09-24T01:01:00Z"),
				h.check("flaky", 120, "completed", "failure", ""),
				h.check("flaky", 120, "completed", "success", ""),
				h.check("e2e", 130, "completed", "failure", "2026-09-24T01:07:00Z"),
				h.check("e2e", 130, "completed", "success", "2026-09-24T01:07:00Z"),
				h.suite("7001", "in_progress", "", "2026-09-24T01:00:00Z"),
				h.suite("7001", "completed", "failure", "2026-09-24T01:08:00Z"),
			},
			{
				h.check("lint", 110, "in_progress", "", "2026-09-24T02:00:00Z"),
				h.check("lint", 110, "completed", "success", "2026-09-24T02:03:00Z"),
				h.check("lint", 101, "completed", "failure", "2026-09-24T01:04:00Z"),
				h.check("build", 100, "completed", "success", "2026-09-24T01:05:00Z"),
				h.check("docs", 111, "completed", "skipped", "2026-09-24T02:01:00Z"),
				h.suite("7002", "completed", "success", "2026-09-24T02:04:00Z"),
			},
		}
	}
	serialWaves, batchedWaves := waves(serial), waves(batched)
	pub := &recPub{}
	var serialSettlements, batchedSettlements []Summary
	for wave := range serialWaves {
		for i, observation := range serialWaves[wave] {
			if err := record(s, observation); err != nil {
				t.Fatalf("wave %d: serial observation %d: %v", wave, i, err)
			}
		}
		writesBefore := len(kv.states(batched.key()))
		requireNoErrors(t, fmt.Sprintf("wave %d, batched", wave), recordAsOneBatch(t, s, kv, batchedWaves[wave]))
		if writes := len(kv.states(batched.key())) - writesBefore; writes != 2 {
			t.Fatalf("wave %d took %d writes, want 2: the held first observation, then the other %d as one batch", wave, writes, len(batchedWaves[wave])-1)
		}

		serialState := getState(t, s, serial.owner, serial.repo, serial.number, serial.sha)
		batchedState := getState(t, s, batched.owner, batched.repo, batched.number, batched.sha)
		if !reflect.DeepEqual(serialState.Checks, batchedState.Checks) || !reflect.DeepEqual(serialState.Suites, batchedState.Suites) ||
			serialState.Hash() != batchedState.Hash() || serialState.SettledEmitted != batchedState.SettledEmitted ||
			serialState.EmittedCount != batchedState.EmittedCount || (serialState.Claim == nil) != (batchedState.Claim == nil) {
			t.Fatalf("wave %d: batched record differs from serial writes in arrival order:\n serial:  %+v\n batched: %+v", wave, serialState, batchedState)
		}
		serialSettlements = append(serialSettlements, settle(t, s, serial, pub)...)
		batchedSettlements = append(batchedSettlements, settle(t, s, batched, pub)...)
	}

	// A wave of exact duplicates changes neither record, so it writes nothing and settles nothing.
	writesBefore := len(kv.states(serial.key())) + len(kv.states(batched.key()))
	duplicates := []contracts.CIObservation{
		serial.check("build", 100, "completed", "success", "2026-09-24T01:05:00Z"),
		serial.check("lint", 110, "completed", "success", "2026-09-24T02:03:00Z"),
		batched.check("build", 100, "completed", "success", "2026-09-24T01:05:00Z"),
		batched.check("lint", 110, "completed", "success", "2026-09-24T02:03:00Z"),
	}
	var wg sync.WaitGroup
	errs := make([]error, len(duplicates))
	for i, observation := range duplicates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = record(s, observation)
		}()
	}
	waitAll(t, &wg, "duplicate wave")
	requireNoErrors(t, "duplicate wave", errs)
	if writesAfter := len(kv.states(serial.key())) + len(kv.states(batched.key())); writesAfter != writesBefore {
		t.Fatalf("a wave of duplicates wrote %d times, want none", writesAfter-writesBefore)
	}
	runSummaryTick(s, pub, time.Second, logging.New("test"))
	if got := pub.count(); got != len(serialSettlements)+len(batchedSettlements) {
		t.Fatalf("the duplicate wave published %d more settlements, want none", got-len(serialSettlements)-len(batchedSettlements))
	}

	if len(serialSettlements) != 2 || len(batchedSettlements) != 2 {
		t.Fatalf("settlements: %d serial and %d batched, want one per wave for each", len(serialSettlements), len(batchedSettlements))
	}
	for i := range serialSettlements {
		want, got := serialSettlements[i], batchedSettlements[i]
		if !reflect.DeepEqual(want.CheckRuns, got.CheckRuns) || want.Snapshot != got.Snapshot ||
			want.SupersededSettlement != got.SupersededSettlement ||
			!reflect.DeepEqual(want.Failed, got.Failed) || !reflect.DeepEqual(want.Passed, got.Passed) ||
			!reflect.DeepEqual(want.Running, got.Running) || !reflect.DeepEqual(want.Queued, got.Queued) ||
			!reflect.DeepEqual(want.Cancelled, got.Cancelled) || !reflect.DeepEqual(want.Skipped, got.Skipped) {
			t.Fatalf("settlement %d differs:\n serial:  %+v\n batched: %+v", i, want, got)
		}
	}
	for _, sequence := range [][]Summary{serialSettlements, batchedSettlements} {
		if sequence[1].Generation <= sequence[0].Generation {
			t.Fatalf("settlement generations %d then %d, want strictly increasing", sequence[0].Generation, sequence[1].Generation)
		}
	}
	requireGenerationFencesSnapshots(t, kv, serial.key())
	requireGenerationFencesSnapshots(t, kv, batched.key())
	t.Logf("writes: serial %d, batched %d; settlement generations: serial %d,%d, batched %d,%d",
		len(kv.states(serial.key())), len(kv.states(batched.key())),
		serialSettlements[0].Generation, serialSettlements[1].Generation, batchedSettlements[0].Generation, batchedSettlements[1].Generation)
}

// recordBurst records n distinct completed checks of h concurrently, released together, as a
// check_run burst delivers them, and returns each call's error.
func recordBurst(s *Store, h head, n int) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = s.Record(h.check(fmt.Sprintf("unit-tests (shard %d, ubuntu-latest)", i), uint64(50_000_000_000+i), "completed", "success", "2026-09-24T01:29:00Z"))
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// A check_run burst on one head is many concurrent webhooks writing one record. Each write to it
// takes 25 ms here, so writes that each carry one observation succeed at most one per 25 ms and
// no more than 80 of them fit the record's 2 s budget: every observation past those would be
// refused, and GitHub does not redeliver it. Folding the burst into few writes records all 200.
func TestABurstOfObservationsOnOneHeadIsRecordedUnderSlowWrites(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s, kv := wrapStore(t, conn, 25*time.Millisecond)
	const n = 200
	h := head{"example-org", "example-repo", "42", "0123456789abcdef0123456789abcdef01234567"}

	errs := recordBurst(s, h, n)

	writes := len(kv.states(h.key()))
	failed := 0
	var first error
	for _, err := range errs {
		if err != nil {
			failed++
			if first == nil {
				first = err
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d observations refused (first: %v) after %d writes", failed, n, first, writes)
	}
	if checks := len(getState(t, s, h.owner, h.repo, h.number, h.sha).Checks); checks != n {
		t.Fatalf("record holds %d checks after %d writes, want all %d", checks, writes, n)
	}
	t.Logf("%d observations recorded in %d writes", n, writes)
}

// failureHead is the head the failure-path tests write.
var failureHead = head{"example-org", "example-repo", "42", "3333333333333333333333333333333333333333"}

// startCheck records check-i of failureHead in its own goroutine, storing its error in errs[i].
func startCheck(s *Store, errs []error, wg *sync.WaitGroup, i int) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[i] = s.Record(failureHead.check(fmt.Sprintf("check-%d", i), uint64(500+i), "completed", "success", "2026-09-24T01:00:00Z"))
	}()
}

// A batch's write that fails reaches every caller in the batch as that failure, the writer's role
// passes to the callers that queued meanwhile, and their write goes through: a failed write leaves
// nobody waiting and the record accepting writes. A write that panics fails its batch instead of
// stranding it.
func TestAFailedBatchWriteReachesEveryCallerAndTheNextBatchIsWritten(t *testing.T) {
	h := failureHead
	t.Run("a KV failure", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		s, kv := wrapStore(t, conn, 0)
		injected := errors.New("injected KV failure")
		enteredFirst, enteredSecond := make(chan struct{}), make(chan struct{})
		releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
		kv.onWrite(func(call int) error {
			switch call {
			case 1:
				close(enteredFirst)
				<-releaseFirst
			case 2:
				close(enteredSecond)
				<-releaseSecond
				return injected
			}
			return nil
		})
		errs := make([]error, 7)
		var wg sync.WaitGroup
		startCheck(s, errs, &wg, 0)
		recv(t, enteredFirst, "check-0's write")
		for i := 1; i <= 4; i++ {
			startCheck(s, errs, &wg, i)
			waitQueued(t, s, h.key(), i)
		}
		close(releaseFirst)
		recv(t, enteredSecond, "the write of check-1..4's batch")
		for i := 5; i <= 6; i++ {
			startCheck(s, errs, &wg, i)
			waitQueued(t, s, h.key(), i-4)
		}
		close(releaseSecond)
		waitAll(t, &wg, "a KV failure")

		for i, err := range errs {
			want := error(nil)
			if i >= 1 && i <= 4 {
				want = injected
			}
			if !errors.Is(err, want) {
				t.Fatalf("caller %d returned %v; want the injected failure for callers 1-4 and success for the rest", i, err)
			}
		}
		checks := getState(t, s, h.owner, h.repo, h.number, h.sha).Checks
		for _, name := range []string{"check-0", "check-5", "check-6"} {
			if _, ok := checks[name]; !ok {
				t.Fatalf("record lacks %s: %v", name, checks)
			}
		}
		if len(checks) != 3 {
			t.Fatalf("record holds %d checks, want only the 3 whose writes succeeded", len(checks))
		}
		if waiting := len(s.combiners); waiting != 0 {
			t.Fatalf("%d records still have a writer or a queue after every caller returned", waiting)
		}
	})

	t.Run("a write that panics", func(t *testing.T) {
		conn, cleanup := connectNATS(t)
		defer cleanup()
		s, kv := wrapStore(t, conn, 0)
		entered, release := make(chan struct{}), make(chan struct{})
		kv.onWrite(func(call int) error {
			if call == 1 {
				close(entered)
				<-release
				panic("injected panic")
			}
			return nil
		})
		errs := make([]error, 4)
		var recovered any
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { recovered = recover() }()
			errs[0] = s.Record(h.check("check-0", 500, "completed", "success", "2026-09-24T01:00:00Z"))
		}()
		recv(t, entered, "check-0's write")
		for i := 1; i <= 3; i++ {
			startCheck(s, errs, &wg, i)
			waitQueued(t, s, h.key(), i)
		}
		close(release)
		waitAll(t, &wg, "a write that panics")
		if recovered != "injected panic" {
			t.Fatalf("the writer's panic = %v, want it re-raised to the writer", recovered)
		}
		requireNoErrors(t, "the batch after a panicked write", errs[1:])
		if checks := getState(t, s, h.owner, h.repo, h.number, h.sha).Checks; len(checks) != 3 {
			t.Fatalf("record holds %d checks, want the 3 queued behind the panicked write", len(checks))
		}
	})
}

// A batch whose write loses its compare-and-swap to another listener task retries over that
// task's write with every observation in the batch, not only the first. Here check-0's write is
// held until check-1..3 queue behind it, and the other task writes the record between the read and
// the update of the three-observation batch's first attempt.
func TestALostCompareAndSwapRetriesTheWholeBatch(t *testing.T) {
	h := failureHead
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s, kv := wrapStore(t, conn, 0)
	// The other task writes the bucket directly, outside this store.
	other := kv.KeyValue
	if err := s.Record(h.check("seed", 400, "completed", "success", "2026-09-24T01:00:00Z")); err != nil {
		t.Fatalf("seed the record: %v", err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	kv.onWrite(func(call int) error {
		switch call {
		case 1:
			close(entered)
			<-release
		case 2:
			entry, err := other.Get(h.key())
			if err != nil {
				return err
			}
			var state State
			if err := json.Unmarshal(entry.Value(), &state); err != nil {
				return err
			}
			state.Checks["from-the-other-task"] = Check{Name: "from-the-other-task", CheckRunID: 450, Status: "completed", Conclusion: "success"}
			state.Generation++
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if _, err := other.Update(h.key(), raw, entry.Revision()); err != nil {
				return err
			}
		}
		return nil
	})
	errs := make([]error, 4)
	var wg sync.WaitGroup
	startCheck(s, errs, &wg, 0)
	recv(t, entered, "check-0's write")
	for i := 1; i <= 3; i++ {
		startCheck(s, errs, &wg, i)
		waitQueued(t, s, h.key(), i)
	}
	close(release)
	waitAll(t, &wg, "a lost compare-and-swap")
	requireNoErrors(t, "a lost compare-and-swap", errs)
	if attempts := kv.writeCalls(); attempts < 3 {
		t.Fatalf("%d write attempts, want the batch's lost one and its retry after check-0's", attempts)
	}
	checks := getState(t, s, h.owner, h.repo, h.number, h.sha).Checks
	for _, name := range []string{"seed", "from-the-other-task", "check-0", "check-1", "check-2", "check-3"} {
		if _, ok := checks[name]; !ok {
			t.Fatalf("record lacks %s after the batch's retried write: %v", name, checks)
		}
	}
}

// A listener shutting down closes its NATS connection with callers still queued behind a write.
// Each queued batch then fails at once on the closed connection, so every caller returns promptly
// with that error rather than waiting on a write that can no longer happen.
func TestQueuedCallersReturnPromptlyWhenTheConnectionCloses(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s, kv := wrapStore(t, conn, 0)
	h := head{"example-org", "example-repo", "42", "4444444444444444444444444444444444444444"}
	entered, release := kv.holdNextWrite()
	errs := make([]error, 10)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Record(h.check(fmt.Sprintf("check-%d", i), uint64(600+i), "completed", "success", "2026-09-24T01:00:00Z"))
		}()
		if i == 0 {
			recv(t, entered, "check-0's write")
		} else {
			waitQueued(t, s, h.key(), i)
		}
	}
	conn.Close()
	began := time.Now()
	release()
	waitAll(t, &wg, "closed connection")
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("callers took %s to return after the connection closed", elapsed)
	}
	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d returned success on a closed connection", i)
		}
	}
}

// BenchmarkRecordBurst records a burst of concurrent observations on one fresh head per
// iteration, against the shared real NATS, with each write taking the given time.
func BenchmarkRecordBurst(b *testing.B) {
	for _, bc := range []struct {
		n     int
		delay time.Duration
	}{{200, 0}, {400, 0}, {200, 2 * time.Millisecond}} {
		b.Run(fmt.Sprintf("n=%d,write=%s", bc.n, bc.delay), func(b *testing.B) {
			conn, cleanup := connectNATS(b)
			defer cleanup()
			s, kv := wrapStore(b, conn, bc.delay)
			writes := 0
			b.ResetTimer()
			for i := range b.N {
				h := head{"example-org", "example-repo", "42", fmt.Sprintf("%040x", i+1)}
				for _, err := range recordBurst(s, h, bc.n) {
					if err != nil {
						b.Fatalf("record: %v", err)
					}
				}
				writes += len(kv.states(h.key()))
			}
			b.ReportMetric(float64(writes)/float64(b.N), "writes/burst")
		})
	}
}
