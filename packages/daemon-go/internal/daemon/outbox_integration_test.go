package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
)

func TestOutboxLeaseLetsOnlyOneRunnerExecuteDueRow(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}, entered: entered, release: release}
	first := &outbox{pool: pool, records: records, dispatch: client, now: func() time.Time { return now }}
	second := &outbox{pool: pool, records: records, dispatch: client, now: func() time.Time { return now }}

	done := make(chan error, 1)
	go func() { done <- first.RunOnce(context.Background()) }()
	<-entered
	if err := second.RunOnce(context.Background()); err != nil {
		t.Fatalf("second runner: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first runner: %v", err)
	}
	if got := client.count(); got != 1 {
		t.Fatalf("status writes = %d, want one leased execution", got)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows = %d, want finished row", remaining)
	}
}

func TestOutboxRetryBacksOffFailedEffect(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))
	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}, fail: errors.New("Dispatch down")}
	runner := &outbox{pool: pool, records: records, dispatch: client, now: func() time.Time { return now }}

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}
	var attempts int
	var nextAt time.Time
	var lastError string
	if err := pool.QueryRow(context.Background(), "select attempts, next_at, last_error from outbox").Scan(&attempts, &nextAt, &lastError); err != nil {
		t.Fatalf("read retried row: %v", err)
	}
	delay := nextAt.Sub(now)
	if attempts != 1 || delay < time.Second-time.Microsecond || delay > time.Second+time.Microsecond || lastError != "set Dispatch issue LEGION-208 to in_progress: Dispatch down" {
		t.Fatalf("retry = attempts=%d delay=%s error=%q", attempts, delay, lastError)
	}
}

func TestOutboxRestartDrainsDueThenExpiredLeaseRows(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	// Two issues: one issue's status rows run one at a time, so a second row of the same issue would
	// wait for the leased one by design.
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-209", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))

	var claimed record.OutboxRow
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		rows, err := records.ClaimDue(context.Background(), tx, now, 1, outboxLease)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return errors.New("expected one row to be leased before restart")
		}
		claimed = rows[0]
		return nil
	}); err != nil {
		t.Fatalf("lease row before restart: %v", err)
	}

	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}}
	restarted := &outbox{pool: pool, records: records, dispatch: client, now: func() time.Time { return now }}
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("drain due row after restart: %v", err)
	}
	if got := client.count(); got != 1 {
		t.Fatalf("writes before lease expiry = %d, want one due row", got)
	}

	restarted.now = func() time.Time { return claimed.LeaseUntil.Add(time.Nanosecond) }
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("drain expired lease after restart: %v", err)
	}
	if got := client.count(); got != 2 {
		t.Fatalf("writes after lease expiry = %d, want both rows", got)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows = %d, want both completed", remaining)
	}
}

func TestOutboxStaleLeaseFinishLeavesNewLeaseRowUntouched(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))

	var first, second record.OutboxRow
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		rows, err := records.ClaimDue(context.Background(), tx, now, 1, outboxLease)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return errors.New("expected first lease")
		}
		first = rows[0]
		return nil
	}); err != nil {
		t.Fatalf("claim first lease: %v", err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		rows, err := records.ClaimDue(context.Background(), tx, first.LeaseUntil.Add(time.Nanosecond), 1, outboxLease)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return errors.New("expected replacement lease")
		}
		second = rows[0]
		return nil
	}); err != nil {
		t.Fatalf("claim replacement lease: %v", err)
	}
	if first.LeaseToken == second.LeaseToken {
		t.Fatal("replacement lease reused the stale token")
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.FinishOutbox(context.Background(), tx, first.ID, first.LeaseToken)
	}); err != nil {
		t.Fatalf("finish stale lease: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 1 {
		t.Fatalf("outbox rows after stale finish = %d, want one row under replacement lease", remaining)
	}
}

func mustOutboxRow(t *testing.T, issue string, payload record.OutboxPayload, now time.Time) record.OutboxRow {
	t.Helper()
	row, err := record.NewOutboxRow(issue, payload, now)
	if err != nil {
		t.Fatalf("new outbox row: %v", err)
	}
	return row
}

func enqueueOutbox(t *testing.T, pool *pgxpool.Pool, records record.Store, row record.OutboxRow) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error { return records.Enqueue(context.Background(), tx, row) }); err != nil {
		t.Fatalf("enqueue outbox row: %v", err)
	}
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox").Scan(&rows); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return rows
}

func isolatedOutboxPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("connect test admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "legion_outbox_test_" + outboxSuffix(t)
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	databaseURL := *base
	databaseURL.Path = "/" + name
	st, err := legionstore.Open(context.Background(), databaseURL.String())
	if err != nil {
		t.Fatalf("open isolated store: %v", err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate isolated store: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(context.Background(), databaseURL.String())
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func outboxSuffix(t *testing.T) string {
	t.Helper()
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("random database suffix: %v", err)
	}
	return hex.EncodeToString(bytes[:])
}

type blockingDispatch struct {
	mu      sync.Mutex
	issue   dispatch.Issue
	entered chan struct{}
	release <-chan struct{}
	fail    error
	writes  int
}

func (d *blockingDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}
func (d *blockingDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return d.issue, nil
}
func (d *blockingDispatch) SetStatus(context.Context, string, string) error {
	d.mu.Lock()
	d.writes++
	entered, release, fail := d.entered, d.release, d.fail
	d.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if release != nil {
		<-release
	}
	return fail
}
func (d *blockingDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *blockingDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *blockingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *blockingDispatch) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writes
}

// One issue's status writes reach Dispatch in the order the workflow made them. Before, a newer row
// ran while an older one backed off, found the board at a status it did not expect, took that for a
// human's change and finished unwritten; the older row then wrote, leaving the board a phase behind.
func TestOutboxWritesOneIssuesStatusesInOrderWhenTheOlderFailsFirst(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "in_progress", Status: "testing"}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "testing", Status: "needs_review"}, now))
	board := &boardDispatch{status: "in_progress", failures: 1}
	runner := &outbox{pool: pool, records: records, dispatch: board, now: func() time.Time { return now }}

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	runner.now = func() time.Time { return now.Add(time.Minute) }
	for range 3 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if board.status != "needs_review" || outboxRows(t, pool) != 0 {
		t.Fatalf("board = %q with %d rows left, want needs_review and none", board.status, outboxRows(t, pool))
	}
}

// boardDispatch is one issue's Dispatch board: it fails the first failures writes, then applies.
type boardDispatch struct {
	status   string
	failures int
}

func (d *boardDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}
func (d *boardDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	return dispatch.Issue{Key: key, Status: d.status}, nil
}
func (d *boardDispatch) SetStatus(_ context.Context, _ string, status string) error {
	if d.failures > 0 {
		d.failures--
		return errors.New("Dispatch down")
	}
	d.status = status
	return nil
}
func (d *boardDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *boardDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *boardDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}

// A failed transaction is one tick's failure, never the runner's end: a Postgres restart drops the
// pool's connections, and the outbox has to keep draining once they are back. A claim that fails is
// tried again on the next tick; a row whose finish fails keeps its lease, and runs again once the
// lease expires.
func TestOutboxRunSurvivesAFailedClaimAndAFailedFinish(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := &flakyOutboxStore{Store: record.NewStore(), claimFailures: 1, finishFailures: 1}
	var mu sync.Mutex
	now := time.Now().UTC()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))
	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}}
	runner := &outbox{pool: pool, records: records, dispatch: client, now: clock, log: quietLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()

	eventually(t, "the row executed and its finish failed", func() bool { return client.count() == 1 && records.failedFinishes() == 1 })
	mu.Lock()
	now = now.Add(outboxLease + time.Second)
	mu.Unlock()
	eventually(t, "the row finished after its lease expired", func() bool { return outboxRows(t, pool) == 0 })
	cancel()
	<-done
	if got := client.count(); got != 2 {
		t.Fatalf("status writes = %d, want the first run and the one after the lease expired", got)
	}
}

// flakyOutboxStore fails the first claimFailures ClaimDue and finishFailures FinishOutbox calls the
// way a dropped connection does, and passes every later call to the real store.
type flakyOutboxStore struct {
	record.Store
	mu             sync.Mutex
	claimFailures  int
	finishFailures int
	finishesFailed int
}

func (s *flakyOutboxStore) ClaimDue(ctx context.Context, tx pgx.Tx, now time.Time, limit int, leaseFor time.Duration) ([]record.OutboxRow, error) {
	s.mu.Lock()
	fail := s.claimFailures > 0
	if fail {
		s.claimFailures--
	}
	s.mu.Unlock()
	if fail {
		return nil, errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")
	}
	return s.Store.ClaimDue(ctx, tx, now, limit, leaseFor)
}

func (s *flakyOutboxStore) FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	s.mu.Lock()
	fail := s.finishFailures > 0
	if fail {
		s.finishFailures--
		s.finishesFailed++
	}
	s.mu.Unlock()
	if fail {
		return errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")
	}
	return s.Store.FinishOutbox(ctx, tx, id, leaseToken)
}

func (s *flakyOutboxStore) failedFinishes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishesFailed
}
