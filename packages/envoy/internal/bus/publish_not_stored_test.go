package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// On a three-copy stream with a byte limit (in-process three-server cluster), a Dispatch envelope,
// which publishes under a MsgId as a GitHub delivery does, that the full stream refused through
// Publish, and one refused through a window's send, are never reported sent while the stream does
// not hold them. nats-server v2.10.29 records the id of a refused message at sequence 0 and
// acknowledges every retry of it a duplicate with no sequence: the retry is ErrNotStored, which is
// no ErrRefused, on the full stream and after a purge made room, since the record stays on the
// stream's leader. Once the leader has changed, the retry is stored, at a sequence of its own,
// with its MsgId.
func TestARetryOfAPublishTheFullClusterStreamRefusedFailsUntilTheStreamStoresIt(t *testing.T) {
	const limit = 16 << 10
	cluster := testnats.StartCluster(t, 3)
	client := startOnCluster(t, cluster, WithReplicas(3), WithStreamMaxBytes(limit))
	js := cluster.JetStream(t)
	testnats.AwaitSettledStream(t, js, Stream)
	envelope := func(i int) contracts.Envelope {
		return contracts.Envelope{
			EventID:        fmt.Sprintf("event-%d", i),
			Source:         "dispatch",
			Topic:          fmt.Sprintf("notifications.agent.s%d", i),
			DedupeKey:      fmt.Sprintf("not-stored.%d", i),
			PayloadSummary: strings.Repeat("x", 1<<10),
		}
	}
	if !contracts.DedupeKeyNamesTheUpstreamEvent(envelope(0)) {
		t.Fatal("the test's envelope publishes under no MsgId")
	}
	const most = 64
	refused := 0
	for ; refused < most; refused++ {
		err := client.Publish(envelope(refused))
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrStreamFull) {
			t.Fatalf("publish %d = %v, want nil or ErrStreamFull", refused, err)
		}
		break
	}
	if refused == 0 || refused == most {
		t.Fatalf("%d of %d publishes stored within %d bytes, want the stream to fill part way", refused, most, limit)
	}
	published, sent := envelope(refused), envelope(refused+1)
	if err := <-client.Window().PublishAsync(sent); !errors.Is(err, ErrStreamFull) {
		t.Fatalf("a window's send into the full stream = %v, want ErrStreamFull", err)
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
	// retry publishes the refused envelope again through Publish and the sent one through a new
	// window, and returns each answer, failing the test on a success the stream does not hold and
	// on a refusal.
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
	if !errors.Is(publishErr, ErrNotStored) || !errors.Is(sendErr, ErrNotStored) {
		t.Fatalf("on the full stream, retries = %v and %v, want ErrNotStored", publishErr, sendErr)
	}
	if err := js.PurgeStream(Stream); err != nil {
		t.Fatalf("purge the stream: %v", err)
	}
	publishErr, sendErr = retry("after a purge")
	if !errors.Is(publishErr, ErrNotStored) || !errors.Is(sendErr, ErrNotStored) {
		t.Fatalf("after a purge, retries = %v and %v, want ErrNotStored", publishErr, sendErr)
	}

	leader := testnats.AwaitSettledStream(t, js, Stream).Cluster.Leader
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	answer, err := conn.Request("$JS.API.STREAM.LEADER.STEPDOWN."+Stream, nil, 10*time.Second)
	if err != nil {
		t.Fatalf("step the stream's leader down: %v", err)
	}
	var stepDown struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(answer.Data, &stepDown); err != nil || !stepDown.Success {
		t.Fatalf("step the stream's leader down = %s, %v; want success", answer.Data, err)
	}
	testnats.AwaitStreamLeader(t, js, Stream, leader)
	testnats.AwaitSettledStream(t, js, Stream)

	if publishErr, sendErr = retry("once the leader changed"); publishErr != nil || sendErr != nil {
		t.Fatalf("once the leader changed, retries = %v and %v, want both stored", publishErr, sendErr)
	}
	for _, item := range []contracts.Envelope{published, sent} {
		want, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		msg := held(item)
		if msg.Sequence == 0 || string(msg.Data) != string(want) || msg.Header.Get(nats.MsgIdHdr) != item.DedupeKey+":"+item.Topic {
			t.Fatalf("stored %s = sequence %d, %s, id %q; want a sequence, %s and id %q", item.Topic, msg.Sequence, msg.Data, msg.Header.Get(nats.MsgIdHdr), want, item.DedupeKey+":"+item.Topic)
		}
	}
}
