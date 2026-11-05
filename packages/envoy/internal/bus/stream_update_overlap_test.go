package bus

import (
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Every nats-server before v2.12.5 can refuse the later of two updates of one stream as
// overlapping an existing stream, the stream itself, when the other update lands while the server
// checks this one (updateStream); that is how TestTwoStartsRaisingOneStreamAtOnceBothSucceed
// failed on v2.10.29. Such an update is settled from the stream: one that, read again, holds what
// the caller asked for is done, one that does not is sent again, and one refused on every send, as
// a stream another stream really does overlap is, returns the refusal after overlapSends sends. An
// unanswered update is not sent again, and any other refusal is returned at once.
func TestAnUpdateRefusedAsOverlappingItsOwnStreamIsSettledFromTheStream(t *testing.T) {
	overlap := &nats.APIError{Code: 400, ErrorCode: jsStreamSubjectOverlap, Description: "subjects overlap with an existing stream"}
	otherRefusal := &nats.APIError{Code: 503, ErrorCode: 10023, Description: "insufficient resources"}
	for _, test := range []struct {
		name string
		// the server's answer to each send in turn, nil once the answers run out
		answers []error
		// the other program's update lands with the first answer that is not nil, giving the
		// stream what this update asks for
		landed bool
		sends  int
		reads  int
		want   error
	}{
		{name: "the other update gave the stream what this one asks", answers: []error{overlap}, landed: true, sends: 1, reads: 1},
		{name: "the stream read again does not hold it yet", answers: []error{overlap}, sends: 2, reads: 1},
		{name: "refused, then accepted on the last send", answers: slices.Repeat([]error{overlap}, overlapSends-1), sends: overlapSends, reads: overlapSends - 1},
		{name: "refused on every send", answers: slices.Repeat([]error{overlap}, overlapSends+1), sends: overlapSends, reads: overlapSends, want: overlap},
		{name: "unanswered, and the stream does not hold it", answers: []error{nats.ErrTimeout}, sends: 1, reads: 1, want: nats.ErrTimeout},
		{name: "unanswered, and the stream holds it", answers: []error{nats.ErrTimeout}, landed: true, sends: 1, reads: 1},
		{name: "refused otherwise", answers: []error{otherRefusal}, sends: 1, want: otherRefusal},
		{name: "refused otherwise while the stream holds the update", answers: []error{otherRefusal}, landed: true, sends: 1, want: otherRefusal},
	} {
		t.Run(test.name, func(t *testing.T) {
			js := &scriptedUpdateJetStream{config: nats.StreamConfig{Name: Stream, Replicas: 1}, answers: test.answers, landed: test.landed}
			update := nats.StreamConfig{Name: Stream, Replicas: 3}

			err := updateStream(js, &update, func(again *nats.StreamInfo) bool { return again.Config.Replicas >= 3 })

			if !errors.Is(err, test.want) {
				t.Fatalf("updateStream = %v, want %v", err, test.want)
			}
			if len(js.sent) != test.sends || js.reads != test.reads {
				t.Fatalf("updateStream sent %d updates and read the stream %d times, want %d and %d", len(js.sent), js.reads, test.sends, test.reads)
			}
			for i, sent := range js.sent {
				if !reflect.DeepEqual(sent, update) {
					t.Fatalf("update %d = %+v, want the caller's %+v", i+1, sent, update)
				}
			}
		})
	}
}

// scriptedUpdateJetStream answers each stream update with the next of answers, applying the update
// when that answer is nil, and every read with the stream as it holds it, counting the reads. With
// landed, the other program's update lands with the first answer that is not nil, and it is this
// update itself: the stream then holds what the caller asked for without the caller's own send
// having been applied.
type scriptedUpdateJetStream struct {
	nats.JetStreamContext
	config  nats.StreamConfig
	answers []error
	landed  bool
	sent    []nats.StreamConfig
	reads   int
}

func (js *scriptedUpdateJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.sent = append(js.sent, *cfg)
	var answer error
	if len(js.answers) > 0 {
		answer = js.answers[0]
		js.answers = js.answers[1:]
	}
	if answer == nil {
		js.config = *cfg
		return &nats.StreamInfo{Config: js.config}, nil
	}
	if js.landed {
		js.config = *cfg
		js.landed = false
	}
	return nil, answer
}

func (js *scriptedUpdateJetStream) StreamInfo(string, ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.reads++
	return &nats.StreamInfo{Config: js.config}, nil
}

// On a real three-server cluster, concurrent reconciles of one stream all end with the stream
// holding what each asked for. Three clients, one per member, send the same update at once, round
// after round: on nats-server v2.10.29 a run of these rounds meets the overlap refusal above (one
// in a few hundred sends), and with it settled from the stream no round fails and the stream holds
// every round's byte limit. The rounds are the measurement, so they are many and each is cheap: no
// round restarts the cluster.
func TestConcurrentUpdatesOfOneStreamEndWithTheStreamHoldingThem(t *testing.T) {
	const rounds = 300
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(3))
	deployed := testnats.AwaitStreamCopies(t, js, Stream, 3).Config

	members := make([]nats.JetStreamContext, 0, len(cluster.URLs()))
	for i, url := range cluster.URLs() {
		memberJS, err := testnats.Connect(t, url).JetStream()
		if err != nil {
			t.Fatalf("member %d JetStream: %v", i, err)
		}
		members = append(members, memberJS)
	}

	for round := range rounds {
		update := deployed
		update.MaxBytes = testStreamMaxBytes + int64(round+1)
		errs := make([]error, len(members))
		var wg sync.WaitGroup
		for i, member := range members {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sent := update
				errs[i] = updateStream(member, &sent, func(again *nats.StreamInfo) bool {
					return again.Config.MaxBytes >= update.MaxBytes
				})
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, the update of member %d: %v", round, i, err)
			}
		}
		if got := deployedStream(t, js).Config.MaxBytes; got != update.MaxBytes {
			t.Fatalf("round %d left the stream's byte limit at %d, want the %d every update asked for", round, got, update.MaxBytes)
		}
	}
}
