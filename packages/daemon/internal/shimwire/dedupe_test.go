package shimwire

import (
	"reflect"
	"testing"
)

// A step is one frame crossing the shim in one direction, with what the dedupe must do about it:
// for a daemon frame, whether OMP sees it and what goes straight back; for an OMP frame, what is
// written to the daemon after it.
type step struct {
	daemon      Frame
	agent       Frame
	wantForward bool
	wantOut     []Frame
}

func ack(id string) Response { return Response{ID: id, Command: TypePrompt, Success: true} }
func from(id, delivery string) Prompt {
	return Prompt{ID: id, DeliveryID: delivery, Message: "do the work"}
}

// The shim-side delivery dedupe: one logical delivery, its request ids, and the phase it has
// reached (worker-shim.ts:223-231). The daemon retries a delivery it has not seen confirmed;
// the shim answers the retry itself rather than prompting OMP twice.
func TestTheDeliveryDedupeReproducesTheShimsBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{
			// worker-shim.ts:372-377 — a deliveryId the shim has not seen becomes the delivery,
			// and the prompt goes to OMP.
			name:  "a new deliveryId is recorded and forwarded",
			steps: []step{{daemon: from("r1", "d1"), wantForward: true}},
		},
		{
			// :352-354 and :370 — the first prompt is still unanswered, so the retry is
			// swallowed: OMP is not prompted twice, and OMP's own answer will cover both ids.
			name: "a retry while the delivery is still unacked is swallowed silently",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{daemon: from("r2", "d1"), wantForward: false},
			},
		},
		{
			// :263-264 advances to acked; :354-362 answers the retry locally. The phase is not
			// started, so nothing is replayed.
			name: "a retry after OMP acked is answered locally without a second prompt",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{agent: ack("r1")},
				{daemon: from("r2", "d1"), wantForward: false, wantOut: []Frame{ack("r2")}},
			},
		},
		{
			// :275-283 advances to started; :363-366 replays the start, and :365 is the detail
			// that lives only here — the synthetic agent_start carries the deliveryId, which a
			// real OMP agent_start does not.
			name: "a retry after the turn started replays a start carrying the deliveryId",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{agent: ack("r1")},
				{agent: AgentStart{}},
				{
					daemon:      from("r2", "d1"),
					wantForward: false,
					wantOut:     []Frame{ack("r2"), AgentStart{DeliveryID: "d1"}},
				},
			},
		},
		{
			// :367 — the turn is over, so the replay seeds the daemon idle rather than leaving
			// it waiting for an agent_end that already happened.
			name: "a retry after the turn ended replays the end as well",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{agent: ack("r1")},
				{agent: AgentStart{}},
				{agent: AgentEnd{}},
				{
					daemon:      from("r2", "d1"),
					wantForward: false,
					wantOut:     []Frame{ack("r2"), AgentStart{DeliveryID: "d1"}, AgentEnd{}},
				},
			},
		},
		{
			// :271-274 — a chunked agent_end is the one frame big enough to be chunked, and it
			// arrives as rpc_chunk lines the shim never reassembles. It still ends the turn, so
			// the replay preserves the idle outcome.
			name: "a chunked agent_end ends the turn, so the replay still seeds the daemon idle",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{agent: ack("r1")},
				{agent: AgentStart{}},
				{agent: RPCChunk{ChunkID: "agent-end-1", Index: 0, Count: 5, ByteLength: 2 << 20, Data: "eA=="}},
				{
					daemon:      from("r2", "d1"),
					wantForward: false,
					wantOut:     []Frame{ack("r2"), AgentStart{DeliveryID: "d1"}, AgentEnd{}},
				},
			},
		},
		{
			// :260-269 — OMP answers only the request id it was given, but every retry of that
			// delivery is a request the daemon is still waiting on. One answer fans out to all
			// of them, in the order they arrived.
			name: "a duplicate response fans out to every requestId that shared the delivery",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{daemon: from("r2", "d1"), wantForward: false},
				{daemon: from("r3", "d1"), wantForward: false},
				{agent: ack("r1"), wantOut: []Frame{ack("r2"), ack("r3")}},
			},
		},
		{
			// :261-262 — a refusal is not a delivery; the record is dropped, so the same
			// deliveryId arriving again is a fresh prompt for OMP rather than a local answer.
			name: "a refused prompt drops the delivery, and the same id starts over",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{
					agent: Response{ID: "r1", Command: TypePrompt, Success: false, Error: "busy"},
				},
				{daemon: from("r2", "d1"), wantForward: true},
			},
		},
		{
			// :372-377 — the shim tracks one delivery. A new one replaces the record, and the
			// superseded id is no longer a retry to answer.
			name: "a new deliveryId replaces the record",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{agent: ack("r1")},
				{agent: AgentStart{}},
				{agent: AgentEnd{}},
				{daemon: from("r2", "d2"), wantForward: true},
				{daemon: from("r3", "d1"), wantForward: true},
			},
		},
		{
			// :376 records whether a turn was already running when the prompt went out, and
			// :278 only attributes a start to the delivery when it was not. A foreign turn's
			// agent_start never advances someone else's delivery to started, so a retry replays
			// nothing it did not observe.
			name: "a start belonging to a turn already running never advances the delivery",
			steps: []step{
				{agent: AgentStart{}},
				{daemon: from("r1", "d1"), wantForward: true},
				{agent: ack("r1")},
				{agent: AgentEnd{}},
				{agent: AgentStart{}},
				{daemon: from("r2", "d1"), wantForward: false, wantOut: []Frame{ack("r2")}},
			},
		},
		{
			// :347-351 — a prompt without a deliveryId predates the dedupe (a daemon mid-
			// deployment); it is forwarded untouched and never becomes a delivery.
			name: "a prompt without a deliveryId is forwarded untouched",
			steps: []step{
				{daemon: Prompt{ID: "r1", Message: "do the work"}, wantForward: true},
				{agent: ack("r1")},
				{daemon: Prompt{ID: "r2", Message: "do the work"}, wantForward: true},
			},
		},
		{
			// Everything else the daemon sends is a frame for OMP; the dedupe passes it through.
			name: "every other daemon frame is forwarded untouched",
			steps: []step{
				{daemon: GetState{ID: "r1"}, wantForward: true},
				{daemon: NegotiateProtocol{ID: "r2", ProtocolVersion: 2}, wantForward: true},
				{daemon: Raw{Type: "interrupt", JSON: []byte(`{"type":"interrupt"}`)}, wantForward: true},
			},
		},
		{
			// A response to something other than a prompt is not a delivery's answer and fans
			// out to nothing (:255-256).
			name: "a get_state answer is never mistaken for a delivery's answer",
			steps: []step{
				{daemon: from("r1", "d1"), wantForward: true},
				{daemon: from("r2", "d1"), wantForward: false},
				{agent: Response{ID: "r1", Command: TypeGetState, Success: true}},
				{daemon: from("r3", "d1"), wantForward: false},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dedupe Dedupe
			for i, s := range tc.steps {
				if s.daemon != nil {
					forward, replies := dedupe.DaemonFrame(s.daemon)
					if forward != s.wantForward {
						t.Fatalf("step %d: DaemonFrame(%#v) forward = %v, want %v", i, s.daemon, forward, s.wantForward)
					}
					if !sameFrames(replies, s.wantOut) {
						t.Fatalf("step %d: DaemonFrame(%#v) replied %#v, want %#v", i, s.daemon, replies, s.wantOut)
					}
					continue
				}
				extra := dedupe.AgentFrame(s.agent)
				if !sameFrames(extra, s.wantOut) {
					t.Fatalf("step %d: AgentFrame(%#v) wrote %#v, want %#v", i, s.agent, extra, s.wantOut)
				}
			}
		})
	}
}

// The shim reads the daemon's stream and the wrapped process's stdout on two goroutines; the
// delivery record is shared between them, so it is the dedupe's own lock that keeps it whole.
// Under -race, an unguarded state machine fails here.
func TestTheDedupeIsSafeForTheShimsTwoPumps(t *testing.T) {
	var dedupe Dedupe
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 500 {
			dedupe.AgentFrame(ack("r1"))
			dedupe.AgentFrame(AgentStart{})
			if i%2 == 0 {
				dedupe.AgentFrame(AgentEnd{})
			}
		}
	}()
	for range 500 {
		dedupe.DaemonFrame(from("r1", "d1"))
		dedupe.DaemonFrame(from("r2", "d1"))
	}
	<-done
}

func sameFrames(got, want []Frame) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			return false
		}
	}
	return true
}
