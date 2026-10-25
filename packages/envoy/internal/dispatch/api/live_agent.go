package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
)

// The agent view is one live session's own conversation on a page's live socket: its turns, tool
// calls and streamed output, relayed from the session itself (agentstream), one agent frame each.
// It replaced GET /api/v1/agents/{session_id}/stream, the server-sent stream, and keeps every rule
// that stream kept (agent_stream.go): a person's alone, nothing written, no frame's bytes logged.
//
// A subscription subscribes to the session's frames before anything else, which is what makes the
// replay safe to stitch on: a frame the session publishes while the replay request is in flight
// lands in the subscription's buffer rather than falling between the two. It then tells the session
// a viewer is attached, since a session publishes only while one is, and asks for the replay. All
// of it runs on the subscription's own goroutine, the replay's wait of up to
// agentStreamReplayTimeout and every re-arm included, so the socket's read loop never waits on a
// session.
//
// A viewer that cannot keep up ends its subscription (behind) rather than losing a frame. The relay
// never parses a frame, so it cannot tell a superseded snapshot from a tool result or the settled
// frame of an earlier message, and dropping the wrong one leaves a tool call reading "running…"
// until the page reloads. Ending costs the page a subscribe, and the subscribe rebuilds from the
// session's own replay, which is exactly the state that was lost.

// The events an agent frame names: whether anyone answers on the session's control subject, the
// history the session can still replay, and one frame it published.
const (
	agentEventResponder = "responder"
	agentEventReplay    = "replay"
	agentEventFrame     = "frame"
)

// startAgentView checks an agent view, then starts the view's subscription. Only a person may
// watch a session's conversation, and the view names its kind and a session uuid alone; a
// deployment with no relay (no NATS) has nothing to watch.
func (s *server) startAgentView(ctx context.Context, stream liveStreamer, sub string, view LiveView, _ *EventsLiveResume) (liveSubscription, *LiveError) {
	if !stream.Human() {
		refusal := liveRefusal(sub, "HUMAN_ONLY", "only users may watch an agent's conversation")
		return nil, &refusal
	}
	if view.Window != nil || view.Query != nil || view.At != nil {
		refusal := liveRefusal(sub, "INVALID_FRAME", "an agent view names its kind and its session_id and nothing else")
		return nil, &refusal
	}
	raw := ""
	if view.SessionID != nil {
		raw = *view.SessionID
	}
	sessionID, problem := agentStreamSessionID(raw)
	if problem != "" {
		refusal := liveRefusal(sub, "SESSION_ID_INPUT", problem)
		return nil, &refusal
	}
	if s.deps.AgentStream == nil {
		refusal := liveRefusal(sub, "STREAM_UNAVAILABLE", "this deployment has no agent conversation relay (it needs NATS)")
		return nil, &refusal
	}
	return s.startAgentSubscription(ctx, stream, sub, sessionID), nil
}

// startAgentSubscription starts sub's view of sessionID's conversation on a goroutine of its own and
// returns at once. The goroutine writes the subscription's frames through stream until ctx ends or
// the subscription is stopped: the responder verdict first, then the session's replay when it has
// one, then each frame the session publishes, and an ended frame should the view fall behind
// (behind), or the relay drop the viewer or fail it (dropped).
func (s *server) startAgentSubscription(ctx context.Context, stream liveStreamer, sub string, sessionID string) liveSubscription {
	return runSubscription(ctx, func(ctx context.Context) { s.relayAgentConversation(ctx, stream, sub, sessionID) })
}

// relayAgentConversation is one agent subscription's whole life: it returns once the subscription
// has ended, on its own or because ctx did.
func (s *server) relayAgentConversation(ctx context.Context, stream liveStreamer, sub, sessionID string) {
	source := s.deps.AgentStream
	frames := make(chan agentstream.Frame, agentStreamBuffer)
	// behind closes once a frame found the buffer full; dropped once the relay ended this viewer,
	// which only the in-memory source's test hook does (agentstream.Memory.Drop): a session's own
	// frame is never nil. The e2e suite has no other way to make a page rebuild its transcript from a
	// second replay.
	behind, dropped := make(chan struct{}), make(chan struct{})
	var behindOnce, droppedOnce sync.Once
	unsubscribe, err := source.Subscribe(sessionID, func(frame agentstream.Frame) {
		if frame == nil {
			droppedOnce.Do(func() { close(dropped) })
			return
		}
		select {
		case frames <- frame:
		default:
			behindOnce.Do(func() { close(behind) })
		}
	})
	if err != nil {
		endAgentAfterFailure(ctx, stream, sub, "subscribe to the session's frames", err)
		return
	}
	var stopRelayOnce sync.Once
	stopRelay := func() { stopRelayOnce.Do(unsubscribe) }
	defer stopRelay()
	// The session publishes only while a viewer is attached, so it has to hear from this one
	// before the first turn, not after it.
	if err := source.Watch(sessionID); err != nil {
		stopRelay()
		endAgentAfterFailure(ctx, stream, sub, "tell the session a viewer is attached", err)
		return
	}

	skipped := 0
	defer func() {
		if skipped > 0 {
			slog.Warn("dispatch: skipped agent frames that are not JSON", "skipped", skipped)
		}
	}()
	// send writes data, which the socket carries as JSON it never parses, as sub's agent frame named
	// event. A session's bytes that are not JSON could not be carried at all (the writer would close
	// the socket), so they are skipped and counted. It is false once the subscription has ended.
	send := func(event string, data []byte) bool {
		if !json.Valid(data) {
			skipped++
			return true
		}
		return stream.SendWait(ctx, liveAgentFrame(sub, event, data)) == nil
	}
	// answer says whether anyone is listening on the session's control subject, which says whether
	// it can stream at all - every plugin release that can answers, and one that predates the live
	// view never does - and then sends the history it answered with. A session that answers with
	// nothing has simply not spoken yet, and a replay that failed for any other reason leaves the
	// viewer the live conversation with no history behind it.
	answer := func(replay agentstream.Frame, responding bool) bool {
		if !send(agentEventResponder, responderEvent(responding)) {
			return false
		}
		return len(replay) == 0 || send(agentEventReplay, replay)
	}
	replay, responding := s.replayAgentStream(ctx, source, sessionID)
	if !answer(replay, responding) {
		return
	}

	// endedBy is why the view has ended, or "": it fell behind far enough to lose a frame, or the
	// relay dropped it as a lost connection does.
	endedBy := func() string {
		select {
		case <-behind:
			return liveEndedBehind
		default:
		}
		select {
		case <-dropped:
			return liveEndedDropped
		default:
			return ""
		}
	}
	rearm := time.NewTicker(s.agentWatchInterval)
	defer rearm.Stop()
	for {
		var frame agentstream.Frame
		select {
		case <-ctx.Done():
			return
		case <-behind:
		case <-dropped:
		case frame = <-frames:
		case <-rearm.C:
			if err := source.Watch(sessionID); err != nil {
				stopRelay()
				endAgentAfterFailure(ctx, stream, sub, "re-arm the session", err)
				return
			}
			if responding {
				continue
			}
			// The session was not on its control subject when this viewer arrived. It may be now -
			// a session restarted onto a plugin that streams - so every re-arm asks again, and the
			// first answer delivers its history without the page subscribing again.
			retry, answered := s.replayAgentStream(ctx, source, sessionID)
			if !answered {
				continue
			}
			responding = true
			if !answer(retry, true) {
				return
			}
			continue
		}
		// A frame goes only while nothing has ended the view: draining what is buffered after the
		// frame the full buffer lost would stitch a gap into the middle of the conversation, and the
		// page's subscribe replays it whole, which is the honest answer.
		if reason := endedBy(); reason != "" {
			_ = stream.SendWait(ctx, liveEnded(sub, reason))
			return
		}
		if !send(agentEventFrame, frame) {
			return
		}
	}
}

// endAgentAfterFailure ends a view whose relay call failed with an ended frame (dropped), so its
// page subscribes again rather than watching a session that never hears of it. It waits
// agentStreamRetry first: a page answers an ended frame by subscribing again at once, and a relay
// that keeps failing would otherwise have the page and the subscription take turns as fast as the
// failure comes back. A view whose own context ended (stopped, or its socket closed) sends nothing.
func endAgentAfterFailure(ctx context.Context, stream liveStreamer, sub, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.Warn("dispatch: an agent view could not "+what+"; it ends, and its page subscribes again", "error", err)
	if !waitFor(ctx, agentStreamRetry) {
		return
	}
	_ = stream.SendWait(ctx, liveEnded(sub, liveEndedDropped))
}

// liveAgentFrame is sub's agent frame named event, carrying data: the session's own JSON, or the
// responder verdict Dispatch writes (responderEvent).
func liveAgentFrame(sub, event string, data []byte) LiveAgentFrame {
	return LiveAgentFrame{Type: "agent", Sub: sub, Event: event, Data: data}
}
