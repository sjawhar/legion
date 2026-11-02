package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The settings of every session consumer (SessionConsumers).
const (
	// SessionConsumerAckWait is how long the server waits for a pulled message's ack, or its next
	// InProgress, before it delivers the message again.
	SessionConsumerAckWait = 60 * time.Second
	// SessionConsumerMaxAckPending is how many of a session's messages can be delivered and not yet
	// acked at once; the server delivers no more until one is.
	SessionConsumerMaxAckPending = 1024
	// SessionConsumerInactiveThreshold is how long a session consumer outlives its last pull: the
	// stream's retention, so a session that comes back while the stream still holds what it missed
	// resumes where it stopped.
	SessionConsumerInactiveThreshold = contracts.DeliveryDuplicateWindow
)

// sessionConsumerPrefix begins the name of every session consumer (SessionConsumerName).
const sessionConsumerPrefix = "s-"

// ErrConsumerNotGranted is wrapped, with the server's refusal (ErrPublishDenied), by a session
// consumer call the connection's NATS user may not make: a hub whose grant predates session
// consumers refuses their info, create and delete. The listener answers such a session's
// registration with core delivery.
var ErrConsumerNotGranted = errors.New("bus: the NATS user may not manage session consumers")

// ErrSessionConsumerNotFound is returned by a call on a session consumer the stream does not hold:
// never created, deleted, or removed by the server after SessionConsumerInactiveThreshold.
var ErrSessionConsumerNotFound = errors.New("bus: no session consumer")

// ErrNoSessionConsumerName is returned for a session id no consumer can be named after
// (SessionConsumerName).
var ErrNoSessionConsumerName = errors.New("bus: a session id no consumer can be named after")

// ErrNoStreamID is returned for a stream that carries no StreamIDMetadataKey metadata (StreamID).
var ErrNoStreamID = fmt.Errorf("bus: %s carries no %s metadata", Stream, StreamIDMetadataKey)

// SessionConsumerName is the name of session sessionID's consumer on the stream, s-<sessionID>, and
// whether it has one: a session id that is empty or holds a `.`, `*`, `>`, whitespace, `/` or `\`
// would make no valid consumer name, or one that reaches into another API subject, so such a session
// gets no consumer.
func SessionConsumerName(sessionID string) (string, bool) {
	if sessionID == "" || strings.ContainsAny(sessionID, `.*>/\`) || strings.ContainsFunc(sessionID, unicode.IsSpace) {
		return "", false
	}
	return sessionConsumerPrefix + sessionID, true
}

// sessionConsumerNameOf is SessionConsumerName(sessionID), or ErrNoSessionConsumerName for a session
// that can have no consumer.
func sessionConsumerNameOf(sessionID string) (string, error) {
	name, ok := SessionConsumerName(sessionID)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoSessionConsumerName, sessionID)
	}
	return name, nil
}

// SessionConsumers manages the stream's session consumers, one durable pull consumer per session
// (SessionConsumerName), and pulls their messages, on a connection of its own (ConnectSessionConsumers).
//
// Each consumer's info, create and delete are requests the bus confirms as a role forward's
// publish is confirmed (requestConfirmed), so a call the NATS user may not make returns
// ErrConsumerNotGranted as soon as the server refuses it; the jetstream package's own requests
// wait out their whole timeout for an answer a refused request never gets. The configuration and
// the answers are the jetstream package's types. A pull (SessionPull) sends its own pull
// requests, so starting one costs no API call.
type SessionConsumers struct {
	client *Client
	// ackWait is SessionConsumerAckWait, shortened by a test that waits out a redelivery.
	ackWait time.Duration
	// pullExpires and pullHeartbeat are sessionPullExpires and sessionPullHeartbeat, shortened by
	// a test of a pull's later requests.
	pullExpires, pullHeartbeat time.Duration
	// pendingMu guards pending and createFault.
	pendingMu sync.Mutex
	// pending holds, by consumer name, the start of each consumer a restart (RestartAtFloor) read
	// the floor of and may have deleted, until a create at that start is confirmed: the position
	// the consumer must come back at, whichever call creates it next.
	pending map[string]uint64
	// createFault, when not nil, runs before each consumer create is sent, and an error it returns
	// fails the create unsent. It is nil outside tests (SetCreateFault).
	createFault func(consumer string) error

	// streamMu guards streamNext and streamReading (streamState).
	streamMu sync.Mutex
	// streamNext is the stream read the streamState calls made since the last one began wait for;
	// nil when none waits. streamReading is set while a goroutine sends the reads.
	streamNext    *streamRead
	streamReading bool
}

// ConnectSessionConsumers opens SessionConsumers on a Client of its own, named
// "envoy-session-consumers" unless options name it: it connects as Connect does, as the NATS user,
// through the proxy and under the remote-NATS rule the environment names, reconnects forever, and
// dials a replacement for a connection nats.go gave up on. A pull (Pull) started on a connection
// that was replaced ends with an error, and its caller pulls again.
func ConnectSessionConsumers(urls []string, options ...ConnectOption) (*SessionConsumers, error) {
	client, err := Connect(urls, append([]ConnectOption{WithName("envoy-session-consumers")}, options...)...)
	if err != nil {
		return nil, err
	}
	return &SessionConsumers{client: client, ackWait: SessionConsumerAckWait, pullExpires: sessionPullExpires, pullHeartbeat: sessionPullHeartbeat}, nil
}

// Close closes the connection; a pull in progress ends.
func (s *SessionConsumers) Close() { s.client.Close() }

// SetCreateFault makes every consumer create call fault(consumer name) first and fail, unsent,
// with the error it returns; nil removes it. It is a seam for tests, which fail the create of a
// restart after its delete; nothing else sets it.
func (s *SessionConsumers) SetCreateFault(fault func(consumer string) error) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.createFault = fault
}

// Drain drains the connection, letting acks already sent reach the server, within timeout.
func (s *SessionConsumers) Drain(timeout time.Duration) error { return s.client.Drain(timeout) }

// Publish publishes msg over core NATS on the connection, waiting for a connection nats.go is
// replacing until ctx ends: a relay frame on a session's DeliverSubject, which the stream does not
// store, so it travels on the connection the frame's message was pulled and is answered on.
func (s *SessionConsumers) Publish(ctx context.Context, msg *nats.Msg) error {
	if err := s.client.ensureConnWithContext(ctx); err != nil {
		return err
	}
	return s.client.currentConn().PublishMsg(msg)
}

// SubscribeReplies subscribes handler over core NATS to subject, typically a reply-inbox wildcard,
// on the connection, and keeps the subscription across a reconnect and on a connection dialled to
// replace a closed one (Client.SubscribeCore). It is the connection's one core subscription: a
// second call on another subject is refused with ErrSubscriptionHeld. It returns once the server
// holds the subscription.
func (s *SessionConsumers) SubscribeReplies(subject string, handler nats.MsgHandler) error {
	if _, err := s.client.SubscribeCore(subject, handler); err != nil {
		return err
	}
	return s.client.currentConn().Flush()
}

// SessionConsumerStart is what Ensure or RestartAtFloor did to a session's consumer.
type SessionConsumerStart struct {
	// Created is set when the call created the consumer.
	Created bool
	// StartSeq is the stream sequence the consumer starts at: the one a consumer the call created
	// starts at, and, for one it found and updated, the one it was created at (an update may not
	// change it, and a restart that recreated it reports where it started again). Every message at
	// or after it is the consumer's to deliver, so the listener records it on the session's entry
	// (session.SessionEntry.ConsumerStart).
	StartSeq uint64
	// Gap is set when StartSeq is below the stream's first sequence: messages the consumer would
	// have delivered from StartSeq have aged out of the stream or been discarded.
	Gap bool
	// StreamID is the stream's identity (StreamIDMetadataKey) as the call read it, when it created
	// the consumer; "" when the stream carries none.
	StreamID string
}

// Ensure makes sessionID's consumer carry the session consumer settings and the filters subjects
// reduce to (ReduceFilterSubjects), and creates it when the stream holds none.
//
// An existing consumer's filters and settings are updated, changing nothing when both already
// match, and it keeps its position: the next stream sequence it examines. A newly followed subject
// starts there. The server moves that position only when the consumer looks for its next message,
// at each pull request and each message stored on one of its filters (nats-server v2.10.29 signals
// a consumer only for those), so a message stored on the new subject after the consumer last
// looked, and before the update, is delivered too.
//
// A missing consumer is created starting at stream sequence startSeq, or, when startSeq is 0, at
// the sequence after the stream's last, which delivers what the stream stores from then on, as
// DeliverNew does, and gives the consumer a start its ack floor never reads below (RestartAtFloor).
// A consumer missing because a restart of it (RestartAtFloor) on this SessionConsumers deleted it,
// or may have, and did not confirm its create, is created at the start that restart read instead,
// whatever startSeq is, so the session loses nothing it had not acked.
func (s *SessionConsumers) Ensure(ctx context.Context, sessionID string, subjects []string, startSeq uint64) (SessionConsumerStart, error) {
	name, filters, err := sessionConsumerTarget(sessionID, subjects)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	for attempt := 0; ; attempt++ {
		info, err := s.info(ctx, name)
		if errors.Is(err, ErrSessionConsumerNotFound) {
			start := startSeq
			if pending, ok := s.pendingStart(name); ok {
				start = pending
			}
			created, err := s.create(ctx, name, filters, start)
			// A consumer that appeared since the info, created by another call, is updated instead.
			if errors.Is(err, jetstream.ErrConsumerExists) && attempt == 0 {
				continue
			}
			if err == nil {
				s.setPendingStart(name, 0)
			}
			return created, err
		}
		if err != nil {
			return SessionConsumerStart{}, err
		}
		// The consumer exists, so it holds its own position: the delete of a restart's call that
		// failed never reached the server, or its create did. Its start is the sequence it was
		// created at, which the caller records for the durable path (SessionConsumerStart).
		s.setPendingStart(name, 0)
		return SessionConsumerStart{StartSeq: info.Config.OptStartSeq}, s.update(ctx, name, info.Config, filters)
	}
}

// UpdateFilter makes the existing consumer of sessionID filter on what subjects reduce to, keeping
// its position (Ensure); it returns ErrSessionConsumerNotFound when the session has none.
func (s *SessionConsumers) UpdateFilter(ctx context.Context, sessionID string, subjects []string) error {
	name, filters, err := sessionConsumerTarget(sessionID, subjects)
	if err != nil {
		return err
	}
	info, err := s.info(ctx, name)
	if err != nil {
		return err
	}
	return s.update(ctx, name, info.Config, filters)
}

// RestartAtFloor restarts sessionID's consumer at its ack floor: it reads the stream sequence up to
// which the consumer has every message acked (ackFloor), deletes the consumer and creates it again
// starting at the sequence after it, filtering on what subjects reduce to, so the consumer delivers
// again, in stream order, every message it delivered and was not acked for, and the ones acked out
// of order after them. It returns ErrSessionConsumerNotFound when the session has no consumer.
// nats-server v2.10.29 has no consumer position reset (no CONSUMER.RESET API; an update refuses a changed deliver policy or start sequence), so the restart deletes and creates.
//
// A consumer with nothing awaiting an ack has nothing to deliver again, and the sequence it
// examines next is already the one after its floor (ackFloor reads its last delivery then), so it
// is kept: its filters and settings are updated in place, as Ensure updates them, and the start
// returned is that next sequence, with Created unset. No delete and no create are sent, so the
// session is never without a consumer, and the server writes no consumer to its store. A waiting
// pull request of a pull that ended is no reason to restart: the server skips one whose reply
// subject nobody subscribes to any more (nats-server v2.10.29 consumer.nextWaiting).
//
// A restart that deletes makes the floor's next sequence the consumer's pending start until a
// create at it is confirmed. It is recorded before the delete, so a delete that failed, its answer
// lost while the server applied it, leaves it as surely as a create that failed after a delete that
// succeeded.
// The next RestartAtFloor that finds the consumer missing creates it at the pending start rather
// than returning ErrSessionConsumerNotFound, and so does an Ensure (Ensure). A consumer still there
// (a delete the server never applied, a create it applied whose answer was lost) restarts at its
// own floor, which is the pending start or past it. A consumer missing with no pending start (never
// created, or removed after SessionConsumerInactiveThreshold) is ErrSessionConsumerNotFound, as
// before.
//
// The pending start is this SessionConsumers' own, in memory. Its rule holds only while every call
// for one session through it waits for the one before (the listener holds a lock per session
// around each): two at once could read and change the consumer between each other's info, delete
// and create. Another SessionConsumers knows nothing of it, whether a second listener's or the one
// a listener killed between a delete and its create opens when it starts again: while the consumer
// is missing its Ensure creates it at the start it is given or the stream's end, its restart reads
// the consumer as missing, and its calls are not ordered with this one's.
func (s *SessionConsumers) RestartAtFloor(ctx context.Context, sessionID string, subjects []string) (SessionConsumerStart, error) {
	name, filters, err := sessionConsumerTarget(sessionID, subjects)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	// The stream's state is read alongside the consumer's info, from a read the restarts at once
	// share (streamState); only a restart that keeps the consumer waits for it.
	stream := s.joinStreamRead()
	var startSeq uint64
	info, err := s.info(ctx, name)
	switch {
	case err == nil && info.NumAckPending == 0:
		return s.keepAtFloor(ctx, name, info, filters, stream)
	case err == nil:
		startSeq = ackFloor(info) + 1
		s.setPendingStart(name, startSeq)
		if err := s.delete(ctx, name); err != nil {
			return SessionConsumerStart{}, err
		}
	case errors.Is(err, ErrSessionConsumerNotFound):
		pending, ok := s.pendingStart(name)
		if !ok {
			return SessionConsumerStart{}, err
		}
		startSeq = pending
	default:
		return SessionConsumerStart{}, err
	}
	start, err := s.create(ctx, name, filters, startSeq)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	s.setPendingStart(name, 0)
	return start, nil
}

// keepAtFloor is RestartAtFloor of consumer name, read as info, when nothing it delivered awaits an
// ack: it updates the consumer's filters and settings in place, drops any pending start (the
// consumer exists, so it holds its own position), and returns the sequence the consumer examines
// next as the start, with the stream's identity and gap as stream reads them.
func (s *SessionConsumers) keepAtFloor(ctx context.Context, name string, info *jetstream.ConsumerInfo, filters []string, stream *streamRead) (SessionConsumerStart, error) {
	state, err := stream.wait(ctx)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	if err := s.update(ctx, name, info.Config, filters); err != nil {
		return SessionConsumerStart{}, err
	}
	s.setPendingStart(name, 0)
	startSeq := ackFloor(info) + 1
	return SessionConsumerStart{StartSeq: startSeq, Gap: startSeq < state.firstSeq, StreamID: state.id}, nil
}

// pendingStart is consumer name's pending start (RestartAtFloor), and whether it has one.
func (s *SessionConsumers) pendingStart(name string) (uint64, bool) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	start, ok := s.pending[name]
	return start, ok
}

// setPendingStart records start as consumer name's pending start (RestartAtFloor), or drops the
// one it has when start is 0.
func (s *SessionConsumers) setPendingStart(name string, start uint64) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if start == 0 {
		delete(s.pending, name)
		return
	}
	if s.pending == nil {
		s.pending = map[string]uint64{}
	}
	s.pending[name] = start
}

// ackFloor is the stream sequence up to which consumer info has every message it delivered acked.
// The server's own AckFloor.Stream reads low: nats-server v2.10.29 reports 0 for a consumer until
// an ack moves it (a new consumer's stored state sets only its delivered sequence), so a consumer
// that has delivered nothing, or whose first delivery is still unacked, would read as acked up to
// nothing, and a restart there would deliver the stream from its first message. So the floor is
// also never below the sequence before the consumer's start (every session consumer is created at
// a start sequence, Ensure), and, while nothing it delivered awaits an ack, it is the last sequence
// it delivered.
func ackFloor(info *jetstream.ConsumerInfo) uint64 {
	floor := info.AckFloor.Stream
	if info.Config.OptStartSeq > 0 {
		floor = max(floor, info.Config.OptStartSeq-1)
	}
	if info.NumAckPending == 0 {
		floor = max(floor, info.Delivered.Stream)
	}
	return floor
}

// Info returns sessionID's consumer as the server reports it, or ErrSessionConsumerNotFound.
func (s *SessionConsumers) Info(ctx context.Context, sessionID string) (*jetstream.ConsumerInfo, error) {
	name, err := sessionConsumerNameOf(sessionID)
	if err != nil {
		return nil, err
	}
	return s.info(ctx, name)
}

// ListSessionConsumers reads every session consumer the stream holds, by session id, with the
// paged consumer list ($JS.API.CONSUMER.LIST; nats-server v2.10.29 answers 256 consumers a page),
// so a listener that starts relaying every session at once reads them in a few requests rather
// than one info each (KeepListed). A list the NATS user may not make is ErrConsumerNotGranted.
func (s *SessionConsumers) ListSessionConsumers(ctx context.Context) (map[string]*jetstream.ConsumerInfo, error) {
	listed := map[string]*jetstream.ConsumerInfo{}
	for offset := 0; ; {
		var page struct {
			Total     int                       `json:"total"`
			Consumers []*jetstream.ConsumerInfo `json:"consumers"`
			Error     *jetstream.APIError       `json:"error"`
		}
		if err := s.consumerRequest(ctx, "$JS.API.CONSUMER.LIST."+Stream, struct {
			Offset int `json:"offset"`
		}{offset}, &page); err != nil {
			return nil, err
		}
		if page.Error != nil {
			return nil, fmt.Errorf("bus: list the consumers of %s: %w", Stream, page.Error)
		}
		for _, info := range page.Consumers {
			if sessionID, ok := strings.CutPrefix(info.Name, sessionConsumerPrefix); ok {
				listed[sessionID] = info
			}
		}
		offset += len(page.Consumers)
		if len(page.Consumers) == 0 || offset >= page.Total {
			return listed, nil
		}
	}
}

// KeepListed is RestartAtFloor of sessionID's consumer, as info (ListSessionConsumers) listed it,
// when that restart would send nothing: nothing it delivered awaits an ack, its filters and
// settings are already what subjects reduce to, and no restart left it a pending start. It then
// returns the start RestartAtFloor's keep returns (the sequence after the floor, the stream's
// identity and the gap, from a stream read shared as streamState shares it) and true, with no API
// call of its own. Otherwise it returns false and the caller restarts the consumer
// (RestartAtFloor). The caller holds the session's lock, as for RestartAtFloor.
func (s *SessionConsumers) KeepListed(ctx context.Context, sessionID string, subjects []string, info *jetstream.ConsumerInfo) (SessionConsumerStart, bool, error) {
	name, filters, err := sessionConsumerTarget(sessionID, subjects)
	if err != nil {
		return SessionConsumerStart{}, false, err
	}
	if info == nil || info.Name != name || info.NumAckPending != 0 {
		return SessionConsumerStart{}, false, nil
	}
	if _, changed := s.updated(info.Config, filters); changed {
		return SessionConsumerStart{}, false, nil
	}
	if _, pending := s.pendingStart(name); pending {
		return SessionConsumerStart{}, false, nil
	}
	state, err := s.streamState(ctx)
	if err != nil {
		return SessionConsumerStart{}, false, err
	}
	startSeq := ackFloor(info) + 1
	return SessionConsumerStart{StartSeq: startSeq, Gap: startSeq < state.firstSeq, StreamID: state.id}, true, nil
}

// StreamID returns the stream's identity, its StreamIDMetadataKey metadata, which a relay frame
// carries and a session's resume point names.
func (s *SessionConsumers) StreamID(ctx context.Context) (string, error) {
	state, err := s.streamState(ctx)
	if err != nil {
		return "", err
	}
	if state.id == "" {
		return "", ErrNoStreamID
	}
	return state.id, nil
}

// sessionConsumerTarget names sessionID's consumer and reduces subjects to its filters, refusing a
// session that can have no consumer and a filter set that is empty, which would deliver the whole
// stream.
func sessionConsumerTarget(sessionID string, subjects []string) (string, []string, error) {
	name, err := sessionConsumerNameOf(sessionID)
	if err != nil {
		return "", nil, err
	}
	filters := ReduceFilterSubjects(subjects)
	if len(filters) == 0 {
		return "", nil, fmt.Errorf("bus: no filter subject for the consumer of session %q", sessionID)
	}
	return name, filters, nil
}

// applySettings sets the session consumer settings and filters on config, leaving what an update
// may not change (its name, deliver policy and start sequence, ack policy) as it is.
func (s *SessionConsumers) applySettings(config *jetstream.ConsumerConfig, filters []string) {
	config.AckPolicy = jetstream.AckExplicitPolicy
	config.AckWait = s.ackWait
	config.MaxAckPending = SessionConsumerMaxAckPending
	config.MaxDeliver = -1
	config.InactiveThreshold = SessionConsumerInactiveThreshold
	config.FilterSubject = ""
	config.FilterSubjects = filters
}

// update sends consumer name's current configuration with the session consumer settings and
// filters applied, when that changes anything.
func (s *SessionConsumers) update(ctx context.Context, name string, current jetstream.ConsumerConfig, filters []string) error {
	next, changed := s.updated(current, filters)
	if !changed {
		return nil
	}
	_, err := s.upsert(ctx, name, next, "update")
	return err
}

// updated is current with the session consumer settings and filters applied, and whether that
// changes anything an update sends.
func (s *SessionConsumers) updated(current jetstream.ConsumerConfig, filters []string) (jetstream.ConsumerConfig, bool) {
	next := current
	s.applySettings(&next, filters)
	same := next.AckWait == current.AckWait &&
		next.MaxAckPending == current.MaxAckPending &&
		next.MaxDeliver == current.MaxDeliver &&
		next.InactiveThreshold == current.InactiveThreshold &&
		next.FilterSubject == current.FilterSubject &&
		slices.Equal(next.FilterSubjects, current.FilterSubjects)
	return next, !same
}

// create creates consumer name filtering on filters, starting at stream sequence startSeq, or at the
// sequence after the stream's last when startSeq is 0 (Ensure).
func (s *SessionConsumers) create(ctx context.Context, name string, filters []string, startSeq uint64) (SessionConsumerStart, error) {
	state, err := s.streamState(ctx)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	if startSeq == 0 {
		startSeq = state.lastSeq + 1
	}
	config := jetstream.ConsumerConfig{
		Durable:       name,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   startSeq,
	}
	s.applySettings(&config, filters)
	s.pendingMu.Lock()
	fault := s.createFault
	s.pendingMu.Unlock()
	if fault != nil {
		if err := fault(name); err != nil {
			return SessionConsumerStart{}, err
		}
	}
	if _, err := s.upsert(ctx, name, config, "create"); err != nil {
		return SessionConsumerStart{}, err
	}
	return SessionConsumerStart{Created: true, StartSeq: startSeq, Gap: startSeq < state.firstSeq, StreamID: state.id}, nil
}

// consumerAPI is the JetStream API subject of operation op on consumer name of the stream.
func consumerAPI(op, name string) string {
	return "$JS.API.CONSUMER." + op + "." + Stream + "." + name
}

// consumerResponse is the JetStream API's answer to a consumer's create, update or info.
type consumerResponse struct {
	jetstream.ConsumerInfo
	Error *jetstream.APIError `json:"error"`
}

// upsert sends config to the server as consumer name's create or update (action).
func (s *SessionConsumers) upsert(ctx context.Context, name string, config jetstream.ConsumerConfig, action string) (*jetstream.ConsumerInfo, error) {
	request := struct {
		Stream string                    `json:"stream_name"`
		Config *jetstream.ConsumerConfig `json:"config"`
		Action string                    `json:"action"`
	}{Stream: Stream, Config: &config, Action: action}
	var response consumerResponse
	if err := s.consumerRequest(ctx, consumerAPI("CREATE", name), request, &response); err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, fmt.Errorf("bus: %s session consumer %s: %w", action, name, response.Error)
	}
	return &response.ConsumerInfo, nil
}

// info reads consumer name, or returns ErrSessionConsumerNotFound.
func (s *SessionConsumers) info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	var response consumerResponse
	if err := s.consumerRequest(ctx, consumerAPI("INFO", name), nil, &response); err != nil {
		return nil, err
	}
	if response.Error != nil {
		if response.Error.ErrorCode == jetstream.JSErrCodeConsumerNotFound {
			return nil, fmt.Errorf("%w: %s", ErrSessionConsumerNotFound, name)
		}
		return nil, fmt.Errorf("bus: read session consumer %s: %w", name, response.Error)
	}
	return &response.ConsumerInfo, nil
}

// delete deletes consumer name; one the stream does not hold is no error.
func (s *SessionConsumers) delete(ctx context.Context, name string) error {
	var response struct {
		Error *jetstream.APIError `json:"error"`
	}
	if err := s.consumerRequest(ctx, consumerAPI("DELETE", name), nil, &response); err != nil {
		return err
	}
	if response.Error != nil && response.Error.ErrorCode != jetstream.JSErrCodeConsumerNotFound {
		return fmt.Errorf("bus: delete session consumer %s: %w", name, response.Error)
	}
	return nil
}

// consumerRequest sends request, JSON or nothing when nil, to a consumer API subject and decodes the
// answer into response. A request the server refuses under the connection's grant is
// ErrConsumerNotGranted.
func (s *SessionConsumers) consumerRequest(ctx context.Context, subject string, request, response any) error {
	var data []byte
	if request != nil {
		var err error
		if data, err = json.Marshal(request); err != nil {
			return err
		}
	}
	reply, err := s.client.requestConfirmed(ctx, subject, data)
	if errors.Is(err, ErrPublishDenied) {
		return fmt.Errorf("%w: %w", ErrConsumerNotGranted, err)
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(reply.Data, response); err != nil {
		return fmt.Errorf("bus: read the answer of %s: %w", subject, err)
	}
	return nil
}
