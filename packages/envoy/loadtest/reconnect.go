package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// staleBacklog is how far below the event log's max(id) a stale stream (-sse-stale) resumes: an
// hour of the outage night's 3.5 events per second, a browser tab that comes back after an hour.
const staleBacklog = 12600

// replayHeader is the response header with which Dispatch says that a resume's catch-up was sent
// in full, so the client need not refetch (internal/dispatch/api/events.go eventReplayHeader). A
// build that never sets it makes every reconnect refetch.
const (
	replayHeader   = "X-Dispatch-Event-Replay"
	replayComplete = "complete"
)

// dashboardCaller is the route-mix caller whose routes are the dashboard tab's reads; a reconnect
// without a complete replay issues each of them once (refetchRoutes).
const dashboardCaller = "browser"

// The kinds of reconnect, by the since it sends.
const (
	reconnectLastID = "last_id" // since = the last event id the stream received
	reconnectStale  = "stale"   // since = max(id) - staleBacklog, once, at the restart
	reconnectNoID   = "no_id"   // no since: the stream had received no event yet
)

// reconnectKinds is every kind in the order the tables list them.
var reconnectKinds = []string{reconnectLastID, reconnectStale, reconnectNoID}

// reconnectDelay is the dashboard's wait before its next stream attempt
// (packages/dispatch/web/src/api/live.ts reconnectDelayMs): 1 s, doubled for each attempt since
// the stream last opened, at most 30 s.
func reconnectDelay(attempt int) time.Duration {
	d := time.Second
	for range attempt {
		d *= 2
		if d >= 30*time.Second {
			return 30 * time.Second
		}
	}
	return d
}

// refetchRoutes are the routes a dashboard tab refetches when a reconnect carries no complete
// replay: every route of the mix whose caller is the dashboard's.
func refetchRoutes(m routeMix) []route {
	var out []route
	for _, r := range m.Routes {
		if r.Caller == dashboardCaller {
			out = append(out, r)
		}
	}
	return out
}

// reconnectRec is one reconnect of a resuming SSE stream (-sse-resume): a connection that opened
// after an earlier one of the same stream ended. Times are wall-clock.
type reconnectRec struct {
	Stream int    `json:"stream"`
	Kind   string `json:"kind"`
	// At is when the request that opened the connection started; AfterRestart is set when it
	// started after stack.sh restart-dispatch did.
	At           time.Time `json:"at"`
	AfterRestart bool      `json:"after_restart"`
	// Since is the since the request sent (0 for no_id); Head is the event log's max(id), read
	// from Postgres just before the request.
	Since int64 `json:"since"`
	Head  int64 `json:"head"`
	// FailedAttempts are the attempts since the previous connection ended that got no 200 (a
	// refused connection while Dispatch was down, or an error status).
	FailedAttempts int `json:"failed_attempts"`
	// GapMs is from the end of the previous connection to this one's response headers.
	GapMs float64 `json:"gap_ms"`
	// TTFBMs is from the request's start to the response's first byte.
	TTFBMs   float64 `json:"ttfb_ms"`
	Complete bool    `json:"complete"`
	// ReplayFrames and ReplayBytes count the frames whose event id is at or below Head;
	// ReplayMs is from the request's start to the last of them.
	ReplayFrames int     `json:"replay_frames"`
	ReplayBytes  int64   `json:"replay_bytes"`
	ReplayMs     float64 `json:"replay_ms"`
	// Refetch is the refetch the reconnect made, nil when it carried a complete replay or the
	// route mix has no dashboard route.
	Refetch *refetchRec `json:"refetch,omitempty"`
}

// refetchRec is one reconnect's refetch: each dashboard route once.
type refetchRec struct {
	Requests  int       `json:"requests"`
	Failures  int       `json:"failures"` // non-2xx or transport error
	Bytes     int64     `json:"bytes"`    // response bodies
	LatencyMs []float64 `json:"latency_ms"`
}

// replayTally counts one connection's replayed frames: those whose event id is at or below the
// head read just before the connection opened.
type replayTally struct {
	head   int64
	frames int
	bytes  int64
	last   int64 // Unix ns of the last replayed frame
}

// add counts f when it is replayed and reports whether it is.
func (t *replayTally) add(f sseFrame) bool {
	if f.id > t.head {
		return false
	}
	t.frames++
	t.bytes += f.bytes
	t.last = f.at
	return true
}

// eventHead reads the event log's max(id) over one direct Postgres connection, opened on first use
// and again after a failed read.
type eventHead struct {
	url  string
	mu   sync.Mutex
	conn *pgx.Conn
}

func (h *eventHead) read(ctx context.Context) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		c, err := pgx.Connect(ctx, h.url)
		if err != nil {
			return 0, err
		}
		h.conn = c
	}
	var id int64
	if err := h.conn.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&id); err != nil {
		h.conn.Close(context.Background())
		h.conn = nil
		return 0, err
	}
	return id, nil
}

func (h *eventHead) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != nil {
		h.conn.Close(context.Background())
		h.conn = nil
	}
}

// checkStaleBacklog refuses -sse-stale on an event log that does not hold the staleBacklog rows a
// stale stream goes back over.
func checkStaleBacklog(ctx context.Context, pgURL string) error {
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return fmt.Errorf("-sse-stale: count the event log: %w", err)
	}
	defer conn.Close(context.Background())
	var rows int64
	if err := conn.QueryRow(ctx, `select count(*) from events`).Scan(&rows); err != nil {
		return fmt.Errorf("-sse-stale: count the event log: %w", err)
	}
	if rows < staleBacklog {
		return fmt.Errorf("-sse-stale: the event log holds %d rows, fewer than the %d a stale stream goes back over; extend the seed with stack.sh seed --min-events 13000", rows, staleBacklog)
	}
	return nil
}

// refetcher makes the refetch a dashboard tab makes on a reconnect without a complete replay
// (packages/dispatch/web/src/api/sse.ts onOpen refreshes every query the app holds): every
// dashboard route of the mix once, all at once, as the stream's viewer, on the stream's issue.
type refetcher struct {
	a      *api
	routes []route
	issues []seedIssue // the issues in use
}

func (r refetcher) do(ctx context.Context, stream int) *refetchRec {
	is := r.issues[stream%len(r.issues)]
	out := &refetchRec{Requests: len(r.routes)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, rt := range r.routes {
		wg.Go(func() {
			res := r.a.call(ctx, "GET", rt.render(is, is.Project), who{person: viewerName(stream)}, nil)
			mu.Lock()
			defer mu.Unlock()
			out.Bytes += int64(len(res.body))
			out.LatencyMs = append(out.LatencyMs, float64(res.end-res.arrival())/1e6)
			if !res.ok() {
				out.Failures++
			}
		})
	}
	wg.Wait()
	return out
}

// viewerName is the person a dashboard stream and its refetches are made as.
func viewerName(stream int) string { return "viewer-" + strconv.Itoa(stream) }

// sseStreams are the dashboard SSE streams (-sse). Without resume, each connection opens with no
// since, and a stream whose connection ended opens again after 1 s. With resume, a stream reopens
// as the dashboard does: with since its last event id, after the dashboard's backoff
// (reconnectDelay), and each reconnect is recorded. The first stale streams send
// since = max(id) - staleBacklog on their first reconnect after the restart began.
type sseStreams struct {
	base   string
	client *http.Client
	// nonce is the run's marker nonce, whose markers readSSE finds in the frames.
	nonce   int64
	resume  bool
	stale   int
	head    func(context.Context) (int64, error)
	refetch func(ctx context.Context, stream int) *refetchRec // nil: no refetch
	// restartBegun reports whether stack.sh restart-dispatch has started.
	restartBegun func() bool
	// frame receives every frame of every connection, replayed ones marked.
	frame func(sseFrame)
	bytes atomic.Int64
	mu    sync.Mutex
	recs  []reconnectRec
}

// reconnects are the reconnects recorded so far, in the order their connections ended.
func (s *sseStreams) reconnects() []reconnectRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reconnectRec(nil), s.recs...)
}

// run drives stream i until ctx ends.
func (s *sseStreams) run(ctx context.Context, i int) {
	var lastID int64
	var connEnd time.Time
	opened, staleDue := false, i < s.stale
	attempt, failed := 0, 0
	for ctx.Err() == nil {
		reconnect := opened && s.resume
		rec := reconnectRec{Stream: i}
		url := s.base + "/api/v1/events"
		if reconnect {
			head, err := s.head(ctx)
			if err != nil {
				failed++
				sleepUntil(ctx, time.Now().Add(s.nextDelay(&attempt)))
				continue
			}
			rec.Head = head
			switch {
			case staleDue && s.restartBegun():
				rec.Kind, rec.Since = reconnectStale, max(0, head-staleBacklog)
			case lastID > 0:
				rec.Kind, rec.Since = reconnectLastID, lastID
			default:
				rec.Kind = reconnectNoID
			}
			if rec.Kind != reconnectNoID {
				url += "?since=" + strconv.FormatInt(rec.Since, 10)
			}
		}
		var firstByte atomic.Int64
		trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { firstByte.Store(time.Now().UnixNano()) }}
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", url, nil)
		req.Header.Set("X-Dispatch-User", viewerName(i))
		req.Header.Set("Accept", "text/event-stream")
		start := time.Now()
		resp, err := s.client.Do(req)
		opens := err == nil && resp.StatusCode == http.StatusOK
		if err == nil && !opens {
			_ = resp.Body.Close()
		}
		if !opens {
			failed++
			sleepUntil(ctx, time.Now().Add(s.nextDelay(&attempt)))
			continue
		}
		attempt = 0
		var refetched chan *refetchRec
		if reconnect {
			if rec.Kind == reconnectStale {
				staleDue = false
			}
			rec.At, rec.AfterRestart = start, s.restartBegun()
			rec.FailedAttempts, failed = failed, 0
			rec.GapMs = float64(time.Since(connEnd)) / 1e6
			if fb := firstByte.Load(); fb != 0 {
				rec.TTFBMs = float64(fb-start.UnixNano()) / 1e6
			}
			rec.Complete = resp.Header.Get(replayHeader) == replayComplete
			if !rec.Complete && s.refetch != nil {
				refetched = make(chan *refetchRec, 1)
				go func() { refetched <- s.refetch(ctx, i) }()
			}
		}
		tally := replayTally{head: rec.Head}
		n, _ := readSSE(resp.Body, s.nonce, time.Now, func(f sseFrame) {
			lastID = max(lastID, f.id)
			f.replay = tally.add(f)
			s.frame(f)
		})
		s.bytes.Add(n)
		_ = resp.Body.Close()
		connEnd = time.Now()
		if reconnect {
			rec.ReplayFrames, rec.ReplayBytes = tally.frames, tally.bytes
			if tally.frames > 0 {
				rec.ReplayMs = float64(tally.last-start.UnixNano()) / 1e6
			}
			if refetched != nil {
				rec.Refetch = <-refetched
			}
			s.mu.Lock()
			s.recs = append(s.recs, rec)
			s.mu.Unlock()
		}
		opened = true
		sleepUntil(ctx, time.Now().Add(s.nextDelay(&attempt)))
	}
}

// nextDelay is the wait before a stream's next attempt: 1 s without resume, the dashboard's
// backoff with it, counting the attempt.
func (s *sseStreams) nextDelay(attempt *int) time.Duration {
	if !s.resume {
		return time.Second
	}
	d := reconnectDelay(*attempt)
	*attempt++
	return d
}

// reconnectGroup reduces the reconnects of one kind (or all) in the window.
type reconnectGroup struct {
	Reconnects     int `json:"reconnects"`
	Complete       int `json:"complete"`
	AfterRestart   int `json:"after_restart"`
	FailedAttempts int `json:"failed_attempts"`
	// GapMs, TTFBMs, ReplayFrames, ReplayBytes and ReplayMs hold one value per reconnect;
	// ReplayMs only those that replayed a frame.
	GapMs             stat  `json:"gap_ms"`
	TTFBMs            stat  `json:"ttfb_ms"`
	ReplayFrames      stat  `json:"replay_frames"`
	ReplayBytes       stat  `json:"replay_bytes"`
	ReplayMs          stat  `json:"replay_ms"`
	ReplayFramesTotal int   `json:"replay_frames_total"`
	ReplayBytesTotal  int64 `json:"replay_bytes_total"`
	// Refetches are the reconnects that refetched; RefetchRequests, RefetchFailures and
	// RefetchBytes their requests, and RefetchMs one value per request.
	Refetches       int   `json:"refetches"`
	RefetchRequests int   `json:"refetch_requests"`
	RefetchFailures int   `json:"refetch_failures"`
	RefetchBytes    int64 `json:"refetch_bytes"`
	RefetchMs       stat  `json:"refetch_ms"`
}

// reconnectSummary is the window's reconnects of the resuming SSE streams.
type reconnectSummary struct {
	Stale int `json:"stale_streams"`
	// RefetchRoutes are the routes each refetch issues.
	RefetchRoutes []string `json:"refetch_routes"`
	// Groups are by kind (last_id, stale, no_id), and "all".
	Groups map[string]reconnectGroup `json:"groups"`
}

// summarizeReconnects reduces the reconnects whose request started in the window.
func summarizeReconnects(recs []reconnectRec, stale int, routes []route, inWindow func(time.Time) bool) *reconnectSummary {
	out := &reconnectSummary{Stale: stale, RefetchRoutes: []string{}, Groups: map[string]reconnectGroup{}}
	for _, r := range routes {
		out.RefetchRoutes = append(out.RefetchRoutes, r.Name)
	}
	type values struct{ gap, ttfb, frames, bytes, replayMs, refetchMs []float64 }
	vals := map[string]*values{}
	for _, r := range recs {
		if !inWindow(r.At) {
			continue
		}
		for _, key := range []string{"all", r.Kind} {
			g, v := out.Groups[key], vals[key]
			if v == nil {
				v = &values{}
				vals[key] = v
			}
			g.Reconnects++
			if r.Complete {
				g.Complete++
			}
			if r.AfterRestart {
				g.AfterRestart++
			}
			g.FailedAttempts += r.FailedAttempts
			g.ReplayFramesTotal += r.ReplayFrames
			g.ReplayBytesTotal += r.ReplayBytes
			v.gap = append(v.gap, r.GapMs)
			v.ttfb = append(v.ttfb, r.TTFBMs)
			v.frames = append(v.frames, float64(r.ReplayFrames))
			v.bytes = append(v.bytes, float64(r.ReplayBytes))
			if r.ReplayFrames > 0 {
				v.replayMs = append(v.replayMs, r.ReplayMs)
			}
			if f := r.Refetch; f != nil {
				g.Refetches++
				g.RefetchRequests += f.Requests
				g.RefetchFailures += f.Failures
				g.RefetchBytes += f.Bytes
				v.refetchMs = append(v.refetchMs, f.LatencyMs...)
			}
			out.Groups[key] = g
		}
	}
	for key, g := range out.Groups {
		v := vals[key]
		g.GapMs, g.TTFBMs, g.ReplayFrames, g.ReplayBytes = mkStat(v.gap), mkStat(v.ttfb), mkStat(v.frames), mkStat(v.bytes)
		g.ReplayMs, g.RefetchMs = mkStat(v.replayMs), mkStat(v.refetchMs)
		out.Groups[key] = g
	}
	return out
}
