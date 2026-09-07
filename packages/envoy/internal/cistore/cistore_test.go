package cistore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
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
	conn := testnats.Connect(t, uri)
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
func recordCheck(s *Store, owner, repo, number, sha, checkName, checkRunID, url, status, conclusion, observedAt string) error {
	id, err := strconv.ParseUint(checkRunID, 10, 64)
	if err != nil {
		return err
	}
	return s.Record(contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		CheckName:  checkName,
		CheckRunID: id,
		URL:        url,
		Status:     status,
		Conclusion: conclusion,
		ObservedAt: observedAt,
	})
}

func recordSuite(s *Store, owner, repo, number, sha, suiteID, status, conclusion, appID, observedAt string) error {
	return s.RecordSuite(contracts.CIObservation{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		SHA:        sha,
		SuiteID:    suiteID,
		AppID:      appID,
		Status:     status,
		Conclusion: conclusion,
		ObservedAt: observedAt,
	})
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

type interleavingKV struct {
	natsgo.KeyValue
	afterGet    func(string)
	afterUpdate func(string, []byte, uint64)
}

func (kv *interleavingKV) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	if err == nil && kv.afterGet != nil {
		kv.afterGet(key)
	}
	return entry, err
}

func (kv *interleavingKV) Update(key string, value []byte, revision uint64) (uint64, error) {
	updated, err := kv.KeyValue.Update(key, value, revision)
	if err == nil && kv.afterUpdate != nil {
		kv.afterUpdate(key, value, updated)
	}
	return updated, err
}
func TestStateUnmarshalJSONAcceptsLegacyAndNumericCheckRunIDs(t *testing.T) {
	for name, checkRunID := range map[string]string{
		"legacy string": `"987654321"`,
		"number":        "987654321",
	} {
		t.Run(name, func(t *testing.T) {
			var state State
			if err := json.Unmarshal([]byte(fmt.Sprintf(`{"checks":{"build":{"check_run_id":%s}}}`, checkRunID)), &state); err != nil {
				t.Fatalf("unmarshal state: %v", err)
			}
			if got := state.Checks["build"].CheckRunID; got != 987654321 {
				t.Fatalf("check_run_id = %d, want 987654321", got)
			}
		})
	}
}

func TestRecordAccumulatesChecks(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := recordCheck(s, "sjawhar", "legion", "42", "abc123", "build", "1", "", "completed", "success", ""); err != nil {
		t.Fatalf("record build: %v", err)
	}
	before := getState(t, s, "sjawhar", "legion", "42", "abc123")
	if err := recordCheck(s, "sjawhar", "legion", "42", "abc123", "test", "1", "", "in_progress", "", ""); err != nil {
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

func TestRecordStartsGenerationAtZero(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := recordCheck(s, "example-org", "example-repo", "42", "abcdef1234567", "build", "1", "https://example.test/1", "completed", "success", ""); err != nil {
		t.Fatalf("record check: %v", err)
	}
	if generation := getState(t, s, "example-org", "example-repo", "42", "abcdef1234567").Generation; generation != 0 {
		t.Fatalf("new state generation = %d, want 0", generation)
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
			errs <- recordCheck(s, "o", "r", "1", "sha", name, "1", "", "completed", "success", "")
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

	if err := recordCheck(s, "o", "r", "7", "sha7", "build", "1", "", "completed", "success", ""); err != nil {
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

func TestRecordIgnoresStaleCheckRunID(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := recordCheck(s,
		"example-org", "example-repo", "42", "abcdef1234567",
		"unit-tests", "200", "https://example-host/checks/200", "completed", "failure", "",
	); err != nil {
		t.Fatalf("record latest attempt: %v", err)
	}
	if err := recordCheck(s,
		"example-org", "example-repo", "42", "abcdef1234567",
		"unit-tests", "199", "https://example-host/checks/199", "in_progress", "", "",
	); err != nil {
		t.Fatalf("record stale attempt: %v", err)
	}

	check := getState(t, s, "example-org", "example-repo", "42", "abcdef1234567").Checks["unit-tests"]
	if check.CheckRunID != 200 || check.URL != "https://example-host/checks/200" ||
		check.Status != "completed" || check.Conclusion != "failure" {
		t.Fatalf("stale attempt overwrote latest check: %+v", check)
	}
}

func TestRecordIgnoresOlderObservationForSameRunAndSuite(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	const (
		owner      = "example-org"
		repo       = "example-repo"
		number     = "42"
		sha        = "abcdef1234567"
		completed  = "2026-09-07T03:00:00Z"
		inProgress = "2026-09-07T02:00:00Z"
	)
	if err := recordCheck(s,
		owner, repo, number, sha, "unit-tests", "200", "https://example-host/checks/200",
		"completed", "failure", completed,
	); err != nil {
		t.Fatalf("record completed check: %v", err)
	}
	if err := recordCheck(s,
		owner, repo, number, sha, "unit-tests", "200", "https://example-host/checks/200",
		"in_progress", "", inProgress,
	); err != nil {
		t.Fatalf("record delayed check: %v", err)
	}
	check := getState(t, s, owner, repo, number, sha).Checks["unit-tests"]
	if check.Status != "completed" || check.Conclusion != "failure" {
		t.Fatalf("delayed check re-armed state: %+v", check)
	}

	if err := recordSuite(s, owner, repo, number, sha, "900", "completed", "success", "77", completed); err != nil {
		t.Fatalf("record completed suite: %v", err)
	}
	if err := recordSuite(s, owner, repo, number, sha, "900", "in_progress", "", "77", inProgress); err != nil {
		t.Fatalf("record delayed suite: %v", err)
	}
	suite := getState(t, s, owner, repo, number, sha).Suites["900"]
	if suite.Status != "completed" || suite.Conclusion != "success" {
		t.Fatalf("delayed suite re-armed state: %+v", suite)
	}
	state := getState(t, s, owner, repo, number, sha)
	claimedState, claimed, err := s.ClaimSettlement(Key(owner, repo, number, sha), state.Hash(), state.Generation, time.Now().UnixMilli(), 0)
	if err != nil {
		t.Fatalf("claim settlement: %v", err)
	}
	if !claimed {
		t.Fatal("claim settlement did not claim completed state")
	}
	marked, err := s.MarkSettled(Key(owner, repo, number, sha), claimedState.Generation)
	if err != nil {
		t.Fatalf("mark settled: %v", err)
	}
	if !marked {
		t.Fatal("mark settled did not mark claimed state")
	}
	if err := recordCheck(s, owner, repo, number, sha, "unit-tests", "200", "https://example-host/checks/200", "in_progress", "", inProgress); err != nil {
		t.Fatalf("record delayed check after settlement: %v", err)
	}
	if err := recordSuite(s, owner, repo, number, sha, "900", "in_progress", "", "77", inProgress); err != nil {
		t.Fatalf("record delayed suite after settlement: %v", err)
	}
	if !getState(t, s, owner, repo, number, sha).SettledEmitted {
		t.Fatal("delayed observations re-armed settled state")
	}
}

func TestRecordHeadAndHead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const sha = "abcdef1234567890abcdef1234567890abcdef12"

	if err := s.RecordHead("example-org", "example-repo", "42", sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		head, ok := s.Head("example-org", "example-repo", "42")
		if ok {
			if head != sha {
				t.Fatalf("head = %q, want %q", head, sha)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("head never reached watch cache")
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func TestRecordHeadOrdersTimestampedUpdatesAndAcceptsMissingTimestamp(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		headC = "cccccccccccccccccccccccccccccccccccccccc"
		headD = "dddddddddddddddddddddddddddddddddddddddd"
	)
	readHead := func(t *testing.T) headRecord {
		t.Helper()
		entry, err := s.kv.Get(headKey(owner, repo, pr))
		if err != nil {
			t.Fatalf("get durable head: %v", err)
		}
		var head headRecord
		if err := json.Unmarshal(entry.Value(), &head); err != nil {
			t.Fatalf("decode durable head: %v", err)
		}
		return head
	}
	if err := s.RecordHead(owner, repo, pr, headB, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record current head: %v", err)
	}
	if err := s.RecordHead(owner, repo, pr, headA, "2026-09-07T02:00:00Z"); err != nil {
		t.Fatalf("record delayed head: %v", err)
	}
	if got := readHead(t).SHA; got != headB {
		t.Fatalf("older timestamp replaced head with %q, want %q", got, headB)
	}
	if err := s.RecordHead(owner, repo, pr, headD, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record equal-timestamp head: %v", err)
	}
	if got := readHead(t).SHA; got != headD {
		t.Fatalf("later equal-timestamp receipt did not replace head: got %q, want %q", got, headD)
	}
	if err := s.RecordHead(owner, repo, pr, headC, ""); err != nil {
		t.Fatalf("record head without timestamp: %v", err)
	}
	if got := readHead(t); got.SHA != headC || got.UpdatedAt != "" {
		t.Fatalf("untimestamped head = %+v, want SHA %q with no timestamp", got, headC)
	}
}

func TestWatchEvictsMalformedState(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567"
	)

	if err := recordCheck(s, owner, repo, number, sha, "build", "300", "https://example-host/checks/300", "completed", "success", ""); err != nil {
		t.Fatalf("record valid state: %v", err)
	}
	waitCacheChecks(t, s, owner, repo, number, sha, 1)
	if _, err := s.kv.Put(Key(owner, repo, number, sha), []byte("{")); err != nil {
		t.Fatalf("put malformed state: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if len(s.List()) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("malformed state remained in cache: %+v", s.List())
		case <-time.After(15 * time.Millisecond):
		}
	}
}

func TestWatchDistinguishesHeadRecordsFromStateKeys(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		stateOwner = "head-x"
		headOwner  = "x"
		repo       = "example-repo"
		number     = "42"
		sha        = "abcdef1234567890abcdef1234567890abcdef12"
	)

	if err := recordCheck(s, stateOwner, repo, number, sha, "build", "600", "https://example-host/checks/600", "completed", "success", ""); err != nil {
		t.Fatalf("record state: %v", err)
	}
	waitCacheChecks(t, s, stateOwner, repo, number, sha, 1)
	if err := s.RecordHead(headOwner, repo, number, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}

	waitHead(t, s, headOwner, repo, number, sha)
	states := s.List()
	if len(states) != 1 || states[0].Owner != stateOwner || states[0].SHA != sha {
		t.Fatalf("head record or state key misclassified in cache: %+v", states)
	}
}

func TestRecordHeadRejectsInvalidSHA(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := s.RecordHead("example-org", "example-repo", "42", "abcdef1234567", "2026-09-07T03:00:00Z"); err == nil {
		t.Fatal("RecordHead accepted an invalid SHA")
	}
}
func TestRecordSameIDDoesNotRegressCompletedAtEqualOrMissingTimestamps(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	const (
		owner     = "example-org"
		repo      = "example-repo"
		number    = "42"
		sha       = "abcdef1234567"
		timestamp = "2026-09-07T03:00:00Z"
	)
	if err := recordCheck(s, owner, repo, number, sha, "build", "800", "https://example.test/800", "completed", "success", timestamp); err != nil {
		t.Fatalf("record completed check: %v", err)
	}
	if err := recordCheck(s, owner, repo, number, sha, "build", "800", "https://example.test/800", "in_progress", "", timestamp); err != nil {
		t.Fatalf("record equal-timestamp in-progress check: %v", err)
	}
	if err := recordCheck(s, owner, repo, number, sha, "build", "800", "https://example.test/800", "in_progress", "", ""); err != nil {
		t.Fatalf("record timestamp-less in-progress check: %v", err)
	}
	if check := getState(t, s, owner, repo, number, sha).Checks["build"]; check.Status != "completed" || check.Conclusion != "success" {
		t.Fatalf("completed check regressed: %+v", check)
	}

	if err := recordSuite(s, owner, repo, number, sha, "900", "completed", "success", "77", timestamp); err != nil {
		t.Fatalf("record completed suite: %v", err)
	}
	if err := recordSuite(s, owner, repo, number, sha, "900", "in_progress", "", "77", timestamp); err != nil {
		t.Fatalf("record equal-timestamp in-progress suite: %v", err)
	}
	if err := recordSuite(s, owner, repo, number, sha, "900", "in_progress", "", "77", ""); err != nil {
		t.Fatalf("record timestamp-less in-progress suite: %v", err)
	}
	if suite := getState(t, s, owner, repo, number, sha).Suites["900"]; suite.Status != "completed" || suite.Conclusion != "success" {
		t.Fatalf("completed suite regressed: %+v", suite)
	}
}

func TestRecordTimestamplessObservationUpdatesTimestamplessState(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)

	if err := recordCheck(s, "example-org", "example-repo", "42", "abcdef1234567", "build", "800", "https://example.test/800", "queued", "", ""); err != nil {
		t.Fatalf("record queued check: %v", err)
	}
	if err := recordCheck(s, "example-org", "example-repo", "42", "abcdef1234567", "build", "800", "https://example.test/800", "in_progress", "", ""); err != nil {
		t.Fatalf("record in-progress check: %v", err)
	}
	if check := getState(t, s, "example-org", "example-repo", "42", "abcdef1234567").Checks["build"]; check.Status != "in_progress" {
		t.Fatalf("timestamp-less observation did not apply: %+v", check)
	}
}

func TestRecordHeadEqualTimestampUsesLatestObservation(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		when  = "2026-09-07T03:00:00Z"
	)
	if err := s.RecordHead(owner, repo, pr, headA, when); err != nil {
		t.Fatalf("record first head: %v", err)
	}
	if err := s.RecordHead(owner, repo, pr, headB, when); err != nil {
		t.Fatalf("record later same-time head: %v", err)
	}
	entry, err := s.kv.Get(headKey(owner, repo, pr))
	if err != nil {
		t.Fatalf("get durable head: %v", err)
	}
	var head headRecord
	if err := json.Unmarshal(entry.Value(), &head); err != nil {
		t.Fatalf("decode durable head: %v", err)
	}
	if head.SHA != headB {
		t.Fatalf("head = %+v, want later SHA %q", head, headB)
	}
}

func TestClaimSettlementRefusesWhenDurableHeadChanged(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		shaA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	if err := recordCheck(s, owner, repo, pr, shaA, "build", "801", "https://example.test/801", "completed", "success", ""); err != nil {
		t.Fatalf("record check: %v", err)
	}
	state := getState(t, s, owner, repo, pr, shaA)
	if err := s.RecordHead(owner, repo, pr, shaB, "2026-09-07T03:00:01Z"); err != nil {
		t.Fatalf("record replacement head: %v", err)
	}
	_, claimed, err := s.ClaimSettlement(Key(owner, repo, pr, shaA), state.Hash(), state.Generation, time.Now().UnixMilli(), 0)
	if err != nil {
		t.Fatalf("claim settlement: %v", err)
	}
	if claimed {
		t.Fatal("ClaimSettlement claimed a state whose durable head changed")
	}
	if getState(t, s, owner, repo, pr, shaA).Claim != nil {
		t.Fatal("stale state gained a settlement claim")
	}
}

func TestClaimSettlementRefusesWhenHashMoved(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		sha   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	if err := recordCheck(s, owner, repo, pr, sha, "build", "801", "https://example.test/801", "completed", "success", ""); err != nil {
		t.Fatalf("record initial check: %v", err)
	}
	initial := getState(t, s, owner, repo, pr, sha)
	if err := recordCheck(s, owner, repo, pr, sha, "build", "802", "https://example.test/802", "completed", "failure", ""); err != nil {
		t.Fatalf("record rerun: %v", err)
	}

	_, claimed, err := s.ClaimSettlement(Key(owner, repo, pr, sha), initial.Hash(), initial.Generation, time.Now().UnixMilli(), 0)
	if err != nil {
		t.Fatalf("claim settlement: %v", err)
	}
	if claimed {
		t.Fatal("ClaimSettlement claimed a state whose hash changed")
	}
	if getState(t, s, owner, repo, pr, sha).Claim != nil {
		t.Fatal("hash-moved state gained a settlement claim")
	}
}

func TestClaimSettlementRechecksDurableDebounce(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		sha   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	debounce := time.Second
	if err := recordCheck(s, owner, repo, pr, sha, "build", "801", "https://example.test/801", "completed", "success", ""); err != nil {
		t.Fatalf("record initial terminal check: %v", err)
	}
	stale := getState(t, s, owner, repo, pr, sha)
	if err := recordCheck(s, owner, repo, pr, sha, "build", "801", "https://example.test/801-rerendered", "completed", "success", ""); err != nil {
		t.Fatalf("record changed terminal check: %v", err)
	}
	durable := getState(t, s, owner, repo, pr, sha)
	if stale.Hash() != durable.Hash() || stale.Generation != durable.Generation {
		t.Fatalf("changed terminal metadata altered settlement identity: stale=%+v durable=%+v", stale, durable)
	}

	key := Key(owner, repo, pr, sha)
	_, claimed, err := s.ClaimSettlement(key, stale.Hash(), stale.Generation, durable.LastEventAt+debounce.Milliseconds()-1, debounce)
	if err != nil {
		t.Fatalf("claim inside durable debounce: %v", err)
	}
	if claimed {
		t.Fatal("ClaimSettlement claimed inside the durable debounce window")
	}
	if getState(t, s, owner, repo, pr, sha).Claim != nil {
		t.Fatal("durable-debounce rejection created a claim")
	}

	_, claimed, err = s.ClaimSettlement(key, stale.Hash(), stale.Generation, durable.LastEventAt+debounce.Milliseconds(), debounce)
	if err != nil {
		t.Fatalf("claim after durable debounce: %v", err)
	}
	if !claimed {
		t.Fatal("ClaimSettlement did not claim after the durable debounce window")
	}
}

func TestClaimSettlementStampsClaimAtCASTime(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		sha   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	debounce := time.Second
	if err := recordCheck(s, owner, repo, pr, sha, "build", "801", "https://example.test/801", "completed", "success", ""); err != nil {
		t.Fatalf("record terminal check: %v", err)
	}
	key := Key(owner, repo, pr, sha)
	entry, err := s.kv.Get(key)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	var state State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	state.LastEventAt = 0
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode old state: %v", err)
	}
	if _, err := s.kv.Update(key, raw, entry.Revision()); err != nil {
		t.Fatalf("write old state: %v", err)
	}

	tickNow := time.Now().Add(-3 * debounce).UnixMilli()
	claimedState, claimed, err := s.ClaimSettlement(key, state.Hash(), state.Generation, tickNow, debounce)
	if err != nil {
		t.Fatalf("claim settlement: %v", err)
	}
	if !claimed {
		t.Fatal("ClaimSettlement did not claim an already quiet state")
	}
	staleBefore := time.Now().Add(-2 * debounce).UnixMilli()
	if claimedState.Claim == nil || claimedState.Claim.ClaimedAt < staleBefore {
		t.Fatalf("claim timestamp = %+v, want a fresh non-reclaimable claim", claimedState.Claim)
	}
	reclaimed, err := s.ReclaimSettlement(key, claimedState.Generation, staleBefore)
	if err != nil {
		t.Fatalf("reclaim fresh claim: %v", err)
	}
	if reclaimed {
		t.Fatal("ReclaimSettlement reclaimed a claim stamped at CAS time")
	}
}

func TestMarkSettledCacheWriteDoesNotOverwriteNewerWatcherRevision(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		sha   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	if err := recordCheck(s, owner, repo, pr, sha, "build", "801", "https://example.test/801", "completed", "success", ""); err != nil {
		t.Fatalf("record check: %v", err)
	}
	state := getState(t, s, owner, repo, pr, sha)
	claimedState, claimed, err := s.ClaimSettlement(Key(owner, repo, pr, sha), state.Hash(), state.Generation, time.Now().UnixMilli(), 0)
	if err != nil || !claimed {
		t.Fatalf("claim settlement = (%+v, %t, %v), want claimed state", claimedState, claimed, err)
	}

	originalKV := s.kv
	var watcherRevision uint64
	s.kv = &interleavingKV{
		KeyValue: originalKV,
		afterUpdate: func(key string, _ []byte, _ uint64) {
			entry, err := originalKV.Get(key)
			if err != nil {
				t.Fatalf("get marked state: %v", err)
			}
			var newer State
			if err := json.Unmarshal(entry.Value(), &newer); err != nil {
				t.Fatalf("decode marked state: %v", err)
			}
			newer.Generation++
			newer.SettledEmitted = false
			raw, err := json.Marshal(newer)
			if err != nil {
				t.Fatalf("encode newer watcher state: %v", err)
			}
			watcherRevision, err = originalKV.Update(key, raw, entry.Revision())
			if err != nil {
				t.Fatalf("write newer watcher state: %v", err)
			}
			s.mu.Lock()
			s.cacheStateLocked(key, newer, watcherRevision)
			s.mu.Unlock()
		},
	}
	t.Cleanup(func() { s.kv = originalKV })

	marked, err := s.MarkSettled(Key(owner, repo, pr, sha), claimedState.Generation)
	if err != nil {
		t.Fatalf("mark settled: %v", err)
	}
	if !marked {
		t.Fatal("MarkSettled did not mark the claimed generation")
	}
	key := Key(owner, repo, pr, sha)
	s.mu.RLock()
	cached := s.cache[key]
	cachedRevision := s.cacheRevisions[key]
	s.mu.RUnlock()
	if cached.Generation != claimedState.Generation+1 || cached.SettledEmitted {
		t.Fatalf("cached state = %+v, want the newer watcher state", cached)
	}
	if cachedRevision != watcherRevision {
		t.Fatalf("cached revision = %d, want watcher revision %d", cachedRevision, watcherRevision)
	}
}

func TestWatchEvictsMalformedHead(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	s := openStore(t, conn)
	const (
		owner = "example-org"
		repo  = "example-repo"
		pr    = "42"
		sha   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	if err := s.RecordHead(owner, repo, pr, sha, "2026-09-07T03:00:00Z"); err != nil {
		t.Fatalf("record head: %v", err)
	}
	waitHead(t, s, owner, repo, pr, sha)
	if _, err := s.kv.Put(headKey(owner, repo, pr), []byte("{")); err != nil {
		t.Fatalf("put malformed head: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		if _, ok := s.Head(owner, repo, pr); !ok {
			return
		}
		select {
		case <-deadline:
			t.Fatal("malformed head remained in cache")
		case <-time.After(15 * time.Millisecond):
		}
	}
}
