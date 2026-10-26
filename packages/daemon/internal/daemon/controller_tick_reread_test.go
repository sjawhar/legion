package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The periodic turn re-reads from Dispatch exactly the issues something is waiting on, and
// nothing else: the unfinished blockers of the roots on the waiting line and of the children a
// blocker holds at phase.Admitted, each asked for once however many issues name it. A blocker the
// mirror already shows done is asked for nothing, and neither is an issue nothing waits on.
//
// This fake serves none of them, so every one of those reads answers not-found — an issue
// Dispatch no longer serves is no error, and the tick is applied all the same.
func TestThePeriodicTurnReReadsOnlyWhatSomethingWaitsOn(t *testing.T) {
	pool := isolatedOutboxPool(t)
	client := &tickDispatch{}
	w := tickRuntime(t, pool, client)

	seedWaitingRoot(t, pool, "LEGION-BLOCKED", "A")
	seedTickMirror(t, pool, "LEGION-BLOCKED", "todo", 1, "EXT-1")
	seedWaitingRoot(t, pool, "LEGION-FREE", "B")
	seedTickMirror(t, pool, "LEGION-FREE", "todo", 1)
	seedHeldChild(t, pool, "LEGION-CHILD", "LEGION-BLOCKED")
	seedTickMirror(t, pool, "LEGION-CHILD", "todo", 1, "EXT-1", "EXT-2", "EXT-DONE")
	seedTickMirror(t, pool, "EXT-DONE", "done", 1)

	w.applyControllerTick(context.Background(), time.Now())

	if asked, want := client.asked(), []string{"EXT-1", "EXT-2"}; !slices.Equal(asked, want) {
		t.Fatalf("the turn asked Dispatch for %v, want exactly %v", asked, want)
	}
	if listings := client.listings(); listings != 0 {
		t.Fatalf("the turn listed the project %d times, want none: two blockers are far under the bound", listings)
	}
	if processed := tickProcessed(t, pool); processed != 1 {
		t.Fatalf("processed ticks = %d, want the one tick applied: a read Dispatch answers not-found never costs the turn", processed)
	}
}

// Nothing waiting is almost every turn, and it reads nothing from Dispatch at all: the mirror
// alone answers, and the tick applied is the tick as it always was.
func TestThePeriodicTurnWithNothingWaitingReadsNothingFromDispatch(t *testing.T) {
	pool := isolatedOutboxPool(t)
	client := &tickDispatch{}
	w := tickRuntime(t, pool, client)

	seedWaitingRoot(t, pool, "LEGION-FREE", "A")
	seedTickMirror(t, pool, "LEGION-FREE", "todo", 1)

	w.applyControllerTick(context.Background(), time.Now())

	if asked, listings := client.asked(), client.listings(); len(asked) != 0 || listings != 0 {
		t.Fatalf("the turn asked Dispatch for %v and listed the project %d times, want neither", asked, listings)
	}
	if slotted := tickSlotted(t, pool); !slices.Equal(slotted, []string{"LEGION-FREE"}) {
		t.Fatalf("slots = %v, want the unblocked root admitted as ever", slotted)
	}
}

// The backstop the turn exists for: the event closing a blocker never arrived, so the mirror
// still shows it open and the root it holds has stood on the waiting line. The turn re-reads that
// blocker, the mirror handler writes what Dispatch answered ahead of the handlers that judge in
// the same transaction, and the root is admitted on this very tick.
func TestThePeriodicTurnAdmitsARootWhoseBlockerClosedWithNoEvent(t *testing.T) {
	pool := isolatedOutboxPool(t)
	client := &tickDispatch{issues: map[string]dispatch.Issue{
		"EXT-1": {Key: "EXT-1", Status: "done", LastSeq: 7},
	}}
	w := tickRuntime(t, pool, client)

	seedWaitingRoot(t, pool, "LEGION-BLOCKED", "A")
	seedTickMirror(t, pool, "LEGION-BLOCKED", "todo", 1, "EXT-1")
	seedTickMirror(t, pool, "EXT-1", "todo", 1)
	if slotted := tickSlotted(t, pool); len(slotted) != 0 {
		t.Fatalf("slots = %v before the turn, want the blocked root waiting", slotted)
	}

	w.applyControllerTick(context.Background(), time.Now())

	if slotted := tickSlotted(t, pool); !slices.Equal(slotted, []string{"LEGION-BLOCKED"}) {
		t.Fatalf("slots = %v, want the root admitted once the turn saw its blocker is done", slotted)
	}
	if row := tickMirrored(t, pool)["EXT-1"]; row.Status != "done" || row.Seq != 7 {
		t.Fatalf("the mirror holds EXT-1 at %q seq %d, want the turn's own reading (done, 7)", row.Status, row.Seq)
	}
}

// A failed read never skips the tick: the tick is also the controller's daily-report wake and the
// only wake a tree waiting on a refused root claim gets, so the turn logs the failure and applies
// the tick with what it has — here nothing, which leaves the blocked root exactly where it was.
func TestThePeriodicTurnWakesTheControllerWhenTheDispatchReadFails(t *testing.T) {
	pool := isolatedOutboxPool(t)
	registerTickController(t, pool)
	client := &tickDispatch{err: errors.New("dispatch is unreachable")}
	w := tickRuntime(t, pool, client)

	seedWaitingRoot(t, pool, "LEGION-BLOCKED", "A")
	seedTickMirror(t, pool, "LEGION-BLOCKED", "todo", 1, "EXT-1")

	w.applyControllerTick(context.Background(), time.Now())

	if notices := tickNotices(t, pool); notices != 1 {
		t.Fatalf("tick controller notices = %d after a failed read, want the one wake the tick always carries", notices)
	}
	if slotted := tickSlotted(t, pool); len(slotted) != 0 {
		t.Fatalf("slots = %v, want the root still waiting: a failed read tells the daemon nothing new", slotted)
	}
}

// The fan-out is bounded on both sides: up to blockerReadFanout distinct blocker keys are read
// one at a time, and above it the project is listed once instead — one request rather than the
// twenty-six single reads the waiting set would otherwise ask for.
func TestThePeriodicTurnListsTheProjectOnceAboveTheSingleReadBound(t *testing.T) {
	t.Run("at the bound, one read per key", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{}
		w := tickRuntime(t, pool, client)
		seedBlockedRoots(t, pool, blockerReadFanout)

		w.applyControllerTick(context.Background(), time.Now())

		if asked, listings := client.asked(), client.listings(); len(asked) != blockerReadFanout || listings != 0 {
			t.Fatalf("the turn made %d single reads and %d listings, want %d and none", len(asked), listings, blockerReadFanout)
		}
	})
	t.Run("above the bound, one listing", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{}
		for i := 1; i <= blockerReadFanout+1; i++ {
			client.summaries = append(client.summaries, dispatch.IssueSummary{Key: fmt.Sprintf("EXT-%d", i), Status: "done", LastSeq: 7})
		}
		w := tickRuntime(t, pool, client)
		seedBlockedRoots(t, pool, blockerReadFanout+1)

		w.applyControllerTick(context.Background(), time.Now())

		if asked, listings := client.asked(), client.listings(); len(asked) != 0 || listings != 1 {
			t.Fatalf("the turn made %d single reads and %d listings, want none and one", len(asked), listings)
		}
		mirrored := tickMirrored(t, pool)
		for i := 1; i <= blockerReadFanout+1; i++ {
			key := fmt.Sprintf("EXT-%d", i)
			if row := mirrored[key]; row.Status != "done" || row.Seq != 7 {
				t.Fatalf("the mirror holds %s at %q seq %d, want the listing's own reading (done, 7)", key, row.Status, row.Seq)
			}
		}
	})
}

// The turn's re-read carries the claim Dispatch shows on each issue it reads, by a single read
// and by the listing alike, and the mirror writes it.
func TestThePeriodicTurnsReReadMirrorsTheClaim(t *testing.T) {
	t.Run("a single read", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{issues: map[string]dispatch.Issue{
			"EXT-1": {Key: "EXT-1", Status: "in_progress", LastSeq: 7, Claim: &dispatch.Claim{Kind: "user", ID: "alice"}},
		}}
		w := tickRuntime(t, pool, client)
		seedWaitingRoot(t, pool, "LEGION-BLOCKED", "A")
		seedTickMirror(t, pool, "LEGION-BLOCKED", "todo", 1, "EXT-1")
		seedTickMirror(t, pool, "EXT-1", "todo", 1)

		w.applyControllerTick(context.Background(), time.Now())

		if row := tickMirrored(t, pool)["EXT-1"]; row.ClaimedBy != "user:alice" || row.ClaimSeq != 7 {
			t.Fatalf("the mirror holds EXT-1's claim as %q at %d, want the turn's reading (user:alice, 7)", row.ClaimedBy, row.ClaimSeq)
		}
	})
	t.Run("the listing", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{}
		for i := 1; i <= blockerReadFanout+1; i++ {
			client.summaries = append(client.summaries, dispatch.IssueSummary{Key: fmt.Sprintf("EXT-%d", i), Status: "in_progress", LastSeq: 7, ClaimedBy: "user:alice"})
		}
		w := tickRuntime(t, pool, client)
		seedBlockedRoots(t, pool, blockerReadFanout+1)

		w.applyControllerTick(context.Background(), time.Now())

		if row := tickMirrored(t, pool)["EXT-1"]; row.ClaimedBy != "user:alice" || row.ClaimSeq != 7 {
			t.Fatalf("the mirror holds EXT-1's claim as %q at %d, want the listing's reading (user:alice, 7)", row.ClaimedBy, row.ClaimSeq)
		}
	})
}

// tickDispatch is Dispatch as the periodic turn reads it: it counts and records what the turn
// asked for, answers each key from its own table — not-found for a key it does not hold, as
// Dispatch answers for an issue it no longer serves — and fails every read while err is set.
// Nothing else of the client is reached by a turn.
type tickDispatch struct {
	mu        sync.Mutex
	issues    map[string]dispatch.Issue
	summaries []dispatch.IssueSummary
	keys      []string
	lists     int
	err       error
}

func (d *tickDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys = append(d.keys, key)
	if d.err != nil {
		return dispatch.Issue{}, d.err
	}
	issue, served := d.issues[key]
	if !served {
		return dispatch.Issue{}, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no such issue"}
	}
	return issue, nil
}

func (d *tickDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lists++
	if d.err != nil {
		return nil, d.err
	}
	return d.summaries, nil
}

// asked is the keys the turn read one at a time, in the order it asked for them.
func (d *tickDispatch) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.keys)
}

func (d *tickDispatch) listings() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lists
}

func (d *tickDispatch) SetStatus(context.Context, string, string) error   { return nil }
func (d *tickDispatch) PostMessage(context.Context, string, string) error { return nil }
func (d *tickDispatch) HandOver(context.Context, dispatch.Issue, string) error {
	return nil
}
func (d *tickDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *tickDispatch) EventsAfter(context.Context, string, int64) ([]json.RawMessage, error) {
	return nil, nil
}
func (d *tickDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *tickDispatch) BuildCommit(context.Context) (string, error) { return "", nil }
func (d *tickDispatch) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}

// tickRecords is the record store the boot gives the runtime (openWorkflow): this project's
// issues alone, with no outbox wake, since no outbox runs in a tick test.
func tickRecords() record.Store {
	return projectRecords{Store: record.NewStore(), project: "CAPTURE"}
}

// tickRuntime is a runtime whose turn reads client, with the production handler chain — the
// mirror first, then the two handlers that judge from its rows — and one admission slot.
func tickRuntime(t *testing.T, pool *pgxpool.Pool, client dispatch.Client) *workflowRuntime {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	records := tickRecords()
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories}, log)
	admission := admit.New(records, engine, 1, "CAPTURE", log)
	return &workflowRuntime{
		pool: pool, records: records, admission: admission,
		handlers:        []intake.Handler{blockers.NewMirror(records, "CAPTURE"), engine, admission},
		dispatch:        client,
		dispatchProject: "CAPTURE", bootID: "test-boot", log: log,
	}
}

// seedWaitingRoot records a handed-over root of this project in todo, where it waits for a slot
// (record.Waiting).
func seedWaitingRoot(t *testing.T, pool *pgxpool.Pool, key, rank string) {
	t.Helper()
	seedTickIssue(t, pool, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "CAPTURE",
		Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: rank,
		HandedOver: true, LastDispatchSeq: 1,
	})
}

// seedHeldChild records a child of tree where a blocker holds it: admitted into its tree and
// started by nothing until the last issue it waits on is done.
func seedHeldChild(t *testing.T, pool *pgxpool.Pool, key, tree string) {
	t.Helper()
	parent := tree
	seedTickIssue(t, pool, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "CAPTURE",
		Title: key, Tree: tree, Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo",
		Rank: "A", LastDispatchSeq: 1,
	})
}

// seedBlockedRoots records count roots, each waiting on a distinct blocker of its own that the
// mirror has never seen a row for, so the waiting set holds exactly count keys.
func seedBlockedRoots(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	for i := 1; i <= count; i++ {
		key := fmt.Sprintf("LEGION-%d", i)
		seedWaitingRoot(t, pool, key, fmt.Sprintf("A%03d", i))
		seedTickMirror(t, pool, key, "todo", 1, fmt.Sprintf("EXT-%d", i))
	}
}

func seedTickIssue(t *testing.T, pool *pgxpool.Pool, issue record.Issue) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, issue)
	}); err != nil {
		t.Fatalf("seed the record of %s: %v", issue.Key, err)
	}
}

func seedTickMirror(t *testing.T, pool *pgxpool.Pool, key, status string, seq int64, blocked ...string) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchIssueRow{
			Key: key, Project: "CAPTURE", Status: status, Blockers: blocked, Seq: seq})
	}); err != nil {
		t.Fatalf("seed the mirror row of %s: %v", key, err)
	}
}

// registerTickController registers a controller for this project, the daemon's own path: `legion
// controller start` mints the capability and its session registers with it, both under the
// project token (api/controller.go). Without one, a tick's wake is queued for nobody.
func registerTickController(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	token, err := claim.ProjectToken("CAPTURE")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := st.MintController(context.Background(), token, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller capability: %v", err)
	}
	if ok, err := st.RegisterController(context.Background(), token, generation, "ses-controller", []byte("secret"), time.Now(), 0); err != nil || !ok {
		t.Fatalf("register the controller = %t, %v", ok, err)
	}
}

func tickSlotted(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var slots []record.Slot
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		slots, err = record.NewStore().Slots(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatalf("read the admission slots: %v", err)
	}
	keys := make([]string, 0, len(slots))
	for _, slot := range slots {
		keys = append(keys, slot.Issue)
	}
	slices.Sort(keys)
	return keys
}

func tickMirrored(t *testing.T, pool *pgxpool.Pool) map[string]record.DispatchIssueRow {
	t.Helper()
	var mirrored []record.DispatchIssueRow
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		mirrored, err = record.NewStore().DispatchIssues(context.Background(), tx, "CAPTURE")
		return err
	}); err != nil {
		t.Fatalf("read the mirror: %v", err)
	}
	rows := make(map[string]record.DispatchIssueRow, len(mirrored))
	for _, row := range mirrored {
		rows[row.Key] = row
	}
	return rows
}

func tickProcessed(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var processed int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from processed_events where event_id like 'controller-tick:CAPTURE:test-boot:%'`).Scan(&processed); err != nil {
		t.Fatalf("read the processed ticks: %v", err)
	}
	return processed
}

func tickNotices(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var notices int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1 and payload->>'kind' = 'tick'`,
		string(record.OutboxKindControllerNotice)).Scan(&notices); err != nil {
		t.Fatalf("read the tick notices: %v", err)
	}
	return notices
}
