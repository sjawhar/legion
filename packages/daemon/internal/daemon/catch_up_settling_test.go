package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// hookPublisher is the listener with one operation run inside the publish: while the listener has
// the notice and has answered nothing, the hook runs to completion, which is where a lane meets
// the daemon — the publish is outside every transaction of the row's run. The publish is then
// given answer, nil for the delivery.
type hookPublisher struct {
	mu        sync.Mutex
	hook      func() error
	answer    error
	hookErr   error
	delivered []outboxPublish
}

func (p *hookPublisher) Publish(_ context.Context, topic, message string, payload any, key string) error {
	p.mu.Lock()
	hook := p.hook
	p.hook = nil
	p.mu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			p.mu.Lock()
			p.hookErr = err
			p.mu.Unlock()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.answer != nil {
		return p.answer
	}
	p.delivered = append(p.delivered, outboxPublish{topic: topic, message: message, key: key, payload: payload})
	return nil
}

func (p *hookPublisher) published() []outboxPublish {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]outboxPublish(nil), p.delivered...)
}

func (p *hookPublisher) ranHook(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hookErr != nil {
		t.Fatalf("the operation run inside the publish failed: %v", p.hookErr)
	}
	if p.hook != nil {
		t.Fatal("the operation armed for the publish never ran: the row published nothing")
	}
}

// keptCatchUp is the catch-up of root a ready writes, naming every decision nobody claims and
// claiming them for its row (workflow.QueueCatchUp), for launch.
func keptCatchUp(t *testing.T, pool *pgxpool.Pool, records record.Store, root string, launch uint64, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		stored, err := records.Issue(ctx, tx, root)
		if err != nil || stored == nil {
			return fmt.Errorf("read %s: %w", root, err)
		}
		return workflow.QueueCatchUp(ctx, tx, records, config.DesignGateRootIssues, *stored, launch, now)
	}); err != nil {
		t.Fatalf("queue the catch-up of %s for launch %d: %v", root, launch, err)
	}
}

// leaseCatchUp leases the one catch-up of root the outbox holds, as a lane does before it runs it
// (Store.ClaimRow, which writes the lease from the caller's clock), and returns it with its
// payload.
func leaseCatchUp(t *testing.T, pool *pgxpool.Pool, records record.Store, root string, now time.Time) (record.OutboxRow, record.Notice) {
	t.Helper()
	ctx := context.Background()
	rows := catchUpRows(t, pool, root)
	if len(rows) != 1 {
		t.Fatalf("%d catch-ups of %s in the outbox, want the one to lease", len(rows), root)
	}
	var leased record.OutboxRow
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		row, claimed, err := records.ClaimRow(ctx, tx, rows[0].ID, now, time.Minute)
		if err == nil && !claimed {
			return fmt.Errorf("outbox row %d was not claimed", rows[0].ID)
		}
		leased = row
		return err
	}); err != nil {
		t.Fatalf("lease the catch-up of %s: %v", root, err)
	}
	payload, err := record.DecodeOutboxPayload(leased)
	if err != nil {
		t.Fatalf("decode the catch-up of %s: %v", root, err)
	}
	notice, ok := payload.(record.Notice)
	if !ok || notice.CatchUp == nil {
		t.Fatalf("outbox row %d is %T, want a catch-up notice", leased.ID, payload)
	}
	return leased, notice
}

// catchUpRows is every catch-up notice of root the outbox holds, oldest first.
func catchUpRows(t *testing.T, pool *pgxpool.Pool, root string) []record.OutboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox
		where kind = $1 and issue = $2 and payload->>'kind' = 'catch-up' order by id`, string(record.OutboxKindNotice), root)
	if err != nil {
		t.Fatalf("read the catch-ups of %s: %v", root, err)
	}
	defer rows.Close()
	var held []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a catch-up of %s: %v", root, err)
		}
		held = append(held, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the catch-ups of %s: %v", root, err)
	}
	return held
}

// catchUpsNaming is, for each catch-up of root in the outbox other than row, whether its task
// names ref, by row id.
func catchUpsNaming(t *testing.T, pool *pgxpool.Pool, root string, row int64, ref string) map[int64]bool {
	t.Helper()
	naming := map[int64]bool{}
	for _, held := range catchUpRows(t, pool, root) {
		if held.ID == row {
			continue
		}
		payload, err := record.DecodeOutboxPayload(held)
		if err != nil {
			t.Fatalf("decode the catch-up %d of %s: %v", held.ID, root, err)
		}
		notice := payload.(record.Notice)
		naming[held.ID] = strings.Contains(notice.CatchUp.Decided, ref)
	}
	return naming
}

// namingRef is how many of the catch-ups catchUpsNaming read name the decision.
func namingRef(naming map[int64]bool) int {
	var named int
	for _, names := range naming {
		if names {
			named++
		}
	}
	return named
}

// keepDecision keeps one person's decision on issue, as the one rule for a decision nobody in its
// tree hears keeps it (record.RelayParkedDecision).
func keepDecision(t *testing.T, pool *pgxpool.Pool, records record.Store, issue, ref string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: issue, Ref: ref,
			Decision: record.DecisionAskAnswered, Actor: "user:alice", At: at})
	}); err != nil {
		t.Fatalf("keep the decision %s: %v", ref, err)
	}
}

// A ready's drop of the root's queued catch-ups (Store.DropCatchUps) leaves the one catch-up that
// is publishing what it named: the drop would release its decisions and the fresh catch-up, written
// in the same transaction, would name them again, telling the architect that already has the
// publish in its hands a person's decision twice. A catch-up under a live lease that carries kept
// decisions is left for its own ending to settle — here the publish, which forgets exactly what it
// named — and the fresh catch-up names only what nobody claims.
func TestAReadyLeavesTheCatchUpThatIsPublishingWhatItNamed(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	ask := "dispatch://LEGION-1/ask/ask-1"
	now := time.Now()
	keepDecision(t, pool, records, "LEGION-1", ask, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	keptCatchUp(t, pool, records, "LEGION-1", 1, now)
	leased, payload := leaseCatchUp(t, pool, records, "LEGION-1", now)
	if payload.CatchUp.Kept == nil || !strings.Contains(payload.CatchUp.Decided, ask) {
		t.Fatalf("the leased catch-up = %+v, want it to name %s", payload.CatchUp, ask)
	}

	// The architect's next ready, while that catch-up is publishing: it drops the root's queued
	// catch-ups and writes a fresh one in the same transaction.
	publisher := &hookPublisher{hook: func() error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			stored, err := records.Issue(ctx, tx, "LEGION-1")
			if err != nil {
				return err
			}
			return workflow.QueueCatchUp(ctx, tx, records, config.DesignGateRootIssues, *stored, 2, now)
		})
	}}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
		supervisor: sup, project: "legion", now: func() time.Time { return now }}

	if err := runner.notice(ctx, leased, payload); err != nil {
		t.Fatalf("run the publishing catch-up: %v", err)
	}
	publisher.ranHook(t)

	var told int
	for _, published := range publisher.published() {
		if strings.Contains(published.payload.(record.Notice).CatchUp.Decided, ask) {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("the publish told %s %d times, want once: the row the architect has in its hands names it", ask, told)
	}
	if naming := catchUpsNaming(t, pool, "LEGION-1", leased.ID, ask); len(naming) != 1 || namingRef(naming) != 0 {
		t.Fatalf("the catch-ups left beside the publishing row name %s: %v, want the one fresh catch-up to name nothing of it: "+
			"a decision the architect has already been told is not told again", ask, naming)
	}
	if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != 0 {
		t.Fatalf("kept after the publish = %s, want none: the publish forgets exactly what it named", keptRefs(left))
	}
}

// A catch-up whose receipt the architect's session never confirmed, and which a newer catch-up
// superseded while it was publishing, is queued again by nothing (reholdNotice's superseded
// branch): its copy would arrive after the fresh catch-up. What it named is handed over there, as a
// superseded catch-up's own ending hands it over (supersededKept) — unclaimed, and named at once by
// a fresh catch-up while the root has an architect to tell. Left to the row's deletion, those
// decisions would wait unclaimed with nothing to name them while the architect worked the tree.
func TestAReceiptTimeoutOnASupersededCatchUpHandsOverWhatItNamed(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	ask := "dispatch://LEGION-1/ask/ask-2"
	now := time.Now()
	keepDecision(t, pool, records, "LEGION-1", ask, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	keptCatchUp(t, pool, records, "LEGION-1", 1, now)
	leased, payload := leaseCatchUp(t, pool, records, "LEGION-1", now)

	// The root is re-admitted while the catch-up is publishing, which is what supersedes it
	// (catchUpSuperseded reads the root's generation against the catch-up's), and the listener then
	// reports that its architect's session never confirmed the receipt.
	publisher := &hookPublisher{
		answer: fmt.Errorf("publish notice: listener returned 504 Gateway Timeout: %w",
			notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses-other"}),
		hook: func() error {
			return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				stored, err := records.Issue(ctx, tx, "LEGION-1")
				if err != nil {
					return err
				}
				stored.Generation = 2
				return records.PutIssue(ctx, tx, *stored)
			})
		},
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
		supervisor: sup, project: "legion", now: func() time.Time { return now }}

	if err := runner.notice(ctx, leased, payload); err != nil {
		t.Fatalf("run the catch-up whose receipt timed out: %v", err)
	}
	publisher.ranHook(t)
	// The lane finishes the row its run returned nothing for, which releases whatever it still
	// claims.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.FinishOutbox(ctx, tx, leased.ID, leased.LeaseToken)
	}); err != nil {
		t.Fatalf("finish the superseded catch-up: %v", err)
	}

	naming := catchUpsNaming(t, pool, "LEGION-1", leased.ID, ask)
	if named := namingRef(naming); named != 1 {
		t.Fatalf("the catch-ups left after the superseded receipt timeout name %s %d times: %v, want the hand-over's "+
			"fresh catch-up to name it once: a release never leaves a decision unclaimed while its root has an architect to tell",
			ask, named, naming)
	}
	if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != 1 || left[0].ClaimedBy == 0 {
		t.Fatalf("kept after the hand-over = %s, want the decision claimed by the fresh catch-up", keptRefs(left))
	}
}

// A catch-up whose last copy's receipt the architect's session never confirmed has spent its
// re-holds (noticeReholdDelays), so no copy carries its claims on. Its architect still holds the
// launch the catch-up was written for, so no ready of a new launch is coming to name what the row's
// deletion would release. What it named is handed to a fresh catch-up of that launch instead
// (spentKept): the decision waits in the outbox, claimed, and reaches the architect once it
// confirms a delivery. Left to the row's deletion, the decision would wait unclaimed with nothing
// to name it while the architect worked the tree.
func TestACatchUpWhoseReholdsAreSpentHandsOverWhatItNamed(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	ask := "dispatch://LEGION-1/ask/ask-2"
	now := time.Now()
	keepDecision(t, pool, records, "LEGION-1", ask, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	keptCatchUp(t, pool, records, "LEGION-1", 1, now)
	// The catch-up is the last copy the re-hold queues: it has been queued again the most times a
	// notice is.
	if _, err := pool.Exec(ctx, `update outbox set payload = jsonb_set(payload, '{resends}', to_jsonb($1::int))
		where payload->>'kind' = 'catch-up'`, len(noticeReholdDelays)); err != nil {
		t.Fatalf("make the catch-up its last copy: %v", err)
	}
	leased, payload := leaseCatchUp(t, pool, records, "LEGION-1", now)
	if payload.Resends != len(noticeReholdDelays) {
		t.Fatalf("the leased catch-up has been queued again %d times, want %d", payload.Resends, len(noticeReholdDelays))
	}

	publisher := &hookPublisher{answer: fmt.Errorf("publish notice: listener returned 504 Gateway Timeout: %w",
		notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses-architect"})}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
		supervisor: sup, project: "legion", now: func() time.Time { return now }}
	if err := runner.notice(ctx, leased, payload); err != nil {
		t.Fatalf("run the catch-up whose last receipt timed out: %v", err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.FinishOutbox(ctx, tx, leased.ID, leased.LeaseToken)
	}); err != nil {
		t.Fatalf("finish the spent catch-up: %v", err)
	}

	naming := catchUpsNaming(t, pool, "LEGION-1", leased.ID, ask)
	if named := namingRef(naming); named != 1 {
		t.Fatalf("the catch-ups left after the spent catch-up name %s %d times: %v, want a fresh catch-up of the architect's "+
			"launch to name it once: a decision whose catch-up gave up must not wait in the table with nothing to name it",
			ask, named, naming)
	}
	if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != 1 || left[0].ClaimedBy == 0 {
		t.Fatalf("kept after the spent catch-up = %s, want the decision claimed by the fresh catch-up", keptRefs(left))
	}
}

// An earlier listener reports the spent copy's failed forward after the row has finished: its
// deletion released what it claimed, and no row is left to hand over from (spentKept's claims of
// 0). What the tree is owed and nothing claims is named by a fresh catch-up while the root has an
// architect to hear it, and waits KEPT, with nothing written, while it has none — a lingering
// root's next start or ready names it.
func TestALateReportOfASpentCatchUpNamesWhatItsRowReleased(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lingers bool
		want    int
	}{
		{name: "the root hears", lingers: false, want: 1},
		{name: "the root lingers", lingers: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, tc.lingers)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			token := architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			ask := "dispatch://LEGION-1/ask/ask-2"
			now := time.Now()
			keepDecision(t, pool, records, "LEGION-1", ask, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
			keptCatchUp(t, pool, records, "LEGION-1", 1, now)
			leased, payload := leaseCatchUp(t, pool, records, "LEGION-1", now)
			// The row's last copy finished on the listener's 202, and its deletion released what it
			// claimed; the listener reports the failed forward only after that.
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return records.FinishOutbox(ctx, tx, leased.ID, leased.LeaseToken)
			}); err != nil {
				t.Fatalf("finish the spent catch-up: %v", err)
			}
			payload.Resends = len(noticeReholdDelays)
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
				notices: &hookPublisher{}, supervisor: sup, project: "legion", now: func() time.Time { return now }}
			reported := reportedNotice{issue: "LEGION-1", notice: payload, row: leased.ID, architect: token}
			if err := runner.reholdNotice(ctx, reported, notify.ReasonReceiptTimeout, "ses-architect"); err != nil {
				t.Fatalf("answer the late report of the spent copy: %v", err)
			}

			naming := catchUpsNaming(t, pool, "LEGION-1", leased.ID, ask)
			if named := namingRef(naming); named != tc.want {
				t.Fatalf("the catch-ups after the late report name %s %d times: %v, want %d", ask, named, naming, tc.want)
			}
			left := keptDecisions(t, pool, records, "LEGION-1")
			if len(left) != 1 {
				t.Fatalf("kept after the late report = %s, want the decision kept", keptRefs(left))
			}
			if claimed := left[0].ClaimedBy != 0; claimed != (tc.want == 1) {
				t.Fatalf("kept after the late report = %s, want it claimed only by a fresh catch-up of a root that hears", keptRefs(left))
			}
		})
	}
}
