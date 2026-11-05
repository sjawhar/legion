package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The pace these tests hold the daemon to: the shipped eight wakes a minute in bursts of one
// (config.Config.WorkerWakesPerMinute, WorkerWakeBurst), whose interval is what one wake costs.
const (
	testWakeRate     = 8
	testWakeBurst    = 1
	testWakeInterval = time.Minute / testWakeRate
)

// wakeOrigin is the one instant these tests reserve and run from, so every moment reads as an
// offset from it and no test needs a wall clock.
var wakeOrigin = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// pacedWakes is the daemon's own wake limiter at the shipped pace, with the jitter the daemon
// spreads its paced releases by (openSupervision's rand.N): a test that reads the schedule reads
// the production one, gaps and all.
func pacedWakes() *wakepace.Limiter {
	return wakepace.New(testWakeRate, testWakeBurst, rand.N[time.Duration])
}

// wakeEngine is the workflow engine as the daemon builds it when the pacing is on: its clock held
// at now, and its Reserve the limiter's (openWorkflow). A nil limiter is the pacing off, which is
// the daemon giving the engine no Reserve at all.
func wakeEngine(records record.Store, limiter *wakepace.Limiter, now func() time.Time) *workflow.Engine {
	cfg := workflow.Config{Project: "legion", Repositories: testRepositories, Clock: now}
	if limiter != nil {
		cfg.Reserve = limiter.Reserve
	}
	return workflow.New(records, cfg, quietLogger())
}

// runningArchitect records a root of its own tree, in_progress at generation 1, with an architect
// claim already working: the state an outbox start row finds when a re-admission reaches a tree
// whose session lived on.
func runningArchitect(t *testing.T, pool *pgxpool.Pool, records record.Store, sup *supervisor, key string) {
	t.Helper()
	putOutboxIssue(t, pool, records, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: key, Title: "Root " + key, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: mustClaimToken(t, key, claim.RoleArchitect), Project: "legion", Tree: key, Issue: key,
		Role: claim.RoleArchitect, State: supervise.StateWorking,
	}, ""); err != nil {
		t.Fatalf("create the running architect claim of %s: %v", key, err)
	}
}

// startRow is the start an outbox runs for key's root architect at its current generation.
func startRow(t *testing.T, key string, now time.Time) record.OutboxRow {
	t.Helper()
	return mustOutboxRow(t, key, record.SuperviseRequest{Op: "start", Tree: key, Role: claim.RoleArchitect, Generation: 1}, now)
}

// noticeRow is one outbox notice row as the database holds it: its id, whether its payload is
// marked paced, when it was written and when it is due.
type noticeRow struct {
	id        int64
	paced     bool
	createdAt time.Time
	nextAt    time.Time
}

// noticeRows is every notice row of the project, oldest first, with the payload mark and the two
// moments the pacing is read from.
func noticeRows(t *testing.T, pool *pgxpool.Pool) []noticeRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, coalesce((payload->>'paced')::boolean, false), created_at, next_at
		from outbox where kind = $1 order by id`, string(record.OutboxKindNotice))
	if err != nil {
		t.Fatalf("read the notice rows: %v", err)
	}
	defer rows.Close()
	var held []noticeRow
	for rows.Next() {
		var row noticeRow
		if err := rows.Scan(&row.id, &row.paced, &row.createdAt, &row.nextAt); err != nil {
			t.Fatalf("scan a notice row: %v", err)
		}
		row.createdAt, row.nextAt = row.createdAt.UTC(), row.nextAt.UTC()
		held = append(held, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the notice rows: %v", err)
	}
	return held
}

// pacedCatchUp writes the catch-up a start row's ready writes when the pacing holds it to at, and
// stamps the row as the engine's transaction would: composed at now, due at at. The stamp is the
// test's, because the database writes created_at from its own clock while these tests hold theirs
// still; everything else is workflow.QueueCatchUp's own work.
func pacedCatchUp(t *testing.T, pool *pgxpool.Pool, records record.Store, root string, launch uint64, now, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		stored, err := records.Issue(ctx, tx, root)
		if err != nil || stored == nil {
			return fmt.Errorf("read %s: %w", root, err)
		}
		return workflow.QueueCatchUp(ctx, tx, records, config.DesignGateRootIssues, *stored, launch, now, at)
	}); err != nil {
		t.Fatalf("queue the paced catch-up of %s: %v", root, err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `update outbox set created_at = $2 where id =
		(select max(id) from outbox where kind = $1) returning id`, string(record.OutboxKindNotice), now).Scan(&id); err != nil {
		t.Fatalf("stamp the paced catch-up of %s: %v", root, err)
	}
	return id
}

// Forty re-admissions whose architects are all still running write forty catch-ups in place of
// forty launches, and the rows they write are the limiter's schedule: the first is due at once,
// every later one at least an interval and at most two after the one before it (the jitter the
// daemon spreads its releases by), and no minute of that schedule holds more than the rate plus
// the burst. That is the whole point of pacing these wakes: a restart that found every architect
// alive used to write forty catch-ups due at once, and forty sessions then read their whole
// context inside the minute that follows.
func TestFortyStartsOfRunningArchitectsWriteCatchUpsOnTheLimitersSchedule(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	now := func() time.Time { return wakeOrigin }
	engine := wakeEngine(records, pacedWakes(), now)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		handlers: []intake.Handler{engine, admit.New(records, engine, 2, "legion", quietLogger())}, now: now}

	const starts = 40
	for i := 1; i <= starts; i++ {
		key := fmt.Sprintf("LEGION-%d", i)
		runningArchitect(t, pool, records, sup, key)
		if err := runner.execute(ctx, startRow(t, key, wakeOrigin)); err != nil {
			t.Fatalf("start the running architect of %s: %v", key, err)
		}
	}

	rows := noticeRows(t, pool)
	if len(rows) != starts {
		t.Fatalf("notice rows after %d starts = %d, want one catch-up each", starts, len(rows))
	}
	for _, row := range rows {
		if !row.paced {
			t.Fatalf("catch-up row %d is not marked paced: a ready would pull it forward and the wave would go out at once", row.id)
		}
	}
	if !rows[0].nextAt.Equal(wakeOrigin) {
		t.Fatalf("the first catch-up is due at %s, want %s: the burst lets the first wake straight out", rows[0].nextAt, wakeOrigin)
	}
	for i := 1; i < len(rows); i++ {
		gap := rows[i].nextAt.Sub(rows[i-1].nextAt)
		if gap < testWakeInterval || gap >= 2*testWakeInterval {
			t.Fatalf("catch-ups %d and %d are %s apart, want at least %s and less than %s: one interval plus the jitter",
				rows[i-1].id, rows[i].id, gap, testWakeInterval, 2*testWakeInterval)
		}
	}
	for i, row := range rows {
		inMinute := 0
		for _, other := range rows[i:] {
			if other.nextAt.Sub(row.nextAt) < time.Minute {
				inMinute++
			}
		}
		if inMinute > testWakeRate+testWakeBurst {
			t.Fatalf("the minute from %s holds %d catch-ups, want at most %d: the rate plus the burst",
				row.nextAt, inMinute, testWakeRate+testWakeBurst)
		}
	}
}

// A ready of an architect releases the notices that wait for it (releaseWaiting), and a catch-up
// the pacing holds is not one of them: its wait is the turn the operator's rate gave this wake,
// not the architect being away. A notice waiting on the outbox's backoff beside it is released as
// it always was.
func TestAReadyLeavesAPacedCatchUpAtItsOwnMoment(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architect := architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	now := wakeOrigin
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		supervisor: sup, project: "legion", now: func() time.Time { return now }}

	due := now.Add(5 * testWakeInterval)
	paced := pacedCatchUp(t, pool, records, "LEGION-1", 1, now, due)
	held := mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, now.Add(time.Minute))
	enqueueOutbox(t, pool, records, held)

	if err := runner.releaseWaiting(ctx, architect); err != nil {
		t.Fatalf("release the notices waiting for %s: %v", architect, err)
	}

	for _, row := range noticeRows(t, pool) {
		switch {
		case row.id == paced && !row.nextAt.Equal(due):
			t.Fatalf("the paced catch-up is due at %s after the ready, want %s: a ready that pulls it forward lets the wave out at once",
				row.nextAt, due)
		case row.id != paced && !row.nextAt.Equal(now):
			t.Fatalf("the notice waiting on its backoff is due at %s after the ready, want %s: its architect is back", row.nextAt, now)
		}
	}
}

// A later notice of the tree, written to the same architect while its catch-up waits its turn, is
// published after the catch-up and never before it: the architect is woken once, by the catch-up,
// and reads the later notice in the turn that wake starts.
func TestALaterNoticeIsPublishedAfterThePacedCatchUpAndNeverBeforeIt(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	now := wakeOrigin
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
		supervisor: sup, project: "legion", now: func() time.Time { return now }}

	due := now.Add(4 * testWakeInterval)
	pacedCatchUp(t, pool, records, "LEGION-1", 1, now, due)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RoleArchitect,
		Phase: phase.Planning, Summary: "the planner finished"}, now))

	// The later notice is due now and the catch-up is not: it runs, finds the catch-up ahead of
	// it, and publishes nothing.
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the outbox while the catch-up waits: %v", err)
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 0 {
		t.Fatalf("published %+v while the catch-up waits, want nothing: the architect is woken once, by its catch-up", delivered)
	}

	now = due
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the outbox at the catch-up's moment: %v", err)
	}
	_, delivered := publisher.snapshot()
	kinds := make([]string, len(delivered))
	for i, published := range delivered {
		kinds[i] = string(published.payload.(record.Notice).Kind)
	}
	if fmt.Sprint(kinds) != "[catch-up phase-finished]" {
		t.Fatalf("published %v, want the catch-up then the notice behind it", kinds)
	}
	if rows := outboxRows(t, pool); rows != 0 {
		t.Fatalf("%d outbox rows left, want none: both went", rows)
	}
}

// The row the fence holds behind a paced catch-up runs once, at the catch-up's own moment, rather
// than climbing the outbox's backoff to it: a 49-minute hold on the backoff is some twenty-five
// attempts, and every attempt from the seventh is a warning (waitWarnAttempts), so each architect
// of a wave would leave a column of warnings that say only that the pacing is working.
func TestTheRowFencedBehindAPacedCatchUpRunsOnceAtItsMoment(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	now := wakeOrigin
	var logged bytes.Buffer
	runner := &outbox{log: slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
		pool: pool, dispatchProject: "LEGION", records: records, notices: &holderPublisher{},
		supervisor: sup, project: "legion", now: func() time.Time { return now }}

	due := now.Add(40 * time.Minute)
	paced := pacedCatchUp(t, pool, records, "LEGION-1", 1, now, due)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect,
		Reason: "max_fix_attempts"}, now))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the outbox while the catch-up waits: %v", err)
	}
	var fenced noticeRow
	for _, row := range noticeRows(t, pool) {
		if row.id != paced {
			fenced = row
		}
	}
	if !fenced.nextAt.Equal(due) {
		t.Fatalf("the fenced row is due at %s, want the catch-up's own moment %s rather than its backoff at %s",
			fenced.nextAt, due, now.Add(retryBackoff(0)))
	}

	// Nothing more runs until that moment: the row waited once, not on a climbing backoff.
	attempts := func() int {
		var attempts int
		if err := pool.QueryRow(ctx, "select attempts from outbox where id = $1", fenced.id).Scan(&attempts); err != nil {
			t.Fatalf("read the fenced row's attempts: %v", err)
		}
		return attempts
	}
	now = now.Add(30 * time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the outbox half way through the hold: %v", err)
	}
	if got := attempts(); got != 1 {
		t.Fatalf("the fenced row has %d attempts half way through the hold, want the one it waited on", got)
	}
	for line := range strings.Lines(logged.String()) {
		var entry struct{ Level, Msg string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry.Level == slog.LevelWarn.String() || entry.Level == slog.LevelError.String() {
			t.Fatalf("the fence logged %s %q, want nothing above info: a wake waiting its turn is not something an operator should look at",
				entry.Level, entry.Msg)
		}
	}
}

// Neither of the two catch-ups the pacing must not delay is delayed: the one a launch's own ready
// writes, whose launch already took its turn at the machine's gate, and the one a hand-over
// writes, which answers a person who is waiting. Both are written while the limiter is spent, so a
// reservation of their own would be minutes out.
func TestAnOrdinaryCatchUpAndAHandOversAreNotDelayed(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	now := wakeOrigin
	limiter := pacedWakes()
	engine := wakeEngine(records, limiter, func() time.Time { return now })
	for range 10 {
		limiter.Reserve(now)
	}

	// The launch's own ready: the fact the supervision observation applies, which carries no
	// pacing mark (workflowRuntime.applyTerminal).
	if _, err := intake.ApplyFact(ctx, pool, "test", "ready:LEGION-1:1",
		intake.ClaimReady{Issue: "LEGION-1", Role: claim.RoleArchitect, Launch: 1}, engine); err != nil {
		t.Fatalf("apply the launch's ready: %v", err)
	}
	assertDueAtOnce(t, pool, now, "the catch-up of a launch's ready")

	// The hand-over's, which drops the ready's as every catch-up does (DropCatchUps): the composer
	// every released decision's catch-up goes through (workflow.HandOverKept, CatchUpTheTreesOwed).
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return workflow.CatchUpTheTreesOwed(ctx, tx, records, config.DesignGateRootIssues, []string{"LEGION-1"}, now)
	}); err != nil {
		t.Fatalf("write the hand-over's catch-up: %v", err)
	}
	assertDueAtOnce(t, pool, now, "the catch-up of a hand-over")
}

// assertDueAtOnce is every notice row in the outbox being unpaced and due at now, which is what
// "not delayed" means for a catch-up the pacing must not hold.
func assertDueAtOnce(t *testing.T, pool *pgxpool.Pool, now time.Time, what string) {
	t.Helper()
	rows := noticeRows(t, pool)
	if len(rows) == 0 {
		t.Fatalf("%s wrote no notice row", what)
	}
	for _, row := range rows {
		if row.paced || !row.nextAt.Equal(now) {
			t.Fatalf("%s is row %d, paced %v and due at %s; want unpaced and due at %s", what, row.id, row.paced, row.nextAt, now)
		}
	}
}

// The outbox writes the one line a wake writes where that wake runs: when it runs a catch-up the
// pacing held. The shape is internal/supervise's own (its wakeRan), so one grep over the daemon's
// log finds every wake of a wave, whichever lane caused it.
func TestTheOutboxWritesTheWakeLineWhenItRunsAPacedNoticeRow(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	now := wakeOrigin
	var logged bytes.Buffer
	runner := &outbox{log: slog.New(slog.NewJSONHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records,
		notices: &holderPublisher{}, supervisor: sup, project: "legion",
		wakePace: supervise.WakePace{Rate: testWakeRate, Burst: testWakeBurst},
		now:      func() time.Time { return now }}

	held := 3 * testWakeInterval
	pacedCatchUp(t, pool, records, "LEGION-1", 1, now, now.Add(held))
	now = now.Add(held)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the paced catch-up at its moment: %v", err)
	}

	var lines []map[string]any
	for line := range strings.Lines(logged.String()) {
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry["msg"] == "supervise: wake ran" {
			lines = append(lines, entry)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("wake lines = %d, want the one the catch-up wrote: %s", len(lines), logged.String())
	}
	want := map[string]any{
		"level": "INFO", "msg": "supervise: wake ran", "claim": string(mustClaimToken(t, "LEGION-1", claim.RoleArchitect)),
		"kind": "catch-up", "waited": true, "waitedFor": float64(held), "paced": true,
		"rate": float64(testWakeRate), "burst": float64(testWakeBurst),
	}
	got := lines[0]
	delete(got, "time")
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the wake line is %v, want %v", got, want)
	}
}
