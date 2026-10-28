package intake_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	drainProject = "LEGION"
	drainRepo    = "acme/widgets"
	// drainRunning is the slotted roots the project runs, each with a live child, and the cap.
	drainRunning = 350
	// drainRecords is every issue record the project keeps; the ones beyond the running trees are
	// done, finished trees of a root and drainDoneChildren children each.
	drainRecords      = 10_000
	drainDoneChildren = 4
	// drainOthers is the events of pull requests and workflows that are not Legion's.
	drainOthers = 5_000
	// drainLegionIssues each get drainLegionSteps events: opened, two pushes, the checks of the
	// last head and an approval of it.
	drainLegionIssues = 10
	drainLegionSteps  = 5
	drainLegion       = drainLegionIssues * drainLegionSteps
	// drainRoundTrip is the network between the daemon and its Postgres.
	drainRoundTrip = 3 * time.Millisecond
	// drainBound is how long the backlog may take to drain when the drain is measured.
	drainBound = 60 * time.Second
)

func drainRoot(i int) string  { return fmt.Sprintf("LEGION-%d", 1000+i) }
func drainChild(i int) string { return fmt.Sprintf("LEGION-%d", 2000+i) }

// drainRepositories serves acme/widgets at main, as the daemon's mapping would.
type drainRepositories struct{}

func (drainRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if resolved == drainRepo {
		return workflow.Resolution{Repo: drainRepo, DefaultBranch: "main", GitHubRepo: drainRepo}, nil
	}
	return workflow.Resolution{Refusal: resolved + " is not mapped to project " + drainProject + " in Dispatch"}, nil
}

// transactions counts the transactions a pool's connections begin.
type transactions struct{ begun atomic.Int64 }

func (c *transactions) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(data.SQL)), "begin") {
		c.begun.Add(1)
	}
	return ctx
}

func (c *transactions) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// warnings keeps every log line at warn or above the consumer and the handlers write.
type warnings struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *warnings) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *warnings) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A repository Legion works in carries far more GitHub events than Legion's own pull requests:
// other pull requests opened and pushed to, their reviews and CI settlements, and every workflow
// its CI runs. Every event waits behind the ones before it on the GitHub consumer, so a backlog of
// other pull requests' events holds Legion's own back for as long as the consumer takes over them.
// Here a project runs 350 trees and keeps 10,000 records, its Postgres is a 3 ms round trip away,
// and 5,000 events that are not Legion's wait on the stream with 50 of Legion's among them. The
// daemon's consumer, with the handlers the daemon runs, drains them: every Legion event is applied
// and every other one is acknowledged with no transaction. With LEGION_TEST_DRAIN_MEASURE=1 the
// drain must also take under a minute from the first publish; the time is logged either way.
func TestABacklogOfOtherPullRequestsDrainsAndAppliesEveryLegionEvent(t *testing.T) {
	ctx := context.Background()
	dsn := testpg.Database(t, "legion_intake_drain_test")
	direct := drainPool(t, dsn)
	seedDrainProject(t, direct)

	counter := &transactions{}
	delayed, err := pgxpool.ParseConfig(testpg.Delayed(t, dsn, drainRoundTrip))
	if err != nil {
		t.Fatalf("parse the delayed database URL: %v", err)
	}
	delayed.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, delayed)
	if err != nil {
		t.Fatalf("open the pool through the delay: %v", err)
	}
	t.Cleanup(pool.Close)

	js := testnats.JetStream(t)
	stream := testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	logs := &warnings{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	repository := ghrepo.MustParse(drainRepo)
	deploys := []config.Deploy{{Repo: repository, Workflow: ".github/workflows/deploy.yaml", Step: "Apply"}}
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: drainProject, Repositories: drainRepositories{}, Deploys: deploys}, log)
	admission := admit.New(records, engine, drainRunning, drainProject, log)
	// GitHub answers that the approver can write to the repository; the count is every review the
	// consumer asked GitHub about.
	var asked atomic.Int64
	consumers, err := intake.OpenConsumers(ctx, js, pool, intake.ConsumerSpec{
		Project: drainProject, Repositories: []ghrepo.Repository{repository}, Deploys: deploys,
		AckWait: 30 * time.Second, NakDelay: time.Second, Logger: log,
		ReviewPermission: func(context.Context, intake.PullRequestReview) (bool, error) {
			asked.Add(1)
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	counter.begun.Store(0)

	backlog := drainBacklog()
	started := time.Now()
	var last uint64
	for _, event := range backlog {
		ack, err := js.Publish(ctx, event.subject, event.data)
		if err != nil {
			t.Fatalf("publish %s: %v", event.subject, err)
		}
		last = ack.Sequence
	}
	if waiting := githubWaiting(t, consumers); waiting != uint64(len(backlog)) {
		t.Fatalf("the GitHub consumer has %d messages waiting before it runs, want the backlog's %d", waiting, len(backlog))
	}

	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- consumers.Run(run, blockers.NewMirror(records, drainProject), engine, admission)
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	defer stop()

	deadline := time.Now().Add(10 * time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-5*time.Second).Before(deadline) {
		deadline = testDeadline.Add(-5 * time.Second)
	}
	for githubWaiting(t, consumers) != 0 {
		if begun := counter.begun.Load(); begun > drainLegion {
			t.Fatalf("%d transactions begun with %d messages still waiting, want one for each of the %d Legion events and none for any other",
				begun, githubWaiting(t, consumers), drainLegion)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the backlog of %d events is not drained after %s: %d still wait", len(backlog), time.Since(started).Round(time.Millisecond), githubWaiting(t, consumers))
		}
		time.Sleep(20 * time.Millisecond)
	}
	drained := time.Since(started)
	stop()
	t.Logf("a backlog of %d events, %d of them Legion's, drained in %s with Postgres %s away", len(backlog), drainLegion, drained.Round(time.Millisecond), drainRoundTrip)
	if os.Getenv("LEGION_TEST_DRAIN_MEASURE") == "1" && drained >= drainBound {
		t.Errorf("the backlog drained in %s, want under %s", drained.Round(time.Millisecond), drainBound)
	}

	// Every message was delivered once, so none was nacked or redelivered, and nothing was logged at
	// warn or above, so none was poison or failed.
	item, err := stream.Consumer(ctx, "legion-go-"+drainProject+"-github")
	if err != nil {
		t.Fatalf("read the GitHub consumer: %v", err)
	}
	info, err := item.Info(ctx)
	if err != nil {
		t.Fatalf("read the GitHub consumer's info: %v", err)
	}
	if info.Delivered.Consumer != uint64(len(backlog)) || info.AckFloor.Stream != last {
		t.Errorf("the GitHub consumer delivered %d messages and acknowledged through stream sequence %d, want each of the %d delivered once and every one through %d acknowledged",
			info.Delivered.Consumer, info.AckFloor.Stream, len(backlog), last)
	}
	if logged := logs.String(); logged != "" {
		t.Errorf("the consumer and the handlers logged, want nothing at warn or above:\n%s", logged)
	}

	// Only the Legion events took a transaction, and each was applied.
	if begun := counter.begun.Load(); begun != drainLegion {
		t.Errorf("%d transactions begun, want %d: one for each Legion event and none for any other", begun, drainLegion)
	}
	var want []string
	for _, event := range backlog {
		if event.legion {
			want = append(want, event.id)
		}
	}
	slices.Sort(want)
	if got := processedGitHubEvents(t, direct); !slices.Equal(got, want) {
		t.Errorf("processed events = %v, want exactly the %d Legion events %v", got, len(want), want)
	}
	if got := asked.Load(); got != drainLegionIssues {
		t.Errorf("GitHub was asked about %d reviewers, want %d: one for each Legion pull request's approval and none for any other pull request", got, drainLegionIssues)
	}
	assertDrainedPullRequests(t, direct, records)
}

// drainEvent is one message of the backlog: its subject and envelope, the id its fact is applied
// under (processed_events; intake's envoyEnvelope.factID: a webhook delivery's dedupe key, a
// settlement's event id), and whether it is a Legion event.
type drainEvent struct {
	subject string
	data    []byte
	id      string
	legion  bool
}

// drainBacklog is drainOthers events of pull requests and workflows that are not Legion's, one
// Legion event after every hundred of them. The others cycle through a pull request opened on a
// branch that is no issue's, a push to it, an approval and a CI settlement of a pull request the
// daemon does not record, and a run of a workflow no deploy names. Each Legion pull request is
// opened on its issue's branch, pushed to twice, settled green at its last head and approved there.
func drainBacklog() []drainEvent {
	events := make([]drainEvent, 0, drainOthers+drainLegion)
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for j := range drainOthers {
		number := 1000 + j
		switch j % 5 {
		case 0:
			events = append(events, drainPullRequestEvent(fmt.Sprintf("other-%d", j), number, "opened", fmt.Sprintf("feature/change-%d", number), fmt.Sprintf("other-%d-a", number), at))
		case 1:
			events = append(events, drainPullRequestEvent(fmt.Sprintf("other-%d", j), number-1, "synchronize", fmt.Sprintf("feature/change-%d", number-1), fmt.Sprintf("other-%d-b", number-1), at))
		case 2:
			events = append(events, drainReviewEvent(fmt.Sprintf("other-%d", j), number, int64(100_000+j), fmt.Sprintf("other-%d-a", number), at))
		case 3:
			events = append(events, drainChecksEvent(fmt.Sprintf("other-%d", j), number, fmt.Sprintf("other-%d-a", number), int64(200_000+j)))
		case 4:
			events = append(events, drainWorkflowRunEvent(fmt.Sprintf("other-%d", j), int64(300_000+j), at))
		}
		if j%100 != 99 {
			continue
		}
		k := j / 100
		i, step := k/drainLegionSteps, k%drainLegionSteps
		branch, number, id := "legion/"+drainChild(i), 100+i, fmt.Sprintf("legion-%d", k)
		updated := at.Add(time.Duration(k) * time.Minute)
		switch step {
		case 0:
			events = append(events, drainPullRequestEvent(id, number, "opened", branch, drainHead(i, 1), updated))
		case 1:
			events = append(events, drainPullRequestEvent(id, number, "synchronize", branch, drainHead(i, 2), updated))
		case 2:
			events = append(events, drainPullRequestEvent(id, number, "synchronize", branch, drainHead(i, 3), updated))
		case 3:
			events = append(events, drainChecksEvent(id, number, drainHead(i, 3), int64(400_000+i)))
		case 4:
			events = append(events, drainReviewEvent(id, number, drainReviewID(i), drainHead(i, 3), updated))
		}
		events[len(events)-1].legion = true
	}
	return events
}

func drainHead(i, push int) string { return fmt.Sprintf("%s-head-%d", drainChild(i), push) }
func drainReviewID(i int) int64    { return int64(7000 + i) }

const drainSubjectPrefix = "notifications.github.acme.widgets"

// drainWebhookEnvelope is the envelope Envoy's listener publishes for one GitHub webhook delivery
// (Envoy's contracts.GithubEnvelope): its payload is the normalizer's map of strings.
func drainWebhookEnvelope(delivery, subject, summary string, payload map[string]string) drainEvent {
	return drainEvent{subject: subject, id: "github." + delivery, data: drainJSON(map[string]any{
		"event_id": delivery + "-event", "source": "github", "source_event_id": delivery, "topic": subject,
		"dedupe_key": "github." + delivery, "issued_at": int64(1_790_124_795_391), "payload_summary": summary,
		"payload": string(drainJSON(payload)), "trace_id": delivery + "-trace",
	})}
}

func drainPullRequestEvent(delivery string, number int, action, branch, head string, updated time.Time) drainEvent {
	n := strconv.Itoa(number)
	return drainWebhookEnvelope(delivery, drainSubjectPrefix+".pr."+n, "pr "+action+": "+drainRepo+"#"+n, map[string]string{
		"kind": "pr", "action": action, "repo": drainRepo, "number": n, "title": "change " + n, "author": "someone",
		"url": "https://github.com/" + drainRepo + "/pull/" + n, "head_sha": head, "head_ref": branch, "base_ref": "main",
		"merged": "false", "merge_commit_sha": "", "merged_by": "", "updated_at": updated.Format(time.RFC3339), "body": "a change",
	})
}

func drainReviewEvent(delivery string, number int, id int64, head string, submitted time.Time) drainEvent {
	n := strconv.Itoa(number)
	return drainWebhookEnvelope(delivery, drainSubjectPrefix+".pr."+n+".review", "review approved on "+drainRepo+"#"+n, map[string]string{
		"kind": "review", "action": "submitted", "repo": drainRepo, "number": n, "title": "change " + n, "parent_kind": "pr",
		"author": "maintainer", "url": "https://github.com/" + drainRepo + "/pull/" + n + "#review", "state": "approved",
		"review_id": strconv.FormatInt(id, 10), "submitted_at": submitted.Format(time.RFC3339), "commit_id": head, "head_sha": head,
		"body": "looks good",
	})
}

func drainWorkflowRunEvent(delivery string, run int64, updated time.Time) drainEvent {
	id := strconv.FormatInt(run, 10)
	return drainWebhookEnvelope(delivery, drainSubjectPrefix+".workflow.ci_yaml.completed", "workflow CI completed on "+drainRepo, map[string]string{
		"kind": "workflow", "action": "completed", "repo": drainRepo, "workflow": "CI", "path": ".github/workflows/ci.yaml",
		"branch": "main", "head_branch": "main", "status": "completed", "conclusion": "success", "run_id": id, "run_attempt": "1",
		"head_sha": "main-" + id, "pr_numbers": "", "run_started_at": updated.Format(time.RFC3339), "updated_at": updated.Format(time.RFC3339),
		"url": "https://github.com/" + drainRepo + "/actions/runs/" + id,
	})
}

// drainChecksEvent is the settled pr.<n>.checks envelope Envoy's CI store publishes: one green
// check run at head.
func drainChecksEvent(id string, number int, head string, run int64) drainEvent {
	n := strconv.Itoa(number)
	subject := drainSubjectPrefix + ".pr." + n + ".checks"
	none := map[string]any{"count": 0, "checks": []string{}}
	return drainEvent{subject: subject, id: id + "-checks", data: drainJSON(map[string]any{
		"event_id": id + "-checks", "source": "github", "source_event_id": id + "-settlement", "topic": subject,
		"dedupe_key": "github.checks." + drainRepo + ".pr." + n + "." + head + ".g0", "issued_at": int64(1_790_124_840_596),
		"payload_summary": "checks settled on " + drainRepo + "#" + n + ": 1 passed", "trace_id": id + "-trace",
		"payload": string(drainJSON(map[string]any{
			"kind": "checks", "repo": drainRepo, "number": n, "sha": head, "check_runs": []map[string]any{{"name": "unit", "id": run}},
			"generation": 0, "snapshot": drainSnapshot(head), "settled_at": int64(1_790_124_840_596),
			"failed": none, "running": none, "queued": none, "cancelled": none, "skipped": none,
			"passed": map[string]any{"count": 1, "checks": []string{"unit"}}, "failing_checks": []any{},
		})),
	})}
}

func drainSnapshot(head string) string { return "settled-" + head }

func drainJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

// githubWaiting is the GitHub consumer's waiting messages, as the daemon's state reads them
// (Consumers.Lag).
func githubWaiting(t *testing.T, consumers *intake.Consumers) uint64 {
	t.Helper()
	for _, lag := range consumers.Lag(context.Background()) {
		if lag.Name != "legion-go-"+drainProject+"-github" {
			continue
		}
		if lag.Unavailable != "" {
			t.Fatalf("the GitHub consumer's lag is unavailable: %s", lag.Unavailable)
		}
		return lag.Waiting
	}
	t.Fatal("the consumers' lag names no GitHub consumer")
	return 0
}

// processedGitHubEvents is every processed event's id, sorted in Go's byte order as the test's
// expected list is: an SQL `order by` follows the database's collation, which can order
// punctuation differently (`legion-33-checks` before `legion-3-checks`).
func processedGitHubEvents(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select source || ':' || event_id from processed_events`)
	if err != nil {
		t.Fatalf("read the processed events: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the processed events: %v", err)
	}
	for i, id := range ids {
		source, eventID, _ := strings.Cut(id, ":")
		if source != "github" {
			t.Fatalf("processed event %s is from %s, want only GitHub's", eventID, source)
		}
		ids[i] = eventID
	}
	slices.Sort(ids)
	return ids
}

// assertDrainedPullRequests checks that each Legion pull request is recorded on its issue at its
// last head, settled green there and approved, and that no other pull request is recorded.
func assertDrainedPullRequests(t *testing.T, pool *pgxpool.Pool, records record.Store) {
	t.Helper()
	var recorded int
	if err := pool.QueryRow(context.Background(), `select count(*) from pull_requests`).Scan(&recorded); err != nil {
		t.Fatalf("count the recorded pull requests: %v", err)
	}
	if recorded != drainLegionIssues {
		t.Errorf("%d pull requests recorded, want the %d Legion ones", recorded, drainLegionIssues)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		for i := range drainLegionIssues {
			pr, err := records.PullRequestByNumber(context.Background(), tx, drainRepo, 100+i)
			if err != nil {
				return err
			}
			head := drainHead(i, 3)
			switch {
			case pr == nil:
				t.Errorf("%s#%d is not recorded, want %s's pull request", drainRepo, 100+i, drainChild(i))
			case pr.Issue != drainChild(i) || pr.HeadSHA != head || pr.CheckedHead != head || pr.Snapshot != drainSnapshot(head) || pr.ReviewSeen.ID != drainReviewID(i):
				t.Errorf("%s#%d is recorded on %s at head %s, settled at %s (%s), newest review %d; want %s at %s, settled there (%s), review %d",
					drainRepo, 100+i, pr.Issue, pr.HeadSHA, pr.CheckedHead, pr.Snapshot, pr.ReviewSeen.ID, drainChild(i), head, drainSnapshot(head), drainReviewID(i))
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the recorded pull requests: %v", err)
	}
}

// drainPool is the migrated database at dsn, reached directly: the seed and the assertions do not
// pay the delay the consumer does.
func drainPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	st, err := legionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate store: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedDrainProject copies in drainRunning slotted roots implementing, each with a child
// implementing, and done trees up to drainRecords records, every record with its Dispatch mirror
// row, in one transaction of bulk copies.
func seedDrainProject(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	admitted := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	var issues, mirror, slots [][]any
	add := func(key, tree string, parent *string, at phase.Phase, status string) {
		issues = append(issues, []any{key, tree, drainProject, key, parent, string(at), int64(1), status, status, "M", true, int64(1), drainRepo, "main", drainRepo})
		mirror = append(mirror, []any{key, drainProject, status, []string{}, int64(1)})
	}
	for i := range drainRunning {
		root := drainRoot(i)
		add(root, root, nil, phase.Implementing, "in_progress")
		add(drainChild(i), root, &root, phase.Implementing, "in_progress")
		slots = append(slots, []any{root, i, admitted})
	}
	for tree := 0; len(issues) < drainRecords; tree++ {
		root := fmt.Sprintf("LEGION-%d", 10_000+tree*(1+drainDoneChildren))
		add(root, root, nil, phase.Done, "done")
		for c := 1; c <= drainDoneChildren && len(issues) < drainRecords; c++ {
			add(fmt.Sprintf("LEGION-%d", 10_000+tree*(1+drainDoneChildren)+c), root, &root, phase.Done, "done")
		}
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		for _, table := range []struct {
			name    string
			columns []string
			rows    [][]any
		}{
			{"issues", []string{"key", "tree", "project", "title", "parent", "phase", "generation", "status", "dispatch_status", "rank", "handed_over",
				"last_dispatch_seq", "repo", "default_branch", "github_repo"}, issues},
			{"dispatch_issues", []string{"key", "project", "status", "blockers", "dispatch_seq"}, mirror},
			{"slots", []string{"issue", "index", "admitted_at"}, slots},
		} {
			if _, err := tx.CopyFrom(context.Background(), pgx.Identifier{table.name}, table.columns, pgx.CopyFromRows(table.rows)); err != nil {
				return fmt.Errorf("copy %d rows into %s: %w", len(table.rows), table.name, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
}
