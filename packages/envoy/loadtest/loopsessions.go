package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The Verified Linux spec loop's own sessions (-unit-starts), beside the step's flat -sessions
// population (sessions.go), which a step without the flag still runs alone.
//
// A unit session is what the loop's spec work is made of: it starts, registers on its listener
// with consumer delivery, follows exactly one proposal's issue topic, heartbeats while it lives,
// lives -unit-life, publishes one exit message on a topic of the run's own, unsubscribes its
// topics and stops heartbeating. There is no deregistration route: the listener's registry drops
// a session 300 s after its last write, so a session stops by unsubscribing and going quiet
// (packages/envoy/cmd/listener/api.go, POST /v1/interests/unsubscribe).
//
// Beside them one driver session follows -driver-topics issue topics and the units' exit topic
// for the whole step, as the loop's own driver does.
//
// What each class was owed and what it got is counted by the delivery join, under the rule a
// session's own subscribe and unsubscribe acknowledgements give (delivery.go, sessionWindow):
// this file records those four times for every session it runs.

// unitCapPerProposal is how many unit sessions one proposal holds at once. A draw past it goes to
// the next proposal by share, and the spill is counted (unitPlacer.take).
const unitCapPerProposal = 30

// unitStartStream is the PRNG stream the unit sessions' start schedule is drawn from: stream 1 is
// the event generator's and stream 2 the request generator's (runEvents, runRequests), and a new
// generator takes the next free number.
const unitStartStream = 3

// loopHeartbeat is how often a loop session re-registers, the heartbeat of the pi plugin
// (packages/envoy-client/src/envoy-settings.ts).
const loopHeartbeat = 90 * time.Second

// loopCallTimeout is how long a loop session waits for one listener call, the timeout the flat
// population's registrations take (registerSessions).
const loopCallTimeout = 10 * time.Second

// loopLogLimit is how many errors of one kind the loop sessions log: a step of hundreds of
// sessions would otherwise fill run.log with one listener's refusals. The summary counts them all.
const loopLogLimit = 5

// The session classes the delivery join counts apart (eventsSummary.ByClass).
const (
	deliveryClassUnit   = "unit"
	deliveryClassDriver = "driver"
)

// unitExitTopic is the topic every unit session publishes its one exit message on and the driver
// session follows: an ad-hoc topic of this run's own, which no stream subject captures, so the
// listener publishes it over core NATS and confirms it with each self-subscribed session
// registered on it (packages/envoy/AGENTS.md, POST /v1/messages/publish).
func unitExitTopic(nonce int64) string {
	return "notifications.loadtest." + formatNonce(nonce) + ".unit-exit"
}

// --- the flags -------------------------------------------------------------------------------

// loopSessionFlags are run's loop-session flags as given, before they are checked.
type loopSessionFlags struct {
	unitStarts   float64
	unitLife     time.Duration
	listeners    string
	driverTopics int
	deadline     time.Duration
}

// check refuses what no loop-session population can run and answers the listener endpoints
// -loop-listeners named, nil for a step without the flag. loopSeed says whether the step has a
// -loop-seed: without one there are no proposals for a unit session to follow, so both flags are
// refused.
func (f loopSessionFlags) check(loopSeed bool) ([]string, error) {
	eps, err := parseLoopListeners(f.listeners)
	if err != nil {
		return nil, err
	}
	switch {
	case f.unitStarts < 0 || !finite(f.unitStarts):
		return nil, fmt.Errorf("-unit-starts is the rate the unit sessions start at and must be 0 or more")
	case f.listeners != "" && !loopSeed:
		return nil, fmt.Errorf("-loop-listeners applies only with -loop-seed: the loop's sessions follow its proposals")
	case f.unitStarts > 0 && !loopSeed:
		return nil, fmt.Errorf("-unit-starts applies only with -loop-seed: a unit session follows one of its proposals")
	case f.unitLife <= 0:
		return nil, fmt.Errorf("-unit-life is how long a unit session lives and must be more than 0")
	case f.driverTopics < 1:
		return nil, fmt.Errorf("-driver-topics is how many issue topics the driver session follows and must be at least 1")
	case f.deadline <= 0:
		return nil, fmt.Errorf("-receipt-deadline is the deadline a receipt is expected by and must be more than 0")
	case f.unitStarts > 0 && len(eps) == 0:
		return nil, fmt.Errorf("-unit-starts needs -loop-listeners: the unit sessions and the driver session register on them")
	}
	return eps, nil
}

// parseLoopListeners reads -loop-listeners: the listener endpoints, comma separated, the unit
// sessions over all but the last and the driver session on the last. Fewer than two is refused,
// since then one endpoint would carry both.
func parseLoopListeners(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []string
	for _, ep := range strings.Split(v, ",") {
		ep = strings.TrimSpace(ep)
		if !isEndpointURL("-loop-listeners", ep) {
			return nil, fmt.Errorf("-loop-listeners %q is not a URL with a scheme and a host", ep)
		}
		out = append(out, strings.TrimSuffix(ep, "/"))
	}
	if len(out) < 2 {
		return nil, fmt.Errorf("-loop-listeners needs at least two endpoints: the unit sessions take all but the last, and the driver session the last")
	}
	return out, nil
}

// --- the placement ---------------------------------------------------------------------------

// unitEnd is one live unit session's end: when its life runs out and the proposal it holds until
// then.
type unitEnd struct {
	at       time.Time
	proposal int
}

// unitPlacer places unit sessions on the loop's proposals. A start lands on a proposal drawn in
// proportion to its requirement ids, as a read or a write does (loopSeed.pick); when that
// proposal already holds unitCapPerProposal live sessions the start goes to the next proposal by
// share that has room, and the spill is counted. It keeps no clock: every call carries the time
// the schedule planned, and a session is released when its planned life has run out, so one
// -rng-seed gives one placement whatever the machine does.
type unitPlacer struct {
	ls  *loopSeed
	cap int
	// order is the proposals of the weighted draw in its own order, and pos where each of them
	// stands in it: a spilled start walks order from the proposal it drew.
	order []int
	pos   map[int]int

	mu sync.Mutex
	// live counts the live sessions of each proposal; ends holds every session's end in end
	// order, at the first that has not passed. Every session lives the same -unit-life and the
	// starts are planned in order, so their ends are in order too and releasing them is a walk.
	live  []int
	ends  []unitEnd
	at    int
	total int
	most  int
	spill int
}

func newUnitPlacer(ls *loopSeed, capPerProposal int) *unitPlacer {
	p := &unitPlacer{ls: ls, cap: capPerProposal, order: ls.picks[loopNeedAny].at,
		pos: make(map[int]int, len(ls.Proposals)), live: make([]int, len(ls.Proposals))}
	for i, at := range p.order {
		p.pos[at] = i
	}
	return p
}

// take places a unit session that starts at now and lives for life on the proposal x in [0, 1)
// draws, after releasing every session whose life has run out. It answers the proposal and
// whether the draw spilled, and -1 for a draw no proposal had room for, whose start is dropped.
func (p *unitPlacer) take(now time.Time, x float64, life time.Duration) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.release(now)
	drawn := p.ls.pick(loopNeedAny, x)
	spilled := p.live[drawn] >= p.cap
	if spilled {
		p.spill++
	}
	from := p.pos[drawn]
	for i := range p.order {
		at := p.order[(from+i)%len(p.order)]
		if p.live[at] >= p.cap {
			continue
		}
		p.live[at]++
		p.total++
		p.most = max(p.most, p.live[at])
		p.ends = append(p.ends, unitEnd{at: now.Add(life), proposal: at})
		return at, spilled
	}
	return -1, spilled
}

// release gives back every session whose life ended at or before now. Under mu.
func (p *unitPlacer) release(now time.Time) {
	for p.at < len(p.ends) && !p.ends[p.at].at.After(now) {
		p.live[p.ends[p.at].proposal]--
		p.total--
		p.at++
	}
}

// liveAt is how many unit sessions are live at now. It releases nothing, so the sampler that
// reads it over the step changes nothing the schedule's own placement depends on.
func (p *unitPlacer) liveAt(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.total
	for i := p.at; i < len(p.ends) && !p.ends[i].at.After(now); i++ {
		n--
	}
	return n
}

// mostOnOneProposal is the most live unit sessions one proposal held, and spilled how many starts
// drew a proposal that was at the cap.
func (p *unitPlacer) mostOnOneProposal() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.most
}

func (p *unitPlacer) spilled() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spill
}

// unitStart is one unit session the schedule plans: its number in the step, when it starts, the
// proposal the placement gave it, and whether the cap spilled it off the proposal it drew. A
// start before the window is no different from one in it: what the delivery join reads is the
// life each session went on to have (sessionWindow), not when it began.
type unitStart struct {
	N        int
	At       time.Time
	Proposal int
	Spilled  bool
}

// planUnitStarts walks the unit sessions' open-loop schedule on unitStartStream from start until
// winEnd and places every start (unitPlacer.take), without a clock of its own. The draw of the
// proposal follows its arrival in the stream, as every openLoopPlan generator draws what an
// arrival does, so one -rng-seed gives one schedule and one placement; fn returns false to end
// the walk. A start no proposal had room for is dropped, counted as a spill, and fn never sees it.
func planUnitStarts(rngSeed uint64, rate float64, life time.Duration, p *unitPlacer, start, winStart, winEnd time.Time, fn func(unitStart) bool) {
	n := 0
	openLoopPlan(rngSeed, unitStartStream, rate, start, winStart, winEnd, func(r *rand.Rand, next time.Time, _ bool) bool {
		x := r.Float64()
		at, spilled := p.take(next, x, life)
		if at < 0 {
			return true
		}
		n++
		return fn(unitStart{N: n, At: next, Proposal: at, Spilled: spilled})
	})
}

// driverProposals is the proposals the driver session follows, at most n of them: every proposal
// the loop's driver is live on first (LiveInDriver), then the others by the size of their part
// documents.
//
// The exported seed holds no byte size for a part document (loopSeedPart), and the planner packs
// a proposal's parts up to one markdown budget each (loopseed.go, loopMarkdownBudget), so the
// part count is what the seed says about those bytes: the proposal with the most part documents
// holds the most of them. Ties go to the proposal with more requirement ids, then to the seed's
// own order, so one seed gives one list.
func driverProposals(ls *loopSeed, n int) []int {
	at := make([]int, len(ls.Proposals))
	for i := range at {
		at[i] = i
	}
	slices.SortStableFunc(at, func(a, b int) int {
		x, y := &ls.Proposals[a], &ls.Proposals[b]
		if x.LiveInDriver != y.LiveInDriver {
			if x.LiveInDriver {
				return -1
			}
			return 1
		}
		if d := len(y.Parts) - len(x.Parts); d != 0 {
			return d
		}
		if d := y.IDs - x.IDs; d != 0 {
			return d
		}
		return a - b
	})
	return at[:min(n, len(at))]
}

// --- the sessions ----------------------------------------------------------------------------

// sessionBus is where a loop session's subscriptions live: the step's NATS connections in a run,
// a stand-in in a test.
type sessionBus interface {
	// subscribe takes the messages of topic on connection conn, which the sessions share.
	subscribe(conn int, topic string, h func(busMsg)) (sessionSub, error)
}

// busMsg is one message a subscription took: its bytes, the headers a relay frame carries, and
// the answer to it.
type busMsg interface {
	data() []byte
	header(name string) string
	respond(b []byte) error
}

// sessionSub is one subscription, which the session drops when it stops.
type sessionSub interface {
	unsubscribe() error
}

// natsBus is the step's own NATS connections as a sessionBus: a session subscribes on the
// connection its number lands on, as the flat population's sessions share theirs
// (subscribeSessions).
type natsBus struct{ conns []*nats.Conn }

func (b natsBus) subscribe(conn int, topic string, h func(busMsg)) (sessionSub, error) {
	nc := b.conns[conn%len(b.conns)]
	sub, err := nc.Subscribe(topic, func(m *nats.Msg) { h(natsMsg{m: m}) })
	if err != nil {
		return nil, err
	}
	return natsSub{sub: sub}, nil
}

type natsMsg struct{ m *nats.Msg }

func (m natsMsg) data() []byte              { return m.m.Data }
func (m natsMsg) header(name string) string { return m.m.Header.Get(name) }

// respond answers a message that asked for one; a message with no reply subject is answered
// nothing, as the relay's reply does (relaySessions.reply).
func (m natsMsg) respond(b []byte) error {
	if m.m.Reply == "" {
		return fmt.Errorf("the message has no reply subject")
	}
	return m.m.Respond(b)
}

type natsSub struct{ sub *nats.Subscription }

func (s natsSub) unsubscribe() error { return s.sub.Unsubscribe() }

// loopListener is one loop session's listener: the endpoint it registers, heartbeats,
// unsubscribes and publishes on, with the run's listener bearer.
type loopListener struct {
	url, token string
	client     *http.Client
}

func newLoopListener(url, token string) *loopListener {
	return &loopListener{url: url, token: token, client: &http.Client{Timeout: loopCallTimeout}}
}

// post sends body as JSON to path and reads the answer; a non-2xx answer is an error naming the
// status and what the listener said, as the registrar's registration is (registrar.register).
func (l *loopListener) post(ctx context.Context, path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", l.url+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.token)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return b, fmt.Errorf("%d %s", resp.StatusCode, trunc(b))
	}
	return b, nil
}

// loopRelay is one loop session's consumer delivery: the epoch it registers with, whether the
// listener's last answer gave it consumer delivery, its resume point and the stream sequences it
// has taken. The flat population keeps the same by session index (relaySessions, which holds the
// protocol); a loop session comes and goes, so it keeps its own.
type loopRelay struct {
	epoch    string
	mu       sync.Mutex
	consumer bool
	stream   string
	written  uint64
	seen     map[uint64]struct{}
}

// newLoopRelay names the epoch after the session and the run, which no other session of any run
// shares, so a registration of this epoch is this session's.
func newLoopRelay(id string, nonce int64) *loopRelay {
	return &loopRelay{epoch: id + "-" + formatNonce(nonce), seen: map[uint64]struct{}{}}
}

// delivery is the session's registration delivery field: consumer delivery, its epoch, and its
// resume point once it has one, as the pi plugin sends at every registration.
func (r *loopRelay) delivery() map[string]any {
	d := map[string]any{"consumer": true, "epoch": r.epoch}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stream != "" && r.written > 0 {
		d["resume"] = map[string]any{"stream_id": r.stream, "seq": r.written}
	}
	return d
}

// answered reads a registration's answer: a core answer, or none, leaves the session on its core
// subscriptions, and a consumer answer naming another stream than its resume point starts that
// point again on the new stream.
func (r *loopRelay) answered(body []byte) {
	var ans registerAnswer
	consumer, stream := false, ""
	if json.Unmarshal(body, &ans) == nil && ans.Delivery != nil {
		consumer, stream = ans.Delivery.Mode == sessionDeliveryConsumer, ans.Delivery.StreamID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consumer = consumer
	if consumer && stream != "" && stream != r.stream {
		r.stream, r.written = stream, 0
	}
}

// relayed reports whether the listener's last answer relays the session, so that a core copy of
// one of its subjects is no receipt.
func (r *loopRelay) relayed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.consumer
}

// take reports whether seq is a frame the session has not taken. A repeat of one it has is the
// stream's redelivery, which the plugin's ledger hides, and is no receipt.
func (r *loopRelay) take(seq uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.seen[seq]; seen {
		return false
	}
	r.seen[seq] = struct{}{}
	return true
}

// wrote moves the session's resume point past seq of stream, once it answered the frame done.
func (r *loopRelay) wrote(stream string, seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stream != "" && stream != r.stream {
		r.stream, r.written = stream, 0
	}
	r.written = max(r.written, seq)
}

// loopSession is one session of the loop's population: which class it belongs to, its id, the
// listener it registers on, the NATS connection it subscribes over, the issues it follows and the
// topics it registers (the driver's hold the units' exit topic as well).
type loopSession struct {
	class    string
	id       string
	listener *loopListener
	conn     int
	issues   []string
	topics   []string
	relay    *loopRelay
	// ready closes once the session's registration has been answered, or has failed, so that the
	// step can wait for the driver before it opens its window.
	ready chan struct{}
}

// fields is the session's registration with the listener, as the pi plugin sends it: its
// directory, title, the topics it follows and, since every loop session asks for consumer
// delivery, its delivery field (relay.go).
func (s *loopSession) fields() map[string]any {
	return map[string]any{"session_id": s.id, "dir": "/work/" + s.id, "title": "load " + s.id,
		"topics": s.topics, "port": 0, "self_subscribed": true, "driving": s.class == deliveryClassDriver,
		"capabilities": []string{}, "delivery": s.relay.delivery()}
}

// loopSessions is the spec loop's session population: the unit sessions the schedule starts and
// the one driver session, their listeners, their placement, every session's window and the times
// of the calls each one made.
type loopSessions struct {
	cfg   runConfig
	lg    *log.Logger
	nonce int64
	bus   sessionBus
	// receipts is the step's own collector: a loop session's receipts carry its session id, so
	// the join counts them by class (delivery.go).
	receipts  *collector[receipt]
	listeners []*loopListener
	placer    *unitPlacer
	exit      string
	// heartbeat is how often a live session re-registers, loopHeartbeat in a run; a test that
	// watches a session's whole life sets its own.
	heartbeat time.Duration

	windows                               *collector[sessionWindow]
	startMs, subscribeMs, unsubscribeMs   *collector[float64]
	concurrent                            *collector[float64]
	started, stopped, startErrors         atomic.Int64
	unsubscribeErrors, unsubscribedTopics atomic.Int64
	heartbeatErrors                       atomic.Int64
	exits, exitErrors                     atomic.Int64
	driverTopics                          int
	driverUp                              atomic.Bool
	wg                                    sync.WaitGroup
}

// newLoopSessions builds the population from the step's configuration: a listener per
// -loop-listeners endpoint, the cap's placement over the loop seed's proposals, and the run's own
// exit topic.
func newLoopSessions(cfg runConfig, lg *log.Logger, nonce int64, b sessionBus, receipts *collector[receipt]) *loopSessions {
	lp := &loopSessions{cfg: cfg, lg: lg, nonce: nonce, bus: b, receipts: receipts,
		placer: newUnitPlacer(cfg.loop, unitCapPerProposal), exit: unitExitTopic(nonce),
		windows:     newCollector[sessionWindow](1 << 12),
		heartbeat:   loopHeartbeat,
		startMs:     newCollector[float64](1 << 12),
		subscribeMs: newCollector[float64](1 << 12),
		// A heartbeat sends no unsubscribe, so a session gives one of each of the two below.
		unsubscribeMs: newCollector[float64](1 << 12),
		concurrent:    newCollector[float64](1 << 10),
	}
	for _, ep := range cfg.loopListeners {
		lp.listeners = append(lp.listeners, newLoopListener(ep, cfg.ep.listenerToken))
	}
	return lp
}

// startLoopSessions starts the spec loop's own sessions, or answers nil for a step without them
// (-unit-starts 0), which runs the flat population alone. Its driver session registers here,
// before the window opens, so that it follows every write of the window; the unit sessions start
// on their own schedule once it has (runUnits).
func (st *step) startLoopSessions(ctx context.Context, ns *natsSessions) *loopSessions {
	if st.cfg.unitStarts <= 0 {
		return nil
	}
	lp := newLoopSessions(st.cfg, st.lg, st.nonce, natsBus{conns: ns.conns}, ns.receipts)
	lp.startDriver(ctx)
	st.lg.Printf("loop sessions: driver on %s following %d topics, units starting at %g/s over %d listeners, each living %s (cap %d a proposal), exit topic %s",
		lp.cfg.loopListeners[len(lp.cfg.loopListeners)-1], lp.driverTopics, lp.cfg.unitStarts,
		len(lp.cfg.loopListeners)-1, lp.cfg.unitLife, unitCapPerProposal, lp.exit)
	return lp
}

// startDriver registers the driver session on the last -loop-listeners endpoint and waits for its
// registration to be answered: it follows the step's driver topics and the units' exit topic, and
// lives until the step ends.
func (lp *loopSessions) startDriver(ctx context.Context) {
	s := &loopSession{class: deliveryClassDriver, id: "ses-lg-driver", listener: lp.listeners[len(lp.listeners)-1],
		relay: newLoopRelay("ses-lg-driver", lp.nonce), ready: make(chan struct{})}
	for _, at := range driverProposals(lp.cfg.loop, lp.cfg.driverTopics) {
		key := lp.cfg.loop.Proposals[at].Key
		s.issues = append(s.issues, key)
		s.topics = append(s.topics, issueTopicOf(key))
	}
	s.topics = append(s.topics, lp.exit)
	lp.driverTopics = len(s.topics)
	lp.wg.Go(func() { lp.run(ctx, s, time.Time{}) })
	select {
	case <-s.ready:
	case <-ctx.Done():
	}
}

// runUnits walks the unit sessions' schedule, from the step's start to the end of its window, and
// starts every session the placement gave a proposal. The channel it answers closes once the
// schedule has ended; the sessions it started live on until their life runs out or ctx ends, and
// stop waits for them.
func (lp *loopSessions) runUnits(ctx context.Context, start, winStart, winEnd time.Time) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		stopSampler := lp.sampleConcurrent(ctx)
		defer close(stopSampler)
		planUnitStarts(lp.cfg.rngSeed, lp.cfg.unitStarts, lp.cfg.unitLife, lp.placer, start, winStart, winEnd, func(u unitStart) bool {
			if !sleepUntil(ctx, u.At) {
				return false
			}
			s := lp.unitSession(u)
			end := u.At.Add(lp.cfg.unitLife)
			lp.wg.Go(func() { lp.run(ctx, s, end) })
			return true
		})
	}()
	return done
}

// unitSession is the session one planned start runs as: its own id, the proposal's issue topic
// alone, and the listener its number lands on, which is any but the driver's.
func (lp *loopSessions) unitSession(u unitStart) *loopSession {
	id := fmt.Sprintf("ses-lg-unit-%05d", u.N)
	key := lp.cfg.loop.Proposals[u.Proposal].Key
	return &loopSession{class: deliveryClassUnit, id: id, listener: lp.listeners[u.N%(len(lp.listeners)-1)],
		conn: u.N, issues: []string{key}, topics: []string{issueTopicOf(key)},
		relay: newLoopRelay(id, lp.nonce), ready: make(chan struct{})}
}

// sampleConcurrent records the live unit sessions once a second, so the step says how many were
// live over it rather than at one instant. The channel it answers stops it, as ctx does.
func (lp *loopSessions) sampleConcurrent(ctx context.Context) chan struct{} {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case now := <-t.C:
				lp.concurrent.add(float64(lp.placer.liveAt(now)))
			}
		}
	}()
	return stop
}

// stop waits for every loop session to have returned, which each does when its life runs out or
// when ctx ends, so that every window is recorded before the summary reads them.
func (lp *loopSessions) stop() { lp.wg.Wait() }

// run is one loop session's life: it subscribes its relay subject, its agent topic and the topics
// it follows, registers with the listener, heartbeats until end (the driver until ctx ends),
// publishes its exit message, unsubscribes the topics it held and drops its subscriptions. It
// records the session's window whatever happens, so a session whose registration failed is
// counted as one that followed nothing.
func (lp *loopSessions) run(ctx context.Context, s *loopSession, end time.Time) {
	began := time.Now().UnixNano()
	w := sessionWindow{Class: s.class, ID: s.id, Issues: s.issues}
	defer func() { lp.windows.add(w) }()
	ready := sync.OnceFunc(func() { close(s.ready) })
	defer ready()

	var subs []sessionSub
	defer func() {
		for _, sub := range subs {
			_ = sub.unsubscribe()
		}
	}()
	// The relay subject first, as the pi plugin subscribes it before it registers (relay.go),
	// then the agent topic and the topics the session follows, as a self-subscribed session does.
	for _, sub := range []struct {
		topic string
		take  func(busMsg)
	}{{bus.DeliverSubject(s.id), lp.frames(s)}, {contracts.AgentSubject(s.id), lp.copies(s, true)}} {
		got, err := lp.bus.subscribe(s.conn, sub.topic, sub.take)
		if err != nil {
			lp.failedStart(s, fmt.Errorf("subscribe %s: %w", sub.topic, err))
			return
		}
		subs = append(subs, got)
	}
	for _, topic := range s.topics {
		got, err := lp.bus.subscribe(s.conn, topic, lp.copies(s, false))
		if err != nil {
			lp.failedStart(s, fmt.Errorf("subscribe %s: %w", topic, err))
			return
		}
		subs = append(subs, got)
	}

	w.SubSent = time.Now().UnixNano()
	answer, err := s.listener.post(ctx, "/v1/interests/subscribe", s.fields())
	if err != nil {
		lp.failedStart(s, fmt.Errorf("subscribe: %w", err))
		return
	}
	w.SubAck = time.Now().UnixNano()
	s.relay.answered(answer)
	lp.subscribeMs.add(msBetween(w.SubSent, w.SubAck))
	lp.startMs.add(msBetween(began, w.SubAck))
	lp.started.Add(1)
	if s.class == deliveryClassDriver {
		lp.driverUp.Store(true)
	}
	ready()

	if !lp.live(ctx, s, end) {
		// The step ended first: the session was still subscribed when it stopped measuring, so
		// its window stays open and every later write is counted against it.
		return
	}
	if s.class == deliveryClassUnit {
		lp.publishExit(ctx, s)
	}
	w.UnsubSent = time.Now().UnixNano()
	removed, err := lp.unsubscribe(ctx, s)
	w.UnsubAck = time.Now().UnixNano()
	lp.unsubscribeMs.add(msBetween(w.UnsubSent, w.UnsubAck))
	lp.unsubscribedTopics.Add(int64(len(removed)))
	if err != nil {
		// A refused unsubscribe ends the session all the same: its subscriptions are gone, so
		// nothing after it could have reached it. The refusal is counted, and stops no other
		// session and no generator.
		if n := lp.unsubscribeErrors.Add(1); n <= loopLogLimit && ctx.Err() == nil {
			lp.lg.Printf("loop sessions: %s unsubscribe: %v", s.id, err)
		}
	}
	lp.stopped.Add(1)
}

// failedStart counts and logs a session that could not start, which leaves its window closed: it
// followed nothing, so it was owed nothing.
func (lp *loopSessions) failedStart(s *loopSession, err error) {
	if n := lp.startErrors.Add(1); n <= loopLogLimit {
		lp.lg.Printf("loop sessions: %s %v", s.id, err)
	}
}

// live heartbeats the session until end, and answers true when it reached it; a session ctx ended
// first answers false and neither publishes nor unsubscribes, since the step is over. The driver
// session has no end and lives until ctx does.
func (lp *loopSessions) live(ctx context.Context, s *loopSession, end time.Time) bool {
	var stop <-chan time.Time
	if !end.IsZero() {
		timer := time.NewTimer(time.Until(end))
		defer timer.Stop()
		stop = timer.C
	}
	beat := time.NewTicker(lp.heartbeat)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-stop:
			return true
		case <-beat.C:
			answer, err := s.listener.post(ctx, "/v1/interests/subscribe", s.fields())
			if err == nil {
				s.relay.answered(answer)
				continue
			}
			if n := lp.heartbeatErrors.Add(1); n <= loopLogLimit && ctx.Err() == nil {
				lp.lg.Printf("loop sessions: %s heartbeat: %v", s.id, err)
			}
		}
	}
}

// publishExit publishes the unit session's one exit message on the run's exit topic through its
// own listener, as a session publishes an ad-hoc topic (POST /v1/messages/publish): the driver
// follows that topic, so a unit's stop is itself traffic the step measures.
func (lp *loopSessions) publishExit(ctx context.Context, s *loopSession) {
	body := map[string]any{"topic": lp.exit, "message": "unit session " + s.id + " is stopping",
		"source": "agent", "source_session": s.id}
	if _, err := s.listener.post(ctx, "/v1/messages/publish", body); err != nil {
		if n := lp.exitErrors.Add(1); n <= loopLogLimit && ctx.Err() == nil {
			lp.lg.Printf("loop sessions: %s exit message: %v", s.id, err)
		}
		return
	}
	lp.exits.Add(1)
}

// unsubscribe drops the topics the session held, which is how a session stops: there is no
// deregistration route, and the registry drops a session 300 s after its last write. It answers
// the topics the listener says it removed.
func (lp *loopSessions) unsubscribe(ctx context.Context, s *loopSession) ([]string, error) {
	body, err := s.listener.post(ctx, "/v1/interests/unsubscribe", map[string]any{"session_id": s.id, "topics": s.topics})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Removed []string `json:"removed"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return nil, fmt.Errorf("unreadable answer: %s", trunc(body))
	}
	return answer.Removed, nil
}

// copies takes a core copy of one of the session's subjects: the event in it is the session's
// receipt, under its own id. For a session the listener relays, the relay frame is the receipt
// instead, and the core copy only answers the lane receipt on the agent topic, as the pi plugin
// does (relay.go).
func (lp *loopSessions) copies(s *loopSession, agent bool) func(busMsg) {
	return func(m busMsg) {
		now := time.Now().UnixNano()
		if s.relay.relayed() {
			if agent {
				_ = m.respond(nil)
			}
			return
		}
		id, _, ok := envelopeEvent(m.data())
		if !ok {
			return
		}
		lp.receipts.add(receipt{EventID: id, Recv: now, Agent: agent, Session: s.id})
	}
}

// frames takes the session's relay frames: it answers received at once, counts the first frame of
// each stream sequence as the session's receipt, with the stream's store time, answers done and
// moves its resume point, as the pi plugin does (relay.go).
func (lp *loopSessions) frames(s *loopSession) func(busMsg) {
	return func(m busMsg) {
		now := time.Now().UnixNano()
		_ = m.respond(relayReplyReceived)
		seq, _ := strconv.ParseUint(m.header(relayHeaderStreamSeq), 10, 64)
		var stored int64
		if at, err := time.Parse(time.RFC3339Nano, m.header(relayHeaderStoredAt)); err == nil {
			stored = at.UnixNano()
		}
		if id, _, ok := envelopeEvent(m.data()); ok && s.relay.take(seq) {
			lp.receipts.add(receipt{EventID: id, Recv: now, Published: stored, Session: s.id,
				Agent: m.header(relayHeaderSubject) == contracts.AgentSubject(s.id)})
		}
		_ = m.respond(relayReplyDone)
		s.relay.wrote(m.header(relayHeaderStreamID), seq)
	}
}

// msBetween is the milliseconds from one wall-clock nanosecond to another, as every latency of a
// step is recorded.
func msBetween(from, to int64) float64 { return float64(to-from) / 1e6 }

// loopSessionsSummary is how the spec loop's sessions went: what they ran at, what the placement
// did, how long their calls took and how many units were live over the step. What each class was
// owed and what it got is the delivery join's, under events.by_class (classDelivery).
type loopSessionsSummary struct {
	UnitStartsPerS    float64 `json:"unit_starts_per_s"`
	UnitLifeS         float64 `json:"unit_life_s"`
	ReceiptDeadlineMs float64 `json:"receipt_deadline_ms"`
	// Listeners is how many endpoints -loop-listeners named; the unit sessions take all but the
	// last and the driver session the last.
	Listeners int `json:"listeners"`
	// Started counts the unit and driver sessions the listener acknowledged the subscribe of,
	// Stopped those that reached the end of their life and unsubscribed, and StartErrors the
	// sessions that could not subscribe at all.
	Started     int64 `json:"started"`
	Stopped     int64 `json:"stopped"`
	StartErrors int64 `json:"start_errors"`
	// UnsubscribeErrors counts the unsubscribes the listener refused, which end their session all
	// the same, and UnsubscribedTopics the topics it said it removed. HeartbeatErrors counts the
	// re-registrations that failed.
	UnsubscribeErrors  int64 `json:"unsubscribe_errors"`
	UnsubscribedTopics int64 `json:"unsubscribed_topics"`
	HeartbeatErrors    int64 `json:"heartbeat_errors"`
	// ExitMessages counts the exit messages the units published and ExitErrors those the listener
	// refused.
	ExitMessages int64 `json:"exit_messages"`
	ExitErrors   int64 `json:"exit_errors"`
	// StartMs is the round trip of a whole session start, from the first subscription to the
	// acknowledged registration; SubscribeMs and UnsubscribeMs the round trips of those two calls.
	StartMs       stat `json:"start_ms"`
	SubscribeMs   stat `json:"subscribe_ms"`
	UnsubscribeMs stat `json:"unsubscribe_ms"`
	// Concurrent is the live unit sessions, sampled every second while the schedule ran.
	Concurrent stat `json:"concurrent"`
	// MostOnOneProposal is the most live unit sessions one proposal held, which the cap holds at
	// unitCapPerProposal, and Spilled how many starts drew a proposal that was at the cap.
	MostOnOneProposal int `json:"most_on_one_proposal"`
	Spilled           int `json:"spilled"`
	// DriverTopics is how many topics the driver session registered, its issue topics and the
	// units' exit topic, and DriverSubscribed whether the listener acknowledged it.
	DriverTopics     int    `json:"driver_topics"`
	DriverSubscribed bool   `json:"driver_subscribed"`
	ExitTopic        string `json:"exit_topic"`
}

// summary is what the population recorded, read once the schedule has ended and every session has
// returned (stop).
func (lp *loopSessions) summary() loopSessionsSummary {
	return loopSessionsSummary{
		UnitStartsPerS: lp.cfg.unitStarts, UnitLifeS: lp.cfg.unitLife.Seconds(),
		ReceiptDeadlineMs: float64(lp.cfg.receiptDeadline) / 1e6, Listeners: len(lp.listeners),
		Started: lp.started.Load(), Stopped: lp.stopped.Load(), StartErrors: lp.startErrors.Load(),
		UnsubscribeErrors: lp.unsubscribeErrors.Load(), UnsubscribedTopics: lp.unsubscribedTopics.Load(),
		HeartbeatErrors: lp.heartbeatErrors.Load(),
		ExitMessages:    lp.exits.Load(), ExitErrors: lp.exitErrors.Load(),
		StartMs: mkStat(lp.startMs.snapshot()), SubscribeMs: mkStat(lp.subscribeMs.snapshot()),
		UnsubscribeMs: mkStat(lp.unsubscribeMs.snapshot()), Concurrent: mkStat(lp.concurrent.snapshot()),
		MostOnOneProposal: lp.placer.mostOnOneProposal(), Spilled: lp.placer.spilled(),
		DriverTopics: lp.driverTopics, DriverSubscribed: lp.driverUp.Load(), ExitTopic: lp.exit,
	}
}
