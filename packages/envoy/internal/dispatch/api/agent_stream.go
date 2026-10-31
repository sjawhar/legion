package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

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
	// Frames held for a page that is reading slower than the session is producing. A view that
	// fills it ends (behind) rather than losing a frame, and the page's subscribe rebuilds from the
	// session's replay.
	agentStreamBuffer = 64
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
	sessionID, problem := agentStreamSessionID(r.PathValue("session_id"))
	if problem != "" {
		writeError(w, "SESSION_ID_INPUT", http.StatusBadRequest, problem)
		return
	}
	switch r.URL.Query().Get("as") {
	case "replay":
		memory.SetReplay(sessionID, frame)
	case "drop":
		// Ends every agent view of this session as dropped, as a lost connection does. The page
		// subscribes again, which asks the session for its replay again and rebuilds the whole
		// transcript from whatever `as=replay` set by then.
		memory.Drop(sessionID)
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
