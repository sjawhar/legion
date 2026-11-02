// The listener's check of whether a session's plugin still holds a NATS connection: the two words
// an answer gives for a session, the reply shape both sides of the check write against, and the
// one core request the send route makes for it.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// The two words the send route's answer and GET /v1/registry/<session_id>?include_stopped=1 give
// for a session. Connected is a session whose plugin answered the presence check, one whose entry
// declares no check, and one this listener may not ask about; stopped is a session Envoy knows and
// no plugin of which was shown to be there. Nothing else is a state: a session neither store knows
// is the 404 both routes have always answered.
const (
	sessionStateConnected = "connected"
	sessionStateStopped   = "stopped"
)

// presenceReply is what a session's plugin answers on its presence subject (bus.PresenceSubject),
// and the one definition of that wire shape: this listener decodes it here, and each plugin
// (packages/pi-envoy and packages/claude-envoy, through packages/envoy-client) answers it. A reply
// is a JSON object of two string fields:
//
//	{"session_id": "ses_abc123", "client_instance_id": "ci_7f20"}
//
// SessionID is the session the answering plugin speaks for, and it is the only field compared: a
// reply naming the probed session says a plugin of it holds a NATS connection, which is the whole
// question. ClientInstanceID is the client instance holding that connection
// (session.SessionEntry.ClientInstanceID); it is logged and never compared, so a predecessor
// process of the same session that still holds the subscription reads connected rather than
// stopped - something is there to take the message now. It is empty for a client that mints no
// instance id.
//
// A reply that does not decode as this, or one naming another session, is not a connected answer:
// probeSessionPresence treats it as stopped and logs it, since such a reply shows nothing about
// the session that was asked about.
type presenceReply struct {
	SessionID        string `json:"session_id"`
	ClientInstanceID string `json:"client_instance_id,omitempty"`
}

// presenceProbeTimeout bounds the one core request the send route makes on a target's presence
// subject. A second is the whole wait: a plugin holding the subscription answers from memory in
// well under it, and a deployment whose live-topics lane subscribes to every notification topic
// (ENVOY_SUBSCRIBE_ALL_TOPICS=1) has a subscriber on every presence subject, so NATS answers no
// no-responders status there and this wait is the only thing that can report a session gone. The
// probe runs beside the sender stamp, under the same second of sendRequestBudget
// (senderStampReadTimeout), so spending it costs the send nothing it did not already spend.
const presenceProbeTimeout = 1 * time.Second

// probeSessionPresence asks sessionID's own plugin, on that session's presence subject, whether it
// still holds a NATS connection, and reports whether it was shown to. The request is one core
// NATS request bounded by presenceProbeTimeout within ctx (bus.Client.RequestCore).
//
// A reply naming sessionID (presenceReply) is connected. nats.ErrNoResponders, nothing within the
// second, a reply that does not decode and a reply naming another session are each stopped. A
// publish the server denies under this listener's own grant (bus.ErrPublishDenied) is connected,
// at once and logged: that answer is about this listener's NATS user, not about the session, and a
// listener that may not ask must not call every session of the deployment stopped.
func probeSessionPresence(ctx context.Context, client *bus.Client, sessionID string, logger *logging.Logger) bool {
	probeCtx, cancel := context.WithTimeout(ctx, presenceProbeTimeout)
	defer cancel()
	reply, err := client.RequestCore(probeCtx, bus.PresenceSubject(sessionID), nil)
	switch {
	case errors.Is(err, bus.ErrPublishDenied):
		logger.Warn("listener presence check denied under its own grant; the session counts as connected",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return true
	case errors.Is(err, nats.ErrNoResponders) || errors.Is(err, context.DeadlineExceeded):
		// Nothing of this session is listening, or nothing answered within the second: the
		// ordinary way a stopped session reads stopped, and no line of its own.
		return false
	case err != nil:
		logger.Warn("listener presence check failed; the session counts as stopped",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return false
	}
	var answer presenceReply
	if err := json.Unmarshal(reply.Data, &answer); err != nil {
		logger.Warn("listener presence reply could not be read; the session counts as stopped",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return false
	}
	if answer.SessionID != sessionID {
		logger.Warn("listener presence reply names another session; the session counts as stopped",
			slog.String("session_id", sessionID), slog.String("answered_session_id", answer.SessionID))
		return false
	}
	logger.Info("listener presence check answered",
		slog.String("session_id", sessionID), slog.String("client_instance_id", answer.ClientInstanceID))
	return true
}
