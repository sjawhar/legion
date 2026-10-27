package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An issue's route says where its messages go; whether that reaches anyone is a separate fact,
// and an issue routed to a role nobody holds reads as owned while nobody is working it. So every
// issue read carries route_status beside the route, judged against the listener's live-session
// list (GET /v1/sessions, each row carrying the roles its session holds), never one lookup per
// issue. Every process judges against one stored copy of that list, the route_roster row
// (migration 0122): a read uses it while it is fresh by the database's clock, so a route_status
// can be up to routeRosterTTL (5 s) old, and inside that age every process reads a route the same
// way, and a filtered listing paged across processes is cut from one answer. A process that finds
// the row stale or missing asks the listener once however many of its reads are waiting
// (rosterCache's single-flight fetch), and stores the answer for every process. Like
// fetchHolderLiveness, the listener call runs with no transaction open and no pooled connection
// held (envoy_resolve.go carries the reason). A read that waits for the fetch waits on the
// listener too, so the rule binds every caller of readRouteRoster: listIssues and getIssue call it
// only once their pooled reads have finished, and a new caller must do the same.
//
// no_holder is one read and says only "nobody right now": a session that restarts or moves
// between agent boxes keeps its id but is absent from the listener for minutes. The read does not
// try to tell that gap from a vacancy with a last-seen time, because the listener offers none
// that a read may take: GET /v1/roles/{role} reports a lapsed holder's last_seen only by
// releasing the expired claim as it answers (cmd/listener resolveLiveRoleHolder), so a read
// would mutate role state and the next read would see "unclaimed" with no last_seen, and the
// last-seen memory behind it is the answering listener's own process cache, not the shared
// registry. The owner audit (skills/legion-controller/references/backlog-stewardship.md) and
// `dispatch issues --help` (route_status) ask for two reads ten minutes apart instead.

// routeRoster is one listener answer about who is running. answered is false when the listener
// is unconfigured or did not answer; every route then reads unknown. Its maps are read-only once
// built, since every read in this process served from one stored answer shares them
// (rosterCache.built).
type routeRoster struct {
	answered bool
	live     map[string]struct{}
	// holders maps a role to the live session holding it. The listener gives a role one holder
	// (last claim wins); should two live rows ever name one role, the most recently seen session
	// is named, and the id breaks a tie, so a read never flips between them.
	holders map[string]string
}

// routeRosterTimeout bounds the one listener read a roster fetch makes. The listener answers in
// milliseconds; a listener that has stopped answering would otherwise hold every issue list and
// detail waiting on that fetch for the client's whole five-second window, when the read's answer
// is only "unknown".
const routeRosterTimeout = 2 * time.Second

// routeRosterTTL is how long one listener answer serves every process's issue reads. A route that
// reads no_holder already means only "nobody right now" (above), and the owner audit reads ten
// minutes apart, so five seconds of age changes no decision; it takes the listener call off every
// read but one in five seconds, and the decode of the whole fleet's list off every read but one
// per process. A test process may set another (Deps.RouteRosterTTL).
// routeRosterSilenceTTL is how long a listener that failed or did not answer is left alone: every
// read in that window, on every process, reads unknown at once rather than waiting out
// routeRosterTimeout of its own. A failed fetch serves unknown, never an older answer, so this
// window changes no answer's age; it is as long as routeRosterTTL so that a listener that hangs
// costs fewer routeRosterTimeout waits: one shared wait per
// routeRosterTimeout+routeRosterSilenceTTL (7 s) rather than per 3 s. A route_status, unknown
// included, then lags the listener by at most routeRosterTTL, plus routeRosterTimeout while the
// listener hangs.
const (
	routeRosterTTL        = 5 * time.Second
	routeRosterSilenceTTL = 5 * time.Second
)

// rosterCache is how this process reads and refreshes the stored listener answer: the TTLs it
// stores an answer under, its one fetch in flight, and the roster it last built from the stored
// row, so a read of a row that has not changed decodes nothing.
type rosterCache struct {
	// ttl and silenceTTL are how long an answered and an unanswered fetch's stored answer serves
	// reads; register sets routeRosterTTL (or Deps.RouteRosterTTL) and routeRosterSilenceTTL.
	// Zero keeps nothing past the fetch, though the reads that arrive while it runs still share it.
	ttl        time.Duration
	silenceTTL time.Duration
	// timeout overrides the bound on a fetch's listener read; zero, which every server the
	// deployment builds leaves it, means routeRosterTimeout. Only a test that holds a fetch open
	// while other reads arrive sets it, so how long it holds the fetch is its own choice rather
	// than a race against two seconds of wall clock.
	timeout time.Duration

	mu sync.Mutex
	// built is the roster this process last built from a stored answer, and builtSent that
	// answer's sent_at, zero before any: a read of a row still carrying that sent_at takes built
	// rather than reading and decoding the row's sessions again.
	built     routeRoster
	builtSent time.Time
	// fetch is the listener read in flight, nil when none is.
	fetch *rosterFetch
}

// rosterFetch is one listener read that every read in this process arriving while it runs waits
// on. roster is set before done closes.
type rosterFetch struct {
	done   chan struct{}
	roster routeRoster
}

// rosterSession is one live session as the stored answer keeps it: only what a route's reach is
// judged by.
type rosterSession struct {
	SessionID string   `json:"session_id"`
	Roles     []string `json:"roles"`
	LastSeen  int64    `json:"last_seen"`
}

// readRouteRoster answers who is running: the stored answer while it is fresh, else the answer of
// this process's one fetch in flight, starting it when none is. A read never fails on the
// listener's account: a listener that is unconfigured, does not answer, or answers late leaves
// every route unknown, and so does a read whose own request ends while it waits; the fetch runs on
// for the other reads, and stores its answer for every process. A read whose own read of the row
// fails reads unknown too, since the stored answer is the one every process judges by.
func (s *server) readRouteRoster(ctx context.Context) routeRoster {
	if s.deps.Envoy == nil {
		return routeRoster{}
	}
	roster, fresh, now, err := s.storedRouteRoster(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: read the stored listener answer; routes read unknown", "error", err)
		}
		return routeRoster{}
	}
	if fresh {
		return roster
	}
	cache := &s.rosters
	cache.mu.Lock()
	fetch := cache.fetch
	if fetch == nil {
		fetch = &rosterFetch{done: make(chan struct{})}
		cache.fetch = fetch
		// The fetch is every waiting read's, so it runs on the server's lifetime rather than on
		// the request that started it, whose end must not end it and whose values (its
		// transaction-tracking mark among them) belong to that request alone; the fetch bound
		// still limits it, and a shutdown ends it. now, the database's clock on the read that
		// found the row stale, is when its call counts as sent.
		go s.fetchRouteRoster(s.lifetime(), fetch, now)
	}
	cache.mu.Unlock()
	select {
	case <-fetch.done:
		return fetch.roster
	case <-ctx.Done():
		return routeRoster{}
	}
}

// routeRosterQuery reads the stored answer and the database's now() in one statement, which
// answers one row whether route_roster holds one or not; $1 is the sent_at of the roster this
// process last built, and the sessions are left out when the row still carries it.
const routeRosterQuery = `
	select now(), coalesce(r.answered, false), r.sent_at, r.expires_at,
	       case when r.sent_at is distinct from $1::timestamptz then r.sessions end
	from (values (true)) as one (present)
	left join route_roster r on true`

// storedRouteRoster reads the route_roster row on the pool, outside any transaction, and answers
// the roster it holds, whether that is still fresh by the database's clock, and that clock's
// now(). No row reads as not fresh.
func (s *server) storedRouteRoster(ctx context.Context) (roster routeRoster, fresh bool, now time.Time, err error) {
	cache := &s.rosters
	cache.mu.Lock()
	built, builtSent := cache.built, cache.builtSent
	cache.mu.Unlock()
	var known *time.Time
	if !builtSent.IsZero() {
		known = &builtSent
	}
	var answered bool
	var sentAt, expiresAt *time.Time
	var sessions []byte
	if err := s.deps.Store.Pool.QueryRow(ctx, routeRosterQuery, known).Scan(&now, &answered, &sentAt, &expiresAt, &sessions); err != nil {
		return routeRoster{}, false, time.Time{}, err
	}
	if sentAt == nil {
		return routeRoster{}, false, now, nil
	}
	roster = built
	if sessions != nil {
		if roster, err = cache.remember(*sentAt, answered, sessions); err != nil {
			return routeRoster{}, false, time.Time{}, err
		}
	}
	return roster, expiresAt.After(now), now, nil
}

// remember builds the roster a stored answer holds and keeps it as this process's built roster.
func (c *rosterCache) remember(sent time.Time, answered bool, sessions []byte) (routeRoster, error) {
	roster := routeRoster{}
	if answered {
		var listed []rosterSession
		if err := json.Unmarshal(sessions, &listed); err != nil {
			return routeRoster{}, err
		}
		roster = buildRouteRoster(listed)
	}
	c.keep(sent, roster)
	return roster, nil
}

// keep makes roster, built from the answer sent at sent, this process's built roster, unless the
// one it holds came from a later answer.
func (c *rosterCache) keep(sent time.Time, roster routeRoster) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sent.After(c.builtSent) {
		c.built, c.builtSent = roster, sent
	}
}

// fetchRouteRoster runs one listener read, stores its answer for every process, and hands every
// read waiting on fetch the stored answer. An answer's TTL counts from sent, the database's clock
// before the call, so no read is served a list older than the TTL however long the listener took;
// a silence's counts from the moment it was known, so a read that timed out is still left alone.
func (s *server) fetchRouteRoster(ctx context.Context, fetch *rosterFetch, sent time.Time) {
	listed, answered := s.listRouteRoster(ctx)
	fetch.roster = s.storeRouteRoster(ctx, sent, listed, answered)
	cache := &s.rosters
	cache.mu.Lock()
	cache.fetch = nil
	cache.mu.Unlock()
	close(fetch.done)
}

// storeRouteRosterQuery stores one listener answer unless the stored one was sent later: a call
// that answers after a later call on another process never replaces that newer answer. It returns
// a row only when it stored this one. Every parameter is cast, since $1 and $3 each appear twice.
const storeRouteRosterQuery = `
	insert into route_roster (answered, sessions, sent_at, expires_at)
	values ($1::boolean, $2::jsonb, $3::timestamptz,
	        case when $1::boolean then $3::timestamptz + make_interval(secs => $4::float8)
	             else now() + make_interval(secs => $5::float8) end)
	on conflict (id) do update
	set answered = excluded.answered, sessions = excluded.sessions,
	    sent_at = excluded.sent_at, expires_at = excluded.expires_at
	where route_roster.sent_at < excluded.sent_at
	returning true`

// storeRouteRoster stores the answer of a call sent at sent and answers the roster every waiting
// read is served: this answer when it was stored, the newer stored one when it was not. A failed
// call is stored too (answered false, for the silence), so every process reads unknown together
// rather than each waiting out the listener on its own. Should the database refuse the write, the
// waiting reads in this process are served this answer, which no other process sees.
func (s *server) storeRouteRoster(ctx context.Context, sent time.Time, listed []rosterSession, answered bool) routeRoster {
	own := routeRoster{}
	sessions := []byte("[]")
	if answered {
		own = buildRouteRoster(listed)
		encoded, err := json.Marshal(listed)
		if err != nil {
			return own
		}
		sessions = encoded
	}
	var stored bool
	err := s.deps.Store.Pool.QueryRow(ctx, storeRouteRosterQuery,
		answered, sessions, sent, s.rosters.ttl.Seconds(), s.rosters.silenceTTL.Seconds()).Scan(&stored)
	if err == nil {
		s.rosters.keep(sent, own)
		return own
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if newer, _, _, err := s.storedRouteRoster(ctx); err == nil {
			return newer
		}
		return own
	}
	if ctx.Err() == nil {
		slog.Warn("dispatch: store the listener's answer; this process alone reads it", "error", err)
	}
	return own
}

// listRouteRoster asks the listener who is running, within routeRosterTimeout, and answers the
// sessions it listed, false when it did not answer.
func (s *server) listRouteRoster(ctx context.Context) ([]rosterSession, bool) {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(s.rosters.timeout, routeRosterTimeout))
	defer cancel()
	sessions, err := s.deps.Envoy.Sessions(ctx)
	if err != nil {
		return nil, false
	}
	listed := make([]rosterSession, len(sessions))
	for index, session := range sessions {
		listed[index] = rosterSession{SessionID: session.SessionID, Roles: session.Roles, LastSeen: session.LastSeen}
	}
	return listed, true
}

// buildRouteRoster indexes the listed sessions by id and by the roles they hold.
func buildRouteRoster(listed []rosterSession) routeRoster {
	roster := routeRoster{
		answered: true,
		live:     make(map[string]struct{}, len(listed)),
		holders:  map[string]string{},
	}
	lastSeen := map[string]int64{}
	for _, session := range listed {
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
