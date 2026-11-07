package supervise

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// toolArgs is one call's arguments as Oh My Pi writes them.
const toolArgs = `{"command":"echo hi"}`

// toolArgsHash is the fingerprint the stream measures a start's arguments by
// (stream.stepArgsHash): the machine is handed that number and never the text, so a case that
// stands in for the stream computes it the way the stream does.
func toolArgsHash(args string) string {
	if args == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(args))
	return hex.EncodeToString(sum[:8])
}

// stepped posts one tool frame of the claim's live connection, as the stream reads it.
func (h *harness) stepped(tool, callID string, args string, end bool) {
	h.t.Helper()
	step := StreamToolStep{Claim: h.token, Tool: tool, CallID: callID, End: end, ConnSequence: h.conn.Sequence()}
	if !end {
		step.ArgsBytes, step.ArgsHash = len(args), toolArgsHash(args)
	}
	h.must(step)
}

// The record a call leaves: the tool and the call id its two frames carry, the length and a hash
// of the arguments its start carried, and both times, each stamped when the daemon read the
// frame. The end frame repeats neither the arguments nor the intent, so what stands beside its
// own time is what the start left.
func TestACallsStartAndEndLeaveTheRecordNamingThatCallWithBothTimes(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.advance(time.Second)
	startedAt := h.clock.Now()
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)

	open := h.store.step(h.token)
	if open.Tool != "bash" || open.CallID != "toolu_turn1_0" || open.ArgsBytes != len(toolArgs) || open.ArgsHash == "" {
		t.Fatalf("the record of the open call = %+v, want bash/toolu_turn1_0 with %d bytes measured and hashed", open, len(toolArgs))
	}
	if !open.StartedAt.Equal(startedAt) || !open.EndedAt.IsZero() || !open.At.Equal(startedAt) {
		t.Fatalf("the open call's times = %+v, want it started at %s and still open", open, startedAt)
	}

	h.advance(2 * time.Second)
	endedAt := h.clock.Now()
	h.stepped("bash", "toolu_turn1_0", "", true)

	ended := h.store.step(h.token)
	if ended.Tool != open.Tool || ended.CallID != open.CallID ||
		ended.ArgsBytes != open.ArgsBytes || ended.ArgsHash != open.ArgsHash {
		t.Fatalf("the record at the end = %+v, want the call the start named, with its arguments measured: %+v", ended, open)
	}
	if !ended.StartedAt.Equal(startedAt) || !ended.EndedAt.Equal(endedAt) || !ended.At.Equal(endedAt) {
		t.Fatalf("the ended call's times = %+v, want %s to %s", ended, startedAt, endedAt)
	}
}

// Two calls of one tool in one assistant message come back with byte-identical arguments and
// their ends in either order: the call id is the only thing that pairs an end with its start,
// and each end is written with its own call's start.
func TestTwoConcurrentToolStepsOfOneToolWithIdenticalArgumentsAreToldApart(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.advance(time.Second)
	firstStarted := h.clock.Now()
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	h.advance(time.Second)
	secondStarted := h.clock.Now()
	h.stepped("bash", "toolu_turn1_1", toolArgs, false)

	// The second call ends first, which is the order the frames can arrive in.
	h.advance(time.Second)
	h.stepped("bash", "toolu_turn1_1", "", true)
	second := h.store.step(h.token)
	if second.CallID != "toolu_turn1_1" || !second.StartedAt.Equal(secondStarted) || !second.EndedAt.Equal(h.clock.Now()) {
		t.Fatalf("the record after the second call ended = %+v, want toolu_turn1_1 started at %s", second, secondStarted)
	}

	h.advance(time.Second)
	h.stepped("bash", "toolu_turn1_0", "", true)
	first := h.store.step(h.token)
	if first.CallID != "toolu_turn1_0" || !first.StartedAt.Equal(firstStarted) || !first.EndedAt.Equal(h.clock.Now()) {
		t.Fatalf("the record after the first call ended = %+v, want toolu_turn1_0 started at %s, not the other call's start", first, firstStarted)
	}
	if first.ArgsHash != second.ArgsHash || first.ArgsBytes != second.ArgsBytes {
		t.Fatalf("calls with byte-identical arguments measured differently: %+v and %+v", first, second)
	}
}

// A frame of a connection the claim has replaced, or of one it no longer has: the shim holds
// what it could not send and replays it on the next connection, and the stream's queue can hand
// a machine a frame of a connection that was replaced while the frame waited. Such a frame says
// what the agent was doing and nothing about what it is doing now, so it records its call and
// leaves the claim's last-step time exactly where it was.
func TestAFrameOfADeadIncarnationRecordsItsCallAndMovesNoStepTime(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.advance(time.Second)
	live := h.clock.Now()
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	replaced := h.conn.Sequence()

	// The shim redialed: the directory holds a newer connection for the claim.
	h.conns.Register(h.token, fake.NewConn())
	h.advance(time.Second)
	const replayedArgs = `{"path":"README.md"}`
	h.must(StreamToolStep{Claim: h.token, Tool: "read", CallID: "toolu_turn0_9",
		ArgsBytes: len(replayedArgs), ArgsHash: toolArgsHash(replayedArgs), ConnSequence: replaced})

	replayed := h.store.step(h.token)
	if replayed.CallID != "toolu_turn0_9" || replayed.Tool != "read" || replayed.ArgsBytes == 0 {
		t.Fatalf("the record after the replayed frame = %+v, want the call it names recorded", replayed)
	}
	if !replayed.At.Equal(live) {
		t.Fatalf("the claim's last step moved to %s, want it left at %s: the frame is of a connection that is gone", replayed.At, live)
	}

	// A claim with no connection at all is the same: there is no live incarnation to be of.
	h.conns.Remove(h.token)
	h.advance(time.Second)
	h.must(StreamToolStep{Claim: h.token, Tool: "write", CallID: "toolu_turn0_10", End: true, ConnSequence: replaced})
	if after := h.store.step(h.token); after.CallID != "toolu_turn0_10" || !after.At.Equal(live) {
		t.Fatalf("the record after a frame of no connection = %+v, want the call recorded and the last step still %s", after, live)
	}
}

// A claim whose agent sends no tool frame at all is judged from the start of its turn, which the
// turn's own event writes through the same narrow call: a turn that hangs before its first call
// reads as the turn it is rather than as a claim that has never stepped.
func TestATurnWithNoToolStepKeepsItsStartAsTheLastStep(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.advance(time.Second)
	started := h.clock.Now()
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: h.token})

	step := h.store.step(h.token)
	if !step.TurnStartedAt.Equal(started) {
		t.Fatalf("the turn's start = %s, want %s", step.TurnStartedAt, started)
	}
	if !step.At.IsZero() || step.CallID != "" || step.Tool != "" {
		t.Fatalf("the record of a claim that has stepped none = %+v, want the turn's start and no call", step)
	}
}

// The record is overwritten by the next call and cleared by nothing, a turn's end least of all:
// the recovery it exists for ends the hung process and starts a turn on the replacement, and the
// one fact anybody is looking for is which call was open when the agent stopped.
func TestTheTurnsEndLeavesTheCallTheRecordNames(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.advance(time.Second)
	h.stepped("bash", "toolu_turn1_0", toolArgs, false)
	open := h.store.step(h.token)

	h.must(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	if ended := h.store.step(h.token); ended != open {
		t.Fatalf("the record after the turn ended = %+v, want the open call it named: %+v", ended, open)
	}

	h.advance(time.Second)
	next := h.clock.Now()
	h.must(RequestDeliver{Claim: h.token, Task: "fix the test"})
	h.must(StreamTurnStart{Claim: h.token})
	after := h.store.step(h.token)
	if after.CallID != open.CallID || !after.StartedAt.Equal(open.StartedAt) || !after.EndedAt.IsZero() {
		t.Fatalf("the record after the next turn started = %+v, want the call that was open: %+v", after, open)
	}
	if !after.TurnStartedAt.Equal(next) {
		t.Fatalf("the turn's start = %s, want the new turn's %s", after.TurnStartedAt, next)
	}
}

// The arguments' own text is nowhere: not in a column, not in a log line. A shell command
// routinely holds a credential, and this record reaches every surface an operator reads, so the
// stream hands the machine the length and the fingerprint of a call's arguments and never their
// text (stream.ToolStep), and the machine writes those two and nothing else of them. The case
// stands in for the stream, hashing the arguments the way it does.
func TestACredentialInAToolStepsArgumentsIsInNoColumnAndNoLogLine(t *testing.T) {
	const secret = "sk-live-9d41f0c2a7b34e11"
	h := newHarness(t)
	h.reach(StateWorking)
	args := `{"command":"curl -H 'Authorization: Bearer ` + secret + `' https://example.test"}`
	h.stepped("bash", "toolu_turn1_0", args, false)
	h.advance(time.Second)
	h.stepped("bash", "toolu_turn1_0", "", true)

	step := h.store.step(h.token)
	if step.ArgsBytes != len(args) {
		t.Fatalf("the arguments measured = %d bytes, want %d", step.ArgsBytes, len(args))
	}
	for _, column := range []string{step.Tool, step.CallID, step.ArgsHash} {
		if strings.Contains(column, secret) {
			t.Fatalf("the record carries the secret in %q", column)
		}
	}
	for _, c := range h.store.history() {
		if strings.Contains(c.Step.Tool, secret) || strings.Contains(c.Step.ArgsHash, secret) ||
			strings.Contains(c.Failure, secret) {
			t.Fatalf("a claim written to the store carries the secret: %+v", c.Step)
		}
	}
	for _, line := range h.logs.lines() {
		if strings.Contains(line, secret) {
			t.Fatalf("the daemon logged the secret: %s", line)
		}
	}
}

// Two frames this daemon cannot pair, and what each records. An end whose start it never read —
// a call open across a restart, or a start the shim's backlog dropped — names the call with no
// arguments measured, which is what the daemon knows of it. A start with no call id at all (the
// Claude Code bridge writes one) is a step with no call to name: it moves the claim's last-step
// time and leaves the call the record holds exactly as it stands.
func TestAnUnpairedEndAndACallIdlessStartRecordWhatTheDaemonKnows(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.advance(time.Second)
	ended := h.clock.Now()
	h.stepped("bash", "toolu_turn0_4", "", true)

	orphan := h.store.step(h.token)
	if orphan.CallID != "toolu_turn0_4" || orphan.Tool != "bash" || !orphan.EndedAt.Equal(ended) {
		t.Fatalf("the record of an end with no start = %+v, want the call it names, ended at %s", orphan, ended)
	}
	if orphan.ArgsBytes != 0 || orphan.ArgsHash != "" || !orphan.StartedAt.IsZero() {
		t.Fatalf("the record of an end with no start = %+v, want nothing measured of arguments it never carried", orphan)
	}

	h.advance(time.Second)
	stepped := h.clock.Now()
	h.must(StreamToolStep{Claim: h.token, Tool: "bash", ArgsBytes: len(toolArgs),
		ArgsHash: toolArgsHash(toolArgs), ConnSequence: h.conn.Sequence()})
	nameless := h.store.step(h.token)
	if !nameless.At.Equal(stepped) {
		t.Fatalf("the last step = %s, want the frame's own %s: a step with no call id is still a step", nameless.At, stepped)
	}
	if nameless.CallID != orphan.CallID || !nameless.EndedAt.Equal(orphan.EndedAt) {
		t.Fatalf("the record after a frame with no call id = %+v, want the call it named left alone: %+v", nameless, orphan)
	}
}
