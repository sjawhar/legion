package bus

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
)

// unansweredJetStream is a JetStream context that leaves a start's first stream reads and first
// stream creates unanswered, as a server whose meta leader is changing leaves them: the request
// never reaches the server and the caller is given the deadline its wait ran out with. Once the
// unanswered sends are spent every later request reaches the real context, and the counts say how
// many sends each request took. With refuseCreate set it answers every create with that error
// instead, which is how a row tells a refusal from a lost request without asking a particular
// server version to refuse anything.
type unansweredJetStream struct {
	nats.JetStreamContext
	mu               sync.Mutex
	unansweredReads  int
	unansweredCreate int
	refuseCreate     error
	reads            int
	creates          int
}

// send records one send of a request and reports whether it reaches the server: it does not while
// unanswered sends are left, and each such send spends one.
func (js *unansweredJetStream) send(unanswered, sent *int) bool {
	js.mu.Lock()
	defer js.mu.Unlock()
	*sent++
	if *unanswered > 0 {
		*unanswered--
		return false
	}
	return true
}

func (js *unansweredJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if !js.send(&js.unansweredReads, &js.reads) {
		return nil, context.DeadlineExceeded
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

func (js *unansweredJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if !js.send(&js.unansweredCreate, &js.creates) {
		return nil, context.DeadlineExceeded
	}
	if refusal := js.refusal(); refusal != nil {
		return nil, refusal
	}
	return js.JetStreamContext.AddStream(cfg, opts...)
}

// refusal is the answer the context gives every create in place of the server's, or nil when the
// server answers them.
func (js *unansweredJetStream) refusal() error {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.refuseCreate
}

// sent is how many stream reads and how many creates the start has sent through the context.
func (js *unansweredJetStream) sent() (reads, creates int) {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.reads, js.creates
}

// A start whose stream read draws no answer sends it again and goes on, rather than ending the
// program with the bare deadline of the JetStream context's own wait.
func TestAStreamReadTheServerLeavesUnansweredIsSentAgainAndTheStartGoesOn(t *testing.T) {
	js := streamTestJetStream(t)
	losing := &unansweredJetStream{JetStreamContext: js, unansweredReads: 1}
	cfg := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	if err := ensureStreamWithConfig(losing, &cfg, capsAnUnsetStream); err != nil {
		t.Fatalf("a start whose first stream read drew no answer = %v, want it to go on", err)
	}

	if subjects := deployedStream(t, js).Config.Subjects; !slices.Equal(subjects, cfg.Subjects) {
		t.Fatalf("stream subjects = %v, want %v", subjects, cfg.Subjects)
	}
	if reads, _ := losing.sent(); reads != 2 {
		t.Fatalf("stream reads sent = %d, want 2: the lost one and the one that answered", reads)
	}
}

// A start whose create draws no answer sends it again and goes on. Nothing reached the server the
// first time, so the second send is the create that makes the stream, with its own configuration
// and the id it writes.
func TestACreateTheServerLeavesUnansweredIsSentAgainAndTheStartGoesOn(t *testing.T) {
	js := streamTestJetStream(t)
	losing := &unansweredJetStream{JetStreamContext: js, unansweredCreate: 1}
	cfg := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	if err := ensureStreamWithConfig(losing, &cfg, capsAnUnsetStream); err != nil {
		t.Fatalf("a start whose first create drew no answer = %v, want it to go on", err)
	}

	info := deployedStream(t, js)
	if !slices.Equal(info.Config.Subjects, cfg.Subjects) {
		t.Fatalf("stream subjects = %v, want %v", info.Config.Subjects, cfg.Subjects)
	}
	if id := info.Config.Metadata[StreamIDMetadataKey]; id == "" {
		t.Fatalf("stream metadata = %v, want the %s the create writes", info.Config.Metadata, StreamIDMetadataKey)
	}
	if _, creates := losing.sent(); creates != 2 {
		t.Fatalf("creates sent = %d, want 2: the lost one and the one that made the stream", creates)
	}
}

// A create the server refuses is the server's answer, not a lost one, so the start returns it at
// once and sends it no second time. The refusal is the context's own and not a particular
// server's: which create a server refuses changes between versions (nats-server 2.10.29 refuses a
// three-copy create on one server and 2.15.1 lands it), and what this row holds is that any
// answer ends the sending.
func TestARefusedCreateIsSentOnce(t *testing.T) {
	js := streamTestJetStream(t)
	refusal := &nats.APIError{Code: 400, ErrorCode: 10052, Description: "insufficient resources"}
	refused := &unansweredJetStream{JetStreamContext: js, refuseCreate: refusal}
	cfg := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	err := ensureStreamWithConfig(refused, &cfg, capsAnUnsetStream)
	var apiErr *nats.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.ErrorCode != refusal.ErrorCode {
		t.Fatalf("a start whose create the server refused = %v, want that refusal back", err)
	}
	if _, creates := refused.sent(); creates != 1 {
		t.Fatalf("creates sent = %d, want 1: a refusal is the server's answer and is not sent again", creates)
	}
}

// startSendBound is the most sends of one never-answered JetStream request a start may make
// before it gives up. Each send carries its own ten-second wait, so six is a full minute spent on
// a bus that is not answering, and the deployed listener's start-up probe allows 300 s in all
// (failureThreshold 60 at periodSeconds 5, infra/gcp/testdata/dispatch-listener-pod.golden): past
// that the kubelet kills the process, and the operator reads a killed container rather than the
// start's own fatal line naming the step that could not reach the bus.
const startSendBound = 6

// A request the server never answers is given up on, and the start ends with the deadline rather
// than sending for ever while its port answers /healthz "starting".
//
// The three rows above lose one send each and so pass whatever the bound is: with sentAgain's
// loop made unbounded, or unansweredSends raised to 100, go vet stays clean and all six rows of
// this change stay green. This row is the one that holds the giving up itself, in both
// directions - a start that sends past the bound never reaches the deadline below.
func TestAStreamReadTheServerNeverAnswersIsGivenUpOn(t *testing.T) {
	js := streamTestJetStream(t)
	lost := &unansweredJetStream{JetStreamContext: js, unansweredReads: startSendBound + 1}
	cfg := parseConnectOptions([]ConnectOption(nil)).streamConfig()

	err := ensureStreamWithConfig(lost, &cfg, capsAnUnsetStream)
	if !unanswered(err) {
		t.Fatalf("a start whose stream read is never answered = %v, want the deadline back: a start that goes on sending writes no fatal line of its own", err)
	}
	if reads, _ := lost.sent(); reads > startSendBound {
		t.Fatalf("stream reads sent = %d, want at most %d: each send carries its own wait, and the start must end inside the listener's start-up probe", reads, startSendBound)
	}
}
