package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestBothIntakesKeepTheirSubscriptions starts the two intakes as cmd/dispatch does (StartIntakes,
// beside a client of its own that owns the stream and publishes) and holds that each connects under
// its own name, keeps its durable push-bound and handles what is published to it. A bus.Client
// keeps one JetStream subscription, so two intakes binding through one client leave whichever bound
// first with its durable unbound: its messages stay pending and its handler never runs.
func TestBothIntakesKeepTheirSubscriptions(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	fake := newJobsFake(t)

	runCtx, cancel := context.WithCancel(ctx)
	var names []string
	var clients []*bus.Client
	stop, err := StartIntakes(runCtx, func(name string) (*bus.Client, error) {
		names = append(names, name)
		client, err := bus.Connect([]string{natsURL}, bus.WithName(name))
		if err == nil {
			clients = append(clients, client)
		}
		return client, err
	}, pool, fake.newTestClient())
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	// Run reads settingsPollInterval, which a later test's shrinkIntakeFlowControl writes, so the
	// test ends only once both intakes have stopped (startIntake's reason).
	t.Cleanup(func() {
		cancel()
		stop()
	})
	if want := []string{"dispatch-intake", "dispatch-job-intake"}; !slices.Equal(names, want) {
		t.Fatalf("StartIntakes connected as %q, want %q", names, want)
	}

	durables := []string{deliveryConsumerName, jobConsumerName}
	consumers := func() string {
		var states []string
		for _, durable := range durables {
			info, err := publisher.JS().ConsumerInfo(bus.Stream, durable)
			if err != nil {
				states = append(states, fmt.Sprintf("%s: %v", durable, err))
				continue
			}
			states = append(states, fmt.Sprintf("%s: push_bound=%t pending=%d ack_pending=%d delivered=%d",
				durable, info.PushBound, info.NumPending, info.NumAckPending, info.Delivered.Consumer))
		}
		return strings.Join(states, "; ")
	}
	pushBound := func(durable string) bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, durable)
		return err == nil && info.PushBound
	}
	waitFor(t, 20*time.Second, func() bool {
		return pushBound(deliveryConsumerName) && pushBound(jobConsumerName)
	}, func() string {
		return "the intakes' durables are not both push-bound: " + consumers()
	})
	// The server can read the two durables while one registration's unsubscribe is still on its way
	// to it, so both are read again a moment later: nothing binds an unbound durable again.
	time.Sleep(time.Second)
	for _, durable := range durables {
		if !pushBound(durable) {
			t.Fatalf("%s lost its subscription once both intakes had bound: %s", durable, consumers())
		}
	}

	// A pull request the intake handles records the event's time (deliver), and a job of a tracked
	// deploy run has the job intake read the run's jobs from GitHub once the burst goes quiet.
	publishPullRequestEnvelope(t, publisher, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	publishGitHubEnvelope(t, publisher, "notifications.github.acme.widgets.job.500.completed", jobPayload("acme/widgets", 500))
	waitFor(t, 20*time.Second, func() bool {
		settings, err := GetSettings(ctx, pool)
		return err == nil && settings.LastEventAt != nil
	}, func() string {
		return "the intake never handled the pull request published to it: " + consumers()
	})
	waitFor(t, 20*time.Second, func() bool { return fake.calls.Load() == 1 }, func() string {
		return fmt.Sprintf("the job intake read the run's jobs %d times, want 1: %s", fake.calls.Load(), consumers())
	})
	for _, durable := range durables {
		var info *natsgo.ConsumerInfo
		waitFor(t, 20*time.Second, func() bool {
			info, err = publisher.JS().ConsumerInfo(bus.Stream, durable)
			return err == nil && info.NumPending == 0 && info.NumAckPending == 0 && info.Delivered.Consumer > 0
		}, func() string {
			return fmt.Sprintf("%s holds messages its intake never acknowledged: %s", durable, consumers())
		})
	}

	// stop waits for both intakes, which stop once ctx is cancelled, and closes both clients.
	cancel()
	stop()
	for i, client := range clients {
		if !client.Conn.IsClosed() {
			t.Errorf("the %s client is open after stop", names[i])
		}
	}
}

// TestAMessageInItsHandlerAtShutdownIsAcknowledged: a merged pull request whose GitHub fetch is
// still running when ctx is cancelled is acknowledged once the fetch returns, because the intake's
// client stays open until stop. deliver runs on the subscription's delivery goroutine under a
// deadline of its own, so it outlives Run; a client closed as Run returned took the acknowledgement
// down with it, and the message stayed outstanding until the ack wait handed it to the next
// process.
func TestAMessageInItsHandlerAtShutdownIsAcknowledged(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	fake := newFakeGitHub(t)
	entered, held := make(chan struct{}), make(chan struct{})
	var enter sync.Once
	fake.handle("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		enter.Do(func() { close(entered) })
		<-held
		w.WriteHeader(http.StatusInternalServerError)
	})
	github := fake.newTestClient()
	release := sync.OnceFunc(func() { close(held) })
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handler it holds.
	t.Cleanup(release)

	runCtx, cancel := context.WithCancel(ctx)
	stop, err := StartIntakes(runCtx, func(string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL})
	}, pool, github)
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stop()
	})
	awaitBoundDurable(t, publisher)
	consumer := func() string {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		if err != nil {
			return err.Error()
		}
		return fmt.Sprintf("push_bound=%t delivered=%d ack_floor=%d ack_pending=%d redelivered=%d",
			info.PushBound, info.Delivered.Consumer, info.AckFloor.Consumer, info.NumAckPending, info.NumRedelivered)
	}

	publishPullRequestEnvelope(t, publisher, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatalf("the intake never fetched the merged pull request: %s", consumer())
	}
	cancel()
	// Run has returned once the durable loses its subscriber: Run unsubscribes as it ends.
	waitFor(t, 20*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		return err == nil && !info.PushBound
	}, func() string {
		return "the intake's durable kept its subscriber after ctx was cancelled: " + consumer()
	})
	release()
	// deliver logs an acknowledgement that failed, and the negative acknowledgement after it.
	failedAcks := func() []map[string]any {
		var failed []map[string]any
		for _, record := range logs() {
			if message, _ := record["msg"].(string); strings.Contains(message, "acknowledge") {
				failed = append(failed, record)
			}
		}
		return failed
	}
	waitFor(t, 20*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		return err == nil && info.Delivered.Consumer == 1 && info.AckFloor.Consumer == 1 && info.NumAckPending == 0
	}, func() string {
		return fmt.Sprintf("the message in its handler at shutdown was never acknowledged: %s; failed acknowledgements: %v",
			consumer(), failedAcks())
	})
	stop()
	if failed := failedAcks(); len(failed) > 0 {
		t.Fatalf("an acknowledgement failed at shutdown: %v", failed)
	}
}

// TestIntakesWithoutTheAppConnectNothing: with no GitHub App neither intake reads anything, so
// StartIntakes opens no connection and its stop returns at once.
func TestIntakesWithoutTheAppConnectNothing(t *testing.T) {
	stop, err := StartIntakes(t.Context(), func(string) (*bus.Client, error) {
		t.Error("connected a client for intakes that have no GitHub App")
		return nil, errors.New("no connection here")
	}, nil, nil)
	if err != nil {
		t.Fatalf("start the intakes: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping intakes with no GitHub App did not return")
	}
}

// TestIntakesWhoseSecondConnectFailsStartNeither: the job intake's connect failing returns its
// error with neither intake started and the intake's client closed, so no durable is bound.
func TestIntakesWhoseSecondConnectFailsStartNeither(t *testing.T) {
	publisher := intakeTestClient(t)
	natsURL := testnats.URL(t)
	var first *bus.Client
	refused := errors.New("refused")
	_, err := StartIntakes(t.Context(), func(string) (*bus.Client, error) {
		if first != nil {
			return nil, refused
		}
		var err error
		first, err = bus.Connect([]string{natsURL})
		return first, err
	}, nil, newFakeGitHub(t).newTestClient())
	if !errors.Is(err, refused) {
		t.Fatalf("start the intakes = %v, want the job intake's connect error", err)
	}
	if first == nil || !first.Conn.IsClosed() {
		t.Fatal("the intake's client was left open when the job intake's connect failed")
	}
	for _, durable := range []string{deliveryConsumerName, jobConsumerName} {
		if _, err := publisher.JS().ConsumerInfo(bus.Stream, durable); !errors.Is(err, natsgo.ErrConsumerNotFound) {
			t.Errorf("%s after a failed start: %v, want no such durable", durable, err)
		}
	}
}

// TestTwoIntakesOnOneClientRefuseTheSecondBind: the intakes on one client, as cmd/dispatch wired
// them before StartIntakes, no longer lose a durable silently. The job intake binds first; the
// delivery intake's bind on the same client is refused with bus.ErrSubscriptionHeld and logged at
// ERROR, and the job intake's durable keeps its subscriber.
func TestTwoIntakesOnOneClientRefuseTheSecondBind(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	shared := intakeTestClient(t)
	github := newFakeGitHub(t).newTestClient()
	pushBound := func(durable string) bool {
		info, err := shared.JS().ConsumerInfo(bus.Stream, durable)
		return err == nil && info.PushBound
	}

	jobsCtx, stopJobs := context.WithCancel(ctx)
	jobsDone := make(chan struct{})
	go func() {
		defer close(jobsDone)
		newJobIntake(shared, pool, github, realJobClock{}).Run(jobsCtx)
	}()
	t.Cleanup(func() {
		stopJobs()
		<-jobsDone
	})
	waitFor(t, 20*time.Second, func() bool { return pushBound(jobConsumerName) }, func() string {
		return "the job intake never bound its durable"
	})

	seedDeliverySettings(t, ctx, pool)
	startIntake(t, newIntake(shared, pool, github))
	var refusal map[string]any
	waitFor(t, 20*time.Second, func() bool {
		for _, record := range logs() {
			message, _ := record["error"].(string)
			if record["msg"] == "dispatch delivery: bind NATS consumer" && record["name"] == deliveryConsumerName &&
				strings.Contains(message, bus.ErrSubscriptionHeld.Error()) {
				refusal = record
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("the delivery intake's bind on the job intake's client was not refused with %q; %s=%t %s=%t",
			bus.ErrSubscriptionHeld, jobConsumerName, pushBound(jobConsumerName), deliveryConsumerName, pushBound(deliveryConsumerName))
	})
	if refusal["level"] != "ERROR" {
		t.Fatalf("the refused bind was logged at %v, want ERROR: %v", refusal["level"], refusal)
	}
	if !pushBound(jobConsumerName) {
		t.Fatal("the job intake's durable lost its subscriber to the refused bind")
	}
	if pushBound(deliveryConsumerName) {
		t.Fatal("the delivery intake's durable is bound on a client that holds the job intake's")
	}
}
