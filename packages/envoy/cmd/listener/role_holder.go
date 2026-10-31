// Role-holder resolution: reading a role's claim and its holder's session through to the
// bucket, releasing a lapsed claim, answering the race a superseded release loses, deciding
// whether a soft claim takes the role from the holder it found, and the 404 that names a role
// with no live holder.

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
// lastSeenAvailable and claimRelease only for roleHolderLapsed. claimRevision is the role
// bucket revision of the claim record this resolution leaves behind, which GET /v1/roles/<role>
// reports as claim_revision and a conditional claim names as if_claim_revision: the revision the
// claim was read at while a record stands, and zero where none does - an unclaimed role, and a
// lapsed holder whose claim this resolution released or found already gone.
type roleHolderResult struct {
	state             roleHolderState
	holder            string
	entry             session.SessionEntry
	lastSeen          int64
	lastSeenAvailable bool
	claimRelease      store.ExpiredRoleClaimRelease
	claimRevision     uint64
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
	// roleHolderChurn: every release found the claim superseded by a fresher one, past
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
	Error  string          `json:"error"`
	Reason roleHolderState `json:"reason"`
	Holder string          `json:"holder,omitempty"`
	// ClaimRevision is the role's claim revision, as the 200 reports it: zero for an
	// unclaimed role and for a lapsed holder whose claim is gone, so a scheduler reading this
	// 404 can claim the role with if_claim_revision and have that claim refused if any record
	// appears first.
	ClaimRevision uint64 `json:"claim_revision"`
	LastSeen      *int64 `json:"last_seen,omitempty"`
	ClaimReleased *bool  `json:"claim_released,omitempty"`
}

func writeRoleHolderError(w http.ResponseWriter, role string, result roleHolderResult) {
	response := roleHolderError{
		Reason:        result.state,
		ClaimRevision: result.claimRevision,
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

// releaseExpiredRoleClaim is the single point in the listener that interprets
// store.ExpiredRoleClaimRelease's ExpiredRoleClaimSuperseded outcome. resolveRoleHolder, the one
// resolution behind HTTP role lookups, the publish route's role sends and role delivery, calls it
// instead of registry.ReleaseExpiredRoleClaim directly, so a claim that raced a fresh SetRole
// before the conditional delete landed is reported as "must re-resolve" in exactly one place. A
// second, drifted branch on ExpiredRoleClaimSuperseded is exactly how a live replacement holder
// ends up reported as this call's stale verdict — the defect this helper exists to make
// structurally impossible.
//
// The release waits no longer than ctx: a route passes its request's own context, role delivery
// context.Background(). store.Registry.ReleaseExpiredRoleClaim says why a delete it abandons is safe
// to leave running.
func releaseExpiredRoleClaim(ctx context.Context, registry roleClaimResolver, role, sessionID string, sessionTTL time.Duration) (release store.ExpiredRoleClaimRelease, superseded bool, err error) {
	release, err = registry.ReleaseExpiredRoleClaim(ctx, role, sessionID, sessionTTL)
	if err != nil {
		return store.ExpiredRoleClaimRetained, false, err
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

// bucketSessionEntry is sessionID's entry read from the session bucket itself
// (session.SessionRegistry.Refresh) and from nowhere else, for the one decision that must not be
// made on a cached entry: a soft claim naming a predecessor, which reads that predecessor's client
// instance id to decide whether the role passes to the claimant (POST /v1/roles/set). It asks the
// cache nothing, not even as a first answer the way liveSessionEntry does, because the cache
// follows the bucket through a watcher and so trails it: the entry it still holds can carry an
// instance id that session has since replaced, or no id at all where the bucket has one, and a
// stale entry would then decide who keeps a role - handing it to a client instance the predecessor
// no longer belongs to, which is the whole point of reading the id.
//
// nats.ErrKeyNotFound is the bucket's word that the session is gone, and the caller acts on it;
// any other error is a read that did not answer, on which nothing is taken from a holder. The read
// gets bound, derived from ctx, so a route answers its 503 inside the deadline its own caller
// holds.
func bucketSessionEntry(ctx context.Context, sessions *session.SessionRegistry, sessionID string, bound time.Duration) (session.SessionEntry, error) {
	readCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return sessions.Refresh(readCtx, sessionID)
}

// roleSupersession is what a soft claim may do to the role holder it found (POST /v1/roles/set,
// the issue's R6): take the role from that holder, leave it where it is, or neither, because a
// session read did not answer. roleSupersessionVerdict answers one of these, so the route states
// the rule that decides it nowhere and words only the answers.
type roleSupersession uint8

const (
	// roleStaysWithHolder: nothing is taken. A live holder the claimant did not name as its
	// predecessor keeps the role, and so does a named predecessor still live inside another
	// client instance; store.Registry.SetRoleWithPrevious then answers the 409 that names it.
	roleStaysWithHolder roleSupersession = iota
	// roleTakenFromHolder: the holder gives the role up, because it has no live session entry
	// in the bucket, or because it is the claimant's named predecessor and its entry carries
	// the claimant's own client instance id - the same conversation continuing inside the
	// instance that held the role.
	roleTakenFromHolder
	// roleTakenFromUnidentifiedPredecessor is roleTakenFromHolder for the one case
	// session.InstanceMatches accepts without two ids to compare: a live named predecessor
	// where its entry or the claimant carries no client instance id, a client predating the
	// field. It is a verdict of its own only so the route can log that takeover without
	// restating the rule that reached it.
	roleTakenFromUnidentifiedPredecessor
	// roleHolderUnread: a session read that did not answer, so nothing is taken from anyone.
	// It comes with the error the route answers (writeBusUnavailable), already worded for the
	// read that failed.
	roleHolderUnread
)

// roleSupersessionVerdict decides whether a soft claim takes the role from the holder it found,
// for the one route that asks (roleSetHandler). holder is the session the role's claim names,
// neither empty nor the claimant itself; previous is the predecessor the claimant declared,
// already trimmed; claimantInstanceID is the client instance the claimant's own registration
// carries.
//
// A holder the claimant did not name as its predecessor is weighed on liveness alone
// (liveSessionEntry): gone from the session bucket it has lapsed and the role is free, live it
// keeps the role. The declared predecessor is weighed on the client instance it belongs to as
// well, and is read from the bucket itself (bucketSessionEntry, which says why a cached entry
// cannot decide this): gone, it gives the role up; live, it gives it up only to a claimant
// session.InstanceMatches accepts, which is its own client instance, or a side that carries no
// id at all - the one of those two arms the route logs, and the only reason this function tells
// them apart.
//
// The error is non-nil only with roleHolderUnread, wrapped as the route reports it, naming the
// read that failed. A key the bucket does not hold is that bucket's word that the session is
// gone, not a failure; every other read failure takes nothing from a holder. Both reads are
// bounded by roleHolderReadTimeout within ctx, so the route answers inside the budget its own
// caller holds.
func roleSupersessionVerdict(ctx context.Context, sessions *session.SessionRegistry, holder, previous, claimantInstanceID string) (roleSupersession, error) {
	if previous == "" || holder != previous {
		_, err := liveSessionEntry(ctx, sessions, holder, roleHolderReadTimeout, registered)
		switch {
		case errors.Is(err, nats.ErrKeyNotFound):
			return roleTakenFromHolder, nil
		case err != nil:
			return roleHolderUnread, fmt.Errorf("read role holder liveness: %w", err)
		default:
			return roleStaysWithHolder, nil
		}
	}
	predecessor, err := bucketSessionEntry(ctx, sessions, holder, roleHolderReadTimeout)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
		return roleTakenFromHolder, nil
	case err != nil:
		return roleHolderUnread, fmt.Errorf("read role predecessor registration: %w", err)
	case !session.InstanceMatches(predecessor.ClientInstanceID, claimantInstanceID):
		return roleStaysWithHolder, nil
	case predecessor.ClientInstanceID == "" || claimantInstanceID == "":
		return roleTakenFromUnidentifiedPredecessor, nil
	default:
		return roleTakenFromHolder, nil
	}
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
// holder session gone and lost its conditional release to a fresh claim another writer had already
// installed. Every bus read answered, so this is a lost compare-and-swap against a live
// replacement holder and not a bus failure, and a caller recognises it rather than sweeping it in
// with the reads that did not answer (publishHandler's role send, roleGetHandler).
var errRoleHolderChanged = errors.New("role holder changed while resolving")

// resolveRoleHolder resolves role's current holder inside ctx: the claim's holder, live when live
// accepts its session entry (liveSessionEntry). A holder whose entry is gone or that live refuses
// has lapsed: its claim is released through releaseExpiredRoleClaim, and when the release finds the
// claim already superseded by a fresh one the role is resolved again, bounded to
// roleHolderResolutionAttempts, so a claim raced by a live replacement resolves to that
// replacement instead of reporting a session the fresh claim has already superseded.
//
// live is the one rule its callers differ on. An HTTP lookup (GET /v1/roles/<role>) and the
// publish route's first resolution pass registered, any entry. Role delivery — the role lane
// (resolveCoreRoleHolder) and the publish route's attempts — passes laneHolderFresh, which
// distrusts a holder that has not heartbeated within session.ClaimStaleAfter even while its
// registry cache entry has not expired, since a dropped delivery is costlier than an HTTP 404.
//
// Every bus call it makes waits no longer than ctx: the claim read and the conditional release,
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
func resolveRoleHolder(ctx context.Context, registry roleClaimResolver, sessions *session.SessionRegistry, role string, live func(session.SessionEntry) bool) (roleHolderResult, error) {
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
			return roleHolderResult{state: roleHolderLive, holder: holder, entry: entry, claimRevision: claim.Revision}, nil
		}
		if !errors.Is(err, nats.ErrKeyNotFound) {
			return roleHolderResult{holder: holder}, &roleHolderFailure{step: roleHolderSessionRead, cause: err}
		}
		lastSeen := sessions.LastSeen(holder)
		release, superseded, err := releaseExpiredRoleClaim(ctx, registry, role, holder, sessions.TTL())
		if err != nil {
			return roleHolderResult{holder: holder}, &roleHolderFailure{step: roleHolderRelease, cause: err}
		}
		if superseded {
			continue
		}
		// A released claim and one already gone leave no record, so the revision this
		// resolution read names nothing a later claim can be conditional on; a retained one
		// (a restored claim still inside its grace) is still in the bucket at the revision
		// read, and a conditional claim against it is exactly how a scheduler takes a role
		// its restart-grace holder never came back for.
		claimRevision := claim.Revision
		if release == store.ExpiredRoleClaimReleased || release == store.ExpiredRoleClaimMissing {
			claimRevision = 0
		}
		return roleHolderResult{
			state:             roleHolderLapsed,
			holder:            holder,
			lastSeen:          lastSeen,
			lastSeenAvailable: lastSeen != 0,
			claimRelease:      release,
			claimRevision:     claimRevision,
		}, nil
	}
	return roleHolderResult{}, &roleHolderFailure{step: roleHolderChurn, cause: errRoleHolderChanged}
}
