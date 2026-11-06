package bus

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A role forward to a holder with a serving port is a window's Request to the holder's agent
// subject, under the forward's ForwardMsgID and with AcceptStreamAck, which only the stream
// answers. On a three-copy stream with a byte limit (in-process three-server cluster), a full
// stream refuses such a forward, which answers ErrNotAcknowledged and leaves the stream holding
// nothing of it. Once the stream has room, the forward sent again under the same id is stored on
// the holder's subject, exactly once: a further send under that id leaves that one copy and adds
// no second. A window whose deadline has passed writes nothing at all: its forward answers
// ErrWindowDeadline before anything is sent, and the stream holds nothing of it.
func TestAForwardRequestTheFullClusterStreamRefusedIsStoredOnceTheStreamHasRoom(t *testing.T) {
	const limit = 16 << 10
	cluster := testnats.StartCluster(t, 3)
	client := startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(limit))
	js := cluster.JetStream(t)
	testnats.AwaitSettledStream(t, js, Stream)

	// forward is the envelope the outbox sends a role's holder, its topic the role's whatever
	// subject it goes to.
	forward := func(name string) contracts.Envelope {
		return contracts.Envelope{
			EventID:       "evt-" + name,
			Source:        "dispatch",
			SourceEventID: "src-" + name,
			Topic:         contracts.RoleTopicPrefix + "request-not-stored",
			DedupeKey:     "envoy.role.forward.dispatch-" + name,
			IssuedAt:      contracts.NowMillis(),
		}
	}
	// fill publishes until the stream refuses a message, as a busy day's deliveries do.
	fill := func(round int) {
		t.Helper()
		const most = 1024
		for filled := 0; ; filled++ {
			if filled == most {
				t.Fatalf("round %d: %d fill publishes all stored within %d bytes, want the stream full", round, most, limit)
			}
			err := client.Publish(contracts.Envelope{
				EventID:   fmt.Sprintf("fill-%d-%d", round, filled),
				Source:    "dispatch",
				Topic:     fmt.Sprintf("notifications.agent.ses_fill_%d_%d", round, filled),
				DedupeKey: fmt.Sprintf("fill.%d.%d", round, filled),
			})
			if errors.Is(err, ErrStreamFull) {
				return
			}
			if err != nil {
				t.Fatalf("round %d fill publish %d = %v, want nil or ErrStreamFull", round, filled, err)
			}
		}
	}
	// held is how many messages the stream holds on subject.
	held := func(subject string) uint64 {
		t.Helper()
		info, err := js.StreamInfo(Stream, &nats.StreamInfoRequest{SubjectsFilter: subject})
		if err != nil {
			t.Fatalf("read the stream: %v", err)
		}
		return info.State.Subjects[subject]
	}
	// answer waits for outcome's answer, far longer than any request's own timeout here.
	answer := func(outcome <-chan error) error {
		t.Helper()
		select {
		case err := <-outcome:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("no answer within 30 s")
			return nil
		}
	}
	// refused fills the stream, forwards item to holder under opts, which the full stream refuses
	// with an error acknowledgement that answers nothing, and purges the stream, leaving the
	// stream with room to store the forward.
	refused := func(round int, holder string, item contracts.Envelope, opts RequestOptions) {
		t.Helper()
		fill(round)
		err := answer(client.WindowUntil(nil).Request(holder, item, opts))
		if !errors.Is(err, ErrNotAcknowledged) {
			t.Fatalf("round %d: a forward into the full stream = %v, want ErrNotAcknowledged", round, err)
		}
		if n := held(holder); n != 0 {
			t.Fatalf("round %d: the full stream holds %d copies on %s, want none", round, n, holder)
		}
		if err := js.PurgeStream(Stream); err != nil {
			t.Fatalf("round %d: purge the stream: %v", round, err)
		}
	}

	holder := contracts.AgentSubject("ses_forward_stored")
	item := forward("stored")
	opts := RequestOptions{MsgID: ForwardMsgID(item.DedupeKey, holder), AcceptStreamAck: true, Timeout: 2 * time.Second}
	refused(1, holder, item, opts)
	if err := answer(client.WindowUntil(nil).Request(holder, item, opts)); err != nil {
		t.Fatalf("the forward retried once the stream had room = %v, want nil", err)
	}
	if n := held(holder); n != 1 {
		t.Fatalf("the retried forward answered nil and the stream holds %d copies on %s, want 1", n, holder)
	}

	// The same forward sent again is stored once: the stream answers under the id from the copy it
	// already holds and stores no second one.
	if err := answer(client.WindowUntil(nil).Request(holder, item, opts)); err != nil {
		t.Fatalf("the forward sent again once the stream held it = %v, want nil", err)
	}
	if n := held(holder); n != 1 {
		t.Fatalf("the forward sent again left %d copies on %s, want 1", n, holder)
	}

	fencedHolder := contracts.AgentSubject("ses_forward_fenced")
	fenced := forward("fenced")
	fencedOpts := RequestOptions{MsgID: ForwardMsgID(fenced.DedupeKey, fencedHolder), AcceptStreamAck: true, Timeout: 5 * time.Second}
	fencedWindow := client.WindowUntil(func() time.Time { return time.Now().Add(-time.Second) })
	if err := answer(fencedWindow.Request(fencedHolder, fenced, fencedOpts)); !errors.Is(err, ErrWindowDeadline) {
		t.Fatalf("a forward through a window whose deadline has passed = %v, want ErrWindowDeadline", err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := held(fencedHolder); n != 0 {
		t.Fatalf("the stream holds %d copies on %s of the forward the window's deadline fenced, want none", n, fencedHolder)
	}
}
