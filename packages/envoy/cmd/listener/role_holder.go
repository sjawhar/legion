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
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

type roleClaimResolver interface {
	RoleClaim(role string) (store.RoleClaim, error)
	ReleaseExpiredRoleClaim(role, sessionID string, sessionTTL time.Duration) (store.ExpiredRoleClaimRelease, error)
}

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

const roleHolderResolutionAttempts = 2

// releaseExpiredRoleClaim is the single point in the listener that
// interprets store.ExpiredRoleClaimRelease's ExpiredRoleClaimSuperseded
// outcome. Both resolveLiveRoleHolder (HTTP role lookups) and
// resolveCoreRoleHolder (synchronous role-lane delivery, delivery.go) call
// it instead of registry.ReleaseExpiredRoleClaim directly, so a claim that
// raced a fresh SetRole before the conditional delete landed is reported as
// "must re-resolve" in exactly one place. A second, drifted branch on
// ExpiredRoleClaimSuperseded is exactly how a live replacement holder ends
// up reported as this call's stale verdict — the defect this helper exists
// to make structurally impossible.
func releaseExpiredRoleClaim(registry roleClaimResolver, role, sessionID string, sessionTTL time.Duration) (release store.ExpiredRoleClaimRelease, superseded bool, err error) {
	release, err = registry.ReleaseExpiredRoleClaim(role, sessionID, sessionTTL)
	if err != nil {
		return release, false, err
	}
	return release, release == store.ExpiredRoleClaimSuperseded, nil
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
// derived from ctx, so a lookup that cannot read the bucket answers its 500, a soft claim and a
// send their 503, inside the deadline their own caller holds.
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

// roleHolderSession is liveSessionEntry as the role paths ask it. They carry no request context of
// their own - the reaper and role delivery answer no caller at all - and they share one bound, so
// every decision that can take a role from its holder waits the same roleHolderReadTimeout out of
// one place rather than out of each call site. It is also the read behind the per-session route
// GET /v1/registry/<session_id>, whose caller is likewise about to decide that a session is gone,
// and which therefore reads through to the bucket for the same reason a send does.
func roleHolderSession(sessions *session.SessionRegistry, sessionID string, live func(session.SessionEntry) bool) (session.SessionEntry, error) {
	return liveSessionEntry(context.Background(), sessions, sessionID, roleHolderReadTimeout, live)
}

// registered is the liveness a lookup, a soft claim, the reaper and a session-addressed send ask of
// the session they name: any entry.
func registered(session.SessionEntry) bool { return true }

// roleHolderReadTimeout bounds the role paths' bucket read, well inside the listener's 10 s HTTP
// write timeout (main.go) and the 10 s JetStream MaxWait the read would otherwise wait out.
const roleHolderReadTimeout = 2 * time.Second

// errRoleHolderChanged is resolveLiveRoleHolder's answer when every one of
// roleHolderResolutionAttempts was spent on a supersession: each attempt found the claim's holder
// session gone and lost its conditional release to a fresh claim another writer had already
// installed. Every bus read answered, so this is a lost compare-and-swap against a live
// replacement holder and not a bus failure, and a caller recognises it rather than sweeping it in
// with the reads that did not answer (publishHandler).
var errRoleHolderChanged = errors.New("role holder changed while resolving")

func resolveLiveRoleHolder(registry roleClaimResolver, sessions *session.SessionRegistry, role string) (roleHolderResult, error) {
	for range roleHolderResolutionAttempts {
		claim, err := registry.RoleClaim(role)
		if err != nil {
			return roleHolderResult{}, err
		}
		if claim.HolderSessionID == "" {
			return roleHolderResult{state: roleHolderUnclaimed}, nil
		}
		entry, err := roleHolderSession(sessions, claim.HolderSessionID, registered)
		if errors.Is(err, nats.ErrKeyNotFound) {
			lastSeen := sessions.LastSeen(claim.HolderSessionID)
			release, superseded, err := releaseExpiredRoleClaim(registry, role, claim.HolderSessionID, sessions.TTL())
			if err != nil {
				return roleHolderResult{}, fmt.Errorf("drop expired role claim: %w", err)
			}
			if superseded {
				continue
			}
			return roleHolderResult{
				state:             roleHolderLapsed,
				holder:            claim.HolderSessionID,
				lastSeen:          lastSeen,
				lastSeenAvailable: lastSeen != 0,
				claimRelease:      release,
			}, nil
		}
		if err != nil {
			return roleHolderResult{}, fmt.Errorf("read role holder session: %w", err)
		}
		return roleHolderResult{
			state:  roleHolderLive,
			holder: claim.HolderSessionID,
			entry:  entry,
		}, nil
	}
	return roleHolderResult{}, errRoleHolderChanged
}
