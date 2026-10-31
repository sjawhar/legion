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
	// relay takes the session for unreachable and pauses: it sends nothing more until it probes the
	// session again after relayPauseFirst, or the session registers.
	relayReceiptWait = 3 * time.Second
	// relayPauseFirst and relayPauseMax bound the wait before a paused relay restarts at the ack
	// floor and sends the session its unanswered messages again; it doubles from the first to the
	// max while the session answers none of them, and a frame the session answers puts it back to
	// the first (sessionRelays.relay).
	//
	// The first wait is a little under the receipt wait, so a session that was merely slow -- on the
	// 2-CPU load-test box a receipt took 2.1 s at the 99th percentile, late but alive -- has its
	// messages again about 5 s after the frame it missed, and a session that is truly gone costs one
	// restart of its consumer every 5 s at first. The max is relayRetryMax, so a relay's longest
	// idle wait is the same whatever stopped it, and it is three times under the 90 s heartbeat
	// whose registration used to be the only resume: a session that comes back has its messages
	// within 33 s of the probe that found it silent, against 85 s measured before (the waits run
	// 2, 4, 8, 16, then 30 s, each after a 3 s receipt wait, so the fifth probe is 42 s in).
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
	// relayHeldRecheck is how often the relay judges a message it holds again: one another path is
	// already pushing (holdWhilePushedElsewhere), which it neither pushes nor acks until that push
	// lands or the attempt record naming it is gone.
	relayHeldRecheck = time.Second
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
	// relayResume: the session registered; a relay that paused starts again.
	relayResume relayEvent = 1 << iota
	// relayRestart: the session registered with a new epoch; its relay restarts at the ack floor.
	relayRestart
)

// relayOutcome is how one pull of a session's consumer ended (sessionRelays.relayNATS,
// sessionRelays.relayServingPort).
type relayOutcome uint8

const (
	// relayEnded: the relay was stopped, or it may start again only at the session's next
	// registration.
	relayEnded relayOutcome = iota
	// relayRetry: a consumer call or the pull failed; the relay restarts at the ack floor after a
	// wait while it is still this listener's.
	relayRetry
	// relayPaused: the session answered no frame within relayReceiptWait; the relay restarts at the
	// ack floor after the pause backoff, or at the session's next registration, whichever is first.
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
	registry  *store.Registry
	deliverer *session.Deliverer
	// client publishes the exception of a stored message a serving-port relay refuses to push.
	client *bus.Client
	// rules are the refusals a message on a topic the session follows is judged by
	// (judgeFollowedTopic), the durable path's fan-out and this relay alike; its sessions are the
	// session entries this relay reads too.
	rules     followedTopicRules
	machineID string
	logger    *logging.Logger
	// replyPrefix is the listener's reply inbox and a dot: a frame's reply subject is
	// replyPrefix<token>, and one subscription to the inbox's wildcard hears every reply.
	replyPrefix string
	// pauseFirst and pauseMax are the bounds of a paused relay's backoff, relayPauseFirst and
	// relayPauseMax; a test shortens them. Set before the first relay starts and read-only after.
	pauseFirst time.Duration
	pauseMax   time.Duration

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
}

// relayRun is one start of a session's relay: it runs until it is stopped or replaced. A run of a
// NATS session that the session stops answering pauses (relayPaused) and goes on probing it.
type relayRun struct {
	sessionID   string
	servingPort bool
	cancel      context.CancelFunc
	// done is closed once the run's goroutine has returned.
	done chan struct{}
	// restarted is set once the run has restarted the consumer at its ack floor, so a new epoch
	// heard after that needs a restart of its own, and one heard before it does not.
	restarted atomic.Bool
	// resume carries, at most one at a time, the registrations heard for the session: a paused run
	// takes one instead of waiting its backoff out (sessionRelays.apply, relayRun.waitPause).
	resume chan struct{}
	// answered is set whenever the session answers a frame of the run and cleared each time the run
	// pauses, so the pause reads whether the session answered anything since the last one: the
	// backoff starts again from the first wait and the pause is logged as a new episode.
	answered atomic.Bool
	// listed is the session's consumer as the burst that started the run listed it
	// (sessionRelays.listed), which its first restart keeps without a call when it can
	// (bus.SessionConsumers.KeepListed); nil for a run started alone, and after that first restart.
	listed *jetstream.ConsumerInfo
	// outstanding holds the run's frames awaiting done or nak, by token; unanswered holds, in the
	// order they were sent, its frames the session may not have answered yet. Both under
	// sessionRelays.framesMu.
	outstanding map[string]*relayFrame
	unanswered  []*relayFrame
}

// wake has a paused run restart at once: the session registered. A wake sent while the run is not
// paused is dropped by the next pause, which names a registration heard before it.
func (r *relayRun) wake() {
	select {
	case r.resume <- struct{}{}:
	default:
	}
}

// waitPause waits out a paused run's backoff, d, the session's next registration, or the end of the
// run, and reports whether the run goes on.
func (r *relayRun) waitPause(ctx context.Context, d time.Duration) bool {
	select {
	case <-r.resume:
	default:
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.resume:
	case <-timer.C:
	}
	return true
}

// ended reports whether the run's goroutine has returned: it was stopped, or it may start again
// only at the session's next registration.
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
func startSessionRelays(delivery *sessionDelivery, cfg listenerDeliveryHandlerConfig) (*sessionRelays, error) {
	inbox := nats.NewInbox()
	m := &sessionRelays{
		delivery:    delivery,
		registry:    cfg.registry,
		deliverer:   cfg.deliverer,
		client:      cfg.client,
		rules:       cfg.followedTopicRules(),
		machineID:   cfg.machineID,
		logger:      cfg.logger,
		replyPrefix: inbox + ".",
		pauseFirst:  relayPauseFirst,
		pauseMax:    relayPauseMax,
		loopDone:    make(chan struct{}),
		events:      map[string]relayEvent{},
		wake:        make(chan struct{}, 1),
		relays:      map[string]*relayRun{},
		restarts:    make(chan struct{}, relayRestartLimit),
		frames:      map[string]*relayFrame{},
	}
	if err := delivery.consumers.SubscribeReplies(inbox+".>", m.onReply); err != nil {
		return nil, err
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.stopChange = cfg.sessions.OnChange(m.onChange)
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
			m.rules.sessions.PruneLapsed()
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
			entries, err := m.rules.sessions.List()
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
// listener's to relay, stopped when it is not, started again when it registered and the run ended
// (or, for a serving port, when its entry changed: such a session may register through a listener
// other than its owner), restarted at the ack floor on a new epoch, and, for a run waiting out a
// pause, woken by the registration so it probes the session at once.
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
		run.wake()
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
	entry, err := m.rules.sessions.Get(sessionID)
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
	run := &relayRun{sessionID: sessionID, servingPort: servingPort, cancel: cancel, done: make(chan struct{}), resume: make(chan struct{}, 1), listed: m.listed[sessionID]}
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

// relay runs one session's relay until it is stopped, restarting at the ack floor after a failed
// call or pull, with a wait that doubles from relayRetryFirst to relayRetryMax, and after a pause
// (relayPaused), with a wait that doubles from pauseFirst to pauseMax, cut short by the session's
// next registration. Both only while the session is still this listener's.
//
// The pause is logged once an episode: the line is written for the first pause since the session
// last answered a frame, not for each probe that found it silent again.
func (m *sessionRelays) relay(ctx context.Context, run *relayRun) {
	retry, pause, paused := relayRetryFirst, m.pauseFirst, false
	for {
		var outcome relayOutcome
		if run.servingPort {
			outcome = m.relayServingPort(ctx, run)
		} else {
			outcome = m.relayNATS(ctx, run)
		}
		switch outcome {
		case relayEnded:
			return
		case relayPaused:
			if run.answered.Swap(false) {
				// The session answered a frame since the last pause: this one is a new episode.
				pause, paused = m.pauseFirst, false
			}
			if !paused {
				paused = true
				m.logger.Info("session relay paused: the session answered no frame within the receipt wait; it sends again after the pause backoff, or at the session's next registration",
					slog.String("session_id", run.sessionID),
					slog.String("receipt_wait", relayReceiptWait.String()),
					slog.String("backoff", pause.String()))
			}
			if !run.waitPause(ctx, pause) {
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
// MaxAckPending in flight. It pauses, sending nothing more, once the oldest frame the session has
// not answered has waited relayReceiptWait: the frames in flight are left to their ack wait, and
// the relay starts again at the ack floor after its pause backoff or at the session's next
// registration, whichever comes first (sessionRelays.relay), so the messages it was sending reach
// a session that was only slow without waiting for a registration.
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
// nak end the frame. Any of them is the session answering, which puts the run's pause backoff back
// to its first wait. A reply to a token no frame awaits, or with any other body, is ignored.
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
	frame.run.answered.Store(true)
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

// pushUntilAccepted pushes msg to the session's serving port until the port answers 204, then
// records the dedupe key and acks it. Any other answer, a transport error or a timeout keeps it at
// the head of the session's messages: it is pushed again after a wait that doubles from
// pushRetryFirst to pushRetryMax (pushWithBackoff, the durable path's worker pushing likewise),
// kept in progress so its ack wait never runs out. It returns false when the relay was stopped
// first.
//
// A message on a topic a session follows (not its own agent subject or a role) is judged as the
// durable path's fan-out judges it (followedTopicVerdict): one the session already has is acked
// unpushed, and one another path is already pushing is held until that push lands or ends
// (holdWhilePushedElsewhere).
func (m *sessionRelays) pushUntilAccepted(ctx context.Context, sessionID string, msg bus.SessionMessage) bool {
	var item contracts.Envelope
	if err := decodeStoredEnvelope(msg.Data, &item); err != nil {
		m.logger.Error("session relay dropped a stored message that is no valid envelope", slog.String("session_id", sessionID), slog.String("subject", msg.Subject), slog.String("error", err.Error()))
		_ = msg.Ack()
		return true
	}
	switch m.holdWhilePushedElsewhere(ctx, sessionID, item, msg) {
	case relayAcksIt:
		_ = msg.Ack()
		return true
	case relayStopped:
		// The relay was stopped while it held the message, which stays unacked: whichever relay
		// starts next pulls it again.
		return false
	}
	return pushWithBackoff(ctx, m.logger, sessionID, item, func(attempt int) (bool, error) {
		if err := m.push(ctx, sessionID, item, msg); err != nil {
			_ = msg.InProgress()
			return false, err
		}
		// The port took it, so the key is recorded as a delivery of the durable path records it
		// (applyDeliveryOutcome's recordDedupe): another copy of the same event, stored on another
		// topic this session follows, is refused rather than pushed again.
		m.rules.dedupeCache.Record(item.DedupeKey, sessionID)
		if ackErr := msg.Ack(); ackErr != nil {
			m.logger.Warn("session relay could not ack a pushed message", slog.String("session_id", sessionID), slog.String("event_id", item.EventID), slog.String("error", ackErr.Error()))
		}
		m.logger.DeliveryLog(slog.LevelInfo, "session relay pushed", sessionID, item.Topic, item.EventID, "delivered", slog.Int("attempts", attempt))
		return true, nil
	})
}

// push makes one prompt_async push of item to the session's port at the address its current entry
// gives, keeping msg in progress first.
func (m *sessionRelays) push(ctx context.Context, sessionID string, item contracts.Envelope, msg bus.SessionMessage) error {
	entry, err := m.rules.sessions.Get(sessionID)
	if err != nil {
		return err
	}
	_ = msg.InProgress()
	return m.deliverer.Push(ctx, item, sessionID, entry)
}

// relayVerdict is what the relay of a serving-port session's consumer does with a message on a
// topic the session follows (sessionRelays.followedTopicVerdict).
type relayVerdict uint8

const (
	// relayPushesIt: the relay pushes the message to the session's port.
	relayPushesIt relayVerdict = iota
	// relayAcksIt: the message is refused, and acked unpushed.
	relayAcksIt
	// relayHoldsIt: another path is already pushing this message (an attempt record), so the relay
	// neither pushes nor acks it yet (holdWhilePushedElsewhere).
	relayHoldsIt
	// relayStopped: the relay was stopped while it held the message, which stays unacked.
	relayStopped
)

// followedTopicVerdict applies the durable path's judgement (judgeFollowedTopic) to item, a
// message on a topic the session follows, and says what the relay does with it: a copy the
// session already took, an echo of its own message, and a delivery mode it does not advertise or
// that cannot be read are acked unpushed, the last reported as a delivery_failed exception as the
// fan-out reports it; a copy another path is already pushing is held. A message on the session's
// agent subject, a role forward included, is addressed to this session alone and is never refused
// here.
func (m *sessionRelays) followedTopicVerdict(sessionID string, item contracts.Envelope) relayVerdict {
	if isControlTopic(item.Topic) {
		return relayPushesIt
	}
	mode, modeErr := envelopeDeliveryMode(item)
	verdict, _, err := judgeFollowedTopic(m.rules, sessionID, item, mode, modeErr)
	switch verdict {
	case takeFollowedTopic:
		return relayPushesIt
	case followedTopicEcho:
		m.logger.DeliveryLog(slog.LevelInfo, "session relay skipped an echo", sessionID, item.Topic, item.EventID, "skipped")
		return relayAcksIt
	case followedTopicDeduped:
		m.logger.DeliveryLog(slog.LevelInfo, "session relay skipped a copy the session already has", sessionID, item.Topic, item.EventID, "dedupe", slog.String("dedupe_key", item.DedupeKey))
		return relayAcksIt
	case followedTopicAttempted:
		return relayHoldsIt
	}
	m.logger.DeliveryLog(slog.LevelWarn, "session relay refused a message the session cannot take", sessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
	if publishErr := publishDeliveryException(m.client, item, "delivery_failed", sessionID, true, ""); publishErr != nil {
		m.logger.Warn("session relay could not publish a delivery exception", slog.String("session_id", sessionID), slog.String("event_id", item.EventID), slog.String("error", publishErr.Error()))
	}
	return relayAcksIt
}

// holdWhilePushedElsewhere judges item for the relay and holds msg while another path's push of it
// is in flight, judging it again every relayHeldRecheck and keeping it in progress meanwhile. It
// returns the verdict the hold ended on, relayStopped when the relay was stopped first.
//
// An attempt record is a push in flight, not a delivery: the path that wrote it is a serving-port
// worker handed the push before the session's entry moved to consumer delivery under it
// (serve_port_workers.go). Acking the message on that record would rest the session's only copy on
// a process-local cache that expires and on a worker that may yet drop or hand back its push, so
// the message is held unacked instead, and judged again:
//   - the worker's 204 records the dedupe key: the session has the message, so it is acked;
//   - the worker drops its push (its session is relayed now) or hands it back, either of which
//     clears the attempt: the relay pushes the message itself;
//   - neither happens and the record expires (attemptCacheTTL, 5 minutes from when it was
//     written): the relay pushes the message itself. A copy that lands from both pushes is one the
//     session drops by its dedupe key, so an expiry race costs a duplicate, never the message.
//
// The hold is therefore bounded by attemptCacheTTL plus one relayHeldRecheck, and it stalls
// nothing beyond the session's own later messages, which wait behind it in stream order as they
// wait behind a head message the port keeps refusing (pushUntilAccepted) -- a wait with no bound at
// all. The message is kept in progress, so the consumer's ack wait (bus.SessionConsumerAckWait)
// never runs out under it; the messages the server delivers behind it are bounded by the
// consumer's MaxAckPending (bus.SessionConsumerMaxAckPending) and are redelivered without limit
// (MaxDeliver -1) until the relay takes each in turn, the redelivered copies of the ones it has
// pushed by then acked and skipped (relayServingPort's pushedThrough).
func (m *sessionRelays) holdWhilePushedElsewhere(ctx context.Context, sessionID string, item contracts.Envelope, msg bus.SessionMessage) relayVerdict {
	var since time.Time
	for {
		verdict := m.followedTopicVerdict(sessionID, item)
		if verdict != relayHoldsIt {
			if !since.IsZero() && verdict == relayPushesIt {
				m.logger.DeliveryLog(slog.LevelInfo, "session relay pushes a message whose other push ended unlanded", sessionID, item.Topic, item.EventID, "retrying",
					slog.String("dedupe_key", item.DedupeKey), slog.String("held", time.Since(since).String()))
			}
			return verdict
		}
		if since.IsZero() {
			since = time.Now()
			m.logger.DeliveryLog(slog.LevelInfo, "session relay holds a message another path is pushing", sessionID, item.Topic, item.EventID, "retrying", slog.String("dedupe_key", item.DedupeKey))
		}
		_ = msg.InProgress()
		if waitUnlessDone(ctx, relayHeldRecheck) != nil {
			return relayStopped
		}
	}
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
