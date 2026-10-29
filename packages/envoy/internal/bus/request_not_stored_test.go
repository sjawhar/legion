package bus

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A role forward to a holder with a serving port is a window's Request to the holder's agent
// subject, under the forward's ForwardMsgID and with AcceptStreamAck, which only the stream
// answers. On a three-copy stream with a byte limit (in-process three-server cluster), a full
// stream refuses such a forward and records its id at sequence 0 on the stream's leader, a purge
// notwithstanding, so every later forward under the id is acknowledged a duplicate with no
// sequence though nothing is stored (ErrNotStored). The request is then sent once more without its
// id, which that record does not match: once the stream has room it is stored on the holder's
// subject and the forward answers nil, where without that send the duplicate would answer nothing
// and every retry would run out of time. A window whose deadline passes before the duplicate's
// acknowledgement comes back sends nothing more: the forward answers ErrNotStored and
// ErrWindowDeadline, and the stream holds nothing of it.
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
	// leader holding the forward's id at sequence 0 and room to store the forward.
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

	fencedHolder := contracts.AgentSubject("ses_forward_fenced")
	fenced := forward("fenced")
	fencedOpts := RequestOptions{MsgID: ForwardMsgID(fenced.DedupeKey, fencedHolder), AcceptStreamAck: true, Timeout: 5 * time.Second}
	refused(2, fencedHolder, fenced, fencedOpts)
	var deadline atomic.Int64
	deadline.Store(time.Now().Add(time.Hour).UnixNano())
	window := client.WindowUntil(func() time.Time { return time.Unix(0, deadline.Load()) })
	outcome := window.Request(fencedHolder, fenced, fencedOpts)
	// The request has left; the deadline passes before the duplicate's acknowledgement is in.
	deadline.Store(time.Now().Add(-time.Second).UnixNano())
	err := answer(outcome)
	if !errors.Is(err, ErrNotStored) || !errors.Is(err, ErrWindowDeadline) {
		t.Fatalf("a not-stored forward whose window's deadline passed = %v, want ErrNotStored and ErrWindowDeadline", err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := held(fencedHolder); n != 0 {
		t.Fatalf("the stream holds %d copies on %s of the forward the window did not send again, want none", n, fencedHolder)
	}
}
