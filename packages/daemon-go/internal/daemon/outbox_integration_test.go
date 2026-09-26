package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

func TestOutboxLeaseLetsOnlyOneRunnerExecuteDueRow(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}, entered: entered, release: release}
	first := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: func() time.Time { return now }}
	second := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: func() time.Time { return now }}

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
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: func() time.Time { return now }}

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
		rows, err := records.ClaimDue(context.Background(), tx, "LEGION", now, 1, outboxLease)
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
	restarted := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: func() time.Time { return now }}
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
		rows, err := records.ClaimDue(context.Background(), tx, "LEGION", now, 1, outboxLease)
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
		rows, err := records.ClaimDue(context.Background(), tx, "LEGION", first.LeaseUntil.Add(time.Nanosecond), 1, outboxLease)
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
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: board, now: func() time.Time { return now }}

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

// A status Dispatch refuses outright — the issue is gone, or the status is one it will not take —
// can never be written, and an issue's status writes run one at a time: retried forever, it held
// every later status of that issue behind it, so nothing the workflow decided afterwards ever
// reached the board. The refused row is finished and said so, and the issue's later writes run.
// What each of them then does is the ordinary rule: this one finds the board at a status neither
// its own nor the one it was made from, takes that for someone else's move, and leaves it.
func TestAPermanentlyRefusedStatusWriteDoesNotHoldBackTheIssuesLaterWrites(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "in_progress", Status: "testing"}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "testing", Status: "needs_review"}, now))
	board := &refusingDispatch{status: "in_progress", refuse: map[string]*dispatch.Error{
		"testing": {Status: 422, Code: "INVALID_STATUS", Message: "testing is not a status of this issue"},
	}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: board, now: func() time.Time { return now }, log: quietLogger()}

	for range 3 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run: %v", err)
		}
		now = now.Add(time.Minute)
		runner.now = func() time.Time { return now }
	}

	var rows int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where issue = 'LEGION-208'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("outbox holds %d rows for LEGION-208, want none: the refused row is finished and the one behind it runs", rows)
	}
	if board.reads < 2 {
		t.Fatalf("Dispatch was read %d times, want the later write to have run too", board.reads)
	}
}

// Only Dispatch's own refusal of the write is permanent. A revoked or expired token is answered
// 401 or 403 and says nothing about the write — an operator restores it and the same body is
// taken — and a 4xx carrying no Dispatch error code came from whatever sits in front of Dispatch,
// not from Dispatch judging the write. Reading either as permanent dropped every later status of
// that issue for good, which is the one outcome this outbox exists to prevent. Each case names
// the arm that saves it, so deleting one fails here.
func TestACredentialOrUncodedRefusalIsRiddenOutRatherThanDroppingTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal *dispatch.Error
	}{
		{"a revoked token", &dispatch.Error{Status: 401, Code: "UNAUTHORIZED", Message: "token revoked"}},
		{"a token without the scope", &dispatch.Error{Status: 403, Code: "FORBIDDEN", Message: "forbidden"}},
		{"a refusal Dispatch did not make", &dispatch.Error{Status: 422, Message: "<html>gateway</html>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			now := time.Now().UTC()
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "in_progress", Status: "testing"}, now))
			board := &refusingDispatch{status: "in_progress", refuse: map[string]*dispatch.Error{"testing": tc.refusal}}
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: board, now: func() time.Time { return now }, log: quietLogger()}

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run: %v", err)
			}

			if rows := outboxRows(t, pool); rows != 1 {
				t.Fatalf("outbox holds %d rows, want the write still pending", rows)
			}

			// The operator restores the token, or whatever answered instead of Dispatch is gone,
			// and the write is taken.
			delete(board.refuse, "testing")
			now = now.Add(time.Minute)
			runner.now = func() time.Time { return now }
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run after the refusal stops: %v", err)
			}
			if board.status != "testing" || outboxRows(t, pool) != 0 {
				t.Fatalf("board = %q with %d rows left, want testing and none", board.status, outboxRows(t, pool))
			}
		})
	}
}

// refusingDispatch answers SetStatus for a named status with a Dispatch refusal that will never
// change, and takes every other status.
type refusingDispatch struct {
	status string
	reads  int
	refuse map[string]*dispatch.Error
}

func (d *refusingDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}
func (d *refusingDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	d.reads++
	return dispatch.Issue{Key: key, Status: d.status}, nil
}
func (d *refusingDispatch) SetStatus(_ context.Context, _ string, status string) error {
	if refusal, refused := d.refuse[status]; refused {
		return fmt.Errorf("set Dispatch status %s: %w", status, refusal)
	}
	d.status = status
	return nil
}
func (d *refusingDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *refusingDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *refusingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
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
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, now: clock, log: quietLogger()}
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

func (s *flakyOutboxStore) ClaimDue(ctx context.Context, tx pgx.Tx, project string, now time.Time, limit int, leaseFor time.Duration) ([]record.OutboxRow, error) {
	s.mu.Lock()
	fail := s.claimFailures > 0
	if fail {
		s.claimFailures--
	}
	s.mu.Unlock()
	if fail {
		return nil, errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")
	}
	return s.Store.ClaimDue(ctx, tx, project, now, limit, leaseFor)
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

// An outbox tree_close row (lingerExpired enqueues one for every claim of the tree) is the one
// supervise request that ends the tree's root claim; the supervisor refuses any other stop of a root.
func TestOutboxTreeCloseRowEndsTheTreesRootClaim(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	// A tree's close is the expiry of its linger, so the tree lingers when the row runs.
	until := time.Now().Add(-time.Minute)
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}
	putOutboxIssue(t, pool, records, issue)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleArchitect, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create root claim: %v", err)
	}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets"}

	if err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "tree_close", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: issue.Generation}, time.Now())); err != nil {
		t.Fatalf("stop the root at its tree's close: %v", err)
	}

	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Fatalf("root claim state after the tree's close = %s, want retired", got)
	}
	if releases := runtime.CallsOf("Release"); len(releases) != 1 || releases[0].Released.Claim != token {
		t.Fatalf("releases = %+v, want the root released once", releases)
	}
}

// The daemon wires treeClosable into every claim's machine whenever a workflow is configured, and
// the workflow closes a tree whose linger expired while it still holds that tree's issue record —
// the very record treeClosable refuses on. So the two must not meet: a workflow tree_close is the
// workflow's own decision and is never put to treeClosable, while the operator's close of the same
// tree is refused. Without this wiring in the test, a predicate that refused both looked green.
func TestTheWorkflowsTreeCloseIsNotPutToTheOperatorsPredicate(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	until := time.Now().Add(-time.Minute)
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}
	putOutboxIssue(t, pool, records, issue)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.TreeClosable = (&workflowRuntime{pool: pool, records: records}).treeClosable // exactly what daemon.go wires in production
	token, err := claim.NewToken("legion", issue.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleArchitect, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create root claim: %v", err)
	}

	// The operator's close of a tree a workflow issue backs is refused, and changes nothing.
	operatorClose := machine.Handle(context.Background(), supervise.RequestOperatorClose{Claim: token})
	var refused *supervise.RefusedError
	if !errors.As(operatorClose, &refused) || !strings.Contains(refused.Error(), "closes when its linger expires") {
		t.Fatalf("operator close = %v, want the workflow-tree refusal", operatorClose)
	}
	if got := machine.Claim().State; got == supervise.StateRetired {
		t.Fatal("a refused operator close retired the root claim")
	}

	// The workflow's own close of that same tree goes through and releases the root.
	runner := &outbox{pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets"}
	if err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.SuperviseRequest{
		Op: "tree_close", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: issue.Generation,
	}, time.Now())); err != nil {
		t.Fatalf("the workflow's tree close: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Fatalf("root claim state after the workflow's tree close = %s, want retired", got)
	}
	if releases := runtime.CallsOf("Release"); len(releases) != 1 || releases[0].Released.Claim != token {
		t.Fatalf("releases = %+v, want the root released once", releases)
	}
}

// The daemon wires phaseHolds into every claim's machine, and one path a delivery takes rewrites
// the delivery: a prompt acknowledged and then refused is taken back under a new id
// (supervise.promptFailed). That is exactly the state a late foreign turn leaves behind, which is
// the state this drop exists for, so the workflow's own task must still be dropped after the
// rewrite once its issue has left the phase the task was queued for. A predicate that reads
// anything the retry rewrites answers this wrong in production and green in a test that skips it.
func TestAWorkflowTaskIsDroppedAfterItsRetryRewritesTheDelivery(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds // exactly what daemon.go wires in production
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create the implementer claim: %v", err)
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	runner := &outbox{
		pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
		log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}

	// The workflow starts the implementer on its phase, through the outbox row that mints the
	// delivery the daemon owns.
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, record.SuperviseRequest{
		Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing,
		Task: "Continue Workflow. Issue: LEGION-208. Phase: implementing.",
	}, time.Now())); err != nil {
		t.Fatalf("start the implementer on its phase: %v", err)
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl"},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-impl"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	eventually(t, "the phase's task to be acknowledged", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
	queued := machine.Claim().Pending.ID

	// A turn the task did not start, and the agent then refusing the prompt it had acknowledged:
	// the delivery is taken back under a new id, and the foreign turn ends.
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start the foreign turn: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamLateRefusal{Claim: token, DeliveryID: queued,
		Error: "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."}); err != nil {
		t.Fatalf("refuse the acknowledged prompt: %v", err)
	}
	if rotated := machine.Claim().Pending; rotated == nil || rotated.ID == queued {
		t.Fatalf("pending delivery after the refusal = %+v, want it kept under a new id", rotated)
	}

	// The phase ends while the retry waits: the work the task asks for is done.
	putOutboxIssue(t, pool, records, record.Issue{
		Key: issue.Key, Project: issue.Project, Tree: issue.Tree, Title: issue.Title, Phase: phase.Testing, Generation: 1, Status: "testing",
	})
	prompts := len(conn.Prompts())

	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("end the foreign turn: %v", err)
	}

	if got := machine.Claim().Pending; got != nil {
		t.Fatalf("the finished phase's task is still pending as %+v, want it dropped", got)
	}
	// A send runs on its own goroutine, so counting the prompts as Handle returns can only ever
	// see none. The machine's own wait is what says every goroutine this event started is done.
	machine.Wait()
	if got := len(conn.Prompts()) - prompts; got != 0 {
		t.Fatalf("the finished worker was handed its own finished task %d more times, want none", got)
	}
}

// A start whose task meets the claim's own pending delivery — a claim a retry relaunched still
// holds the task it was relaunched with, until that turn ends — waits: the row stays and runs again
// on the outbox's backoff, and the wait is logged at debug, never as a failed row.
func TestOutboxStartWaitingOnThePendingDeliveryIsNotAFailure(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create implementer claim: %v", err)
	}
	for _, ev := range []supervise.Event{supervise.RequestSpawn{Claim: token}, supervise.RequestDeliver{Claim: token, Task: "the task it was relaunched with"}} {
		if err := machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("%T: %v", ev, err)
		}
	}
	var logs bytes.Buffer
	now := time.Now().UTC()
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
		now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Task: "the retry's task", Generation: issue.Generation}, now))

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("logged a failure for a wait:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "level=DEBUG") || !strings.Contains(logs.String(), "pending delivery") {
		t.Errorf("logged no debug line naming the pending delivery:\n%s", logs.String())
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Errorf("outbox rows = %d, want the start kept to run again", rows)
	}
	if pending := machine.Claim().Pending; pending == nil || pending.Task != "the task it was relaunched with" {
		t.Errorf("pending %+v, want the claim's own task kept", pending)
	}
}
