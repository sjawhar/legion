package intake

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// factLocks counts the statements a pool's connections send that take the fact lock
// (SerializeFacts), which every transaction ApplyFact opens takes first.
type factLocks struct{ taken atomic.Int64 }

func (l *factLocks) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "pg_advisory_xact_lock") {
		l.taken.Add(1)
	}
	return ctx
}

func (l *factLocks) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A mapped repository carries far more GitHub events than Legion's own pull requests: other pull
// requests opened and pushed to, their reviews and CI settlements, and every workflow its CI runs.
// None of them can change a record, so each is acknowledged without a transaction, the fact lock
// or a GitHub read: a pull request event on a branch that is no issue's, a review or a checks
// settlement of a pull request the daemon does not record, and a run of a workflow no deploy names.
// Once the daemon records the pull request, the same review and settlement are applied under the
// lock, the review's author's permission read first, and so are a pull request event on an issue's
// branch and a deploy's run.
func TestAnEventTheDaemonCannotActOnIsAcknowledgedWithNoTransaction(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	locks := &factLocks{}
	traced := pool.Config().Copy()
	traced.ConnConfig.Tracer = locks
	counted, err := pgxpool.NewWithConfig(context.Background(), traced)
	if err != nil {
		t.Fatalf("open the counted pool: %v", err)
	}
	t.Cleanup(counted.Close)

	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	spec.Deploys = []config.Deploy{{Repo: ghrepo.MustParse("sjawhar/legion"), Workflow: ".github/workflows/deploy.yaml", Step: "Apply"}}
	var mu sync.Mutex
	var asked []string
	spec.ReviewPermission = func(_ context.Context, review PullRequestReview) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, review.Author)
		return true, nil
	}
	stop := startConsume(t, js, spec, counted, writeHandler("applied", nil))
	defer stop()

	onOtherBranch := func(name string) []byte {
		t.Helper()
		data := capturedGitHubEnvelope(t, name)
		other := bytes.ReplaceAll(data, []byte("legion/LEGION-208"), []byte("dependabot/go_modules/x"))
		if bytes.Equal(other, data) {
			t.Fatalf("%s names no issue branch to replace", name)
		}
		return other
	}
	run := func(eventID, path string) []byte {
		return envelopeJSON(t, eventID, "github", `{"kind":"workflow","repo":"sjawhar/legion","action":"completed","run_id":"40203304600",`+
			`"run_attempt":"1","path":"`+path+`","head_branch":"main","status":"completed","conclusion":"success"}`)
	}
	const (
		prSubject     = "notifications.github.sjawhar.legion.pr.42"
		reviewSubject = "notifications.github.sjawhar.legion.pr.42.review"
		checksSubject = "notifications.github.sjawhar.legion.pr.42.checks"
		runSubject    = "notifications.github.sjawhar.legion.workflow.ci_yaml.completed"
	)
	for _, event := range []struct {
		name, subject string
		body          []byte
	}{
		{"a pull request opened on a branch that is no issue's", prSubject, onOtherBranch("pr-opened.json")},
		{"a push to such a pull request", prSubject, onOtherBranch("pr-synchronized.json")},
		{"a review of a pull request the daemon does not record", reviewSubject, capturedGitHubEnvelope(t, "review.json")},
		{"a checks settlement of such a pull request", checksSubject, capturedGitHubEnvelope(t, "checks.json")},
		{"a run of a workflow no deploy names", runSubject, run("ci-run", ".github/workflows/ci.yaml")},
	} {
		waitAcknowledged(t, stream, githubConsumerName(spec.Project), publishAt(t, js, event.subject, event.body))
		if taken := locks.taken.Load(); taken != 0 {
			t.Fatalf("%s took the fact lock %d times, want never", event.name, taken)
		}
		if processed, writes := processedEvents(t, pool), writeCount(t, pool); processed != 0 || writes != 0 {
			t.Fatalf("%s left %d processed events and %d handler writes, want none: no transaction was to open", event.name, processed, writes)
		}
	}
	mu.Lock()
	if len(asked) != 0 {
		t.Fatalf("the review's author's permission was read for %v, want no GitHub read for a pull request the daemon does not record", asked)
	}
	mu.Unlock()

	recordPullRequest(t, pool, "LEGION-208", 42)
	for i, event := range []struct {
		name, subject string
		body          []byte
	}{
		{"the review of the recorded pull request", reviewSubject, capturedGitHubEnvelope(t, "review.json")},
		{"its checks settlement", checksSubject, capturedGitHubEnvelope(t, "checks.json")},
		{"a pull request opened on an issue's branch", prSubject, capturedGitHubEnvelope(t, "pr-opened.json")},
		{"a run of the deploy's workflow", "notifications.github.sjawhar.legion.workflow.deploy_yaml.completed", run("deploy-run", ".github/workflows/deploy.yaml")},
	} {
		waitAcknowledged(t, stream, githubConsumerName(spec.Project), publishAt(t, js, event.subject, event.body))
		if taken := locks.taken.Load(); taken != int64(i+1) {
			t.Fatalf("after %s the fact lock was taken %d times, want %d: once for each event a record takes", event.name, taken, i+1)
		}
		if processed, writes := processedEvents(t, pool), writeCount(t, pool); processed != i+1 || writes != i+1 {
			t.Fatalf("after %s, %d processed events and %d handler writes, want %d of each", event.name, processed, writes, i+1)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "reviewer" {
		t.Fatalf("the permission was read for %v, want once, for the recorded pull request's reviewer", asked)
	}
}

// publishAt publishes data on subject and returns its stream sequence.
func publishAt(t *testing.T, js jetstream.JetStream, subject string, data []byte) uint64 {
	t.Helper()
	ack, err := js.Publish(context.Background(), subject, data)
	if err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	return ack.Sequence
}

// waitAcknowledged waits until consumer has acknowledged every message up to the stream sequence
// last, with nothing pending or waiting for its acknowledgement: a nak'd message would still be
// waiting, so this is every message handled for good.
func waitAcknowledged(t *testing.T, stream jetstream.Stream, consumer string, last uint64) {
	t.Helper()
	testwait.Eventually(t, consumer+" acknowledged every message", func() bool {
		item, err := stream.Consumer(context.Background(), consumer)
		if err != nil {
			return false
		}
		info, err := item.Info(context.Background())
		return err == nil && info.AckFloor.Stream >= last && info.NumAckPending == 0 && info.NumPending == 0
	})
}

func processedEvents(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `select count(*) from processed_events`).Scan(&count); err != nil {
		t.Fatalf("count processed events: %v", err)
	}
	return count
}

// recordPullRequest records key, on sjawhar/legion, with its open pull request number on its
// issue branch, as the workflow records one from its opened event.
func recordPullRequest(t *testing.T, pool *pgxpool.Pool, key string, number int) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Tree: key, Project: "LEGION", Title: key, Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", Rank: "U", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion"}); err != nil {
			return err
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: "sjawhar/legion", Number: number, Branch: "legion/" + key,
			HeadSHA: "head-captured", State: record.PullRequestOpen, Generation: 1})
	}); err != nil {
		t.Fatalf("record %s's pull request #%d: %v", key, number, err)
	}
}
