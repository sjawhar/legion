// Role-holder resolution: reading a role's claim and its holder's session through to the
// bucket, releasing a lapsed claim, answering the race a superseded release loses, and the 404
// that names a role with no live holder.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

type roleHolderState string

const (
	roleHolderLive      roleHolderState = "live"
	roleHolderUnclaimed roleHolderState = "unclaimed"
	roleHolderLapsed    roleHolderState = "holder_lapsed"
)

type roleHolderResult struct {
	state             roleHolderState
	holder            string
	entry             session.SessionEntry
	lastSeen          int64
	lastSeenAvailable bool
	claimRelease      store.ExpiredRoleClaimRelease
}

type roleHolderError struct {
	Error         string          `json:"error"`
	Reason        roleHolderState `json:"reason"`
	Holder        string          `json:"holder,omitempty"`
	LastSeen      *int64          `json:"last_seen,omitempty"`
	ClaimReleased *bool           `json:"claim_released,omitempty"`
}

func writeRoleHolderError(w http.ResponseWriter, role string, result roleHolderResult) {
	response := roleHolderError{
		Reason: result.state,
	}
	switch result.state {
	case roleHolderUnclaimed:
		response.Error = fmt.Sprintf("no holder for role %s", role)
	case roleHolderLapsed:
		response.Holder = result.holder
		lastSeen := " (last seen unavailable)"
		if result.lastSeenAvailable {
			response.LastSeen = &result.lastSeen
			lastSeen = fmt.Sprintf(" (last seen %d)", result.lastSeen)
		}
		claimReleased := result.claimRelease == store.ExpiredRoleClaimReleased
		response.ClaimReleased = &claimReleased
		switch result.claimRelease {
		case store.ExpiredRoleClaimReleased:
			response.Error = fmt.Sprintf(
				"role %s holder %s lapsed; claim released%s",
				role,
				result.holder,
				lastSeen,
			)
		case store.ExpiredRoleClaimMissing:
			response.Error = fmt.Sprintf(
				"role %s holder %s lapsed; claim was already absent%s",
				role,
				result.holder,
				lastSeen,
			)
		default:
			response.Error = fmt.Sprintf(
				"role %s holder %s lapsed; claim retained%s",
				role,
				result.holder,
				lastSeen,
			)
		}
	}
	writeJSON(w, http.StatusNotFound, response)
}

// writeRoleHolderChanged answers a role-holder resolution that lost the race for the role's claim
// (errRoleHolderChanged), for both routes that resolve one: asking is what the caller asked for, a
// "publish" or a "lookup". The 503 and the Retry-After are the same ask as a bus that did not
// answer - a fresh read wins this race as soon as the replacement claim settles - but the body is
// not: every bus read answered, so it names the race rather than an outage that never happened. It
// returns the status it wrote, for the publish route's record (refuseRoleHolderChanged).
func writeRoleHolderChanged(w http.ResponseWriter, role, asking string) int {
	w.Header().Set("Retry-After", "1")
	writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf(
		"envoy: the holder of role %s changed while this %s was resolving it", role, asking,
	))
	return http.StatusServiceUnavailable
}

// writeRoleClaimUnreadable answers a role-holder resolution that read a claim record it cannot
// decode (store.ErrMalformedRoleClaim), for both routes that resolve one. The bus answered, and
// will answer the same record again, so this is not the bus-unavailable 503: it is a 500 with no
// Retry-After, naming the role and the record. It returns the status it wrote, for the publish
// route's record (refuseRoleClaimUnreadable).
func writeRoleClaimUnreadable(w http.ResponseWriter, role string, err error) int {
	writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf(
		"envoy: the claim record of role %s is unreadable: %s", role, err,
	))
	return http.StatusInternalServerError
}

// liveSessionEntry is sessionID's session entry for a decision that acts on its absence: a role
// lookup or role delivery that releases a lapsed claim, a soft claim that supersedes a gone holder,
// the role reaper, and a session-addressed send, which tells its sender the destination is gone.
// live is the caller's test of an entry. The cache answers when it holds an entry live accepts;
// otherwise the session is read from the session bucket itself (session.SessionRegistry.Refresh),
// because the cache follows the bucket through a watcher and so trails it: a session another
// listener registered a moment ago, or one this listener has not re-read since its bus connection
// came back, is in the bucket before it is here. A role claim is read from the role bucket itself,
// so during a rolling deploy the old task resolves the claims the replacement accepts; a send asks
// the same question for the same reason, since a sender told its destination is gone acts on that
// answer - it routes around the pane, reports it, or stops - and the cache alone cannot tell a
// session that ended from one it has not caught up with. nats.ErrKeyNotFound is the bucket's word
// that the session is gone or fails live; any other error is a read that did not answer, on which
// nothing is taken from a holder and no send is refused as unaddressed. The bucket read gets bound,
// derived from ctx, so a lookup, a soft claim, a role-lane publish and a send that cannot read the
// bucket answer their 503 inside the deadline their own caller holds.
func liveSessionEntry(
	ctx context.Context,
	sessions *session.SessionRegistry,
	sessionID string,
	bound time.Duration,
	live func(session.SessionEntry) bool,
) (session.SessionEntry, error) {
	if entry, err := sessions.Get(sessionID); err == nil && live(entry) {
		return entry, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	entry, err := sessions.Refresh(readCtx, sessionID)
	if err == nil && !live(entry) {
		return session.SessionEntry{}, nats.ErrKeyNotFound
	}
	return entry, err
}

// roleHolderSession is liveSessionEntry as the role reaper asks it: the reaper answers no caller
// at all, so it holds no request context, and waits the role paths' roleHolderReadTimeout. A route
// does not come here: GET /v1/roles/<role>, a role-lane publish (resolveLiveRoleHolder), a soft
// claim (POST /v1/roles/set) and GET /v1/registry/<session_id> read through liveSessionEntry under
// their request's own context, with roleHolderReadTimeout as the cap, and role delivery
// (resolveCoreRoleHolder) reads through roleHolderSessions under context.Background().
func roleHolderSession(sessions *session.SessionRegistry, sessionID string, live func(session.SessionEntry) bool) (session.SessionEntry, error) {
	return liveSessionEntry(context.Background(), sessions, sessionID, roleHolderReadTimeout, live)
}

// roleHolderSessions is the listener's roles.SessionLookup: a holder read through
// liveSessionEntry within the resolution's context, the cache first and then the session bucket,
// each bucket read capped at roleHolderReadTimeout.
type roleHolderSessions struct {
	*session.SessionRegistry
}

func (s roleHolderSessions) Holder(ctx context.Context, sessionID string, live func(session.SessionEntry) bool) (session.SessionEntry, error) {
	return liveSessionEntry(ctx, s.SessionRegistry, sessionID, roleHolderReadTimeout, live)
}

// roleHolderReadTimeout bounds each of the role paths' session-bucket reads, well inside the
// listener's 10 s HTTP write timeout (main.go) and the 10 s JetStream MaxWait the read would
// otherwise wait out. resolveLiveRoleHolder's read is bounded by its caller's context as well, and
// so ends at the earlier of the two.
const roleHolderReadTimeout = 2 * time.Second

// errRoleHolderChanged is resolveLiveRoleHolder's answer when roles.ResolveHolder spent every one
// of its attempts on a supersession (roles.Churned): each attempt found the claim's holder session
// gone and lost its conditional release to a fresh claim another writer had already installed.
// Every bus read answered, so this is a lost compare-and-swap against a live replacement holder and
// not a bus failure, and a caller recognises it rather than sweeping it in with the reads that did
// not answer (publishHandler, roleGetHandler).
var errRoleHolderChanged = errors.New("role holder changed while resolving")

// resolveLiveRoleHolder resolves role's live holder for an HTTP route through roles.ResolveHolder,
// the resolution role delivery and Dispatch's outbox use, inside ctx: the route's own request
// context bounded by sendRequestBudget, so a bus that does not answer is reported to the caller
// within its attempt rather than after it. Any registered session is live here. Every bus call it
// makes waits no longer than ctx: the claim read and the conditional release, which store.Registry
// bounds by ctx, the holder's session read through liveSessionEntry, which also keeps
// roleHolderReadTimeout as its own cap, and a second attempt is not started once ctx has ended. An
// ended ctx is returned wrapped, naming the call that did not answer, and is answered as a bus that
// did not answer. A claim record that cannot be read is store.ErrMalformedRoleClaim, which the
// routes answer as such (writeRoleClaimUnreadable). A lapsed holder's last_seen is the session
// registry's memory of it.
func resolveLiveRoleHolder(ctx context.Context, registry roles.ClaimStore, sessions *session.SessionRegistry, role string) (roleHolderResult, error) {
	resolution, err := roles.ResolveHolder(ctx, registry, roleHolderSessions{sessions}, role, roles.Registered)
	if readErr, ok := errors.AsType[*roles.ReadError](err); ok {
		switch readErr.Step {
		case roles.SessionRead:
			return roleHolderResult{}, fmt.Errorf("read role holder session: %w", readErr.Err)
		case roles.Release:
			return roleHolderResult{}, fmt.Errorf("drop expired role claim: %w", readErr.Err)
		default:
			return roleHolderResult{}, readErr.Err
		}
	}
	if err != nil {
		return roleHolderResult{}, err
	}
	switch resolution.State {
	case roles.Unclaimed:
		return roleHolderResult{state: roleHolderUnclaimed}, nil
	case roles.Lapsed:
		lastSeen := sessions.LastSeen(resolution.Holder)
		return roleHolderResult{
			state:             roleHolderLapsed,
			holder:            resolution.Holder,
			lastSeen:          lastSeen,
			lastSeenAvailable: lastSeen != 0,
			claimRelease:      resolution.Release,
		}, nil
	case roles.Churned:
		return roleHolderResult{}, errRoleHolderChanged
	}
	return roleHolderResult{
		state:  roleHolderLive,
		holder: resolution.Holder,
		entry:  resolution.Entry,
	}, nil
}
