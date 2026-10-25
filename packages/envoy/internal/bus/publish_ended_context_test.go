package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A publish whose context has already ended sends nothing and says so: never a message on the wire
// behind an answer of failure. The core transport put it there before - publishCoreTo kept the
// caller's spent deadline, ensureConnWithContext returned at once on a healthy connection, and
// publishConfirmed published before it found the window gone - so a role lane's holder received a
// message its sender was told failed, and the sender's retry (the HTTP client retries every 5xx)
// delivered it again over a subject with no duplicate window. The route that reaches it is
// publishHandler, whose holder lookup can spend the route's context before the publish begins.
//
// The JetStream transport is pinned beside it. It has no fix of its own: nats.go refuses an ended
// context before sending the publish request (publishJetStream says where), and this is what
// keeps an upgrade that stopped doing so from going unnoticed.
//
// Each case subscribes before it publishes and flushes the publisher's connection after, so a
// message that was sent has reached the server, and been handed on, before the subscriber is read.
// A context ends two ways, and both are refused: a deadline already past, the case above, and a
// cancel with the deadline still ahead, which only the check at publishCoreTo's entry catches,
// since publishConfirmed's own check reads the deadline alone. The control is the same call with a
// live context, which must publish and be received.
func TestAPublishWithAnEndedContextSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		topic string
		core  bool
	}{
		{"a role lane", contracts.RoleTopicPrefix + "ended-context", true},
		{"an ad-hoc topic", "notifications.team.ended-context", true},
		{"a topic the stream captures", contracts.AgentSubject("ses_ended_context"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesCoreTransport(tc.topic); got != tc.core {
				t.Fatalf("usesCoreTransport(%q) = %t, want %t: the case would not exercise the transport it names", tc.topic, got, tc.core)
			}
			uri := testnats.URL(t)
			client, err := ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			subscriber := testnats.Connect(t, uri)
			t.Cleanup(subscriber.Close)
			received, err := subscriber.SubscribeSync(tc.topic)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			if err := subscriber.Flush(); err != nil {
				t.Fatalf("flush the subscription: %v", err)
			}
			envelope := contracts.Envelope{
				EventID:        "evt-ended-context",
				Source:         "agent",
				SourceEventID:  "source-ended-context",
				Topic:          tc.topic,
				DedupeKey:      "publish.ended-context",
				IssuedAt:       contracts.NowMillis(),
				PayloadSummary: "published after its window",
				TraceID:        "trace-ended-context",
			}

			past, cancelPast := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancelPast()
			cancelled, cancel := context.WithTimeout(context.Background(), time.Minute)
			cancel()
			for _, ended := range []struct {
				name string
				ctx  context.Context
				err  error
			}{
				{"a deadline already past", past, context.DeadlineExceeded},
				{"a cancelled context", cancelled, context.Canceled},
			} {
				publishErr := client.PublishWithin(ended.ctx, envelope)
				if publishErr == nil {
					t.Fatalf("publish under %s succeeded, want it refused", ended.name)
				}
				if err := client.Conn.Flush(); err != nil {
					t.Fatalf("flush the publisher's connection: %v", err)
				}
				if message, err := received.NextMsg(500 * time.Millisecond); err == nil {
					t.Fatalf("the subscriber received %s from a publish under %s, whose caller was told it failed: %v", string(message.Data), ended.name, publishErr)
				}
				if !errors.Is(publishErr, ended.err) {
					t.Fatalf("publish under %s = %v, want an error naming %v", ended.name, publishErr, ended.err)
				}
			}
			if !tc.core {
				if _, err := client.JS().GetLastMsg(Stream, tc.topic); !errors.Is(err, nats.ErrMsgNotFound) {
					t.Fatalf("the stream's last message on %s after the refused publishes: %v, want none", tc.topic, err)
				}
			}

			// The control: a live context publishes, the subscriber receives it, and the stream
			// retains it where it captures the topic.
			live, cancelLive := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelLive()
			if err := client.PublishWithin(live, envelope); err != nil {
				t.Fatalf("publish under a live context: %v", err)
			}
			if _, err := received.NextMsg(5 * time.Second); err != nil {
				t.Fatalf("the subscriber received nothing from the live publish: %v", err)
			}
			if !tc.core {
				if _, err := client.JS().GetLastMsg(Stream, tc.topic); err != nil {
					t.Fatalf("the stream did not retain the live publish on %s: %v", tc.topic, err)
				}
			}
		})
	}
}

// publishConfirmed refuses a deadline already spent before it calls publish, and answers it with
// the spent-window text it has always had. publishCoreTo refuses a context ended at its entry, so
// a deadline reaches publishConfirmed spent only when it ran out in between - the check here is
// what closes that gap, and nothing end to end can time it, so it is pinned on its own: publish
// must not be called at all. The text is pinned because a deployed filter may match it; the check
// moved ahead of the publish, the answer did not change. The control is the same call with a
// deadline ahead, which publishes, flushes and confirms.
func TestAPublishConfirmedWithASpentDeadlinePublishesNothing(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn := client.Conn
	const subject = "notifications.team.spent-window"
	published := 0
	publish := func(msg *nats.Msg) error {
		published++
		return conn.PublishMsg(msg)
	}

	err = client.publishConfirmed(conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, time.Now().Add(-time.Second), publish)
	if want := `bus: window elapsed before the publish to "` + subject + `" was flushed`; err == nil || err.Error() != want {
		t.Fatalf("publishConfirmed with a spent deadline = %v, want %q", err, want)
	}
	if published != 0 {
		t.Fatalf("publishConfirmed called publish %d times with its deadline already spent, want none", published)
	}

	if err := client.publishConfirmed(conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, time.Now().Add(5*time.Second), publish); err != nil {
		t.Fatalf("publishConfirmed with a deadline ahead: %v", err)
	}
	if published != 1 {
		t.Fatalf("publishConfirmed with a deadline ahead called publish %d times, want 1", published)
	}
}
