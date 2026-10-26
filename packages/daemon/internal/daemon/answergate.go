package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// answerAppendWait is the longest a claim's start waits for an answer being added to its
// conversation before it starts anyway.
const answerAppendWait = 10 * time.Second

// answerGate keeps the answer loop and a claim's start off one conversation at once (plan 2.8, "One
// writer"). It wraps the store's CheckLaunch, the check every start makes first (launchAfter), as
// launchgate.go does, with state of its own per claim token and never the machine's lock: a start
// waits for an append in progress, at most answerAppendWait, then marks the claim started before it
// asks the store underneath; the loop appends only under the gate, after checking that the claim
// has not been started and, from the machine's View (read without the machine's lock), does not
// run the conversation. It never calls Machine.Claim, which waits out a decision in flight.
type answerGate struct {
	supervise.Store
	// view is the claim's machine's View, false when the daemon supervises no such claim.
	view func(claim.Token) (supervise.ClaimView, bool)
	wait time.Duration
	log  *slog.Logger

	mu sync.Mutex
	// changed is closed, and replaced, whenever an append ends: a start waiting on it looks again.
	changed chan struct{}
	tokens  map[claim.Token]*answerGateState
}

// answerGateState is one claim's: whether the loop is appending to its current conversation, and
// whether it was started (since a launch at generation gen passed its check).
type answerGateState struct {
	appending bool
	started   bool
	gen       uint64
}

func newAnswerGate(store supervise.Store, view func(claim.Token) (supervise.ClaimView, bool), log *slog.Logger) *answerGate {
	return &answerGate{Store: store, view: view, wait: answerAppendWait, log: log, changed: make(chan struct{}), tokens: map[claim.Token]*answerGateState{}}
}

func (g *answerGate) state(token claim.Token) *answerGateState {
	st, ok := g.tokens[token]
	if !ok {
		st = &answerGateState{}
		g.tokens[token] = st
	}
	return st
}

// CheckLaunch is a start's check: it waits for an append to the claim's conversation to end, at
// most the gate's wait, marks the claim started, then asks the store underneath. A start the store
// refuses is not started.
func (g *answerGate) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	g.mu.Lock()
	st := g.state(c.Token)
wait:
	for st.appending {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
			g.mu.Lock()
		case <-timer.C:
			g.log.Warn("answers: a claim starts while an answer is still being added to its conversation", "claim", c.Token, "waited", g.wait)
			g.mu.Lock()
			break wait
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	st.started, st.gen = true, c.Generation
	g.mu.Unlock()
	if err := g.Store.CheckLaunch(ctx, c); err != nil {
		g.mu.Lock()
		if st.gen == c.Generation {
			st.started = false
		}
		g.mu.Unlock()
		return err
	}
	return nil
}

// Running is whether session's conversation is its claim's current one and the claim runs it, is
// starting, or was started (answerloop.Gate).
func (g *answerGate) Running(token claim.Token, session string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runningLocked(token, session)
}

// runningLocked is Running under g.mu. A start's mark lasts until the machine shows a launch after
// it (a later generation) that runs nothing any more: the process the start began has ended.
func (g *answerGate) runningLocked(token claim.Token, session string) bool {
	view, ok := g.view(token)
	if !ok || view.Session == "" || view.Session != session {
		return false
	}
	if st := g.tokens[token]; st != nil && st.started {
		if view.Generation <= st.gen || !view.RunsNothing() {
			return true
		}
		st.started = false
	}
	return !view.RunsNothing()
}

// Append runs write while no start of the claim can pass its check, unless the claim runs the
// conversation or was started: then it writes nothing and answers false (answerloop.Gate). A
// conversation that is not its claim's current one is written under no claim's gate: no start
// resumes it.
func (g *answerGate) Append(token claim.Token, session string, write func() error) (bool, error) {
	g.mu.Lock()
	if g.runningLocked(token, session) {
		g.mu.Unlock()
		return false, nil
	}
	st := g.state(token)
	st.appending = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		st.appending = false
		close(g.changed)
		g.changed = make(chan struct{})
		g.mu.Unlock()
	}()
	return true, write()
}
