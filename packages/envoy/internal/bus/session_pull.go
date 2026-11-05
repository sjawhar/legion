// A pull of one session consumer's messages (SessionConsumers.Pull): the pull requests it sends,
// the messages it hands its caller and how each is answered.

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// SessionAcker answers one pulled message (SessionMessage).
type SessionAcker interface {
	// Ack removes the message from the consumer: it is done.
	Ack() error
	// InProgress restarts the message's ack wait.
	InProgress() error
	// NakWithDelay has the server deliver the message again once delay has passed.
	NakWithDelay(delay time.Duration) error
}

// SessionMessage is one message a session consumer delivered: the stored message and its delivery.
type SessionMessage struct {
	// Subject is the subject the message was stored on.
	Subject string
	Header  nats.Header
	Data    []byte
	// StreamSeq is the message's stream sequence.
	StreamSeq uint64
	// Delivered is how many times the consumer has delivered the message, this time included.
	Delivered uint64
	// StoredAt is when the stream stored the message.
	StoredAt time.Time
	SessionAcker
}

// The pull requests of a SessionPull.
const (
	// sessionPullBatch is how many messages one pull request asks for: the consumer's
	// MaxAckPending, the most it delivers before an ack anyway.
	sessionPullBatch = SessionConsumerMaxAckPending
	// sessionPullExpires is how long a pull request waits at the server; the pull sends the next
	// once it expires.
	sessionPullExpires = 30 * time.Second
	// sessionPullHeartbeat is how often the server signals a pull request that waits with nothing
	// to deliver. A pull that hears nothing from the server for twice as long ends with
	// ErrSessionPullStalled.
	sessionPullHeartbeat = 15 * time.Second
)

// ErrSessionPullStalled ends a pull (SessionPull.Next) that heard nothing from the server, no
// message and no heartbeat, for twice sessionPullHeartbeat.
var ErrSessionPullStalled = errors.New("bus: the session consumer's pull heard nothing from the server")

// ErrSessionPullReconnected ends a pull (SessionPull.Next) whose connection reconnected: the
// server drops a pull request with the connection it was sent on, and the pull would hear nothing
// on the new one, so it ends as soon as the client reports a connection event
// (Client.ConnectionChanged) rather than after the twice sessionPullHeartbeat of silence that
// would otherwise find it.
var ErrSessionPullReconnected = errors.New("bus: the session consumer's pull request was lost when its connection reconnected")

// SessionPull is a pull of one session consumer's messages, which arrive in stream order, a
// redelivery included. It sends the consumer's pull requests ($JS.API.CONSUMER.MSG.NEXT) itself,
// one at a time, each for sessionPullBatch messages, and the next once the server has delivered
// them all or the request expired, so starting it is one publish and no API call. The server
// answers on the pull's own inbox, which holds one subscription.
type SessionPull struct {
	// client is the SessionConsumers' client: the pull reads its connection events
	// (Client.ConnectionChanged) to tell a request the server still holds from one a connection
	// event dropped (lostRequest).
	client *Client
	conn   *nats.Conn
	sub    *nats.Subscription
	// name is the consumer's, next its pull request subject, inbox the subject the server answers
	// on, and request the pull request's body.
	name, next, inbox string
	request           []byte
	// remaining is how many messages the pull request in flight may still deliver; 0 when none is.
	// Only Next reads or writes it.
	remaining int
	// heartbeat is the pull requests' idle heartbeat (sessionPullHeartbeat).
	heartbeat time.Duration
	// reconnects is conn's reconnect count when the pull's first request went out
	// (nats.Conn.Stats): the count moves on a reconnect in place, which drops the request while
	// the connection stays the same one.
	reconnects uint64
	// changed is the client's connection-event channel (Client.ConnectionChanged), taken before
	// the connection the pull request goes out on was read, so an event between the two closes
	// it. Only Next's goroutine reads or replaces it (take).
	changed <-chan struct{}
	// wake holds a token once receive has queued a message for Next.
	wake chan struct{}

	// mu guards queue and heard.
	mu sync.Mutex
	// queue holds what the server sent on the inbox, heartbeats aside, until Next reads it.
	queue []*nats.Msg
	// heard is when the server last sent anything on the inbox, or the pull last sent a request.
	heard time.Time
}

// Pull starts pulling sessionID's consumer on the connection, without reading it first: the first
// Next returns ErrSessionConsumerNotFound when the stream holds no such consumer. Pulled messages
// wait in the pull for Next; Stop ends it, and the server delivers any message it delivered and
// was not acked for again once its ack wait has passed, or at a restart (RestartAtFloor).
func (s *SessionConsumers) Pull(ctx context.Context, sessionID string) (*SessionPull, error) {
	name, err := sessionConsumerNameOf(sessionID)
	if err != nil {
		return nil, err
	}
	if err := s.client.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	request, err := json.Marshal(struct {
		Batch     int           `json:"batch"`
		Expires   time.Duration `json:"expires"`
		Heartbeat time.Duration `json:"idle_heartbeat"`
	}{Batch: sessionPullBatch, Expires: s.pullExpires, Heartbeat: s.pullHeartbeat})
	if err != nil {
		return nil, err
	}
	// The channel is taken before the connection and its reconnect count are read, so an event
	// between the two ends the pull rather than leaving it waiting on a request the server
	// dropped. An event before the request, which nats.go then sends on the new connection, ends
	// it too: the cost is one restart at the ack floor, where the cost of missing the event is a
	// stalled relay.
	changed := s.client.ConnectionChanged()
	conn := s.client.Connection()
	p := &SessionPull{client: s.client, conn: conn, name: name, next: consumerAPI("MSG.NEXT", name), inbox: conn.NewInbox(), request: request, heartbeat: s.pullHeartbeat, reconnects: conn.Stats().Reconnects, changed: changed, wake: make(chan struct{}, 1)}
	if p.sub, err = conn.Subscribe(p.inbox, p.receive); err != nil {
		return nil, err
	}
	if err := p.pull(); err != nil {
		p.Stop()
		return nil, err
	}
	return p, nil
}

// receive takes what the server sends on the pull's inbox: a heartbeat only marks it heard, and
// anything else is queued for Next.
func (p *SessionPull) receive(msg *nats.Msg) {
	p.mu.Lock()
	p.heard = time.Now()
	if status, _ := pullStatus(msg); status != pullStatusHeartbeat {
		p.queue = append(p.queue, msg)
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// The status codes the server answers a pull request with, beside the messages it delivers.
const (
	pullStatusHeartbeat    = "100"
	pullStatusExpired      = "408"
	pullStatusConflict     = "409"
	pullStatusNoResponders = "503"
	// pullLeadershipChange is the description of the 409 that ends a waiting pull request when the
	// consumer's leader changes; the consumer still exists.
	pullLeadershipChange = "Leadership Change"
)

// pullStatus is the status code and description of msg, a status the server sent on a pull's
// inbox, or "" for a message the consumer delivered, which carries its ack subject as its reply
// subject (and may carry any header of its own).
func pullStatus(msg *nats.Msg) (status, description string) {
	if msg.Reply != "" {
		return "", ""
	}
	return msg.Header.Get("Status"), msg.Header.Get("Description")
}

// pull sends a pull request for sessionPullBatch messages.
func (p *SessionPull) pull() error {
	p.mu.Lock()
	p.heard = time.Now()
	p.mu.Unlock()
	if err := p.conn.PublishRequest(p.next, p.inbox, p.request); err != nil {
		return err
	}
	p.remaining = sessionPullBatch
	return nil
}

// Next returns the next message the pull holds, waiting for one until ctx ends. It returns
// ErrSessionConsumerNotFound once the stream holds no such consumer, ErrSessionPullReconnected
// once the connection the pull request was sent on reconnected, and ErrSessionPullStalled once
// the server has sent nothing for twice sessionPullHeartbeat.
func (p *SessionPull) Next(ctx context.Context) (SessionMessage, error) {
	for {
		if p.remaining == 0 {
			if err := p.pull(); err != nil {
				return SessionMessage{}, err
			}
		}
		msg, err := p.take(ctx)
		if err != nil {
			return SessionMessage{}, err
		}
		switch status, description := pullStatus(msg); {
		case status == "":
			p.remaining--
			return sessionMessageOf(msg)
		case status == pullStatusExpired, status == pullStatusConflict && description == pullLeadershipChange:
			// The request ended; the next round sends another.
			p.remaining = 0
		case status == pullStatusNoResponders:
			return SessionMessage{}, fmt.Errorf("%w: %s", ErrSessionConsumerNotFound, p.name)
		default:
			return SessionMessage{}, fmt.Errorf("bus: pull of session consumer %s: %s %s", p.name, status, description)
		}
	}
}

// take returns the first message the server sent that Next has not read, waiting until one comes,
// ctx ends, a connection event dropped the pull's request (lostRequest), or the server has sent
// nothing for twice sessionPullHeartbeat. The messages the server already sent are handed out
// first: they are the consumer's, not the lost request's, and acking one is a publish like any
// other.
func (p *SessionPull) take(ctx context.Context) (*nats.Msg, error) {
	for {
		p.mu.Lock()
		if len(p.queue) > 0 {
			msg := p.queue[0]
			p.queue[0] = nil
			p.queue = p.queue[1:]
			p.mu.Unlock()
			return msg, nil
		}
		stalled := time.Until(p.heard.Add(2 * p.heartbeat))
		p.mu.Unlock()
		if p.lost() {
			return nil, ErrSessionPullReconnected
		}
		if stalled <= 0 {
			return nil, ErrSessionPullStalled
		}
		timer := time.NewTimer(stalled)
		select {
		case <-p.wake:
		case <-p.changed:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
		timer.Stop()
	}
}

// lost reports whether a connection event of the client (ConnectionChanged) has dropped the pull's
// request. An event that left the request's connection the client's current one, open, and with
// its reconnect count unmoved is none of the pull's (lostRequest): it is the moment between a
// reconnect nats.go has installed, which Pull read, and the callback that reports it. The pull
// waits on past such an event, on the next channel.
func (p *SessionPull) lost() bool {
	for {
		select {
		case <-p.changed:
		default:
			return false
		}
		if p.lostRequest() {
			return true
		}
		p.changed = p.client.ConnectionChanged()
	}
}

// lostRequest reports whether the pull's request is lost: the connection it went out on can no
// longer answer it, because the server drops the request when that connection is replaced by
// another the client dialled, when it is closed, and when it reconnects in place, which leaves
// the connection the same one and moves its reconnect count. A disconnect alone ends no pull: the
// pull ends at the reconnect that follows it, as the relay can pull again only then, and by that
// time the server has dropped the old connection's request. A pull ended at the disconnect could
// have its caller publish or pull while the server still held that request, and a message the
// server delivered to it would wait out the consumer's ack wait.
func (p *SessionPull) lostRequest() bool {
	return p.client.Connection() != p.conn || p.conn.IsClosed() || p.conn.Stats().Reconnects != p.reconnects
}

// sessionMessageOf is the message the server delivered as msg, whose reply subject carries its
// delivery (the JetStream ack subject).
func sessionMessageOf(msg *nats.Msg) (SessionMessage, error) {
	meta, err := msg.Metadata()
	if err != nil {
		return SessionMessage{}, err
	}
	return SessionMessage{
		Subject:      msg.Subject,
		Header:       msg.Header,
		Data:         msg.Data,
		StreamSeq:    meta.Sequence.Stream,
		Delivered:    meta.NumDelivered,
		StoredAt:     meta.Timestamp,
		SessionAcker: sessionAck{msg},
	}, nil
}

// sessionAck answers a pulled message on the connection it arrived on, by publishing to its reply
// subject; Ack and NakWithDelay are sent once.
type sessionAck struct{ msg *nats.Msg }

func (a sessionAck) Ack() error                             { return a.msg.Ack() }
func (a sessionAck) InProgress() error                      { return a.msg.InProgress() }
func (a sessionAck) NakWithDelay(delay time.Duration) error { return a.msg.NakWithDelay(delay) }

// Stop ends the pull; the messages it held and Next had not returned are delivered again later.
//
// On a live connection it waits, at most sessionPullStopFlush, for the server to confirm the
// unsubscribe (a flush), so the pull's request has no subscriber left by the time Stop returns.
// The server skips a waiting request whose reply subject nobody subscribes to, which is what lets
// the caller's restart keep a consumer with nothing awaiting an ack (RestartAtFloor) and pull
// again. Without the wait, the server could take a message stored after Stop for the old request
// while the unsubscribe was still on its way, and that message would wait out the consumer's ack
// wait. That happens after a reconnect in place, whose resend gave the old inbox a subscription
// on the new connection. On a connection that is down the unsubscribe only drops the
// subscription from the client, which then sends it to no server.
func (p *SessionPull) Stop() {
	_ = p.sub.Unsubscribe()
	if p.conn.IsConnected() {
		_ = p.conn.FlushTimeout(sessionPullStopFlush)
	}
}

// sessionPullStopFlush bounds Stop's wait for the server to confirm a pull's unsubscribe.
const sessionPullStopFlush = 2 * time.Second
