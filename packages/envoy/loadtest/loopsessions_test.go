package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// --- a bus and a listener a test runs a loop session against ----------------------------------

// fakeBus is a sessionBus with no NATS: it keeps every subscription by topic, drops the ones a
// session unsubscribed, and hands a message to the handlers of one topic.
type fakeBus struct {
	mu   sync.Mutex
	subs map[string][]*fakeSub
}

func newFakeBus() *fakeBus { return &fakeBus{subs: map[string][]*fakeSub{}} }

func (b *fakeBus) subscribe(conn int, topic string, h func(busMsg)) (sessionSub, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &fakeSub{take: h}
	b.subs[topic] = append(b.subs[topic], s)
	return s, nil
}

// deliver hands m to every subscription of topic no session has dropped, and answers how many
// took it.
func (b *fakeBus) deliver(topic string, m busMsg) int {
	b.mu.Lock()
	subs := slices.Clone(b.subs[topic])
	b.mu.Unlock()
	taken := 0
	for _, s := range subs {
		if s.live() {
			s.take(m)
			taken++
		}
	}
	return taken
}

// live is every topic that still has a subscription, sorted.
func (b *fakeBus) live() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for topic, subs := range b.subs {
		if slices.ContainsFunc(subs, func(s *fakeSub) bool { return s.live() }) {
			out = append(out, topic)
		}
	}
	slices.Sort(out)
	return out
}

type fakeSub struct {
	take func(busMsg)
	mu   sync.Mutex
	gone bool
}

func (s *fakeSub) unsubscribe() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gone = true
	return nil
}

func (s *fakeSub) live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.gone
}

// fakeMsg is one message of the fake bus: its bytes, the headers a relay frame carries and the
// answers the session gave it.
type fakeMsg struct {
	body    []byte
	headers map[string]string
	mu      sync.Mutex
	replies []string
}

func (m *fakeMsg) data() []byte              { return m.body }
func (m *fakeMsg) header(name string) string { return m.headers[name] }

func (m *fakeMsg) respond(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replies = append(m.replies, string(b))
	return nil
}

func (m *fakeMsg) answers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.replies)
}

// loopFrame is one relay frame of event id on subject, at stream sequence seq.
func loopFrame(subject, seq string, stored time.Time, id int64) *fakeMsg {
	payload, _ := json.Marshal(map[string]any{"id": id, "type": "comment.created"})
	data, _ := json.Marshal(contracts.Envelope{EventID: "e", Topic: subject, Payload: string(payload)})
	return &fakeMsg{body: data, headers: map[string]string{
		relayHeaderSubject:   subject,
		relayHeaderStreamSeq: seq,
		relayHeaderStreamID:  "ST-1",
		relayHeaderStoredAt:  stored.UTC().Format(time.RFC3339Nano),
	}}
}

// listenerCall is one call a loop session made to the fake listener.
type listenerCall struct {
	path string
	body map[string]any
}

// topics is the topics a subscribe or an unsubscribe call named.
func (c listenerCall) topics() []string {
	var out []string
	raw, _ := c.body["topics"].([]any)
	for _, t := range raw {
		out = append(out, t.(string))
	}
	return out
}

// fakeListener answers the loop sessions' calls as the real listener does: a subscribe is
// answered with consumer delivery, an unsubscribe with the topics it removed, and a publish with
// an empty object. It records every call.
type fakeListener struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []listenerCall
	// refuseUnsubscribe answers every unsubscribe 500, as a listener under load can.
	refuseUnsubscribe bool
}

func startFakeListener(t *testing.T) *fakeListener {
	t.Helper()
	fl := &fakeListener{}
	fl.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fl.mu.Lock()
		fl.calls = append(fl.calls, listenerCall{path: r.URL.Path, body: body})
		refuse := fl.refuseUnsubscribe
		fl.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/interests/subscribe":
			_, _ = io.WriteString(w, `{"delivery":{"mode":"consumer","stream_id":"ST-1"}}`)
		case r.URL.Path == "/v1/interests/unsubscribe" && refuse:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"envoy: the message bus did not answer"}`)
		case r.URL.Path == "/v1/interests/unsubscribe":
			removed, _ := json.Marshal(map[string]any{"removed": body["topics"]})
			_, _ = w.Write(removed)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(fl.server.Close)
	return fl
}

// of is every call the sessions made to one path.
func (fl *fakeListener) of(path string) []listenerCall {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	var out []listenerCall
	for _, c := range fl.calls {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

func (fl *fakeListener) refuse() {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.refuseUnsubscribe = true
}

// testLoopSessions is a population of the test seed's proposals against one fake listener, which
// stands for both the units' endpoint and the driver's, and heartbeats fast enough that a session
// of a few hundred milliseconds sends several.
func testLoopSessions(t *testing.T, ls *loopSeed, fl *fakeListener, b sessionBus) *loopSessions {
	t.Helper()
	cfg := runConfig{
		loop: ls, loopListeners: []string{fl.server.URL, fl.server.URL},
		unitStarts: 2, unitLife: 200 * time.Millisecond, driverTopics: 2,
		receiptDeadline: time.Second, rngSeed: 1, ep: endpoints{listenerToken: "listener-token"},
	}
	lp := newLoopSessions(cfg, log.New(io.Discard, "", 0), 7, b, newCollector[receipt](16))
	lp.heartbeat = 10 * time.Millisecond
	return lp
}

// --- the placement ----------------------------------------------------------------------------

// TestUnitPlacerCapsAProposalAndCountsTheSpill: a proposal holds unitCapPerProposal unit sessions
// and no more; a draw past the cap goes to the next proposal by share and is counted, and the
// proposal takes starts again once its sessions' lives have run out. The clock is the test's: the
// placer takes the time each start was planned for.
func TestUnitPlacerCapsAProposalAndCountsTheSpill(t *testing.T) {
	ls := loopTestSeed(t)
	p := newUnitPlacer(ls, unitCapPerProposal)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	const life = 5 * time.Minute
	// 0.1 of the seed's 400 requirement ids lands on proposal 0, which holds 300 of them.
	for i := range unitCapPerProposal {
		at, spilled := p.take(now.Add(time.Duration(i)*time.Second), 0.1, life)
		if at != 0 || spilled {
			t.Fatalf("start %d landed on proposal %d (spilled %v), want proposal 0 under the cap", i, at, spilled)
		}
	}
	at, spilled := p.take(now.Add(31*time.Second), 0.1, life)
	if at != 1 || !spilled {
		t.Fatalf("the start past the cap landed on proposal %d (spilled %v), want the next proposal by share, counted", at, spilled)
	}
	if most, spills := p.mostOnOneProposal(), p.spilled(); most != unitCapPerProposal || spills != 1 {
		t.Fatalf("most on one proposal %d, spilled %d; want %d and 1", most, spills, unitCapPerProposal)
	}
	// Two of the thirty ran out a second past their life, so the proposal takes a start again.
	later := now.Add(life + time.Second)
	if live := p.liveAt(later); live != 29 {
		t.Fatalf("%d sessions live at %s, want 29: two of the thirty-one have run out", live, later)
	}
	if at, spilled := p.take(later, 0.1, life); at != 0 || spilled {
		t.Fatalf("a start after two lives ran out landed on proposal %d (spilled %v), want proposal 0", at, spilled)
	}
	if most := p.mostOnOneProposal(); most != unitCapPerProposal {
		t.Fatalf("most on one proposal %d, want never above the cap of %d", most, unitCapPerProposal)
	}
}

// TestUnitScheduleAndPlacementAreTheSameTwice: one -rng-seed gives one schedule of unit starts and
// one placement of them, and another seed gives another; the cap holds over the whole schedule.
func TestUnitScheduleAndPlacementAreTheSameTwice(t *testing.T) {
	ls := loopTestSeed(t)
	start := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	winStart, winEnd := start.Add(30*time.Second), start.Add(5*time.Minute)
	plan := func(seed uint64) ([]unitStart, int, int) {
		p := newUnitPlacer(ls, unitCapPerProposal)
		var out []unitStart
		planUnitStarts(seed, 2, 5*time.Minute, p, start, winStart, winEnd, func(u unitStart) bool {
			out = append(out, u)
			return true
		})
		return out, p.mostOnOneProposal(), p.spilled()
	}
	first, most, spilled := plan(1)
	again, againMost, againSpilled := plan(1)
	if len(first) == 0 {
		t.Fatal("the schedule started no unit session")
	}
	if !slices.Equal(first, again) || most != againMost || spilled != againSpilled {
		t.Fatal("one -rng-seed gave two schedules or two placements")
	}
	if other, _, _ := plan(2); slices.Equal(first, other) {
		t.Fatal("another -rng-seed gave the same schedule")
	}
	if most > unitCapPerProposal {
		t.Fatalf("a proposal held %d unit sessions, past the cap of %d", most, unitCapPerProposal)
	}
	// The seed holds three proposals of 30 sessions each, and the schedule starts far more than
	// ninety over five minutes, so the cap spills and the placement never leaves the seed.
	if spilled == 0 {
		t.Fatalf("%d starts over three capped proposals spilled nothing", len(first))
	}
	for _, u := range first {
		if u.Proposal < 0 || u.Proposal >= len(ls.Proposals) {
			t.Fatalf("a start landed on proposal %d of %d", u.Proposal, len(ls.Proposals))
		}
	}
}

// --- the driver -------------------------------------------------------------------------------

// TestDriverFollowsItsProposalsThenTheLargestDocuments: the driver follows every proposal the
// loop's driver is live on first, then the others by the size of their part documents, and
// registers those topics and the units' exit topic.
func TestDriverFollowsItsProposalsThenTheLargestDocuments(t *testing.T) {
	ls := loopTestSeed(t)
	// Proposal 0 is the live one; proposal 1 has one part document and proposal 2 none.
	for n, want := range map[int][]int{1: {0}, 2: {0, 1}, 3: {0, 1, 2}, 9: {0, 1, 2}} {
		if got := driverProposals(ls, n); !slices.Equal(got, want) {
			t.Errorf("-driver-topics %d follows proposals %v, want %v", n, got, want)
		}
	}

	fl := startFakeListener(t)
	b := newFakeBus()
	lp := testLoopSessions(t, ls, fl, b)
	ctx, cancel := context.WithCancel(context.Background())
	lp.startDriver(ctx)
	calls := fl.of("/v1/interests/subscribe")
	if len(calls) != 1 {
		t.Fatalf("the driver made %d subscribe calls, want 1", len(calls))
	}
	want := []string{issueTopicOf("LOOP-1"), issueTopicOf("LOOP-2"), unitExitTopic(7)}
	if got := calls[0].topics(); !slices.Equal(got, want) {
		t.Fatalf("the driver registered %v, want %v", got, want)
	}
	if id, _ := calls[0].body["session_id"].(string); id != "ses-lg-driver" {
		t.Fatalf("the driver registered as %q", id)
	}
	if driving, _ := calls[0].body["driving"].(bool); !driving {
		t.Fatalf("the driver registered with driving %v, want true", driving)
	}
	if s := lp.summary(); s.DriverTopics != 3 || !s.DriverSubscribed {
		t.Fatalf("driver topics %d subscribed %v, want 3 and true", s.DriverTopics, s.DriverSubscribed)
	}
	// It lives until the step ends, so it is still subscribed and its window stays open.
	cancel()
	lp.stop()
	windows := lp.windows.snapshot()
	if len(windows) != 1 || windows[0].UnsubAck != 0 || windows[0].SubAck == 0 {
		t.Fatalf("driver window %+v, want one window the step's end left open", windows)
	}
}

// --- a unit session's life ----------------------------------------------------------------------

// TestUnitSessionStartsFollowsOneProposalAndStops: a unit session subscribes its proposal's topic
// and asks its listener for consumer delivery, takes its relay frames as its own receipts,
// heartbeats while it lives, publishes one exit message, unsubscribes the topics it held and
// drops its subscriptions; the step records its start, its subscribe and its unsubscribe.
func TestUnitSessionStartsFollowsOneProposalAndStops(t *testing.T) {
	ls := loopTestSeed(t)
	fl := startFakeListener(t)
	b := newFakeBus()
	lp := testLoopSessions(t, ls, fl, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := lp.unitSession(unitStart{N: 1, At: time.Now(), Proposal: 0})
	done := make(chan struct{})
	go func() {
		defer close(done)
		lp.run(ctx, s, time.Now().Add(200*time.Millisecond))
	}()
	<-s.ready

	issue := issueTopicOf("LOOP-1")
	// Its relay subject, its agent topic and its proposal's topic, which live reports sorted.
	want := []string{contracts.AgentSubject(s.id), issue, bus.DeliverSubject(s.id)}
	if got := b.live(); !slices.Equal(got, want) {
		t.Fatalf("the unit subscribed %v, want %v", got, want)
	}
	first := fl.of("/v1/interests/subscribe")
	if len(first) == 0 {
		t.Fatal("the unit registered nothing")
	}
	if got := first[0].topics(); !slices.Equal(got, []string{issue}) {
		t.Fatalf("the unit registered %v, want its proposal's topic alone", got)
	}
	delivery, _ := first[0].body["delivery"].(map[string]any)
	if consumer, _ := delivery["consumer"].(bool); !consumer {
		t.Fatalf("the unit registered delivery %v, want consumer delivery", first[0].body["delivery"])
	}

	// Its relay frames are its receipts, under its own session id, each stream sequence once; the
	// core copy of a relayed subject is not counted, as in the pi plugin.
	stored := time.Unix(1762160000, 0)
	frame := loopFrame(issue, "7", stored, 10)
	b.deliver(bus.DeliverSubject(s.id), frame)
	b.deliver(bus.DeliverSubject(s.id), loopFrame(issue, "7", stored, 10))
	b.deliver(issue, loopFrame(issue, "8", stored, 11))
	got := lp.receipts.snapshot()
	if len(got) != 1 || got[0].Session != s.id || got[0].EventID != 10 || got[0].Published != stored.UnixNano() {
		t.Fatalf("receipts %+v, want one of event 10 under %s with the stream's store time", got, s.id)
	}
	if answers := frame.answers(); !slices.Equal(answers, []string{string(relayReplyReceived), string(relayReplyDone)}) {
		t.Fatalf("the unit answered its frame %v, want received then done", answers)
	}

	<-done

	// It heartbeated while it lived, then published its exit message and unsubscribed what it
	// held, in that order.
	if beats := len(fl.of("/v1/interests/subscribe")); beats < 2 {
		t.Fatalf("the unit registered %d times in 200 ms at a 10 ms heartbeat, want its first and at least one heartbeat", beats)
	}
	exits := fl.of("/v1/messages/publish")
	if len(exits) != 1 {
		t.Fatalf("the unit published %d exit messages, want 1", len(exits))
	}
	if topic, _ := exits[0].body["topic"].(string); topic != unitExitTopic(7) {
		t.Fatalf("the unit published its exit message on %q, want %q", topic, unitExitTopic(7))
	}
	drops := fl.of("/v1/interests/unsubscribe")
	if len(drops) != 1 || !slices.Equal(drops[0].topics(), []string{issue}) {
		t.Fatalf("the unit unsubscribed %+v, want one call naming %q", drops, issue)
	}
	if live := b.live(); len(live) != 0 {
		t.Fatalf("the unit left %v subscribed", live)
	}

	windows := lp.windows.snapshot()
	if len(windows) != 1 {
		t.Fatalf("%d windows, want the unit's one", len(windows))
	}
	w := windows[0]
	if w.Class != deliveryClassUnit || !slices.Equal(w.Issues, []string{"LOOP-1"}) {
		t.Fatalf("window %+v, want a unit of LOOP-1", w)
	}
	if !(w.SubSent > 0 && w.SubSent <= w.SubAck && w.SubAck < w.UnsubSent && w.UnsubSent <= w.UnsubAck) {
		t.Fatalf("window times %+v, want a subscribe then an unsubscribe, each acknowledged", w)
	}
	sum := lp.summary()
	if sum.Started != 1 || sum.Stopped != 1 || sum.ExitMessages != 1 || sum.UnsubscribedTopics != 1 {
		t.Fatalf("summary %+v, want one session started, stopped, its exit message and its topic removed", sum)
	}
	if sum.StartMs.N != 1 || sum.SubscribeMs.N != 1 || sum.UnsubscribeMs.N != 1 {
		t.Fatalf("start %+v subscribe %+v unsubscribe %+v, want one sample of each", sum.StartMs, sum.SubscribeMs, sum.UnsubscribeMs)
	}
	if sum.UnsubscribeErrors != 0 || sum.StartErrors != 0 || sum.ExitErrors != 0 {
		t.Fatalf("summary %+v, want no error of a session that ran its whole life", sum)
	}
}

// TestAUnitSessionWhoseUnsubscribeIsRefusedIsCounted: a listener that refuses the unsubscribe is
// counted and stops nothing; the session's life ends all the same, since its subscriptions are
// gone and nothing after it could reach it.
func TestAUnitSessionWhoseUnsubscribeIsRefusedIsCounted(t *testing.T) {
	ls := loopTestSeed(t)
	fl := startFakeListener(t)
	fl.refuse()
	b := newFakeBus()
	lp := testLoopSessions(t, ls, fl, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := lp.unitSession(unitStart{N: 2, At: time.Now(), Proposal: 1})
	lp.run(ctx, s, time.Now().Add(20*time.Millisecond))

	sum := lp.summary()
	if sum.UnsubscribeErrors != 1 || sum.Stopped != 1 {
		t.Fatalf("summary %+v, want one refused unsubscribe on a session that stopped all the same", sum)
	}
	if sum.UnsubscribedTopics != 0 || sum.UnsubscribeMs.N != 1 {
		t.Fatalf("removed %d topics over %+v, want none removed and the refusal's round trip", sum.UnsubscribedTopics, sum.UnsubscribeMs)
	}
	windows := lp.windows.snapshot()
	if len(windows) != 1 || windows[0].UnsubAck == 0 {
		t.Fatalf("window %+v, want the refused unsubscribe to end the session's life", windows)
	}
	if live := b.live(); len(live) != 0 {
		t.Fatalf("the unit left %v subscribed", live)
	}
}

// --- the flags --------------------------------------------------------------------------------

// TestLoopSessionFlagsCheck: the flags of a loop-session population are refused before a step
// connects to anything, each refusal naming the flag it is about; a step that names none of them
// runs the flat population and is refused nothing.
func TestLoopSessionFlagsCheck(t *testing.T) {
	good := loopSessionFlags{unitStarts: 2, unitLife: 5 * time.Minute,
		listeners: "http://127.0.0.1:1, http://127.0.0.1:2/", driverTopics: 614, deadline: time.Second}
	eps, err := good.check(true)
	if err != nil {
		t.Fatalf("a whole loop-session population is refused: %v", err)
	}
	if want := []string{"http://127.0.0.1:1", "http://127.0.0.1:2"}; !slices.Equal(eps, want) {
		t.Fatalf("listeners %v, want %v", eps, want)
	}
	if eps, err := (loopSessionFlags{unitLife: 5 * time.Minute, driverTopics: 614, deadline: time.Second}).check(false); err != nil || eps != nil {
		t.Fatalf("a step with no loop session: %v, %v; want no listener and no refusal", eps, err)
	}

	for _, c := range []struct {
		name     string
		flags    loopSessionFlags
		loopSeed bool
		names    string
	}{
		{"a rate below zero", loopSessionFlags{unitStarts: -1}, true, "-unit-starts"},
		{"units without a loop seed", loopSessionFlags{unitStarts: 1, unitLife: time.Minute, driverTopics: 1,
			deadline: time.Second}, false, "-unit-starts"},
		{"listeners without a loop seed", loopSessionFlags{unitLife: time.Minute, driverTopics: 1,
			deadline: time.Second, listeners: "http://127.0.0.1:1,http://127.0.0.1:2"}, false, "-loop-listeners"},
		{"one listener", loopSessionFlags{unitStarts: 1, unitLife: time.Minute, driverTopics: 1,
			deadline: time.Second, listeners: "http://127.0.0.1:1"}, true, "-loop-listeners"},
		{"a listener that is no URL", loopSessionFlags{listeners: "127.0.0.1:1,http://127.0.0.1:2"}, true, "-loop-listeners"},
		{"units with no listener", loopSessionFlags{unitStarts: 1, unitLife: time.Minute, driverTopics: 1,
			deadline: time.Second}, true, "-loop-listeners"},
		{"a life of nothing", loopSessionFlags{unitLife: 0, driverTopics: 1, deadline: time.Second}, true, "-unit-life"},
		{"no driver topic", loopSessionFlags{unitLife: time.Minute, driverTopics: 0, deadline: time.Second}, true, "-driver-topics"},
		{"a deadline of nothing", loopSessionFlags{unitLife: time.Minute, driverTopics: 1}, true, "-receipt-deadline"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.flags.check(c.loopSeed)
			if err == nil {
				t.Fatalf("%+v is not refused", c.flags)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Fatalf("the refusal %q does not name %s", err, c.names)
			}
		})
	}
}
