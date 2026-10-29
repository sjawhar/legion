package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// On a three-copy stream with a byte limit (in-process three-server cluster), a GitHub delivery,
// built as the webhook builds it and so published under its Nats-Msg-Id, that the full stream
// refused through Publish, and one refused through a window's send, are stored once the stream has
// room, with no change of the stream's leader. nats-server v2.10.29 records the id of a refused
// message at sequence 0 on the stream's leader, a purge notwithstanding, and acknowledges every
// retry under it a duplicate with no sequence (ErrNotStored); the bus then sends the envelope once
// more without the id. While the stream is still full, that send is refused too, and the retry
// fails with ErrNotStored and ErrStreamFull, no ErrRefused; after a purge it is stored, at a
// sequence of its own, without the id.
func TestARetryOfAGitHubDeliveryTheFullClusterStreamRefusedIsStoredOnceTheStreamHasRoom(t *testing.T) {
	const limit = 16 << 10
	cluster := testnats.StartCluster(t, 3)
	client := startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(limit))
	js := cluster.JetStream(t)
	testnats.AwaitSettledStream(t, js, Stream)

	// delivery is the envelope the webhook publishes for a push to branch, GitHub's delivery id.
	delivery := func(id, branch string) contracts.Envelope {
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
	published, sent := delivery("d-published", "published"), delivery("d-sent", "sent")
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
		if !errors.Is(err, ErrNotStored) || !errors.Is(err, ErrStreamFull) {
			t.Fatalf("on the full stream, retries = %v and %v, want ErrNotStored and ErrStreamFull", publishErr, sendErr)
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
		msg := held(item)
		if msg.Sequence == 0 || string(msg.Data) != string(want) || msg.Header.Get(nats.MsgIdHdr) != "" {
			t.Fatalf("stored %s = sequence %d, %s, id %q; want a sequence, %s and no id", item.Topic, msg.Sequence, msg.Data, msg.Header.Get(nats.MsgIdHdr), want)
		}
	}
}
