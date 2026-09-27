package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An issue's route says where its messages go; whether that reaches anyone is a separate fact,
// and an issue routed to a role nobody holds reads as owned while nobody is working it. So every
// issue read carries route_status beside the route, resolved from one GET /v1/sessions per
// request (each listener row carries the roles its session holds), never one lookup per issue,
// and stored nowhere. Like fetchLiveSessions, the listener call runs with no transaction open
// and no pooled connection held (envoy_resolve.go carries the reason).

// routeRoster is one listener answer about who is running, read once per request. answered is
// false when the listener is unconfigured or did not answer; every route then reads unknown.
type routeRoster struct {
	answered bool
	live     map[string]struct{}
	// holders maps a role to the live session holding it. The listener gives a role one holder
	// (last claim wins); should two live rows ever name one role, the most recently seen session
	// is named, and the id breaks a tie, so a read never flips between them.
	holders map[string]string
}

// routeRosterTimeout bounds the one listener read an issue read makes. The listener answers in
// milliseconds; a listener that has stopped answering would otherwise hold every issue list and
// detail for the client's whole five-second window, when the read's answer is only "unknown".
const routeRosterTimeout = 2 * time.Second

// readRouteRoster asks the listener who is running. A read never fails on the listener's
// account: a listener that is unconfigured, does not answer, or answers late leaves every route
// unknown.
func (s *server) readRouteRoster(ctx context.Context) routeRoster {
	if s.deps.Envoy == nil {
		return routeRoster{}
	}
	ctx, cancel := context.WithTimeout(ctx, routeRosterTimeout)
	defer cancel()
	sessions, err := s.deps.Envoy.Sessions(ctx)
	if err != nil {
		return routeRoster{}
	}
	roster := routeRoster{
		answered: true,
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
