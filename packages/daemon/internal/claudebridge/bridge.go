package claudebridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const (
	// Grace is how long Claude Code has to exit after the bridge's SIGTERM before it is killed.
	Grace = 3 * time.Second
	// drainTimeout bounds reading Claude Code's stdout once it has exited. What Claude Code wrote
	// is in the pipe by then, and once it has exited the loop takes its lines without waiting on the
	// shim, so a pipe's worth is read in well under a millisecond: only a process it started and
	// left holding the pipe keeps it open past this, and that output is not Claude Code's.
	drainTimeout = 500 * time.Millisecond
	// flushTimeout bounds, once the bridge is told to stop or its input has ended, how long it
	// still waits for the shim to read the frames queued for it. A shim that reads takes them as
	// fast as it forwards them: at most framesHighWater frames and the few its requests added,
	// most a few hundred bytes, none over shimwire.MaxFrameBytes. One that has not by then is one
	// the stop is not waiting on.
	//
	// Told to stop, the bridge exits within Grace + drainTimeout + inboxFlushTimeout + flushTimeout,
	// 4.1 s, which leaves 0.9 s of the 5 s the shim gives it before killing it (shim.DefaultGrace)
	// for the process to exit on a loaded machine; its inbox is gone 0.5 s before that.
	flushTimeout = 500 * time.Millisecond
	// Busy is the refusal of a prompt that arrives during a turn. It begins with Oh My Pi's own
	// words, which the daemon reads as a turn of the agent's own (supervise's agentBusy): the task
	// goes back unread, uncharged, and is sent again when the turn ends.
	Busy = "Agent is already processing a turn; the Claude Code bridge takes a prompt only between turns"
)

var (
	// errClaudeExited answers each Envoy delivery Claude Code had not taken when it exited.
	errClaudeExited = errors.New("Claude Code has exited")
	// errNotTaken answers a delivery sent as a step whose result came with no re-emission of it:
	// Claude Code dropped the message, which it re-emits before the result of the step taking it.
	errNotTaken = errors.New("Claude Code ended the step without taking the delivery")
)

// event is a frame from the shim or an Envoy delivery from the inbox, for the bridge's loop.
type event struct {
	daemon   []byte    // a frame from the shim
	delivery *delivery // an Envoy delivery from the inbox
}

// delivery is one Envoy delivery waiting to be a user turn. answer, buffered, is answered once:
// with nil once Claude Code has taken it (re-emitted it, --replay-user-messages), else with why it
// never will.
type delivery struct {
	id      string
	content string
	answer  chan<- error
}

// bridge is Run's state. Everything but the readers, the outputs' writers and end runs on Run's
// one goroutine, and none of those touches this state, so none of it is locked.
type bridge struct {
	cfg Config
	log *log.Logger
	// toShim and toClaude are the bridge's two outputs (output.go). frames encodes each frame for
	// the shim into encoded, which toShim then queues.
	toShim, toClaude *output
	frames           *shimwire.Writer
	encoded          lineCopy

	// seeding is the fresh session's `/rename`, sent before anything else and answered by a
	// result that is no turn of the daemon's; held are the prompts that arrived during it, handled
	// once it is over.
	seeding bool
	held    [][]byte

	// inRun is a turn the daemon was told of (agent_start) and not yet told is over; stepping is a
	// user message whose result Claude Code has yet to give. In a turn the bridge itself started it
	// is always stepping: each result either sends the next message or ends the turn. In a turn
	// Claude Code started by itself — a background task's result waking it, which the bridge reads
	// from the `system` `init` line that begins every turn (systemInit) — it is in a turn with no
	// step of its own open, and that turn's one result ends it.
	inRun, stepping bool
	// promptRun is the request id of the daemon's prompt whose turn is running, and "" in a turn
	// no prompt of the daemon's started — an Envoy delivery's, or one Claude Code started by
	// itself when a background task finished. The bridge reports it as Oh My Pi reports its own
	// (shimwire.PromptResult), right behind that turn's agent_end: with background work on it is
	// the only thing that tells the daemon which of the turns it saw was its task's.
	promptRun string
	// queue is the Envoy deliveries waiting for the next step or turn; taking is those sent to
	// Claude Code that it has yet to re-emit, each answered when it does.
	queue, taking []delivery

	// heldBack is the shim having once fallen framesHighWater frames behind, which is logged once.
	heldBack bool
}

// Run starts Claude Code and bridges until it exits, returning its exit status: its code, or 128
// plus the signal that ended it. ctx is the bridge's own termination, which ends Claude Code with
// SIGTERM and then, after Grace, SIGKILL. The error is the bridge's own failure to start.
//
// The loop never waits on a pipe's reader (output.go), and ending Claude Code is end's, so neither
// waits on the other. A shim that stops reading holds back Claude Code's output and nothing else
// (framesHighWater): the loop still takes the shim's input, Envoy's deliveries, Claude Code's exit
// and its own end. Once Claude Code has exited and its stdout is read, every Envoy delivery it has
// not taken is answered with an error, the inbox is removed, and Run returns once the shim has read
// every frame queued for it.
//
// Told to stop (ctx), the bridge returns within Grace + drainTimeout + inboxFlushTimeout +
// flushTimeout, 4.1 s, its inbox removed by Grace + drainTimeout + inboxFlushTimeout; after its
// input ends, within twice Grace + drainTimeout + inboxFlushTimeout + flushTimeout, 7.1 s; and a
// stop or the input's end that comes while it waits for the shim ends the wait within flushTimeout.
// end's timers need nothing of the loop, Claude Code is reaped by Grace after its SIGTERM, the loop
// takes that exit at once, drainTimeout bounds what is left of Claude Code's stdout,
// inboxFlushTimeout the answers the inbox's clients are owed, and flushTimeout what is left of the
// shim's frames. The bounds count on what no reader of the bridge's governs: Claude Code dying of
// SIGKILL, exec's Wait copying nothing (Claude Code's stderr is a file, as a pane's is), and the log
// taking the loop's writes.
func Run(ctx context.Context, cfg Config) (int, error) {
	logOut := cfg.Log
	if logOut == nil {
		logOut = io.Discard
	}
	b := &bridge{cfg: cfg, log: log.New(logOut, "[claude-bridge] ", 0)}
	b.frames = shimwire.NewWriter(&b.encoded)
	// stop ends the outputs' writers and every reader's send, once Run returns. It does not
	// interrupt a readLines blocked in ReadBytes, and Run does not wait for those readers.
	stop := make(chan struct{})
	defer close(stop)
	b.toShim = newOutput("the shim", cfg.Stdout, stop, b.log)
	events := make(chan event)
	// inboxStop releases a delivery's send blocked on the loop and closes every inbox connection,
	// so each client's handler returns and inbox.close's wait for them ends.
	inboxStop := make(chan struct{})
	inbox, err := listenInbox(events, inboxStop, b.log)
	if err != nil {
		return 1, err
	}
	closeInbox := sync.OnceFunc(func() {
		close(inboxStop)
		inbox.close()
	})
	defer closeInbox()

	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
	cmd.Env = append(slices.Clone(cfg.Env), InboxVariable+"="+inbox.path)
	cmd.Stderr = logOut
	setParentDeathSignal(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, fmt.Errorf("start %s: %w", cfg.Argv[0], err)
	}
	// Claude Code's stdout is a pipe of the bridge's own rather than exec's, so its exit and the
	// end of its output are two facts waited on separately: exec's Wait closes its own pipe.
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		return 1, fmt.Errorf("start %s: %w", cfg.Argv[0], err)
	}
	cmd.Stdout = childStdout
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = childStdout.Close()
		return 1, fmt.Errorf("start %s: %w", cfg.Argv[0], err)
	}
	_ = childStdout.Close()
	b.log.Printf("started %s (pid %d) for session %s", cfg.Argv[0], cmd.Process.Pid, cfg.SessionID)
	// A write blocked on a Claude Code that reads nothing ends with Claude Code: exec's Wait closes
	// the pipe once Claude Code is reaped.
	b.toClaude = newOutput("Claude Code", stdin, stop, b.log)
	// stalled is closed once Claude Code has stopped reading, and end ends Claude Code on it.
	stalled := make(chan struct{})
	b.toClaude.limit, b.toClaude.full = claudeQueueLength, func() { close(stalled) }

	// Claude Code's lines come on a channel of their own, so the loop can stop taking them alone.
	claudeLines, pipeEnded := make(chan []byte), make(chan struct{})
	go readLines(stdout, func(line []byte) bool { return send(claudeLines, stop, line) }, func() {
		_ = stdout.Close()
		close(pipeEnded)
	})
	inputEnded, claudeExited := make(chan struct{}), make(chan struct{})
	go readLines(cfg.Stdin, func(line []byte) bool { return send(events, stop, event{daemon: line}) },
		func() { close(inputEnded) })
	var status int // Claude Code's exit status, set before claudeExited is closed
	go func() {
		_ = cmd.Wait()
		status = exitStatus(cmd.ProcessState)
		close(claudeExited)
	}()
	go b.end(ctx, cmd.Process, inputEnded, claudeExited, stalled)

	if cfg.Seed {
		b.seeding = true
		b.writeUser("/rename " + cfg.Title)
	}

	// Claude Code is done once it has exited and its stdout has been read to its end, or
	// drainTimeout after the exit when something it started still holds the pipe: every line it
	// wrote before exiting is acted on first. Stopping is the bridge told to stop or its input
	// ended, which bounds the wait for the shim that follows.
	var (
		exited, claudeDone, stopping, ignoring bool
		claudeExit                             = (<-chan struct{})(claudeExited)
		drained                                = (<-chan struct{})(pipeEnded)
		inputGone                              = (<-chan struct{})(inputEnded)
		done                                   = ctx.Done()
		drainBound, flushBound                 <-chan time.Time
	)
	for {
		if !claudeDone && exited && drained == nil {
			b.log.Printf("Claude Code exited with status %d", status)
			claudeDone = true
		}
		if claudeDone {
			answerAll(b.taking, errClaudeExited)
			answerAll(b.queue, errClaudeExited)
			b.taking, b.queue = nil, nil
			closeInbox()
			if b.toShim.waiting() == 0 {
				return status, nil
			}
			if stopping && flushBound == nil {
				flushBound = time.After(flushTimeout)
			}
		}
		claudeIn := (<-chan []byte)(claudeLines)
		switch {
		case claudeDone:
			claudeIn = nil
		case !exited && b.toShim.waiting() >= framesHighWater:
			claudeIn = nil
			if !b.heldBack {
				b.heldBack = true
				b.log.Printf("the shim has %d frames unread; Claude Code's output waits until it reads them, now and whenever it falls that far behind",
					framesHighWater)
			}
		}
		select {
		case line := <-claudeIn:
			b.fromClaude(line)
		case <-b.toShim.wrote:
			b.toShim.written()
		case <-b.toClaude.wrote:
			b.toClaude.written()
		case ev := <-events:
			switch {
			case claudeDone:
				// Nothing answers the daemon once Claude Code is done: its frames are read so that
				// the end of the shim's input still reaches the loop.
				if ev.delivery != nil {
					ev.delivery.answer <- errClaudeExited
				}
				if !ignoring {
					ignoring = true
					b.log.Printf("ignoring the shim's frames: Claude Code has exited")
				}
			case ev.daemon != nil:
				b.fromDaemon(ev.daemon)
			case ev.delivery != nil:
				b.deliver(*ev.delivery)
			}
		case <-done:
			done, stopping = nil, true
		case <-inputGone:
			// The shim is gone: nothing can reach the daemon. Claude Code ends with its input,
			// closed once what is queued for it is written, and end ends it if it has not within
			// Grace.
			inputGone, stopping = nil, true
			b.toClaude.close(func() { _ = stdin.Close() })
		case <-claudeExit:
			claudeExit, exited = nil, true
			if drained != nil {
				drainBound = time.After(drainTimeout)
			}
		case <-drained:
			drained, drainBound = nil, nil
		case <-drainBound:
			drainBound, claudeDone = nil, true
			b.log.Printf("Claude Code exited with status %d, and something it started still holds its stdout", status)
		case <-flushBound:
			b.log.Printf("the shim has not read the %d frames still queued for it %v after the bridge was told to stop; leaving them",
				b.toShim.waiting(), flushTimeout)
			return status, nil
		}
	}
}

// end ends Claude Code with SIGTERM and then, after Grace, SIGKILL, once the bridge is told to stop
// (ctx), once Claude Code has stopped reading (stalled), or Grace after the bridge's input ended
// without Claude Code ending with it; it returns once Claude Code has exited, or once it has been
// killed. It runs apart from the loop, so its timers need nothing of it (Run states the bounds).
func (b *bridge) end(ctx context.Context, claude *os.Process, inputEnded, exited, stalled <-chan struct{}) {
	var inputBound, killAt <-chan time.Time
	terminate := func(why string) {
		if killAt != nil {
			return
		}
		b.log.Printf("%s: sending SIGTERM to Claude Code", why)
		_ = claude.Signal(syscall.SIGTERM)
		killAt = time.After(Grace)
	}
	done := ctx.Done()
	for {
		select {
		case <-exited:
			return
		case <-done:
			done = nil
			terminate("signalled to stop")
		case <-stalled:
			stalled = nil
			terminate("Claude Code stopped reading its input")
		case <-inputEnded:
			inputEnded = nil
			inputBound = time.After(Grace)
		case <-inputBound:
			inputBound = nil
			terminate(fmt.Sprintf("Claude Code is still running %v after its input ended", Grace))
		case <-killAt:
			b.log.Printf("Claude Code is still running %v after SIGTERM; killing it", Grace)
			_ = claude.Kill()
			return
		}
	}
}

// send hands v to the loop on ch unless stop is closed first.
func send[T any](ch chan<- T, stop <-chan struct{}, v T) bool {
	select {
	case ch <- v:
		return true
	case <-stop:
		return false
	}
}

// readLines calls line with each newline-terminated line of r, a copy the callee owns, until r ends
// or line reports the reader should stop; then end. Claude Code's lines carry whole tool results,
// so no length is imposed.
func readLines(r io.Reader, line func([]byte) bool, end func()) {
	defer end()
	reader := bufio.NewReaderSize(r, 64<<10)
	for {
		raw, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && !line(bytes.Clone(trimmed)) {
			return
		}
		if err != nil {
			return
		}
	}
}

func exitStatus(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}

// fromDaemon answers one frame the shim forwarded: the three RPC commands the daemon sends a
// worker, and a refusal for any other request that names an id.
func (b *bridge) fromDaemon(line []byte) {
	frame, err := shimwire.Decode(line)
	if err != nil {
		b.log.Printf("ignoring a frame from the shim that is not one: %v", err)
		return
	}
	switch f := frame.(type) {
	case shimwire.NegotiateProtocol:
		b.respond(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
	case shimwire.GetState:
		data, _ := json.Marshal(map[string]bool{"isStreaming": b.inRun})
		b.respond(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
	case shimwire.Prompt:
		if b.seeding {
			b.held = append(b.held, line)
			return
		}
		b.prompt(f)
	default:
		var request struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(line, &request)
		if request.ID == "" {
			b.log.Printf("ignoring a %s frame", frame.FrameType())
			return
		}
		b.respond(shimwire.Response{ID: request.ID, Command: frame.FrameType(), Success: false,
			Error: "the Claude Code bridge does not support " + frame.FrameType()})
	}
}

// prompt is the daemon's task. Between turns it starts one; during a turn, the bridge's own or one
// Claude Code started by itself, it is acknowledged and then refused as Oh My Pi refuses it, so the
// daemon sends it again once the turn ends.
func (b *bridge) prompt(p shimwire.Prompt) {
	b.respond(shimwire.Response{ID: p.ID, Command: shimwire.TypePrompt, Success: true})
	if b.inRun {
		b.respond(shimwire.Response{ID: p.ID, Command: shimwire.TypePrompt, Success: false, Error: Busy})
		return
	}
	b.startRun(p.Message, p.ID)
}

// deliver queues an Envoy delivery: between turns it starts one at once; during a turn of the
// bridge's own it is the next step's message, and during one Claude Code started by itself it is
// the turn the bridge starts when that one ends. It is answered once Claude Code has taken it
// (echoed).
func (b *bridge) deliver(d delivery) {
	b.queue = append(b.queue, d)
	if b.inRun || b.seeding {
		b.log.Printf("Envoy delivery %s waits for the next step", d.id)
		return
	}
	b.log.Printf("Envoy delivery %s starts a turn", d.id)
	b.sendQueued()
}

// sendQueued sends the oldest queued delivery as the next user message, a step of the turn running
// or else the first of a turn, and waits for Claude Code to take it.
func (b *bridge) sendQueued() {
	d := b.queue[0]
	b.queue = b.queue[1:]
	b.taking = append(b.taking, d)
	if b.inRun {
		b.step(d.content)
		return
	}
	b.startRun(d.content, "")
}

// beginRun marks a turn running and tells the daemon it began. prompt is the request id of the
// daemon's prompt that started it, and "" for a turn nothing of the daemon's did — an Envoy
// delivery's, or one Claude Code started by itself — which is why it is written rather than
// assumed to be clear already.
func (b *bridge) beginRun(prompt string) {
	b.inRun, b.promptRun = true, prompt
	b.emit(shimwire.AgentStart{})
}

// startRun tells the daemon a turn of the bridge's own starts and sends its first message. prompt
// is the request id of the daemon's prompt that started it, and "" for a turn nothing of the
// daemon's did.
func (b *bridge) startRun(message, prompt string) {
	b.beginRun(prompt)
	b.step(message)
}

// step sends one user message of the turn running.
func (b *bridge) step(message string) {
	b.stepping = true
	b.writeUser(message)
}

// fromClaude acts on one stream-json line of Claude Code's.
func (b *bridge) fromClaude(line []byte) {
	var head struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		Message   json.RawMessage `json:"message"`
		IsReplay  bool            `json:"isReplay"`
		RequestID string          `json:"request_id"`
		IsError   bool            `json:"is_error"`
		Errors    []string        `json:"errors"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		b.log.Printf("ignoring a line from Claude Code that is not JSON (%d bytes)", len(line))
		return
	}
	switch head.Type {
	case "assistant":
		b.assistant(head.Message)
	case "user":
		// A user message Claude Code took from stdin comes back marked isReplay; the others are
		// its own, which the bridge does not read: tool results.
		if head.IsReplay {
			b.echoed(head.Message)
		}
	case "system":
		if head.Subtype == "init" {
			b.systemInit()
		}
	case "control_request":
		b.log.Printf("refusing Claude Code's control request %s: the bridge serves none", head.RequestID)
		b.writeClaude(map[string]any{"type": "control_response", "response": map[string]string{
			"subtype": "error", "request_id": head.RequestID, "error": "the Legion Claude Code bridge serves no control request"}})
	case "result":
		b.result(head.Subtype, head.IsError, head.Errors)
	}
}

// systemInit acts on the `init` line Claude Code writes at the start of every turn and at no other
// time: of a turn the daemon's prompt, an Envoy delivery or the seed started, and of one Claude
// Code started by itself when a background task it had left running finished. Between turns it is
// that second kind, a turn no step of the bridge's is open in, and the daemon is told of it as it
// is told of any other, so the background result is read inside a turn rather than after one the
// daemon thinks has ended. During a turn or the seeding it is the turn already begun: the bridge
// set that state when it wrote the user message, before this line came back.
func (b *bridge) systemInit() {
	if b.inRun || b.seeding {
		return
	}
	b.log.Printf("Claude Code started a turn of its own")
	b.beginRun("")
}

// contentBlock is one block of a stream-json message's content.
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func blocksOf(message json.RawMessage) []contentBlock {
	var m struct {
		Content []contentBlock `json:"content"`
	}
	_ = json.Unmarshal(message, &m)
	return m.Content
}

// assistant shows each tool call of the message on the pane through the shim. Nothing else of it
// is read: the phase machine runs in the pane's `legion` MCP server, driven by Claude Code's own
// hooks, and the bridge is transport.
func (b *bridge) assistant(message json.RawMessage) {
	for _, block := range blocksOf(message) {
		if block.Type != "tool_use" {
			continue
		}
		args := block.Input
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		b.emitRaw(map[string]any{"type": "tool_execution_start", "toolName": block.Name, "args": args})
	}
}

// echoed answers the delivery Claude Code has just taken: the oldest one sent, not yet answered,
// whose text is the re-emitted user message's. The bridge's own user messages (the daemon's prompt,
// the seed) are no delivery sent, so their echoes answer none.
func (b *bridge) echoed(message json.RawMessage) {
	var texts []string
	for _, block := range blocksOf(message) {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	text := strings.Join(texts, "\n")
	for i, d := range b.taking {
		if d.content == text {
			d.answer <- nil
			b.taking = slices.Delete(b.taking, i, i+1)
			return
		}
	}
}

// answerAll answers each of deliveries with err.
func answerAll(deliveries []delivery, err error) {
	for _, d := range deliveries {
		d.answer <- err
	}
}

// result ends a step, or the turn Claude Code started by itself. The seed's ends the seeding; a
// step of a turn's is followed by the next Envoy delivery, else the turn's end; a self-started
// turn's ends that turn, no step of the bridge's being open in it, and the next Envoy delivery
// then starts a turn of the bridge's own. A delivery the step sent that Claude Code ended it
// without taking never will be.
func (b *bridge) result(subtype string, isError bool, errs []string) {
	if b.seeding {
		b.seeding = false
		if isError {
			b.log.Printf("the session's seed failed (%s: %s); the claim registers no transcript until one is written", subtype, strings.Join(errs, "; "))
		} else {
			b.log.Printf("seeded session %s as %q", b.cfg.SessionID, b.cfg.Title)
		}
		held := b.held
		b.held = nil
		for _, line := range held {
			b.fromDaemon(line)
		}
		if !b.inRun && len(b.queue) > 0 {
			b.sendQueued()
		}
		return
	}
	if !b.inRun {
		b.log.Printf("ignoring a %s result outside any turn", subtype)
		return
	}
	stepped := b.stepping
	b.stepping = false
	answerAll(b.taking, errNotTaken)
	b.taking = nil
	if isError {
		// Oh My Pi reports a failed step as the assistant message that ends it, stopReason "error";
		// the daemon reads neither, so the log line is what puts the failure on the pane.
		failure := "Claude Code: " + subtype + " " + strings.Join(errs, "; ")
		b.log.Printf("the turn failed: %s", failure)
		b.emitRaw(map[string]any{"type": "message_end", "message": map[string]any{
			"role": "assistant", "stopReason": "error", "errorMessage": failure}})
	}
	if stepped && len(b.queue) > 0 {
		b.sendQueued()
		return
	}
	b.inRun = false
	b.emit(shimwire.AgentEnd{})
	if prompt := b.promptRun; prompt != "" {
		// The turn that ran the daemon's prompt is over, and this is what says it was that
		// prompt's: an agent_end names no prompt, and a turn Claude Code started by itself when a
		// background task finished ends with one just like it.
		b.promptRun = ""
		b.emit(shimwire.PromptResult{ID: prompt, AgentInvoked: true})
	}
	if len(b.queue) > 0 {
		b.sendQueued()
	}
}

// writeUser sends text to Claude Code as one user message.
func (b *bridge) writeUser(text string) {
	b.writeClaude(map[string]any{"type": "user", "message": map[string]any{
		"role": "user", "content": []map[string]string{{"type": "text", "text": text}}}})
}

// writeClaude queues v for Claude Code as one stream-json line.
func (b *bridge) writeClaude(v any) {
	line, err := json.Marshal(v)
	if err != nil {
		b.log.Printf("could not write to Claude Code: %v", err)
		return
	}
	b.toClaude.put(append(line, '\n'), "")
}

func (b *bridge) respond(r shimwire.Response) { b.emit(r) }

// emit queues f for the shim, encoded and checked as shimwire's Writer writes a frame.
func (b *bridge) emit(f shimwire.Frame) {
	if err := b.frames.WriteFrame(f); err != nil {
		b.log.Printf("could not write a %s frame to the shim: %v", f.FrameType(), err)
		return
	}
	b.toShim.put(b.encoded.take(), f.FrameType())
}

// emitRaw queues v for the shim as one frame line; a line over the frame limit is a pane summary
// the shim goes without.
func (b *bridge) emitRaw(v map[string]any) {
	line, err := json.Marshal(v)
	if err == nil {
		err = b.frames.WriteLine(line)
	}
	switch {
	case err == nil:
		b.toShim.put(b.encoded.take(), "")
	case !errors.Is(err, shimwire.ErrLineTooLong):
		b.log.Printf("could not write to the shim: %v", err)
	}
}

// lineCopy is where frames writes each line it encodes: a copy of the line, which take hands over
// for the shim's queue to own.
type lineCopy struct{ line []byte }

func (c *lineCopy) Write(p []byte) (int, error) {
	c.line = bytes.Clone(p)
	return len(p), nil
}

func (c *lineCopy) take() []byte {
	line := c.line
	c.line = nil
	return line
}
