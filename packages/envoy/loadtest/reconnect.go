package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

// staleBacklog is how far below the event log's max(id) a stale socket (-live-stale) resumes: an
// hour of the outage night's 3.5 events per second, a browser tab that comes back after an hour.
const staleBacklog = 12600

// dashboardCaller is the route-mix caller whose routes are the dashboard tab's reads; a reconnect
// whose subscription did not replay in full issues each of them once (refetchRoutes).
const dashboardCaller = "browser"

// The kinds of reconnect, by the resume they subscribe with.
const (
	reconnectLastID = "last_id" // since = the last event id the socket received
	reconnectStale  = "stale"   // since = max(id) - staleBacklog, once, at the restart
	reconnectNoID   = "no_id"   // no resume: the socket had received no event yet
)

// reconnectKinds is every kind in the order the tables list them.
var reconnectKinds = []string{reconnectLastID, reconnectStale, reconnectNoID}

// The dashboard's waits before its next socket attempt
// (packages/dispatch/web/src/api/live-socket.ts reconnectDelayMs).
const (
	// reconnectSpread is the window the first attempt after a dropped socket is spread over.
	reconnectSpread = 2 * time.Second
	// baseReconnectDelay doubles with each later attempt, up to maxReconnectDelay.
	baseReconnectDelay = time.Second
	maxReconnectDelay  = 30 * time.Second
)

// reconnectDelay is the dashboard's wait before attempt `attempt` (0 for the first after a socket
// was lost), with r for its jitter: attempt 0 is spread uniformly over reconnectSpread, and each
// later attempt waits half of d = min(1 s x 2^attempt, 30 s) plus a random share of the other
// half, so tabs a restart dropped together stay spread.
func reconnectDelay(attempt int, r *rand.Rand) time.Duration {
	if attempt <= 0 {
		return time.Duration(r.Float64() * float64(reconnectSpread))
	}
	ceiling := maxReconnectDelay
	// 1 s doubled five times is already past the ceiling, and shifting further overflows.
	if attempt < 5 {
		ceiling = baseReconnectDelay << attempt
	}
	return ceiling/2 + time.Duration(r.Float64()*float64(ceiling/2))
}

// refetchRoutes are the routes a dashboard tab refetches when a reconnect's subscription did not
// replay in full: every route of the mix whose caller is the dashboard's.
func refetchRoutes(m routeMix) []route {
	var out []route
	for _, r := range m.Routes {
		if r.Caller == dashboardCaller {
			out = append(out, r)
		}
	}
	return out
}

// reconnectRec is one reconnect of a resuming live socket (-live-resume): a socket that opened
// after an earlier one of the same viewer ended. Times are wall-clock.
type reconnectRec struct {
	Socket int    `json:"socket"`
	Kind   string `json:"kind"`
	// At is when the dial that opened the socket started; AfterRestart is set when it started
	// after stack.sh restart-dispatch did.
	At           time.Time `json:"at"`
	AfterRestart bool      `json:"after_restart"`
	// Since is the resume the subscribe frame sent (0 for no_id); Head is the event log's
	// max(id), read from Postgres just before the dial.
	Since int64 `json:"since"`
	Head  int64 `json:"head"`
	// FailedAttempts are the attempts since the previous socket ended that started no
	// subscription (a refused dial while Dispatch was down, or a subscription it would not open).
	FailedAttempts int `json:"failed_attempts"`
	// GapMs is from the end of the previous socket to this subscription's events_start frame.
	GapMs float64 `json:"gap_ms"`
	// StartMs is from the dial's start to the subscription's events_start frame.
	StartMs float64 `json:"start_ms"`
	// Complete is the events_start frame's replay_complete: the subscription replayed every event
	// after the resume it was sent, so the page owes no refetch.
	Complete bool `json:"complete"`
	// ReplayFrames and ReplayBytes count the frames whose event id is at or below Head;
	// ReplayMs is from the dial's start to the last of them.
	ReplayFrames int     `json:"replay_frames"`
	ReplayBytes  int64   `json:"replay_bytes"`
	ReplayMs     float64 `json:"replay_ms"`
	// Refetch is the refetch the reconnect made, nil when its subscription replayed in full or
	// the route mix has no dashboard route.
	Refetch *refetchRec `json:"refetch,omitempty"`
}

// refetchRec is one reconnect's refetch: each dashboard route once.
type refetchRec struct {
	Requests  int       `json:"requests"`
	Failures  int       `json:"failures"` // non-2xx or transport error
	Bytes     int64     `json:"bytes"`    // response bodies
	LatencyMs []float64 `json:"latency_ms"`
}

// replayTally counts one socket's replayed frames: those whose event id is at or below the head
// read just before the socket opened.
type replayTally struct {
	head   int64
	frames int
	bytes  int64
	last   int64 // Unix ns of the last replayed frame
}

// add counts f when it is replayed and reports whether it is.
func (t *replayTally) add(f liveFrame) bool {
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

// checkStaleBacklog refuses -live-stale on an event log that does not hold the staleBacklog rows a
// stale socket goes back over.
func checkStaleBacklog(ctx context.Context, pgURL string) error {
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return fmt.Errorf("-live-stale: count the event log: %w", err)
	}
	defer conn.Close(context.Background())
	var rows int64
	if err := conn.QueryRow(ctx, `select count(*) from events`).Scan(&rows); err != nil {
		return fmt.Errorf("-live-stale: count the event log: %w", err)
	}
	if rows < staleBacklog {
		return fmt.Errorf("-live-stale: the event log holds %d rows, fewer than the %d a stale socket goes back over; extend the seed with stack.sh seed --min-events 13000", rows, staleBacklog)
	}
	return nil
}

// refetcher makes the refetch a dashboard tab makes when a reconnect's subscription did not replay
// in full (packages/dispatch/web/src/api/event-feed.ts refreshes every query the app holds): every
// dashboard route of the mix once, all at once, as the socket's viewer, on the socket's issue.
type refetcher struct {
	a      *api
	routes []route
	issues []seedIssue // the issues in use
}

func (r refetcher) do(ctx context.Context, socket int) *refetchRec {
	is := r.issues[socket%len(r.issues)]
	out := &refetchRec{Requests: len(r.routes)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, rt := range r.routes {
		wg.Go(func() {
			res := r.a.call(ctx, "GET", rt.render(is, is.Project), who{person: viewerName(socket)}, nil)
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

// viewerName is the person a dashboard socket and its refetches are made as.
func viewerName(socket int) string { return "viewer-" + strconv.Itoa(socket) }

// liveSockets are the dashboard live sockets (-live). Without resume, each socket subscribes to
// the events view from the feed's head, and one whose socket ended dials again after retry. With
// resume, a socket reopens as a dashboard tab does: subscribing with since its last event id,
// after the dashboard's backoff (reconnectDelay), and each reconnect is recorded. The first stale
// sockets subscribe with since = max(id) - staleBacklog on their first reconnect after the
// restart began.
type liveSockets struct {
	url    string
	dialer *websocket.Dialer
	// nonce is the run's marker nonce, whose markers readLive finds in the frames.
	nonce int64
	// retry is the wait before the next dial without resume; with it, reconnectDelay says.
	retry time.Duration
	// seed seeds each socket's reconnect jitter.
	seed   uint64
	resume bool
	stale  int
	head   func(context.Context) (int64, error)
	// refetch is the refetch a reconnect that did not replay in full makes; nil: no refetch.
	refetch func(ctx context.Context, socket int) *refetchRec
	// restartBegun reports whether stack.sh restart-dispatch has started.
	restartBegun func() bool
	// frame receives every frame of every socket, replayed ones marked.
	frame func(liveFrame)
	bytes atomic.Int64
	mu    sync.Mutex
	recs  []reconnectRec
}

// reconnects are the reconnects recorded so far, in the order their sockets started.
func (s *liveSockets) reconnects() []reconnectRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reconnectRec(nil), s.recs...)
}

// run drives socket i until ctx ends.
func (s *liveSockets) run(ctx context.Context, i int) {
	var lastID int64
	var sockEnd time.Time
	opened, staleDue := false, i < s.stale
	attempt, failed := 0, 0
	jitter := rand.New(rand.NewPCG(s.seed, uint64(i)))
	header := http.Header{"X-Dispatch-User": {viewerName(i)}}
	for ctx.Err() == nil {
		reconnect := opened && s.resume
		rec := reconnectRec{Socket: i}
		if reconnect {
			head, err := s.head(ctx)
			if err != nil {
				failed++
				sleepUntil(ctx, time.Now().Add(s.nextDelay(&attempt, jitter)))
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
		}
		start := time.Now()
		conn, resp, err := s.dialer.DialContext(ctx, s.url, header)
		if err != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			failed++
			sleepUntil(ctx, time.Now().Add(s.nextDelay(&attempt, jitter)))
			continue
		}
		var refetched chan *refetchRec
		started := false
		tally := replayTally{head: rec.Head}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		n, _ := readLive(conn, s.nonce, rec.Since, time.Now,
			func(at int64, replayComplete bool) {
				if started { // a resubscribe within the socket, after Dispatch ended the feed
					return
				}
				started = true
				if !reconnect {
					return
				}
				if rec.Kind == reconnectStale {
					staleDue = false
				}
				rec.At, rec.AfterRestart = start, s.restartBegun()
				rec.FailedAttempts, failed = failed, 0
				rec.GapMs = float64(at-sockEnd.UnixNano()) / 1e6
				rec.StartMs = float64(at-start.UnixNano()) / 1e6
				rec.Complete = replayComplete
				if !rec.Complete && s.refetch != nil {
					refetched = make(chan *refetchRec, 1)
					go func() { refetched <- s.refetch(ctx, i) }()
				}
			},
			func(f liveFrame) {
				lastID = max(lastID, f.id)
				f.replay = tally.add(f)
				s.frame(f)
			})
		stop()
		_ = conn.Close()
		s.bytes.Add(n)
		sockEnd = time.Now()
		if !started {
			failed++
			sleepUntil(ctx, time.Now().Add(s.nextDelay(&attempt, jitter)))
			continue
		}
		attempt = 0
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
		sleepUntil(ctx, time.Now().Add(s.nextDelay(&attempt, jitter)))
	}
}

// nextDelay is the wait before a socket's next attempt: retry without resume, the dashboard's
// backoff with it, counting the attempt.
func (s *liveSockets) nextDelay(attempt *int, jitter *rand.Rand) time.Duration {
	if !s.resume {
		return s.retry
	}
	d := reconnectDelay(*attempt, jitter)
	*attempt++
	return d
}

// reconnectGroup reduces the reconnects of one kind (or all) in the window.
type reconnectGroup struct {
	Reconnects     int `json:"reconnects"`
	Complete       int `json:"complete"`
	AfterRestart   int `json:"after_restart"`
	FailedAttempts int `json:"failed_attempts"`
	// GapMs, StartMs, ReplayFrames, ReplayBytes and ReplayMs hold one value per reconnect;
	// ReplayMs only those that replayed a frame.
	GapMs             stat  `json:"gap_ms"`
	StartMs           stat  `json:"start_ms"`
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

// reconnectSummary is the window's reconnects of the resuming live sockets.
type reconnectSummary struct {
	Stale int `json:"stale_sockets"`
	// RefetchRoutes are the routes each refetch issues.
	RefetchRoutes []string `json:"refetch_routes"`
	// Groups are by kind (last_id, stale, no_id), and "all".
	Groups map[string]reconnectGroup `json:"groups"`
}

// summarizeReconnects reduces the reconnects whose dial started in the window.
func summarizeReconnects(recs []reconnectRec, stale int, routes []route, inWindow func(time.Time) bool) *reconnectSummary {
	out := &reconnectSummary{Stale: stale, RefetchRoutes: []string{}, Groups: map[string]reconnectGroup{}}
	for _, r := range routes {
		out.RefetchRoutes = append(out.RefetchRoutes, r.Name)
	}
	type values struct{ gap, start, frames, bytes, replayMs, refetchMs []float64 }
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
			v.start = append(v.start, r.StartMs)
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
		g.GapMs, g.StartMs, g.ReplayFrames, g.ReplayBytes = mkStat(v.gap), mkStat(v.start), mkStat(v.frames), mkStat(v.bytes)
		g.ReplayMs, g.RefetchMs = mkStat(v.replayMs), mkStat(v.refetchMs)
		out.Groups[key] = g
	}
	return out
}
