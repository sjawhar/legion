package delivery

// The delivery package's test harness: its NATS server, its Postgres pool, and the helpers the
// intake's tests (intake_route_test.go, intake_consumer_test.go) and the reconcile's share.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

// deliveryTestPool opens a real Postgres pool from DISPATCH_TEST_DATABASE_URL, migrates it, and
// returns it; a test that needs it skips (not fails) when the variable is unset, matching every
// other Postgres-backed test in this module's convention.
func deliveryTestPool(t *testing.T) (*store.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres-backed delivery tests")
	}
	ctx := t.Context()
	pgxPool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pgxPool.Close)
	pool := store.NewPool(pgxPool)
	st := &store.Store{Pool: pool}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Every delivery table is truncated before the test, not after: a failed previous run's rows
	// (useful to inspect) are cleared by the next run that needs a clean slate, not hidden by one
	// that crashes before its own cleanup runs.
	for _, table := range []string{"delivery_run_jobs", "delivery_runs", "delivery_pull_requests", "delivery_reconcile_progress", "delivery_settings"} {
		if _, err := pool.Exec(ctx, "delete from "+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return pool, ctx
}

// seedDeliverySettings writes a delivery_settings row with acme/widgets-shaped placeholders.
func seedDeliverySettings(t *testing.T, ctx context.Context, pool *store.Pool) DeliverySettings {
	t.Helper()
	settings, err := PutSettings(ctx, pool, DeliverySettings{
		DeployRepo:           "acme/widgets",
		DeployWorkflowPath:   ".github/workflows/deploy.yml",
		ProductionJobName:    "widgets-release / widgets-release",
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors:    []string{"octocat"},
		ExcludedRepos:        []string{"acme/playground"},
	}, model.Actor{Kind: "system", ID: "delivery-test"})
	if err != nil {
		t.Fatalf("seed delivery_settings: %v", err)
	}
	return settings
}

// publishPullRequestEnvelope publishes one notifications.github.<owner>.<repo>.pr.<number>
// envelope (the shape intake.go's handlePullRequestEnvelope reads) and waits for the publish to
// be acknowledged by JetStream before returning.
func publishPullRequestEnvelope(t *testing.T, natsClient *bus.Client, owner, repo string, number int, fields map[string]string) {
	t.Helper()
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	envelope := contracts.Envelope{
		EventID:       fmt.Sprintf("test-%s-%s-%d-%d", owner, repo, number, time.Now().UnixNano()),
		Source:        "github",
		SourceEventID: fmt.Sprintf("%d", time.Now().UnixNano()),
		Topic:         contracts.GithubResourceSubject(owner, repo, "pr", fmt.Sprintf("%d", number)),
		Payload:       string(payload),
		TraceID:       "test-trace",
		IssuedAt:      time.Now().Unix(),
	}
	if err := natsClient.Publish(envelope); err != nil {
		t.Fatalf("publish envelope: %v", err)
	}
}

// publishGitHubEnvelope publishes one envelope on topic carrying payload as it stands, for a payload
// no string map can hold (publishPullRequestEnvelope encodes one).
func publishGitHubEnvelope(t *testing.T, natsClient *bus.Client, topic, payload string) {
	t.Helper()
	id := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	if err := natsClient.Publish(contracts.Envelope{
		EventID: id, Source: "github", SourceEventID: id, Topic: topic, Payload: payload,
		TraceID: "test-trace", IssuedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("publish %s: %v", topic, err)
	}
}

// awaitAcknowledged waits for the intake to have acknowledged every message the test's stream
// holds: the durable's ack floor at the stream's last sequence and nothing outstanding. deliver
// logs and writes before it acknowledges, so what a message caused is in place by then.
func awaitAcknowledged(t *testing.T, natsClient *bus.Client) {
	t.Helper()
	stream, err := natsClient.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	last := stream.State.LastSeq
	var info *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
		info, err = natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		return err == nil && info.AckFloor.Stream >= last && info.NumAckPending == 0
	}, func() string {
		return fmt.Sprintf("intake has not acknowledged through stream sequence %d: %+v, %v", last, info, err)
	})
}

// captureLogs sends every slog record from here on to a buffer, as JSON, until the test ends, and
// answers the records written so far. Intake logs from NATS's delivery goroutine, so the buffer is
// locked. Call it before startIntake, so the default logger is restored only once intake stops.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buffer lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
			var record map[string]any
			if line != "" && json.Unmarshal([]byte(line), &record) == nil {
				records = append(records, record)
			}
		}
		return records
	}
}

// lockedBuffer is a bytes.Buffer safe to write from one goroutine while another reads it.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// pathBaseForTest is the workflow file name a workflow subject carries, as intake derives it from
// the configured workflow path.
func pathBaseForTest(workflowPath string) string {
	if i := strings.LastIndex(workflowPath, "/"); i >= 0 {
		return workflowPath[i+1:]
	}
	return workflowPath
}

// shrinkIntakeFlowControl shrinks the intake's own timing so a test exercises its real flow
// control in test time. intakeMaxAckPending is deliberately left alone: it is the bound under
// test.
func shrinkIntakeFlowControl(t *testing.T) {
	t.Helper()
	messageTimeout, pollInterval := intakeMessageTimeout, settingsPollInterval
	intakeMessageTimeout, settingsPollInterval = 5*time.Second, time.Second
	t.Cleanup(func() {
		intakeMessageTimeout, settingsPollInterval = messageTimeout, pollInterval
	})
}

// publishWorkflowEnvelope publishes one workflow-run envelope built as Envoy's own GitHub
// normalizer builds it -- source "github", the delivery GUID as SourceEventID, and the dedupe
// key that pairs with it ("github.<guid>"), which is what earns the publish a JetStream MsgId
// (contracts.DedupeKeyNamesTheUpstreamEvent). Publishing through production's own path rather
// than an ad-hoc envelope is what makes a burst test say anything about production: an envelope
// carrying another dedupe key is stored under different rules. Reports JetStream's own duplicate
// verdict, so a caller can tell a message the stream already held from a new one.
func publishWorkflowEnvelope(t *testing.T, natsClient *bus.Client, owner, repo, workflowPath string, runID int64) bool {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "workflow", "action": "in_progress", "repo": owner + "/" + repo,
		"path": workflowPath, "run_id": strconv.FormatInt(runID, 10),
	})
	if err != nil {
		t.Fatalf("encode workflow payload: %v", err)
	}
	deliveryID := fmt.Sprintf("%s-%d", t.Name(), runID)
	envelope := contracts.Envelope{
		EventID:       deliveryID,
		Source:        "github",
		SourceEventID: deliveryID,
		DedupeKey:     "github." + deliveryID,
		Topic:         contracts.GithubWorkflowSubject(owner, repo, pathBaseForTest(workflowPath), "in_progress"),
		Payload:       string(payload),
		TraceID:       deliveryID,
		IssuedAt:      time.Now().Unix(),
	}
	if !contracts.DedupeKeyNamesTheUpstreamEvent(envelope) {
		t.Fatalf("the test envelope's dedupe key does not name its event, so it is not published the way production publishes")
	}
	duplicate, err := natsClient.PublishReportingDuplicate(envelope)
	if err != nil {
		t.Fatalf("publish workflow envelope: %v", err)
	}
	return duplicate
}

// startIntake runs intake until the test ends, and waits for Run to return before the test's own
// cleanups go on. Cancelling alone does not: Run reads settingsPollInterval and the intake
// flow-control vars, and the next test's shrinkIntakeFlowControl writes them, so a Run still
// unwinding from the previous test is a data race on them.
func startIntake(t *testing.T, intake *Intake) {
	t.Helper()
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// intakeTestClient connects one NATS client for an intake test and closes it with the test.
func intakeTestClient(t *testing.T) *bus.Client {
	t.Helper()
	natsClient, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(natsClient.Close)
	return natsClient
}

// awaitBoundDurable waits for the intake to have created its durable and subscribed to it, so a
// test that means to publish into a bound consumer does not race the bind.
func awaitBoundDurable(t *testing.T, natsClient *bus.Client) {
	t.Helper()
	var err error
	waitFor(t, 20*time.Second, func() bool {
		var info *natsgo.ConsumerInfo
		info, err = natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		return err == nil && info.PushBound && info.Config.MaxAckPending == intakeMaxAckPending
	}, func() string {
		return fmt.Sprintf("intake never bound its durable: %v", err)
	})
}

// awaitRuns waits for the intake to have stored want delivery_runs rows, and fails with the
// stream's message count and the consumer's whole state when it does not -- the numbers that say
// whether a shortfall is a consumer that stopped being given messages or messages that never
// reached the stream.
func awaitRuns(t *testing.T, ctx context.Context, pool *store.Pool, natsClient *bus.Client, want int, within time.Duration) {
	t.Helper()
	stored := 0
	waitFor(t, within, func() bool {
		stored = countRuns(t, ctx, pool)
		return stored >= want
	}, func() string {
		return fmt.Sprintf("delivery_runs rows = %d, want %d after %s\n%s", stored, want, within, intakeStateReport(t, natsClient))
	})
}

// waitFor polls done until it reports true, and fails the test once within has passed with the
// message failure builds at that moment -- so the message carries the state the wait gave up on,
// not the state it started from.
func waitFor(t *testing.T, within time.Duration, done func() bool, failure func() string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal(failure())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// intakeStateReport is the stream's message count per subject and the consumer's delivery state,
// one line each.
func intakeStateReport(t *testing.T, natsClient *bus.Client) string {
	t.Helper()
	report := ""
	if stream, err := natsClient.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: ">"}); err != nil {
		report += fmt.Sprintf("stream: %v\n", err)
	} else {
		report += fmt.Sprintf("stream: %d messages, by subject %v\n", stream.State.Msgs, stream.State.Subjects)
	}
	if info, err := natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName); err != nil {
		report += fmt.Sprintf("consumer: %v", err)
	} else {
		report += fmt.Sprintf("consumer: delivered=%d ack_floor=%d ack_pending=%d pending=%d redelivered=%d filter_subjects=%q",
			info.Delivered.Consumer, info.AckFloor.Consumer, info.NumAckPending, info.NumPending, info.NumRedelivered, info.Config.FilterSubjects)
	}
	report += "\n" + singleFilterProbe(t, natsClient)
	return report
}

// singleFilterProbe reports what a consumer carrying one filter subject sees of the same stream.
// Read beside the intake consumer's own numbers it separates the two explanations of a shortfall:
// a stream that does not hold the messages, or a consumer that holds them pending and does not
// hand them over.
func singleFilterProbe(t *testing.T, natsClient *bus.Client) string {
	t.Helper()
	subject := contracts.GithubWorkflowSubject("acme", "widgets", "deploy.yml", "in_progress")
	probe, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		FilterSubject: subject,
		AckPolicy:     natsgo.AckExplicitPolicy,
	})
	if err != nil {
		return fmt.Sprintf("single-filter probe on %q: %v", subject, err)
	}
	defer deleteProbeConsumer(t, natsClient, probe.Name)
	return fmt.Sprintf("single-filter probe on %q: pending=%d", subject, probe.NumPending)
}

// deleteProbeConsumer removes a consumer a test created only to probe the stream, and logs a
// delete that fails rather than failing the test over its own cleanup.
func deleteProbeConsumer(t *testing.T, natsClient *bus.Client, name string) {
	t.Helper()
	if err := natsClient.JS().DeleteConsumer(bus.Stream, name); err != nil {
		t.Logf("delete probe consumer %s: %v", name, err)
	}
}
