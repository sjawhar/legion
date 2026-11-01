package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
)

// A session's own conversation, relayed to a person watching it through an agent view on the live
// socket (live_agent.go). It is the most sensitive thing Dispatch ever carries - a tool result
// holds whatever the tool printed - so the view is a person's alone, writes nothing to the
// database, and logs no frame's bytes. What it forwards it forgets.
const (
	// How long a viewer waits for the session's own replay before settling for the live frames.
	// Mirrors AGENT_STREAM_REPLAY_TIMEOUT_MS in packages/contracts/src/agent-stream.ts.
	agentStreamReplayTimeout = 2 * time.Second
	// How often an attached viewer re-arms the session. Well inside the session's own watch
	// window (AGENT_STREAM_WATCH_TTL_MS), so a live viewer never lets it lapse.
	agentStreamWatchInterval = 10 * time.Second
	// The most frame bytes a view holds for a page that is reading slower than the session is
	// producing, counted as each frame's payload length from the moment it is queued until it has
	// been handed to the socket's stream (liveSocketStream, 64 frames), which is where a page that
	// stops reading backs up next. A session publishes a message's snapshot at most every 100 ms
	// and a view queues for up to agentStreamReplayTimeout before its first write, so 20 snapshots
	// of 400 KiB fit; NATS's 1 MiB max_payload caps one frame, so a single frame always fits. A
	// view that fills it ends (behind) rather than losing a frame, and the page's subscribe
	// rebuilds from the session's replay.
	agentStreamBufferBytes = 8 << 20
	// How long a view whose relay call failed waits before it ends (dropped), so a relay that keeps
	// failing - a NATS connection that is reconnecting - is not asked again as fast as the page can
	// subscribe.
	agentStreamRetry = time.Second
)

// agentStreamSessionID is the one place a caller-supplied session id becomes part of a NATS
// subject. An Envoy session id is a UUID, and anything else is refused here rather than
// concatenated: `*` or `>` in that position is a wildcard, and one view naming it would subscribe
// a single viewer to every armed session on the bus at once. It answers the session id, or why raw
// is none.
func agentStreamSessionID(raw string) (string, string) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", "session_id is required"
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", "session_id must be a session uuid"
	}
	return id, ""
}

// agentStreamQueue holds the frames a view has been delivered and not yet written, in the order
// they arrived, within agentStreamBufferBytes.
type agentStreamQueue struct {
	// wake has one slot: a push fills it, and the view takes every queued frame when it fires.
	wake chan struct{}
	// overflow closes once, when a frame would take the view past its budget.
	overflow chan struct{}

	mu     sync.Mutex
	frames []agentstream.Frame
	// queued is the bytes of the queued frames; size is those plus the bytes taken and not yet
	// released.
	queued     int
	size       int
	overflowed bool
}

func newAgentStreamQueue() *agentStreamQueue {
	return &agentStreamQueue{wake: make(chan struct{}, 1), overflow: make(chan struct{})}
}

// push queues frame, or ends the view when frame would take it past its budget. Once the view has
// overflowed it queues nothing more and holds nothing. It never blocks: it runs on the transport's
// own goroutine.
func (q *agentStreamQueue) push(frame agentstream.Frame) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.overflowed {
		return
	}
	if q.size+len(frame) > agentStreamBufferBytes {
		q.overflowed = true
		q.frames, q.queued = nil, 0
		close(q.overflow)
		return
	}
	q.frames = append(q.frames, frame)
	q.queued += len(frame)
	q.size += len(frame)
	signal(q.wake)
}

// take hands over every queued frame in order, and their bytes. Those bytes stay counted against
// the budget until release, so the budget bounds what the view holds while the frames are being
// queued on the socket's stream too. After an overflow it hands over nothing.
func (q *agentStreamQueue) take() ([]agentstream.Frame, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	frames, size := q.frames, q.queued
	q.frames, q.queued = nil, 0
	return frames, size
}

// release returns the bytes of frames take handed over once they are queued on the socket's
// stream (liveSocketStream), which is where the writer takes them from.
func (q *agentStreamQueue) release(size int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.size -= size
}

// replayAgentStream asks the session for its history, reporting separately whether anyone
// answered at all. Only ErrNoResponder means nobody did: a timeout leaves the question open
// (somebody may be there and slow), and the viewer is not told a live session cannot stream on
// the strength of one slow answer.
func (s *server) replayAgentStream(
	ctx context.Context, source agentstream.Source, sessionID string,
) (agentstream.Frame, bool) {
	replayCtx, cancel := context.WithTimeout(ctx, agentStreamReplayTimeout)
	defer cancel()
	replay, err := source.Replay(replayCtx, sessionID)
	if errors.Is(err, agentstream.ErrNoResponder) {
		return nil, false
	}
	if err != nil {
		return nil, true
	}
	return replay, true
}

// responderEvent is the stream's `responder` payload; see AgentStreamResponder in
// packages/contracts/src/agent-stream.ts.
func responderEvent(responding bool) []byte {
	if responding {
		return []byte(`{"v":1,"responding":true}`)
	}
	return []byte(`{"v":1,"responding":false}`)
}

// publishAgentStreamFrame injects one frame into a session's viewers on every process. Mounted
// only with Deps.TestHooksEnabled: the e2e harness runs Dispatch with NATS disabled, so a browser
// test of the conversation view has no session and no bus to take frames from, and each process
// serves its viewers from an in-process source of its own (runTestHookEverywhere carries the
// frame to each). Whether this deployment's relay takes frames from a test hook at all is judged
// here, on the process the call reached.
func (s *server) publishAgentStreamFrame(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	if _, ok := s.deps.AgentStream.(*agentstream.Memory); !ok {
		writeError(w, "STREAM_UNAVAILABLE", http.StatusServiceUnavailable,
			"this deployment's relay takes frames from sessions, not from a test hook")
		return
	}
	frame, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONRequestBytes))
	if err != nil {
		writeError(w, "FRAME_INPUT", http.StatusBadRequest, "could not read the frame")
		return
	}
	// A frame is JSON text, and a notification carries text: a byte sequence that is not UTF-8
	// would reach the other processes altered rather than as it was sent.
	if !utf8.Valid(frame) {
		writeError(w, "FRAME_INPUT", http.StatusBadRequest, "a frame is UTF-8 text")
		return
	}
	sessionID, problem := agentStreamSessionID(r.PathValue("session_id"))
	if problem != "" {
		writeError(w, "SESSION_ID_INPUT", http.StatusBadRequest, problem)
		return
	}
	call := testHookCall{Hook: hookPublishFrame, Session: sessionID}
	switch as := r.URL.Query().Get("as"); as {
	case "replay":
		call.As, call.Frame = as, string(frame)
	case "drop":
		// Ends every agent view of this session as dropped, as a lost connection does. The page
		// subscribes again, which asks the session for its replay again and rebuilds the whole
		// transcript from whatever `as=replay` set by then.
		call.As = as
	case "responder":
		// Whether anyone is on the session's control subject, with no history behind it: a live
		// session that has produced nothing, or one whose plugin cannot stream at all.
		var body struct {
			Responding bool `json:"responding"`
		}
		if err := json.Unmarshal(frame, &body); err != nil {
			writeError(w, "FRAME_INPUT", http.StatusBadRequest, "responder takes {\"responding\": bool}")
			return
		}
		call.As, call.Responding = as, body.Responding
	default:
		call.Frame = string(frame)
	}
	s.runTestHookEverywhere(w, r, call)
}
