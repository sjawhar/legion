package claudebridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// PhaseStall is where a phase worker's phase stands between the daemon's assignment and a
// successful `handoff_complete`, the state machine of packages/pi-legion/src/phase-stall.ts that
// the bridge runs for a Claude Code worker, since no Oh My Pi hook runs there:
//   - closed: no assignment since the last successful completion, or none seen yet;
//   - open: assigned and not completed, so a run that settles now gets the follow-up;
//   - text-call: still open, and this stall's follow-up went to a final message holding a tool
//     call written as text, so a run that settles on such a call again gets one more, the last;
//   - quiet: still open, and this stall already had its follow-ups or a WAITING reply, so nothing
//     more is sent until the next inbound event or assignment.
//
// A stall is the span between two inbound events (an assignment or an Envoy delivery), which no
// follow-up causes, so it gets at most two follow-ups whatever the model answers.
type PhaseStall string

const (
	StallClosed   PhaseStall = "closed"
	StallOpen     PhaseStall = "open"
	StallTextCall PhaseStall = "text-call"
	StallQuiet    PhaseStall = "quiet"
)

// waitingReply is a line of the final message that starts with WAITING (after any markdown emphasis
// or quoting); toolCallShaped is the opening of a tool call written as text in the Anthropic invoke
// markup, with or without a namespace prefix. Both are phase-stall.ts's, verbatim.
var (
	waitingReply   = regexp.MustCompile(`(?m)^\W*WAITING\b`)
	toolCallShaped = regexp.MustCompile(`<(?:[\w-]+:)?(?:function_calls\b|invoke\s+name\s*=|parameter\s+name\s*=)`)
)

// FollowUp is the model-visible follow-up a run that settles with its phase open gets: the text of
// phase-stall.ts's FOLLOW_UP, held identical to it by a test.
const FollowUp = "Your turn ended with your Legion phase still open: the `legion` tool's `handoff_complete` has " +
	"not succeeded since your assignment. If your phase's work is done, call the `legion` tool now " +
	"with op `handoff_complete` and the fields your role's instructions give. If you are waiting on " +
	"something (CI, a review, a person), reply with a line starting WAITING that names what you are " +
	"waiting on; nothing more will be sent until a new event arrives."

// textToolCall leads the follow-up when the settled message holds a tool call written as text:
// phase-stall.ts's TEXT_TOOL_CALL.
const textToolCall = "Your last message holds a tool call written as text, so it did not run: make it a real tool call. "

// assigned is the daemon's prompt arriving: the phase is open.
func (PhaseStall) assigned() PhaseStall { return StallOpen }

// completed is a `handoff_complete` that succeeded: the phase is closed.
func (PhaseStall) completed() PhaseStall { return StallClosed }

// inbound is an Envoy delivery arriving: a quiet or text-call phase is open again, since the event
// may be what it waited on.
func (s PhaseStall) inbound() PhaseStall {
	if s == StallQuiet || s == StallTextCall {
		return StallOpen
	}
	return s
}

// settle is a run about to end with lastAssistantText as its final message: the next state, and the
// follow-up to send as one more turn, or "" for none. A stall's one follow-up, unless that one
// answered a tool call written as text: then a second text call gets one more, since the call it
// failed to make is still the next step.
func (s PhaseStall) settle(lastAssistantText string) (PhaseStall, string) {
	if s != StallOpen && s != StallTextCall {
		return s, ""
	}
	if waitingReply.MatchString(lastAssistantText) {
		return StallQuiet, ""
	}
	textCall := toolCallShaped.MatchString(lastAssistantText)
	switch {
	case s == StallTextCall && textCall:
		return StallQuiet, textToolCall + FollowUp
	case s == StallTextCall:
		return StallQuiet, ""
	case textCall:
		return StallTextCall, textToolCall + FollowUp
	}
	return StallQuiet, FollowUp
}

type stallRecord struct {
	State PhaseStall `json:"state"`
}

// readStall is the state path holds, or closed when there is no file: a session before the daemon's
// first assignment. A file that does not hold a state is an error naming it.
func readStall(path string) (PhaseStall, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return StallClosed, nil
	}
	if err != nil {
		return "", fmt.Errorf("read the phase-stall state: %w", err)
	}
	var record stallRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", fmt.Errorf("read the phase-stall state %s: %w", path, err)
	}
	switch record.State {
	case StallClosed, StallOpen, StallTextCall, StallQuiet:
		return record.State, nil
	}
	return "", fmt.Errorf("read the phase-stall state %s: %q is not closed, open, text-call or quiet", path, record.State)
}

// writeStall replaces path with state, through a temporary file renamed into place, so a relaunch
// never reads half a write.
func writeStall(path string, state PhaseStall) error {
	raw, err := json.Marshal(stallRecord{State: state})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".legion-phase-stall-*")
	if err != nil {
		return fmt.Errorf("write the phase-stall state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write the phase-stall state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write the phase-stall state: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write the phase-stall state: %w", err)
	}
	return nil
}
