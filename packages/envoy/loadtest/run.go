package main

import (
	"context"
	"encoding/json"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// maxRequestsInFlight caps the request generator: above 600 req/s times the 30 s client
// timeout. A request the cap refuses is counted as dropped, never sent.
const maxRequestsInFlight = 25000

// The request sources: the route mix (runRequests) or a replay bundle's logged writes (runReplay).
const (
	sourceRequests = "requests"
	sourceReplay   = "replay"
)

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
	sse          int
	rngSeed      uint64
	natsConns    int
	buildCommit  string
	buildDirty   bool
	// boxBusyBefore is the box's busy fraction read before Dispatch started (run-step.sh reads it
	// with `loadgen box-busy` before its reset); negative means collectMeta reads it itself.
	boxBusyBefore float64
	args          []string
	// source is the request source; replay is the replay's plan and roster under -source replay.
	source string
	replay *replaySetup
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
	sse := fs.Int("sse", -1, "dashboard SSE streams (default max(1, round(requests/30)))")
	rngSeed := fs.Uint64("rng-seed", 1, "seed of the event and request schedules")
	natsConns := fs.Int("nats-conns", 50, "NATS connections the sessions share")
	out := fs.String("out", "", "output directory (required)")
	name := fs.String("name", "", "step name (default: the -out directory's name)")
	seedPath := fs.String("seed", "", "seed.json path (default $LOADTEST_ROOT/seed.json)")
	buildCommit := fs.String("build-commit", "", "commit Dispatch was built from, for meta.json")
	buildDirty := fs.Bool("build-dirty", false, "the build's tree had uncommitted changes, for meta.json")
	boxBusyBefore := fs.Float64("box-busy-before", -1, "the box's busy fraction read before Dispatch started (loadgen box-busy); default: read it now")
	source := fs.String("source", sourceRequests, "request source: "+sourceRequests+" (the route mix) or "+sourceReplay+" (a replay bundle's logged writes, replay_source.go)")
	rf := registerReplayFlags(fs)
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
	replay := *source == sourceReplay
	if !replay && *source != sourceRequests {
		return fmt.Errorf("-source must be %s or %s, not %q", sourceRequests, sourceReplay, *source)
	}
	var replayOnly []string
	fs.Visit(func(f *flag.Flag) {
		if strings.HasPrefix(f.Name, "replay-") {
			replayOnly = append(replayOnly, "-"+f.Name)
		}
	})
	switch {
	case *events < 0 || !finite(*events):
		return fmt.Errorf("-events is required and must be 0 or more")
	case *requests < 0 || !finite(*requests):
		return fmt.Errorf("-requests is required and must be 0 or more")
	case replay && (*events != 0 || *requests != 0):
		return fmt.Errorf("-source replay sends the replay alone: -events 0 and -requests 0 are required")
	case replay && *sessions != -1:
		return fmt.Errorf("-sessions does not apply to -source replay: -replay-roster sets the sessions")
	case !replay && len(replayOnly) > 0:
		return fmt.Errorf("%s applies only to -source replay", strings.Join(replayOnly, ", "))
	case !replay && *sessions < 1:
		return fmt.Errorf("-sessions is required and must be at least 1")
	case *human < 0 || *human > 1 || !finite(*human):
		return fmt.Errorf("-human must be between 0 and 1")
	case *warmup < 0 || *grace < 0 || *duration <= 0:
		return fmt.Errorf("-warmup and -grace must be 0 or more, -duration more than 0")
	case *natsConns < 1:
		return fmt.Errorf("-nats-conns must be at least 1")
	case *sse < -1:
		return fmt.Errorf("-sse must be 0 or more")
	case *boxBusyBefore > 1 || !finite(*boxBusyBefore):
		return fmt.Errorf("-box-busy-before must be a fraction between 0 and 1")
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	var sf seedFile
	var rs *replaySetup
	if replay {
		// The window is the sub-window; the roster is the sessions; seed.json is not read.
		o, err := rf.options(*rngSeed)
		if err != nil {
			return err
		}
		if rs, err = newReplaySetup(o); err != nil {
			return err
		}
		*duration = o.to - o.from
		*sessions = len(rs.roster)
		if *sse < 0 {
			*sse = 2
		}
	} else {
		if *seedPath == "" {
			*seedPath = filepath.Join(loadtestRoot(), "seed.json")
		}
		if sf, err = readSeed(*seedPath); err != nil {
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
	}
	if *sse < 0 {
		*sse = max(1, int(math.Round(*requests/30)))
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
		human: *human, sse: *sse, rngSeed: *rngSeed, natsConns: *natsConns,
		buildCommit: *buildCommit, buildDirty: *buildDirty, boxBusyBefore: *boxBusyBefore, args: args,
		source: *source, replay: rs,
	}, sf)
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
	SSE          int      `json:"sse"`
	EventMix     eventMix `json:"event_mix"`
	RouteMix     string   `json:"route_mix"`
	Human        float64  `json:"human"`
	WarmupS      float64  `json:"warmup_s"`
	DurationS    float64  `json:"duration_s"`
	GraceS       float64  `json:"grace_s"`
	RNGSeed      uint64   `json:"rng_seed"`
	NATSConns    int      `json:"nats_conns"`
	// MarkerNonce is the run's marker nonce (markerTag): every marker the run wrote starts with it,
	// and the run counts only the commit rows that carry it.
	MarkerNonce string `json:"marker_nonce"`
	// Source is the request source: requests or replay.
	Source string `json:"source,omitempty"`
}

type sessionsSummary struct {
	Registered        int   `json:"registered"`
	RegisterErrors    int   `json:"register_errors"`
	HeartbeatErrors   int64 `json:"heartbeat_errors"`
	NATSSubscriptions int   `json:"nats_subscriptions"`
	NATSConnections   int   `json:"nats_connections"`
}

type summary struct {
	Config          summaryConfig     `json:"config"`
	Events          eventsSummary     `json:"events"`
	Requests        requestsSummary   `json:"requests"`
	Resources       *resourcesSummary `json:"resources"`
	CommitOrderLock lockSummary       `json:"commit_order_lock"`
	UngrantedLocks  ungrantedSummary  `json:"ungranted_locks"`
	Sessions        sessionsSummary   `json:"sessions"`
	SSE             sseSummary        `json:"sse"`
	SSEBytes        int64             `json:"sse_bytes"`
	// StackExits are the stack processes that exited during the step; such a step fails and its
	// row does not measure the load.
	StackExits []stackExit `json:"stack_exits,omitempty"`
	// Replay is the replay's record under -source replay (replay_record.go).
	Replay *replayRecord `json:"replay,omitempty"`
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
	BoxBusyBefore        *float64        `json:"box_busy_before"`
	BoxBusyBeforeWindowS float64         `json:"box_busy_before_window_s"`
	GOMAXPROCS           string          `json:"gomaxprocs"`
	PostgresVersion      string          `json:"postgres_version"`
	BuildCommit          string          `json:"build_commit"`
	BuildDirty           bool            `json:"build_dirty"`
	SeedMeta             json.RawMessage `json:"seed_meta,omitempty"`
	Stack                string          `json:"stack"`
	PGDirect             bool            `json:"pg_direct"`
	AllowForeign         bool            `json:"allow_foreign"`
	PIDs                 map[string]int  `json:"pids"`
	MarkerNonce          string          `json:"marker_nonce"`
	// Replay is the replay's inputs, build and heap-profile times under -source replay.
	Replay *replayMetaRecord `json:"replay,omitempty"`
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
	lg.Printf("config: step=%s source=%s sessions=%d events=%g/s requests=%g/s event-mix=%s route-mix=%s warmup=%s duration=%s grace=%s human=%.2f sse=%d rng-seed=%d marker-nonce=%s",
		cfg.name, cfg.source, cfg.sessions, cfg.events, cfg.requests, cfg.mix.Name, cfg.routes.Source, cfg.warmup, cfg.duration, cfg.grace, cfg.human, cfg.sse, cfg.rngSeed, formatNonce(nonce))

	if err := checkStack(ctx, cfg.ep); err != nil {
		return err
	}
	lg.Printf("guard: NATS, Dispatch and the listener are the stack's own (%s)", cfg.ep.stack)
	meta := collectMeta(ctx, cfg, lg)
	meta.MarkerNonce = formatNonce(nonce)
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, lg)
	logSamplerStart(lg, cfg.ep)
	st := newStep(cfg, sf, lg, nonce)
	if cfg.replay != nil {
		if err := st.prepareReplay(ctx); err != nil {
			return err
		}
		meta.Replay = &st.replay.meta
	}

	ns, err := st.subscribeSessions()
	defer ns.close()
	if err != nil {
		return err
	}
	reg := st.registerSessions(ctx)
	st.startHeartbeat(ctx, reg)

	st.openWindow(time.Now())
	meta.Start, meta.WindowStart, meta.WindowEnd = st.start, st.winStart, st.winEnd
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, lg)
	lg.Printf("window %s to %s", st.winStart.UTC().Format(time.RFC3339), st.winEnd.UTC().Format(time.RFC3339))

	a := newAPI(cfg.ep.dispatch, cfg.ep.token, 2048)
	sse := st.startSSE(ctx)
	writes, eventsDone := st.runEvents(ctx, newWriter(a, sf, nonce, lg))
	var reqs *requestLoad
	var requestsDone <-chan struct{}
	if cfg.replay != nil {
		reqs, requestsDone = st.runReplay(ctx, a)
	} else {
		reqs, requestsDone = st.runRequests(ctx, a)
	}
	pr := st.startProbes(ctx)

	<-eventsDone
	<-requestsDone
	lg.Printf("generators done at %s; grace %s", time.Since(st.start).Round(time.Second), cfg.grace)
	time.Sleep(cfg.grace)
	pr.wait()
	cancel()
	ns.close()
	ns.envelopes.write(cfg.out, lg)

	sum := st.buildSummary(ns, reg, writes, reqs, sse, pr)
	writeJSONFile(filepath.Join(cfg.out, "summary.json"), sum, lg)
	meta.End = time.Now()
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, lg)
	lg.Printf("step %s done: events offered %d committed %d, receipts %d of %d, requests sent %d (%.1f/s of %g)",
		cfg.name, sum.Events.Offered, sum.Events.Committed, sum.Events.IssueTopicReceipts, sum.Events.ExpectedIssueTopicReceipts,
		sum.Requests.Sent, sum.Requests.SentPerS, cfg.requests)
	if err := errStackExits(sum.StackExits); err != nil {
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
	// replay is the replay's run state under -source replay.
	replay *replayRun
}

// newStep lays out the sessions: session i works on issue i (mod the issues in use) and watches
// four others, so it comments on other sessions' documents. The seed routed issue i to session i.
func newStep(cfg runConfig, sf seedFile, lg *log.Logger, nonce int64) *step {
	if cfg.replay != nil {
		return newReplayStep(cfg, lg, nonce)
	}
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

// sseStreams is what the dashboard SSE streams read: their bytes and the frames that arrived in
// the window.
type sseStreams struct {
	bytes  atomic.Int64
	frames *collector[sseFrame]
}

// startSSE opens the dashboard SSE streams, each reconnecting a second after its stream ends,
// until ctx ends. Every data frame is timed on arrival (readSSE).
func (st *step) startSSE(ctx context.Context) *sseStreams {
	ss := &sseStreams{frames: newCollector[sseFrame](1 << 14)}
	client := &http.Client{}
	for i := range st.cfg.sse {
		go func() {
			for ctx.Err() == nil {
				req, _ := http.NewRequestWithContext(ctx, "GET", st.cfg.ep.dispatch+"/api/v1/events", nil)
				req.Header.Set("X-Dispatch-User", fmt.Sprintf("viewer-%d", i))
				req.Header.Set("Accept", "text/event-stream")
				if resp, err := client.Do(req); err == nil {
					n, _ := readSSE(resp.Body, st.nonce, time.Now, func(f sseFrame) {
						if st.inWindow(time.Unix(0, f.at)) {
							ss.frames.add(f)
						}
					})
					ss.bytes.Add(n)
					_ = resp.Body.Close()
				}
				sleepUntil(ctx, time.Now().Add(time.Second))
			}
		}()
	}
	return ss
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
	lp       *lockProbe
	ls       *lockSampler
	xidsPerS *float64
	wg       sync.WaitGroup // every probe but the sampler, which runs until ctx ends
}

// startProbes starts the probes. The two lock samplers run from 20 s into the window to its end.
func (st *step) startProbes(ctx context.Context) *probes {
	p := &probes{smp: newSampler(st.cfg.ep), lp: &lockProbe{}, ls: &lockSampler{}}
	go p.smp.loop(ctx, st.winStart, st.winEnd)
	lockFrom := st.winStart.Add(20 * time.Second)
	if lockFrom.After(st.winEnd) {
		lockFrom = st.winStart
	}
	pg := st.cfg.ep.pg
	p.wg.Go(func() { p.lp.loop(ctx, pg, lockFrom, st.winEnd, st.lg) })
	p.wg.Go(func() { p.ls.loop(ctx, pg, lockFrom, st.winEnd, st.lg) })
	p.wg.Go(func() { p.xidsPerS = xidRate(ctx, pg, st.winStart, st.winEnd, st.lg) })
	p.wg.Go(func() { st.profile(ctx) })
	return p
}

// wait checks once more, after the grace, that every stack process is alive, then waits for the
// probes and the profiles.
func (p *probes) wait() {
	p.smp.checkAlive(time.Now())
	p.wg.Wait()
}

// profile fetches Dispatch's profiles. The CPU profile covers the whole window; the goroutine
// dump and the heap profile are taken once it has finished, outside it, and under -source replay
// after the replay's window-end reads (heap-end.pprof, then Dispatch's /proc read).
func (st *step) profile(ctx context.Context) {
	if !sleepUntil(ctx, st.winStart) {
		return
	}
	pprof, out := st.cfg.ep.pprof, st.cfg.out
	secs := int(math.Ceil(st.cfg.duration.Seconds()))
	fetchTo(pprof+"/debug/pprof/profile?seconds="+strconv.Itoa(secs), filepath.Join(out, "cpu.pprof"), st.cfg.duration+60*time.Second, st.lg)
	if st.replay != nil {
		select {
		case <-st.replay.endRead:
		case <-ctx.Done():
			return
		}
	}
	fetchTo(pprof+"/debug/pprof/goroutine?debug=2", filepath.Join(out, "goroutines.txt"), 120*time.Second, st.lg)
	fetchTo(pprof+"/debug/pprof/heap", filepath.Join(out, "heap.pprof"), 60*time.Second, st.lg)
	fetchTo(pprof+"/debug/pprof/heap?debug=1", filepath.Join(out, "heap.txt"), 60*time.Second, st.lg)
}

// buildSummary joins what the phases recorded into summary.json, and writes samples.json. It
// reads the run's commit rows and names the ungranted locks' relations, so it runs after the load.
func (st *step) buildSummary(ns *natsSessions, reg *registrar, writes *collector[writeRec], reqs *requestLoad, sse *sseStreams, pr *probes) summary {
	cfg := st.cfg
	ws := writes.snapshot()
	frames := sse.frames.snapshot()
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
			Step: cfg.name, EventsPerS: cfg.events, Sessions: cfg.sessions, RequestsPerS: cfg.requests, SSE: cfg.sse,
			EventMix: cfg.mix, RouteMix: cfg.routes.Source, Human: cfg.human, WarmupS: cfg.warmup.Seconds(),
			DurationS: durS, GraceS: cfg.grace.Seconds(), RNGSeed: cfg.rngSeed, NATSConns: cfg.natsConns,
			MarkerNonce: formatNonce(st.nonce), Source: cfg.source,
		},
		Events:   joinDelivery(ws, commits, ns.receipts.snapshot(), watcherCount, durS),
		Requests: summarizeRequests(reqs.recs.snapshot(), int(reqs.dropped.Load()), cfg.requests, durS),
		Sessions: sessionsSummary{
			Registered: int(reg.ok.Load()), RegisterErrors: int(reg.errs.Load()), HeartbeatErrors: reg.heartbeatErrs.Load(),
			NATSSubscriptions: ns.subs, NATSConnections: len(ns.conns),
		},
		CommitOrderLock: summarizeLock(pr.lp.samples, pr.lp.errors),
		UngrantedLocks:  summarizeUngranted(pr.ls.samples, pr.ls.errors),
		SSE:             summarizeSSE(frames, ws, commits),
		SSEBytes:        sse.bytes.Load(),
	}
	if commitsErr != nil {
		sum.Events.CommitsError = commitsErr.Error()
		st.lg.Printf("commits: %v", commitsErr)
	}
	ss := pr.smp.all()
	writeJSONFile(filepath.Join(cfg.out, "samples.json"), ss, st.lg)
	sum.Resources = summarizeResources(ss, sum.Events.Committed+sum.Requests.Sent, durS)
	if sum.Resources != nil {
		sum.Resources.XIDsPerS = pr.xidsPerS
	}
	sum.StackExits = pr.smp.stackExits()
	if st.replay != nil {
		sum.Replay = st.replay.record(ss, durS)
	}
	return sum
}

// stackExitsError fails a step in which stack processes exited.
type stackExitsError struct {
	Exits []stackExit
}

func (e *stackExitsError) Error() string {
	gone := make([]string, len(e.Exits))
	for i, x := range e.Exits {
		gone[i] = fmt.Sprintf("%s (pid %d) exited during the step (first seen gone at %s)", x.Name, x.PID, x.FirstSeenGone.UTC().Format(time.RFC3339))
	}
	return strings.Join(gone, ", ") + "; this row does not measure the load"
}

// errStackExits is the step's failure when a stack process exited during it, nil otherwise.
func errStackExits(exits []stackExit) error {
	if len(exits) == 0 {
		return nil
	}
	return &stackExitsError{Exits: exits}
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
	if conn, err := pgx.Connect(pgCtx, cfg.ep.pg); err == nil {
		if err := conn.QueryRow(pgCtx, "select version()").Scan(&m.PostgresVersion); err != nil {
			lg.Printf("meta: postgres version: %v", err)
		}
		conn.Close(context.Background())
	} else {
		lg.Printf("meta: postgres: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(loadtestRoot(), "seed.meta.json")); err == nil && json.Valid(b) {
		m.SeedMeta = b
	}
	return m
}
