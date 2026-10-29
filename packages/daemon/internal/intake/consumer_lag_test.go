package intake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A consumer the daemon is not running counts every message published to it as waiting, the
// oldest at the first one's stream time; it is stalled once 60 seconds pass with messages waiting
// and none handled since it opened, and not once it acknowledges one, until 60 more seconds pass
// with none handled — a message it keeps naking is not progress. A poison message it terminates is
// handled.
func TestConsumerLagCountsWhatWaitsAndCallsAConsumerStalledWhenNoneIsHandledFor60Seconds(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	clock := &lagClock{now: time.Now()}
	spec := consumerSpec(&lockedBuffer{})
	spec.Now = clock.Now
	github, dispatch := githubConsumerName(spec.Project), dispatchConsumerName(spec.Project)
	consumers, err := OpenConsumers(ctx, js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	var stored []time.Time
	for number := 1; number <= 3; number++ {
		stored = append(stored, publishStored(t, js, stream, fmt.Sprintf("notifications.github.sjawhar.legion.pr.%d", number), pullRequestOpenedEnvelope(t, number)))
	}

	lags := consumers.Lag(ctx)
	if names := lagNames(lags); !slices.Equal(names, []string{dispatch, dispatchServerConsumerName(spec.Project), github}) {
		t.Fatalf("Lag names %q, want the Dispatch, Dispatch-server and GitHub consumers", names)
	}
	for _, lag := range lags[:2] {
		if lag.Waiting != 0 || !lag.OldestAt.IsZero() || lag.Stalled || lag.Unavailable != "" {
			t.Errorf("%s = %+v, want nothing waiting and nothing unavailable", lag.Name, lag)
		}
	}
	got := lagNamed(t, lags, github)
	if got.Waiting != 3 || !got.OldestAt.Equal(stored[0]) || got.OldestAge != clock.Now().Sub(stored[0]) || got.Stalled || !got.LastHandledAt.IsZero() || got.Unavailable != "" {
		t.Fatalf("GitHub consumer before any is handled = %+v, want 3 waiting, the oldest stored at %s and %s old, not stalled, none handled",
			got, stored[0], clock.Now().Sub(stored[0]))
	}

	clock.advance(consumerStallAfter - time.Second)
	if got := lagNamed(t, consumers.Lag(ctx), github); got.Stalled {
		t.Fatalf("GitHub consumer 59 seconds after it opened = %+v, want not stalled yet", got)
	}
	clock.advance(2 * time.Second)
	if got := lagNamed(t, consumers.Lag(ctx), github); !got.Stalled || got.Waiting != 3 {
		t.Fatalf("GitHub consumer 61 seconds after it opened with none handled = %+v, want 3 waiting and stalled", got)
	}

	// pr-2 fails every time it is applied, so it is naked again and again and keeps waiting.
	var naks atomic.Int32
	stop := runConsumers(t, consumers, handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		opened, ok := fact.(PullRequestOpened)
		if !ok {
			return Result{}, fmt.Errorf("unexpected fact %T", fact)
		}
		if opened.Number == 2 {
			naks.Add(1)
			return Result{}, errors.New("pr-2 cannot be applied")
		}
		_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, fmt.Sprintf("pr-%d", opened.Number))
		return Result{}, err
	}))
	defer stop()
	handledAt := clock.Now()
	testwait.Eventually(t, "pr-1 and pr-3 acknowledged and pr-2 naked twice", func() bool {
		return slices.Equal(writeHandlers(t, pool), []string{"pr-1", "pr-3"}) && naks.Load() >= 2 && lagNamed(t, consumers.Lag(ctx), github).Waiting == 1
	})
	got = lagNamed(t, consumers.Lag(ctx), github)
	if got.Waiting != 1 || !got.OldestAt.Equal(stored[1]) || !got.LastHandledAt.Equal(handledAt) || got.Stalled || got.Unavailable != "" {
		t.Fatalf("GitHub consumer after two acknowledgements = %+v, want pr-2 waiting, oldest at its %s, last handled at %s, not stalled", got, stored[1], handledAt)
	}

	clock.advance(consumerStallAfter + time.Second)
	nakedBefore := naks.Load()
	testwait.Eventually(t, "pr-2 naked again after the clock moved", func() bool { return naks.Load() > nakedBefore })
	if got := lagNamed(t, consumers.Lag(ctx), github); !got.Stalled || got.Waiting != 1 || !got.LastHandledAt.Equal(handledAt) {
		t.Fatalf("GitHub consumer 61 seconds after its last acknowledgement, naking since = %+v, want stalled, last handled at %s", got, handledAt)
	}

	publish(t, js, "notifications.dispatch.issue.CAPTURE-208.issue.updated", []byte(`{`))
	testwait.Eventually(t, "the poison message terminated and counted as handled", func() bool {
		lag := lagNamed(t, consumers.Lag(ctx), dispatch)
		return lag.Waiting == 0 && lag.LastHandledAt.Equal(clock.Now())
	})
}

// What NATS does not answer, or refuses, is named in the consumer's unavailable, and Lag still
// returns every consumer: a CONSUMER.INFO that fails leaves waiting and stalled unknown; a
// STREAM.MSG.GET that fails or never answers leaves only the oldest unknown, the never-answered
// read cut at its own bound, before the consumer's.
func TestConsumerLagNamesWhatNATSDidNotAnswer(t *testing.T) {
	ctx := context.Background()
	served, stream := testJetStream(t)
	standIn := &lagStandIn{JetStream: served}
	clock := &lagClock{now: time.Now()}
	spec := consumerSpec(&lockedBuffer{})
	spec.Now = clock.Now
	github := githubConsumerName(spec.Project)
	consumers, err := OpenConsumers(ctx, standIn, migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	publishStored(t, served, stream, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	clock.advance(consumerStallAfter + time.Second)

	t.Run("the consumer's info refused", func(t *testing.T) {
		refused := errors.New("nats: permissions violation")
		standIn.info, standIn.getMsg = refused, nil
		defer func() { standIn.info = nil }()
		lags := consumers.Lag(ctx)
		if len(lags) != 3 {
			t.Fatalf("Lag = %+v, want all three consumers", lags)
		}
		for _, lag := range lags {
			if lag.Waiting != 0 || lag.Stalled || !lag.OldestAt.IsZero() || lag.Unavailable != "read the consumer's info: "+refused.Error() {
				t.Errorf("%s = %+v, want nothing known and the refusal named", lag.Name, lag)
			}
		}
	})

	t.Run("the oldest waiting message refused", func(t *testing.T) {
		refused := errors.New("nats: permissions violation")
		standIn.getMsg = func(context.Context) (*jetstream.RawStreamMsg, error) { return nil, refused }
		lags := consumers.Lag(ctx)
		got := lagNamed(t, lags, github)
		if got.Waiting != 1 || !got.Stalled || !got.OldestAt.IsZero() || got.Unavailable != "read the oldest waiting message: "+refused.Error() {
			t.Fatalf("GitHub consumer = %+v, want 1 waiting and stalled kept, the oldest unknown and the refusal named", got)
		}
		if dispatch := lagNamed(t, lags, dispatchConsumerName(spec.Project)); dispatch.Unavailable != "" {
			t.Errorf("Dispatch consumer, nothing waiting = %+v, want no message read and nothing unavailable", dispatch)
		}
	})

	t.Run("the oldest waiting message never answered", func(t *testing.T) {
		standIn.getMsg = func(ctx context.Context) (*jetstream.RawStreamMsg, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		started := time.Now()
		lags := consumers.Lag(ctx)
		if took := time.Since(started); took < consumerOldestBound || took >= consumerLagBound {
			t.Fatalf("Lag took %s against a read that never answers, want it cut at the oldest message's %s bound, before the consumer's %s", took, consumerOldestBound, consumerLagBound)
		}
		got := lagNamed(t, lags, github)
		if got.Waiting != 1 || !got.Stalled || !got.OldestAt.IsZero() || got.Unavailable != "read the oldest waiting message: "+context.DeadlineExceeded.Error() {
			t.Fatalf("GitHub consumer = %+v, want 1 waiting and stalled kept, the oldest unknown and the deadline named", got)
		}
	})
}

// A NATS server refuses a publish its user lacks with an asynchronous -ERR alone and never answers
// the request, so a stored-message get (STREAM.MSG.GET, which a deployment's daemon user lacks
// until its envoy-infra release grants it) waits out its bound. The three consumers are read at the
// same time, each within a bound of its own: the Dispatch and GitHub consumers each have messages
// waiting, so each one's get waits consumerOldestBound out inside its own consumerLagBound while
// the other's info answers all the same, and the server consumer has nothing waiting and reads no
// message. Each consumer whose oldest message could not be read keeps its waiting count and
// stalled flag and names that read, and Lag as a whole stays within one consumerLagBound.
func TestConsumerLagCountsEveryConsumerWhileNATSRefusesTheOldestMessageRead(t *testing.T) {
	ctx := context.Background()
	daemonSeed, daemonPublic := testnats.User(t)
	adminSeed, adminPublic := testnats.User(t)
	_, uri := testnats.StartNkeyAuthorizedUsers(t, testnats.Image,
		testnats.NkeyUser{Public: daemonPublic, Permissions: `{ publish: { deny: ["$JS.API.STREAM.MSG.GET.>"] } }`},
		testnats.NkeyUser{Public: adminPublic})
	admin := jetStreamAs(t, uri, adminSeed)
	if _, err := admin.CreateStream(ctx, jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create the notification stream: %v", err)
	}
	clock := &lagClock{now: time.Now()}
	spec := consumerSpec(&lockedBuffer{})
	spec.Now = clock.Now
	consumers, err := OpenConsumers(ctx, jetStreamAs(t, uri, daemonSeed), migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers as the daemon's user: %v", err)
	}
	publish(t, admin, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	for number := 1; number <= 3; number++ {
		publish(t, admin, fmt.Sprintf("notifications.github.sjawhar.legion.pr.%d", number), pullRequestOpenedEnvelope(t, number))
	}
	clock.advance(consumerStallAfter + time.Second)

	started := time.Now()
	lags := consumers.Lag(ctx)
	if took := time.Since(started); took > consumerLagBound+time.Second {
		t.Fatalf("Lag took %s, want it within its %s bound", took, consumerLagBound)
	}
	refused := "read the oldest waiting message: " + context.DeadlineExceeded.Error()
	want := []ConsumerLag{
		{Name: dispatchConsumerName(spec.Project), Waiting: 1, Stalled: true, Unavailable: refused},
		{Name: dispatchServerConsumerName(spec.Project)},
		{Name: githubConsumerName(spec.Project), Waiting: 3, Stalled: true, Unavailable: refused},
	}
	if !slices.Equal(lags, want) {
		t.Fatalf("Lag with the stored-message get refused =\n%+v\nwant\n%+v", lags, want)
	}
}

// jetStreamAs is a JetStream client of the server at uri, connected as the nkey user seed is the
// seed of, closed when t ends.
func jetStreamAs(t *testing.T, uri, seed string) jetstream.JetStream {
	t.Helper()
	conn, err := natsauth.Connect([]string{uri}, seed)
	if err != nil {
		t.Fatalf("connect to NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// lagClock is a clock a test moves by hand (ConsumerSpec.Now).
type lagClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lagClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lagClock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

// lagStandIn is the test's NATS server whose notification stream refuses or never answers what a
// test says: every CONSUMER.INFO fails with info when it is set, and every STREAM.MSG.GET is
// getMsg's answer when it is set; everything else is the real server's. The test sets both only
// while no consumer runs, so nothing reads them concurrently.
type lagStandIn struct {
	jetstream.JetStream
	info   error
	getMsg func(context.Context) (*jetstream.RawStreamMsg, error)
}

func (s *lagStandIn) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	stream, err := s.JetStream.Stream(ctx, name)
	if err != nil {
		return nil, err
	}
	return lagStandInStream{Stream: stream, standIn: s}, nil
}

type lagStandInStream struct {
	jetstream.Stream
	standIn *lagStandIn
}

func (s lagStandInStream) Consumer(ctx context.Context, name string) (jetstream.Consumer, error) {
	if s.standIn.info != nil {
		return nil, s.standIn.info
	}
	return s.Stream.Consumer(ctx, name)
}

func (s lagStandInStream) GetMsg(ctx context.Context, seq uint64, opts ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error) {
	if s.standIn.getMsg != nil {
		return s.standIn.getMsg(ctx)
	}
	return s.Stream.GetMsg(ctx, seq, opts...)
}

// publishStored publishes data on subject and is the time the stream stored it at.
func publishStored(t *testing.T, js jetstream.JetStream, stream jetstream.Stream, subject string, data []byte) time.Time {
	t.Helper()
	ack, err := js.Publish(context.Background(), subject, data)
	if err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	message, err := stream.GetMsg(context.Background(), ack.Sequence)
	if err != nil {
		t.Fatalf("read message %d back: %v", ack.Sequence, err)
	}
	return message.Time
}

func lagNames(lags []ConsumerLag) []string {
	names := make([]string, len(lags))
	for i, lag := range lags {
		names[i] = lag.Name
	}
	return names
}

func lagNamed(t *testing.T, lags []ConsumerLag, name string) ConsumerLag {
	t.Helper()
	for _, lag := range lags {
		if lag.Name == name {
			return lag
		}
	}
	t.Fatalf("Lag = %+v names no consumer %s", lags, name)
	return ConsumerLag{}
}
