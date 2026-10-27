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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
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
	sse          int
	rngSeed      uint64
	natsConns    int
	buildCommit  string
	buildDirty   bool
	args         []string
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
	mixFlag := fs.String("event-mix", defaultEventMix, "event mix: a preset (production-2026-10-26, report-2026-10-27) or kind=weight,...")
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
	finite := func(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
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
	case *sse < -1:
		return fmt.Errorf("-sse must be 0 or more")
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
		buildCommit: *buildCommit, buildDirty: *buildDirty, args: args,
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
	Sessions        sessionsSummary   `json:"sessions"`
	SSEBytes        int64             `json:"sse_bytes"`
}

type metaFile struct {
	Step            string          `json:"step"`
	Args            []string        `json:"args"`
	Start           time.Time       `json:"start"`
	WindowStart     time.Time       `json:"window_start"`
	WindowEnd       time.Time       `json:"window_end"`
	End             time.Time       `json:"end,omitzero"`
	Nproc           int             `json:"nproc"`
	CPUModel        string          `json:"cpu_model"`
	Kernel          string          `json:"kernel"`
	LoadavgBefore   string          `json:"loadavg_before"`
	GOMAXPROCS      string          `json:"gomaxprocs"`
	PostgresVersion string          `json:"postgres_version"`
	BuildCommit     string          `json:"build_commit"`
	BuildDirty      bool            `json:"build_dirty"`
	SeedMeta        json.RawMessage `json:"seed_meta,omitempty"`
	Stack           string          `json:"stack"`
	PGDirect        bool            `json:"pg_direct"`
	AllowForeign    bool            `json:"allow_foreign"`
	PIDs            map[string]int  `json:"pids"`
}

var markerPattern = regexp.MustCompile(`lgm[0-9]+x`)

func execute(cfg runConfig, sf seedFile) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logf, err := os.Create(filepath.Join(cfg.out, "run.log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	lg := log.New(io.MultiWriter(os.Stderr, logf), "", log.LstdFlags|log.Lmicroseconds)
	lg.Printf("config: step=%s sessions=%d events=%g/s requests=%g/s event-mix=%s route-mix=%s warmup=%s duration=%s grace=%s human=%.2f sse=%d rng-seed=%d",
		cfg.name, cfg.sessions, cfg.events, cfg.requests, cfg.mix.Name, cfg.routes.Source, cfg.warmup, cfg.duration, cfg.grace, cfg.human, cfg.sse, cfg.rngSeed)

	if err := checkStack(ctx, cfg.ep); err != nil {
		return err
	}
	lg.Printf("guard: NATS, Dispatch and the listener are the stack's own (%s)", cfg.ep.stack)
	meta := collectMeta(ctx, cfg, lg)
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, lg)
	logSamplerStart(lg, cfg.ep)
	a := newAPI(cfg.ep.dispatch, cfg.ep.token, 2048)

	// Topology: session i works on issue i (mod the issues in use) and watches four others, so
	// it comments on other sessions' documents. The seed routed issue i to session i.
	active := min(cfg.sessions, len(sf.Issues))
	sessions := make([]session, cfg.sessions)
	watchers := make([][]int, len(sf.Issues))
	for i := range sessions {
		s := session{id: sessionID(i), home: i % active}
		s.watched = []int{s.home}
		for k := 1; k <= 4; k++ {
			s.watched = append(s.watched, (s.home+k*37)%active)
		}
		for _, w := range s.watched {
			watchers[w] = append(watchers[w], i)
		}
		sessions[i] = s
	}

	// NATS: sessions share natsConns connections; each subscribes its own agent topic and its
	// watched issues' topics, as the pi plugin's self-subscription does.
	var recvMu sync.Mutex
	receipts := make([]receipt, 0, 1<<16)
	var sampleMu sync.Mutex
	envelopes := map[string][]byte{}
	var conns []*nats.Conn
	defer func() {
		for _, nc := range conns {
			nc.Close()
		}
	}()
	for i := range cfg.natsConns {
		nc, err := nats.Connect(cfg.ep.nats, nats.Name(fmt.Sprintf("loadgen-%d", i)), nats.MaxReconnects(-1))
		if err != nil {
			return fmt.Errorf("nats connect: %w", err)
		}
		conns = append(conns, nc)
	}
	handler := func(agent bool) nats.MsgHandler {
		return func(m *nats.Msg) {
			now := time.Now().UnixNano()
			var env contracts.Envelope
			if json.Unmarshal(m.Data, &env) != nil {
				return
			}
			var ev struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(env.Payload), &ev) != nil {
				return
			}
			key := ev.Type
			if agent {
				key = "agent." + ev.Type
			}
			// Keep one envelope per type, preferring one whose payload carries a marker.
			sampleMu.Lock()
			if old, ok := envelopes[key]; !ok || (!markerPattern.Match(old) && markerPattern.MatchString(env.Payload)) {
				envelopes[key] = append([]byte(nil), m.Data...)
			}
			sampleMu.Unlock()
			recvMu.Lock()
			receipts = append(receipts, receipt{EventID: ev.ID, Recv: now, Agent: agent})
			recvMu.Unlock()
		}
	}
	subs := 0
	issueTopic := func(i int) string { return "notifications.dispatch.issue." + sf.Issues[i].Key + ".>" }
	for i, s := range sessions {
		nc := conns[i%len(conns)]
		if _, err := nc.Subscribe(contracts.AgentSubject(s.id), handler(true)); err != nil {
			return err
		}
		subs++
		for _, w := range s.watched {
			if _, err := nc.Subscribe(issueTopic(w), handler(false)); err != nil {
				return err
			}
			subs++
		}
	}
	for _, nc := range conns {
		_ = nc.Flush()
	}
	lg.Printf("nats: %d subscriptions on %d connections", subs, len(conns))

	// Listener registration as the pi plugin does it, and the 90 s heartbeat spread evenly
	// (packages/envoy-client/src/envoy-settings.ts).
	lc := &http.Client{Timeout: 10 * time.Second}
	register := func(s session) error {
		topics := []string{}
		for _, w := range s.watched {
			topics = append(topics, issueTopic(w))
		}
		body, _ := json.Marshal(map[string]any{"session_id": s.id, "dir": "/work/" + s.id, "title": "load " + s.id,
			"topics": topics, "port": 0, "self_subscribed": true, "driving": false, "capabilities": []string{}})
		req, _ := http.NewRequestWithContext(ctx, "POST", cfg.ep.listener+"/v1/interests/subscribe", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.ep.listenerToken)
		resp, err := lc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("%d %s", resp.StatusCode, trunc(b))
		}
		return nil
	}
	var regOK, regErr atomic.Int64
	{
		var wg sync.WaitGroup
		sem := make(chan struct{}, 16)
		for _, s := range sessions {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if err := register(s); err != nil {
					if regErr.Add(1) <= 5 {
						lg.Printf("register %s: %v", s.id, err)
					}
					return
				}
				regOK.Add(1)
			}()
		}
		wg.Wait()
		lg.Printf("registered %d sessions (%d errors)", regOK.Load(), regErr.Load())
	}
	var hbErr atomic.Int64
	go func() {
		t := time.NewTicker(90 * time.Second / time.Duration(len(sessions)))
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				go func(s session) {
					if err := register(s); err != nil && ctx.Err() == nil {
						if hbErr.Add(1) <= 5 {
							lg.Printf("heartbeat %s: %v", s.id, err)
						}
					}
				}(sessions[i%len(sessions)])
			}
		}
	}()

	// Dashboard SSE streams.
	var sseBytes atomic.Int64
	sseClient := &http.Client{}
	for i := range cfg.sse {
		go func() {
			for ctx.Err() == nil {
				req, _ := http.NewRequestWithContext(ctx, "GET", cfg.ep.dispatch+"/api/v1/events", nil)
				req.Header.Set("X-Dispatch-User", fmt.Sprintf("viewer-%d", i))
				req.Header.Set("Accept", "text/event-stream")
				if resp, err := sseClient.Do(req); err == nil {
					n, _ := io.Copy(io.Discard, resp.Body)
					sseBytes.Add(n)
					_ = resp.Body.Close()
				}
				sleepUntil(ctx, time.Now().Add(time.Second))
			}
		}()
	}

	// The measured window.
	start := time.Now()
	winStart := start.Add(cfg.warmup)
	winEnd := winStart.Add(cfg.duration)
	inWindow := func(t time.Time) bool { return !t.Before(winStart) && t.Before(winEnd) }
	meta.Start, meta.WindowStart, meta.WindowEnd = start, winStart, winEnd
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, lg)
	lg.Printf("window %s to %s", winStart.UTC().Format(time.RFC3339), winEnd.UTC().Format(time.RFC3339))

	// Event generator: open-loop Poisson from its own deterministic stream.
	var wmu sync.Mutex
	writes := make([]writeRec, 0, 1<<14)
	wr := newWriter(a, sf, lg)
	genDone := make(chan struct{})
	go func() {
		defer close(genDone)
		if cfg.events <= 0 {
			return
		}
		r := rand.New(rand.NewPCG(cfg.rngSeed, 1))
		next := start
		var marker int64
		var wg sync.WaitGroup
		defer wg.Wait()
		for {
			next = next.Add(time.Duration(r.ExpFloat64() / cfg.events * float64(time.Second)))
			if !next.Before(winEnd) {
				return
			}
			marker++
			p := planEvent(r, cfg, sessions, sf, marker, next)
			if !sleepUntil(ctx, next) {
				return
			}
			in := inWindow(next)
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := wr.do(ctx, p)
				rec.InWindow = in
				wmu.Lock()
				writes = append(writes, rec)
				wmu.Unlock()
			}()
		}
	}()

	// Request generator: open-loop Poisson at the route mix, from its own deterministic stream.
	var rmu sync.Mutex
	reqs := make([]reqRec, 0, 1<<16)
	var reqInflight, reqDropped atomic.Int64
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		if cfg.requests <= 0 {
			return
		}
		r := rand.New(rand.NewPCG(cfg.rngSeed, 2))
		next := start
		var wg sync.WaitGroup
		defer wg.Wait()
		for {
			next = next.Add(time.Duration(r.ExpFloat64() / cfg.requests * float64(time.Second)))
			if !next.Before(winEnd) {
				return
			}
			rt := cfg.routes.pick(r.Float64())
			path := rt.render(sf.Issues[r.IntN(active)], sf.Projects[r.IntN(len(sf.Projects))])
			id := who{}
			if rt.Person {
				id.person = fmt.Sprintf("person-%d", r.IntN(7))
			}
			if !sleepUntil(ctx, next) {
				return
			}
			in := inWindow(next)
			if reqInflight.Load() >= maxRequestsInFlight {
				if in {
					reqDropped.Add(1)
				}
				continue
			}
			wg.Add(1)
			reqInflight.Add(1)
			go func() {
				defer wg.Done()
				defer reqInflight.Add(-1)
				res := a.call(ctx, "GET", path, id, nil)
				if !in {
					return
				}
				rec := reqRec{route: rt.Name, status: res.status, transport: res.err != nil, latency: time.Duration(res.end - res.arrival()), connWait: res.connWait}
				rmu.Lock()
				reqs = append(reqs, rec)
				rmu.Unlock()
			}()
		}
	}()

	// Sampler, lock probe and profiles.
	smp := newSampler(cfg.ep)
	go smp.loop(ctx, winStart, winEnd)
	lp := &lockProbe{}
	lpDone := make(chan struct{})
	go func() {
		defer close(lpDone)
		from := winStart.Add(20 * time.Second)
		if from.After(winEnd) {
			from = winStart
		}
		lp.loop(ctx, cfg.ep.pg, from, winEnd, lg)
	}()
	profDone := make(chan struct{})
	go func() {
		// The CPU profile covers the whole window; the goroutine dump and the heap profile are
		// taken once it has finished, outside it.
		defer close(profDone)
		if !sleepUntil(ctx, winStart) {
			return
		}
		secs := int(math.Ceil(cfg.duration.Seconds()))
		fetchTo(cfg.ep.pprof+"/debug/pprof/profile?seconds="+strconv.Itoa(secs), filepath.Join(cfg.out, "cpu.pprof"), cfg.duration+60*time.Second, lg)
		fetchTo(cfg.ep.pprof+"/debug/pprof/goroutine?debug=2", filepath.Join(cfg.out, "goroutines.txt"), 120*time.Second, lg)
		fetchTo(cfg.ep.pprof+"/debug/pprof/heap", filepath.Join(cfg.out, "heap.pprof"), 60*time.Second, lg)
		fetchTo(cfg.ep.pprof+"/debug/pprof/heap?debug=1", filepath.Join(cfg.out, "heap.txt"), 60*time.Second, lg)
	}()

	<-genDone
	<-reqDone
	lg.Printf("generators done at %s; grace %s", time.Since(start).Round(time.Second), cfg.grace)
	time.Sleep(cfg.grace)
	<-profDone
	<-lpDone
	cancel()
	for _, nc := range conns {
		nc.Close()
	}

	sampleMu.Lock()
	for key, data := range envelopes {
		name := "envelope-" + strings.Map(func(r rune) rune {
			if r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return '_'
		}, key) + ".json"
		if err := os.WriteFile(filepath.Join(cfg.out, name), data, 0o644); err != nil {
			lg.Printf("write %s: %v", name, err)
		}
	}
	sampleMu.Unlock()
	recvMu.Lock()
	rc := append([]receipt(nil), receipts...)
	recvMu.Unlock()
	wmu.Lock()
	ws := append([]writeRec(nil), writes...)
	wmu.Unlock()
	rmu.Lock()
	rq := append([]reqRec(nil), reqs...)
	rmu.Unlock()

	var markers []int64
	for _, w := range ws {
		if w.InWindow {
			markers = append(markers, w.Marker)
		}
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), time.Minute)
	commits, commitsErr := readCommits(readCtx, cfg.ep.pg, markers)
	readCancel()
	watcherCount := map[string]int{}
	for i, subscribed := range watchers {
		watcherCount[sf.Issues[i].Key] = len(subscribed)
	}
	durS := cfg.duration.Seconds()
	sum := summary{
		Config: summaryConfig{
			Step: cfg.name, EventsPerS: cfg.events, Sessions: cfg.sessions, RequestsPerS: cfg.requests, SSE: cfg.sse,
			EventMix: cfg.mix, RouteMix: cfg.routes.Source, Human: cfg.human, WarmupS: cfg.warmup.Seconds(),
			DurationS: durS, GraceS: cfg.grace.Seconds(), RNGSeed: cfg.rngSeed, NATSConns: cfg.natsConns,
		},
		Events:   joinDelivery(ws, commits, rc, watcherCount, durS),
		Requests: summarizeRequests(rq, int(reqDropped.Load()), cfg.requests, durS),
		Sessions: sessionsSummary{
			Registered: int(regOK.Load()), RegisterErrors: int(regErr.Load()), HeartbeatErrors: hbErr.Load(),
			NATSSubscriptions: subs, NATSConnections: len(conns),
		},
		CommitOrderLock: summarizeLock(lp.samples),
		SSEBytes:        sseBytes.Load(),
	}
	if commitsErr != nil {
		sum.Events.CommitsError = commitsErr.Error()
		lg.Printf("commits: %v", commitsErr)
	}
	ss := smp.all()
	writeJSONFile(filepath.Join(cfg.out, "samples.json"), ss, lg)
	sum.Resources = summarizeResources(ss, sum.Events.Committed+sum.Requests.Sent, durS)
	writeJSONFile(filepath.Join(cfg.out, "summary.json"), sum, lg)
	meta.End = time.Now()
	writeJSONFile(filepath.Join(cfg.out, "meta.json"), meta, lg)
	lg.Printf("step %s done: events offered %d committed %d, receipts %d of %d, requests sent %d (%.1f/s of %g)",
		cfg.name, sum.Events.Offered, sum.Events.Committed, sum.Events.IssueTopicReceipts, sum.Events.ExpectedIssueTopicReceipts,
		sum.Requests.Sent, sum.Requests.SentPerS, cfg.requests)
	return nil
}

// planEvent draws one write from the event stream: a session, one of its watched issues, a
// person or the session as author, a kind, and the kind's quote or reply root.
func planEvent(r *rand.Rand, cfg runConfig, sessions []session, sf seedFile, marker int64, at time.Time) eventPlan {
	sIdx := r.IntN(len(sessions))
	s := sessions[sIdx]
	p := eventPlan{marker: marker, session: sIdx, issue: s.watched[r.IntN(len(s.watched))], scheduled: at.UnixNano()}
	p.human = r.Float64() < cfg.human
	p.kind = cfg.mix.pick(r.Float64())
	is := sf.Issues[p.issue]
	switch p.kind {
	case kindAnchored:
		p.quote = is.Quotes[r.IntN(len(is.Quotes))]
	case kindReply:
		anchoredRoot := r.Float64() < replyAnchoredShare
		roots := is.PlainCommentIDs
		if (anchoredRoot && len(is.AnchoredCommentIDs) > 0) || len(roots) == 0 {
			roots = is.AnchoredCommentIDs
		}
		p.replyTo = roots[r.IntN(len(roots))]
	}
	return p
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

// sleepUntil waits for t; false when ctx ended first.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func fetchTo(url, path string, timeout time.Duration, lg *log.Logger) {
	resp, err := (&http.Client{Timeout: timeout}).Get(url)
	if err != nil {
		lg.Printf("fetch %s: %v", url, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		lg.Printf("fetch %s: %d %s", url, resp.StatusCode, trunc(b))
		return
	}
	f, err := os.Create(path)
	if err != nil {
		lg.Printf("create %s: %v", path, err)
		return
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		lg.Printf("fetch %s: %v", url, err)
	}
}

func writeJSONFile(path string, v any, lg *log.Logger) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = os.WriteFile(path, append(b, '\n'), 0o644)
	}
	if err != nil {
		lg.Printf("write %s: %v", path, err)
	}
}

// collectMeta records the machine, the build and the seed before the step starts.
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
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 3 {
			m.LoadavgBefore = strings.Join(f[:3], " ")
		}
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
