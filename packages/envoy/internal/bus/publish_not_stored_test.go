package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// On a three-copy stream with a byte limit (in-process three-server cluster), a GitHub delivery,
// built as the webhook builds it and so published under its Nats-Msg-Id, that the full stream
// refused through Publish, and one refused through a window's send, are stored once the stream
// has room, with no change of the stream's leader. While the stream is still full, the retry is
// refused again with ErrStreamFull and no ErrRefused, so its caller retries it, and the stream
// holds nothing of it. After a purge each is stored, at a sequence of its own and under its id,
// exactly once: sent again under that id, each is answered from the copy the stream holds and no
// second copy is stored.
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
	if err := <-client.WindowUntil(nil).PublishAsync(sent); !errors.Is(err, ErrStreamFull) {
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
	// copies is how many messages the stream holds on item's topic.
	copies := func(item contracts.Envelope) uint64 {
		t.Helper()
		info, err := js.StreamInfo(Stream, &nats.StreamInfoRequest{SubjectsFilter: item.Topic})
		if err != nil {
			t.Fatalf("read the stream: %v", err)
		}
		return info.State.Subjects[item.Topic]
	}
	// retry publishes the refused delivery again through Publish and the sent one through a new
	// window, as GitHub's redeliveries and Dispatch's outbox retries do, and returns each answer,
	// failing the test on a success the stream does not hold and on a refusal.
	retry := func(when string) (publishErr, sendErr error) {
		t.Helper()
		publishErr = client.Publish(published)
		sendErr = <-client.WindowUntil(nil).PublishAsync(sent)
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
		if !errors.Is(err, ErrStreamFull) {
			t.Fatalf("on the full stream, retries = %v and %v, want ErrStreamFull", publishErr, sendErr)
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
		msg, id := held(item), ForwardMsgID(item.DedupeKey, item.Topic)
		if msg.Sequence == 0 || string(msg.Data) != string(want) || msg.Header.Get(nats.MsgIdHdr) != id {
			t.Fatalf("stored %s = sequence %d, %s, id %q; want a sequence, %s and the id %q", item.Topic, msg.Sequence, msg.Data, msg.Header.Get(nats.MsgIdHdr), want, id)
		}
		if n := copies(item); n != 1 {
			t.Fatalf("the stream holds %d copies on %s after the purge, want 1", n, item.Topic)
		}
	}
	if publishErr, sendErr = retry("sent again"); publishErr != nil || sendErr != nil {
		t.Fatalf("sent again under their ids, retries = %v and %v, want both answered from the stored copy", publishErr, sendErr)
	}
	for _, item := range []contracts.Envelope{published, sent} {
		if n := copies(item); n != 1 {
			t.Fatalf("the stream holds %d copies on %s after it was sent again, want 1", n, item.Topic)
		}
	}
}

// A window's send the full stream refused is stored exactly once, by a window whose deadline is
// ahead, once the stream has room: sent again under its id, it leaves that one copy. A window
// whose deadline has passed writes nothing at all - the send is answered ErrWindowDeadline before
// anything is sent, and the stream holds nothing of it.
func TestAWindowSendTheFullStreamRefusedIsStoredOnceAndAPassedDeadlineSendsNothing(t *testing.T) {
	const limit = 16 << 10
	cluster := testnats.StartCluster(t, 3)
	client := startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(limit))
	js := cluster.JetStream(t)
	testnats.AwaitSettledStream(t, js, Stream)

	item := contracts.Envelope{
		EventID: "event-fenced", Source: "dispatch", SourceEventID: "fenced",
		Topic: "notifications.dispatch.issue.T-1.comment.created", DedupeKey: "dispatch.event.fenced",
	}
	if !contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		t.Fatal("the envelope publishes under no MsgId")
	}
	for filled := 0; ; filled++ {
		if filled == 1024 {
			t.Fatal("1024 fill publishes all stored; want the stream full")
		}
		err := client.Publish(contracts.Envelope{
			EventID: fmt.Sprintf("fill-%d", filled), Source: "dispatch",
			Topic: fmt.Sprintf("notifications.agent.s%d", filled), DedupeKey: fmt.Sprintf("fill.%d", filled),
		})
		if errors.Is(err, ErrStreamFull) {
			break
		}
		if err != nil {
			t.Fatalf("fill publish %d = %v, want nil or ErrStreamFull", filled, err)
		}
	}
	// The full stream refuses the envelope and stores nothing of it; a purge then leaves room.
	if err := <-client.WindowUntil(nil).PublishAsync(item); !errors.Is(err, ErrStreamFull) {
		t.Fatalf("a send into the full stream = %v, want ErrStreamFull", err)
	}
	if err := js.PurgeStream(Stream); err != nil {
		t.Fatalf("purge the stream: %v", err)
	}
	// copies is how many messages the stream holds on the envelope's topic.
	copies := func() uint64 {
		t.Helper()
		info, err := js.StreamInfo(Stream, &nats.StreamInfoRequest{SubjectsFilter: item.Topic})
		if err != nil {
			t.Fatalf("read the stream: %v", err)
		}
		return info.State.Subjects[item.Topic]
	}

	passed := client.WindowUntil(func() time.Time { return time.Now().Add(-time.Second) })
	var err error
	select {
	case err = <-passed.PublishAsync(item):
	case <-time.After(10 * time.Second):
		t.Fatal("the send was not answered within 10 s")
	}
	if !errors.Is(err, ErrWindowDeadline) {
		t.Fatalf("a send through a window whose deadline has passed = %v, want ErrWindowDeadline", err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := copies(); n != 0 {
		t.Fatalf("the stream holds %d copies of the envelope the passed deadline fenced, want none", n)
	}

	if err := <-client.WindowUntil(func() time.Time { return time.Now().Add(time.Hour) }).PublishAsync(item); err != nil {
		t.Fatalf("the same send through a window whose deadline is ahead = %v, want it stored", err)
	}
	if n := copies(); n != 1 {
		t.Fatalf("the send through a window whose deadline is ahead left %d copies, want 1", n)
	}
	if err := <-client.WindowUntil(nil).PublishAsync(item); err != nil {
		t.Fatalf("the envelope sent again under its id = %v, want it answered from the stored copy", err)
	}
	if n := copies(); n != 1 {
		t.Fatalf("the envelope sent again under its id left %d copies, want 1", n)
	}
}
