package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
)

// The viewer's stream. A session's own conversation is the most sensitive thing Dispatch ever
// carries — a tool result holds whatever the tool printed — so this route is humans-only, writes
// nothing to the database, and logs no frame body. What it forwards it forgets.
const (
	// How long a viewer waits for the session's own replay before settling for the live stream.
	// Mirrors AGENT_STREAM_REPLAY_TIMEOUT_MS in packages/contracts/src/agent-stream.ts.
	agentStreamReplayTimeout = 2 * time.Second
	// How often an attached viewer re-arms the session. Well inside the session's own watch
	// window (AGENT_STREAM_WATCH_TTL_MS), so a live viewer never lets it lapse.
	agentStreamWatchInterval = 10 * time.Second
	agentStreamHeartbeat     = 15 * time.Second
	// Frames held for a browser that is reading slower than the session is producing. Each frame
	// is a whole-message snapshot, so when this fills the oldest is dropped: the newest snapshot
	// of a message is the one that renders correctly, and an older one it supersedes is worth
	// nothing.
	agentStreamBuffer = 64
)

// agentStreamSessionID is the one place a caller-supplied session id becomes part of a NATS
// subject. An Envoy session id is a UUID, and anything else is refused here rather than
// concatenated: `*` or `>` in that position is a wildcard, and one request carrying it would
// subscribe a single viewer to every armed session on the bus at once.
func agentStreamSessionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("session_id"))
	if raw == "" {
		writeError(w, "SESSION_ID_INPUT", http.StatusBadRequest, "session_id is required")
		return "", false
	}
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, "SESSION_ID_INPUT", http.StatusBadRequest,
			"session_id must be a session uuid")
		return "", false
	}
	return raw, true
}

func (s *server) streamAgentConversation(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	sessionID, ok := agentStreamSessionID(w, r)
	if !ok {
		return
	}
	source := s.deps.AgentStream
	if source == nil {
		writeError(w, "STREAM_UNAVAILABLE", http.StatusServiceUnavailable,
			"this deployment has no agent conversation relay (it needs NATS)")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, "STREAM_UNSUPPORTED", http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Subscribing before anything else is what makes the replay below safe to stitch on: a frame
	// published while the replay request is in flight lands in this buffer rather than falling
	// between the two.
	//
	// A viewer that cannot keep up ends its stream rather than losing a frame. The relay never
	// parses a frame, so it cannot tell a superseded snapshot from a tool result or the settled
	// frame of an earlier message, and dropping the wrong one leaves a tool call reading
	// "running…" until the page is reloaded. Closing here costs a reconnect, and the reconnect
	// rebuilds from the session's own replay, which is exactly the state that was lost.
	frames := make(chan agentstream.Frame, agentStreamBuffer)
	overflow := make(chan struct{})
	var overflowOnce sync.Once
	stop, err := source.Subscribe(sessionID, func(frame agentstream.Frame) {
		select {
		case frames <- frame:
		default:
			overflowOnce.Do(func() { close(overflow) })
		}
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer stop()
	// The session publishes only while a viewer is attached, so it has to hear from this one
	// before the first turn, not after it.
	if err := source.Watch(sessionID); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Two questions, and the viewer needs both answers. Whether anyone is listening on the
	// session's control subject says whether it can stream at all - every plugin release that
	// can answers, and one that predates the live view never does - and a session that answers
	// with nothing has simply not spoken yet. A replay that failed for any other reason is not
	// a failed stream: the viewer gets the live conversation with no history behind it.
	replay, responding := s.replayAgentStream(r.Context(), source, sessionID)
	if err := writeStreamEvent(w, "responder", responderEvent(responding)); err != nil {
		return
	}
	flusher.Flush()
	if len(replay) > 0 {
		if err := writeStreamEvent(w, "replay", replay); err != nil {
			return
		}
		flusher.Flush()
	}

	watch := time.NewTicker(agentStreamWatchInterval)
	defer watch.Stop()
	heartbeat := time.NewTicker(agentStreamHeartbeat)
	defer heartbeat.Stop()
	shutdown := s.lifetime().Done()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-shutdown:
			// The server is shutting down: as streamEvents does, end the stream rather than hold
			// http.Server.Shutdown open. The reconnect rebuilds from the session's own replay.
			return
		case <-overflow:
			// The viewer fell behind far enough to lose a frame. Draining what is buffered
			// first would stitch a gap into the middle of the conversation; the reconnect's
			// replay is the honest answer.
			return
		case frame := <-frames:
			if err := writeStreamEvent(w, "frame", frame); err != nil {
				return
			}
			flusher.Flush()
		case <-watch.C:
			if err := source.Watch(sessionID); err != nil {
				return
			}
			if responding {
				continue
			}
			// The session was not on its control subject when this viewer arrived. It may be
			// now - a session restarted onto a plugin that streams - so every re-arm asks
			// again, and the first answer delivers its history without the viewer reloading.
			retry, answered := s.replayAgentStream(r.Context(), source, sessionID)
			if !answered {
				continue
			}
			responding = true
			if err := writeStreamEvent(w, "responder", responderEvent(true)); err != nil {
				return
			}
			if len(retry) > 0 {
				if err := writeStreamEvent(w, "replay", retry); err != nil {
					return
				}
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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

// publishAgentStreamFrame injects one frame into a session's viewers. Mounted only with
// Deps.TestHooksEnabled: the e2e harness runs Dispatch with NATS disabled, so a browser test of
// the conversation view has no session and no bus to take frames from.
func (s *server) publishAgentStreamFrame(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	memory, ok := s.deps.AgentStream.(*agentstream.Memory)
	if !ok {
		writeError(w, "STREAM_UNAVAILABLE", http.StatusServiceUnavailable,
			"this deployment's relay takes frames from sessions, not from a test hook")
		return
	}
	frame, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONRequestBytes))
	if err != nil {
		writeError(w, "FRAME_INPUT", http.StatusBadRequest, "could not read the frame")
		return
	}
	sessionID, valid := agentStreamSessionID(w, r)
	if !valid {
		return
	}
	switch r.URL.Query().Get("as") {
	case "replay":
		memory.SetReplay(sessionID, frame)
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
		memory.SetResponder(sessionID, body.Responding)
	default:
		memory.Publish(sessionID, frame)
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeStreamEvent writes one SSE event whose data is a frame this server never reads into. A
// frame is JSON and carries no raw newline, but the split keeps the wire format correct for one
// that somehow does rather than truncating the event at it.
func writeStreamEvent(w http.ResponseWriter, event string, data []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n")
	return err
}
