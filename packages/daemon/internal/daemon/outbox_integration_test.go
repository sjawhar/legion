package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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

// The boot sweep alone recovers what a restart finds in the outbox, with no wake and no poll: a due
// row no one leased, and a row a runner that died mid-run left under a lease nobody holds, run as
// soon as the lease has lapsed; a row due later runs when its moment comes on the clock, and the
// sweep's tick before then leaves it alone.
func TestOutboxRestartDrainsDueThenExpiredLeaseRows(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	clock := microClock()
	now := clock.Now()
	// Three issues: one issue's status rows run one at a time by design, so each row is on a lane
	// of its own and none waits for another.
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-209", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-210", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now.Add(time.Hour)))
	if _, err := pool.Exec(context.Background(), "update outbox set lease_token = 'a-runner-that-died', lease_until = $2 where issue = $1",
		"LEGION-209", now.Add(-time.Second)); err != nil {
		t.Fatalf("leave a lapsed lease on LEGION-209's row: %v", err)
	}

	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}}
	restarted := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, log: quietLogger()}
	runLanes(t, restarted, clock)

	testwait.Eventually(t, "the due row and the lapsed lease's row ran on the boot sweep", func() bool {
		return client.count() == 2 && outboxRows(t, pool) == 1
	})
	testwait.Eventually(t, "the later row's lane armed its wait for the hour", func() bool {
		live, _ := clock.armed(time.Hour)
		return live == 1
	})

	// The sweep's tick before the hour wakes the later row's lane, which finds nothing due and arms
	// its wait again; the row stays.
	fireSweep(t, clock)
	testwait.Eventually(t, "the sweep ran and the later row's lane looked again", func() bool {
		live, total := clock.armed(time.Hour)
		_, sweeps := clock.armed(outboxLease)
		return live == 1 && total == 2 && sweeps == 2
	})
	if got, remaining := client.count(), outboxRows(t, pool); got != 2 || remaining != 1 {
		t.Fatalf("after the sweep before the hour: writes = %d with %d rows left, want two and the later row", got, remaining)
	}

	clock.advance(time.Hour)
	if fired := clock.fire(time.Hour); fired != 1 {
		t.Fatalf("hour timers fired = %d, want the later row's lane's", fired)
	}
	testwait.Eventually(t, "the later row ran at its hour", func() bool {
		return client.count() == 3 && outboxRows(t, pool) == 0
	})
}

func TestOutboxStaleLeaseFinishLeavesNewLeaseRowUntouched(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, now))

	first := claimDueOutboxRow(t, pool, records, now)
	second := claimDueOutboxRow(t, pool, records, first.LeaseUntil.Add(time.Nanosecond))
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

// claimDueOutboxRow leases the first row of the project due at now, as a lane does: listed, then
// claimed by its id.
func claimDueOutboxRow(t *testing.T, pool *pgxpool.Pool, records record.Store, now time.Time) record.OutboxRow {
	t.Helper()
	var claimed record.OutboxRow
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		due, err := records.DueRows(context.Background(), tx, "LEGION", "", nil, now)
		if err != nil {
			return err
		}
		if len(due) == 0 {
			return errors.New("no row is due")
		}
		row, ok, err := records.ClaimRow(context.Background(), tx, due[0].ID, now, outboxLease)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("row %d was not claimed", due[0].ID)
		}
		claimed = row
		return nil
	}); err != nil {
		t.Fatalf("claim the due outbox row: %v", err)
	}
	return claimed
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
		if err := testpg.Drop(context.Background(), admin, name); err != nil {
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
func (d *blockingDispatch) SetStatus(context.Context, string, string) (dispatch.Issue, error) {
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
	return dispatch.Issue{}, fail
}
func (d *blockingDispatch) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	return d.SetStatus(ctx, issue.Key, status)
}
func (d *blockingDispatch) HandOver(context.Context, dispatch.Issue, string) error {
	return errors.New("blockingDispatch takes no hand-over")
}
func (d *blockingDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *blockingDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *blockingDispatch) EventsAfter(context.Context, string, int64) ([]json.RawMessage, error) {
	return nil, nil
}
func (d *blockingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *blockingDispatch) BuildCommit(context.Context) (string, error) { return "", nil }
func (d *blockingDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}
func (d *blockingDispatch) WebhookAppInstalled(context.Context, string) (bool, error) {
	return true, nil
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
// not from Dispatch judging the write. A 409 PARENT_CONTENDED says a concurrent reparent moved
// the issue's parent while the write waited for its locks, so nothing was applied and the same
// body is taken once the reparents stop. Reading any of them as permanent dropped every later
// status of that issue for good, which is the one outcome this outbox exists to prevent. Each
// case names the arm that saves it, so deleting one fails here.
func TestACredentialOrUncodedRefusalIsRiddenOutRatherThanDroppingTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal *dispatch.Error
	}{
		{"a revoked token", &dispatch.Error{Status: 401, Code: "UNAUTHORIZED", Message: "token revoked"}},
		{"a token without the scope", &dispatch.Error{Status: 403, Code: "FORBIDDEN", Message: "forbidden"}},
		{"a refusal Dispatch did not make", &dispatch.Error{Status: 422, Message: "<html>gateway</html>"}},
		{"a parent a concurrent reparent moved at every attempt", &dispatch.Error{Status: 409, Code: dispatch.ParentContended, Message: "LEGION-208's parent changed at each of 2 attempts while this request waited to lock it, so nothing was applied: read the issue and retry"}},
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
func (d *refusingDispatch) SetStatus(_ context.Context, _ string, status string) (dispatch.Issue, error) {
	if refusal, refused := d.refuse[status]; refused {
		return dispatch.Issue{}, fmt.Errorf("set Dispatch status %s: %w", status, refusal)
	}
	d.status = status
	return dispatch.Issue{}, nil
}
func (d *refusingDispatch) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	return d.SetStatus(ctx, issue.Key, status)
}
func (d *refusingDispatch) HandOver(context.Context, dispatch.Issue, string) error {
	return errors.New("refusingDispatch takes no hand-over")
}
func (d *refusingDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *refusingDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *refusingDispatch) EventsAfter(context.Context, string, int64) ([]json.RawMessage, error) {
	return nil, nil
}
func (d *refusingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *refusingDispatch) BuildCommit(context.Context) (string, error) { return "", nil }
func (d *refusingDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}
func (d *refusingDispatch) WebhookAppInstalled(context.Context, string) (bool, error) {
	return true, nil
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
func (d *boardDispatch) SetStatus(_ context.Context, _ string, status string) (dispatch.Issue, error) {
	if d.failures > 0 {
		d.failures--
		return dispatch.Issue{}, errors.New("Dispatch down")
	}
	d.status = status
	return dispatch.Issue{}, nil
}
func (d *boardDispatch) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	return d.SetStatus(ctx, issue.Key, status)
}
func (d *boardDispatch) HandOver(context.Context, dispatch.Issue, string) error {
	return errors.New("boardDispatch takes no hand-over")
}
func (d *boardDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *boardDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *boardDispatch) EventsAfter(context.Context, string, int64) ([]json.RawMessage, error) {
	return nil, nil
}
func (d *boardDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *boardDispatch) BuildCommit(context.Context) (string, error) { return "", nil }
func (d *boardDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}
func (d *boardDispatch) WebhookAppInstalled(context.Context, string) (bool, error) { return true, nil }

// A person's handover row (dispatch://LEGION-755) is the one status row that brings its issue
// into the workflow rather than moving one along inside it: a todo observed at the `triage` a
// person's legion label found, or at the `backlog` or `icebox` their approval of a parked root's
// spec found, marked PersonHandover. When Dispatch refuses its fenced write because an event
// landed between the row's read and that write (ISSUE_CHANGED) — an interleaved comment, an edit
// — the retry is judged by the handover's own rule, that the label is still there
// (personHandoverStands), and nothing of the re-read is applied.
//
// Judged by statusStillWanted instead, whose record.OutOfWorkflow is true for exactly those three
// statuses, every such row was dropped without writing and logged as having left a workflow its
// issue had never entered; and the read applied on the triage path told admission the label was
// nobody's — intake.ReadIssue carries no LabelSetter — which queued a `triage` wake for a root
// the controller is told to leave alone (skills/legion-controller/references/triage.md).
//
// The runner holds a real pool and the two handlers that would queue that wake (workflow.New and
// admit.New, the pair the hand-over tests wire): with a nil pool applyRead returns before applying
// anything, so the wake assertion would hold whether the retry applies its read or not. The cases
// share the pool and take a key each, so one case's wake is never read for another's.
func TestAPersonsHandoverIsWrittenAfterDispatchRefusesItsFirstWrite(t *testing.T) {
	const dropped = "outbox: a person's handover is not written; the label is gone"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	for _, tc := range []struct {
		name     string
		key      string
		observed string
		// labelled is the label as the re-read after the refusal shows it: a person who took it
		// off between the queue and this delivery withdrew the handover.
		labelled bool
		statuses []string
		logged   int
	}{
		{name: "a person's label on a triage root", key: "LEGION-751", observed: "triage", labelled: true, statuses: []string{"todo"}},
		{name: "a person's approval of a backlog root", key: "LEGION-752", observed: "backlog", labelled: true, statuses: []string{"todo"}},
		{name: "a person's approval of an icebox root", key: "LEGION-753", observed: "icebox", labelled: true, statuses: []string{"todo"}},
		{name: "the label taken off between the queue and the retry", key: "LEGION-754", observed: "triage", logged: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shown := dispatch.Issue{Key: tc.key, Status: tc.observed, Title: tc.key, Rank: "A", Labels: []string{dispatch.LegionLabel},
				HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 4}
			// What the read after the refusal shows: the issue at the status the row was queued
			// from, at the sequence the interleaved event moved it to.
			reread := shown
			reread.LastSeq, reread.HandedOver = shown.LastSeq+1, tc.labelled
			if !tc.labelled {
				reread.Labels = nil
			}
			client := &refusedOnceDispatch{outboxDispatch: outboxDispatch{issue: shown}, reread: reread}
			var logs bytes.Buffer
			runner := &outbox{pool: pool, records: records, dispatch: client, handlers: []intake.Handler{engine, admission},
				log: slog.New(slog.NewJSONHandler(&logs, nil))}
			row := mustOutboxRow(t, tc.key, record.StatusWrite{Status: "todo", ObservedStatus: tc.observed, PersonHandover: true}, time.Now())

			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute the person's handover: %v", err)
			}

			if client.refusals != 1 {
				t.Fatalf("Dispatch refused %d writes, want the one interleaved change this test is about", client.refusals)
			}
			if !slices.Equal(client.statuses, tc.statuses) {
				t.Fatalf("status writes = %v, want %v", client.statuses, tc.statuses)
			}
			if len(tc.statuses) > 0 && client.at != reread.LastSeq {
				t.Fatalf("the retry wrote at sequence %d, want the re-read's %d", client.at, reread.LastSeq)
			}
			if wakes := triageWakes(t, pool, tc.key); wakes != 0 {
				t.Fatalf("triage wakes queued for the controller = %d, want none: the retry applies nothing of its read", wakes)
			}
			if lines := loggedLines(t, logs.String(), dropped); len(lines) != tc.logged {
				t.Fatalf("logged %q %d times, want %d: %s", dropped, len(lines), tc.logged, logs.String())
			}
		})
	}
}

// The daemon's re-assert of its own status over one a session wrote is the other row queued at a
// status the issue is not meant to be at (workflow's sessionStatusWrite): the session set the
// issue done, backlog or icebox, the record kept the daemon's status, and this row sets Dispatch
// back. One interleaved change — the agent's next comment is enough — refuses its first write, and
// the retry must write again. Before, it asked the ordinary rule, whose record.OutOfWorkflow is
// true for exactly those three statuses, so the re-assert was dropped and the session's own close
// stood; and the retry applied its read, which carries no actor, so the engine took the session's
// close for a person's, lingered the tree and freed its slot. Neither happens now: the read
// decides whether to write and nothing else.
func TestTheDaemonsReassertIsWrittenAfterDispatchRefusesItsFirstWrite(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	for _, tc := range []struct {
		name string
		key  string
		// wrote is the status the session set, which the row observes and the re-read still shows.
		wrote string
	}{
		{name: "a session closed its own issue", key: "LEGION-761", wrote: "done"},
		{name: "a session parked its own issue to backlog", key: "LEGION-762", wrote: "backlog"},
		{name: "a session parked its own issue to icebox", key: "LEGION-763", wrote: "icebox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shown := dispatch.Issue{Key: tc.key, Status: tc.wrote, Title: tc.key, Rank: "A", Labels: []string{dispatch.LegionLabel},
				HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 4}
			reread := shown
			reread.LastSeq = shown.LastSeq + 1
			client := &refusedOnceDispatch{outboxDispatch: outboxDispatch{issue: shown}, reread: reread}
			runner := &outbox{pool: pool, records: records, dispatch: client, handlers: []intake.Handler{engine, admission},
				log: quietLogger()}
			row := mustOutboxRow(t, tc.key, record.StatusWrite{Status: "in_progress", ObservedStatus: tc.wrote, Reassert: true}, time.Now())

			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute the re-assert: %v", err)
			}

			if client.refusals != 1 {
				t.Fatalf("Dispatch refused %d writes, want the one interleaved change this test is about", client.refusals)
			}
			if !slices.Equal(client.statuses, []string{"in_progress"}) {
				t.Fatalf("status writes = %v, want the daemon's own status written after the refusal", client.statuses)
			}
			if client.at != reread.LastSeq {
				t.Fatalf("the retry wrote at sequence %d, want the re-read's %d", client.at, reread.LastSeq)
			}
			// Nothing of the read reached the engine, so the session's close ended no tree: an
			// issue the engine had taken out of the workflow would be recorded, and a linger
			// would have queued its rows.
			if rows := outboxRowsOf(t, pool, tc.key); rows != 0 {
				t.Fatalf("%d rows left for %s, want none: a linger would have queued its own", rows, tc.key)
			}
		})
	}
}

// Every kind of status row has a rule its retry is judged by, and stillWanted has a case for
// each. A kind added without one reaches the default and fails its row rather than being judged
// by another kind's rule, which is the mistake this case exists to make loud: each kind is asked
// for real, against a record and a read that stand, and none may answer with that refusal.
func TestEveryStatusRowKindHasAStillWantedRule(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	runner := &outbox{pool: pool, records: records, log: quietLogger()}
	for _, tc := range []struct {
		kind    statusRowKind
		payload record.StatusWrite
	}{
		{ordinaryStatusRow, record.StatusWrite{Status: "testing", ObservedStatus: "in_progress"}},
		{personHandoverRow, record.StatusWrite{Status: "todo", ObservedStatus: "triage", PersonHandover: true}},
		{reassertRow, record.StatusWrite{Status: "in_progress", ObservedStatus: "done", Reassert: true}},
	} {
		if got := statusKindOf(tc.payload); got != tc.kind {
			t.Fatalf("statusKindOf(%+v) = %d, want %d", tc.payload, got, tc.kind)
		}
		read := dispatch.Issue{Key: "LEGION-208", Status: tc.payload.ObservedStatus, HandedOver: true}
		if _, err := runner.stillWanted(context.Background(), record.OutboxRow{Issue: "LEGION-208"}, tc.payload, read); err != nil {
			t.Fatalf("the rule of status row kind %d answered %v, want it to answer at all", tc.kind, err)
		}
	}
}

// refusedOnceDispatch refuses the first fenced status write with ISSUE_CHANGED, as Dispatch
// refuses a write whose read an event overtook, and shows reread — the issue as it stands at the
// sequence that event moved it to — to every read after that. refusals counts the refusals it
// made, and at is the sequence of the last write it took.
type refusedOnceDispatch struct {
	outboxDispatch
	reread   dispatch.Issue
	refusals int
	at       int64
}

func (d *refusedOnceDispatch) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	if d.refusals == 0 {
		d.refusals, d.issue = 1, d.reread
		return dispatch.Issue{}, &dispatch.Error{Status: http.StatusConflict, Code: "ISSUE_CHANGED", Message: "issue changed"}
	}
	d.at = issue.LastSeq
	return d.outboxDispatch.SetStatusAt(ctx, issue, status)
}

// triageWakes counts the controller notices of kind triage the outbox holds for issue.
func triageWakes(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var wakes int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and issue = $2 and payload->>'kind' = $3`,
		string(record.OutboxKindControllerNotice), issue, string(record.TriageNotice)).Scan(&wakes); err != nil {
		t.Fatalf("count the triage wakes queued for %s: %v", issue, err)
	}
	return wakes
}

// A failed transaction is one row's failure, never the runner's end: a Postgres restart drops the
// pool's connections, and the outbox has to keep draining once they are back. A claim that fails
// leaves the row due for the sweep, whose next tick runs it; a row whose finish fails keeps its
// lease, under which no sweep runs it, and the sweep after the lease lapses runs it again. The
// row's effect is written once before the failed finish and once after.
func TestOutboxRunSurvivesAFailedClaimAndAFailedFinish(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := &flakyOutboxStore{Store: record.NewStore(), claimFailures: 1, finishFailures: 1}
	clock := microClock()
	client := &blockingDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, log: quietLogger()}
	runLanes(t, runner, clock)

	// This outbox has no workflow writing through projectRecords, so the test wakes the lane the
	// way the committed write would.
	row := mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, clock.Now())
	enqueueOutbox(t, pool, records, row)
	runner.wake(row.Issue, row.Kind)
	testwait.Eventually(t, "the claim failed", func() bool { return records.failedClaims() == 1 })
	if got := client.count(); got != 0 {
		t.Fatalf("status writes after the failed claim = %d, want none", got)
	}

	fireSweep(t, clock)
	testwait.Eventually(t, "the row executed and its finish failed", func() bool { return client.count() == 1 && records.failedFinishes() == 1 })

	// Under the lease the failed finish left, the row is no one's to run: the sweep before the
	// lapse wakes its lane, which finds nothing due and exits.
	fireSweep(t, clock)
	testwait.Eventually(t, "the lane exited with the row under its lapsed run's lease", func() bool { return liveLanes(runner) == 0 })
	if got, remaining := client.count(), outboxRows(t, pool); got != 1 || remaining != 1 {
		t.Fatalf("before the lease lapsed: writes = %d with %d rows left, want one and the leased row", got, remaining)
	}

	clock.advance(outboxLease + time.Second)
	fireSweep(t, clock)
	testwait.Eventually(t, "the row finished after its lease lapsed", func() bool { return outboxRows(t, pool) == 0 })
	if got := client.count(); got != 2 {
		t.Fatalf("status writes = %d, want the first run and the one after the lease lapsed", got)
	}
}

// flakyOutboxStore fails the first claimFailures ClaimRow and finishFailures FinishOutbox calls the
// way a dropped connection does, and passes every later call to the real store.
type flakyOutboxStore struct {
	record.Store
	mu             sync.Mutex
	claimFailures  int
	finishFailures int
	claimsFailed   int
	finishesFailed int
}

func (s *flakyOutboxStore) ClaimRow(ctx context.Context, tx pgx.Tx, id int64, now time.Time, leaseFor time.Duration) (record.OutboxRow, bool, error) {
	s.mu.Lock()
	fail := s.claimFailures > 0
	if fail {
		s.claimFailures--
		s.claimsFailed++
	}
	s.mu.Unlock()
	if fail {
		return record.OutboxRow{}, false, errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")
	}
	return s.Store.ClaimRow(ctx, tx, id, now, leaseFor)
}

func (s *flakyOutboxStore) FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	if err := s.failFinish(); err != nil {
		return err
	}
	return s.Store.FinishOutbox(ctx, tx, id, leaseToken)
}

func (s *flakyOutboxStore) failedClaims() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimsFailed
}

func (s *flakyOutboxStore) FinishLeasedOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	if err := s.failFinish(); err != nil {
		return err
	}
	return s.Store.FinishLeasedOutbox(ctx, tx, id, leaseToken)
}

// failFinish is the dropped connection a finish meets while finishFailures remain.
func (s *flakyOutboxStore) failFinish() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finishFailures == 0 {
		return nil
	}
	s.finishFailures--
	s.finishesFailed++
	return errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")
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
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}
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
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir()}

	if err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "tree_close", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: issue.Generation, Linger: issue.Generation}, time.Now())); err != nil {
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
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}
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
	runner := &outbox{pool: pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir()}
	if err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.SuperviseRequest{
		Op: "tree_close", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: issue.Generation, Linger: issue.Generation,
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
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
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
		pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
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
	testwait.Eventually(t, "the phase's task to be acknowledged", func() bool {
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
		Key: issue.Key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: issue.Project, Tree: issue.Tree, Title: issue.Title, Phase: phase.Testing, Generation: 1, Status: "testing",
	})
	prompts := len(conn.Prompts())

	endTurn(ctx, t, machine, token)

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
// on the outbox's backoff, and its first attempt is logged as a wait (wait.ErrWaiting), never as a
// failed row.
func TestOutboxStartWaitingOnThePendingDeliveryIsNotAFailure(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
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
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Task: "the retry's task", Generation: issue.Generation}, now))

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("logged a failure for a wait:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `level=INFO msg="outbox row waits" row=1 kind=supervise`) || !strings.Contains(logs.String(), "a delivery is already pending") {
		t.Errorf("logged no wait line naming the pending delivery:\n%s", logs.String())
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Errorf("outbox rows = %d, want the start kept to run again", rows)
	}
	if pending := machine.Claim().Pending; pending == nil || pending.Task != "the task it was relaunched with" {
		t.Errorf("pending %+v, want the claim's own task kept", pending)
	}
}
