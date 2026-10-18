package claudebridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// fakeClaudeEnv makes the test binary a stand-in for `claude -p` in stream-json: it records every
// line it reads to FAKE_CLAUDE_RECORD and answers each as fakeClaude's script says.
const fakeClaudeEnv = "LEGION_TEST_FAKE_CLAUDE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeClaudeEnv) == "1" {
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}

// fakeClaude answers Claude Code's stream-json the way the probes recorded it: each user message
// gets a system/init, then its script's lines, then a result; an interrupt gets a control_response
// and ends the user message being answered with error_during_execution. A user message's text picks
// its script:
//   - "/rename <title>": no init, an assistant line naming the title, a success result;
//   - "hang": nothing until an interrupt;
//   - "handoff ok" / "handoff refused": a `legion` handoff_complete call and its result;
//   - "say <text>": an assistant message of text;
//   - "inbox <text>": writes one delivery to LEGION_CLAUDE_INBOX and reads its answer;
//   - "fail <text>": an assistant message of text, then an error_during_execution result;
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
	assistant := func(blocks ...map[string]any) {
		emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": blocks}})
	}
	text := func(t string) map[string]any { return map[string]any{"type": "text", "text": t} }
	result := func(subtype string, isError bool) {
		emit(map[string]any{"type": "result", "subtype": subtype, "is_error": isError})
	}
	hanging := false
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return 0
		}
		fmt.Fprintf(record, "in %s", line)
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Message   struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &msg) != nil {
			return 3
		}
		if msg.Type == "control_request" {
			emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": msg.RequestID}})
			if hanging {
				hanging = false
				result("error_during_execution", true)
			}
			continue
		}
		body := msg.Message.Content[0].Text
		if title, ok := strings.CutPrefix(body, "/rename "); ok {
			assistant(text("Session renamed to: " + title))
			result("success", false)
			continue
		}
		emit(map[string]any{"type": "system", "subtype": "init", "session_id": "s"})
		switch {
		case body == "hang":
			hanging = true
			continue
		case body == "handoff ok" || body == "handoff refused":
			assistant(map[string]any{"type": "tool_use", "id": "tu1", "name": "mcp__legion__legion", "input": map[string]any{"op": "handoff_complete", "summary": "s"}})
			emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "tu1", "is_error": body == "handoff refused"}}}})
			assistant(text("completed"))
		case strings.HasPrefix(body, "say "):
			assistant(text(strings.TrimPrefix(body, "say ")))
		case strings.HasPrefix(body, "fail "):
			assistant(text(strings.TrimPrefix(body, "fail ")))
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
				"errors": []string{"API Error: 529 overloaded"}})
			continue
		case strings.HasPrefix(body, "inbox "):
			conn, err := net.Dial("unix", os.Getenv(InboxVariable))
			if err != nil {
				fmt.Fprintln(record, "inbox dial", err)
				return 4
			}
			_ = json.NewEncoder(conn).Encode(InboxDelivery{ID: "ev1", Content: strings.TrimPrefix(body, "inbox ")})
			answer, _ := bufio.NewReader(conn).ReadBytes('\n')
			fmt.Fprintf(record, "inbox-answer %s", answer)
			_ = conn.Close()
			assistant(text("WAITING on the event"))
		default:
			assistant(text("done"))
		}
		result("success", false)
	}
}

// rig runs one bridge over the fake Claude: frames go in through daemon, come out on frames.
type rig struct {
	t       *testing.T
	daemon  *io.PipeWriter
	frames  chan map[string]any
	record  string
	cfg     Config
	done    chan int
	stopped context.CancelFunc
}

func newRig(t *testing.T, args ...string) *rig {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	argv := append([]string{os.Args[0]}, args...)
	env := append(os.Environ(), fakeClaudeEnv+"=1", "FAKE_CLAUDE_RECORD="+record,
		"CLAUDE_CONFIG_DIR="+dir, "LEGION_ROLE=implementer", "LEGION_ISSUE=ACME-7")
	cfg, err := Configure(argv, env)
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	cfg.Stdin, cfg.Stdout, cfg.Log = inR, outW, io.Discard
	r := &rig{t: t, daemon: inW, frames: make(chan map[string]any, 100), record: record, cfg: cfg, done: make(chan int, 1)}
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

// next is the next frame the bridge wrote that is not a pane summary (tool_execution_start).
func (r *rig) next() map[string]any {
	r.t.Helper()
	for {
		select {
		case frame := <-r.frames:
			if frame["type"] == "tool_execution_start" {
				continue
			}
			return frame
		case <-time.After(10 * time.Second):
			r.t.Fatal("the bridge wrote nothing within 10s")
			return nil
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

func (r *rig) recorded() string {
	raw, _ := os.ReadFile(r.record)
	return string(raw)
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

func (r *rig) stallState() PhaseStall {
	r.t.Helper()
	state, err := readStall(r.cfg.StallFile)
	if err != nil {
		r.t.Fatal(err)
	}
	return state
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
	if got := r.stallState(); got != StallClosed {
		t.Errorf("the phase after a successful handoff_complete is %s, want closed", got)
	}
	if strings.Count(r.recorded(), `"type":"user"`) != 1 {
		t.Errorf("a completed phase got a follow-up: %s", r.recorded())
	}
}

// An open phase whose turn settles gets phase-stall.ts's follow-up as one more user message of the
// same turn, once; the turn then ends quiet.
func TestAnOpenPhaseGetsOneFollowUp(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "say all done")
	r.expect("agent_start", nil)
	if !r.state("g1") {
		t.Error("isStreaming during the turn is false")
	}
	r.expect("agent_end", nil)
	if got := strings.Count(r.recorded(), FollowUp); got != 1 {
		t.Fatalf("the follow-up was sent %d times, want once: %s", got, r.recorded())
	}
	if got := r.stallState(); got != StallQuiet {
		t.Errorf("the phase after its follow-up is %s, want quiet", got)
	}
	r.prompt("p2", "handoff refused")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if got := strings.Count(r.recorded(), FollowUp); got != 2 {
		t.Errorf("a refused handoff_complete left no follow-up due: %d sent", got)
	}
}

func TestATextToolCallGetsTheNamedFollowUp(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", `say <invoke name="legion">`)
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if !strings.Contains(r.recorded(), jsonText(textToolCall+FollowUp)) {
		t.Errorf("the follow-up does not name the text tool call: %s", r.recorded())
	}
}

// A prompt during a turn is acknowledged and then refused in Oh My Pi's busy words, so the daemon
// takes the task back uncharged and sends it again at the turn's end.
func TestAPromptDuringATurnIsRefusedBusy(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "hang")
	r.expect("agent_start", nil)
	r.prompt("p2", "say again")
	refusal := r.expect("response", map[string]any{"id": "p2", "command": "prompt", "success": false})
	if !strings.HasPrefix(refusal["error"].(string), "Agent is already processing") {
		t.Errorf("the refusal %q is not Oh My Pi's busy answer", refusal["error"])
	}
	r.send(shimwire.Abort{ID: "a1"})
	r.expect("response", map[string]any{"id": "a1", "command": "abort", "success": true})
	r.expect("agent_end", nil)
	if strings.Contains(r.recorded(), "say again") {
		t.Errorf("Claude Code read the refused prompt: %s", r.recorded())
	}
}

// An abort becomes an interrupt; its answer is Claude Code's acknowledgement, and the interrupted
// user message's result ends the turn with no follow-up.
func TestAnAbortInterruptsAndEndsTheTurn(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "hang")
	r.expect("agent_start", nil)
	r.send(shimwire.Abort{ID: "a1"})
	r.expect("response", map[string]any{"id": "a1", "command": "abort", "success": true})
	r.expect("agent_end", nil)
	if !strings.Contains(r.recorded(), `"subtype":"interrupt"`) {
		t.Errorf("no interrupt reached Claude Code: %s", r.recorded())
	}
	if strings.Contains(r.recorded(), FollowUp) {
		t.Errorf("an interrupted turn got the follow-up: %s", r.recorded())
	}
	// An abort with no turn running is answered at once.
	r.send(shimwire.Abort{ID: "a2"})
	r.expect("response", map[string]any{"id": "a2", "command": "abort", "success": true})
	r.quiet()
}

// A step Claude Code ends in error reaches the daemon as Oh My Pi reports a failed step: the
// assistant message_end with stopReason "error" and the failure as its errorMessage. An interrupted
// step is not a failure, and TestAnAbortInterruptsAndEndsTheTurn holds that it sends none.
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
// Either way the inbox answers once it is queued, and a quiet phase opens again.
func TestEnvoyDeliveriesBecomeUserTurns(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "inbox the CI is green")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	recorded := r.recorded()
	if !strings.Contains(recorded, `inbox-answer {"id":"ev1","ok":true}`) {
		t.Fatalf("the inbox did not answer the delivery: %s", recorded)
	}
	if !strings.Contains(recorded, `"text":"the CI is green"`) {
		t.Fatalf("the delivery was not a user message: %s", recorded)
	}

	conn, err := net.Dial("unix", inboxPathOf(t, r))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(InboxDelivery{ID: "ev2", Content: "say WAITING still"}); err != nil {
		t.Fatal(err)
	}
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	answer, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil || !strings.Contains(string(answer), `"ok":true`) {
		t.Fatalf("the inbox answered %q, %v", answer, err)
	}
	if _, err := conn.Write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if answer, _ := bufio.NewReader(conn).ReadBytes('\n'); !strings.Contains(string(answer), "an id and content") {
		t.Errorf("a malformed delivery was answered %q", answer)
	}
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

// jsonText is text as it appears inside a JSON string the bridge wrote.
func jsonText(text string) string {
	raw, _ := json.Marshal(text)
	return strings.Trim(string(raw), `"`)
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

func TestTheStallStateSurvivesARelaunch(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", "say done")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	if got := r.stallState(); got != StallQuiet {
		t.Fatalf("stall = %s, want quiet", got)
	}
	cfg := r.cfg
	got, err := readStall(cfg.StallFile)
	if err != nil || got != StallQuiet {
		t.Fatalf("a relaunch reads %s, %v", got, err)
	}
}

func TestConfigure(t *testing.T) {
	base := []string{"CLAUDE_CONFIG_DIR=/state/claude/c1", "LEGION_ROLE=tester", "LEGION_ISSUE=ACME-2"}
	cfg, err := Configure([]string{"claude", "-p", "--session-id=" + sessionID}, base)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Seed || cfg.SessionID != sessionID || cfg.Title != "Legion tester · ACME-2" ||
		cfg.StallFile != "/state/claude/c1/legion-phase-stall-"+sessionID+".json" {
		t.Errorf("Configure = %+v", cfg)
	}
	cfg, err = Configure([]string{"claude", "--resume", sessionID, "-p"}, []string{"CLAUDE_CONFIG_DIR=/c"})
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
		{[]string{"claude", "--resume", "a"}, []string{"CLAUDE_CONFIG_DIR=rel"}, "CLAUDE_CONFIG_DIR must be an absolute path"},
		{[]string{"claude", "--session-id", "a"}, []string{"CLAUDE_CONFIG_DIR=/c"}, "LEGION_ROLE and LEGION_ISSUE"},
	} {
		if _, err := Configure(tc.argv, tc.env); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Configure(%q) = %v, want %q", tc.argv, err, tc.want)
		}
	}
}

// The follow-up and the phase-stall rules are pi-legion's, verbatim: an Oh My Pi worker and a Claude
// Code worker are told the same thing. phase-stall.ts writes each text as double-quoted pieces
// joined by `+`, which jsConstant joins back into the one string.
func TestTheFollowUpIsPiLegions(t *testing.T) {
	source, err := os.ReadFile("../../../pi-legion/src/phase-stall.ts")
	if err != nil {
		t.Fatal(err)
	}
	for constant, want := range map[string]string{"FOLLOW_UP": FollowUp, "TEXT_TOOL_CALL": textToolCall} {
		if got := jsConstant(t, string(source), constant); got != want {
			t.Errorf("phase-stall.ts's %s is\n%q\nthe bridge's is\n%q", constant, got, want)
		}
	}
	for _, pattern := range []string{
		"const WAITING_REPLY = /" + strings.TrimPrefix(waitingReply.String(), "(?m)") + "/m;",
		"const TOOL_CALL_SHAPED = /" + toolCallShaped.String() + "/;",
	} {
		if !strings.Contains(string(source), pattern) {
			t.Errorf("phase-stall.ts does not hold %q", pattern)
		}
	}
}

// jsConstant is the string `const <name> = "…" + "…";` builds in source: each double-quoted piece,
// in order, while only whitespace and `+` stand between them.
func jsConstant(t *testing.T, source, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(source, "const "+name+" =")
	if !ok {
		t.Fatalf("phase-stall.ts declares no %s", name)
	}
	piece := regexp.MustCompile(`^[\s+]*("(?:[^"\\]|\\.)*")`)
	var joined strings.Builder
	for {
		match := piece.FindStringSubmatchIndex(rest)
		if match == nil {
			break
		}
		var s string
		if err := json.Unmarshal([]byte(rest[match[2]:match[3]]), &s); err != nil {
			t.Fatalf("%s piece %s: %v", name, rest[match[2]:match[3]], err)
		}
		joined.WriteString(s)
		rest = rest[match[1]:]
	}
	return joined.String()
}

func TestStallTransitions(t *testing.T) {
	for _, tc := range []struct {
		from   PhaseStall
		text   string
		want   PhaseStall
		follow bool
	}{
		{StallOpen, "done", StallQuiet, true},
		{StallOpen, "**WAITING** on CI", StallQuiet, false},
		{StallQuiet, "done", StallQuiet, false},
		{StallClosed, "done", StallClosed, false},
	} {
		got, followUp := tc.from.settle(tc.text)
		if got != tc.want || (followUp != "") != tc.follow {
			t.Errorf("%s.settle(%q) = %s, %q", tc.from, tc.text, got, followUp)
		}
	}
	if StallQuiet.inbound() != StallOpen || StallClosed.inbound() != StallClosed || StallOpen.inbound() != StallOpen {
		t.Error("inbound transitions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "stall.json")
	if s, err := readStall(path); err != nil || s != StallClosed {
		t.Errorf("no file reads %s, %v", s, err)
	}
	if err := writeStall(path, StallOpen); err != nil {
		t.Fatal(err)
	}
	if s, err := readStall(path); err != nil || s != StallOpen {
		t.Errorf("reads %s, %v", s, err)
	}
	if err := os.WriteFile(path, []byte(`{"state":"later"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readStall(path); err == nil || !strings.Contains(err.Error(), "not closed, open or quiet") {
		t.Errorf("a bad state read %v", err)
	}
}
