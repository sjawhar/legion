package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An issue's route says where its messages go; whether that reaches anyone is a separate fact,
// and an issue routed to a role nobody holds reads as owned while nobody is working it. So every
// issue read carries route_status beside the route, judged against the listener's live-session
// list (GET /v1/sessions, each row carrying the roles its session holds), never one lookup per
// issue, and stored in no table. The process keeps one copy of that list (rosterCache): every
// issue read in the process judges against it, and GET /api/v1/agents (listAgents) lists the
// sessions of that same copy, so a route_status or an agents list can be up to routeRosterTTL
// (5 s) old, and one fetch serves every read that asks while it runs, issue read or agents list.
// Like fetchHolderLiveness, the listener call runs with no transaction open and no pooled
// connection held (envoy_resolve.go carries the reason). A read that waits for the fetch waits on
// the listener too, so the rule binds every caller of readRouteRoster: listIssues and getIssue
// call it only once their pooled reads have finished, listAgents before its two aggregates run,
// and a new caller must do the same.
//
// no_holder is one read and says only "nobody right now": a session that restarts or moves
// between agent boxes keeps its id but is absent from the listener for minutes. The read does not
// try to tell that gap from a vacancy with a last-seen time. It judges every issue against one
// roster read rather than a lookup per issue, and the last seen a lapsed holder's lookup gives
// (GET /v1/roles/{role}, which releases nothing and so may be read as often as a caller likes:
// cmd/listener keepLapsedRoleClaim) is the answering listener's own process cache, not the shared
// registry, so two listeners answer it differently and neither answer is the registry's. The
// owner audit (skills/legion-controller/references/backlog-stewardship.md) and
// `dispatch issues --help` (route_status) ask for two reads ten minutes apart instead.

// routeRoster is one listener answer about who is running. answered is false when the listener
// is unconfigured or did not answer; every route then reads unknown. Its maps and its session
// slice are read-only once built, since every read the cache serves shares them: a reader that
// sorts or edits the sessions copies them first, and never writes a session's Roles or
// Capabilities, which every copy shares too.
type routeRoster struct {
	answered bool
	// sessions is the listener's answer, in the listener's order.
	sessions []envoy.Session
	live     map[string]struct{}
	// holders maps a role to the live session holding it. The listener gives a role one holder
	// (last claim wins); should two live rows ever name one role, the most recently seen session
	// is named, and the id breaks a tie, so a read never flips between them.
	holders map[string]string
	// err is why the roster did not answer: the listener client's own error, or, for a read whose
	// request ended while it waited on the shared fetch, envoy.ErrUnavailable wrapping the
	// request's context error, so it always matches envoy.ErrUnavailable. It is nil when the
	// roster answered or the listener is unconfigured.
	err error
}

// routeRosterTimeout bounds the one listener read a roster fetch makes. The listener answers in
// milliseconds; a listener that has stopped answering would otherwise hold every issue list and
// detail, and every agents list, waiting on that fetch for the client's whole five-second window,
// when the read's answer is only "unknown" (an issue read) or a 503 (the agents list).
const routeRosterTimeout = 2 * time.Second

// routeRosterTTL is how long one listener answer serves the process's issue reads and agents
// lists. A route that reads no_holder already means only "nobody right now" (above), and the
// owner audit reads ten minutes apart, so five seconds of age changes no decision, nor does it
// for an agents list a human or a session reads to pick a target; it takes the listener call and
// the decode of the whole fleet's list off every read but one in five seconds.
// routeRosterSilenceTTL is how long a listener that failed or did not answer is left alone: every
// read in that window answers at once rather than waiting out routeRosterTimeout of its own, an
// issue read with every route unknown and an agents list with the failed fetch's 503
// ENVOY_UNAVAILABLE. A failed fetch serves that failure, never an older answer, so this window
// changes no answer's age; it is as long as routeRosterTTL so that a listener that hangs costs
// fewer routeRosterTimeout waits: one shared wait per routeRosterTimeout+routeRosterSilenceTTL
// (7 s) rather than per 3 s. A route_status, unknown included, then lags the listener by at most
// routeRosterTTL, plus routeRosterTimeout while the listener hangs.
const (
	routeRosterTTL        = 5 * time.Second
	routeRosterSilenceTTL = 5 * time.Second
)

// rosterCache is the process's one copy of the listener's live-session list, and the fetch that
// refreshes it.
type rosterCache struct {
	// ttl and silenceTTL are how long an answered and an unanswered fetch serve reads; register
	// sets routeRosterTTL and routeRosterSilenceTTL. Zero keeps nothing past the fetch, though the
	// reads that arrive while it runs still share it.
	ttl        time.Duration
	silenceTTL time.Duration
	// timeout overrides the bound on a fetch's listener read; zero, which every server the
	// deployment builds leaves it, means routeRosterTimeout. Only a test that holds a fetch open
	// while other reads arrive sets it, so how long it holds the fetch is its own choice rather
	// than a race against two seconds of wall clock.
	timeout time.Duration

	mu      sync.Mutex
	roster  routeRoster
	expires time.Time
	// fetch is the listener read in flight, nil when none is.
	fetch *rosterFetch
}

// rosterFetch is one listener read that every read arriving while it runs waits on. roster is
// set before done closes.
type rosterFetch struct {
	done   chan struct{}
	roster routeRoster
}

// readRouteRoster answers who is running: the cached roster while it is fresh, else the answer of
// the one fetch in flight, starting it when none is. A listener that is unconfigured, does not
// answer, or answers late leaves the roster unanswered, and so does a read whose own request ends
// while it waits (its err then wraps the request's context error); the fetch runs on for the
// other reads, and its answer fills the cache. The fetch runs on the process's lifetime, so once
// a shutdown begins it ends or fails at once, and that failure is kept for the silence window.
// What an unanswered roster means is the caller's: an issue read reads every route unknown and
// still succeeds, while listAgents, which has nothing to list without the listener, answers 503
// with the roster's err, or, during a shutdown drain, asks the listener on its own request.
func (s *server) readRouteRoster(ctx context.Context) routeRoster {
	if s.deps.Envoy == nil {
		return routeRoster{}
	}
	if s.deps.ListenerRosterUncached {
		// An end-to-end test process: every read asks the listener itself, with no TTL, no shared
		// fetch and nothing kept.
		return s.listRouteRoster(ctx)
	}
	cache := &s.rosters
	cache.mu.Lock()
	if time.Now().Before(cache.expires) {
		roster := cache.roster
		cache.mu.Unlock()
		return roster
	}
	fetch := cache.fetch
	if fetch == nil {
		fetch = &rosterFetch{done: make(chan struct{})}
		cache.fetch = fetch
		// The fetch is every waiting read's, so it runs on the server's lifetime rather than on
		// the request that started it, whose end must not end it and whose values (its
		// transaction-tracking mark among them) belong to that request alone; the fetch bound
		// still limits it, and a shutdown ends it.
		go s.fetchRouteRoster(s.lifetime, fetch)
	}
	cache.mu.Unlock()
	select {
	case <-fetch.done:
		return fetch.roster
	case <-ctx.Done():
		return routeRoster{err: fmt.Errorf("%w: %v", envoy.ErrUnavailable, ctx.Err())}
	}
}

// fetchRouteRoster runs one listener read, stores its answer for the TTL its outcome earns, and
// hands it to every read waiting on fetch. An answer's TTL counts from the moment the read was
// sent, so no read is served a list older than the TTL however long the listener took; a
// silence's counts from the moment it was known, so a read that timed out is still left alone.
func (s *server) fetchRouteRoster(ctx context.Context, fetch *rosterFetch) {
	sent := time.Now()
	fetch.roster = s.listRouteRoster(ctx)
	cache := &s.rosters
	expires := sent.Add(cache.ttl)
	if !fetch.roster.answered {
		expires = time.Now().Add(cache.silenceTTL)
	}
	cache.mu.Lock()
	cache.roster, cache.expires, cache.fetch = fetch.roster, expires, nil
	cache.mu.Unlock()
	close(fetch.done)
}

// listRouteRoster asks the listener who is running, within routeRosterTimeout.
func (s *server) listRouteRoster(ctx context.Context) routeRoster {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(s.rosters.timeout, routeRosterTimeout))
	defer cancel()
	sessions, err := s.deps.Envoy.Sessions(ctx)
	if err != nil {
		return routeRoster{err: err}
	}
	roster := routeRoster{
		answered: true,
		sessions: sessions,
		live:     make(map[string]struct{}, len(sessions)),
		holders:  map[string]string{},
	}
	lastSeen := map[string]int64{}
	for _, session := range sessions {
		roster.live[session.SessionID] = struct{}{}
		for _, role := range session.Roles {
			current, held := roster.holders[role]
			if held && (lastSeen[current] > session.LastSeen ||
				(lastSeen[current] == session.LastSeen && current < session.SessionID)) {
				continue
			}
			roster.holders[role] = session.SessionID
			lastSeen[session.SessionID] = session.LastSeen
		}
	}
	return roster
}

// reach judges one route against the roster: null for no route, unknown when the listener did
// not answer, live with the holder when a running session receives it, no_holder otherwise. A
// stored route that no longer parses reaches nobody.
func (r routeRoster) reach(route *string) model.IssueRouteReach {
	if route == nil {
		return model.IssueRouteReach{}
	}
	status := func(value string) *string { return &value }
	if !r.answered {
		return model.IssueRouteReach{RouteStatus: status(model.RouteUnknown)}
	}
	parsed, err := model.ParseRoute(*route)
	if err != nil {
		return model.IssueRouteReach{RouteStatus: status(model.RouteNoHolder)}
	}
	holder := ""
	switch parsed.Kind {
	case "role":
		holder = r.holders[parsed.ID]
	case "session":
		if _, live := r.live[parsed.ID]; live {
			holder = parsed.ID
		}
	}
	if holder == "" {
		return model.IssueRouteReach{RouteStatus: status(model.RouteNoHolder)}
	}
	return model.IssueRouteReach{RouteStatus: status(model.RouteLive), RouteHolder: &holder}
}

// parseRouteStatusFilter reads the `route_status` list filter: empty for none, or one of live,
// no_holder and unknown.
func parseRouteStatusFilter(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	switch value {
	case "", model.RouteLive, model.RouteNoHolder, model.RouteUnknown:
		return value, nil
	}
	return "", errorf(http.StatusBadRequest, "INVALID_ROUTE_STATUS",
		"route_status must be %s, %s or %s, got %q", model.RouteLive, model.RouteNoHolder, model.RouteUnknown, raw)
}
