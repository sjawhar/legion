package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// githubDelivery is the envelope the webhook publishes for a push to branch under GitHub's
// delivery id, the one a real delivery carries a Nats-Msg-Id of.
func githubDelivery(t *testing.T, id, branch string) contracts.Envelope {
	t.Helper()
	var body map[string]any
	push := `{"ref":"refs/heads/` + branch + `","repository":{"name":"legion","owner":{"login":"sjawhar"},"full_name":"sjawhar/legion"}}`
	if err := json.Unmarshal([]byte(push), &body); err != nil {
		t.Fatal(err)
	}
	items := contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
		Event: "push", Delivery: id, Body: body, EventID: "event-" + id, TraceID: "trace-" + id,
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("a push to %s makes %d envelopes, want 1", branch, len(items))
	}
	if !contracts.DedupeKeyNamesTheUpstreamEvent(items[0]) {
		t.Fatalf("the delivery %s publishes under no MsgId", id)
	}
	return items[0]
}

// On a three-copy stream with a byte limit (in-process three-server cluster), a GitHub delivery,
// built as the webhook builds it and so published under its Nats-Msg-Id, that the full stream
// refused through Publish, and one refused through a window's send, are stored once the stream has
// room, with no change of the stream's leader. The server this module links, nats-server v2.15.2,
// keeps no record of the id of a message its full stream refused, so every retry while the stream
// is still full is refused the same way, with ErrStreamFull and no ErrRefused, and the retry after
// a purge is stored under its own id. A hub below v2.15 answers such a retry with a duplicate
// carrying no sequence instead (ErrNotStored), which the bus answers by sending the envelope once
// more without its id; that path is
// TestADuplicateAcknowledgementOfAMessageTheStreamDoesNotHoldSendsTheEnvelopeAgain's.
func TestARetryOfAGitHubDeliveryTheFullClusterStreamRefusedIsStoredOnceTheStreamHasRoom(t *testing.T) {
	const limit = 16 << 10
	cluster := testnats.StartCluster(t, 3)
	client := startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(limit))
	js := cluster.JetStream(t)
	testnats.AwaitSettledStream(t, js, Stream)

	published, sent := githubDelivery(t, "d-published", "published"), githubDelivery(t, "d-sent", "sent")
	if published.Topic == sent.Topic {
		t.Fatalf("both deliveries publish to %s, want a topic each", published.Topic)
	}

	const most = 1024
	filled := 0
	for ; filled < most; filled++ {
		err := client.Publish(contracts.Envelope{
			EventID: fmt.Sprintf("fill-%d", filled), Source: "dispatch",
			Topic: fmt.Sprintf("notifications.agent.s%d", filled), DedupeKey: fmt.Sprintf("fill.%d", filled),
		})
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrStreamFull) {
			t.Fatalf("fill publish %d = %v, want nil or ErrStreamFull", filled, err)
		}
		break
	}
	if filled == 0 || filled == most {
		t.Fatalf("%d of %d fill publishes stored within %d bytes, want the stream to fill part way", filled, most, limit)
	}
	if err := client.Publish(published); !errors.Is(err, ErrStreamFull) {
		t.Fatalf("a publish of the delivery into the full stream = %v, want ErrStreamFull", err)
	}
	if err := <-client.Window().PublishAsync(sent); !errors.Is(err, ErrStreamFull) {
		t.Fatalf("a window's send of the delivery into the full stream = %v, want ErrStreamFull", err)
	}

	// held reads item's message from the stream, nil when the stream holds none on its topic.
	held := func(item contracts.Envelope) *nats.RawStreamMsg {
		t.Helper()
		msg, err := js.GetLastMsg(Stream, item.Topic)
		if errors.Is(err, nats.ErrMsgNotFound) {
			return nil
		}
		if err != nil {
			t.Fatalf("read %s: %v", item.Topic, err)
		}
		return msg
	}
	// retry publishes the refused delivery again through Publish and the sent one through a new
	// window, as GitHub's redeliveries and Dispatch's outbox retries do, and returns each answer,
	// failing the test on a success the stream does not hold and on a refusal.
	retry := func(when string) (publishErr, sendErr error) {
		t.Helper()
		publishErr = client.Publish(published)
		sendErr = <-client.Window().PublishAsync(sent)
		for _, answer := range []struct {
			how  string
			item contracts.Envelope
			err  error
		}{{"Publish", published, publishErr}, {"a window's send", sent, sendErr}} {
			t.Logf("%s, a retry through %s: %v", when, answer.how, answer.err)
			switch {
			case answer.err == nil && held(answer.item) == nil:
				t.Fatalf("%s, a retry through %s succeeded and the stream does not hold its message", when, answer.how)
			case errors.Is(answer.err, ErrRefused):
				t.Fatalf("%s, a retry through %s = %v, want a failure to retry", when, answer.how, answer.err)
			}
		}
		return publishErr, sendErr
	}

	publishErr, sendErr := retry("on the full stream")
	for _, err := range []error{publishErr, sendErr} {
		if !errors.Is(err, ErrStreamFull) || errors.Is(err, ErrNotStored) {
			t.Fatalf("on the full stream, retries = %v and %v, want ErrStreamFull and no ErrNotStored", publishErr, sendErr)
		}
	}
	if held(published) != nil || held(sent) != nil {
		t.Fatal("on the full stream, the stream holds a refused delivery")
	}
	if err := js.PurgeStream(Stream); err != nil {
		t.Fatalf("purge the stream: %v", err)
	}
	if publishErr, sendErr = retry("after a purge"); publishErr != nil || sendErr != nil {
		t.Fatalf("after a purge, retries = %v and %v, want both stored", publishErr, sendErr)
	}
	for _, item := range []contracts.Envelope{published, sent} {
		want, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		id := ForwardMsgID(item.DedupeKey, item.Topic)
		msg := held(item)
		if msg.Sequence == 0 || string(msg.Data) != string(want) || msg.Header.Get(nats.MsgIdHdr) != id {
			t.Fatalf("stored %s = sequence %d, %s, id %q; want a sequence, %s and the id %q", item.Topic, msg.Sequence, msg.Data, msg.Header.Get(nats.MsgIdHdr), want, id)
		}
	}
}

// notStoredJetStream answers one publish of each path -- Publish and a window's PublishMsgAsync --
// with the acknowledgement a nats-server below v2.15 gives a retry under the id of a message its
// full stream refused: a duplicate carrying no sequence, which is ErrNotStored. It answers that
// only once armed and only once per path, so the resend the bus makes without the id, and every
// publish before the arm, reaches the real server and is answered by it.
type notStoredJetStream struct {
	nats.JetStreamContext

	mu           sync.Mutex
	publishArmed bool
	sendArmed    bool
}

// arm has the next publish of each path answered with the stubbed acknowledgement.
func (js *notStoredJetStream) arm() {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.publishArmed, js.sendArmed = true, true
}

// fire reports whether this publish is the armed one of its path, disarming it.
func (js *notStoredJetStream) fire(armed *bool) bool {
	js.mu.Lock()
	defer js.mu.Unlock()
	if !*armed {
		return false
	}
	*armed = false
	return true
}

func (js *notStoredJetStream) Publish(subject string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error) {
	if js.fire(&js.publishArmed) {
		return &nats.PubAck{Stream: Stream, Duplicate: true}, nil
	}
	return js.JetStreamContext.Publish(subject, data, opts...)
}

func (js *notStoredJetStream) PublishMsgAsync(msg *nats.Msg, opts ...nats.PubOpt) (nats.PubAckFuture, error) {
	if js.fire(&js.sendArmed) {
		acknowledged := make(chan *nats.PubAck, 1)
		acknowledged <- &nats.PubAck{Stream: Stream, Duplicate: true}
		return duplicateWithNoSequence{acknowledged: acknowledged, msg: msg}, nil
	}
	return js.JetStreamContext.PublishMsgAsync(msg, opts...)
}

// duplicateWithNoSequence is notStoredJetStream's answer to a window's send: the stubbed
// acknowledgement, waiting on Ok(). Err() never answers, as it does not for a send the server
// acknowledged.
type duplicateWithNoSequence struct {
	acknowledged chan *nats.PubAck
	msg          *nats.Msg
}

func (f duplicateWithNoSequence) Ok() <-chan *nats.PubAck { return f.acknowledged }

func (f duplicateWithNoSequence) Err() <-chan error { return nil }

func (f duplicateWithNoSequence) Msg() *nats.Msg { return f.msg }

// A hub below v2.15 records the id of a publish its full stream refused and acknowledges every
// retry under that id a duplicate with no sequence, for a message it does not hold (ErrNotStored).
// Publish and a window's send answer that acknowledgement by sending the same envelope once more
// without its Nats-Msg-Id, which no duplicate record matches: with room in the stream the resend is
// stored, at a sequence of its own and with no id, and the publisher is answered success; with the
// stream still full the resend is refused too and the publisher is answered an error wrapping both
// ErrNotStored and ErrStreamFull, never a success. The server this module links, v2.15.2, gives no
// such acknowledgement (TestARetryOfAGitHubDeliveryTheFullClusterStreamRefusedIsStoredOnceTheStreamHasRoom),
// so the acknowledgement here is stubbed (notStoredJetStream) while the resend it provokes goes to
// a real single server and the stream's own answer to it is the server's.
func TestADuplicateAcknowledgementOfAMessageTheStreamDoesNotHoldSendsTheEnvelopeAgain(t *testing.T) {
	const limit = 16 << 10
	// armed starts a client on a single server whose stream carries the byte limit, and puts the
	// stubbed acknowledgement in front of its JetStream context, returning both it and the
	// server's own context to read the stream through.
	armed := func(t *testing.T) (*Client, *notStoredJetStream, nats.JetStreamContext) {
		t.Helper()
		client, err := ConnectOwningStream([]string{testnats.URL(t)}, WithStreamMaxBytes(limit))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(client.Close)
		server := client.JS()
		stub := &notStoredJetStream{JetStreamContext: server}
		client.mu.Lock()
		client.js = stub
		client.mu.Unlock()
		return client, stub, server
	}

	t.Run("with room in the stream the envelope is stored without its id", func(t *testing.T) {
		client, stub, server := armed(t)
		published, sent := githubDelivery(t, "d-room-publish", "room-publish"), githubDelivery(t, "d-room-send", "room-send")
		stub.arm()
		if err := client.Publish(published); err != nil {
			t.Fatalf("a publish acknowledged a duplicate the stream does not hold = %v, want the resend stored", err)
		}
		if err := <-client.Window().PublishAsync(sent); err != nil {
			t.Fatalf("a window's send acknowledged a duplicate the stream does not hold = %v, want the resend stored", err)
		}
		for _, item := range []contracts.Envelope{published, sent} {
			want, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := server.GetLastMsg(Stream, item.Topic)
			if err != nil {
				t.Fatalf("read %s: %v", item.Topic, err)
			}
			if msg.Sequence == 0 || string(msg.Data) != string(want) || msg.Header.Get(nats.MsgIdHdr) != "" {
				t.Fatalf("stored %s = sequence %d, %s, id %q; want a sequence, %s and no id", item.Topic, msg.Sequence, msg.Data, msg.Header.Get(nats.MsgIdHdr), want)
			}
		}
	})

	t.Run("on a full stream the resend is refused too", func(t *testing.T) {
		client, stub, server := armed(t)
		published, sent := githubDelivery(t, "d-full-publish", "full-publish"), githubDelivery(t, "d-full-send", "full-send")
		const most = 1024
		filled := 0
		for ; filled < most; filled++ {
			err := client.Publish(contracts.Envelope{
				EventID: fmt.Sprintf("fill-%d", filled), Source: "dispatch",
				Topic: fmt.Sprintf("notifications.agent.s%d", filled), DedupeKey: fmt.Sprintf("fill.%d", filled),
			})
			if err == nil {
				continue
			}
			if !errors.Is(err, ErrStreamFull) {
				t.Fatalf("fill publish %d = %v, want nil or ErrStreamFull", filled, err)
			}
			break
		}
		if filled == 0 || filled == most {
			t.Fatalf("%d of %d fill publishes stored within %d bytes, want the stream to fill part way", filled, most, limit)
		}
		// The refusal a hub below v2.15 would record each delivery's id at sequence 0 for.
		if err := client.Publish(published); !errors.Is(err, ErrStreamFull) {
			t.Fatalf("a publish of the delivery into the full stream = %v, want ErrStreamFull", err)
		}
		if err := <-client.Window().PublishAsync(sent); !errors.Is(err, ErrStreamFull) {
			t.Fatalf("a window's send of the delivery into the full stream = %v, want ErrStreamFull", err)
		}
		stub.arm()
		publishErr := client.Publish(published)
		sendErr := <-client.Window().PublishAsync(sent)
		for _, answer := range []struct {
			how  string
			item contracts.Envelope
			err  error
		}{{"Publish", published, publishErr}, {"a window's send", sent, sendErr}} {
			t.Logf("a retry through %s on the full stream: %v", answer.how, answer.err)
			if !errors.Is(answer.err, ErrNotStored) || !errors.Is(answer.err, ErrStreamFull) || errors.Is(answer.err, ErrRefused) {
				t.Fatalf("a retry through %s = %v, want ErrNotStored and ErrStreamFull and no ErrRefused", answer.how, answer.err)
			}
			if _, err := server.GetLastMsg(Stream, answer.item.Topic); !errors.Is(err, nats.ErrMsgNotFound) {
				t.Fatalf("the full stream holds %s after a refused resend (%v)", answer.item.Topic, err)
			}
		}
	})
}
