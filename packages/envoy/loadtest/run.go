package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

// maxRequestsInFlight caps the request generator: above 600 req/s times the 30 s client
// timeout. A request the cap refuses is counted as dropped, never sent.
const maxRequestsInFlight = 25000

type runConfig struct {
	ep           endpoints
	allowForeign bool
	name, out    string
	seedPath     string
	sessions     int
	events       float64
	requests     float64
	warmup       time.Duration
	duration     time.Duration
	grace        time.Duration
	mix          eventMix
	routes       routeMix
	human        float64
	live         int
	rngSeed      uint64
	natsConns    int
	// sessionDelivery is how the simulated sessions take their messages: sessionDeliveryCore
	// (their core subscriptions) or sessionDeliveryConsumer (relay frames of their consumers).
	sessionDelivery string
	buildCommit     string
	buildDirty      bool
	// boxBusyBefore is the box's busy fraction read before Dispatch started (run-step.sh reads it
	// with `loadgen box-busy` before its reset); negative means collectMeta reads it itself.
	boxBusyBefore float64
	// args are the run's flags as meta.json records them: the normalized flag list, the token
	// values redacted and the URL passwords removed (recordedArgs).
	args []string
}

func runLoad(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var ef endpointFlags
	ef.register(fs)
	events := fs.Float64("events", -1, "events per second (required; 0 starts no event generator)")
	sessions := fs.Int("sessions", -1, "sessions (required)")
	requests := fs.Float64("requests", -1, "requests per second (required; 0 starts no request generator)")
	warmup := fs.Duration("warmup", 30*time.Second, "warm-up before the measured window")
	duration := fs.Duration("duration", 180*time.Second, "measured window")
	grace := fs.Duration("grace", 20*time.Second, "wait for late deliveries once the generators finish")
	mixFlag := fs.String("event-mix", defaultEventMix, "event mix: a preset ("+eventMixPresetNames()+") or kind=weight,...")
	routeFlag := fs.String("route-mix", "", "route-mix file (default: the committed "+defaultRouteMixName+")")
	human := fs.Float64("human", 0.1, "share of writes made by a person")
	live := fs.Int("live", -1, "dashboard live sockets (default max(1, round(requests/30)))")
	rngSeed := fs.Uint64("rng-seed", 1, "seed of the event and request schedules")
	natsConns := fs.Int("nats-conns", 50, "NATS connections the sessions share")
	out := fs.String("out", "", "output directory (required)")
	name := fs.String("name", "", "step name (default: the -out directory's name)")
	seedPath := fs.String("seed", "", "seed.json path (default $LOADTEST_ROOT/seed.json)")
	buildCommit := fs.String("build-commit", "", "commit Dispatch was built from, for meta.json")
	buildDirty := fs.Bool("build-dirty", false, "the build's tree had uncommitted changes, for meta.json")
	boxBusyBefore := fs.Float64("box-busy-before", -1, "the box's busy fraction read before Dispatch started (loadgen box-busy); default: read it now")
	delivery := fs.String("session-delivery", envOr("LOADTEST_SESSION_DELIVERY", sessionDeliveryCore), "how the sessions take their messages: core (their core subscriptions) or consumer (the listener's relay of their consumers); default $LOADTEST_SESSION_DELIVERY, else core")
	_ = fs.Parse(args)

	// Every input is validated before the first connection.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// The endpoint guard, then the two mixes, then the rates: each refusal names its input.
	ep, err := ef.resolve()
	if err != nil {
		return err
	}
	mix, err := parseEventMix(*mixFlag)
	if err != nil {
		return err
	}
	routes, err := loadRouteMix(*routeFlag)
	if err != nil {
		return err
	}
	switch {
	case *events < 0 || !finite(*events):
		return fmt.Errorf("-events is required and must be 0 or more")
	case *requests < 0 || !finite(*requests):
		return fmt.Errorf("-requests is required and must be 0 or more")
	case *sessions < 1:
		return fmt.Errorf("-sessions is required and must be at least 1")
	case *human < 0 || *human > 1 || !finite(*human):
		return fmt.Errorf("-human must be between 0 and 1")
	case *warmup < 0 || *grace < 0 || *duration <= 0:
		return fmt.Errorf("-warmup and -grace must be 0 or more, -duration more than 0")
	case *natsConns < 1:
		return fmt.Errorf("-nats-conns must be at least 1")
	case *live < -1:
		return fmt.Errorf("-live must be 0 or more")
	case *delivery != sessionDeliveryCore && *delivery != sessionDeliveryConsumer:
		return fmt.Errorf("-session-delivery must be %s or %s, not %q", sessionDeliveryCore, sessionDeliveryConsumer, *delivery)
	case *boxBusyBefore > 1 || !finite(*boxBusyBefore):
		return fmt.Errorf("-box-busy-before must be a fraction between 0 and 1")
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	if *seedPath == "" {
		*seedPath = filepath.Join(loadtestRoot(), "seed.json")
	}
	sf, err := readSeed(*seedPath)
	if err != nil {
		return err
	}
	for _, is := range sf.Issues {
		if mix.Weights[kindReply] > 0 && len(is.AnchoredCommentIDs)+len(is.PlainCommentIDs) == 0 {
			return fmt.Errorf("seed %s: issue %s has no root comment ids for replies; reseed with this loadgen", *seedPath, is.Key)
		}
		if mix.Weights[kindAnchored] > 0 && len(is.Quotes) == 0 {
			return fmt.Errorf("seed %s: issue %s has no quotes for anchored comments", *seedPath, is.Key)
		}
	}
	if *live < 0 {
		*live = max(1, int(math.Round(*requests/30)))
	}
	if *name == "" {
		*name = filepath.Base(filepath.Clean(*out))
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	return execute(runConfig{
		ep: ep, allowForeign: ef.allowForeign, name: *name, out: *out, seedPath: *seedPath,
		sessions: *sessions, events: *events, requests: *requests,
		warmup: *warmup, duration: *duration, grace: *grace, mix: mix, routes: routes,
		human: *human, live: *live, rngSeed: *rngSeed, natsConns: *natsConns, sessionDelivery: *delivery,
		buildCommit: *buildCommit, buildDirty: *buildDirty, boxBusyBefore: *boxBusyBefore, args: recordedArgs(fs),
	}, sf)
}

// redacted replaces a token, or a value whose password cannot be found, in what a step records.
const redacted = "<redacted>"

// recordedArgs is every flag given to fs, once parsed, as "-name=value" in fs's (lexical) order:
// the value of each tokenFlags flag replaced by redacted, and each urlFlags flag's value without
// its credentials (redactEndpoint: -pg as its hosts, ports and database alone). runLoad takes no
// positional argument, so these are all of its arguments.
func recordedArgs(fs *flag.FlagSet) []string {
	var out []string
	fs.Visit(func(f *flag.Flag) {
		name, value := "-"+f.Name, f.Value.String()
		switch {
		case slices.Contains(tokenFlags, name):
			value = redacted
		case slices.Contains(urlFlags, name):
			value = redactEndpoint(name, value)
		}
		out = append(out, name+"="+value)
	})
	return out
}

type session struct {
	id      string
	home    int
	watched []int
}

// reqRec is one request the request generator sent in the window.
type reqRec struct {
	route     string
	status    int
	transport bool
	latency   time.Duration // arrival (or start, when the write never completed) to response read
	connWait  time.Duration
}

type routeSummary struct {
	Count           int            `json:"count"`
	Failures        int            `json:"failures"` // non-2xx or transport error
	LatencyMs       stat           `json:"latency_ms"`
	Status          map[string]int `json:"status"`
	TransportErrors int            `json:"transport_errors"`
}

type requestsSummary struct {
	TargetPerS float64 `json:"target_per_s"`
	Sent       int     `json:"sent"`
	SentPerS   float64 `json:"sent_per_s"`
	// BelowTarget is set when the generator sent under 95% of the target rate.
	BelowTarget        bool                    `json:"below_target"`
	OK2xx              int                     `json:"ok_2xx"`
	DroppedInflightCap int                     `json:"dropped_inflight_cap"`
	LatencyMs          stat                    `json:"latency_ms"`
	ConnWaitMs         stat                    `json:"conn_wait_ms"`
	Routes             map[string]routeSummary `json:"routes"`
}

type summaryConfig struct {
	Step         string   `json:"step"`
	EventsPerS   float64  `json:"events_per_s"`
	Sessions     int      `json:"sessions"`
	RequestsPerS float64  `json:"requests_per_s"`
	Live         int      `json:"live"`
	EventMix     eventMix `json:"event_mix"`
	RouteMix     string   `json:"route_mix"`
	Human        float64  `json:"human"`
	WarmupS      float64  `json:"warmup_s"`
	DurationS    float64  `json:"duration_s"`
	GraceS       float64  `json:"grace_s"`
	RNGSeed      uint64   `json:"rng_seed"`
	NATSConns    int      `json:"nats_conns"`
	// SessionDelivery is the delivery the sessions asked for (-session-delivery): core or
	// consumer. sessions.delivery says how many got consumer delivery.
	SessionDelivery string `json:"session_delivery"`
	// MarkerNonce is the run's marker nonce (markerTag): every marker the run wrote starts with it,
	// and the run counts only the commit rows that carry it.
	MarkerNonce string `json:"marker_nonce"`
}

type sessionsSummary struct {
	Registered        int   `json:"registered"`
	RegisterErrors    int   `json:"register_errors"`
	HeartbeatErrors   int64 `json:"heartbeat_errors"`
	NATSSubscriptions int   `json:"nats_subscriptions"`
	NATSConnections   int   `json:"nats_connections"`
	// Delivery is how the sessions' consumer delivery went; absent on the core path.
	Delivery *sessionDeliverySummary `json:"delivery,omitempty"`
}

type summary struct {
	Config          summaryConfig     `json:"config"`
	Events          eventsSummary     `json:"events"`
	Requests        requestsSummary   `json:"requests"`
	Resources       *resourcesSummary `json:"resources"`
	CommitOrderLock lockSummary       `json:"commit_order_lock"`
	UngrantedLocks  ungrantedSummary  `json:"ungranted_locks"`
	Sessions        sessionsSummary   `json:"sessions"`
	Live            liveSummary       `json:"live"`
	LiveBytes       int64             `json:"live_bytes"`
	// StackExits are the stack processes that exited during the step; such a step fails and its
	// row does not measure the load.
	StackExits []stackExit `json:"stack_exits,omitempty"`
}

type metaFile struct {
	Step        string    `json:"step"`
	Args        []string  `json:"args"`
	Start       time.Time `json:"start"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	End         time.Time `json:"end,omitzero"`
	Nproc       int       `json:"nproc"`
	CPUModel    string    `json:"cpu_model"`
	Kernel      string    `json:"kernel"`
	// BoxBusyBefore is the box's busy fraction over the BoxBusyBeforeWindowS seconds before the
	// step, from /proc/stat; null when /proc/stat gave no ticks.
	BoxBusyBefore        *float64 `json:"box_busy_before"`
	BoxBusyBeforeWindowS float64  `json:"box_busy_before_window_s"`
	GOMAXPROCS           string   `json:"gomaxprocs"`
	// PostgresVersion is select version() on -pg, "" when it could not be read, and then
	// PostgresVersionError is why.
	PostgresVersion      string          `json:"postgres_version"`
	PostgresVersionError string          `json:"postgres_version_error,omitempty"`
	BuildCommit          string          `json:"build_commit"`
	BuildDirty           bool            `json:"build_dirty"`
	SeedMeta             json.RawMessage `json:"seed_meta,omitempty"`
	Stack                string          `json:"stack"`
	PGDirect             bool            `json:"pg_direct"`
	AllowForeign         bool            `json:"allow_foreign"`
	PIDs                 map[string]int  `json:"pids"`
	MarkerNonce          string          `json:"marker_nonce"`
}

func execute(cfg runConfig, sf seedFile) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logf, err := os.Create(filepath.Join(cfg.out, "run.log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	lg := log.New(io.MultiWriter(os.Stderr, logf), "", log.LstdFlags|log.Lmicroseconds)
	nonce := newMarkerNonce()
	lg.Printf("config: step=%s sessions=%d events=%g/s requests=%g/s event-mix=%s route-mix=%s warmup=%s duration=%s grace=%s human=%.2f live=%d rng-seed=%d session-delivery=%s marker-nonce=%s",
		cfg.name, cfg.sessions, cfg.events, cfg.requests, cfg.mix.Name, cfg.routes.Source, cfg.warmup, cfg.duration, cfg.grace, cfg.human, cfg.live, cfg.rngSeed, cfg.sessionDelivery, formatNonce(nonce))

	liveURL, err := liveSocketURL(cfg.ep.dispatch)
	if err != nil {
		return err
	}

	if err := checkStack(ctx, cfg.ep); err != nil {
		return err
	}
	lg.Printf("guard: NATS, Dispatch and the listener are the stack's own (%s)", cfg.ep.stack)
	meta := collectMeta(ctx, cfg, lg)
	meta.MarkerNonce = formatNonce(nonce)
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, metaMode, lg)
	logSamplerStart(lg, cfg.ep)
	st := newStep(cfg, sf, lg, nonce)

	ns, err := st.subscribeSessions()
	defer ns.close()
	if err != nil {
		return err
	}
	reg := st.registerSessions(ctx, ns)
	st.startHeartbeat(ctx, reg)

	st.openWindow(time.Now())
	meta.Start, meta.WindowStart, meta.WindowEnd = st.start, st.winStart, st.winEnd
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, metaMode, lg)
	lg.Printf("window %s to %s", st.winStart.UTC().Format(time.RFC3339), st.winEnd.UTC().Format(time.RFC3339))

	a := newAPI(cfg.ep.dispatch, cfg.ep.token, 2048)
	live := st.startLive(ctx, liveURL)
	writes, eventsDone := st.runEvents(ctx, newWriter(a, sf, nonce, lg))
	reqs, requestsDone := st.runRequests(ctx, a)
	pr := st.startProbes(ctx, ns.pulls)

	<-eventsDone
	<-requestsDone
	lg.Printf("generators done at %s; grace %s", time.Since(st.start).Round(time.Second), cfg.grace)
	time.Sleep(cfg.grace)
	pr.wait()
	cancel()
	pr.waitSampler()
	ns.close()
	ns.envelopes.write(cfg.out, lg)
	if ns.relay != nil {
		ns.relay.writeFrames(cfg.out, st.sessions, lg)
	}

	sum := st.buildSummary(ns, reg, writes, reqs, live, pr)
	writeJSONFile(filepath.Join(cfg.out, "summary.json"), sum, 0o644, lg)
	meta.End = time.Now()
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, metaMode, lg)
	committed := fmt.Sprintf("committed %d, receipts %d of %d", sum.Events.Committed, sum.Events.IssueTopicReceipts, sum.Events.ExpectedIssueTopicReceipts)
	if d := sum.Sessions.Delivery; d != nil {
		committed += fmt.Sprintf(", sessions on consumer delivery %d of %d (core %d %v)", d.ConsumerSessions, cfg.sessions, d.CoreSessions, d.CoreReasons)
	}
	if sum.Events.commitsUnread() {
		committed = "committed and receipts not read"
	}
	lg.Printf("step %s done: events offered %d %s, requests sent %d (%.1f/s of %g)",
		cfg.name, sum.Events.Offered, committed, sum.Requests.Sent, sum.Requests.SentPerS, cfg.requests)
	if err := stepFailure(sum); err != nil {
		lg.Print(err)
		return err
	}
	return nil
}

// step is what a run's phases share: the configuration, the seed, the log, the marker nonce, the
// session topology and, once it opens, the window.
type step struct {
	cfg      runConfig
	sf       seedFile
	lg       *log.Logger
	nonce    int64
	active   int       // the issues in use: min(sessions, seeded issues)
	sessions []session // by index
	watchers [][]int   // by issue, the sessions that watch it
	// start is when the generators start; the window runs from winStart to winEnd.
	start, winStart, winEnd time.Time
}

// newStep lays out the sessions: session i works on issue i (mod the issues in use) and watches
// four others, so it comments on other sessions' documents. The seed routed issue i to session i.
func newStep(cfg runConfig, sf seedFile, lg *log.Logger, nonce int64) *step {
	st := &step{cfg: cfg, sf: sf, lg: lg, nonce: nonce, active: min(cfg.sessions, len(sf.Issues)),
		sessions: make([]session, cfg.sessions), watchers: make([][]int, len(sf.Issues))}
	for i := range st.sessions {
		s := session{id: sessionID(i), home: i % st.active}
		s.watched = []int{s.home}
		for k := 1; k <= 4; k++ {
			s.watched = append(s.watched, (s.home+k*37)%st.active)
		}
		for _, w := range s.watched {
			st.watchers[w] = append(st.watchers[w], i)
		}
		st.sessions[i] = s
	}
	return st
}

func (st *step) issueTopic(i int) string {
	return "notifications.dispatch.issue." + st.sf.Issues[i].Key + ".>"
}

// openWindow starts the generators' clock at start: the window opens after the warm-up.
func (st *step) openWindow(start time.Time) {
	st.start = start
	st.winStart = start.Add(st.cfg.warmup)
	st.winEnd = st.winStart.Add(st.cfg.duration)
}

func (st *step) inWindow(t time.Time) bool { return inWindow(t, st.winStart, st.winEnd) }

func inWindow(t, winStart, winEnd time.Time) bool { return !t.Before(winStart) && t.Before(winEnd) }

// liveSockets is what the dashboard live sockets read: their bytes and the event frames that
// arrived in the window.
type liveSockets struct {
	bytes  atomic.Int64
	frames *collector[liveFrame]
}

// startLive opens the dashboard live sockets on url, each subscribed to the event feed as a
// dashboard tab is and reconnecting a second after its socket ends, until ctx ends. Every event
// frame is timed on arrival (readLive).
func (st *step) startLive(ctx context.Context, url string) *liveSockets {
	ls := &liveSockets{frames: newCollector[liveFrame](1 << 14)}
	// A browser offers permessage-deflate, and Dispatch accepts it.
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second, EnableCompression: true}
	for i := range st.cfg.live {
		header := http.Header{"X-Dispatch-User": {fmt.Sprintf("viewer-%d", i)}}
		go followLive(ctx, dialer, url, header, st.nonce, time.Second, func(f liveFrame) {
			if st.inWindow(time.Unix(0, f.at)) {
				ls.frames.add(f)
			}
		}, func(n int64) { ls.bytes.Add(n) })
	}
	return ls
}

// openLoop is an open-loop Poisson schedule at rate per second, from stream `stream` of rngSeed:
// it draws the next arrival from start until one is at or after winEnd, sleeps until it, and calls
// fire with the stream and whether the arrival is in the window [winStart, winEnd). fire draws
// what the arrival does from the same stream, so the draws alternate, an arrival then its plan,
// and one seed gives one schedule. A rate of 0 or less fires nothing. It returns when the
// schedule ends or ctx does.
func openLoop(ctx context.Context, rngSeed, stream uint64, rate float64, start, winStart, winEnd time.Time, fire func(r *rand.Rand, in bool)) {
	if rate <= 0 {
		return
	}
	r := rand.New(rand.NewPCG(rngSeed, stream))
	for next := start; ; {
		next = next.Add(time.Duration(r.ExpFloat64() / rate * float64(time.Second)))
		if !next.Before(winEnd) || !sleepUntil(ctx, next) {
			return
		}
		fire(r, inWindow(next, winStart, winEnd))
	}
}

// runEvents starts the event generator, open-loop on stream 1; done closes once every write it
// sent has returned.
func (st *step) runEvents(ctx context.Context, wr *writer) (writes *collector[writeRec], done <-chan struct{}) {
	writes = newCollector[writeRec](1 << 14)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		var wg sync.WaitGroup
		defer wg.Wait()
		var marker int64
		openLoop(ctx, st.cfg.rngSeed, 1, st.cfg.events, st.start, st.winStart, st.winEnd, func(r *rand.Rand, in bool) {
			marker++
			p := planEvent(r, st.cfg, st.sessions, st.sf, marker)
			wg.Go(func() {
				rec := wr.do(ctx, p)
				rec.InWindow = in
				writes.add(rec)
			})
		})
	}()
	return writes, closed
}

// requestLoad is what the request generator sent in the window, and what the in-flight cap
// dropped.
type requestLoad struct {
	recs    *collector[reqRec]
	dropped atomic.Int64
}

// runRequests starts the request generator at the route mix, open-loop on stream 2; done closes
// once every request it sent has returned.
func (st *step) runRequests(ctx context.Context, a *api) (load *requestLoad, done <-chan struct{}) {
	load = &requestLoad{recs: newCollector[reqRec](1 << 16)}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		var wg sync.WaitGroup
		defer wg.Wait()
		var inflight atomic.Int64
		openLoop(ctx, st.cfg.rngSeed, 2, st.cfg.requests, st.start, st.winStart, st.winEnd, func(r *rand.Rand, in bool) {
			rt := st.cfg.routes.pick(r.Float64())
			path := rt.render(st.sf.Issues[r.IntN(st.active)], st.sf.Projects[r.IntN(len(st.sf.Projects))])
			id := who{}
			if rt.Person {
				id.person = fmt.Sprintf("person-%d", r.IntN(7))
			}
			if inflight.Load() >= maxRequestsInFlight {
				if in {
					load.dropped.Add(1)
				}
				return
			}
			inflight.Add(1)
			wg.Go(func() {
				defer inflight.Add(-1)
				res := a.call(ctx, "GET", path, id, nil)
				if !in {
					return
				}
				load.recs.add(reqRec{route: rt.Name, status: res.status, transport: res.err != nil, latency: time.Duration(res.end - res.arrival()), connWait: res.connWait})
			})
		})
	}()
	return load, closed
}

// probes are the 5 s sampler, the lock probe (20 ms), the ungranted-lock sampler (1 s), the
// transaction-id probe and the profiles.
type probes struct {
	smp      *sampler
	smpDone  chan struct{} // closed when the sampler, which runs until ctx ends, has returned
	lp       *lockProbe
	ls       *lockSampler
	xidsPerS *float64
	xidErr   error          // why xidsPerS is nil
	wg       sync.WaitGroup // every probe but the sampler
}

// startProbes starts the probes. The two lock samplers run from 20 s into the window to its end.
// The sampler reads pulls' counts at each sample, when pulls is not nil.
func (st *step) startProbes(ctx context.Context, pulls *pullSpy) *probes {
	p := &probes{smp: newSampler(st.cfg.ep), smpDone: make(chan struct{}), lp: &lockProbe{}, ls: &lockSampler{}}
	p.smp.pulls = pulls
	go func() {
		defer close(p.smpDone)
		p.smp.loop(ctx, st.winStart, st.winEnd, st.lg)
	}()
	lockFrom := st.winStart.Add(20 * time.Second)
	if lockFrom.After(st.winEnd) {
		lockFrom = st.winStart
	}
	pg := st.cfg.ep.pg
	p.wg.Go(func() { p.lp.loop(ctx, pg, lockFrom, st.winEnd, st.lg) })
	p.wg.Go(func() { p.ls.loop(ctx, pg, lockFrom, st.winEnd, st.lg) })
	p.wg.Go(func() { p.xidsPerS, p.xidErr = xidRate(ctx, pg, st.winStart, st.winEnd, st.lg) })
	p.wg.Go(func() { st.profile(ctx) })
	return p
}

// wait checks once more, after the grace, that every stack process is alive, then waits for the
// probes and the profiles.
func (p *probes) wait() {
	p.smp.checkAlive(time.Now())
	p.wg.Wait()
}

// waitSampler waits, once ctx has ended, for the sampler to take its last sample and log the
// totals of its failed reads.
func (p *probes) waitSampler() { <-p.smpDone }

// profile fetches Dispatch's profiles. The CPU profile covers the whole window; the goroutine
// dump and the heap profile are taken once it has finished, outside it.
func (st *step) profile(ctx context.Context) {
	if !sleepUntil(ctx, st.winStart) {
		return
	}
	pprof, out := st.cfg.ep.pprof, st.cfg.out
	secs := int(math.Ceil(st.cfg.duration.Seconds()))
	fetchTo(pprof+"/debug/pprof/profile?seconds="+strconv.Itoa(secs), filepath.Join(out, "cpu.pprof"), st.cfg.duration+60*time.Second, st.lg)
	fetchTo(pprof+"/debug/pprof/goroutine?debug=2", filepath.Join(out, "goroutines.txt"), 120*time.Second, st.lg)
	fetchTo(pprof+"/debug/pprof/heap", filepath.Join(out, "heap.pprof"), 60*time.Second, st.lg)
	fetchTo(pprof+"/debug/pprof/heap?debug=1", filepath.Join(out, "heap.txt"), 60*time.Second, st.lg)
}

// buildSummary joins what the phases recorded into summary.json, and writes samples.json. It
// reads the run's commit rows and names the ungranted locks' relations, so it runs after the load.
func (st *step) buildSummary(ns *natsSessions, reg *registrar, writes *collector[writeRec], reqs *requestLoad, live *liveSockets, pr *probes) summary {
	cfg := st.cfg
	ws := writes.snapshot()
	frames := live.frames.snapshot()
	pr.ls.resolveRelations(cfg.ep.pg)

	var markers []int64
	for _, w := range ws {
		if w.InWindow {
			markers = append(markers, w.Marker)
		}
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), time.Minute)
	commits, commitsErr := readCommits(readCtx, cfg.ep.pg, st.nonce, markers)
	readCancel()
	watcherCount := map[string]int{}
	for i, subscribed := range st.watchers {
		watcherCount[st.sf.Issues[i].Key] = len(subscribed)
	}
	durS := cfg.duration.Seconds()
	sum := summary{
		Config: summaryConfig{
			Step: cfg.name, EventsPerS: cfg.events, Sessions: cfg.sessions, RequestsPerS: cfg.requests, Live: cfg.live,
			EventMix: cfg.mix, RouteMix: cfg.routes.Source, Human: cfg.human, WarmupS: cfg.warmup.Seconds(),
			DurationS: durS, GraceS: cfg.grace.Seconds(), RNGSeed: cfg.rngSeed, NATSConns: cfg.natsConns,
			SessionDelivery: cfg.sessionDelivery, MarkerNonce: formatNonce(st.nonce),
		},
		Events:   joinDelivery(ws, commits, ns.receipts.snapshot(), watcherCount, durS),
		Requests: summarizeRequests(reqs.recs.snapshot(), int(reqs.dropped.Load()), cfg.requests, durS),
		Sessions: sessionsSummary{
			Registered: int(reg.ok.Load()), RegisterErrors: int(reg.errs.Load()), HeartbeatErrors: reg.heartbeatErrs.Load(),
			NATSSubscriptions: ns.subs, NATSConnections: len(ns.conns),
		},
		CommitOrderLock: summarizeLock(pr.lp.samples, pr.lp.errors),
		UngrantedLocks:  summarizeUngranted(pr.ls.samples, pr.ls.errors),
		Live:            summarizeLive(frames, ws, commits),
		LiveBytes:       live.bytes.Load(),
	}
	if ns.relay != nil {
		d := ns.relay.summary()
		sum.Sessions.Delivery = &d
	}
	if commitsErr != nil {
		sum.Events.CommitsError = commitsErr.Error()
		st.lg.Printf("commits: %v", commitsErr)
	}
	ss := pr.smp.all()
	writeJSONFile(filepath.Join(cfg.out, "samples.json"), ss, 0o644, st.lg)
	sum.Resources = summarizeResources(ss, sum.Events.Committed+sum.Requests.Sent, durS)
	if sum.Resources != nil {
		sum.Resources.XIDsPerS = pr.xidsPerS
		if pr.xidErr != nil {
			sum.Resources.XIDReadError = pr.xidErr.Error()
		}
		if commitsErr != nil {
			// Its operations count the committed events, which were not read.
			sum.Resources.DispatchCPUSPerOp = nil
		}
	}
	sum.StackExits = pr.smp.stackExits()
	return sum
}

// stepFailure is why the step failed, nil when it did not: a stack process exited during it
// (errStackExits), or the run's commit rows were not read (events.commits_error), so its
// committed, receipt and delivery columns measure nothing. The other reads of the run's database,
// the lock probe, the ungranted-lock sampler, the 5 s sampler's pg_stat_activity, the
// transaction-id probe and the Postgres version, feed none of those columns: a failure of one
// marks its own table, or meta.json, and does not fail the step.
func stepFailure(sum summary) error {
	var why []string
	if err := errStackExits(sum.StackExits); err != nil {
		why = append(why, err.Error())
	}
	if sum.Events.commitsUnread() {
		why = append(why, fmt.Sprintf("the run's commit rows were not read (summary.json events.commits_error: %s); this row's committed, receipt and delivery columns do not measure the load",
			sum.Events.CommitsError))
	}
	if len(why) == 0 {
		return nil
	}
	return errors.New(strings.Join(why, "; "))
}

// errStackExits is the step's failure when a stack process exited during it, nil otherwise.
func errStackExits(exits []stackExit) error {
	if len(exits) == 0 {
		return nil
	}
	gone := make([]string, len(exits))
	for i, x := range exits {
		gone[i] = fmt.Sprintf("%s (pid %d) exited during the step (first seen gone at %s)", x.Name, x.PID, x.FirstSeenGone.UTC().Format(time.RFC3339))
	}
	return fmt.Errorf("%s; this row does not measure the load", strings.Join(gone, ", "))
}

func summarizeRequests(reqs []reqRec, dropped int, target, durationS float64) requestsSummary {
	out := requestsSummary{TargetPerS: target, Sent: len(reqs), DroppedInflightCap: dropped, Routes: map[string]routeSummary{}}
	byRoute := map[string][]float64{}
	var all, wait []float64
	for _, r := range reqs {
		ms := float64(r.latency) / 1e6
		byRoute[r.route] = append(byRoute[r.route], ms)
		all = append(all, ms)
		wait = append(wait, float64(r.connWait)/1e6)
		rs := out.Routes[r.route]
		if rs.Status == nil {
			rs.Status = map[string]int{}
		}
		rs.Count++
		rs.Status[strconv.Itoa(r.status)]++
		if r.transport {
			rs.TransportErrors++
		}
		if r.transport || r.status/100 != 2 {
			rs.Failures++
		} else {
			out.OK2xx++
		}
		out.Routes[r.route] = rs
	}
	for name, rs := range out.Routes {
		rs.LatencyMs = mkStat(byRoute[name])
		out.Routes[name] = rs
	}
	out.LatencyMs = mkStat(all)
	out.ConnWaitMs = mkStat(wait)
	if durationS > 0 {
		out.SentPerS = float64(out.Sent) / durationS
	}
	out.BelowTarget = target > 0 && out.SentPerS < 0.95*target
	return out
}

// boxBusyBeforeWindow is the interval of the box-busy reading before a step, and
// boxBusyBeforeWarn the busy fraction above which run.log warns that the box is not idle. On
// gVisor /proc/loadavg always reads 0.00 and each Legion role has its own PID namespace, so only
// /proc/stat shows another role's CPU use.
const (
	boxBusyBeforeWindow = 5 * time.Second
	boxBusyBeforeWarn   = 0.10
)

// collectMeta records the machine, how busy it was over the 5 s before the step, the build and
// the seed, before the step starts.
func collectMeta(ctx context.Context, cfg runConfig, lg *log.Logger) metaFile {
	m := metaFile{
		Step: cfg.name, Args: cfg.args, Nproc: runtime.NumCPU(), BuildCommit: cfg.buildCommit, BuildDirty: cfg.buildDirty,
		Stack: cfg.ep.stack, PGDirect: cfg.ep.pgDirect, AllowForeign: cfg.allowForeign, PIDs: cfg.ep.pids, GOMAXPROCS: "unset",
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for line := range strings.SplitSeq(string(b), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				m.CPUModel = strings.TrimSpace(v)
				break
			}
		}
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		m.Kernel = strings.TrimSpace(string(b))
	}
	m.BoxBusyBeforeWindowS = boxBusyBeforeWindow.Seconds()
	busy, ok := cfg.boxBusyBefore, cfg.boxBusyBefore >= 0
	if !ok {
		busy, ok = boxBusy(ctx, boxBusyBeforeWindow)
	}
	if ok {
		m.BoxBusyBefore = &busy
		if busy > boxBusyBeforeWarn {
			lg.Printf("warning: the box was %.0f%% busy over the %s before the step (above %.0f%%): another process on the machine is using CPU, so this step shares it",
				100*busy, boxBusyBeforeWindow, 100*boxBusyBeforeWarn)
		}
	} else {
		lg.Printf("meta: box busy before: /proc/stat gave no ticks")
	}
	// GOMAXPROCS as Dispatch was started with it, from its environment when its pid is known.
	if v, ok := os.LookupEnv("GOMAXPROCS"); ok {
		m.GOMAXPROCS = v
	}
	if pid := cfg.ep.pids["dispatch"]; pid > 0 {
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
			m.GOMAXPROCS = "unset"
			for kv := range strings.SplitSeq(string(b), "\x00") {
				if v, ok := strings.CutPrefix(kv, "GOMAXPROCS="); ok {
					m.GOMAXPROCS = v
				}
			}
		}
	}
	pgCtx, pgCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pgCancel()
	if v, err := postgresVersion(pgCtx, cfg.ep.pg); err != nil {
		m.PostgresVersionError = err.Error()
		lg.Printf("meta: postgres version: %v", err)
	} else {
		m.PostgresVersion = v
	}
	if b, err := os.ReadFile(filepath.Join(loadtestRoot(), "seed.meta.json")); err == nil && json.Valid(b) {
		m.SeedMeta = b
	}
	return m
}

// postgresVersion reads select version() on the Postgres at url.
func postgresVersion(ctx context.Context, url string) (string, error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.Background())
	var v string
	err = conn.QueryRow(ctx, "select version()").Scan(&v)
	return v, err
}
