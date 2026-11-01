package claudebridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// fakeClaudeEnv makes the test binary a stand-in for `claude -p` in stream-json: it records every
// line it reads to FAKE_CLAUDE_RECORD and answers each as fakeClaude's script says.
const fakeClaudeEnv = "LEGION_TEST_FAKE_CLAUDE"

// fakeClaudeGateEnv names the file whose appearance lets a replay write its stream's last line
// (rig.open), so a test can act while the turn that line ends is still running.
const fakeClaudeGateEnv = "FAKE_CLAUDE_GATE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeClaudeEnv) == "1" {
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}

// fakeClaude answers Claude Code's stream-json the way the probes recorded it: each user message
// gets a system/init, then the message re-emitted as --replay-user-messages does (the same message,
// marked isReplay, once the step takes it), then its script's lines, then a result. A user
// message's text picks its script:
//   - "/rename <title>": no init and no re-emission, an assistant line naming the title, a success
//     result;
//   - "handoff ok": a `legion` handoff_complete call and its result;
//   - "say <text>": an assistant message of text;
//   - "held <name>": the re-emission only once the test releases name (rig.release);
//   - "wait <name>": an assistant message "done" only once the test releases name;
//   - "die": exits, never re-emitting it;
//   - "fail <text>": an assistant message of text, then an error_during_execution result;
//   - "maxturns <text>": an assistant message of text, then an error_max_turns result, which is
//     Claude Code's own cap on the steps of one turn rather than the provider refusing it;
//   - "fail-when <name>": a step the provider refused, held until the test opens the gate <name>,
//     so a test can decide what has reached the bridge before the refusal does;
//   - "replay <file>": no init of the fake's own — the lines of testdata/<file>, a recorded Claude
//     Code stream, written as they were recorded, the last held back until the gate opens;
//   - anything else: an assistant message "done".
func fakeClaude() int {
	record, err := os.OpenFile(os.Getenv("FAKE_CLAUDE_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer record.Close()
	fmt.Fprintf(record, "argv %s\n", strings.Join(os.Args[1:], " "))
	fmt.Fprintf(record, "inbox %s\n", os.Getenv(InboxVariable))
	out := json.NewEncoder(os.Stdout)
	emit := func(v any) { _ = out.Encode(v) }
	// released waits for the test to create name beside the record, and takes it.
	released := func(name string) {
		path := filepath.Join(filepath.Dir(os.Getenv("FAKE_CLAUDE_RECORD")), name)
		for os.Remove(path) != nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	assistant := func(blocks ...map[string]any) {
		emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": blocks}})
	}
	text := func(t string) map[string]any { return map[string]any{"type": "text", "text": t} }
	result := func(subtype string, isError bool) {
		emit(map[string]any{"type": "result", "subtype": subtype, "is_error": isError})
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return 0
		}
		fmt.Fprintf(record, "in %s", line)
		var msg struct {
			Message json.RawMessage `json:"message"`
		}
		var user struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(line, &msg) != nil {
			return 3
		}
		if json.Unmarshal(msg.Message, &user) != nil {
			return 3
		}
		body := user.Content[0].Text
		if title, ok := strings.CutPrefix(body, "/rename "); ok {
			assistant(text("Session renamed to: " + title))
			result("success", false)
			continue
		}
		if file, ok := strings.CutPrefix(body, "replay "); ok {
			replay(record, file)
			continue
		}
		emit(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		if name, ok := strings.CutPrefix(body, "held "); ok {
			released(name)
		}
		if body == "die" {
			return 0
		}
		emit(map[string]any{"type": "user", "message": msg.Message, "session_id": "s", "parent_tool_use_id": nil,
			"uuid": "u", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "isReplay": true})
		switch {
		case body == "handoff ok":
			assistant(map[string]any{"type": "tool_use", "id": "tu1", "name": "mcp__legion__legion", "input": map[string]any{"op": "handoff_complete", "summary": "s"}})
			emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tu1"}}}})
			assistant(text("completed"))
		case strings.HasPrefix(body, "say "):
			assistant(text(strings.TrimPrefix(body, "say ")))
		case strings.HasPrefix(body, "fail "):
			assistant(text(strings.TrimPrefix(body, "fail ")))
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
				"errors": []string{"API Error: 529 overloaded"}})
			continue
		case strings.HasPrefix(body, "maxturns "):
			assistant(text(strings.TrimPrefix(body, "maxturns ")))
			emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true,
				"errors": []string{"reached the maximum number of turns"}})
			continue
		case strings.HasPrefix(body, "fail-when "):
			// The step fails only once the test opens the gate. The fake cannot order a delivery
			// against its own result: both reach the bridge through two cases of one select, and
			// a queued delivery emits no frame for this process to see. So the test sends the
			// delivery, waits for the bridge's own log line saying it is queued, and opens the
			// gate; the refusal then lands with the delivery already in the queue, which is the
			// order the case under test is about.
			released(strings.TrimPrefix(body, "fail-when "))
			assistant(text("trying"))
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
				"errors": []string{"API Error: 529 overloaded"}})
			continue
		case strings.HasPrefix(body, "wait "):
			released(strings.TrimPrefix(body, "wait "))
			assistant(text("done"))
		default:
			assistant(text("done"))
		}
		result("success", false)
	}
}

// replay writes a recorded Claude Code stream line for line, holding its last line back until the
// gate opens. The recordings are of the pinned Claude Code (2.1.310, the version
// docker/claude-code/package-lock.json pins) under claudelaunch.Argv's flags of the day, before
// --replay-user-messages was one of them: a line is what that binary wrote, never written by hand.
func replay(record *os.File, file string) {
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		fmt.Fprintln(record, "replay", err)
		return
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for i, line := range lines {
		if i == len(lines)-1 {
			waitForGate(record)
		}
		fmt.Fprintf(os.Stdout, "%s\n", line)
	}
}

// waitForGate waits up to 10s for the test to open the gate, and returns at once when none is set.
func waitForGate(record *os.File) {
	gate := os.Getenv(fakeClaudeGateEnv)
	if gate == "" {
		return
	}
	for range 1000 {
		if _, err := os.Stat(gate); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Fprintln(record, "the gate never opened")
}

// rig runs one bridge over the fake Claude: frames go in through daemon, come out on frames.
type rig struct {
	t       *testing.T
	daemon  *io.PipeWriter
	frames  chan map[string]any
	record  string
	log     *lockedBuffer
	gate    string
	done    chan int
	stopped context.CancelFunc
}

func newRig(t *testing.T, args ...string) *rig {
	t.Helper()
	return newRigWithEnv(t, nil, args...)
}

// newRigWithEnv is newRig with more of the pane's environment than a bare bridge needs: the
// daemon, boot token file and generation a bridge reports its turns with (turnoutcome.go).
func newRigWithEnv(t *testing.T, extra []string, args ...string) *rig {
	t.Helper()
	dir := t.TempDir()
	record, gate := filepath.Join(dir, "record"), filepath.Join(dir, "gate")
	argv := append([]string{os.Args[0]}, args...)
	env := append(os.Environ(), fakeClaudeEnv+"=1", "FAKE_CLAUDE_RECORD="+record, fakeClaudeGateEnv+"="+gate,
		"CLAUDE_CONFIG_DIR="+dir, "LEGION_ROLE=implementer", "LEGION_ISSUE=ACME-7")
	env = append(env, extra...)
	cfg, err := Configure(argv, env)
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	logged := &lockedBuffer{}
	cfg.Stdin, cfg.Stdout, cfg.Log = inR, outW, logged
	r := &rig{t: t, daemon: inW, frames: make(chan map[string]any, 100), record: record, gate: gate,
		log: logged, done: make(chan int, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	r.stopped = cancel
	go func() {
		scanner := bufio.NewScanner(outR)
		scanner.Buffer(nil, 1<<20)
		for scanner.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				t.Errorf("the bridge wrote a line that is not JSON: %q", scanner.Text())
				continue
			}
			r.frames <- frame
		}
	}()
	go func() {
		code, err := Run(ctx, cfg)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		_ = outW.Close()
		r.done <- code
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("the bridge did not end")
		}
	})
	return r
}

func (r *rig) send(frame shimwire.Frame) {
	r.t.Helper()
	line, err := json.Marshal(frame)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.daemon.Write(append(line, '\n')); err != nil {
		r.t.Fatal(err)
	}
}

// nextRaw is the next frame the bridge wrote, pane summaries (tool_execution_start) included.
func (r *rig) nextRaw() map[string]any {
	r.t.Helper()
	select {
	case frame := <-r.frames:
		return frame
	case <-time.After(10 * time.Second):
		r.t.Fatal("the bridge wrote nothing within 10s")
		return nil
	}
}

// next is the next frame the bridge wrote that is not a pane summary (tool_execution_start).
func (r *rig) next() map[string]any {
	r.t.Helper()
	for {
		if frame := r.nextRaw(); frame["type"] != "tool_execution_start" {
			return frame
		}
	}
}

// expect is the next frame, which must be a typ frame carrying every field of want.
func (r *rig) expect(typ string, want map[string]any) map[string]any {
	r.t.Helper()
	frame := r.next()
	if frame["type"] != typ {
		r.t.Fatalf("got %v, want a %s frame", frame, typ)
	}
	for k, v := range want {
		if frame[k] != v {
			r.t.Fatalf("got %v, want %s %v", frame, k, v)
		}
	}
	return frame
}

// quiet holds that the bridge writes no frame within a short while.
func (r *rig) quiet() {
	r.t.Helper()
	select {
	case frame := <-r.frames:
		r.t.Fatalf("got %v, want no frame", frame)
	case <-time.After(300 * time.Millisecond):
	}
}

// open lets a replay write the last line of its recorded stream (fakeClaudeGateEnv).
func (r *rig) open() {
	r.t.Helper()
	if err := os.WriteFile(r.gate, nil, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) recorded() string {
	raw, _ := os.ReadFile(r.record)
	return string(raw)
}

// waitFor waits up to 10 s for holds, failing with what it waited for.
func (r *rig) waitFor(what string, holds func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !holds(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.t.Fatalf("waited 10s for %s; the bridge logged:\n%s\nClaude Code recorded:\n%s", what, r.log, r.recorded())
		}
	}
}

// logged waits for the bridge to log text.
func (r *rig) logged(text string) {
	r.t.Helper()
	r.waitFor("the log line "+strconv.Quote(text), func() bool { return strings.Contains(r.log.String(), text) })
}

// release lets the fake's "held <name>" or "wait <name>" go on, once it has taken the release.
func (r *rig) release(name string) {
	r.t.Helper()
	path := filepath.Join(filepath.Dir(r.record), name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.waitFor("Claude Code to take the release "+name, func() bool {
		_, err := os.Stat(path)
		return errors.Is(err, os.ErrNotExist)
	})
}

// inboxClient is one connection to the bridge's inbox, which claude-envoy's channel server keeps
// for every delivery, writing each without waiting for the answers before it.
type inboxClient struct {
	t       *testing.T
	conn    net.Conn
	answers *bufio.Reader
}

func (r *rig) dialInbox() *inboxClient {
	r.t.Helper()
	r.waitFor("Claude Code to start", func() bool { return strings.Contains(r.recorded(), "\ninbox /") })
	conn, err := net.Dial("unix", inboxPathOf(r.t, r))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	return &inboxClient{t: r.t, conn: conn, answers: bufio.NewReader(conn)}
}

func (c *inboxClient) deliver(id, content string) {
	c.t.Helper()
	if err := json.NewEncoder(c.conn).Encode(InboxDelivery{ID: id, Content: content}); err != nil {
		c.t.Fatal(err)
	}
}

// answer is the inbox's next answer, which must come within 10 s.
func (c *inboxClient) answer() InboxAnswer {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	line, err := c.answers.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("the inbox answered %q, %v", line, err)
	}
	var answer InboxAnswer
	if err := json.Unmarshal(line, &answer); err != nil {
		c.t.Fatalf("the inbox answered %q: %v", line, err)
	}
	return answer
}

// noAnswer holds that the inbox answers nothing within a short while.
func (c *inboxClient) noAnswer() {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		c.t.Fatal(err)
	}
	line, err := c.answers.ReadBytes('\n')
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || len(line) > 0 {
		c.t.Fatalf("the inbox answered %q, %v; want no answer yet", line, err)
	}
}

func (r *rig) prompt(id, message string) {
	r.t.Helper()
	r.send(shimwire.Prompt{ID: id, DeliveryID: "d-" + id, Message: message})
	r.expect("response", map[string]any{"id": id, "command": "prompt", "success": true})
}

func (r *rig) state(id string) bool {
	r.t.Helper()
	r.send(shimwire.GetState{ID: id})
	frame := r.expect("response", map[string]any{"id": id, "command": "get_state", "success": true})
	return frame["data"].(map[string]any)["isStreaming"].(bool)
}

const sessionID = "11111111-2222-4333-8444-555555555555"

func TestAFreshSessionIsSeededBeforeTheDaemonsFirstPrompt(t *testing.T) {
	r := newRig(t, "-p", "--session-id", sessionID)
	r.send(shimwire.NegotiateProtocol{ID: "n1", ProtocolVersion: 2})
	r.expect("response", map[string]any{"id": "n1", "command": "negotiate_protocol", "success": true})
	r.prompt("p1", "say hello WAITING")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	lines := strings.Split(r.recorded(), "\n")
	var inputs []string
	for _, line := range lines {
		if body, ok := strings.CutPrefix(line, "in "); ok {
			inputs = append(inputs, body)
		}
	}
	if len(inputs) < 2 || !strings.Contains(inputs[0], `"text":"/rename Legion implementer · ACME-7"`) || !strings.Contains(inputs[1], "say hello") {
		t.Fatalf("Claude Code read %q, want the seed and then the prompt", inputs)
	}
	if path := inboxPathOf(t, r); !strings.HasSuffix(path, "/inbox.sock") {
		t.Errorf("Claude Code was told the inbox %q", path)
	}
}

func TestAResumedSessionIsNotSeeded(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "say WAITING for CI")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if strings.Contains(r.recorded(), "/rename") {
		t.Fatalf("a resumed session was seeded: %s", r.recorded())
	}
}

func TestATurnIsOneUserMessageWhenTheHandoffSucceeds(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	if r.state("g1") {
		t.Fatal("isStreaming before any turn")
	}
	r.prompt("p1", "handoff ok")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if strings.Count(r.recorded(), `"type":"user"`) != 1 {
		t.Errorf("a turn was more than one user message: %s", r.recorded())
	}
}

// A prompt during a turn is acknowledged and then refused in Oh My Pi's busy words, so the daemon
// takes the task back uncharged and sends it again at the turn's end. The gate holds the turn's
// own result back, so the turn is still running when the second prompt arrives.
func TestAPromptDuringATurnIsRefusedBusy(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", backgroundDuringPrompt)
	r.expect("agent_start", nil)
	if !r.state("g1") {
		t.Error("isStreaming during the turn is false")
	}
	r.prompt("p2", "say again")
	refusal := r.expect("response", map[string]any{"id": "p2", "command": "prompt", "success": false})
	if !strings.HasPrefix(refusal["error"].(string), "Agent is already processing") {
		t.Errorf("the refusal %q is not Oh My Pi's busy answer", refusal["error"])
	}
	r.open()
	r.expect("agent_end", nil)
	if strings.Contains(r.recorded(), "say again") {
		t.Errorf("Claude Code read the refused prompt: %s", r.recorded())
	}
}

// A step Claude Code ends in error reaches the daemon as Oh My Pi reports a failed step: the
// assistant message_end with stopReason "error" and the failure as its errorMessage.
func TestAFailedStepIsOhMyPisFailedMessage(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "fail WAITING on the provider")
	r.expect("agent_start", nil)
	frame := r.expect("message_end", nil)
	message, _ := frame["message"].(map[string]any)
	if message["role"] != "assistant" || message["stopReason"] != "error" {
		t.Fatalf("the failed step's message is %v, want an assistant message with stopReason error", frame)
	}
	if got, _ := message["errorMessage"].(string); !strings.Contains(got, "error_during_execution") ||
		!strings.Contains(got, "API Error: 529 overloaded") {
		t.Errorf("errorMessage %q names neither the result's subtype nor its error", got)
	}
	r.expect("agent_end", nil)
}

// An Envoy delivery during a turn is the turn's next user message; one between turns starts a turn.
// Either way the inbox answers it once Claude Code has taken it, and refuses a malformed one.
func TestEnvoyDeliveriesBecomeUserTurns(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "wait r1")
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "say WAITING on the CI")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.release("r1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.expect("agent_end", nil)
	// The prompt's turn is over and the bridge says so: the frame that names the prompt, which an
	// agent_end never does (shimwire.PromptResult).
	r.expect("prompt_result", map[string]any{"id": "p1", "agentInvoked": true})
	if recorded := r.recorded(); !strings.Contains(recorded, `"text":"say WAITING on the CI"`) {
		t.Fatalf("the delivery was not a user message: %s", recorded)
	}

	inbox.deliver("ev2", "say WAITING still")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev2", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.logged("Envoy delivery ev2 starts a turn")
	if _, err := inbox.conn.Write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if answer := inbox.answer(); answer.OK || !strings.Contains(answer.Error, "an id and content") {
		t.Errorf("a malformed delivery was answered %+v", answer)
	}
}

// The inbox answers a delivery once Claude Code has taken it, re-emitting it as the user message it
// reads (--replay-user-messages), not once the bridge has queued or written it: a Claude Code that
// has read the delivery and not yet taken it holds the answer back.
func TestTheInboxAnswersOnceClaudeCodeTakesTheDelivery(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "held h1")
	r.expect("agent_start", nil)
	r.waitFor("Claude Code to read the delivery", func() bool { return strings.Contains(r.recorded(), `"text":"held h1"`) })
	inbox.noAnswer()
	r.release("h1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.expect("agent_end", nil)
}

// Deliveries that arrive during a turn, written down one connection without waiting, are the turn's
// next steps in the order they came, and each is answered as Claude Code takes it, not before.
func TestDeliveriesQueuedInATurnAreAnsweredAsEachIsTaken(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "wait r1")
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "wait d1")
	inbox.deliver("ev2", "wait d2")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.logged("Envoy delivery ev2 waits for the next step")
	inbox.noAnswer()
	r.release("r1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v, want ev1 taken", answer)
	}
	inbox.noAnswer()
	r.release("d1")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev2", OK: true}) {
		t.Fatalf("the inbox answered %+v, want ev2 taken", answer)
	}
	r.release("d2")
	r.expect("agent_end", nil)
	r.expect("prompt_result", map[string]any{"id": "p1", "agentInvoked": true})
	r.quiet()
}

// A delivery Claude Code never takes, because it exits first, is answered with an error: the one it
// read and exited without taking, and the one the bridge still held for the step after it.
func TestADeliveryClaudeCodeNeverTakesIsAnsweredWithAnError(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "wait r1")
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "die")
	inbox.deliver("ev2", "say never")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.logged("Envoy delivery ev2 waits for the next step")
	r.release("r1")
	for _, id := range []string{"ev1", "ev2"} {
		if answer := inbox.answer(); answer != (InboxAnswer{ID: id, Error: "Claude Code has exited"}) {
			t.Fatalf("the inbox answered %+v, want %s refused: Claude Code has exited", answer, id)
		}
	}
}

// The bridge's own user messages come back re-emitted too, and answer no delivery: the echo of the
// daemon's prompt leaves a delivery queued with the very same text unanswered until Claude Code
// takes that delivery in its turn.
func TestAPromptsEchoAnswersNoDelivery(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "held same")
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev1", "held same")
	r.logged("Envoy delivery ev1 waits for the next step")
	r.release("same")
	inbox.noAnswer()
	r.release("same")
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev1", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.expect("agent_end", nil)
}

// backgroundTurn and backgroundDuringPrompt are the two recorded streams these cases drive the
// bridge with (replay). In backgroundTurn one prompt starts a background Bash command and ends its
// turn; the task finishes between turns, and lines 13-15 — task_updated, task_notification and
// background_tasks_changed, each naming the task and its run — are followed by the system/init of
// the turn Claude Code starts by itself, its assistant lines and its own result. In
// backgroundDuringPrompt the task finishes while a foreground command of the same turn still runs,
// and the same three lines arrive inside that turn, which has one result, its last line.
const (
	backgroundTurn         = "replay background-turn.jsonl"
	backgroundDuringPrompt = "replay background-during-prompt.jsonl"
)

// A background task that finishes between turns wakes Claude Code into a turn of its own, which
// the daemon is told of as a turn: agent_start when the init of that turn arrives, agent_end when
// its result does, and in between only what Claude Code's own lines carry. The prompt's turn ends
// on its own result first, so the daemon sees two turns, not one long one — and only the first
// carries a prompt_result behind it, which is the whole of how the daemon tells them apart.
func TestATurnClaudeCodeStartsByItselfIsATurnOfTheDaemons(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", backgroundTurn)
	r.open()
	var types []string
	for ends := 0; ends < 2; {
		typ, _ := r.nextRaw()["type"].(string)
		types = append(types, typ)
		if typ == "agent_end" {
			ends++
		}
	}
	// The prompt's turn carries one tool call, the background Bash command, which the bridge shows
	// on the pane; the turn Claude Code started by itself calls no tool, so the bridge writes
	// nothing of its own between that turn's start and its end.
	want := []string{"agent_start", "tool_execution_start", "agent_end", "prompt_result", "agent_start", "agent_end"}
	if !slices.Equal(types, want) {
		t.Fatalf("the daemon saw %q, want %q", types, want)
	}
	r.quiet()
}

// The three lines a background task writes when it finishes during a turn end no turn: the daemon
// sees one agent_start, and the agent_end only once the turn's own result arrives.
func TestABackgroundTaskFinishingDuringATurnDoesNotEndIt(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", backgroundDuringPrompt)
	r.expect("agent_start", nil)
	// The turn's two tool calls, the background command and the foreground one; the task's three
	// lines come between the second call and the turn's result, which the gate holds back.
	for i := range 2 {
		if frame := r.nextRaw(); frame["type"] != "tool_execution_start" {
			t.Fatalf("got %v, want tool call %d of the turn", frame, i+1)
		}
	}
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	r.expect("prompt_result", map[string]any{"id": "p1", "agentInvoked": true})
	r.quiet()
}

// A prompt that arrives during a turn Claude Code started by itself is refused as one during any
// turn is, and the daemon sends it again at that turn's end; the turn itself is untouched, and
// Claude Code never reads the refused task.
func TestAPromptDuringAClaudeStartedTurnIsRefusedBusy(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", backgroundTurn)
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.expect("prompt_result", map[string]any{"id": "p1", "agentInvoked": true})
	r.expect("agent_start", nil)
	if !r.state("g1") {
		t.Error("isStreaming during the turn Claude Code started is false")
	}
	r.prompt("p2", "say again")
	refusal := r.expect("response", map[string]any{"id": "p2", "command": "prompt", "success": false})
	if !strings.HasPrefix(refusal["error"].(string), "Agent is already processing") {
		t.Errorf("the refusal %q is not Oh My Pi's busy answer", refusal["error"])
	}
	r.open()
	r.expect("agent_end", nil)
	r.quiet()
	if strings.Contains(r.recorded(), "say again") {
		t.Errorf("Claude Code read the refused prompt: %s", r.recorded())
	}
}

// An Envoy delivery that arrives during a turn Claude Code started by itself is queued as it is
// during any turn — the turn runs on, and the inbox waits — and is the user message of the turn
// the bridge starts as soon as that one ends, answered once Claude Code takes it there.
func TestAnEnvoyDeliveryDuringAClaudeStartedTurnFollowsIt(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", backgroundTurn)
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.expect("prompt_result", map[string]any{"id": "p1", "agentInvoked": true})
	r.expect("agent_start", nil)
	inbox := r.dialInbox()
	inbox.deliver("ev9", "say the CI is green")
	r.logged("Envoy delivery ev9 waits for the next step")
	inbox.noAnswer()
	if strings.Contains(r.recorded(), "the CI is green") {
		t.Errorf("the delivery reached Claude Code during the turn it started: %s", r.recorded())
	}
	r.quiet()
	r.open()
	r.expect("agent_end", nil)
	r.expect("agent_start", nil)
	if answer := inbox.answer(); answer != (InboxAnswer{ID: "ev9", OK: true}) {
		t.Fatalf("the inbox answered %+v", answer)
	}
	r.expect("agent_end", nil)
	if !strings.Contains(r.recorded(), `"text":"say the CI is green"`) {
		t.Errorf("the delivery was no user message: %s", r.recorded())
	}
}

// An inbox client still connected when the bridge stops (claude-envoy's channel server keeps one
// connection for every delivery, and may outlive Claude Code) neither holds the bridge's exit nor
// keeps its inbox's directory.
func TestTheBridgeEndsWithAnInboxClientConnected(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "say WAITING for CI")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	path := inboxPathOf(t, r)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// An answer proves the inbox accepted the connection and serves it, so its client is one the
	// inbox's close waits for: a malformed delivery is answered with its refusal.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if answer, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil || !strings.Contains(string(answer), "an id and content") {
		t.Fatalf("the inbox answered %q, %v", answer, err)
	}
	r.stopped()
	select {
	case code := <-r.done:
		r.done <- code
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge did not end within 10s with an inbox client connected")
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the inbox's directory %s is still there (%v)", filepath.Dir(path), err)
	}
}

// A Claude Code that stops reading its stdin while it makes control requests fills the queue of
// the bridge's refusals: the bridge says so once, ends Claude Code, and exits with its inbox
// removed. (A shim that stops reading holds Claude Code's output back instead; cmd/legion's
// TestClaudeBridgeHoldsClaudeCodeWhileTheShimPauses.)
func TestAClaudeCodeThatStopsReadingEndsTheBridge(t *testing.T) {
	// A refusal repeats its request's id, so a long id fills the pipe within a few dozen refusals.
	controlRequest := `{"type":"control_request","request_id":"` + strings.Repeat("r", 1000) + `","request":{"subtype":"can_use_tool"}}`
	// The inbox's socket path must fit sun_path, so its TMPDIR is under /tmp.
	tmp, err := os.MkdirTemp("/tmp", "legion-bridge")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	dir := t.TempDir()
	stream := filepath.Join(dir, "stream")
	if err := os.WriteFile(stream, []byte(strings.Repeat(controlRequest+"\n", 2*claudeQueueLength)), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, daemon := io.Pipe()
	t.Cleanup(func() { _ = daemon.Close() })
	logged := &lockedBuffer{}
	cfg := Config{Argv: []string{"/bin/sh", "-c", "cat " + stream + "\nexec sleep 300\n"}, Env: os.Environ(),
		SessionID: sessionID, Stdin: stdin, Stdout: io.Discard, Log: logged}
	done := make(chan int, 1)
	go func() {
		code, err := Run(context.Background(), cfg)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- code
	}()
	select {
	case code := <-done:
		if code != 128+int(syscall.SIGTERM) {
			t.Errorf("exit status %d, want %d, Claude Code's once SIGTERM ended it:\n%s", code, 128+int(syscall.SIGTERM), logged)
		}
	case <-time.After(30 * time.Second):
		// Claude Code still runs, unreaped, under a bridge that never ended.
		if match := regexp.MustCompile(`\(pid (\d+)\)`).FindStringSubmatch(logged.String()); match != nil {
			pid, _ := strconv.Atoi(match[1])
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		t.Fatalf("the bridge did not end within 30s of Claude Code no longer reading:\n%s", logged)
	}
	overflow := "Claude Code has left " + strconv.Itoa(claudeQueueLength) + " lines unread"
	if n := strings.Count(logged.String(), overflow); n != 1 {
		t.Errorf("the bridge said %q %d times, want once:\n%s", overflow, n, logged)
	}
	if !strings.Contains(logged.String(), "Claude Code stopped reading its input: sending SIGTERM to Claude Code") {
		t.Errorf("the bridge did not end Claude Code:\n%s", logged)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "legion-claude-*")); len(left) > 0 {
		t.Errorf("the bridge left %q in TMPDIR", left)
	}
}

// lockedBuffer is a log the bridge's goroutines write while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// inboxPathOf is the inbox Claude Code was told, as the fake recorded it.
func inboxPathOf(t *testing.T, r *rig) string {
	t.Helper()
	for _, line := range strings.Split(r.recorded(), "\n") {
		if path, ok := strings.CutPrefix(line, "inbox "); ok && path != "" {
			return path
		}
	}
	t.Fatalf("Claude Code was told no inbox: %s", r.recorded())
	return ""
}

func TestUnknownRequestsAreRefusedByName(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.send(shimwire.Raw{Type: "steer", JSON: []byte(`{"type":"steer","id":"s1","message":"x"}`)})
	frame := r.expect("response", map[string]any{"id": "s1", "command": "steer", "success": false})
	if !strings.Contains(frame["error"].(string), "does not support steer") {
		t.Errorf("the refusal is %q", frame["error"])
	}
}

// The bridge's exit status is Claude Code's, and the shim closing the bridge's input ends it.
func TestTheShimClosingInputEndsClaudeCode(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "say WAITING")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	_ = r.daemon.Close()
	select {
	case code := <-r.done:
		if code != 0 {
			t.Errorf("exit status %d, want Claude Code's 0", code)
		}
		r.done <- code
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge outlived its input")
	}
}

func TestConfigure(t *testing.T) {
	base := []string{"LEGION_ROLE=tester", "LEGION_ISSUE=ACME-2"}
	cfg, err := Configure([]string{"claude", "-p", "--session-id=" + sessionID}, base)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Seed || cfg.SessionID != sessionID || cfg.Title != "Legion tester · ACME-2" {
		t.Errorf("Configure = %+v", cfg)
	}
	cfg, err = Configure([]string{"claude", "--resume", sessionID, "-p"}, nil)
	if err != nil || cfg.Seed || cfg.SessionID != sessionID {
		t.Errorf("resume: %+v, %v", cfg, err)
	}
	for _, tc := range []struct {
		argv []string
		env  []string
		want string
	}{
		{nil, base, "no claude command"},
		{[]string{"claude", "-p"}, base, "exactly one --session-id or --resume"},
		{[]string{"claude", "--session-id", "a", "--resume", "a"}, base, "exactly one"},
		{[]string{"claude", "--resume="}, base, "--resume names no session"},
		{[]string{"claude", "--session-id", "a"}, nil, "LEGION_ROLE and LEGION_ISSUE"},
	} {
		if _, err := Configure(tc.argv, tc.env); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Configure(%q) = %v, want %q", tc.argv, err, tc.want)
		}
	}
}
