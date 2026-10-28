package delivery

// How the intake runs its messages: one key at a time per pull request or run, keys at once, the
// subjects it does not want acknowledged before any decode, and what it gives back when it stops
// delivering.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// heldPullRequests is a fake GitHub pull request read that records each fetch's start and end by
// number and holds the first fetch of pull request 1 until release. Every fetch answers 500, which
// the intake logs and acknowledges.
type heldPullRequests struct {
	mu      sync.Mutex
	events  []string
	entered chan struct{}
	enter   sync.Once
	held    chan struct{}
	release func()
}

func holdPullRequestOne(fake *fakeGitHub) *heldPullRequests {
	held := &heldPullRequests{entered: make(chan struct{}), held: make(chan struct{})}
	held.release = sync.OnceFunc(func() { close(held.held) })
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number := r.PathValue("number")
		held.record("start " + number)
		if number == "1" {
			first := false
			held.enter.Do(func() {
				first = true
				close(held.entered)
			})
			if first {
				<-held.held
			}
		}
		held.record("end " + number)
		w.WriteHeader(http.StatusInternalServerError)
	})
	return held
}

func (h *heldPullRequests) record(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

// of is the recorded events of pull request number, in order.
func (h *heldPullRequests) of(number string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var events []string
	for _, event := range h.events {
		if event == "start "+number || event == "end "+number {
			events = append(events, event)
		}
	}
	return events
}

func (h *heldPullRequests) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the intake never fetched pull request 1")
	}
}

// mergedPullRequest is the envelope fields of a merged pull request of acme/widgets by octocat,
// which the intake fetches from GitHub.
func mergedPullRequest(number int) map[string]string {
	return map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
		"number": strconv.Itoa(number), "author": "octocat",
	}
}

func deliveryConsumerInfo(t *testing.T, natsClient *bus.Client) *natsgo.ConsumerInfo {
	t.Helper()
	info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the delivery consumer: %v", err)
	}
	return info
}

// runIntake runs intake until the returned stop is called or the test ends, and stop waits for Run
// to return.
func runIntake(t *testing.T, intake *Intake) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(ctx)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return stop
}

// While GitHub holds the fetch of pull request 1, a message of pull request 2 is handled and
// acknowledged, and a second message of pull request 1 waits behind the first: it starts only
// once the first has ended.
func TestAPullRequestHeldOnGitHubHoldsBackOnlyItsOwnMessages(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	// After the fake's server, so it runs first: the server's Close waits for the fetch it holds.
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 2, mergedPullRequest(2))

	var info *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return slices.Equal(held.of("2"), []string{"start 2", "end 2"}) &&
			info.Delivered.Consumer-before == 3 && info.NumAckPending == 2
	}, func() string {
		return fmt.Sprintf("pull request 2 was not handled and acknowledged while pull request 1 was held: events %q, consumer %+v", held.of("2"), info)
	})
	if events := held.of("1"); !slices.Equal(events, []string{"start 1"}) {
		t.Fatalf("pull request 1's events while its first fetch is held = %q, want its first start alone", events)
	}

	held.release()
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: its second message must start after its first ends", events, want)
	}
}

// Every subject but a pull request's own and a workflow run's is acknowledged on its subject alone,
// with no decode: a comment and a review on a pull request, its checks, an issue and a job event.
// The pull request's own message is decoded, once.
func TestSubjectsTheIntakeDoesNotWantAreAcknowledgedWithoutADecode(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	var decodes atomic.Int64
	decode := decodeEnvelope
	decodeEnvelope = func(data []byte) (contracts.Envelope, error) {
		decodes.Add(1)
		return decode(data)
	}
	// Before startIntake, so it is restored only once the intake has stopped.
	t.Cleanup(func() { decodeEnvelope = decode })
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)

	payload := `{"kind":"comment","action":"created","repo":"acme/widgets","number":"7","author":"octocat"}`
	for _, kind := range []string{"pr.7.comment", "pr.7.review", "pr.7.checks", "issue.3", "job.500.completed"} {
		publishGitHubEnvelope(t, natsClient, contracts.GithubSubject("acme", "widgets", kind), payload)
	}
	awaitAcknowledged(t, natsClient)
	if got := decodes.Load(); got != 0 {
		t.Fatalf("decoded %d envelopes of subjects the intake does not want, want none", got)
	}

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	awaitAcknowledged(t, natsClient)
	if got := decodes.Load(); got != 1 {
		t.Fatalf("decoded %d envelopes for one pull request message, want 1", got)
	}
}

// A message waiting behind its key when the intake stops is negatively acknowledged, so the next
// process to bind the durable is handed it at once rather than after the ack wait; the message in
// its handler then finishes and is acknowledged by the stopping intake.
func TestAMessageQueuedBehindItsKeyAtStopIsRedeliveredAtOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	stopFirst := runIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	var info *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return info.Delivered.Consumer-before == 2 && info.NumAckPending == 2
	}, func() string {
		return fmt.Sprintf("the second message of pull request 1 was not delivered behind the first: %+v", info)
	})

	// Run stops delivering as its context ends, and returns once the held fetch has ended.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopFirst()
	}()
	waitFor(t, 20*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake kept its subscription"
	})
	held.release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once its handler ended")
	}
	if events := held.of("1"); !slices.Equal(events, []string{"start 1", "end 1"}) {
		t.Fatalf("the stopping intake's fetches of pull request 1 = %q, want the held one alone", events)
	}

	// The ack wait is minutes; the second process is handed the message within seconds.
	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: the next process handles the message given back", events, want)
	}
}

// holdPullRequestOneUntilAbandoned is holdPullRequestOne, except that the held fetch also ends when
// the intake abandons it (the request's context ends), so its end is recorded as soon as the
// intake stops waiting on it, by a release or by giving the call up.
func holdPullRequestOneUntilAbandoned(fake *fakeGitHub) *heldPullRequests {
	held := &heldPullRequests{entered: make(chan struct{}), held: make(chan struct{})}
	held.release = sync.OnceFunc(func() { close(held.held) })
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number := r.PathValue("number")
		held.record("start " + number)
		if number == "1" {
			first := false
			held.enter.Do(func() {
				first = true
				close(held.entered)
			})
			if first {
				select {
				case <-held.held:
				case <-r.Context().Done():
				}
			}
		}
		held.record("end " + number)
		w.WriteHeader(http.StatusInternalServerError)
	})
	return held
}

// A process standing by for the durable when the intake holding it stops does not start a pull
// request's next message while the stopping intake is still handling the one before it: the order
// of one key's messages holds across the hand-over, as it does inside one intake. The message the
// stopping intake gives back reaches the standby only once its handler of that key has ended or
// given up its GitHub call.
func TestAHandOverKeepsAPullRequestsMessagesInOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	first := newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	stopFirst := runIntake(t, first)
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	var info *natsgo.ConsumerInfo
	waitFor(t, 20*time.Second, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return info.Delivered.Consumer-before == 2 && info.NumAckPending == 2
	}, func() string {
		return fmt.Sprintf("the second message of pull request 1 was not delivered behind the first: %+v", info)
	})

	// The standby is running before the stop, as the second envoy-dispatch process of a rolling
	// deploy is.
	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopFirst()
	}()
	waitFor(t, 20*time.Second, func() bool {
		first.mu.Lock()
		defer first.mu.Unlock()
		return first.delivering != nil && first.delivering.stopping.Load()
	}, func() string { return "the first intake never began stopping" })

	// overlapped reports whether a fetch of pull request 1 started while an earlier one had not
	// yet ended.
	overlapped := func(events []string) bool {
		open := false
		for _, event := range events {
			switch event {
			case "start 1":
				if open {
					return true
				}
				open = true
			case "end 1":
				open = false
			}
		}
		return false
	}
	// The standby polls for the durable every durableBindInterval, so several of them give it the
	// time to bind and be handed whatever the stopping intake gave back.
	for deadline := time.Now().Add(4 * durableBindInterval); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if events := held.of("1"); overlapped(events) {
			held.release()
			t.Fatalf("pull request 1's events across the hand-over = %q: the standby started the pull request's next message "+
				"while the stopping intake's handler of the one before it was still waiting on GitHub", events)
		}
	}

	held.release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once its handler ended")
	}
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q", events, want)
	}
}

// A stopping intake does not wait out a GitHub call that does not answer. Dispatch's whole
// shutdown has cmd/dispatch's stopGrace, 30 seconds (the pod's terminationGracePeriodSeconds less
// its pre-stop wait), and the intake's stop starts at the signal, so it returns well inside that
// however long GitHub holds the handler's call: here GitHub never answers.
func TestAStoppingIntakeDoesNotWaitOutAGitHubCallThatDoesNotAnswer(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	stop := runIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		held.release()
		t.Fatal("the stopping intake was still waiting on its handler's GitHub call 20 s after its context ended; Dispatch's shutdown has 30 s in all")
	}
}

// bindReturnsAfterSignal is how long after Run's ctx ends TestABindThatReturnsAfterTheSignalStopsByTheBudgetFromTheSignal
// holds the bind that has subscribed.
const bindReturnsAfterSignal = 2 * time.Second

// A bind that subscribed before Run's ctx ended and returns after it -- a standby's bind poll under
// way at the signal, whose JetStream calls do not watch ctx -- is stopped under the one stop budget,
// which runs from ctx's end: the handler of the message the subscription was handed is given up
// intakeStopBudget after the signal, however late the bind returned, and its message handed back,
// so the next process to bind the durable redoes it at once. With the budget begun at the bind's
// return, a late bind pushed the whole stop -- and cmd/dispatch's deregister and pool close behind
// it -- that much later.
func TestABindThatReturnsAfterTheSignalStopsByTheBudgetFromTheSignal(t *testing.T) {
	budget := intakeStopBudget
	intakeStopBudget = 3 * time.Second
	t.Cleanup(func() { intakeStopBudget = budget })
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	subscribed, bindReturns := make(chan struct{}), make(chan struct{})
	intake.boundHook = func() {
		close(subscribed)
		<-bindReturns
	}
	stop := runIntake(t, intake)
	// After runIntake's cleanup, so it runs first: Run waits for the bind it holds.
	letBindReturn := sync.OnceFunc(func() { close(bindReturns) })
	t.Cleanup(letBindReturn)
	select {
	case <-subscribed:
	case <-time.After(20 * time.Second):
		t.Fatal("the intake never subscribed to its durable")
	}
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)

	signalled := time.Now()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	time.Sleep(bindReturnsAfterSignal)
	letBindReturn()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return 20 s after its context ended")
	}
	if waited, within := time.Since(signalled), intakeStopBudget+time.Second; waited > within {
		t.Fatalf("Run returned %s after its ctx ended, its bind %s after it; want within %s: the stop budget ran from the bind's return, not from ctx's end",
			waited.Round(time.Millisecond), bindReturnsAfterSignal, within)
	}
	// The fake records the end once it sees the request cancelled, a moment after the client did.
	waitFor(t, 5*time.Second, func() bool { return slices.Equal(held.of("1"), []string{"start 1", "end 1"}) }, func() string {
		return fmt.Sprintf("pull request 1's events once Run returned = %q, want the held fetch given up", held.of("1"))
	})

	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("1"), []string{"start 1", "end 1", "start 1", "end 1"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 1's events = %q, want %q: the next process redoes the message given up", events, want)
	}
}

// A message NATS has delivered into the client's buffer, but the subscription's callback has not
// yet routed when the intake stops, is negatively acknowledged rather than handled, and the next
// process to bind the durable handles it.
func TestAMessageInTheClientBufferAtStopIsGivenBack(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	// The first decode, the workflow run's in the callback, waits until unblock: the messages
	// behind it stay in the client's buffer meanwhile.
	blocked, unblock := make(chan struct{}), make(chan struct{})
	var first sync.Once
	decode := decodeEnvelope
	decodeEnvelope = func(data []byte) (contracts.Envelope, error) {
		hold := false
		first.Do(func() { hold = true })
		if hold {
			close(blocked)
			<-unblock
		}
		return decode(data)
	}
	t.Cleanup(func() { decodeEnvelope = decode })
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	natsClient := intakeTestClient(t)
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	stopFirst := runIntake(t, intake)
	release := sync.OnceFunc(func() { close(unblock) })
	t.Cleanup(release)
	awaitBoundDurable(t, natsClient)

	// Another repository's run: whichever way the stop finds it, nothing is fetched for it.
	publishWorkflowEnvelope(t, natsClient, "other-org", "other-repo", ".github/workflows/deploy.yml", 9001)
	select {
	case <-blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("the intake's callback never decoded the workflow run's envelope")
	}
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 9, mergedPullRequest(9))
	intake.mu.Lock()
	delivery := intake.delivering
	intake.mu.Unlock()
	waitFor(t, 20*time.Second, func() bool {
		buffered, _, err := delivery.sub.Pending()
		return err == nil && buffered >= 2
	}, func() string { return "the pull request's message never reached the client's buffer" })

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stopFirst()
	}()
	waitFor(t, 20*time.Second, delivery.stopping.Load, func() string { return "the intake never began stopping" })
	release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once its callback was free")
	}
	if events := held.of("9"); len(events) != 0 {
		t.Fatalf("the stopping intake fetched the buffered pull request: %q", events)
	}

	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("9"), []string{"start 9", "end 9"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 9's events = %q, want %q: the next process handles the message given back", events, want)
	}
}

// subscribe at a deliver subject it is given binds the durable created with that subject without
// looking it up, as a process taking the durable over after recreating it does; StopDelivering
// ends that subscription, and a second call returns at once.
func TestAnIntakeSubscribesAtTheDeliverSubjectItIsGiven(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsClient := intakeTestClient(t)
	wanted := deliveryConsumerConfig()
	// Starting at new messages, so a durable recreated on the package's shared stream is handed
	// only this test's.
	wanted.DeliverPolicy = natsgo.DeliverNewPolicy
	if err := natsClient.JS().DeleteConsumer(bus.Stream, wanted.Durable); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
		t.Fatalf("delete the delivery durable: %v", err)
	}
	if _, err := natsClient.JS().AddConsumer(bus.Stream, &wanted); err != nil {
		t.Fatalf("create the delivery durable: %v", err)
	}
	intake := newIntake(natsClient, pool, nil, newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	if _, err := intake.subscribe(wanted.DeliverSubject); err != nil {
		t.Fatalf("subscribe at %s: %v", wanted.DeliverSubject, err)
	}
	t.Cleanup(func() { intake.StopDelivering(context.Background()) })

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	awaitAcknowledged(t, natsClient)
	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastEventAt == nil {
		t.Fatalf("settings after the handled message = %+v, %v: want last_event_at recorded", settings, err)
	}

	intake.StopDelivering(t.Context())
	intake.StopDelivering(t.Context())
	waitFor(t, 5*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the durable kept its subscriber after StopDelivering returned"
	})
}

// A stopped intake stays stopped across a reconnect of its client: the reconnect does not bind its
// stopped subscription to the durable again, where its callback would hand every message straight
// back. A subscribe afterwards on the same client, at the deliver subject of the durable created
// again, as a process that lost and regained the durable does, binds it and handles its messages.
func TestAStoppedIntakeStaysStoppedAcrossAReconnectAndSubscribesAgain(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsClient := intakeTestClient(t)
	hooked := make(chan struct{}, 8)
	natsClient.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})
	recreate := func() natsgo.ConsumerConfig {
		t.Helper()
		wanted := deliveryConsumerConfig()
		// Starting at new messages, so a durable recreated on the package's shared stream is handed
		// only this test's.
		wanted.DeliverPolicy = natsgo.DeliverNewPolicy
		if err := natsClient.JS().DeleteConsumer(bus.Stream, wanted.Durable); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
			t.Fatalf("delete the delivery durable: %v", err)
		}
		if _, err := natsClient.JS().AddConsumer(bus.Stream, &wanted); err != nil {
			t.Fatalf("create the delivery durable: %v", err)
		}
		return wanted
	}
	intake := newIntake(natsClient, pool, nil, newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	if _, err := intake.subscribe(recreate().DeliverSubject); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { intake.StopDelivering(context.Background()) })
	intake.StopDelivering(t.Context())
	waitFor(t, 5*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the durable kept its subscriber after StopDelivering returned"
	})

	if err := natsClient.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	select {
	case <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the client's reconnect hooks never ran")
	}
	// The client restores its subscriptions before it runs the hooks, and nats-server applies a
	// subscriber's interest within moments, so a rebind would show by now.
	time.Sleep(time.Second)
	if deliveryConsumerInfo(t, natsClient).PushBound {
		t.Fatal("the reconnect bound the stopped intake's subscription to the durable again")
	}

	if _, err := intake.subscribe(recreate().DeliverSubject); err != nil {
		t.Fatalf("subscribe again on the same client: %v", err)
	}
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 8, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "8", "author": "octocat",
	})
	awaitAcknowledged(t, natsClient)
	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastEventAt == nil {
		t.Fatalf("settings after the message handled by the second subscription = %+v, %v: want last_event_at recorded", settings, err)
	}
}

// A stop after the client replaced its connection drains the subscription the client bound again
// on the new connection, which is the one delivering, rather than the handle subscribe returned,
// which the closed connection took with it. The durable loses its subscriber as the stop begins, so
// a message published while a handler still runs is not handed to the stopping process, which would
// hand it back and be handed it again; the stop logs no failed drain, and the next process to bind
// the durable handles the message.
func TestAStopAfterTheConnectionWasReplacedDrainsTheRestoredSubscription(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	held := holdPullRequestOne(fake)
	github := fake.newTestClient()
	t.Cleanup(held.release)
	natsClient := intakeTestClient(t)
	hooked := make(chan struct{}, 8)
	natsClient.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})
	intake := newIntake(natsClient, pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	stop := runIntake(t, intake)
	awaitBoundDurable(t, natsClient)

	// The client dials a connection to replace the closed one and binds its registration there: a
	// handle of its own, not the one subscribe returned.
	natsClient.Conn.Close()
	select {
	case <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the client's reconnect hooks never ran")
	}
	waitFor(t, 20*time.Second, func() bool { return natsClient.SubOK() && deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the client never bound the intake's subscription again on its new connection"
	})
	intake.mu.Lock()
	delivery := intake.delivering
	intake.mu.Unlock()
	if delivery.sub.IsValid() {
		t.Fatal("the handle subscribe returned is still valid on the replaced connection; the test proves nothing")
	}

	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 1, mergedPullRequest(1))
	held.awaitEntered(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	waitFor(t, 20*time.Second, delivery.stopping.Load, func() string { return "the intake never began stopping" })
	waitFor(t, 5*time.Second, func() bool { return !deliveryConsumerInfo(t, natsClient).PushBound }, func() string {
		return "the stopping intake's subscription stayed bound to the durable while its handler ran: the stop did not drain " +
			"the subscription the client restored on its new connection"
	})
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 2, mergedPullRequest(2))
	// nats-server hands a bound consumer's subscriber a new message within moments.
	time.Sleep(time.Second)

	held.release()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the stopping intake did not return once its handler ended")
	}
	if delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer; delivered != before {
		t.Fatalf("the durable delivered %d message(s) to the stopping intake after its stop began, want none", delivered-before)
	}
	for _, record := range logs() {
		if message, _ := record["msg"].(string); strings.Contains(message, "drain") {
			t.Errorf("the stop logged %q (%v)", message, record["error"])
		}
	}

	runIntake(t, newIntake(intakeTestClient(t), pool, deliveryTestLeases(t, pool), github, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitAcknowledged(t, natsClient)
	if events, want := held.of("2"), []string{"start 2", "end 2"}; !slices.Equal(events, want) {
		t.Fatalf("pull request 2's events = %q, want %q: the next process handles the message published during the stop", events, want)
	}
}
