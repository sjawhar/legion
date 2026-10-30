package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// The modes of a registration's delivery answer (deliveryAnswer.Mode).
const (
	// deliveryModeConsumer: the listener holds the session's consumer, filtered on the session's
	// agent subject and its stored followed topics, and relays it on the session's relay subject.
	deliveryModeConsumer = "consumer"
	// deliveryModeCore: the session stays on the core path, its core subscriptions and the
	// durable's Skipped acknowledgement, as before consumer delivery.
	deliveryModeCore = "core"
)

// The reasons of a core delivery answer (deliveryAnswer.Reason), short and machine-readable.
const (
	// deliveryReasonNoGrant: the listener's NATS user may not manage session consumers (a hub whose
	// grant predates them, bus.ErrConsumerNotGranted).
	deliveryReasonNoGrant = "no_grant"
	// deliveryReasonInvalidSessionID: the session id names no consumer (bus.SessionConsumerName).
	deliveryReasonInvalidSessionID = "invalid_session_id"
	// deliveryReasonConsumerError: the consumer could not be read, created or updated, or this
	// listener has no session-consumer connection.
	deliveryReasonConsumerError = "consumer_error"
	// deliveryReasonNotRequested: the registration carried delivery without asking for a consumer.
	deliveryReasonNotRequested = "not_requested"
)

// sessionConsumerCallTimeout bounds the consumer calls a registration makes before it answers, and
// those of one relay restart, timed from when it holds its turn and the session's lock
// (sessionRelays.restart).
const sessionConsumerCallTimeout = 5 * time.Second

// noGrantBackoff is how long, after a consumer call the server refused for want of a grant, every
// registration that asks for a consumer is answered core with reason no_grant without a call to the
// server; the next registration after it probes again. A hub whose grant predates session consumers
// would otherwise log a permissions violation, on the server and here, for every heartbeat of
// every session.
const noGrantBackoff = 10 * time.Minute

// sessionConsumerDrain bounds the drain of the session-consumer connection at shutdown, inside
// what is left of the HTTP stop's budget.
const sessionConsumerDrain = 2 * time.Second

// deliveryAnswer is the delivery field of a registration's answer (subscribeResponse.Delivery),
// present whenever the registration carried delivery or registered a serving port. A listener of an
// earlier release answers without it, which the plugin reads as core.
type deliveryAnswer struct {
	Mode string `json:"mode"`
	// Subject is the session's relay subject (bus.DeliverSubject), in consumer mode.
	Subject string `json:"subject,omitempty"`
	// StreamID is the stream's identity (bus.StreamIDMetadataKey), in consumer mode: the one a
	// relay frame carries and the plugin's resume point names.
	StreamID string `json:"stream_id,omitempty"`
	// Gap, in consumer mode, is true when this registration created the consumer at a start below
	// the stream's first sequence: messages it would have delivered are gone.
	Gap *bool `json:"gap,omitempty"`
	// Reason says why, in core mode.
	Reason string `json:"reason,omitempty"`
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
	// epoch: the plugin connected anew, or came over from the core path, so the relay restarts
	// the session's consumer at its ack floor before it relays again.
	registered(sessionID string, epochChanged bool)
}

// sessionDelivery manages the session consumers (bus.SessionConsumers) for the listener's
// registrations: it creates each one a registration asks for, keeps its filter on the session's
// agent subject and stored followed topics as they change, and tells the relay of each
// registration. A nil sessionDelivery, or one without consumers (the session-consumer connection
// failed at start), answers every registration that asks with core.
type sessionDelivery struct {
	consumers *bus.SessionConsumers
	logger    *logging.Logger
	// relay hears each registration that records consumer delivery. A sessionDelivery with
	// consumers has one (startSessionRelays sets it before /v1 opens, and a relay that could not
	// start replaces the whole sessionDelivery with one without consumers, which answers every
	// registration core), so a registration answered consumer is relayed.
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
// urls. A connection that fails is logged once and leaves the listener serving every registration
// on the core path, as a listener of an earlier release does.
func connectSessionDelivery(urls []string, logger *logging.Logger) *sessionDelivery {
	consumers, err := bus.ConnectSessionConsumers(urls)
	if err != nil {
		logger.Error("session consumer connection failed; every session stays on core delivery", slog.String("error", err.Error()))
		return &sessionDelivery{logger: logger, now: time.Now}
	}
	return &sessionDelivery{consumers: consumers, logger: logger, now: time.Now}
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

// register gives a registration the delivery it asks for, once its topics are stored: topics are
// the session's stored interest topics. A registration that carried delivery with consumer set, or
// registered a serving port, gets the session's consumer, created when the stream holds none (at
// the sequence after its resume point when that names the stream's current identity, else at the
// stream's end) and otherwise updated to the current filter, before the registration answers.
func (m *sessionDelivery) register(ctx context.Context, body subscribeBody, topics []string) sessionDeliveryOutcome {
	if body.Delivery == nil && body.Port <= 0 {
		return sessionDeliveryOutcome{}
	}
	if body.Port <= 0 && !body.Delivery.Consumer {
		return coreDeliveryOutcome(deliveryReasonNotRequested)
	}
	if _, ok := bus.SessionConsumerName(body.SessionID); !ok {
		return coreDeliveryOutcome(deliveryReasonInvalidSessionID)
	}
	if m == nil || m.consumers == nil {
		return coreDeliveryOutcome(deliveryReasonConsumerError)
	}
	if m.grantRefused() {
		return coreDeliveryOutcome(deliveryReasonNoGrant)
	}
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	streamID, err := m.consumers.StreamID(ctx)
	if err != nil {
		return m.refused(body.SessionID, err)
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
		return m.refused(body.SessionID, err)
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
	}
}

// coreDeliveryOutcome is the outcome of a registration left on the core path for reason.
func coreDeliveryOutcome(reason string) sessionDeliveryOutcome {
	return sessionDeliveryOutcome{answer: &deliveryAnswer{Mode: deliveryModeCore, Reason: reason}}
}

// refused is the core outcome of a consumer call for sessionID that failed with err. A refusal for
// want of a grant starts the no-grant backoff (noGrantBackoff), or extends the episode running.
func (m *sessionDelivery) refused(sessionID string, err error) sessionDeliveryOutcome {
	if consumerGrantRefused(err) {
		m.grantMu.Lock()
		m.noGrantUntil = m.now().Add(noGrantBackoff)
		first := !m.noGrantLogged
		m.noGrantLogged = true
		m.grantMu.Unlock()
		if first {
			m.logger.Warn("the NATS user may not manage session consumers; sessions stay on core delivery until the grant carries them",
				slog.String("session_id", sessionID), slog.String("error", err.Error()), slog.String("probe_again_in", noGrantBackoff.String()))
		}
		return coreDeliveryOutcome(deliveryReasonNoGrant)
	}
	m.logger.Warn("session consumer call failed; the session stays on core delivery",
		slog.String("session_id", sessionID), slog.String("error", err.Error()))
	return coreDeliveryOutcome(deliveryReasonConsumerError)
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
// that removed some (an unsubscribe), when the session's entry records consumer delivery; a session
// on the core path, or whose consumer is gone, is left alone, and its next registration sets the
// filter. A failure is logged, not returned: the consumer then still delivers a topic the session
// no longer follows, and the plugin drops a frame no followed topic matches.
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
// every one of topics the stream stores (bus.StreamCaptures). A topic the stream does not store (a
// team or other ad-hoc topic, a role lane) reaches the session over its core subscription only.
// SessionConsumers reduces the list to filters that do not overlap.
func sessionConsumerFilter(sessionID string, topics []string) []string {
	filter := make([]string, 0, len(topics)+1)
	filter = append(filter, contracts.AgentSubject(sessionID))
	for _, topic := range topics {
		if bus.StreamCaptures(topic) {
			filter = append(filter, topic)
		}
	}
	return filter
}
