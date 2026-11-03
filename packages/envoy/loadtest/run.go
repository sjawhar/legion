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
	live         int
	// dashboard is the dashboard stream the build under test serves, probed once before the
	// window (probeDashboard): dashboardLive or dashboardSSE.
	dashboard string
	rngSeed   uint64
	natsConns int
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
	// copyMeta is the -copy-meta file's JSON object, which meta.json records under copy; nil
	// without the flag.
	copyMeta json.RawMessage
	// replay is the replay's plan and roster under -source replay; nil under -source requests.
	replay *replaySetup
}

// sourceName is the request source: the replay when the run has one, else the route mix.
func (c runConfig) sourceName() string {
	if c.replay != nil {
		return sourceReplay
	}
	return sourceRequests
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
	copyMetaPath := fs.String("copy-meta", "", "JSON object file that describes the copy a foreign run measures, recorded verbatim in meta.json under copy")
	delivery := fs.String("session-delivery", envOr("LOADTEST_SESSION_DELIVERY", sessionDeliveryCore), "how the sessions take their messages: core (their core subscriptions) or consumer (the listener's relay of their consumers); default $LOADTEST_SESSION_DELIVERY, else core")
	source := fs.String("source", sourceRequests, "request source: "+sourceRequests+" (the route mix) or "+sourceReplay+" (a replay bundle's logged writes, replay_plan.go)")
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
	var copyMeta json.RawMessage
	if *copyMetaPath != "" {
		if copyMeta, err = readCopyMeta(*copyMetaPath); err != nil {
			return err
		}
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
		if *live < 0 {
			*live = 2
		}
	} else {
		if *seedPath == "" {
			*seedPath = filepath.Join(loadtestRoot(), "seed.json")
		}
		if sf, err = readSeed(*seedPath); err != nil {
			return err
		}
		if err := checkSeedMix(sf, mix, *seedPath); err != nil {
			return err
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
		copyMeta: copyMeta, replay: rs,
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
	route  string
	status int
	// code is the error code of a non-2xx answer's body ({"code": ...}); "" for none.
	code string
	// tolerated says the action expected this non-2xx answer (the CLI's own 404 on an absent
	// optional resource): it is no failure.
	tolerated bool
	transport bool
	latency   time.Duration // arrival (or start, when the write never completed) to response read
	connWait  time.Duration
}

type routeSummary struct {
	Count    int `json:"count"`
	Failures int `json:"failures"` // non-2xx or transport error, tolerated answers apart
	// Tolerated counts the non-2xx answers the action expected (reqRec.tolerated).
	Tolerated int            `json:"tolerated,omitempty"`
	LatencyMs stat           `json:"latency_ms"`
	Status    map[string]int `json:"status"`
	// Codes counts the non-2xx answers by "<status> <code>", the code read from the error body
	// ("<status>" alone when the body named none).
	Codes           map[string]int `json:"codes,omitempty"`
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
	Step         string  `json:"step"`
	EventsPerS   float64 `json:"events_per_s"`
	Sessions     int     `json:"sessions"`
	RequestsPerS float64 `json:"requests_per_s"`
	Live         int     `json:"live"`
	// Dashboard is the dashboard stream those live streams ran on: live (one socket per tab on
	// /ws/live) or sse (GET /api/v1/events, a build before that route was removed). The harness
	// probes the build under test once before the window (probeDashboard).
	Dashboard string   `json:"dashboard"`
	EventMix  eventMix `json:"event_mix"`
	RouteMix  string   `json:"route_mix"`
	Human     float64  `json:"human"`
	WarmupS   float64  `json:"warmup_s"`
	DurationS float64  `json:"duration_s"`
	GraceS    float64  `json:"grace_s"`
	RNGSeed   uint64   `json:"rng_seed"`
	NATSConns int      `json:"nats_conns"`
	// SessionDelivery is the delivery the sessions asked for (-session-delivery): core or
	// consumer. sessions.delivery says how many got consumer delivery.
	SessionDelivery string `json:"session_delivery"`
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
	BoxBusyBefore        *float64 `json:"box_busy_before"`
	BoxBusyBeforeWindowS float64  `json:"box_busy_before_window_s"`
	GOMAXPROCS           string   `json:"gomaxprocs"`
	// PostgresVersion is select version() on -pg, "" when it could not be read, and then
	// PostgresVersionError is why.
	PostgresVersion      string `json:"postgres_version"`
	PostgresVersionError string `json:"postgres_version_error,omitempty"`
	BuildCommit          string `json:"build_commit"`
	BuildDirty           bool   `json:"build_dirty"`
	// DispatchCommit, DispatchBinarySHA256 and DispatchBuild are the Dispatch the step ran, from
	// $LOADTEST_ROOT/bin/envoy-dispatch.json (stack.sh's build record): the commit that binary was
	// built from, which LOADTEST_DISPATCH_COMMIT (--dispatch-commit) sets otherwise than
	// BuildCommit, its sha256, and the whole record. Empty when that file is missing or not JSON.
	DispatchCommit       string          `json:"dispatch_commit"`
	DispatchBinarySHA256 string          `json:"dispatch_binary_sha256"`
	DispatchBuild        json.RawMessage `json:"dispatch_build,omitempty"`
	SeedMeta             json.RawMessage `json:"seed_meta,omitempty"`
	Stack                string          `json:"stack"`
	PGDirect             bool            `json:"pg_direct"`
	AllowForeign         bool            `json:"allow_foreign"`
	PIDs                 map[string]int  `json:"pids"`
	MarkerNonce          string          `json:"marker_nonce"`
	// PGClock is Postgres's clock against loadgen's on -pg, read at the step's start and after its
	// window (readPGClock).
	PGClock *pgClocks `json:"pg_clock,omitempty"`
	// Copy is the -copy-meta file's JSON object, verbatim; absent without the flag.
	Copy json.RawMessage `json:"copy,omitempty"`
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
	lg.Printf("config: step=%s source=%s sessions=%d events=%g/s requests=%g/s event-mix=%s route-mix=%s warmup=%s duration=%s grace=%s human=%.2f live=%d rng-seed=%d session-delivery=%s marker-nonce=%s",
		cfg.name, cfg.sourceName(), cfg.sessions, cfg.events, cfg.requests, cfg.mix.Name, cfg.routes.Source, cfg.warmup, cfg.duration, cfg.grace, cfg.human, cfg.live, cfg.rngSeed, cfg.sessionDelivery, formatNonce(nonce))

	if err := checkStack(ctx, cfg.ep); err != nil {
		return err
	}
	lg.Printf("guard: NATS, Dispatch and the listener are the stack's own (%s)", cfg.ep.stack)
	dashboard, dashURL, err := probeDashboard(ctx, cfg.ep.dispatch)
	if err != nil {
		return err
	}
	cfg.dashboard = dashboard
	lg.Printf("dashboard: %d %s stream(s) on %s", cfg.live, dashboard, redactURL(dashURL))
	meta := collectMeta(ctx, cfg, lg)
	meta.MarkerNonce = formatNonce(nonce)
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, metaMode, lg)
	logSamplerStart(lg, cfg.ep)
	st := newStep(cfg, sf, lg, nonce)
	a := newAPI(cfg.ep.dispatch, cfg.ep.token, 2048)
	if cfg.replay != nil {
		if err := st.prepareReplay(ctx, a); err != nil {
			return err
		}
		meta.Replay = &st.replay.meta
	}

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

	live := st.startDashboard(ctx, dashURL)
	writes, eventsDone := st.runEvents(ctx, newWriter(a, sf, nonce, lg))
	var reqs *requestLoad
	var requestsDone <-chan struct{}
	if cfg.replay != nil {
		reqs, requestsDone = st.runReplay(ctx, a)
	} else {
		reqs, requestsDone = st.runRequests(ctx, a)
	}
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
	end := readPGClock(context.Background(), cfg.ep.pg)
	logPGClock(lg, "end", end)
	meta.PGClock.End = &end
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

// liveSockets is what the dashboard's streams read, live sockets or SSE streams: their bytes and
// the event frames that arrived in the window.
type liveSockets struct {
	bytes  atomic.Int64
	frames *collector[liveFrame]
}

// startDashboard opens the dashboard's streams on url, as many as -live: the live sockets of a
// build that serves /ws/live, else the SSE streams of one that serves GET /api/v1/events
// (probeDashboard). Each stream subscribes to the event feed as a dashboard tab does and
// reconnects a second after it ends, until ctx ends. Every event frame is timed on arrival
// (readLive, readSSE).
func (st *step) startDashboard(ctx context.Context, url string) *liveSockets {
	ls := &liveSockets{frames: newCollector[liveFrame](1 << 14)}
	// A browser offers permessage-deflate, and Dispatch accepts it.
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second, EnableCompression: true}
	client := &http.Client{}
	for i := range st.cfg.live {
		header := http.Header{"X-Dispatch-User": {fmt.Sprintf("viewer-%d", i)}}
		onFrame := func(f liveFrame) {
			if st.inWindow(time.Unix(0, f.at)) {
				ls.frames.add(f)
			}
		}
		onBytes := func(n int64) { ls.bytes.Add(n) }
		if st.cfg.dashboard == dashboardSSE {
			header.Set("Accept", "text/event-stream")
			go followSSE(ctx, client, url, header, st.nonce, time.Second, onFrame, onBytes)
			continue
		}
		go followLive(ctx, dialer, url, header, st.nonce, time.Second, onFrame, onBytes)
	}
	return ls
}

// openLoopPlan walks an open-loop Poisson schedule at rate per second, from stream `stream` of
// rngSeed, without a clock: it draws the next arrival from start until one is at or after winEnd,
// and calls plan with the stream, the arrival and whether it is in the window [winStart, winEnd).
// plan draws what the arrival does from the same stream, so the draws alternate, an arrival then
// its plan, and one seed gives one schedule; it returns false to end the walk. A rate of 0 or
// less plans nothing. The schedule is a pure function of the seed, the stream, the rate and the
// three times, so loadgen plan-docs walks the very schedule a run sends (plandocs.go).
func openLoopPlan(rngSeed, stream uint64, rate float64, start, winStart, winEnd time.Time, plan func(r *rand.Rand, next time.Time, in bool) bool) {
	if rate <= 0 {
		return
	}
	r := rand.New(rand.NewPCG(rngSeed, stream))
	for next := start; ; {
		next = next.Add(time.Duration(r.ExpFloat64() / rate * float64(time.Second)))
		if !next.Before(winEnd) || !plan(r, next, inWindow(next, winStart, winEnd)) {
			return
		}
	}
}

// openLoop is openLoopPlan's schedule, sent: it sleeps until each arrival and calls fire with the
// stream and whether the arrival is in the window. It returns when the schedule ends or ctx does.
func openLoop(ctx context.Context, rngSeed, stream uint64, rate float64, start, winStart, winEnd time.Time, fire func(r *rand.Rand, in bool)) {
	openLoopPlan(rngSeed, stream, rate, start, winStart, winEnd, func(r *rand.Rand, next time.Time, in bool) bool {
		if !sleepUntil(ctx, next) {
			return false
		}
		fire(r, in)
		return true
	})
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

// planWrites walks the event generator's schedule without a clock and calls fn with every write
// the step plans and whether its arrival is in the window: the plans runEvents sends, in its
// order, since both walk openLoopPlan's stream 1 and draw each plan with planEvent.
func (st *step) planWrites(fn func(p eventPlan, in bool)) {
	var marker int64
	openLoopPlan(st.cfg.rngSeed, 1, st.cfg.events, st.start, st.winStart, st.winEnd, func(r *rand.Rand, _ time.Time, in bool) bool {
		marker++
		fn(planEvent(r, st.cfg, st.sessions, st.sf, marker), in)
		return true
	})
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
				load.recs.add(reqRec{route: rt.Name, status: res.status, code: res.errorCode(), transport: res.err != nil, latency: time.Duration(res.end - res.arrival()), connWait: res.connWait})
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
// dump and the heap profile are taken once it has finished, outside it, and under -source replay
// after the replay's window-end reads (heap-end.pprof, then Dispatch's /proc read). heap.txt is the
// run's one read of /debug/pprof/heap?debug=1 (the sampler reads the binary profile), for
// codepaths' runtime.MemStats; it has 10 minutes: with a heap of tens of GiB it is over 500 MB,
// and 60 s cut it before its runtime.MemStats block.
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
	fetchTo(pprof+"/debug/pprof/heap?debug=1", filepath.Join(out, "heap.txt"), 10*time.Minute, st.lg)
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
			Dashboard: cfg.dashboard,
			EventMix:  cfg.mix, RouteMix: cfg.routes.Source, Human: cfg.human, WarmupS: cfg.warmup.Seconds(),
			DurationS: durS, GraceS: cfg.grace.Seconds(), RNGSeed: cfg.rngSeed, NATSConns: cfg.natsConns,
			SessionDelivery: cfg.sessionDelivery, MarkerNonce: formatNonce(st.nonce), Source: cfg.sourceName(),
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
	if st.replay != nil {
		sum.Replay = st.replay.record(ss, durS)
	}
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
		switch {
		case !r.transport && r.status/100 == 2:
			out.OK2xx++
		case !r.transport && r.tolerated:
			rs.Tolerated++
		default:
			rs.Failures++
		}
		if !r.transport && r.status/100 != 2 {
			if rs.Codes == nil {
				rs.Codes = map[string]int{}
			}
			rs.Codes[strings.TrimSpace(strconv.Itoa(r.status)+" "+r.code)]++
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
	start := readPGClock(ctx, cfg.ep.pg)
	logPGClock(lg, "start", start)
	m.PGClock = &pgClocks{Start: &start}
	m.Copy = cfg.copyMeta
	if b, err := os.ReadFile(filepath.Join(loadtestRoot(), "seed.meta.json")); err == nil && json.Valid(b) {
		m.SeedMeta = b
	}
	// The Dispatch binary the step ran, which is this checkout's build only when the step set no
	// LOADTEST_DISPATCH_COMMIT.
	if bld, ok := readDispatchBuild(loadtestRoot()); ok {
		m.DispatchCommit, m.DispatchBinarySHA256, m.DispatchBuild = bld.commit, bld.binarySHA256, bld.raw
	} else {
		lg.Printf("meta: no Dispatch build record at %s", filepath.Join(loadtestRoot(), "bin", "envoy-dispatch.json"))
	}
	return m
}

// dispatchBuild is $LOADTEST_ROOT/bin/envoy-dispatch.json, stack.sh's record of the Dispatch binary
// the stack runs: dispatch_commit, dispatch_tree, build_dirty, patch, patch_sha256,
// dispatch_pprof_overlay, dispatch_binary_sha256, go_version and newest_migration.
type dispatchBuild struct {
	raw          json.RawMessage
	commit       string
	binarySHA256 string
}

// readDispatchBuild reads bin/envoy-dispatch.json under root; ok is false when it is absent or
// holds no JSON.
func readDispatchBuild(root string) (dispatchBuild, bool) {
	b, err := os.ReadFile(filepath.Join(root, "bin", "envoy-dispatch.json"))
	if err != nil || !json.Valid(b) {
		return dispatchBuild{}, false
	}
	var r struct {
		Commit       string `json:"dispatch_commit"`
		BinarySHA256 string `json:"dispatch_binary_sha256"`
	}
	_ = json.Unmarshal(b, &r)
	return dispatchBuild{raw: b, commit: r.Commit, binarySHA256: r.BinarySHA256}, true
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
