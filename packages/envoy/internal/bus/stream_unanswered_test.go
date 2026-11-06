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
// many sends each request took.
type unansweredJetStream struct {
	nats.JetStreamContext
	mu               sync.Mutex
	unansweredReads  int
	unansweredCreate int
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
	return js.JetStreamContext.AddStream(cfg, opts...)
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

	if err := ensureStreamWithConfig(losing, &cfg); err != nil {
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

	if err := ensureStreamWithConfig(losing, &cfg); err != nil {
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
// once and sends it no second time. A single server refuses a create at three copies.
func TestARefusedCreateIsSentOnce(t *testing.T) {
	js := streamTestJetStream(t)
	refused := &unansweredJetStream{JetStreamContext: js}
	cfg := parseConnectOptions([]ConnectOption{WithReplicas(3)}).streamConfig()

	err := ensureStreamWithConfig(refused, &cfg)
	var apiErr *nats.APIError
	if err == nil || !errors.As(err, &apiErr) {
		t.Fatalf("a start creating a three-copy stream on one server = %v, want the server's refusal", err)
	}
	if _, creates := refused.sent(); creates != 1 {
		t.Fatalf("creates sent = %d, want 1: a refusal is the server's answer and is not sent again", creates)
	}
}
