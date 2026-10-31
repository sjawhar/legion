package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// deliveryModeConsumer is the one mode a registration is answered (deliveryAnswer.Mode): the
// listener holds the session's consumer, filtered on the session's agent subject and its stored
// followed topics, and relays it on the session's relay subject. A registration the listener
// cannot give a consumer is refused (deliveryRefusal), since no other path reaches a session.
const deliveryModeConsumer = "consumer"

// The reasons a registration is refused for want of a session consumer (deliveryRefusal.Reason),
// short and machine-readable.
const (
	// deliveryReasonNoGrant: the listener's NATS user may not manage session consumers (a hub whose
	// grant predates them, bus.ErrConsumerNotGranted).
	deliveryReasonNoGrant = "no_grant"
	// deliveryReasonInvalidSessionID: the session id names no consumer (bus.SessionConsumerName).
	deliveryReasonInvalidSessionID = "invalid_session_id"
	// deliveryReasonConsumerError: the consumer could not be read, created or updated, or this
	// listener has no session-consumer connection.
	deliveryReasonConsumerError = "consumer_error"
	// deliveryReasonNotRequested: the registration asked for no consumer and registered no serving
	// port, so nothing would deliver to the session it describes.
	deliveryReasonNotRequested = "not_requested"
)

// sessionConsumerCallTimeout bounds the consumer calls a registration makes before it answers, and
// those of one relay restart, timed from when it holds its turn and the session's lock
// (sessionRelays.restart).
const sessionConsumerCallTimeout = 5 * time.Second

// noGrantBackoff is how long, after a consumer call the server refused for want of a grant, every
// registration that asks for a consumer is refused no_grant without a call to the server; the next
// registration after it probes again. The backoff survives the refusal it once spared a session:
// without it a hub whose grant predates session consumers would have every heartbeat of every
// session call the server, and log a permissions violation there and here, for an answer this
// listener already knows.
const noGrantBackoff = 10 * time.Minute

// sessionConsumerDrain bounds the drain of the session-consumer connection at shutdown, inside
// what is left of the HTTP stop's budget.
const sessionConsumerDrain = 2 * time.Second

// deliveryAnswer is the delivery field of a registration's answer (subscribeResponse.Delivery).
// Every registration this listener answers at all carries one: the session's consumer is held and
// relayed, which is the only way a message reaches a session.
type deliveryAnswer struct {
	Mode string `json:"mode"`
	// Subject is the session's relay subject (bus.DeliverSubject).
	Subject string `json:"subject,omitempty"`
	// StreamID is the stream's identity (bus.StreamIDMetadataKey): the one a relay frame carries
	// and the plugin's resume point names.
	StreamID string `json:"stream_id,omitempty"`
	// Gap is true when this registration created the consumer at a start below the stream's first
	// sequence: messages it would have delivered are gone.
	Gap *bool `json:"gap,omitempty"`
}

// deliveryRefusal is the answer to a registration the listener cannot give a session consumer.
// The registration is refused before anything is written, so it leaves no interest row and no
// session entry behind (subscribeHandler): a session nothing delivers to is not registered.
type deliveryRefusal struct {
	// status is 503 for a cause that may pass of itself - this listener holds no session-consumer
	// connection, or a consumer call failed - and 409 for one that will not: the registration
	// asked for no consumer, or its session id names none. The plugin's own half of this is
	// dispatch://INFRA-771: it starts with Envoy inbound unavailable, says so, and asks again at
	// each heartbeat.
	status int
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// sessionDeliveryOutcome is what sessionDelivery.register did for one registration.
type sessionDeliveryOutcome struct {
	// answer is the registration answer's delivery field, nil when the answer carries none.
	answer *deliveryAnswer
	// epoch is the plugin's epoch, recorded on the entry when the session is relayed.
	epoch string
	// consumerStart is the stream sequence the session's consumer starts at, recorded on the entry
	// beside the epoch: the durable path still delivers a stored message below it, which is in no
	// consumer (session.SessionEntry.ConsumerStart).
	consumerStart uint64
}

// relayed reports whether the session's entry records consumer delivery (session.DeliveryConsumer):
// its consumer is held and the relay serves it, so the durable path skips the stored messages that
// consumer holds. It is the answer's own mode, so the entry the registration writes and the answer
// the session reads can never say different things.
func (o sessionDeliveryOutcome) relayed() bool {
	return o.answer != nil && o.answer.Mode == deliveryModeConsumer
}

// sessionRelay is what the relay of session consumers hears from registrations.
type sessionRelay interface {
	// registered reports a registration of sessionID that recorded consumer delivery on its entry.
	// epochChanged is set when the entry it replaced did not hold consumer delivery with the same
	// epoch: the plugin connected anew, so the relay restarts the session's consumer at its ack
	// floor before it relays again.
	registered(sessionID string, epochChanged bool)
}

// sessionDelivery manages the session consumers (bus.SessionConsumers) for the listener's
// registrations: it creates each one a registration asks for, keeps its filter on the session's
// agent subject and stored followed topics as they change, and tells the relay of each
// registration. A nil sessionDelivery, or one without consumers, refuses every registration with
// consumer_error; main's listener has neither, since its start ends when the session-consumer
// connection fails, and only a test builds a listener without one.
type sessionDelivery struct {
	consumers *bus.SessionConsumers
	logger    *logging.Logger
	// relay hears each registration that records consumer delivery. A sessionDelivery with
	// consumers has one (startSessionRelays sets it before /v1 opens, and main's start ends when
	// that relay cannot start), so a registration answered consumer is relayed.
	relay sessionRelay
	// now is the clock of the no-grant backoff; time.Now unless a test sets it.
	now func() time.Time
	// locks keeps the consumer calls made for one session one at a time: a registration's create or
	// filter update, an unsubscribe's filter update and the relay's restart at the ack floor, which
	// deletes the consumer and creates it again when a message awaits an ack. A create landing
	// between that delete and create would start the consumer at the stream's end and skip what the
	// session has not acked. Under the lock a restart that read the floor and failed after its
	// delete or create leaves that floor pending, and the next call that finds the consumer missing,
	// a registration's create or the relay's retry, creates it there
	// (bus.SessionConsumers.RestartAtFloor), so in this process no restart loses the session's
	// position. The lock and the pending start are this listener's alone: a second
	// listener (a rolling deploy's other task, or the one a session re-registers through) can still
	// create the consumer in that window at the session's resume point or the stream's end, or run
	// its own restart's calls between this one's.
	locks sessionLocks
	// grantMu guards the refusal episode below.
	grantMu sync.Mutex
	// noGrantUntil is when the registrations' calls may probe the grant again after a refusal for
	// want of it; zero while no refusal episode runs. noGrantLogged is set once the episode's one
	// warning is logged, so a hub without the grant costs one line per episode rather than one per
	// registration. A consumer call that succeeds ends the episode.
	noGrantUntil  time.Time
	noGrantLogged bool
}

// connectSessionDelivery opens the session-consumer connection (bus.ConnectSessionConsumers) on
// urls. Every session takes its messages from the consumer this connection manages, so a failure
// is the whole start's: main ends the process with it, as it does for the live topics lane.
func connectSessionDelivery(urls []string, logger *logging.Logger) (*sessionDelivery, error) {
	consumers, err := bus.ConnectSessionConsumers(urls)
	if err != nil {
		return nil, err
	}
	return &sessionDelivery{consumers: consumers, logger: logger, now: time.Now}, nil
}

// close closes the session-consumer connection, if one is open.
func (m *sessionDelivery) close() {
	if m != nil && m.consumers != nil {
		m.consumers.Close()
	}
}

// drain drains the session-consumer connection, so the acks the relays sent reach the server,
// within sessionConsumerDrain or what is left of ctx if that is less; with nothing left of ctx it
// closes the connection. The relays are stopped first (sessionRelays.stop).
func (m *sessionDelivery) drain(ctx context.Context) {
	if m == nil || m.consumers == nil {
		return
	}
	timeout := sessionConsumerDrain
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if timeout <= 0 {
		m.consumers.Close()
		return
	}
	if err := m.consumers.Drain(timeout); err != nil {
		m.logger.Warn("session consumer connection drain error", slog.String("error", err.Error()))
	}
}

// register gives a registration the session consumer every portless session is delivered through,
// filtered on topics, the topics the registration's interest row will hold. One this listener
// cannot give a consumer is refused by name, and the handler then writes nothing.
//
// A session on a serving port is the exception: the listener that owns it pushes to its port, so
// a consumer it cannot be given leaves it registered with no delivery field, as a listener before
// session consumers answered it, and its messages come down the durable path
// (dispatch://INFRA-429 removes that path and this exception with it).
func (m *sessionDelivery) register(ctx context.Context, body subscribeBody, topics []string) (sessionDeliveryOutcome, *deliveryRefusal) {
	outcome, refusal := m.consumerFor(ctx, body, topics)
	if refusal != nil && body.Port > 0 {
		return sessionDeliveryOutcome{}, nil
	}
	return outcome, refusal
}

// consumerFor creates the session's consumer when the stream holds none (at the sequence after its
// resume point when that names the stream's current identity, else at the stream's end) and
// otherwise updates it to the current filter, before the registration answers; it refuses a
// registration that asks for none, or that this listener can give none.
func (m *sessionDelivery) consumerFor(ctx context.Context, body subscribeBody, topics []string) (sessionDeliveryOutcome, *deliveryRefusal) {
	if body.Port <= 0 && (body.Delivery == nil || !body.Delivery.Consumer) {
		// A registration with no delivery field at all is a plugin older than consumer delivery.
		// It receives nothing either way, so it is told so rather than left registered and silent.
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusConflict,
			Error: fmt.Sprintf("%s asked for no session consumer: a session receives only what its own consumer holds, so register with delivery.consumer true",
				body.SessionID),
			Reason: deliveryReasonNotRequested,
		}
	}
	if _, ok := bus.SessionConsumerName(body.SessionID); !ok {
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusConflict,
			Error: fmt.Sprintf("%q names no session consumer: a session id holds no dot, star, angle bracket, slash or whitespace",
				body.SessionID),
			Reason: deliveryReasonInvalidSessionID,
		}
	}
	if m == nil || m.consumers == nil {
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusServiceUnavailable,
			Error:  "this listener holds no session-consumer connection, so it can give " + body.SessionID + " no consumer",
			Reason: deliveryReasonConsumerError,
		}
	}
	if m.grantRefused() {
		return sessionDeliveryOutcome{}, noGrantRefusal(body.SessionID)
	}
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	streamID, err := m.consumers.StreamID(ctx)
	if err != nil {
		return sessionDeliveryOutcome{}, m.refused(body.SessionID, err)
	}
	var startSeq uint64
	var epoch string
	if body.Delivery != nil {
		epoch = body.Delivery.Epoch
		if resume := body.Delivery.Resume; resume != nil && resume.Seq > 0 && resume.StreamID == streamID {
			startSeq = resume.Seq + 1
		}
	}
	unlock := m.locks.lock(body.SessionID)
	start, err := m.consumers.Ensure(ctx, body.SessionID, sessionConsumerFilter(body.SessionID, topics), startSeq)
	unlock()
	if err != nil {
		return sessionDeliveryOutcome{}, m.refused(body.SessionID, err)
	}
	m.granted()
	gap := start.Gap
	return sessionDeliveryOutcome{
		answer: &deliveryAnswer{
			Mode:     deliveryModeConsumer,
			Subject:  bus.DeliverSubject(body.SessionID),
			StreamID: streamID,
			Gap:      &gap,
		},
		epoch:         epoch,
		consumerStart: start.StartSeq,
	}, nil
}

// noGrantRefusal is the refusal of sessionID's registration for want of the grant that manages
// session consumers, from a call the server refused or from the backoff that call started.
func noGrantRefusal(sessionID string) *deliveryRefusal {
	return &deliveryRefusal{
		status: http.StatusServiceUnavailable,
		Error:  "this listener may not manage session consumers, so it can give " + sessionID + " none",
		Reason: deliveryReasonNoGrant,
	}
}

// refused is the refusal of a registration whose consumer call for sessionID failed with err:
// both causes may pass of themselves, so both are 503. A refusal for want of a grant starts the
// no-grant backoff (noGrantBackoff), or extends the episode running.
func (m *sessionDelivery) refused(sessionID string, err error) *deliveryRefusal {
	if consumerGrantRefused(err) {
		m.grantMu.Lock()
		m.noGrantUntil = m.now().Add(noGrantBackoff)
		first := !m.noGrantLogged
		m.noGrantLogged = true
		m.grantMu.Unlock()
		if first {
			m.logger.Warn("the NATS user may not manage session consumers; every registration is refused until the grant carries them",
				slog.String("session_id", sessionID), slog.String("error", err.Error()), slog.String("probe_again_in", noGrantBackoff.String()))
		}
		return noGrantRefusal(sessionID)
	}
	m.logger.Warn("session consumer call failed; the registration is refused",
		slog.String("session_id", sessionID), slog.String("error", err.Error()))
	return &deliveryRefusal{
		status: http.StatusServiceUnavailable,
		Error:  "the session consumer of " + sessionID + " could not be read or created",
		Reason: deliveryReasonConsumerError,
	}
}

// consumerGrantRefused reports whether err is the server's refusal of a consumer call for want of a
// grant. bus.ErrConsumerNotGranted always wraps bus.ErrPublishDenied, so the one test covers both.
func consumerGrantRefused(err error) bool {
	return errors.Is(err, bus.ErrPublishDenied)
}

// grantRefused reports whether a refusal for want of a grant was met less than noGrantBackoff
// ago, so a registration is answered no_grant without asking the server.
func (m *sessionDelivery) grantRefused() bool {
	m.grantMu.Lock()
	defer m.grantMu.Unlock()
	return !m.noGrantUntil.IsZero() && m.now().Before(m.noGrantUntil)
}

// granted ends a refusal episode once a consumer call succeeds: the grant now carries session
// consumers, and a later refusal is logged again.
func (m *sessionDelivery) granted() {
	m.grantMu.Lock()
	defer m.grantMu.Unlock()
	m.noGrantUntil = time.Time{}
	m.noGrantLogged = false
}

// refollow sets the filter of sessionID's consumer to its stored interest topics after a change
// that removed some (an unsubscribe), when the session's entry records consumer delivery; a
// session whose entry records none, an entry the previous build wrote, or whose consumer is gone,
// is left alone, and its next registration sets the filter. A failure is logged, not returned:
// the consumer then still delivers a topic the session no longer follows, and the plugin drops a
// frame no followed topic matches.
func (m *sessionDelivery) refollow(ctx context.Context, d *listenerDeps, sessionID string) {
	if m == nil || m.consumers == nil {
		return
	}
	entry, err := d.sessions.Get(sessionID)
	if err != nil || entry.Delivery != session.DeliveryConsumer {
		return
	}
	topics, err := storedInterestTopics(ctx, d.registry, sessionID)
	if err != nil {
		m.logger.Warn("session consumer filter left unchanged: the session's interests could not be read",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	unlock := m.locks.lock(sessionID)
	err = m.consumers.UpdateFilter(ctx, sessionID, sessionConsumerFilter(sessionID, topics))
	unlock()
	if err != nil && !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		m.logger.Warn("session consumer filter update failed", slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
}

// storedInterestTopics is the topics of sessionID's stored interest row, read within ctx: none when
// it has no row, or a row this build cannot decode, whose topics nothing routes (store.Registry.Get).
func storedInterestTopics(ctx context.Context, registry *store.Registry, sessionID string) ([]string, error) {
	interest, err := registry.Get(ctx, sessionID)
	if errors.Is(err, nats.ErrKeyNotFound) || errors.Is(err, store.ErrMalformedInterest) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return interest.Topics, nil
}

// sessionLocks holds one lock per session id, kept only while somebody holds or waits for it.
type sessionLocks struct {
	mu    sync.Mutex
	locks map[string]*sessionLock
}

type sessionLock struct {
	sync.Mutex
	// users counts the callers holding or waiting for the lock, under sessionLocks.mu.
	users int
}

// lock takes sessionID's lock and returns the func that releases it.
func (l *sessionLocks) lock(sessionID string) (unlock func()) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*sessionLock{}
	}
	held := l.locks[sessionID]
	if held == nil {
		held = &sessionLock{}
		l.locks[sessionID] = held
	}
	held.users++
	l.mu.Unlock()
	held.Lock()
	return func() {
		held.Unlock()
		l.mu.Lock()
		held.users--
		if held.users == 0 {
			delete(l.locks, sessionID)
		}
		l.mu.Unlock()
	}
}

// sessionConsumerFilter is the subjects sessionID's consumer filters on: its agent subject and
// every one of topics the stream stores (bus.StreamCaptures), a team topic among them. A topic
// the stream does not store reaches the session on no path of its own: a role lane reaches its
// holder as a forward on the holder's agent subject, and a topic an earlier build's row still
// names, which /v1/interests/subscribe refuses now, reaches nothing. SessionConsumers reduces the
// list to filters that do not overlap.
//
// A topic the server would refuse as a filter subject (bus.CheckFilterSubject: an empty token, as
// in a GitHub topic that spells a repository name with a dot, or a `>` before the last token) is
// left out, though the row stores it: with it in the list the server refuses the whole call and
// the session gets no consumer at all, and no message can reach such a subject anyway.
func sessionConsumerFilter(sessionID string, topics []string) []string {
	filter := make([]string, 0, len(topics)+1)
	filter = append(filter, contracts.AgentSubject(sessionID))
	for _, topic := range topics {
		if bus.StreamCaptures(topic) && bus.CheckFilterSubject(topic) == nil {
			filter = append(filter, topic)
		}
	}
	return filter
}
