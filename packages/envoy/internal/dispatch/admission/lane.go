package admission

// The heavy reads' lanes. A few read routes cost the database far more than the rest, and the
// class limits count them as one unit each like any other read, so nothing stops several of them
// running at once: the processors go to those few and every other request waits behind them.
// A lane is a limit of its own for one such route, taken before the request's class units.

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Lane is a heavy read route's own concurrency limit. A route carries one (api's heavyRead); every
// other route carries NoLane and is bounded by its class alone.
type Lane int

const (
	// NoLane is every route but the heavy reads.
	NoLane Lane = iota
	// Search is GET /api/v1/search, the full-text union.
	Search
	// References is the reference closure: GET /api/v1/issues/{key}/references and
	// GET /api/v1/references.
	References
	// IssueArtifacts is GET /api/v1/issues/{key}/artifacts, an issue's documents and files.
	IssueArtifacts
	// IssueList is GET /api/v1/issues, the filtered issue list.
	IssueList
	laneCount
)

var laneNames = [laneCount]string{NoLane: "", Search: "search", References: "references",
	IssueArtifacts: "issue_artifacts", IssueList: "issue_list"}

func (l Lane) String() string {
	if l < 0 || l >= laneCount {
		return ""
	}
	return laneNames[l]
}

// HeavyReadLanes are the lanes in order, for a caller that reports on each of them (the metrics
// endpoint).
func HeavyReadLanes() []Lane { return []Lane{Search, References, IssueArtifacts, IssueList} }

// HeavyReadLaneLimit is how many requests of one heavy read route may run at once in a process.
//
// It is measured rather than chosen. On the load test's recovery step (packages/envoy/loadtest,
// 0.5x:60s,10x:60s,0.5x:120s) the windows that fail the recovery criterion are 3 to 4 second
// stalls in which Postgres, sampled every 250 ms, has three to five of these statements running at
// once and waiting on nothing - the search union at 283 to 983 ms of statement age, the issue list
// at 104 to 558 ms, the references closure, an issue's artifacts at 165 to 449 ms - while
// everything else on the box queues behind them. Over the two minutes after the drop those four
// statements are 101 seconds of the 124 seconds of statement time the whole process spends, on a
// box of three processors (INFRA-592, j-report Step 4):
//
//	the search union (websearch_to_tsquery)   46.4 s over 142 calls, 327 ms each
//	the references closure (recursive, refs)  25.8 s over 158 calls, 164 ms each
//	an issue's artifacts                      15.0 s over  52 calls, 288 ms each
//	the issue list (recursive, filtered)      14.1 s over  95 calls, 149 ms each
//
// Three is measured too, against one and two on that step, three runs each (INFRA-592, j-report
// Step 5). The three answer the same load differently and the difference is where the cost shows:
//
//	lane limit  2xx served  what the recovery windows failed on
//	1           8,300       the refusals: 85 to 98% of the requests of a window succeeded
//	2           8,083       the refusals, smaller: 97 to 99%
//	3           8,122       neither in one run of three (it recovered in 0 s); the p99 of the
//	                        40-60 s windows in the other two, and one window at 99.0%
//
// Below three a lane refuses requests the box would have served - at one, nearly every window of
// every run was refused past the criterion's 99% - and above it the point of the lane goes: at
// four, four searches at 327 ms each are more database processor than the box has. Three serves
// about 8% more requests than the same tree without lanes (7,161 to 7,541), so the lane is not
// costing throughput; what it buys is that one route cannot hold the database while everything
// else waits. It is a per-process limit, like every other limit here: two processes serve twice
// as many, and each is held to what one box can serve.
const HeavyReadLaneLimit = 3

// laneGate is one lane's limit and its queue: as a class's gate, but with a lane's own mutex,
// since a lane is taken before the classes and holds none of their units while it waits. Its
// waiters are served first in, first out - a request takes its lane before anything else it does,
// so the order it reaches the queue is the order it arrived in.
type laneGate struct {
	lane  Lane
	limit int
	wait  time.Duration

	mu      sync.Mutex
	used    int
	waiters list.List // of chan struct{}, in arrival order

	queueFull, waitExceeded atomic.Uint64
}

// LaneStats is what one lane holds and has refused.
type LaneStats struct {
	Lane Lane
	// Limit is how many requests of the lane may run at once, InFlight how many run now and
	// Queued how many wait for room.
	Limit, InFlight, Queued int
	// QueueFull and WaitExceeded count the requests the lane refused for each reason since the
	// Limiter was made.
	QueueFull, WaitExceeded uint64
}

// LaneStats reads every lane's counts.
func (l *Limiter) LaneStats() []LaneStats {
	stats := make([]LaneStats, 0, len(HeavyReadLanes()))
	for _, lane := range HeavyReadLanes() {
		g := l.lanes[lane]
		g.mu.Lock()
		stats = append(stats, LaneStats{Lane: lane, Limit: g.limit, InFlight: g.used,
			Queued: g.waiters.Len(), QueueFull: g.queueFull.Load(), WaitExceeded: g.waitExceeded.Load()})
		g.mu.Unlock()
	}
	return stats
}

// EnterLane takes a place in lane for a request about to ask for its class's units, and returns
// the release that gives the place back. A request that finds the lane full waits in its queue,
// in arrival order, for the read wait (Limits.ReadWait), holding no admission unit while it waits -
// which is the point of taking the lane first: a heavy read waiting for its lane costs the other
// routes nothing. One that finds the queue already holding queueFactor times the lane's limit is
// refused at once, and one whose wait runs out is refused then: either way a *Refusal naming the
// lane, which its caller answers 429 OVERLOADED with Retry-After, as it answers a class's refusal.
//
// A request of NoLane takes nothing and releases nothing.
func (l *Limiter) EnterLane(ctx context.Context, lane Lane) (func(), error) {
	if lane <= NoLane || lane >= laneCount {
		return func() {}, nil
	}
	g := l.lanes[lane]
	g.mu.Lock()
	if g.waiters.Len() == 0 && g.used < g.limit {
		g.used++
		g.mu.Unlock()
		return func() { g.leave() }, nil
	}
	if g.waiters.Len() >= queueFactor*g.limit {
		g.mu.Unlock()
		g.queueFull.Add(1)
		return nil, &Refusal{Class: Read, Reason: QueueFull, Lane: lane}
	}
	ready := make(chan struct{})
	element := g.waiters.PushBack(ready)
	g.mu.Unlock()

	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	var failure error
	select {
	case <-ready:
		return func() { g.leave() }, nil
	case <-ctx.Done():
		failure = ctx.Err()
	case <-timer.C:
		failure = &Refusal{Class: Read, Reason: WaitExceeded, Lane: lane}
	}

	g.mu.Lock()
	select {
	case <-ready:
		// Granted in the moment the wait ended: the place is this request's, and a request
		// that is no longer there gives it straight back.
		g.mu.Unlock()
		if ctx.Err() == nil {
			return func() { g.leave() }, nil
		}
		g.leave()
		return nil, failure
	default:
	}
	g.waiters.Remove(element)
	g.mu.Unlock()
	if _, refused := failure.(*Refusal); refused {
		g.waitExceeded.Add(1)
	}
	return nil, failure
}

// leave gives one place in the lane back and hands it to the waiter at the head of the queue.
func (g *laneGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used--
	if front := g.waiters.Front(); front != nil {
		g.waiters.Remove(front)
		g.used++
		close(front.Value.(chan struct{}))
	}
}
