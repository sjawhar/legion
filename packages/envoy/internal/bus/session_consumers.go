package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// ReduceFilterSubjects returns subjects as a set of consumer filters no two of which overlap, which
// JetStream requires of one consumer's filters, matching every subject any of them matched: a
// subject another contains (SubjectContains) is dropped, and two that overlap without either
// containing the other are replaced by their longest common prefix of whole tokens followed by `>`
// (`>` alone when the first tokens differ), until no two overlap. `X` and `X.>` do not overlap (one
// matches X alone, the other only what lies under X), so both stay. The result is sorted and holds
// each subject once, so the same set always reduces to the same list.
func ReduceFilterSubjects(subjects []string) []string {
	reduced := slices.Clone(subjects)
	for {
		slices.Sort(reduced)
		reduced = slices.Compact(reduced)
		next, changed := reduceFilterPair(reduced)
		if !changed {
			return reduced
		}
		reduced = next
	}
}

// reduceFilterPair applies ReduceFilterSubjects's rule to the first pair of subjects that overlap,
// reporting false when none do.
func reduceFilterPair(subjects []string) ([]string, bool) {
	for i, a := range subjects {
		for j := i + 1; j < len(subjects); j++ {
			b := subjects[j]
			switch {
			case SubjectContains(a, b):
				return slices.Delete(slices.Clone(subjects), j, j+1), true
			case SubjectContains(b, a):
				return slices.Delete(slices.Clone(subjects), i, i+1), true
			case subjectsOverlap(a, b):
				merged := commonTokenPrefix(a, b) + ">"
				rest := slices.DeleteFunc(slices.Clone(subjects), func(s string) bool { return s == a || s == b })
				return append(rest, merged), true
			}
		}
	}
	return subjects, false
}

// commonTokenPrefix is the tokens a and b begin with alike, up to the first that differs or is `>`,
// with the dot after the last of them, or "" when their first tokens differ.
func commonTokenPrefix(a, b string) string {
	aTokens, bTokens := strings.Split(a, "."), strings.Split(b, ".")
	var prefix strings.Builder
	for index := 0; index < len(aTokens) && index < len(bTokens); index++ {
		if aTokens[index] != bTokens[index] || aTokens[index] == ">" {
			break
		}
		prefix.WriteString(aTokens[index] + ".")
	}
	return prefix.String()
}

// SessionConsumers manages the stream's session consumers, one durable pull consumer per session
// (SessionConsumerName), and pulls their messages, on a connection of its own (ConnectSessionConsumers).
//
// Each consumer's info, create and delete are requests the bus confirms as RequestCoreTo confirms its
// forward (requestConfirmed), so a call the NATS user may not make returns ErrConsumerNotGranted as
// soon as the server refuses it; the jetstream package's own requests wait out their whole timeout
// for an answer a refused request never gets. The configuration and the answers are the jetstream
// package's types, and pulling is its pull consumer.
type SessionConsumers struct {
	client *Client
	// ackWait is SessionConsumerAckWait, shortened by a test that waits out a redelivery.
	ackWait time.Duration
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
	return &SessionConsumers{client: client, ackWait: SessionConsumerAckWait}, nil
}

// Close closes the connection; a pull in progress ends.
func (s *SessionConsumers) Close() { s.client.Close() }

// Drain drains the connection, letting acks already sent reach the server, within timeout.
func (s *SessionConsumers) Drain(timeout time.Duration) error { return s.client.Drain(timeout) }

// Connected reports whether the connection is live.
func (s *SessionConsumers) Connected() bool { return s.client.Connected() }

// SessionConsumerStart is what Ensure or RestartAtFloor did to a session's consumer.
type SessionConsumerStart struct {
	// Created is set when the call created the consumer.
	Created bool
	// StartSeq is the stream sequence a consumer the call created starts at.
	StartSeq uint64
	// Gap is set when StartSeq is below the stream's first sequence: messages the consumer would
	// have delivered from StartSeq have aged out of the stream or been discarded.
	Gap bool
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
func (s *SessionConsumers) Ensure(ctx context.Context, sessionID string, subjects []string, startSeq uint64) (SessionConsumerStart, error) {
	name, filters, err := sessionConsumerTarget(sessionID, subjects)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	for attempt := 0; ; attempt++ {
		info, err := s.info(ctx, name)
		if errors.Is(err, ErrSessionConsumerNotFound) {
			start, err := s.create(ctx, name, filters, startSeq)
			// A consumer that appeared since the info, created by another call, is updated instead.
			if apiErrorCode(err) == jetstream.JSErrCodeConsumerExists && attempt == 0 {
				continue
			}
			return start, err
		}
		if err != nil {
			return SessionConsumerStart{}, err
		}
		return SessionConsumerStart{}, s.update(ctx, name, info.Config, filters)
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
func (s *SessionConsumers) RestartAtFloor(ctx context.Context, sessionID string, subjects []string) (SessionConsumerStart, error) {
	name, filters, err := sessionConsumerTarget(sessionID, subjects)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	info, err := s.info(ctx, name)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	if err := s.delete(ctx, name); err != nil {
		return SessionConsumerStart{}, err
	}
	return s.create(ctx, name, filters, ackFloor(info)+1)
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
	name, ok := SessionConsumerName(sessionID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoSessionConsumerName, sessionID)
	}
	return s.info(ctx, name)
}

// Delete deletes sessionID's consumer; one the stream does not hold is no error.
func (s *SessionConsumers) Delete(ctx context.Context, sessionID string) error {
	name, ok := SessionConsumerName(sessionID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSessionConsumerName, sessionID)
	}
	return s.delete(ctx, name)
}

// StreamID returns the stream's identity, its StreamIDMetadataKey metadata, which a relay frame
// carries and a session's resume point names.
func (s *SessionConsumers) StreamID(ctx context.Context) (string, error) {
	state, err := s.streamState(ctx)
	if err != nil {
		return "", err
	}
	if state.id == "" {
		return "", fmt.Errorf("bus: %s carries no %s metadata", Stream, StreamIDMetadataKey)
	}
	return state.id, nil
}

// SessionAcker answers one pulled message (SessionMessage); the jetstream package's message is one.
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

// SessionPull is a pull of one session consumer's messages, which arrive in stream order, a
// redelivery included.
type SessionPull struct {
	messages jetstream.MessagesContext
}

// Pull starts pulling sessionID's consumer, or returns ErrSessionConsumerNotFound. Pulled messages
// wait in the pull for Next; Stop ends it, and the server delivers any message it delivered and
// was not acked for again once its ack wait has passed, or at a restart (RestartAtFloor).
func (s *SessionConsumers) Pull(ctx context.Context, sessionID string) (*SessionPull, error) {
	name, ok := SessionConsumerName(sessionID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoSessionConsumerName, sessionID)
	}
	if err := s.client.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	s.client.mu.Lock()
	conn := s.client.Conn
	s.client.mu.Unlock()
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, err
	}
	consumer, err := js.Consumer(ctx, Stream, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrSessionConsumerNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	messages, err := consumer.Messages()
	if err != nil {
		return nil, err
	}
	return &SessionPull{messages: messages}, nil
}

// Next returns the next message the pull holds, waiting for one until ctx ends.
func (p *SessionPull) Next(ctx context.Context) (SessionMessage, error) {
	msg, err := p.messages.Next(jetstream.NextContext(ctx))
	if err != nil {
		return SessionMessage{}, err
	}
	meta, err := msg.Metadata()
	if err != nil {
		return SessionMessage{}, err
	}
	return SessionMessage{
		Subject:      msg.Subject(),
		Header:       msg.Headers(),
		Data:         msg.Data(),
		StreamSeq:    meta.Sequence.Stream,
		Delivered:    meta.NumDelivered,
		StoredAt:     meta.Timestamp,
		SessionAcker: msg,
	}, nil
}

// Stop ends the pull; the messages it held and Next had not returned are delivered again later.
func (p *SessionPull) Stop() { p.messages.Stop() }

// sessionConsumerTarget names sessionID's consumer and reduces subjects to its filters, refusing a
// session that can have no consumer and a filter set that is empty, which would deliver the whole
// stream.
func sessionConsumerTarget(sessionID string, subjects []string) (string, []string, error) {
	name, ok := SessionConsumerName(sessionID)
	if !ok {
		return "", nil, fmt.Errorf("%w: %q", ErrNoSessionConsumerName, sessionID)
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
	next := current
	s.applySettings(&next, filters)
	if next.AckWait == current.AckWait &&
		next.MaxAckPending == current.MaxAckPending &&
		next.MaxDeliver == current.MaxDeliver &&
		next.InactiveThreshold == current.InactiveThreshold &&
		next.FilterSubject == current.FilterSubject &&
		slices.Equal(next.FilterSubjects, current.FilterSubjects) {
		return nil
	}
	_, err := s.upsert(ctx, name, next, "update")
	return err
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
	if _, err := s.upsert(ctx, name, config, "create"); err != nil {
		return SessionConsumerStart{}, err
	}
	return SessionConsumerStart{Created: true, StartSeq: startSeq, Gap: startSeq < state.firstSeq}, nil
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

// sessionStreamState is what SessionConsumers reads of the stream: its identity, the first sequence
// it still holds and the last it stored.
type sessionStreamState struct {
	id                string
	firstSeq, lastSeq uint64
}

// streamState reads the stream's identity and sequences with its info.
func (s *SessionConsumers) streamState(ctx context.Context) (sessionStreamState, error) {
	reply, err := s.client.requestConfirmed(ctx, "$JS.API.STREAM.INFO."+Stream, nil)
	if err != nil {
		return sessionStreamState{}, err
	}
	var response struct {
		Config struct {
			Metadata map[string]string `json:"metadata"`
		} `json:"config"`
		State struct {
			FirstSeq uint64 `json:"first_seq"`
			LastSeq  uint64 `json:"last_seq"`
		} `json:"state"`
		Error *jetstream.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &response); err != nil {
		return sessionStreamState{}, fmt.Errorf("bus: read the info of %s: %w", Stream, err)
	}
	if response.Error != nil {
		return sessionStreamState{}, fmt.Errorf("bus: read the info of %s: %w", Stream, response.Error)
	}
	return sessionStreamState{
		id:       response.Config.Metadata[StreamIDMetadataKey],
		firstSeq: response.State.FirstSeq,
		lastSeq:  response.State.LastSeq,
	}, nil
}

// apiErrorCode is the JetStream error code err carries, or 0.
func apiErrorCode(err error) jetstream.ErrorCode {
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode
	}
	return 0
}
