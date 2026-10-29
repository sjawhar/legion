package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// The headers of a relay frame, the core message the listener publishes on a session's relay
// subject (bus.DeliverSubject) for each message of its consumer. The frame's data is the stored
// message's bytes, unchanged.
const (
	// relayHeaderSubject is the subject the stream stored the message on.
	relayHeaderSubject = "Envoy-Subject"
	// relayHeaderStreamSeq is the message's stream sequence, in decimal.
	relayHeaderStreamSeq = "Envoy-Stream-Seq"
	// relayHeaderStreamID is the stream's identity (bus.StreamIDMetadataKey).
	relayHeaderStreamID = "Envoy-Stream-Id"
	// relayHeaderDelivered is how many times the consumer has delivered the message, 1 the first.
	relayHeaderDelivered = "Envoy-Delivered"
	// relayHeaderStoredAt is when the stream stored the message, RFC 3339 with nanoseconds, UTC.
	relayHeaderStoredAt = "Envoy-Stored-At"
	// relayHeaderGap is "true" on the first frame after a restart of the consumer that started below
	// the stream's first sequence: messages the session had not taken are gone.
	relayHeaderGap = "Envoy-Gap"
)

// The replies a session sends on a frame's reply subject, each exactly one ASCII word.
const (
	// relayReplyReceived: the frame decoded, before any host work. The receipt.
	relayReplyReceived = "received"
	// relayReplyProgress: the frame is held unwritten; its ack wait starts again.
	relayReplyProgress = "progress"
	// relayReplyDone: the frame is written (or skipped); the message is acked. Also a receipt.
	relayReplyDone = "done"
	// relayReplyNak: the session cannot take the frame; the server delivers it again after
	// relayNakDelay.
	relayReplyNak = "nak"
)

const (
	// relayReceiptWait is how long the oldest frame a session has not answered may wait before the
	// relay takes the session for unreachable and pauses (relayPaused).
	relayReceiptWait = 3 * time.Second
	// relayPauseFirst and relayPauseMax bound the backoff after which a paused relay restarts at
	// the ack floor on its own, unless the session registers first; it doubles from the first to
	// the max and starts at the first again once the session answers a frame. A session that
	// answers late is most often alive and busy (on the 2-CPU load-test box, at 40 events/s to
	// 1,000 sessions, 60 sessions answered a frame more than 3 s after it was sent), so the first
	// restart comes 2 s after the pause, about 5 s after the frame that went unanswered, where a
	// relay waiting for the next registration waited up to a 90 s heartbeat. Each restart resends
	// everything from the ack floor, so restarts of a silent session are at least the receipt wait
	// plus the backoff apart, 5 s at first, and a slow session is not handed its backlog again
	// before it had longer to answer than the receipt wait gave it. 30 s, the cap of the retry
	// after a failed call (relayRetryMax), relays to a session that comes back within about 33 s
	// of its return (a receipt wait, then the backoff), about a third of a heartbeat, while a
	// session that never answers costs one restart and one resend of what it has in flight about
	// every 33 s: 1,000 such sessions make about 30 restarts a second, an eighth of the 250 the
	// load-test box's server sustained (relayRestartLimit).
	relayPauseFirst = 2 * time.Second
	relayPauseMax   = 30 * time.Second
	// relayNakDelay is how long the server waits to deliver a message a session refused (nak).
	relayNakDelay = time.Second
	// relayRetryFirst and relayRetryMax bound the wait before a relay whose consumer call or pull
	// failed restarts at the ack floor; it doubles from the first to the max.
	relayRetryFirst = time.Second
	relayRetryMax   = 30 * time.Second
	// pushRetryFirst and pushRetryMax bound the wait before a serving-port relay pushes its head
	// message again after the port refused it, failed or timed out; it doubles from the first to
	// the max.
	pushRetryFirst = time.Second
	pushRetryMax   = 30 * time.Second
	// relayPruneInterval is how often the relay drops lapsed session entries from the cache
	// (session.SessionRegistry.PruneLapsed), so a session that stopped registering stops being
	// relayed: a lapse has no watch event.
	relayPruneInterval = 30 * time.Second
	// relayRestartLimit is how many relays of one listener may restart their consumer at once
	// (sessionRelays.restart); the others wait their turn. A listener that starts relays every
	// session it owns at once, and a thousand restarts sent together made the server answer none
	// within the call timeout: on the 2-CPU load-test box most of the 1,000 timed out on their info
	// or delete. Restarting 1,000 consumers on the in-process server of that box, each by a delete
	// and a create (a message awaiting an ack; a restart with none sends neither), 1,000 at once took
	// 4.6 s with every call waiting 3.7 s at the median and 4.5 s at worst; 8 at a time took 4.6 s
	// too, at 33 ms a restart; 32 took 4.0 s, the most restarts a second (250), at 126 ms a restart
	// (190 ms p99); 64 took 4.2 s at 257 ms. So the server's throughput is reached from about 8 on,
	// and more only lengthens each call's wait at the server, which a registration's consumer calls
	// wait behind too. 32 keeps that wait near 200 ms on a saturated server, 25 times under
	// sessionConsumerCallTimeout, and leaves room for a hub across the network, where a restart is
	// about ten round trips and the round trip, not the server, bounds a restart: 32 at once still
	// restart 1,000 sessions in about 10 s at a 30 ms round trip, where 8 would take 40 s.
	relayRestartLimit = 32
)

// relayEvent is what the relay loop is told about a session; the flags of the events it has not
// handled yet merge.
type relayEvent uint8

const (
	// relayCheck: the session's entry changed; start or stop its relay to match it.
	relayCheck relayEvent = 0
	// relayResume: the session registered; a relay that paused starts again at once.
	relayResume relayEvent = 1 << iota
	// relayRestart: the session registered with a new epoch; its relay restarts at the ack floor.
	relayRestart
)

// relayOutcome is how one pull of a session's consumer ended (sessionRelays.relayNATS,
// sessionRelays.relayServingPort).
type relayOutcome uint8

const (
	// relayEnded: the relay was stopped, or paused until the session registers again (it has no
	// consumer, or the grant does not carry session consumers).
	relayEnded relayOutcome = iota
	// relayRetry: a consumer call or the pull failed; the relay restarts at the ack floor after a
	// wait while it is still this listener's.
	relayRetry
	// relayPaused: the session answered no frame within relayReceiptWait; the relay restarts at
	// the ack floor after a backoff (relayPauseFirst to relayPauseMax), or at once when the
	// session registers meanwhile, while it is still this listener's.
	relayPaused
)

// sessionRelays relays the session consumers this listener owns (sessionRelays.wanted): one relay
// per session, which restarts the session's consumer at its ack floor whenever it starts, then pulls
// it and hands each message on in stream order. A session on NATS (port 0) gets each message as a
// core frame on its relay subject, answered on a reply subject under the listener's one reply
// inbox; a session on a serving port gets each as the prompt_async push, one at a time.
//
// Among the tasks of one machine id, only the one holding the machine's durable binding relays
// (setOwned), so a rolling deploy's second task relays nothing until the first lets go. The relay
// set follows the watched session cache (session.SessionRegistry.OnChange), so a session that
// moves to another machine id stops being relayed here at once, and a listener that starts relays
// every session the cache holds for it with no registration.
//
// One loop goroutine owns the set of relays (relays) and applies the events it is sent in order;
// each relay runs on a goroutine of its own.
type sessionRelays struct {
	delivery  *sessionDelivery
	sessions  *session.SessionRegistry
	registry  *store.Registry
	deliverer *session.Deliverer
	// client publishes the exception of a stored message a serving-port relay refuses to push.
	client    *bus.Client
	machineID string
	logger    *logging.Logger
	// replyPrefix is the listener's reply inbox and a dot: a frame's reply subject is
	// replyPrefix<token>, and one subscription to the inbox's wildcard hears every reply.
	replyPrefix string

	ctx    context.Context
	cancel context.CancelFunc
	owned  atomic.Bool
	// stopChange unregisters the session cache's change callback.
	stopChange func()
	// runs counts the relay goroutines; loopDone is closed once the loop has returned.
	runs     sync.WaitGroup
	loopDone chan struct{}

	// eventsMu guards events and ownershipChanged, which the loop takes each time wake fires.
	eventsMu         sync.Mutex
	events           map[string]relayEvent
	ownershipChanged bool
	wake             chan struct{}

	// relays is each session's current relay; only the loop reads or writes it.
	relays map[string]*relayRun
	// listed holds, by session id, the consumers listed for the relays that gaining the durable
	// binding starts together (listConsumers), until each relay's start takes its own; nil
	// otherwise. listRefusedLogged is set once a list refused for want of the grant is logged.
	// Only the loop reads or writes them.
	listed            map[string]*jetstream.ConsumerInfo
	listRefusedLogged bool

	// restarts holds a token for each relay restarting its consumer, relayRestartLimit at most.
	restarts chan struct{}

	// framesMu guards frames and each relay's outstanding and unanswered frames.
	framesMu sync.Mutex
	// frames maps the token of every frame awaiting done or nak to it.
	frames    map[string]*relayFrame
	nextToken atomic.Uint64

	// pauseFirst and pauseMax are relayPauseFirst and relayPauseMax; a test sets shorter ones
	// before setOwned(true).
	pauseFirst, pauseMax time.Duration
}

// relayRun is one start of a session's relay: it runs until it is stopped, replaced, or pauses
// until the session registers again.
type relayRun struct {
	sessionID   string
	servingPort bool
	cancel      context.CancelFunc
	// done is closed once the run's goroutine has returned.
	done chan struct{}
	// restarted is set once the run has restarted the consumer at its ack floor, so a new epoch
	// heard after that needs a restart of its own, and one heard before it does not.
	restarted atomic.Bool
	// listed is the session's consumer as the burst that started the run listed it
	// (sessionRelays.listed), which its first restart keeps without a call when it can
	// (bus.SessionConsumers.KeepListed); nil for a run started alone, and after that first restart.
	listed *jetstream.ConsumerInfo
	// outstanding holds the run's frames awaiting done or nak, by token; unanswered holds, in the
	// order they were sent, its frames the session may not have answered yet. Both under
	// sessionRelays.framesMu.
	outstanding map[string]*relayFrame
	unanswered  []*relayFrame
	// heard is set when the session answers any of the run's frames, and cleared by the relay
	// as it takes it, so a pause after it starts a new episode (sessionRelays.relay).
	heard atomic.Bool
	// resume hears the session's registrations (sessionRelays.apply), which cut short the
	// backoff of a paused relay.
	resume chan struct{}
}

// ended reports whether the run's goroutine has returned: it was stopped, or it paused until the
// session registers again.
func (r *relayRun) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// relayFrame is a frame a NATS relay sent and the session has not answered with done or nak.
type relayFrame struct {
	run    *relayRun
	acker  bus.SessionAcker
	sentAt time.Time
	// answered is set once the session sent any reply for the frame, under framesMu.
	answered bool
}

// startSessionRelays starts the relay of delivery's session consumers and makes it delivery's
// relay, serving-port sessions included (sessionDelivery.relay). It relays nothing until
// setOwned(true).
func startSessionRelays(delivery *sessionDelivery, sessions *session.SessionRegistry, registry *store.Registry, deliverer *session.Deliverer, client *bus.Client, machineID string, logger *logging.Logger) (*sessionRelays, error) {
	inbox := nats.NewInbox()
	m := &sessionRelays{
		delivery:    delivery,
		sessions:    sessions,
		registry:    registry,
		deliverer:   deliverer,
		client:      client,
		machineID:   machineID,
		logger:      logger,
		replyPrefix: inbox + ".",
		loopDone:    make(chan struct{}),
		events:      map[string]relayEvent{},
		wake:        make(chan struct{}, 1),
		relays:      map[string]*relayRun{},
		restarts:    make(chan struct{}, relayRestartLimit),
		frames:      map[string]*relayFrame{},
		pauseFirst:  relayPauseFirst,
		pauseMax:    relayPauseMax,
	}
	if err := delivery.consumers.SubscribeReplies(inbox+".>", m.onReply); err != nil {
		return nil, err
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.stopChange = sessions.OnChange(m.onChange)
	delivery.relay = m
	go m.loop()
	return m, nil
}

// setOwned records whether this task holds its machine's durable binding. Gaining it starts the
// relay of every session the cache holds for this machine, each restarted at its ack floor; losing
// it stops every relay.
func (m *sessionRelays) setOwned(owned bool) {
	if m == nil || m.owned.Swap(owned) == owned {
		return
	}
	m.logger.Info("session relays follow the durable binding", slog.Bool("relaying", owned))
	m.eventsMu.Lock()
	m.ownershipChanged = true
	m.eventsMu.Unlock()
	m.signal()
}

// registered implements sessionRelay: the session registered, so a relay that paused resumes and a
// new epoch restarts it at the ack floor.
func (m *sessionRelays) registered(sessionID string, epochChanged bool) {
	event := relayResume
	if epochChanged {
		event |= relayRestart
	}
	m.enqueue(sessionID, event)
}

// onChange hears every change to a session's cached entry.
func (m *sessionRelays) onChange(sessionID string) {
	if m.owned.Load() {
		m.enqueue(sessionID, relayCheck)
	}
}

func (m *sessionRelays) enqueue(sessionID string, event relayEvent) {
	m.eventsMu.Lock()
	if m.events == nil {
		m.eventsMu.Unlock()
		return
	}
	m.events[sessionID] |= event
	m.eventsMu.Unlock()
	m.signal()
}

func (m *sessionRelays) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// stop stops every relay and waits, within ctx, for their goroutines to return, so no relay acks or
// publishes after it; the session-consumer connection is drained after it (sessionDelivery.drain).
func (m *sessionRelays) stop(ctx context.Context) {
	if m == nil {
		return
	}
	m.stopChange()
	m.eventsMu.Lock()
	m.events = nil
	m.eventsMu.Unlock()
	m.cancel()
	select {
	case <-m.loopDone:
	case <-ctx.Done():
		m.logger.Warn("session relay loop still running at shutdown")
		return
	}
	if !waitWithin(ctx, &m.runs) {
		m.logger.Warn("session relays still running at shutdown")
	}
}

// loop applies the relay's events in order until the relay stops, and prunes the lapsed entries of
// the session cache every relayPruneInterval.
func (m *sessionRelays) loop() {
	defer close(m.loopDone)
	prune := time.NewTicker(relayPruneInterval)
	defer prune.Stop()
	for {
		select {
		case <-m.ctx.Done():
			m.stopAll()
			return
		case <-prune.C:
			// Each lapse it finds is a change, which onChange hears.
			m.sessions.PruneLapsed()
			continue
		case <-m.wake:
		}
		m.eventsMu.Lock()
		events, ownershipChanged := m.events, m.ownershipChanged
		m.ownershipChanged = false
		if events == nil {
			// stop has begun; the cancellation it makes next ends the loop.
			m.eventsMu.Unlock()
			continue
		}
		m.events = map[string]relayEvent{}
		m.eventsMu.Unlock()
		if ownershipChanged {
			if !m.owned.Load() {
				m.stopAll()
				continue
			}
			entries, err := m.sessions.List()
			if err != nil {
				m.logger.Warn("session relays could not list the session cache", slog.String("error", err.Error()))
			}
			// A session whose entry is not on consumer delivery and that has no relay to stop needs
			// nothing (wanted).
			for _, entry := range entries {
				if _, ok := events[entry.SessionID]; !ok && (entry.Delivery == session.DeliveryConsumer || m.relays[entry.SessionID] != nil) {
					events[entry.SessionID] = relayCheck
				}
			}
			m.listed = m.listConsumers()
		}
		for sessionID, event := range events {
			m.apply(sessionID, event)
		}
		m.listed = nil
	}
}

// apply makes sessionID's relay match its entry and event: started when the session is this
// listener's to relay, stopped when it is not, resumed after a pause when it registered (or, for a
// serving port, when its entry changed: such a session may register through a listener other than
// its owner), and restarted at the ack floor on a new epoch. A registration also cuts short the
// backoff of a relay paused for want of an answer (relayRun.resume).
func (m *sessionRelays) apply(sessionID string, event relayEvent) {
	want, servingPort := m.wanted(sessionID)
	run := m.relays[sessionID]
	if !want {
		if run != nil {
			run.cancel()
			delete(m.relays, sessionID)
		}
		return
	}
	switch {
	case run == nil:
		m.start(sessionID, servingPort, nil)
	case run.servingPort != servingPort:
		run.cancel()
		m.start(sessionID, servingPort, run.done)
	case run.ended():
		if event&relayResume != 0 || servingPort {
			m.start(sessionID, servingPort, run.done)
		}
	case event&relayRestart != 0 && run.restarted.Load():
		run.cancel()
		m.start(sessionID, servingPort, run.done)
	case event&relayResume != 0:
		select {
		case run.resume <- struct{}{}:
		default:
		}
	}
}

// stopAll stops every relay.
func (m *sessionRelays) stopAll() {
	for sessionID, run := range m.relays {
		run.cancel()
		delete(m.relays, sessionID)
	}
}

// wanted reports whether this listener relays sessionID, and whether the session is on a serving
// port: it holds its machine's durable binding, and the session's entry records consumer delivery
// and names this machine. A serving-port session is the machine's on its stored interest row, else
// on its entry, as the durable path's push decides (session.Deliverer), since only that machine
// reaches its port.
func (m *sessionRelays) wanted(sessionID string) (want, servingPort bool) {
	if !m.owned.Load() {
		return false, false
	}
	entry, err := m.sessions.Get(sessionID)
	if err != nil || entry.Delivery != session.DeliveryConsumer {
		return false, false
	}
	if entry.Port <= 0 {
		return entry.MachineID == m.machineID, false
	}
	owner := entry.MachineID
	if interest, err := m.registry.Get(m.ctx, sessionID); err == nil && interest.MachineID != "" {
		owner = interest.MachineID
	}
	return owner == m.machineID, true
}

// listConsumers reads every session consumer at once (bus.SessionConsumers.ListSessionConsumers)
// for the relays that gaining the durable binding starts together, so a relay whose consumer a
// restart would keep unchanged starts pulling with no call of its own (sessionRelays.restart). It
// returns nil when the list fails, and each relay then reads its own consumer; a list the NATS
// user may not make is logged once.
func (m *sessionRelays) listConsumers() map[string]*jetstream.ConsumerInfo {
	ctx, cancel := context.WithTimeout(m.ctx, sessionConsumerCallTimeout)
	defer cancel()
	listed, err := m.delivery.consumers.ListSessionConsumers(ctx)
	switch {
	case err == nil:
		return listed
	case consumerGrantRefused(err):
		if !m.listRefusedLogged {
			m.listRefusedLogged = true
			m.logger.Warn("the NATS user may not list session consumers; each relay reads its own consumer at the listener's start", slog.String("error", err.Error()))
		}
	default:
		m.logger.Warn("session relays could not list the session consumers; each relay reads its own", slog.String("error", err.Error()))
	}
	return nil
}

// start starts a run of sessionID's relay, once the run it replaces, previous, has returned. A run
// started by the burst of gaining the durable binding takes the session's listed consumer.
func (m *sessionRelays) start(sessionID string, servingPort bool, previous <-chan struct{}) {
	ctx, cancel := context.WithCancel(m.ctx)
	run := &relayRun{sessionID: sessionID, servingPort: servingPort, cancel: cancel, done: make(chan struct{}), listed: m.listed[sessionID], resume: make(chan struct{}, 1)}
	delete(m.listed, sessionID)
	m.relays[sessionID] = run
	m.runs.Go(func() {
		defer close(run.done)
		// A run replaced is always stopped first, so it returns promptly; the wait keeps two pulls
		// of one consumer from overlapping.
		if previous != nil {
			<-previous
		}
		m.relay(ctx, run)
	})
}

// relay runs one session's relay until it is stopped or pauses until the session registers again,
// while the session is still this listener's. It restarts at the ack floor after a failed call or
// pull, with a wait that doubles from relayRetryFirst to relayRetryMax, and after a pause for want
// of an answer (relayPaused), with a backoff that doubles from pauseFirst to pauseMax, cut short by
// the session's registration. One pause episode, from a pause to the session's next answer to any
// frame, which sets the backoff back to pauseFirst, is logged once.
func (m *sessionRelays) relay(ctx context.Context, run *relayRun) {
	retry := relayRetryFirst
	var pause time.Duration // the next pause's backoff; 0 outside a pause episode
	for {
		var outcome relayOutcome
		if run.servingPort {
			outcome = m.relayServingPort(ctx, run)
		} else {
			outcome = m.relayNATS(ctx, run)
		}
		if run.heard.Swap(false) {
			pause = 0
		}
		switch outcome {
		case relayEnded:
			return
		case relayPaused:
			if pause == 0 {
				pause = m.pauseFirst
				m.logger.Info("session relay paused: the session answered no frame within the receipt wait; it restarts at the ack floor after a backoff, or at once when the session registers",
					slog.String("session_id", run.sessionID), slog.String("receipt_wait", relayReceiptWait.String()), slog.String("backoff", pause.String()))
			}
			// A registration heard before the pause does not cut its backoff short.
			select {
			case <-run.resume:
			default:
			}
			if !awaitResume(ctx, run, pause) {
				return
			}
			pause = min(pause*2, m.pauseMax)
		default:
			if waitUnlessDone(ctx, retry) != nil {
				return
			}
			retry = min(retry*2, relayRetryMax)
		}
		if want, servingPort := m.wanted(run.sessionID); !want || servingPort != run.servingPort {
			return
		}
	}
}

// awaitResume waits out a paused relay's backoff, or until the session registers
// (relayRun.resume), and reports whether the relay is still running.
func awaitResume(ctx context.Context, run *relayRun, backoff time.Duration) bool {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-run.resume:
		return true
	case <-ctx.Done():
		return false
	}
}

// restart restarts the session's consumer at its ack floor with its current filter and starts
// pulling it. A pull of nil comes with the outcome to return: relayEnded when the relay was stopped
// or pauses (the session has no consumer, which its next registration creates, or the grant does
// not carry session consumers), relayRetry otherwise.
//
// A restart that read the floor and then failed, its delete or its create, leaves the consumer's
// start pending (bus.SessionConsumers.RestartAtFloor): the retry creates a consumer it finds
// missing at that start rather than pausing, and so does a registration's create meanwhile, so no
// restart loses the session's position. Only a consumer missing with no start pending (never
// created, or removed after its inactive threshold) pauses the relay until the next registration.
//
// The first restart of a run the burst of gaining the durable binding started has the session's
// consumer as that burst listed it (relayRun.listed): when the restart would keep it unchanged
// (bus.SessionConsumers.KeepListed), the relay takes no turn and sends no call before it pulls.
func (m *sessionRelays) restart(ctx context.Context, run *relayRun) (bus.SessionConsumerStart, *bus.SessionPull, relayOutcome) {
	sessionID := run.sessionID
	topics, err := storedInterestTopics(ctx, m.registry, sessionID)
	if err != nil {
		m.logger.Warn("session relay could not read the session's interests; retrying", slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return bus.SessionConsumerStart{}, nil, relayRetry
	}
	filter := sessionConsumerFilter(sessionID, topics)
	if listed := run.listed; listed != nil {
		run.listed = nil
		unlock := m.delivery.locks.lock(sessionID)
		callCtx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
		defer cancel()
		start, kept, err := m.delivery.consumers.KeepListed(callCtx, sessionID, filter, listed)
		unlock()
		if err == nil && kept {
			run.restarted.Store(true)
			var pull *bus.SessionPull
			if pull, err = m.delivery.consumers.Pull(callCtx, sessionID); err == nil {
				return start, pull, relayRetry
			}
		}
		if err != nil {
			return m.restartFailed(ctx, sessionID, err)
		}
	}
	// The restart takes a turn (relayRestartLimit) first and its session's lock second, and only
	// then starts its calls' timeout, so the time it waited in line times nothing out. A
	// registration takes no turn, only the lock, so its consumer calls never wait behind the
	// restarts waiting for one.
	select {
	case m.restarts <- struct{}{}:
	case <-ctx.Done():
		return bus.SessionConsumerStart{}, nil, relayEnded
	}
	defer func() { <-m.restarts }()
	unlock := m.delivery.locks.lock(sessionID)
	callCtx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	start, err := m.delivery.consumers.RestartAtFloor(callCtx, sessionID, filter)
	unlock()
	if err == nil {
		run.restarted.Store(true)
		var pull *bus.SessionPull
		if pull, err = m.delivery.consumers.Pull(callCtx, sessionID); err == nil {
			return start, pull, relayRetry
		}
	}
	return m.restartFailed(ctx, sessionID, err)
}

// restartFailed is the outcome of a restart (sessionRelays.restart) whose call or pull failed
// with err.
func (m *sessionRelays) restartFailed(ctx context.Context, sessionID string, err error) (bus.SessionConsumerStart, *bus.SessionPull, relayOutcome) {
	switch {
	case ctx.Err() != nil:
		return bus.SessionConsumerStart{}, nil, relayEnded
	case errors.Is(err, bus.ErrSessionConsumerNotFound) || errors.Is(err, bus.ErrNoSessionConsumerName):
		m.logger.Info("session relay paused: the session has no consumer; its next registration creates one", slog.String("session_id", sessionID))
		return bus.SessionConsumerStart{}, nil, relayEnded
	case consumerGrantRefused(err):
		m.delivery.refused(sessionID, err)
		return bus.SessionConsumerStart{}, nil, relayEnded
	default:
		m.logger.Warn("session relay could not restart the session's consumer; retrying", slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return bus.SessionConsumerStart{}, nil, relayRetry
	}
}

// relayNATS restarts a NATS session's consumer at its ack floor and publishes each message it pulls
// as a frame on the session's relay subject, in stream order, with up to the consumer's
// MaxAckPending in flight. It pauses (relayPaused), sending nothing more, once the oldest frame the
// session has not answered has waited relayReceiptWait: the frames in flight are left to their ack
// wait or the next restart, which the relay makes after its backoff or at the session's next
// registration (sessionRelays.relay).
func (m *sessionRelays) relayNATS(ctx context.Context, run *relayRun) relayOutcome {
	sessionID := run.sessionID
	start, pull, outcome := m.restart(ctx, run)
	if pull == nil {
		return outcome
	}
	defer pull.Stop()
	if start.StreamID == "" {
		m.logger.Warn("session relay could not read the stream's identity; retrying", slog.String("session_id", sessionID), slog.String("error", bus.ErrNoStreamID.Error()))
		return relayRetry
	}
	defer m.forget(run)
	streamID, gap := start.StreamID, start.Gap
	for {
		next := ctx
		cancelNext := context.CancelFunc(func() {})
		if deadline, waiting := m.receiptDeadline(run); waiting {
			if !time.Now().Before(deadline) {
				return relayPaused
			}
			next, cancelNext = context.WithDeadline(ctx, deadline)
		}
		msg, err := pull.Next(next)
		cancelNext()
		if err != nil {
			if ctx.Err() != nil {
				return relayEnded
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			m.logger.Warn("session relay pull ended; restarting at the ack floor", slog.String("session_id", sessionID), slog.String("error", err.Error()))
			return relayRetry
		}
		if err := m.publishFrame(ctx, run, msg, streamID, gap); err != nil {
			if ctx.Err() != nil {
				return relayEnded
			}
			m.logger.Warn("session relay could not publish a frame; restarting at the ack floor", slog.String("session_id", sessionID), slog.String("error", err.Error()))
			return relayRetry
		}
		gap = false
	}
}

// publishFrame publishes msg as a frame on the session's relay subject, its reply subject a token
// of its own under the listener's reply inbox.
func (m *sessionRelays) publishFrame(ctx context.Context, run *relayRun, msg bus.SessionMessage, streamID string, gap bool) error {
	token := strconv.FormatUint(m.nextToken.Add(1), 36)
	frame := &relayFrame{run: run, acker: msg.SessionAcker, sentAt: time.Now()}
	m.framesMu.Lock()
	if run.outstanding == nil {
		run.outstanding = map[string]*relayFrame{}
	}
	run.outstanding[token] = frame
	run.unanswered = append(run.unanswered, frame)
	m.frames[token] = frame
	m.framesMu.Unlock()
	header := nats.Header{}
	header.Set(relayHeaderSubject, msg.Subject)
	header.Set(relayHeaderStreamSeq, strconv.FormatUint(msg.StreamSeq, 10))
	header.Set(relayHeaderStreamID, streamID)
	header.Set(relayHeaderDelivered, strconv.FormatUint(msg.Delivered, 10))
	header.Set(relayHeaderStoredAt, msg.StoredAt.UTC().Format(time.RFC3339Nano))
	if gap {
		header.Set(relayHeaderGap, "true")
	}
	return m.delivery.consumers.Publish(ctx, &nats.Msg{
		Subject: bus.DeliverSubject(run.sessionID),
		Reply:   m.replyPrefix + token,
		Header:  header,
		Data:    msg.Data,
	})
}

// receiptDeadline is when the oldest frame of run the session has not answered reaches
// relayReceiptWait, and whether there is one.
func (m *sessionRelays) receiptDeadline(run *relayRun) (time.Time, bool) {
	m.framesMu.Lock()
	defer m.framesMu.Unlock()
	for len(run.unanswered) > 0 && run.unanswered[0].answered {
		run.unanswered[0] = nil
		run.unanswered = run.unanswered[1:]
	}
	if len(run.unanswered) == 0 {
		return time.Time{}, false
	}
	return run.unanswered[0].sentAt.Add(relayReceiptWait), true
}

// forget drops every frame of run still awaiting an answer: a reply to one is ignored, and its
// message waits out its ack wait or the next restart at the ack floor.
func (m *sessionRelays) forget(run *relayRun) {
	m.framesMu.Lock()
	defer m.framesMu.Unlock()
	for token := range run.outstanding {
		delete(m.frames, token)
	}
	run.outstanding = nil
	run.unanswered = nil
}

// onReply hears a session's reply to a frame: received marks the frame answered, progress restarts
// its ack wait, done acks it and nak has the server deliver it again after relayNakDelay; done and
// nak end the frame. Any of them ends a pause episode (relayRun.heard). A reply to a token no frame
// awaits, or with any other body, is ignored.
func (m *sessionRelays) onReply(msg *nats.Msg) {
	token, ok := strings.CutPrefix(msg.Subject, m.replyPrefix)
	if !ok {
		return
	}
	reply := string(bytes.TrimSpace(msg.Data))
	m.framesMu.Lock()
	frame := m.frames[token]
	if frame == nil {
		m.framesMu.Unlock()
		return
	}
	switch reply {
	case relayReplyReceived, relayReplyProgress:
		frame.answered = true
	case relayReplyDone, relayReplyNak:
		frame.answered = true
		delete(m.frames, token)
		delete(frame.run.outstanding, token)
	default:
		m.framesMu.Unlock()
		return
	}
	frame.run.heard.Store(true)
	m.framesMu.Unlock()
	var err error
	switch reply {
	case relayReplyProgress:
		err = frame.acker.InProgress()
	case relayReplyDone:
		err = frame.acker.Ack()
	case relayReplyNak:
		err = frame.acker.NakWithDelay(relayNakDelay)
	}
	if err != nil {
		m.logger.Warn("session relay could not answer the consumer for a frame", slog.String("session_id", frame.run.sessionID), slog.String("reply", reply), slog.String("error", err.Error()))
	}
}

// relayServingPort restarts a serving-port session's consumer at its ack floor and pushes each
// message it pulls to the session's port, one at a time, in stream order (pushUntilAccepted).
func (m *sessionRelays) relayServingPort(ctx context.Context, run *relayRun) relayOutcome {
	_, pull, outcome := m.restart(ctx, run)
	if pull == nil {
		return outcome
	}
	defer pull.Stop()
	var pushedThrough uint64
	for {
		msg, err := pull.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return relayEnded
			}
			m.logger.Warn("session relay pull ended; restarting at the ack floor", slog.String("session_id", run.sessionID), slog.String("error", err.Error()))
			return relayRetry
		}
		if msg.StreamSeq <= pushedThrough {
			// A copy of a message this pull already pushed and acked: the server delivered it again
			// once its ack wait ran out while it waited in the pull behind a head message the port
			// kept refusing.
			_ = msg.Ack()
			continue
		}
		if !m.pushUntilAccepted(ctx, run.sessionID, msg) {
			return relayEnded
		}
		pushedThrough = msg.StreamSeq
	}
}

// pushUntilAccepted pushes msg to the session's serving port until the port answers 204, then acks
// it. Any other answer, a transport error or a timeout keeps it at the head of the session's
// messages: it is pushed again after a wait that doubles from pushRetryFirst to pushRetryMax, kept
// in progress so its ack wait never runs out. It returns false when the relay was stopped first.
//
// A message on a topic a session follows (not its own agent subject or a role) is refused as the
// durable path's fan-out refuses it: one the session published itself is acked unpushed, and one
// whose delivery mode the session does not advertise is acked with a delivery_failed exception.
func (m *sessionRelays) pushUntilAccepted(ctx context.Context, sessionID string, msg bus.SessionMessage) bool {
	var item contracts.Envelope
	if err := decodeStoredEnvelope(msg.Data, &item); err != nil {
		m.logger.Error("session relay dropped a stored message that is no valid envelope", slog.String("session_id", sessionID), slog.String("subject", msg.Subject), slog.String("error", err.Error()))
		_ = msg.Ack()
		return true
	}
	if m.refuseFollowedTopic(sessionID, item) {
		_ = msg.Ack()
		return true
	}
	retry := pushRetryFirst
	for attempt := 1; ; attempt++ {
		err := m.push(ctx, sessionID, item, msg)
		if err == nil {
			if ackErr := msg.Ack(); ackErr != nil {
				m.logger.Warn("session relay could not ack a pushed message", slog.String("session_id", sessionID), slog.String("event_id", item.EventID), slog.String("error", ackErr.Error()))
			}
			m.logger.DeliveryLog(slog.LevelInfo, "session relay pushed", sessionID, item.Topic, item.EventID, "delivered", slog.Int("attempts", attempt))
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		m.logger.DeliveryLog(slog.LevelWarn, "session relay push refused; the message waits at the head of the session's queue", sessionID, item.Topic, item.EventID, "retrying",
			slog.Int("attempt", attempt), slog.String("retry_in", retry.String()), slog.String("error", err.Error()))
		_ = msg.InProgress()
		if waitUnlessDone(ctx, retry) != nil {
			return false
		}
		retry = min(retry*2, pushRetryMax)
	}
}

// push makes one prompt_async push of item to the session's port at the address its current entry
// gives, keeping msg in progress first.
func (m *sessionRelays) push(ctx context.Context, sessionID string, item contracts.Envelope, msg bus.SessionMessage) error {
	entry, err := m.sessions.Get(sessionID)
	if err != nil {
		return err
	}
	_ = msg.InProgress()
	return m.deliverer.Push(ctx, item, sessionID, entry)
}

// refuseFollowedTopic applies the durable path's fan-out refusals (fanoutDelivery) to item, a
// message on a topic the session follows, and reports whether item is refused: the session
// published it (an echo), or its delivery mode cannot be read or is one the session does not
// advertise, which is reported as a delivery_failed exception. A message on the session's agent
// subject, a role forward included, is never refused here.
func (m *sessionRelays) refuseFollowedTopic(sessionID string, item contracts.Envelope) bool {
	if isControlTopic(item.Topic) {
		return false
	}
	if item.SourceSession == sessionID {
		m.logger.DeliveryLog(slog.LevelInfo, "session relay skipped an echo", sessionID, item.Topic, item.EventID, "skipped")
		return true
	}
	mode, err := envelopeDeliveryMode(item)
	if err == nil && mode != "" {
		if entry, lookupErr := m.sessions.Get(sessionID); lookupErr == nil && !hasCapability(entry.Capabilities, mode) {
			err = errors.New("the session does not advertise delivery mode " + mode)
		}
	}
	if err == nil {
		return false
	}
	m.logger.DeliveryLog(slog.LevelWarn, "session relay refused a message the session cannot take", sessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
	if publishErr := publishDeliveryException(m.client, item, "delivery_failed", sessionID, true, ""); publishErr != nil {
		m.logger.Warn("session relay could not publish a delivery exception", slog.String("session_id", sessionID), slog.String("event_id", item.EventID), slog.String("error", publishErr.Error()))
	}
	return true
}

// decodeStoredEnvelope decodes a stored message into item under the durable path's rules
// (listenerDeliveryHandler): JSON, the wire rules, then the envelope's own.
func decodeStoredEnvelope(data []byte, item *contracts.Envelope) error {
	if err := json.Unmarshal(data, item); err != nil {
		return err
	}
	if err := contracts.ValidateWire(data); err != nil {
		return err
	}
	return item.Validate()
}
