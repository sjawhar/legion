// Package agentstream relays a live agent session's own conversation — its turns, tool calls and
// streamed output — from the session to a human watching it in Dispatch (LEGION-232).
//
// Nothing here is stored. A frame is forwarded to whoever has the session open and then
// forgotten: no database row, no file, and no log line carrying a frame body. Tool output can
// hold anything the tool printed, secrets included, so the relay treats every frame as opaque
// bytes it never reads into and never records. The transport is core NATS on a subject family
// the notification stream does not capture, so the bus retains nothing either.
package agentstream

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// Frame is one frame exactly as the session published it. Its shape belongs to the session and
// the dashboard (packages/contracts/src/agent-stream.ts); the relay never parses it.
type Frame []byte

// controlReplay asks a session for the history it can still replay; controlWatch tells it a
// viewer is attached, which is what licenses it to publish at all. Both mirror
// AgentStreamControlMessage in packages/contracts/src/agent-stream.ts.
const (
	controlReplay = `{"v":1,"type":"replay"}`
	controlWatch  = `{"v":1,"type":"watch"}`
)

// Source is where a viewer's frames come from.
type Source interface {
	// Subscribe delivers each frame the session publishes until stop is called. deliver runs on
	// the transport's own goroutine and must not block.
	Subscribe(sessionID string, deliver func(Frame)) (stop func(), err error)
	// Replay asks the session for what it can still replay. A session answering with nothing
	// returns a nil frame and no error: an empty history is an answer. A session with nobody
	// listening on its control subject returns ErrNoResponder, which is a different statement
	// and the only thing that tells a session that cannot stream from one that has not spoken
	// yet - every plugin release that can stream answers, and none that cannot does.
	Replay(ctx context.Context, sessionID string) (Frame, error)
	// Watch tells the session a viewer is attached. A session hears nothing for long enough
	// stops publishing, so a viewer that stays open keeps calling it.
	Watch(sessionID string) error
}

// ErrNoResponder is a replay request nobody was listening for. It is not a failure of the relay
// and not an empty history: it says this session is not on the live view's control subject.
var ErrNoResponder = errors.New("agent stream: no responder on the session's control subject")

// NATS is the production Source: core NATS, never JetStream.
type NATS struct {
	conn *nats.Conn
}

// NewNATS returns a Source over conn. conn should be a connection of the relay's own (bus.Dial),
// so a viewer's subscription cannot be lost to another component replacing a shared one.
func NewNATS(conn *nats.Conn) *NATS {
	return &NATS{conn: conn}
}

func (n *NATS) Subscribe(sessionID string, deliver func(Frame)) (func(), error) {
	subscription, err := n.conn.Subscribe(contracts.AgentStreamFramesSubject(sessionID), func(message *nats.Msg) {
		// nats.go reuses the message buffer, so the frame is copied before it leaves the callback.
		frame := make(Frame, len(message.Data))
		copy(frame, message.Data)
		deliver(frame)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe to agent stream: %w", err)
	}
	return func() { _ = subscription.Unsubscribe() }, nil
}

func (n *NATS) Replay(ctx context.Context, sessionID string) (Frame, error) {
	reply, err := n.conn.RequestWithContext(ctx, contracts.AgentStreamControlSubject(sessionID), []byte(controlReplay))
	switch {
	case err == nil:
		frame := make(Frame, len(reply.Data))
		copy(frame, reply.Data)
		return frame, nil
	case errors.Is(err, nats.ErrNoResponders):
		// Nobody is listening on the session's control subject.
		return nil, ErrNoResponder
	case errors.Is(err, nats.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		// Somebody may well be there and slow: the viewer gets the live stream with no history
		// behind it, which is what a session that just started offers anyway.
		return nil, nil
	default:
		return nil, fmt.Errorf("request agent stream replay: %w", err)
	}
}

func (n *NATS) Watch(sessionID string) error {
	if err := n.conn.Publish(contracts.AgentStreamControlSubject(sessionID), []byte(controlWatch)); err != nil {
		return fmt.Errorf("publish agent stream watch: %w", err)
	}
	return nil
}
