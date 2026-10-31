package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
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
// try to tell that gap from a vacancy with a last-seen time, because the listener offers none
// that a read may take: GET /v1/roles/{role} reports a lapsed holder's last_seen only by
// releasing the expired claim as it answers (cmd/listener resolveRoleHolder), so a read
// would mutate role state and the next read would see "unclaimed" with no last_seen, and the
// last-seen memory behind it is the answering listener's own process cache, not the shared
// registry. The owner audit (skills/legion-controller/references/backlog-stewardship.md) and
// `dispatch issues --help` (route_status) ask for two reads ten minutes apart instead.

// routeRoster is one listener answer about who is running. answered is false when the listener
// is unconfigured or gave no list; every route then reads unknown, and reason says why:
// model.RouteReasonRefilling for the listener's refilling answer, whose Retry-After rides in
// retryAfter, model.RouteReasonUnreachable for anything else. Its maps and its session slice are
// read-only once built, since every read the cache serves shares them: a reader that sorts or
// edits the sessions copies them first, and never writes a session's Roles or Capabilities, which
// every copy shares too.
type routeRoster struct {
	answered bool
	// reason is why an unanswered roster gave no list: model.RouteReasonRefilling, with the
	// listener's Retry-After in retryAfter, or model.RouteReasonUnreachable.
	reason     string
	retryAfter string
	// sessions is the listener's answer, in the listener's order. It carries the unresponsive
	// sessions as well as the live ones (the client asks with include=unresponsive), so a row's
	// presence is no longer liveness: every reader tests the row's State.
	sessions []envoy.Session
	// entries maps a session id to the index of its row in sessions, which holds the row itself:
	// a roster of a few hundred sessions keeps one copy of each. A session the registry holds no
	// record of, live or unresponsive, is simply absent, which is the one membership test left
	// in this file and means gone.
	entries map[string]int
	// holders maps a role to the session holding it, whatever that session's state. The listener
	// gives a role one holder (last claim wins); should two rows ever name one role, the most
	// recently seen session is named, and the id breaks a tie, so a read never flips between
	// them - and the most recently seen is the live one when a lapsed claim lingers beside it.
	holders map[string]string
	// err is why the roster did not answer: the listener client's own error, or, for a read whose
	// request ended while it waited on the shared fetch, envoy.ErrUnavailable wrapping the
	// request's context error, so it always matches envoy.ErrUnavailable. It is nil when the
	// roster answered or the listener is unconfigured.
	err error
}

// state is the state the roster holds for a session: the row's own, or envoy.StateGone when the
// roster holds no row for it. The roster lists every session the registry holds a record of,
// live and unresponsive alike, so an absent row is a session the registry has no record of -
// which is exactly gone. It answers for an unanswered roster too, and a caller that has not
// checked answered must not read that as gone.
func (r routeRoster) state(sessionID string) string {
	index, held := r.entries[sessionID]
	if !held {
		return envoy.StateGone
	}
	return r.sessions[index].State
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
// routeRosterTTL, plus routeRosterTimeout while the listener hangs. A listener refilling its list
// is left alone for the silence window like any other unanswered fetch: its reason, and the
// Retry-After it sent, are the roster's and reach every read the window serves.
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
// answer, answers late, or is refilling its list leaves the roster unanswered with the reason
// that says which, and so does a read whose own request ends while it waits (its err then wraps
// the request's context error); the fetch runs on for the other reads, and its answer fills the
// cache. The fetch runs on the process's lifetime, so once a shutdown begins it ends or fails at
// once, and that failure is kept for the silence window. What an unanswered roster means is the
// caller's: an issue read reads every route unknown, with the roster's reason, and still
// succeeds, while listAgents, which has nothing to list without the listener, answers 503 with
// the roster's err, or, during a shutdown drain, asks the listener on its own request.
func (s *server) readRouteRoster(ctx context.Context) routeRoster {
	if s.deps.Envoy == nil {
		return routeRoster{reason: model.RouteReasonUnreachable}
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
		return routeRoster{reason: model.RouteReasonUnreachable, err: fmt.Errorf("%w: %v", envoy.ErrUnavailable, ctx.Err())}
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
	var refilling *envoy.RefillingError
	switch {
	case errors.As(err, &refilling):
		return routeRoster{reason: model.RouteReasonRefilling, retryAfter: refilling.RetryAfter, err: err}
	case err != nil:
		return routeRoster{reason: model.RouteReasonUnreachable, err: err}
	}
	roster := routeRoster{
		answered: true,
		sessions: sessions,
		entries:  make(map[string]int, len(sessions)),
		holders:  map[string]string{},
	}
	lastSeen := map[string]int64{}
	for index, session := range sessions {
		roster.entries[session.SessionID] = index
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

// reach judges one route against the roster: null for no route, unknown with the roster's reason
// when the listener gave no list, live with the holder when a live session receives it,
// holder_unresponsive with that same holder when the session that receives it has missed its
// heartbeats, no_holder otherwise. A stored route that no longer parses reaches nobody.
//
// An unresponsive holder is named, not dropped: the message is stored for it and waits, so a
// reader that wants "nobody is working this" must ask for no_holder, which excludes it.
func (r routeRoster) reach(route *string) model.IssueRouteReach {
	if route == nil {
		return model.IssueRouteReach{}
	}
	if !r.answered {
		return model.IssueRouteReach{
			RouteStatus: new(model.RouteUnknown), RouteStatusReason: new(r.reason),
		}
	}
	parsed, err := model.ParseRoute(*route)
	if err != nil {
		return model.IssueRouteReach{RouteStatus: new(model.RouteNoHolder)}
	}
	holder := ""
	switch parsed.Kind {
	case "role":
		holder = r.holders[parsed.ID]
	case "session":
		holder = parsed.ID
	}
	switch r.state(holder) {
	case envoy.StateLive:
		return model.IssueRouteReach{RouteStatus: new(model.RouteLive), RouteHolder: &holder}
	case envoy.StateUnresponsive:
		return model.IssueRouteReach{
			RouteStatus: new(model.RouteHolderUnresponsive), RouteHolder: &holder,
		}
	}
	return model.IssueRouteReach{RouteStatus: new(model.RouteNoHolder)}
}

// claimLiveness is what an issue read says about the session holding its claim: the state this
// roster read for it, when it was last seen, and from when another agent may take the claim
// without a person's force. It answers the zero value - every field null - for an issue nobody
// holds and for one a person holds, which has no session state to read.
//
// This is the read side of the two sources the claim rule has, and the slacker of them: the
// roster is the server's copy of the listener's session list, up to routeRosterTTL (5 s) old,
// while a claim write re-reads the holder's own record from the listener (fetchHolderLiveness)
// and decides on that. The two can therefore disagree by that much; a read that cannot judge at
// all answers model.ClaimHolderUnknown and never fails, because an issue page must render while
// the listener is down, and only the write refuses.
func (r routeRoster) claimLiveness(claim *model.IssueClaim, now time.Time) model.IssueClaimLiveness {
	if claim == nil || claim.Actor.Kind != "session" {
		return model.IssueClaimLiveness{}
	}
	if !r.answered {
		return model.IssueClaimLiveness{HolderState: new(model.ClaimHolderUnknown)}
	}
	held := r.state(claim.Actor.ID)
	liveness := model.IssueClaimLiveness{HolderState: new(held)}
	if index, found := r.entries[claim.Actor.ID]; found && r.sessions[index].LastSeen > 0 {
		lastSeen := time.UnixMilli(r.sessions[index].LastSeen).UTC()
		liveness.LastSeen = &lastSeen
	}
	switch held {
	case envoy.StateUnresponsive:
		if liveness.LastSeen != nil {
			takeable := liveness.LastSeen.Add(contracts.IssueClaimUnresponsiveGrace)
			liveness.TakeableAt = &takeable
		}
	case envoy.StateGone:
		// Gone is takeable now, and now is when this read said so; the registry keeps no
		// last_seen for a session it holds no record of, so there is no earlier instant to name.
		takeable := now.UTC()
		liveness.TakeableAt = &takeable
	}
	return liveness
}

// parseRouteStatusFilter reads the `route_status` list filter: empty for none, or one of live,
// holder_unresponsive, no_holder and unknown.
func parseRouteStatusFilter(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	switch value {
	case "", model.RouteLive, model.RouteHolderUnresponsive, model.RouteNoHolder, model.RouteUnknown:
		return value, nil
	}
	return "", errorf(http.StatusBadRequest, "INVALID_ROUTE_STATUS",
		"route_status must be %s, %s, %s or %s, got %q",
		model.RouteLive, model.RouteHolderUnresponsive, model.RouteNoHolder, model.RouteUnknown, raw)
}
