package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

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

// A window's send that the stream acknowledges a duplicate with no sequence (ErrNotStored) is sent
// once more without its id only while the window's deadline has not passed: the resend is a write,
// and a publisher whose hold lapsed while the first answer was on its way writes nothing more. The
// answer then wraps ErrNotStored and ErrWindowDeadline, and the stream holds nothing of it. The
// same envelope through a window whose deadline is still ahead is stored, without its id.
func TestAWindowSendsNoResendOfANotStoredDuplicatePastItsDeadline(t *testing.T) {
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
	// The full stream refuses the envelope and records its id at sequence 0; after a purge, every
	// send under that id is acknowledged a duplicate with no sequence.
	if err := <-client.WindowUntil(nil).PublishAsync(item); !errors.Is(err, ErrStreamFull) {
		t.Fatalf("a send into the full stream = %v, want ErrStreamFull", err)
	}
	if err := js.PurgeStream(Stream); err != nil {
		t.Fatalf("purge the stream: %v", err)
	}
	stored := func() bool {
		t.Helper()
		_, err := js.GetLastMsg(Stream, item.Topic)
		if errors.Is(err, nats.ErrMsgNotFound) {
			return false
		}
		if err != nil {
			t.Fatalf("read %s: %v", item.Topic, err)
		}
		return true
	}

	var deadline atomic.Int64
	deadline.Store(time.Now().Add(time.Hour).UnixNano())
	window := client.WindowUntil(func() time.Time { return time.Unix(0, deadline.Load()) })
	answer := window.PublishAsync(item)
	// The first send has left; the deadline passes before its acknowledgement can come back.
	deadline.Store(time.Now().Add(-time.Second).UnixNano())
	var err error
	select {
	case err = <-answer:
	case <-time.After(10 * time.Second):
		t.Fatal("the send was not answered within 10 s")
	}
	if !errors.Is(err, ErrNotStored) || !errors.Is(err, ErrWindowDeadline) {
		t.Fatalf("a not-stored send whose window's deadline passed = %v, want ErrNotStored and ErrWindowDeadline", err)
	}
	time.Sleep(500 * time.Millisecond)
	if stored() {
		t.Fatal("the stream holds the envelope the window resent past its deadline")
	}

	if err := <-client.WindowUntil(func() time.Time { return time.Now().Add(time.Hour) }).PublishAsync(item); err != nil {
		t.Fatalf("the same send through a window whose deadline is ahead = %v, want it stored", err)
	}
	if !stored() {
		t.Fatal("the send through a window whose deadline is ahead answered nil and the stream does not hold it")
	}
}
