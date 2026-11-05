package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// consumerPluginPackage and consumerPluginMinimumVersion name the release of Oh My Pi's plugin
// that first asked for the session's durable consumer, @sjawhar/pi-envoy 14.1.0
// (packages/pi-envoy/CHANGELOG.md, 14.1.0, "Consumer delivery"); every release before it receives
// from a plain NATS subscription this listener no longer feeds. noConsumerRemedy says what the
// refusal below tells a client at or above that release and what it tells every other.
const (
	consumerPluginPackage        = "@sjawhar/pi-envoy"
	consumerPluginMinimumVersion = "14.1.0"
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
	// heard is the registration's mark (sessionDelivery.mark), taken once its topics were stored
	// and its consumer created or updated, when the session is relayed.
	heard uint64
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
	// floor before it relays again, unless the relay's run restarted the consumer after heard,
	// the registration's mark (sessionDelivery.mark).
	registered(sessionID string, epochChanged bool, heard uint64)
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
	// marks is the last mark taken (mark).
	marks atomic.Uint64
	// now is the clock of the no-grant backoff; time.Now unless a test sets it.
	now func() time.Time
	// beforeRegister runs at the top of each register call, before the session's lock is taken and
	// the consumer's filter set; nil unless a test sets it. The window between a registration's
	// plan and its filter is where another writer's row lands and makes the plan's
	// compare-and-swap fail, and only a test that can write in that window can drive the re-plan,
	// and the filter the pass after it sets, of registerSession's loop. It runs for every pass of
	// one registration, so a test that counts its calls drives each pass of that registration on
	// its own.
	beforeRegister func()
	// afterRegisterFilter runs at the end of each register call that was given a consumer, under
	// the session's lock that call returns held and before registerSession writes the interest row
	// the filter was set from; nil unless a test sets it. The window between a registration's
	// filter and its row is the one that lock closes, and only a test that can run in that window
	// can prove a relay restart never narrows the filter back to a row the registration has not
	// written yet. It runs for every pass that holds a consumer, as beforeRegister runs for every
	// pass. A call made in it that takes the same session's lock waits for the registration's pass
	// to release it, so a test drives such a call from a goroutine of its own.
	afterRegisterFilter func()
	// locks keeps the consumer calls made for one session one at a time: a registration's create or
	// filter update, an unsubscribe's filter update and the relay's restart at the ack floor, which
	// deletes the consumer and creates it again when a message awaits an ack. An unsubscribe and a
	// restart read the session's stored topics under the lock; a registration filters on the plan
	// it is about to store and holds the lock from that filter through the plan's write
	// (registerSession). So no call reads the row between a registration's filter and that
	// registration's write: a restart that did would set the filter back to a row without the topic
	// the registration is answered 200 for, and every message published on that topic until the
	// session's next heartbeat would be skipped. The lock does not order a registration's plan,
	// which is read before the lock is taken (registerSession, api.go): a pass whose plan predates
	// another registration's write stores a row that is not the plan it filtered on, and that pass
	// plans and filters again, so the filter of the pass that answers is the row it wrote. A create
	// landing between that delete and create would start the consumer at that registration's own
	// position - its resume point, or else what the stream holds for the session - rather than at
	// the floor the restart read. Under the lock a restart that read the floor and failed after its
	// delete or create leaves that floor pending, and the next call that finds the consumer missing,
	// a registration's create or the relay's retry, creates it there
	// (bus.SessionConsumers.RestartAtFloor), so in this process no restart loses the session's
	// position. The lock and the pending start are this listener's alone: a second
	// listener (a rolling deploy's other task, or the one a session re-registers through) can still
	// create the consumer in that window at the session's resume point, or at what the stream holds
	// for the session, or run its own restart's calls between this one's.
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

// mark returns the next of the marks that order registrations against the relay's restarts: a
// registration takes one once its topics are stored and its consumer created or updated, and a
// relay restart takes one, under the session's lock, before it reads the session's topics and
// consumer. A restart whose mark is above a registration's read everything that registration
// wrote, after the plugin subscribed the connection the registration names, so every frame it
// sent reaches that connection.
func (m *sessionDelivery) mark() uint64 { return m.marks.Add(1) }

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

// register gives a registration the session consumer every session is delivered through,
// filtered on topics, the topics the registration's interest row will hold. One this listener
// cannot give a consumer is refused by name, and the handler then writes nothing.
//
// It returns the session's lock still held, as the func that releases it, and its caller
// releases that func once it has written the row this filter was set from (registerSession): a
// relay restart, which rebuilds the filter from the stored row, then runs wholly before or
// wholly after a registration's pass and never between its filter and its row. The lock
// (sessionLocks) is no re-entrant one, so the pass takes it once and the caller releases it once,
// on every path out of the pass. A registration refused before the lock was taken returns
// noUnlock, so the caller releases what it was given either way.
func (m *sessionDelivery) register(ctx context.Context, body subscribeBody, topics []string) (sessionDeliveryOutcome, *deliveryRefusal, func()) {
	if m != nil && m.beforeRegister != nil {
		m.beforeRegister()
	}
	outcome, refusal, unlock := m.consumerFor(ctx, body, topics)
	if refusal == nil && m != nil && m.afterRegisterFilter != nil {
		m.afterRegisterFilter()
	}
	return outcome, refusal, unlock
}

// noUnlock releases nothing: it is the unlock a consumer call that refused a registration before
// it took the session's lock hands back.
func noUnlock() {}

// clientNamed names the client a registration came from, for the refusal above: its plugin
// package and build when it sent them (subscribeBody.Plugin, PluginVersion), and otherwise that it
// named none, which every build before those fields is. An operator reading the refusal needs the
// build to replace, and a session whose client names nothing at all is itself the finding.
func clientNamed(body subscribeBody) string {
	switch {
	case body.Plugin != "" && body.PluginVersion != "":
		return body.Plugin + " " + body.PluginVersion
	case body.Plugin != "":
		return body.Plugin + ", which names no version"
	case body.PluginVersion != "":
		return "a client naming version " + body.PluginVersion + " and no package"
	default:
		return "a client naming no plugin"
	}
}

// noConsumerRemedy says what the client that asked for no consumer must do, for the refusal below.
// A pi-envoy at or above consumerPluginMinimumVersion asks for a consumer whenever its relay
// subject is open, and sends no delivery field while it holds none
// (packages/pi-envoy/extensions/envoy.ts, the `asking === undefined` spread of its registration
// body), so such a session needs no upgrade: what it lacks is that subject, and its next heartbeat
// registers again once it has one. Every other client - an older pi-envoy, another plugin, or one
// naming no version or no package at all - is told the build to replace, since an operator reading
// this refusal in the session's own transcript has nothing else to go on.
func noConsumerRemedy(body subscribeBody) string {
	named := clientNamed(body)
	if body.Plugin == consumerPluginPackage && versionAtLeast(body.PluginVersion, consumerPluginMinimumVersion) {
		return named + " asks for a consumer only while its relay subject is open, so this session holds none; it registers again at its next heartbeat, with a consumer once that subject is open"
	}
	return "upgrade to " + consumerPluginPackage + " " + consumerPluginMinimumVersion +
		" or later, or to a build of this client that sends the field (this registration came from " + named + ")"
}

// versionAtLeast reports whether version is floor or newer, both as major.minor.patch. A version
// that does not parse that way - a bundle commit, a build name, an empty field - is not: the
// refusal then names the upgrade, which is the answer that holds for a client this listener
// cannot date.
func versionAtLeast(version, floor string) bool {
	for i := range 3 {
		got, gotOK := versionPart(version, i)
		want, wantOK := versionPart(floor, i)
		if !gotOK || !wantOK {
			return false
		}
		if got != want {
			return got > want
		}
	}
	return true
}

// versionPart is the i-th dot-separated number of version, and whether it is one. A suffix on that
// number - a prerelease, a build - is left out of the comparison.
func versionPart(version string, i int) (int, bool) {
	parts := strings.Split(version, ".")
	if i >= len(parts) {
		return 0, false
	}
	digits := parts[i]
	if end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
		digits = digits[:end]
	}
	number, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return number, true
}

// consumerFor creates the session's consumer when the stream holds none and otherwise updates it
// to the current filter, before the registration answers; it refuses a registration that asks for
// none, or that this listener can give none. The consumer is created at the sequence after its
// resume point's seq when that point carries a seq, 0 included (which gives sequence 1), and names
// the stream's current identity. A registration with no resume point, or one with no seq or naming
// another identity, creates it at the first message the stream holds on the session's own agent
// subject, and at the stream's end only when it holds none (bus.SessionConsumers.Ensure). The
// filter is the topics the registration is about to store, so a message published on a newly
// followed topic once the registration is answered is the consumer's to deliver.
//
// It returns the session's lock held over that filter, which the caller releases (register); a
// refusal decided before the lock was taken returns noUnlock.
func (m *sessionDelivery) consumerFor(ctx context.Context, body subscribeBody, topics []string) (sessionDeliveryOutcome, *deliveryRefusal, func()) {
	if body.Delivery == nil || !body.Delivery.Consumer {
		// A registration with no delivery field asks for nothing this listener delivers, so it is
		// told so rather than left registered and silent, with what to do about it
		// (noConsumerRemedy, which says which client each answer is for), since the session's own
		// transcript is where whoever runs it reads why its Envoy went quiet.
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusConflict,
			Error: fmt.Sprintf("%s asked for no session consumer: a session receives only what its own consumer holds, so register with delivery.consumer true; %s",
				body.SessionID, noConsumerRemedy(body)),
			Reason: deliveryReasonNotRequested,
		}, noUnlock
	}
	if _, ok := bus.SessionConsumerName(body.SessionID); !ok {
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusConflict,
			Error: fmt.Sprintf("%q names no session consumer: a session id holds no dot, star, angle bracket, slash or whitespace",
				body.SessionID),
			Reason: deliveryReasonInvalidSessionID,
		}, noUnlock
	}
	if m == nil || m.consumers == nil {
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusServiceUnavailable,
			Error:  "this listener holds no session-consumer connection, so it can give " + body.SessionID + " no consumer",
			Reason: deliveryReasonConsumerError,
		}, noUnlock
	}
	if m.grantRefused() {
		return sessionDeliveryOutcome{}, noGrantRefusal(body.SessionID), noUnlock
	}
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	streamID, err := m.consumers.StreamID(ctx)
	if err != nil {
		return sessionDeliveryOutcome{}, m.refused(body.SessionID, err), noUnlock
	}
	var startSeq uint64
	epoch := body.Delivery.Epoch
	if resume := body.Delivery.Resume; resume != nil && resume.Seq != nil && resume.StreamID == streamID {
		startSeq = *resume.Seq + 1
	}
	unlock := m.locks.lock(body.SessionID)
	start, err := m.consumers.Ensure(ctx, body.SessionID, sessionConsumerFilter(body.SessionID, topics), startSeq)
	if err != nil {
		return sessionDeliveryOutcome{}, m.refused(body.SessionID, err), unlock
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
		heard:         m.mark(),
	}, nil, unlock
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
// is left alone, and its next registration sets the filter. The topics are read under the
// session's lock, as register's consumer call is made under it, so a registration that stored
// more topics meanwhile and wrote its filter first is not overwritten with the set read before
// it. A failure is logged, not returned: the consumer then still delivers a topic the session no
// longer follows, and the plugin drops a frame no followed topic matches.
func (m *sessionDelivery) refollow(ctx context.Context, d *listenerDeps, sessionID string) {
	if m == nil || m.consumers == nil {
		return
	}
	entry, err := d.sessions.Get(sessionID)
	if err != nil || entry.Delivery != session.DeliveryConsumer {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	unlock := m.locks.lock(sessionID)
	topics, err := storedInterestTopics(ctx, d.registry, sessionID)
	if err != nil {
		unlock()
		m.logger.Warn("session consumer filter left unchanged: the session's interests could not be read",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}
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
// every one of topics the stream stores (bus.StreamCaptures), a team topic among them. A Legion
// controller's notice lane (notifications.legion.<project>.controller) is one of those topics
// while the session holds that project's controller role, and this filter is the whole of how it
// now reaches the controller: the lane's live core subscription, which handed the session only
// what was published while it was subscribed, went with the rest of the core receive path
// (INFRA-771), so a notice published while the controller was away is delivered from the stream
// when it returns. A topic the stream does not store reaches the session on no path of its own: a
// role lane reaches its holder as a forward on the holder's agent subject, and a topic an earlier
// build's row still names, which /v1/interests/subscribe drops from a registration with a
// warning now, reaches nothing.
// SessionConsumers reduces the list to filters that do not overlap.
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
