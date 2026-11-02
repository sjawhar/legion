package main

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
)

// countingAcker answers a relayed message as the pull's acker does, and counts what it was told.
type countingAcker struct {
	acked      int
	progressed int
	naked      int
}

func (a *countingAcker) Ack() error                       { a.acked++; return nil }
func (a *countingAcker) InProgress() error                { a.progressed++; return nil }
func (a *countingAcker) NakWithDelay(time.Duration) error { a.naked++; return nil }

// The relay's refusal of an address no attempt of this pod can reach reports itself through the
// durable path's own bookkeeping (applyDeliveryOutcome), so it publishes an exception by exactly
// the rules that path applies: a message on an ordinary topic earns one, and a message on an
// exceptions topic earns none - an exception about an exception is what the durable path has
// never published (delivery_outcome.go's isExceptionsTopic guard), and a relay that rebuilt that
// reporting by hand published one. Either way the message is acked, since no later pull of this
// consumer would reach the session.
func TestARelayedMessageRefusedForAnUnreachableAddressReportsItAsTheDurablePathDoes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		topic         string
		wantException bool
	}{
		{
			name:          "an ordinary topic earns an exception",
			topic:         "notifications.github.owner.repo.issue.11",
			wantException: true,
		},
		{
			name:  "an exceptions topic earns none",
			topic: bus.ExceptionTopicPrefix + "notifications.github.owner.repo.issue.11",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
			probe := exceptionProbe(t, rig.harness.client)
			sessionID := "ses_relay_unreachable_" + id.New()
			item := listenerTestEnvelope(tc.topic, "relay-unreachable-"+id.New())
			acker := &countingAcker{}
			msg := bus.SessionMessage{Subject: tc.topic, StreamSeq: 7, SessionAcker: acker}

			rig.relays.refuseUnreachableAddress(sessionID, item, msg, dedupe.Attempt{}, session.ErrAddressNotReachable)

			if acker.acked != 1 {
				t.Fatalf("the refused message was acked %d times, want once: no pull of this consumer reaches the session either", acker.acked)
			}
			published, err := probe.NextMsg(time.Second)
			switch {
			case tc.wantException && err != nil:
				t.Fatalf("the relay published no delivery exception for the refused address: %v", err)
			case !tc.wantException:
				if !errors.Is(err, natsgo.ErrTimeout) {
					t.Fatalf("the relay published an exception about a message on an exceptions topic (%v), which the durable path never does", err)
				}
				return
			}
			var exception contracts.Envelope
			if err := decodeStoredEnvelope(published.Data, &exception); err != nil {
				t.Fatalf("decode the exception the relay published: %v", err)
			}
			if want := deliveryExceptionID(item.EventID, sessionID, "delivery_failed"); exception.EventID != want {
				t.Fatalf("exception id = %q, want the deterministic %q", exception.EventID, want)
			}
			if published.Subject != bus.ExceptionTopicPrefix+item.Topic {
				t.Fatalf("the exception went to %q, want the exception lane of %q", published.Subject, item.Topic)
			}
		})
	}
}
