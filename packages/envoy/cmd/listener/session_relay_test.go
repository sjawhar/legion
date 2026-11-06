package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// relayRig is a listener's /v1 routes over the in-process server, its session consumers relayed as
// main relays them, by a listener holding its machine's durable binding (relays, which a test that
// orders the paths itself hands the binding to when it chooses).
type relayRig struct {
	*sessionConsumerRig
	relays *sessionRelays
	url    string
}

func newRelayRig(t *testing.T) *relayRig {
	t.Helper()
	return newRelayRigReceipt(t, relayPauseFirst, relayPauseMax, relayReceiptWait)
}

// newRelayRigReceipt is newRelayRig with the backoff a paused relay waits before it sends the
// session's unanswered messages again bounded by first and max, and the receipt wait shortened too:
// how long the oldest frame a session has not answered may wait before the relay pauses. A row
// about what an unreachable session leaves behind spends that wait twice over in fixed sleeps, and
// production's three seconds buy it nothing, so such a row names a short one of its own and scales
// its sleeps and its silent windows to it.
func newRelayRigReceipt(t *testing.T, first, max, receipt time.Duration) *relayRig {
	t.Helper()
	rig := newRelayRigUnowned(t, first, max)
	rig.relays.receiptWait = receipt
	rig.relays.setOwned(true)
	return rig
}

// newRelayRigUnowned bounds the same backoff, for a task that does not hold its machine's durable
// binding yet: it relays nothing, and no relay starts, until the test calls
// rig.relays.setOwned(true). A test that must run the durable path before any relay orders them
// this way rather than by timing.
func newRelayRigUnowned(t *testing.T, first, max time.Duration) *relayRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, url)
	relays, err := startSessionRelays(rig.deps.consumers, rig.harness.config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		relays.stop(ctx)
	})
	// Set before the first relay starts, which gaining the binding is what does.
	relays.pauseFirst, relays.pauseMax = first, max
	return &relayRig{sessionConsumerRig: rig, relays: relays, url: url}
}

// consumerRegistration is the registration body of a session in consumer mode at epoch, following
// topics, as the plugin sends it on every heartbeat.
func consumerRegistration(sessionID, epoch string, topics ...string) string {
	body, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"topics":          append([]string{}, topics...),
		"delivery":        map[string]any{"consumer": true, "epoch": epoch},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// storeEvent stores a new envelope on subject and returns it with its stream sequence.
func (rig *relayRig) storeEvent(t *testing.T, subject string) (contracts.Envelope, uint64) {
	t.Helper()
	item := listenerTestEnvelope(subject, "relay-test-"+id.New())
	ack, err := rig.harness.client.JS().Publish(subject, marshalListenerEnvelope(t, item))
	if err != nil {
		t.Fatalf("store a message on %s: %v", subject, err)
	}
	return item, ack.Sequence
}

// relayedFrame is a frame a session stand-in received on its relay subject.
type relayedFrame struct {
	msg     *natsgo.Msg
	item    contracts.Envelope
	seq     uint64
	arrived time.Time
}

// listener is the reply inbox the frame is to be answered on, which names the listener that relayed
// it.
func (f relayedFrame) listener() string {
	return f.msg.Reply[:strings.LastIndex(f.msg.Reply, ".")]
}

// sessionStandIn takes a session's relay frames on a NATS connection of its own, as a plugin in
// consumer mode does, and answers each at once with the replies answer names for it, unless it is
// silent.
type sessionStandIn struct {
	t      *testing.T
	conn   *natsgo.Conn
	frames chan relayedFrame
	silent atomic.Bool
}

// answerDone answers a frame received and done at once: the session wrote it.
func answerDone(relayedFrame) []string { return []string{relayReplyReceived, relayReplyDone} }

func startSessionStandIn(t *testing.T, url, sessionID string, answer func(relayedFrame) []string) *sessionStandIn {
	t.Helper()
	standIn := &sessionStandIn{t: t, conn: testnats.Connect(t, url), frames: make(chan relayedFrame, 256)}
	t.Cleanup(standIn.conn.Close)
	if _, err := standIn.conn.Subscribe(bus.DeliverSubject(sessionID), func(msg *natsgo.Msg) {
		frame := relayedFrame{msg: msg, arrived: time.Now()}
		if err := json.Unmarshal(msg.Data, &frame.item); err != nil {
			t.Errorf("decode a frame for %s: %v", sessionID, err)
			return
		}
		seq, err := strconv.ParseUint(msg.Header.Get(relayHeaderStreamSeq), 10, 64)
		if err != nil {
			t.Errorf("frame for %s carries stream sequence %q: %v", sessionID, msg.Header.Get(relayHeaderStreamSeq), err)
		}
		frame.seq = seq
		if !standIn.silent.Load() && answer != nil {
			for _, reply := range answer(frame) {
				standIn.reply(frame, reply)
			}
		}
		standIn.frames <- frame
	}); err != nil {
		t.Fatalf("subscribe the stand-in of %s to its relay subject: %v", sessionID, err)
	}
	if err := standIn.conn.Flush(); err != nil {
		t.Fatalf("flush the stand-in's subscription: %v", err)
	}
	return standIn
}

func (s *sessionStandIn) reply(frame relayedFrame, word string) {
	if err := s.conn.Publish(frame.msg.Reply, []byte(word)); err != nil {
		s.t.Errorf("reply %s to a frame: %v", word, err)
	}
	if err := s.conn.Flush(); err != nil {
		s.t.Errorf("flush the reply %s: %v", word, err)
	}
}

// collect returns the next n frames, failing the test when they do not all arrive within the bound.
func (s *sessionStandIn) collect(t *testing.T, n int, within time.Duration) []relayedFrame {
	t.Helper()
	deadline := time.After(within)
	frames := make([]relayedFrame, 0, n)
	for len(frames) < n {
		select {
		case frame := <-s.frames:
			frames = append(frames, frame)
		case <-deadline:
			t.Fatalf("the session received %d of %d frames within %s: %v", len(frames), n, within, frameEvents(frames))
		}
	}
	return frames
}

// none fails the test when a frame arrives within the window.
func (s *sessionStandIn) none(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case frame := <-s.frames:
		t.Fatalf("the session received another frame within %s, want none: seq %d event %s", window, frame.seq, frame.item.EventID)
	case <-time.After(window):
	}
}

func frameEvents(frames []relayedFrame) []string {
	events := make([]string, len(frames))
	for i, frame := range frames {
		events[i] = fmt.Sprintf("%d:%s", frame.seq, frame.item.EventID)
	}
	return events
}

// requireFrames fails the test unless frames carry the events want, in that order and in stream
// order.
func requireFrames(t *testing.T, frames []relayedFrame, want ...contracts.Envelope) {
	t.Helper()
	got := make([]string, len(frames))
	for i, frame := range frames {
		got[i] = frame.item.EventID
		if i > 0 && frame.seq <= frames[i-1].seq {
			t.Fatalf("frames out of stream order: %v", frameEvents(frames))
		}
	}
	wantIDs := make([]string, len(want))
	for i, item := range want {
		wantIDs[i] = item.EventID
	}
	if !slices.Equal(got, wantIDs) {
		t.Fatalf("frames carry %v, want %v", frameEvents(frames), wantIDs)
	}
}

// A session away during publishes (its connection gone) receives each on its return, when it
// registers with a new epoch, once and in stream order, from its consumer: it asks nothing of
// /v1/replay.
func TestSessionConsumerAwaySessionReceivesEachMessageOnceOnReturn(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_away"
	agent := contracts.AgentSubject(sessionID)
	first := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	before, beforeSeq := rig.storeEvent(t, agent)
	requireFrames(t, first.collect(t, 1, 5*time.Second), before)
	awaitAcked(t, rig.consumers, sessionID, beforeSeq)

	first.conn.Close()
	var away []contracts.Envelope
	for range 3 {
		item, _ := rig.storeEvent(t, agent)
		away = append(away, item)
	}
	// The relay sends what it pulls at once, to nobody now.
	time.Sleep(300 * time.Millisecond)

	second := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	requireFrames(t, second.collect(t, 3, 5*time.Second), away...)
	second.none(t, time.Second)
}

// A frame is acked only once the session answers done: after received alone the consumer still
// holds the message for an ack, and after done it holds nothing. The frame carries the contract's
// headers.
func TestSessionConsumerAcksAFrameOnlyOnDone(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_ack_on_done"
	standIn := startSessionStandIn(t, rig.url, sessionID, nil)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	stored, seq := rig.storeEvent(t, contracts.AgentSubject(sessionID))

	frame := standIn.collect(t, 1, 5*time.Second)[0]
	requireFrames(t, []relayedFrame{frame}, stored)
	header := frame.msg.Header
	if got := header.Get(relayHeaderSubject); got != contracts.AgentSubject(sessionID) {
		t.Errorf("%s = %q, want the stored subject", relayHeaderSubject, got)
	}
	if got := header.Get(relayHeaderStreamID); got != rig.streamID(t) {
		t.Errorf("%s = %q, want the stream's identity %q", relayHeaderStreamID, got, rig.streamID(t))
	}
	if got := header.Get(relayHeaderDelivered); got != "1" {
		t.Errorf("%s = %q, want 1", relayHeaderDelivered, got)
	}
	storedAt, err := time.Parse(time.RFC3339Nano, header.Get(relayHeaderStoredAt))
	if err != nil || !strings.HasSuffix(header.Get(relayHeaderStoredAt), "Z") || time.Since(storedAt) > time.Minute {
		t.Errorf("%s = %q, want the stored time in RFC 3339 with nanoseconds, UTC: %v", relayHeaderStoredAt, header.Get(relayHeaderStoredAt), err)
	}
	if got := header.Get(relayHeaderGap); got != "" {
		t.Errorf("%s = %q on a restart that lost nothing", relayHeaderGap, got)
	}

	standIn.reply(frame, relayReplyReceived)
	time.Sleep(300 * time.Millisecond)
	info, err := rig.consumers.Info(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the consumer: %v", err)
	}
	if info.NumAckPending != 1 || info.AckFloor.Stream >= seq {
		t.Fatalf("after received alone the consumer has %d pending ack, ack floor %d; want the message (seq %d) pending", info.NumAckPending, info.AckFloor.Stream, seq)
	}

	standIn.reply(frame, relayReplyDone)
	awaitAcked(t, rig.consumers, sessionID, seq)
}

// awaitAcked waits up to 5 s for sessionID's consumer to hold nothing for an ack with every message
// through seq acked.
func awaitAcked(t *testing.T, consumers *bus.SessionConsumers, sessionID string, seq uint64) {
	t.Helper()
	awaitAckState(t, consumers, sessionID, 0, seq)
}

// awaitAckState waits up to 5 s for sessionID's consumer to hold pending messages for an ack with
// every message through floor acked.
func awaitAckState(t *testing.T, consumers *bus.SessionConsumers, sessionID string, pending int, floor uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := consumers.Info(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("read the consumer of %s: %v", sessionID, err)
		}
		if info.NumAckPending == pending && info.AckFloor.Stream >= floor {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the consumer of %s has %d pending ack, ack floor %d; want %d pending and everything through %d acked", sessionID, info.NumAckPending, info.AckFloor.Stream, pending, floor)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A frame the session holds unwritten (received and progress, no done, as a side question held
// over a turn) does not delay the receipt of the frames after it, which are written and done. When
// the session restarts mid-hold and registers with a new epoch, the relay restarts at the ack floor
// and sends the held frame and those after it again, in stream order, and the session, deduping by
// event id as a plugin's ledger does, shows each event once.
func TestSessionConsumerHeldFrameDoesNotDelayLaterReceiptsAndSurvivesARestart(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_held"
	agent := contracts.AgentSubject(sessionID)
	var ledgerMu sync.Mutex
	var shown []string
	ledger := map[string]bool{}
	held := ""
	show := func(frame relayedFrame) []string {
		ledgerMu.Lock()
		defer ledgerMu.Unlock()
		if frame.item.EventID == held {
			return []string{relayReplyReceived, relayReplyProgress}
		}
		if !ledger[frame.item.EventID] {
			ledger[frame.item.EventID] = true
			shown = append(shown, frame.item.EventID)
		}
		return []string{relayReplyReceived, relayReplyDone}
	}
	first := startSessionStandIn(t, rig.url, sessionID, show)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	e0, e0Seq := rig.storeEvent(t, agent)
	requireFrames(t, first.collect(t, 1, 5*time.Second), e0)

	e1 := listenerTestEnvelope(agent, "relay-test-held")
	ledgerMu.Lock()
	held = e1.EventID
	ledgerMu.Unlock()
	if _, err := rig.harness.client.JS().Publish(agent, marshalListenerEnvelope(t, e1)); err != nil {
		t.Fatalf("store the held message: %v", err)
	}
	storedAt := time.Now()
	e2, _ := rig.storeEvent(t, agent)
	e3, _ := rig.storeEvent(t, agent)
	frames := first.collect(t, 3, 5*time.Second)
	requireFrames(t, frames, e1, e2, e3)
	if late := frames[2].arrived.Sub(storedAt); late > time.Second {
		t.Fatalf("the frame after the held one arrived %s after it was stored, want under 1s", late)
	}
	// Everything but the held frame is written and acked; the held one waits, in progress.
	awaitAckState(t, rig.consumers, sessionID, 1, e0Seq)

	first.conn.Close()
	ledgerMu.Lock()
	held = ""
	ledgerMu.Unlock()
	second := startSessionStandIn(t, rig.url, sessionID, show)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	requireFrames(t, second.collect(t, 3, 5*time.Second), e1, e2, e3)
	second.none(t, time.Second)

	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	want := []string{e0.EventID, e2.EventID, e3.EventID, e1.EventID}
	if !slices.Equal(shown, want) {
		t.Fatalf("the session showed %v, want each event once: %v", shown, want)
	}
}

// A registration's new epoch that the relay loop hears only after a run that restarted after the
// registration took its mark restarts nothing: that run read the session's topics and consumer
// after the plugin subscribed, so every frame it sent went to the connection the registration
// names. A run the registration's own entry write starts is such a run, and the loop hears the
// registration after that run restarted whenever it takes the write's change alone (on CI
// TestSessionConsumerAcksAFrameOnlyOnDone failed so: the second restart sent the message again and
// forgot the frame its done answered). A second restart would send the session every message it
// has not acked again and drop its answers to the frames it holds. A registration with a new epoch
// after the run's restart still restarts it at the ack floor.
func TestSessionConsumerNewEpochHeardAfterTheRunRestartedRestartsNothing(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_epoch_heard_late"
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, func(relayedFrame) []string { return []string{relayReplyReceived} })
	// A mark taken before the session's relay starts, as a registration takes its own before its
	// entry write starts the relay.
	heard := rig.deps.consumers.mark()
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	held, heldSeq := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), held)

	// The run has restarted and sent a frame; the loop hears the registration only now.
	rig.relays.registered(sessionID, true, heard)
	standIn.none(t, time.Second)

	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	again := standIn.collect(t, 1, 5*time.Second)[0]
	requireFrames(t, []relayedFrame{again}, held)
	standIn.reply(again, relayReplyDone)
	awaitAcked(t, rig.consumers, sessionID, heldSeq)
	standIn.none(t, time.Second)
}

// A relay restart sets the consumer's filter to the session's stored topics as they stand under
// the session's consumer lock, not as it read them before it took the lock: a registration that
// stored a topic and wrote its filter while the restart waited keeps that topic, and a message on
// it reaches the session.
func TestSessionConsumerRestartWritesTheTopicsStoredWhenItHoldsTheLock(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_restart_stale"
	const followed = "notifications.dispatch.issue.RESTARTSTALE.>"
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	before, beforeSeq := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), before)
	awaitAcked(t, rig.consumers, sessionID, beforeSeq)

	unlock := rig.deps.consumers.locks.lock(sessionID)
	// A new epoch registered after the run's restart replaces the run, whose restart waits for
	// the lock.
	rig.relays.registered(sessionID, true, rig.deps.consumers.mark())
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 2)
	followWhileLocked(t, rig.sessionConsumerRig, sessionID, followed)
	unlock()
	// The restart has written its filter.
	awaitLockUsers(t, &rig.deps.consumers.locks, sessionID, 0)

	item, _ := rig.storeEvent(t, "notifications.dispatch.issue.RESTARTSTALE.comment")
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), item)
	requireFilters(t, rig.sessionConsumerRig, sessionID, followed)
}

// A registration the loop hears while a run returns on its own is never lost: a wake sent before the
// run's exit is taken by the exit, which asks the loop for a run in its place, and one sent after it
// is refused, so the loop starts that run itself (sessionRelays.apply). A run that decided to return
// (its entry lapsed while it waited to retry) before the registration renewed the entry would
// otherwise end with the registration's wake unread, and nothing would relay the session until its
// next registration.
func TestRelayRunExitTakesOrRefusesEveryWake(t *testing.T) {
	wokenFirst := &relayRun{resume: make(chan struct{}, 1)}
	if !wokenFirst.wake() {
		t.Fatal("a run that is not returning refused a wake")
	}
	if !wokenFirst.exit() {
		t.Fatal("the run's exit missed the wake sent before it")
	}

	exitedFirst := &relayRun{resume: make(chan struct{}, 1)}
	if exitedFirst.exit() {
		t.Fatal("the run's exit found a wake nobody sent")
	}
	if exitedFirst.wake() {
		t.Fatal("a returning run took a wake, which nothing would act on")
	}
}

// A run waiting to retry after a failed call or pull restarts at once when the session registers,
// a registration heard while the failed call ran included, rather than waiting its backoff out.
func TestRelayRunRetryWaitEndsOnAWake(t *testing.T) {
	run := &relayRun{resume: make(chan struct{}, 1)}
	run.wake()
	goesOn := make(chan bool, 1)
	go func() { goesOn <- run.waitRetry(context.Background(), time.Hour) }()
	select {
	case ok := <-goesOn:
		if !ok {
			t.Fatal("the retry wait reported the run stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the retry wait did not end on the wake sent before it")
	}
}

// A session that stops answering is taken for unreachable: within the receipt wait of the oldest
// frame it has not answered the relay sends nothing more, and it logs that once. It resumes on its
// own, with no registration: after its pause backoff it restarts at the ack floor and sends the
// unanswered frame and the one stored during the pause, in stream order, which a session that is
// answering again takes once.
func TestSessionConsumerUnreachableSessionPausesAndSendsAgainAfterItsBackoff(t *testing.T) {
	// Shorter than the production bounds, long enough that the silent window below ends before
	// the first probe: the relay sends again receipt + pauseFirst after the frame it got no
	// answer for. The receipt wait is this row's own rather than production's three seconds,
	// which it would otherwise spend twice over in fixed sleeps (INFRA-985).
	const receipt = 500 * time.Millisecond
	const pauseFirst, pauseMax = 2 * time.Second, 4 * time.Second
	rig := newRelayRigReceipt(t, pauseFirst, pauseMax, receipt)
	const sessionID = "ses_unreachable"
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	e0, _ := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), e0)

	standIn.silent.Store(true)
	e1, _ := rig.storeEvent(t, agent)
	unanswered := standIn.collect(t, 1, 5*time.Second)[0]
	requireFrames(t, []relayedFrame{unanswered}, e1)
	// The relay stops receipt after that frame: a message stored once it has reaches the session
	// only when the relay sends again, which is pauseFirst later.
	time.Sleep(time.Until(unanswered.arrived.Add(receipt + 300*time.Millisecond)))
	e2, _ := rig.storeEvent(t, agent)
	standIn.none(t, 900*time.Millisecond)

	standIn.silent.Store(false)
	requireFrames(t, standIn.collect(t, 2, receipt+2*pauseFirst+2*time.Second), e1, e2)
	standIn.none(t, time.Second)
	if lines := strings.Count(rig.harness.logs.String(), "session relay paused: the session answered no frame"); lines != 1 {
		t.Fatalf("the pause was logged %d times, want once for the episode:\n%s", lines, rig.harness.logs.String())
	}
}

// A registration resumes a paused relay at once, long before its backoff would send again.
func TestSessionConsumerUnreachableSessionResumesAtOnceWhenItRegisters(t *testing.T) {
	// A backoff no test waits out: only the registration can make the relay send again here.
	// The receipt wait is this row's own, as in the row above.
	const receipt = 500 * time.Millisecond
	rig := newRelayRigReceipt(t, time.Minute, time.Minute, receipt)
	const sessionID = "ses_unreachable_registers"
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	e0, _ := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), e0)

	standIn.silent.Store(true)
	e1, _ := rig.storeEvent(t, agent)
	unanswered := standIn.collect(t, 1, 5*time.Second)[0]
	requireFrames(t, []relayedFrame{unanswered}, e1)
	time.Sleep(time.Until(unanswered.arrived.Add(receipt + 500*time.Millisecond)))
	e2, _ := rig.storeEvent(t, agent)
	standIn.none(t, 1500*time.Millisecond)
	if lines := strings.Count(rig.harness.logs.String(), "session relay paused: the session answered no frame"); lines != 1 {
		t.Fatalf("the pause was logged %d times, want once for the episode:\n%s", lines, rig.harness.logs.String())
	}

	standIn.silent.Store(false)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	requireFrames(t, standIn.collect(t, 2, 5*time.Second), e1, e2)
	standIn.none(t, time.Second)
}

// A topic the session follows from a registration on reaches it through its consumer: an event
// published 100 ms after the registration that added it answered is relayed.
func TestSessionConsumerEventPublishedJustAfterAFollowIsRelayed(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_follow"
	const topic = "notifications.dispatch.issue.FOLLOW.>"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	before, _ := rig.storeEvent(t, contracts.AgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), before)

	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", topic))
	time.Sleep(100 * time.Millisecond)
	followed, _ := rig.storeEvent(t, "notifications.dispatch.issue.FOLLOW.comment")
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), followed)
}

// A relay frame travels over core NATS only: after frames are relayed, the stream holds nothing on
// the relay subject.
func TestSessionConsumerRelaySubjectIsNotStored(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_not_stored"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	first, _ := rig.storeEvent(t, contracts.AgentSubject(sessionID))
	second, _ := rig.storeEvent(t, contracts.AgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 2, 5*time.Second), first, second)

	info, err := rig.harness.client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: bus.DeliverSubjectPrefix + ">"})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if len(info.State.Subjects) != 0 {
		t.Fatalf("the stream holds messages on relay subjects: %v", info.State.Subjects)
	}
	if _, err := rig.harness.client.JS().GetLastMsg(bus.Stream, bus.DeliverSubject(sessionID)); !errors.Is(err, natsgo.ErrMsgNotFound) {
		t.Fatalf("the stream's last message on %s: %v, want none", bus.DeliverSubject(sessionID), err)
	}
}

// A listener killed while it relays a session (SIGKILL, no shutdown) relays the frames stored while
// it was down within 10 s of its restart, from its watched session cache, with no new registration.
func TestSessionConsumerFramesStoredWhileTheOwnerWasDownArriveAfterItsRestart(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	const machineID = "relay-owner"
	listener := startListenerProcess(t, binary, uri, machineID)
	listener.waitHealthy(t)
	sessionID := "ses_owner_killed_" + id.New()
	agent := contracts.AgentSubject(sessionID)
	midFlight := newEnvelope("agent", agent, "in flight when the listener died")
	var midFlightSeen atomic.Bool
	standIn := startSessionStandIn(t, uri, sessionID, func(frame relayedFrame) []string {
		if frame.item.EventID == midFlight.EventID && !midFlightSeen.Swap(true) {
			// The session is still writing it when the listener dies; its done reaches nobody.
			return []string{relayReplyReceived}
		}
		return answerDone(frame)
	})
	consumers := observeConsumers(t, uri)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
	publisher := connectPublisher(t, uri)
	before := newEnvelope("agent", agent, "before the kill")
	publish(t, publisher, before)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), before)
	info, err := consumers.Info(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the session's consumer: %v", err)
	}
	awaitAcked(t, consumers, sessionID, info.Delivered.Stream)
	publish(t, publisher, midFlight)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), midFlight)

	if err := listener.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the listener: %v", err)
	}
	<-listener.Exited
	var down []contracts.Envelope
	for i := range 3 {
		item := newEnvelope("agent", agent, fmt.Sprintf("while the listener was down %d", i))
		publish(t, publisher, item)
		down = append(down, item)
	}

	restarted := startListenerProcess(t, binary, uri, machineID)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the restarted listener's output:\n%s", restarted.Output.String())
		}
	})
	startedAt := time.Now()
	frames := standIn.collect(t, 4, 10*time.Second)
	requireFrames(t, frames, append([]contracts.Envelope{midFlight}, down...)...)
	t.Logf("the frames stored while the listener was down arrived %s after its restart", frames[3].arrived.Sub(startedAt).Round(time.Millisecond))
	standIn.none(t, time.Second)
	awaitAcked(t, consumers, sessionID, frames[3].seq)
}

// A session that re-registers through another listener moves to it: the first stops relaying it at
// once and the second relays it, so each frame comes from exactly one listener, the one the session
// last registered through.
func TestSessionConsumerOwnershipMovesWithTheSessionToOneRelayer(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	first := startListenerProcess(t, binary, uri, "relay-first")
	second := startListenerProcess(t, binary, uri, "relay-second")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the first listener's output:\n%s\nthe second listener's output:\n%s", first.Output.String(), second.Output.String())
		}
	})
	first.waitHealthy(t)
	second.waitHealthy(t)
	sessionID := "ses_moved_relay_" + id.New()
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, uri, sessionID, answerDone)
	consumers := observeConsumers(t, uri)
	publisher := connectPublisher(t, uri)
	publishEach := func(label string) []contracts.Envelope {
		var items []contracts.Envelope
		for i := range 3 {
			item := newEnvelope("agent", agent, fmt.Sprintf("%s %d", label, i))
			publish(t, publisher, item)
			items = append(items, item)
		}
		return items
	}

	postListener(t, first.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
	before := publishEach("through the first")
	beforeFrames := standIn.collect(t, 3, 10*time.Second)
	requireFrames(t, beforeFrames, before...)
	// The session wrote all three, so the second listener's restart at the ack floor sends none
	// again.
	awaitAcked(t, consumers, sessionID, beforeFrames[2].seq)

	postListener(t, second.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
	first.awaitSessionMachine(t, sessionID, "relay-second")
	after := publishEach("through the second")
	afterFrames := standIn.collect(t, 3, 10*time.Second)
	requireFrames(t, afterFrames, after...)
	standIn.none(t, 2*time.Second)

	byListener := map[string][]string{}
	for _, frame := range append(beforeFrames, afterFrames...) {
		byListener[frame.listener()] = append(byListener[frame.listener()], frame.item.PayloadSummary)
	}
	firstInbox, secondInbox := beforeFrames[0].listener(), afterFrames[0].listener()
	if firstInbox == secondInbox || len(byListener) != 2 || len(byListener[firstInbox]) != 3 || len(byListener[secondInbox]) != 3 {
		t.Fatalf("frames per relaying listener %v, want three from the first and the next three from the second", byListener)
	}
}

// observeConsumers opens the test's own view of the session consumers on uri.
func observeConsumers(t *testing.T, uri string) *bus.SessionConsumers {
	t.Helper()
	consumers, err := bus.ConnectSessionConsumers([]string{uri})
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	return consumers
}

// bindTestDurable binds a durable of this test's own, filtered on subject, to the listener's
// durable handler: the package's NATS server is shared, so a test binds the one subject whose
// messages its durable path must read.
func bindTestDurable(t *testing.T, rig *relayRig, name, subject string) {
	t.Helper()
	config := natsgo.ConsumerConfig{
		Durable:        name,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  subject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        consumerAckWait,
		MaxAckPending:  consumerMaxAckPending,
		MaxDeliver:     consumerMaxDeliver,
	}
	if _, err := rig.harness.client.JS().AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("create the listener's durable: %v", err)
	}
	subscription, err := rig.harness.client.Subscribe(subject, jetStreamDeliveryHandler(rig.harness.config), natsgo.Bind(bus.Stream, name), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the listener's durable: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
}

// keepTheDurableReading gives topic a second recipient whose interest outlives its session entry,
// and returns its session id. A session whose own consumer holds a stored message takes it from
// there, and one that is self-subscribed takes it off its own subscription, so the durable path
// acknowledges such a message before it decodes it (durableNeedsDelivery,
// takesStoredMessagesItself) and the fan-out never judges it. An interest the fan-out can match
// whose entry it cannot read is the ordinary state of a session whose entry has lapsed -- an
// entry lives about five minutes (session.registry) and an interest about fifteen -- and it is
// what keeps the durable path reading a topic for a test whose subject is what the fan-out does
// with the recipient beside it.
func keepTheDurableReading(t *testing.T, rig *relayRig, topic string) string {
	t.Helper()
	sessionID := "ses_durable_reader_" + id.New()
	if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{topic}); err != nil {
		t.Fatalf("register the interest of %s: %v", sessionID, err)
	}
	return sessionID
}

// awaitFanoutFinished waits for the durable the test bound to have acknowledged the message
// stored at seq with nothing left pending, which the fan-out leaves it once it has decided every
// interest of that message (fanoutDelivery). A test that then starts a relay orders the two
// paths rather than racing them.
func awaitFanoutFinished(t *testing.T, rig *relayRig, durable string, seq uint64) {
	t.Helper()
	awaitDurableState(t, rig.harness.client, durable, 0, seq)
}

// deliveryExceptionsFor reads the delivery exceptions for sessionID that arrive on exceptions
// within window, each as its reason, and ignores those for any other session.
func deliveryExceptionsFor(t *testing.T, exceptions *natsgo.Subscription, sessionID string, window time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(window)
	var reasons []string
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return reasons
		}
		msg, err := exceptions.NextMsg(remaining)
		if errors.Is(err, natsgo.ErrTimeout) {
			return reasons
		}
		if err != nil {
			t.Fatalf("read the exception lane of %s: %v", sessionID, err)
		}
		var envelope contracts.Envelope
		if err := json.Unmarshal(msg.Data, &envelope); err != nil {
			t.Fatalf("decode the exception on %s: %v", msg.Subject, err)
		}
		var payload struct {
			Reason           string `json:"reason"`
			RecipientSession string `json:"recipient_session"`
		}
		if err := json.Unmarshal([]byte(envelope.Payload), &payload); err != nil {
			t.Fatalf("decode the exception payload on %s: %v", msg.Subject, err)
		}
		if payload.RecipientSession == sessionID {
			reasons = append(reasons, payload.Reason)
		}
	}
}

// awaitLogLine waits for line to appear in the listener's log, failing the test when it does not
// within the bound.
func awaitLogLine(t *testing.T, rig *relayRig, line string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(rig.harness.logs.String(), line) {
		if time.Now().After(deadline) {
			t.Fatalf("the listener never logged %q within %s", line, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// grantConfigBeforeSessionConsumers is the configuration of a server whose one user, the nkey user
// public, may publish only on the listener grant a hub carries before session consumers.
func grantConfigBeforeSessionConsumers(public string) string {
	quoted := make([]string, 0, len(listenerGrantBeforeSessionConsumers()))
	for _, subject := range listenerGrantBeforeSessionConsumers() {
		quoted = append(quoted, strconv.Quote(subject))
	}
	return fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ { nkey: %q, permissions: { publish: { allow: [%s] } } } ]\n}\n",
		public, strings.Join(quoted, ", "))
}

// On a hub whose grant predates session consumers, the first refused consumer call holds every
// registration on core for ten minutes without another call: the server sees one consumer call
// refused however many sessions register in that time, and one more after it, at the next
// registration, which probes the grant again. The refusal is logged once.
func TestSessionConsumerWithoutTheGrantProbesItOncePerTenMinutes(t *testing.T) {
	seed, public := testnats.User(t)
	server, uri := testnats.StartConfig(t, grantConfigBeforeSessionConsumers(public), []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, uri)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	rig.deps.consumers.now = func() time.Time { return time.Unix(0, clock.Load()) }
	refusedCalls := func() int {
		calls := 0
		for line := range strings.SplitSeq(server.Log(), "\n") {
			if strings.Contains(line, "Publish Violation") && strings.Contains(line, "$JS.API.CONSUMER.") {
				calls++
			}
		}
		return calls
	}
	register := func(i int) {
		answer := rig.subscribe(t, consumerRegistration(fmt.Sprintf("ses_no_grant_%d", i), "e"))
		if answer.Delivery == nil || answer.Delivery.Mode != "core" || answer.Delivery.Reason != "no_grant" {
			t.Fatalf("answer %s, want delivery core with reason no_grant", answer.raw)
		}
	}
	awaitRefusedCalls := func(want int) {
		deadline := time.Now().Add(2 * time.Second)
		for refusedCalls() < want && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		// Any further refusal would be logged by now too.
		time.Sleep(200 * time.Millisecond)
		if got := refusedCalls(); got != want {
			t.Fatalf("the server refused %d consumer calls, want %d:\n%s", got, want, server.Log())
		}
	}

	for i := range 5 {
		register(i)
		clock.Add(int64(time.Minute))
	}
	awaitRefusedCalls(1)

	clock.Add(int64(6 * time.Minute))
	register(5)
	register(6)
	awaitRefusedCalls(2)
	if lines := strings.Count(rig.harness.logs.String(), "sessions stay on core delivery until the grant carries them"); lines != 1 {
		t.Fatalf("the missing grant was logged %d times, want once:\n%s", lines, rig.harness.logs.String())
	}
}
