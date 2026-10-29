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
	RoleClaim(ctx context.Context, role string) (store.RoleClaim, error)
	ReleaseExpiredRoleClaim(ctx context.Context, role, sessionID string, sessionTTL time.Duration) (store.ExpiredRoleClaimRelease, error)
}

type roleHolderState string

const (
	roleHolderLive      roleHolderState = "live"
	roleHolderUnclaimed roleHolderState = "unclaimed"
	roleHolderLapsed    roleHolderState = "holder_lapsed"
)

// roleHolderResult is one resolution of a role's holder (resolveRoleHolder). holder names the
// claim's holder whenever one was read; entry is set only for roleHolderLive; lastSeen,
// lastSeenAvailable and claimRelease only for roleHolderLapsed.
type roleHolderResult struct {
	state             roleHolderState
	holder            string
	entry             session.SessionEntry
	lastSeen          int64
	lastSeenAvailable bool
	claimRelease      store.ExpiredRoleClaimRelease
}

// roleHolderStep names the read a role-holder resolution failed at, so a caller that reports the
// failure in its own words (the role lane's log lines) can tell which read it was.
type roleHolderStep uint8

const (
	// roleHolderClaimRead: the role claim could not be read, or the resolution's context ended
	// before an attempt began.
	roleHolderClaimRead roleHolderStep = iota
	// roleHolderSessionRead: the holder's session entry could not be read.
	roleHolderSessionRead
	// roleHolderRelease: the lapsed holder's claim could not be released.
	roleHolderRelease
	// roleHolderChurn: every attempt found the claim superseded by a fresher one, past
	// roleHolderResolutionAttempts.
	roleHolderChurn
)

// roleHolderFailure is the error of a resolution that did not resolve. Its message is what an
// HTTP route names in its answer, so it names the step; cause is the read's own error, which the
// role lane logs under a line of its own per step, and errRoleHolderChanged for roleHolderChurn.
// It unwraps to cause, so a route still tells errRoleHolderChanged, store.ErrMalformedRoleClaim
// and a refused or oversized read (writeBusUnavailable) by the read's own error.
type roleHolderFailure struct {
	step  roleHolderStep
	cause error
}

func (f *roleHolderFailure) Error() string {
	switch f.step {
	case roleHolderSessionRead:
		return "read role holder session: " + f.cause.Error()
	case roleHolderRelease:
		return "drop expired role claim: " + f.cause.Error()
	}
	return f.cause.Error()
}

func (f *roleHolderFailure) Unwrap() error { return f.cause }

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

// lapsedRoleClaim is what a resolution does about the claim of a holder whose session is gone
// (resolveRoleHolder). It answers the release outcome the 404 reports, whether the claim was
// superseded by a fresh one while the resolution ran, which makes the resolution read the role
// again, and the failure of its own bus call.
//
// There are two, and which one a caller passes is what makes a read a read: releaseExpiredRoleClaim
// for a decision that takes the role from its holder (role delivery, and the publish route's role
// send), keepLapsedRoleClaim for one that only reports it (GET /v1/roles/<role>).
type lapsedRoleClaim func(ctx context.Context, registry roleClaimResolver, role, sessionID string, sessionTTL time.Duration) (store.ExpiredRoleClaimRelease, bool, *roleHolderFailure)

// releaseExpiredRoleClaim is the single point in the listener that interprets
// store.ExpiredRoleClaimRelease's ExpiredRoleClaimSuperseded outcome. resolveRoleHolder, the one
// resolution behind the publish route's role sends and role delivery, calls it
// instead of registry.ReleaseExpiredRoleClaim directly, so a claim that raced a fresh SetRole
// before the conditional delete landed is reported as "must re-resolve" in exactly one place. A
// second, drifted branch on ExpiredRoleClaimSuperseded is exactly how a live replacement holder
// ends up reported as this call's stale verdict — the defect this helper exists to make
// structurally impossible.
//
// The release waits no longer than ctx: a route passes its request's own context, role delivery
// context.Background(). store.Registry.ReleaseExpiredRoleClaim says why a delete it abandons is safe
// to leave running.
func releaseExpiredRoleClaim(ctx context.Context, registry roleClaimResolver, role, sessionID string, sessionTTL time.Duration) (store.ExpiredRoleClaimRelease, bool, *roleHolderFailure) {
	release, err := registry.ReleaseExpiredRoleClaim(ctx, role, sessionID, sessionTTL)
	if err != nil {
		return store.ExpiredRoleClaimRetained, false, &roleHolderFailure{step: roleHolderRelease, cause: err}
	}
	return release, release == store.ExpiredRoleClaimSuperseded, nil
}

// keepLapsedRoleClaim is releaseExpiredRoleClaim's other half: the lapsed-claim policy of a read,
// which writes nothing at all. It deletes no claim, so a lookup of a role whose holder has gone
// leaves that claim exactly as it found it, and the role reaper (store.Registry.ReapRoleClaims,
// every five minutes) and the next claim of the role (POST /v1/roles/set, whose soft claim
// supersedes a holder that is gone) are what end it. Reaping is theirs because a read that
// released the claim it was asked about could not be used to watch a role: the answer and the
// act would be one call, so a monitor would take the claim it was checking from a holder that had
// gone quiet, and nobody could ask "who holds this?" without risking the answer.
//
// It reads the claim once more in place of the conditional delete, which costs the same one bus
// call, so the resolution's supersession branch still holds for a read: a claim that names another
// session now was superseded while this resolution ran, which the release path learns from its
// lost compare-and-swap and a read learns by reading again, and either way the role is resolved
// again rather than answered with a holder a fresh claim has already replaced. A claim that is
// gone by then names no holder either, so the second attempt answers the role unclaimed, which is
// what the bucket now holds. Its own read failing is a claim read, and is reported as one.
func keepLapsedRoleClaim(ctx context.Context, registry roleClaimResolver, role, sessionID string, _ time.Duration) (store.ExpiredRoleClaimRelease, bool, *roleHolderFailure) {
	claim, err := registry.RoleClaim(ctx, role)
	if err != nil {
		return store.ExpiredRoleClaimRetained, false, &roleHolderFailure{step: roleHolderClaimRead, cause: err}
	}
	return store.ExpiredRoleClaimRetained, claim.HolderSessionID != sessionID, nil
}

// liveSessionEntry is sessionID's session entry for a decision that acts on its absence: a role
// lookup, which reports a lapsed holder, role delivery, which releases its claim, a soft claim
// that supersedes a gone holder,
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
// derived from ctx, so a lookup, a soft claim, a role send through the publish route and a send that cannot read the
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

// roleHolderSession is liveSessionEntry as the role paths that hold no request context ask it: the
// reaper answers no caller at all, and shares one bound with role delivery's resolution
// (resolveRoleHolder under context.Background()), so every such decision that can take a role from
// its holder waits the same roleHolderReadTimeout. A route does not come here: GET
// /v1/roles/<role>, a role send through the publish route (resolveRoleHolder), a soft claim (POST
// /v1/roles/set) and GET /v1/registry/<session_id> read through liveSessionEntry under their
// request's own context, with roleHolderReadTimeout as the cap.
func roleHolderSession(sessions *session.SessionRegistry, sessionID string, live func(session.SessionEntry) bool) (session.SessionEntry, error) {
	return liveSessionEntry(context.Background(), sessions, sessionID, roleHolderReadTimeout, live)
}

// registered is the liveness a lookup, a soft claim, the reaper, a session-addressed send and the
// publish route's first resolution of a role ask of the session they name: any entry.
func registered(session.SessionEntry) bool { return true }

// roleHolderReadTimeout bounds each of the role paths' session-bucket reads, well inside the
// listener's 10 s HTTP write timeout (main.go) and the 10 s JetStream MaxWait the read would
// otherwise wait out. A route's resolveRoleHolder read is bounded by its caller's context as well,
// and so ends at the earlier of the two.
const roleHolderReadTimeout = 2 * time.Second

// errRoleHolderChanged is the cause of resolveRoleHolder's roleHolderChurn failure, when every one
// of roleHolderResolutionAttempts was spent on a supersession: each attempt found the claim's
// holder session gone and another writer's fresh claim already in its place — a lost conditional
// release for a resolution that releases, a second read naming another holder for one that keeps
// the claim (lapsedRoleClaim). Every bus read answered, so this is a live replacement holder and
// not a bus failure, and a caller recognises it rather than sweeping it in
// with the reads that did not answer (publishHandler's role send, roleGetHandler).
var errRoleHolderChanged = errors.New("role holder changed while resolving")

// resolveRoleHolder resolves role's current holder inside ctx: the claim's holder, live when live
// accepts its session entry (liveSessionEntry). A holder whose entry is gone or that live refuses
// has lapsed, and what becomes of its claim is onLapsed's (lapsedRoleClaim): released, or kept by
// a resolution that only reports it. When onLapsed finds the claim already superseded by a fresh
// one the role is resolved again, bounded to
// roleHolderResolutionAttempts, so a claim raced by a live replacement resolves to that
// replacement instead of reporting a session the fresh claim has already superseded.
//
// live and onLapsed are the two rules its callers differ on. An HTTP lookup (GET /v1/roles/<role>)
// and the publish route's first resolution pass registered, any entry. Role delivery — the role
// lane (resolveCoreRoleHolder) and the publish route's attempts — passes laneHolderFresh, which
// distrusts a holder that has not heartbeated within session.ClaimStaleAfter even while its
// registry cache entry has not expired, since a dropped delivery is costlier than an HTTP 404.
// Those delivery paths and the publish route pass releaseExpiredRoleClaim; the lookup, which
// answers a question and acts on nothing, passes keepLapsedRoleClaim.
//
// Every bus call it makes waits no longer than ctx: the claim read and onLapsed's own call,
// which store.Registry bounds by ctx, and the holder's session read through liveSessionEntry, which
// also keeps roleHolderReadTimeout as its own cap; a second attempt is not started once ctx has
// ended. A route passes its request's own context, bounded by its budget, so a bus that does not
// answer is reported to the caller within its attempt rather than after it; role delivery passes
// context.Background().
//
// An error is a *roleHolderFailure naming the read that failed; with it the result still names
// the claim's holder when one was read, so the role lane's log line and the publish route's 504
// can name it. It unwraps to the read's own error: an ended ctx, wrapped naming the call that did
// not answer, which a route answers as a bus that did not answer; store.ErrMalformedRoleClaim, a
// claim record that cannot be read (writeRoleClaimUnreadable); or errRoleHolderChanged.
func resolveRoleHolder(ctx context.Context, registry roleClaimResolver, sessions *session.SessionRegistry, role string, live func(session.SessionEntry) bool, onLapsed lapsedRoleClaim) (roleHolderResult, error) {
	for range roleHolderResolutionAttempts {
		if err := ctx.Err(); err != nil {
			return roleHolderResult{}, &roleHolderFailure{step: roleHolderClaimRead, cause: fmt.Errorf("resolve role %s's holder: %w", role, err)}
		}
		claim, err := registry.RoleClaim(ctx, role)
		if err != nil {
			return roleHolderResult{}, &roleHolderFailure{step: roleHolderClaimRead, cause: err}
		}
		holder := claim.HolderSessionID
		if holder == "" {
			return roleHolderResult{state: roleHolderUnclaimed}, nil
		}
		entry, err := liveSessionEntry(ctx, sessions, holder, roleHolderReadTimeout, live)
		if err == nil {
			return roleHolderResult{state: roleHolderLive, holder: holder, entry: entry}, nil
		}
		if !errors.Is(err, nats.ErrKeyNotFound) {
			return roleHolderResult{holder: holder}, &roleHolderFailure{step: roleHolderSessionRead, cause: err}
		}
		lastSeen := sessions.LastSeen(holder)
		release, superseded, failure := onLapsed(ctx, registry, role, holder, sessions.TTL())
		if failure != nil {
			return roleHolderResult{holder: holder}, failure
		}
		if superseded {
			continue
		}
		return roleHolderResult{
			state:             roleHolderLapsed,
			holder:            holder,
			lastSeen:          lastSeen,
			lastSeenAvailable: lastSeen != 0,
			claimRelease:      release,
		}, nil
	}
	return roleHolderResult{}, &roleHolderFailure{step: roleHolderChurn, cause: errRoleHolderChanged}
}
