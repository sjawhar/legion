package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// Canonical policy for the listener's durable consumer. DeliverSubject is
// deliberately not part of the policy: it is fixed at creation and preserved
// for the consumer's lifetime (nats.Bind attaches to whatever subject the
// server has persisted).
//
// consumerInactiveThreshold lets the server delete a machine's durable
// consumer once its listener has been gone this long. The notification
// stream only retains 72h of messages, so a consumer inactive longer than
// that has already lost data and its durability protects nothing; without
// a threshold, every decommissioned machine leaves a consumer behind
// forever, silently accumulating pending state.
const (
	consumerAckWait           = 60 * time.Second
	consumerMaxAckPending     = 256
	consumerMaxDeliver        = 20
	consumerInactiveThreshold = 7 * 24 * time.Hour
)

// applyListenerConsumerPolicy stamps the canonical consumer policy onto
// config. Shared by the create and drift-correction paths so the policy has
// exactly one definition: the drift correction applies it to a copy of the
// durable's config and updates the durable when the copy differs. The policy
// has no idle heartbeat: the create path starts from a zero config, so a
// durable the listener creates has none. listenerDurableRefusal refuses an
// existing durable whose heartbeat or ack policy differs, since NATS cannot
// change either in place.
func applyListenerConsumerPolicy(config *nats.ConsumerConfig, subjects []string) {
	config.FilterSubject = ""
	config.FilterSubjects = subjects
	config.AckPolicy = nats.AckExplicitPolicy
	config.AckWait = consumerAckWait
	config.MaxAckPending = consumerMaxAckPending
	config.MaxDeliver = consumerMaxDeliver
	config.InactiveThreshold = consumerInactiveThreshold
}

// errListenerDurableRefused marks a durable startListenerSubscription will not bind however often
// it is asked: no retry can succeed, so the listener's startup exits at once.
var errListenerDurableRefused = errors.New("listener durable refused")

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

// listenerDurable reads the machine's durable. It returns a nil info when there is none,
// listenerDurableRefusal's error when the listener's policy cannot use the one there is, and the
// lookup's error when NATS cannot say.
func listenerDurable(client *bus.Client, consumer string) (*nats.ConsumerInfo, error) {
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
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

// startListenerSubscription preserves the durable consumer so restarts resume
// from the last ACKed non-role message instead of skipping pending work.
//
// The consumer is always created server-side and then bound, never created by
// js.Subscribe: the client library deletes consumers it created itself on
// Unsubscribe()/Drain(), and both the bus reconnect recovery and the SIGTERM
// drain paths trigger exactly that — silently resetting the durable cursor.
// The config drift-correction also stamps consumerInactiveThreshold onto
// consumers created before the threshold existed.
func startListenerSubscription(client *bus.Client, consumer string, handler nats.MsgHandler) (*nats.Subscription, error) {
	subjects := bus.StreamSubjects()
	info, err := listenerDurable(client, consumer)
	switch {
	case err != nil:
		return nil, err
	case info == nil:
		// The deliver subject is a random inbox, not a derivable name: on a
		// shared NATS account a predictable subject would let any client
		// hold interest on it — shadow-reading deliveries and making the
		// consumer look push-bound so the listener could never attach.
		config := nats.ConsumerConfig{
			Durable:        consumer,
			DeliverSubject: nats.NewInbox(),
		}
		applyListenerConsumerPolicy(&config, subjects)
		if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
			return nil, err
		}
	default:
		// Only a field the policy writes can differ: the copy shares every other one, the
		// server-set metadata included, and listenerDurable has already refused any ack policy
		// but explicit.
		corrected := info.Config
		applyListenerConsumerPolicy(&corrected, subjects)
		if !reflect.DeepEqual(corrected, info.Config) {
			if _, err := client.JS().UpdateConsumer(bus.Stream, &corrected); err != nil {
				return nil, err
			}
		}
	}
	return client.Subscribe(
		"",
		handler,
		nats.Bind(bus.Stream, consumer),
		nats.ManualAck(),
	)
}

// errListenerDurableBindExhausted marks a bind still refused at its deadline: something other than
// a rolling deploy's old task holds the durable, or JetStream keeps failing the lookup, so the start
// ends and the runtime starts the listener again on a fresh connection.
var errListenerDurableBindExhausted = errors.New("listener durable bind exhausted")

// During a rolling deploy the durable's push binding is the old task's until that task stops, which
// takes its deregistration from the load balancer and its own shutdown: 35 to 71 s in production.
// The bind is polled every durableBindInterval, so it lands within one interval of the old task's
// exit, and given up at durableBindDeadline, the 135 s the backoff it replaced took to exhaust, so
// the subscribe-exhausted page and the runtime's relaunch come no later than they did.
const (
	durableBindInterval = 2 * time.Second
	durableBindDeadline = 135 * time.Second
	durableBindLogEvery = 30 * time.Second
)

// durableBind is how many attempts a bind took and how long it waited.
type durableBind struct {
	attempts int
	waited   time.Duration
}

// bindListenerDurable binds consumer through startListenerSubscription every interval until it
// binds, the bus's auto-resubscribe after a reconnect binds it, the durable is refused
// (errListenerDurableRefused, returned as is), deadline passes (errListenerDurableBindExhausted,
// wrapping the last refusal), or ctx ends (ctx.Err()). It logs the first refusal and then one every
// durableBindLogEvery, and the bind itself.
func bindListenerDurable(ctx context.Context, client *bus.Client, consumer string, handler nats.MsgHandler, logger *logging.Logger, interval, deadline time.Duration) (durableBind, error) {
	start := time.Now()
	var bind durableBind
	bound := func(autoResubscribe bool) (durableBind, error) {
		logger.Info("durable bound",
			slog.Int("attempts", bind.attempts),
			slog.Int64("waited_ms", bind.waited.Milliseconds()),
			slog.Bool("auto_resubscribe", autoResubscribe),
		)
		return bind, nil
	}
	var logged time.Time
	for {
		if err := ctx.Err(); err != nil {
			bind.waited = time.Since(start)
			return bind, err
		}
		bind.attempts++
		_, err := startListenerSubscription(client, consumer, handler)
		bind.waited = time.Since(start)
		if err == nil {
			return bound(false)
		}
		if errors.Is(err, errListenerDurableRefused) {
			return bind, err
		}
		if bind.waited >= deadline {
			return bind, fmt.Errorf("%w after %d attempts in %s: %w", errListenerDurableBindExhausted, bind.attempts, bind.waited.Round(time.Millisecond), err)
		}
		wait := min(interval, deadline-bind.waited)
		if logged.IsZero() || time.Since(logged) >= durableBindLogEvery {
			logged = time.Now()
			logger.Warn("subscribe failed, retrying",
				slog.String("error", err.Error()),
				slog.Int("attempt", bind.attempts),
				slog.String("retry_in", wait.String()),
				slog.String("deadline", deadline.String()),
			)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			bind.waited = time.Since(start)
			return bind, ctx.Err()
		case <-timer.C:
		}
		// A reconnect during the wait re-subscribes the subscription the failed attempt registered
		// (bus.Client.onReconnect), which binds it once the durable is free. Another attempt would
		// replace that handle with a bind racing the server's release of it, which can be refused
		// with "consumer is already bound to a subscription".
		if client.SubOK() {
			bind.waited = time.Since(start)
			return bound(true)
		}
	}
}
