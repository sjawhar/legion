package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// Canonical policy for the listener's durable consumer. DeliverSubject is
// deliberately not part of the policy: it is fixed at creation and preserved
// for the consumer's lifetime, and the lease holder subscribes to whatever
// subject the server has persisted (durableReceiver).
//
// consumerAckWait is short because the holder keeps every message it holds alive: it sends
// InProgress every inProgressEvery for each message it has received and not yet settled, queued
// or in hand, so a message waits out the ack wait only once the pod holding it has died or lost
// the lease, and then returns to the next holder within seconds.
//
// consumerInactiveThreshold lets the server delete a machine's durable
// consumer once its listener has been gone this long. The notification
// stream only retains 72h of messages, so a consumer inactive longer than
// that has already lost data and its durability protects nothing; without
// a threshold, every decommissioned machine leaves a consumer behind
// forever, silently accumulating pending state.
const (
	consumerAckWait           = 6 * time.Second
	consumerMaxAckPending     = 256
	consumerMaxDeliver        = 20
	consumerInactiveThreshold = 7 * 24 * time.Hour
)

// applyListenerConsumerPolicy stamps the canonical consumer policy onto
// config. Shared by the create and drift-correction paths so the policy has
// exactly one definition: the drift correction applies it to a copy of the
// durable's config and updates the durable when the copy differs. The policy
// filters on the full wildcard (consumerFilterAll), so the durable takes every
// message the stream stores and no build's own list of stream subjects narrows
// it: two builds whose stream subjects differ share one durable during a roll.
// The policy has no idle heartbeat: the create path starts from a zero config,
// so a durable the listener creates has none. listenerDurableRefusal refuses an
// existing durable whose heartbeat or ack policy differs, since NATS cannot
// change either in place.
func applyListenerConsumerPolicy(config *nats.ConsumerConfig) {
	config.FilterSubject = consumerFilterAll
	config.FilterSubjects = nil
	config.AckPolicy = nats.AckExplicitPolicy
	config.AckWait = consumerAckWait
	config.MaxAckPending = consumerMaxAckPending
	config.MaxDeliver = consumerMaxDeliver
	config.InactiveThreshold = consumerInactiveThreshold
}

// consumerFilterAll is the durable's filter: every subject, which is every message the stream
// stores. It is a filter rather than none because nats-server 2.10 (2.10.29 measured) accepts an
// update that clears a push durable's filter and then delivers nothing published after it, and
// every durable an earlier listener created carries a filter (its build's stream subjects); an
// update to the full wildcard delivers everything.
const consumerFilterAll = ">"

// errListenerDurableRefused marks a durable the listener will not receive from however often it is
// asked: no retry can succeed, so the listener exits.
var errListenerDurableRefused = errors.New("listener durable refused")

// durableRefusedLine is the line a listener logs when it refuses its durable, at the check after
// the NATS connect and when the lease holder reads the durable. The deployed durable-refusal metric
// filter matches it exactly (defined in the deployment repository's listener infrastructure), so
// both sites use this constant.
const durableRefusedLine = "subscribe refused, shutting down"

// listenerDurableRefusal refuses an existing durable carrying a setting the listener's consumer
// policy fixes and NATS cannot change in place, so the drift correction could never apply it:
//   - An idle heartbeat. The bus logs nats.ErrConsumerNotActive at WARN because only KV watchers'
//     ordered consumers report it, and only while disconnected; a heartbeat here would make a
//     stalled durable report that same WARN.
//   - An ack policy other than explicit. The delivery handler acks each message or NAKs it for a
//     delayed retry, one at a time; under ack all, a later message's ack would also ack an earlier
//     one still waiting for its retry.
//
// Recreating the durable would drop its cursor, so the listener leaves it to an operator, and the
// refusal names every such setting the durable carries and says how to recreate it without
// replaying the stream.
func listenerDurableRefusal(consumer string, config nats.ConsumerConfig) error {
	var settings []string
	if config.Heartbeat != 0 {
		settings = append(settings, fmt.Sprintf("an idle heartbeat of %s", config.Heartbeat))
	}
	if config.AckPolicy != nats.AckExplicitPolicy {
		settings = append(settings, fmt.Sprintf("ack policy %s", config.AckPolicy))
	}
	if len(settings) == 0 {
		return nil
	}
	without := "that setting"
	if len(settings) > 1 {
		without = "those settings"
	}
	return fmt.Errorf("%w: durable consumer %s has %s, which the listener's consumer policy forbids and NATS cannot change in place; "+
		"deleting it lets the listener recreate it at deliver policy all, which replays every retained message, "+
		"so to keep its cursor recreate it from its own config without %s, at deliver policy by_start_sequence "+
		"with opt_start_seq one past its ack_floor.stream_seq (packages/envoy/AGENTS.md, Operational notes)",
		errListenerDurableRefused, consumer, strings.Join(settings, " and "), without)
}

// listenerDurable reads the machine's durable under ctx. It returns a nil info when there is none,
// listenerDurableRefusal's error when the listener's policy cannot use the one there is, and the
// lookup's error when NATS cannot say.
func listenerDurable(ctx context.Context, client *bus.Client, consumer string) (*nats.ConsumerInfo, error) {
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer, nats.Context(ctx))
	if errors.Is(err, nats.ErrConsumerNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := listenerDurableRefusal(consumer, info.Config); err != nil {
		return nil, err
	}
	return info, nil
}

// ensureListenerDurable preserves the durable consumer so restarts resume from the last ACKed
// message instead of skipping pending work, and returns it as the server holds it: it reads the
// durable (listenerDurable) and writes it to the consumer policy (correctListenerDurable). Every
// request runs under ctx. Only the lease holder calls it: a standby neither creates nor updates
// the durable.
func ensureListenerDurable(ctx context.Context, client *bus.Client, consumer string) (*nats.ConsumerInfo, error) {
	info, err := listenerDurable(ctx, client, consumer)
	if err != nil {
		return nil, err
	}
	return correctListenerDurable(ctx, client, consumer, info)
}

// correctListenerDurable writes the durable listenerDurable read as info to the consumer policy and
// returns it as the server then holds it. A durable that is not there (info nil) is created
// server-side, at a random deliver inbox; one that is there is updated when it has drifted from
// the policy, which also stamps consumerInactiveThreshold onto durables created before the
// threshold existed. Every request runs under ctx.
//
// The consumer is always created server-side, never by js.Subscribe: the client library deletes
// a consumer it created itself on Unsubscribe()/Drain(), which would silently reset the cursor.
func correctListenerDurable(ctx context.Context, client *bus.Client, consumer string, info *nats.ConsumerInfo) (*nats.ConsumerInfo, error) {
	if info == nil {
		// The deliver subject is a random inbox, not a derivable name: on a shared NATS account a
		// predictable subject would let any client hold interest on it, shadow-reading deliveries.
		config := nats.ConsumerConfig{
			Durable:        consumer,
			DeliverSubject: nats.NewInbox(),
		}
		applyListenerConsumerPolicy(&config)
		return client.JS().AddConsumer(bus.Stream, &config, nats.Context(ctx))
	}
	// Only a field the policy writes can differ: the copy shares every other one, the server-set
	// metadata included, and listenerDurable has already refused any ack policy but explicit.
	corrected := info.Config
	applyListenerConsumerPolicy(&corrected)
	if reflect.DeepEqual(corrected, info.Config) {
		return info, nil
	}
	return client.JS().UpdateConsumer(bus.Stream, &corrected, nats.Context(ctx))
}

// errDurableUnbound marks a durable consumer that exists but does not deliver to the lease
// holder's subscription: the holder's /healthz quotes it as "durable consumer unavailable: not
// bound to this listener's subscription", and the self-health monitor treats it as terminal, so
// its rebuild subscribes again (durableReceiver.resubscribe). A standby has no subscription and
// never reports it.
var errDurableUnbound = errors.New("not bound to this listener's subscription")

// durableProbeConfirm is how long the holder's durable probe waits before its confirming read.
const durableProbeConfirm = 250 * time.Millisecond

// waitUnlessDone waits d out and reports nil, or returns ctx.Err() when ctx ended first. Every
// wait of the receive loop sleeps this way, so none holds a shutdown or a lost lease waiting for a
// timer the cancellation cannot cut.
func waitUnlessDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
