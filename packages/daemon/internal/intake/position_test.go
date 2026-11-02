package intake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A Dispatch consumer recreated after messages already landed on the stream — deleted and
// reopened, or a first boot against a stream that already carries history — starts at
// DeliverNewPolicy: it will never receive those older messages, so its ack floor can never reach
// a target the listing read against them. DispatchPosition's idle, not the ack floor alone, is
// what tells Reconcile nothing more is ever coming for a record those messages already advanced
// past.
func TestDispatchPositionIsIdleForARecreatedConsumerBehindPreExistingMessages(t *testing.T) {
	js, _ := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.comment.created", envelopeJSON(t, "dispatch-comment", "dispatch",
		`{"id":2,"issue_key":"CAPTURE-3","seq":9,"notify":true,"type":"comment.created","payload":{"body":"hi"}}`))

	consumers, err := OpenConsumers(context.Background(), js, migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	target, err := consumers.DispatchTarget(context.Background())
	if err != nil {
		t.Fatalf("DispatchTarget: %v", err)
	}
	if target < 2 {
		t.Fatalf("target = %d, want at least 2 (the two messages already published)", target)
	}
	position, err := consumers.DispatchPosition(context.Background())
	if err != nil {
		t.Fatalf("DispatchPosition: %v", err)
	}
	if !position.Idle {
		t.Fatalf("idle = false for a freshly (re)created consumer with nothing it can ever deliver from before it existed, want true")
	}
	if position.AckFloorStream >= target {
		t.Fatalf("ack_floor = %d, target = %d; want ack_floor short of target so idle is what closes the gap, not the floor", position.AckFloorStream, target)
	}
}

// A GitHub consumer that is found missing — deleted, or lost with the NATS server's state — while
// events were published is created at the last message it applied, so every event published while
// it was missing is applied, and none twice: the one at its position, already applied, is
// delivered again and dropped as a duplicate. Both ways a consumer is created do it: the boot's
// OpenConsumers, and a mapping refresh's SetRepositories mid-run.
func TestAMissingGitHubConsumerStartsAtItsLastAppliedPosition(t *testing.T) {
	for _, reopen := range []struct {
		name string
		open func(t *testing.T, js jetstream.JetStream, pool *pgxpool.Pool, spec ConsumerSpec, missing func()) func()
	}{
		{"OpenConsumers", func(t *testing.T, js jetstream.JetStream, pool *pgxpool.Pool, spec ConsumerSpec, missing func()) func() {
			missing()
			return startConsume(t, js, spec, pool, factMarks())
		}},
		{"SetRepositories", func(t *testing.T, js jetstream.JetStream, pool *pgxpool.Pool, spec ConsumerSpec, missing func()) func() {
			consumers, err := OpenConsumers(context.Background(), js, pool, spec)
			if err != nil {
				t.Fatalf("OpenConsumers: %v", err)
			}
			missing()
			if err := consumers.SetRepositories(context.Background(), capturedRepositories); err != nil {
				t.Fatalf("SetRepositories: %v", err)
			}
			return runConsumers(t, consumers, factMarks())
		}},
	} {
		t.Run(reopen.name, func(t *testing.T) {
			pool := migratedPool(t)
			createWrites(t, pool)
			js, stream := testJetStream(t)
			logs := &lockedBuffer{}
			spec := consumerSpec(logs)
			github := githubConsumerName(spec.Project)

			stop := startConsume(t, js, spec, pool, factMarks())
			publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
			waitDelivered(t, stream, github)
			testwait.Eventually(t, "the first event applied", func() bool { return slices.Equal(writeHandlers(t, pool), []string{"pr-1"}) })
			stop()

			defer reopen.open(t, js, pool, spec, func() {
				if err := stream.DeleteConsumer(context.Background(), github); err != nil {
					t.Fatalf("delete the GitHub consumer: %v", err)
				}
				for number := 2; number <= 4; number++ {
					publish(t, js, fmt.Sprintf("notifications.github.sjawhar.legion.pr.%d", number), pullRequestOpenedEnvelope(t, number))
				}
			})()
			waitDelivered(t, stream, github)
			want := []string{"pr-1", "pr-2", "pr-3", "pr-4"}
			testwait.Eventually(t, "the events published while the consumer was missing applied", func() bool {
				return len(writeHandlers(t, pool)) >= len(want)
			})
			if got := writeHandlers(t, pool); !slices.Equal(got, want) {
				t.Fatalf("applied %q, want each event applied once, %q", got, want)
			}
			if line := logLine(logs.String(), "level=INFO", "created at the last message it applied", "consumer="+github); line == "" {
				t.Errorf("no INFO line names the consumer created at its position; logs:\n%s", logs)
			}
			if line := logLine(logs.String(), "level=ERROR", "consumer="+github); line != "" {
				t.Errorf("a consumer whose position the stream still holds logged an error: %s", line)
			}
			info, err := stream.Info(context.Background())
			if err != nil {
				t.Fatalf("stream info: %v", err)
			}
			if seq := storedStreamSeq(t, pool, github); seq != int64(info.State.LastSeq) {
				t.Errorf("stored stream_seq = %d, want the last applied message's, %d", seq, info.State.LastSeq)
			}
		})
	}
}

// The Dispatch-server consumer has no listing to catch it up, so a Dispatch start published while it
// was missing is delivered from its position too, and the start it applied last is not applied
// again.
func TestAMissingDispatchServerConsumerStartsAtItsLastAppliedPosition(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	server := dispatchServerConsumerName(spec.Project)

	stop := startConsume(t, js, spec, pool, factMarks())
	publish(t, js, serverStartedSubject, serverStartedEnvelope(t, 1))
	waitDelivered(t, stream, server)
	testwait.Eventually(t, "the first start applied", func() bool { return slices.Equal(writeHandlers(t, pool), []string{serverCommit(1)}) })
	stop()

	if err := stream.DeleteConsumer(context.Background(), server); err != nil {
		t.Fatalf("delete the Dispatch server consumer: %v", err)
	}
	publish(t, js, serverStartedSubject, serverStartedEnvelope(t, 2))
	publish(t, js, serverStartedSubject, serverStartedEnvelope(t, 3))
	defer startConsume(t, js, spec, pool, factMarks())()
	waitDelivered(t, stream, server)
	want := []string{serverCommit(1), serverCommit(2), serverCommit(3)}
	testwait.Eventually(t, "the starts published while the consumer was missing applied", func() bool {
		return len(writeHandlers(t, pool)) >= len(want)
	})
	if got := writeHandlers(t, pool); !slices.Equal(got, want) {
		t.Fatalf("applied %q, want each start applied once, %q", got, want)
	}
	if line := logLine(logs.String(), "level=INFO", "created at the last message it applied", "consumer="+server); line == "" {
		t.Errorf("no INFO line names the consumer created at its position; logs:\n%s", logs)
	}
}

// A consumer that has applied nothing — here the GitHub consumer, whose only message was a push to
// a tag, which is no fact of either kind and is acknowledged without a transaction, recording no
// position — is created at the next message when it is
// found missing, as a consumer created for the first time is: what was published before it is
// not replayed.
func TestAMissingConsumerWithNoStoredPositionStartsAtTheNextMessage(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	github := githubConsumerName(spec.Project)

	stop := startConsume(t, js, spec, pool, factMarks())
	tag := strings.ReplaceAll(string(capturedGitHubEnvelope(t, "push.json")), "refs/heads/legion/LEGION-208", "refs/tags/v1.0.0")
	publish(t, js, "notifications.github.sjawhar.legion.push.tag.v1.0.0", []byte(tag))
	waitDelivered(t, stream, github)
	stop()
	var positions int
	if err := pool.QueryRow(context.Background(), `select count(*) from consumer_positions`).Scan(&positions); err != nil {
		t.Fatalf("count consumer positions: %v", err)
	}
	if positions != 0 {
		t.Fatalf("%d positions stored for a message acknowledged without a transaction, want none", positions)
	}

	if err := stream.DeleteConsumer(context.Background(), github); err != nil {
		t.Fatalf("delete the GitHub consumer: %v", err)
	}
	publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	defer startConsume(t, js, spec, pool, factMarks())()
	if pending := consumerInfo(t, stream, github).NumPending; pending != 0 {
		t.Fatalf("the recreated consumer has %d messages pending, want none of what was published before it", pending)
	}
	publish(t, js, "notifications.github.sjawhar.legion.pr.2", pullRequestOpenedEnvelope(t, 2))
	testwait.Eventually(t, "the next event applied", func() bool { return len(writeHandlers(t, pool)) >= 1 })
	waitDelivered(t, stream, github)
	if got := writeHandlers(t, pool); !slices.Equal(got, []string{"pr-2"}) {
		t.Fatalf("applied %q, want only the event published after the consumer was created", got)
	}
	if line := logLine(logs.String(), "created at the last message it applied"); line != "" {
		t.Errorf("a consumer with no position logged a start at one: %s", line)
	}
}

// A position older than the stream's first message — the consumer was missing longer than the
// stream keeps messages (72 hours), the stream was recreated, or the project was quiet, since only
// an applied fact moves the position — starts the consumer at the stream's first message, and a
// WARN line names the gap: the events published between may have been missed, the daemon applied
// none of them, and the daemon's own read of each issue's pull request heals those it would have
// acted on.
func TestAStoredPositionOlderThanTheStreamWarnsOfEventsThatMayHaveBeenMissed(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	github := githubConsumerName(spec.Project)
	if _, err := pool.Exec(context.Background(), `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 9000, $2)`,
		github, time.Now().Add(-100*time.Hour)); err != nil {
		t.Fatalf("store the old position: %v", err)
	}
	publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))

	defer startConsume(t, js, spec, pool, factMarks())()
	waitDelivered(t, stream, github)
	testwait.Eventually(t, "the stream's first event applied", func() bool { return slices.Equal(writeHandlers(t, pool), []string{"pr-1"}) })
	if line := logLine(logs.String(), "level=WARN", "consumer="+github, "older than the stream's first message", "may have been missed"); line == "" {
		t.Fatalf("no WARN line names the events that may have been missed before the stream's first message; logs:\n%s", logs)
	}
}

// factMarks is a handler that writes each fact it applies as a mark of its own: a pull request's
// opened as pr-<number>, a Dispatch start as its commit.
func factMarks() Handler {
	return handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		var mark string
		switch fact := fact.(type) {
		case PullRequestOpened:
			mark = fmt.Sprintf("pr-%d", fact.Number)
		case ServerStarted:
			mark = fact.Commit
		default:
			return Result{}, fmt.Errorf("unexpected fact %T", fact)
		}
		_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, mark)
		return Result{}, err
	})
}

// pullRequestOpenedEnvelope is the opened of sjawhar/legion#number on LEGION-208's branch, an event
// of its own.
func pullRequestOpenedEnvelope(t *testing.T, number int) []byte {
	t.Helper()
	return envelopeJSON(t, fmt.Sprintf("pr-opened-%d", number), "github", fmt.Sprintf(`{"action":"opened","author":"author","base_ref":"main",`+
		`"body":"Dispatch: LEGION-208","head_ref":"legion/LEGION-208","head_sha":"head-%d","kind":"pr","merged":"false","number":"%d",`+
		`"repo":"sjawhar/legion","title":"a pull request","updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/sjawhar/legion/pull/%d"}`,
		number, number, number))
}

func serverCommit(n int) string {
	return strings.Repeat(fmt.Sprint(n), 40)
}

func serverStartedEnvelope(t *testing.T, n int) []byte {
	t.Helper()
	return envelopeJSON(t, fmt.Sprintf("dispatch-server-started-%d", n), "dispatch",
		fmt.Sprintf(`{"commit":%q,"started_at":"2026-10-19T06:00:00Z"}`, serverCommit(n)))
}

// waitDelivered waits until consumer has delivered and had acknowledged every message it has.
func waitDelivered(t *testing.T, stream jetstream.Stream, consumer string) {
	t.Helper()
	testwait.Eventually(t, consumer+" has every message delivered and acknowledged", func() bool {
		item, err := stream.Consumer(context.Background(), consumer)
		if err != nil {
			return false
		}
		info, err := item.Info(context.Background())
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	})
}

func storedStreamSeq(t *testing.T, pool *pgxpool.Pool, consumer string) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(), `select stream_seq from consumer_positions where consumer = $1`, consumer).Scan(&seq); err != nil {
		t.Fatalf("read the stored position of %s: %v", consumer, err)
	}
	return seq
}

// logLine is the first line of logs holding every one of parts, or "" when none does.
func logLine(logs string, parts ...string) string {
	for line := range strings.SplitSeq(logs, "\n") {
		if !slices.ContainsFunc(parts, func(part string) bool { return !strings.Contains(line, part) }) {
			return line
		}
	}
	return ""
}
