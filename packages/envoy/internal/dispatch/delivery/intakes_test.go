package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestBothIntakesKeepTheirSubscriptions starts the two intakes as cmd/dispatch does (StartIntakes,
// beside a client of its own that owns the stream and publishes) and holds that each keeps its
// durable push-bound and handles what is published to it. A bus.Client keeps one JetStream
// subscription, so two intakes binding through one client leave whichever bound first with its
// durable unbound: its messages stay pending and its handler never runs.
func TestBothIntakesKeepTheirSubscriptions(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	fake := newJobsFake(t)

	runCtx, cancel := context.WithCancel(ctx)
	stopped, err := StartIntakes(runCtx, func() (*bus.Client, error) {
		return bus.Connect([]string{natsURL})
	}, pool, fake.newTestClient())
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	// Run reads settingsPollInterval, which a later test's shrinkIntakeFlowControl writes, so the
	// test ends only once both intakes have stopped (startIntake's reason).
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

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
}

// TestIntakesWithoutTheAppConnectNothing: with no GitHub App neither intake reads anything, so
// StartIntakes opens no connection and reports them stopped at once.
func TestIntakesWithoutTheAppConnectNothing(t *testing.T) {
	stopped, err := StartIntakes(t.Context(), func() (*bus.Client, error) {
		t.Error("connected a client for intakes that have no GitHub App")
		return nil, errors.New("no connection here")
	}, nil, nil)
	if err != nil {
		t.Fatalf("start the intakes: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("intakes with no GitHub App are not reported stopped")
	}
}

// TestIntakesWhoseSecondConnectFailsStartNeither: the job intake's connect failing returns its
// error with neither intake started and the intake's client closed, so no durable is bound.
func TestIntakesWhoseSecondConnectFailsStartNeither(t *testing.T) {
	publisher := intakeTestClient(t)
	natsURL := testnats.URL(t)
	var first *bus.Client
	refused := errors.New("refused")
	_, err := StartIntakes(t.Context(), func() (*bus.Client, error) {
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
