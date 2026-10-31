// Role-holder resolution: reading a role's claim and its holder's session through to the bucket,
// judging that holder's state, and the 404 that names a role whose holder is not answering. No
// resolution here releases a claim: a holder that stopped answering keeps its role until the role
// reaper finds it gone (session.Gone), which is the one place a claim is taken away.

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
}

type roleHolderState string

const (
	roleHolderLive      roleHolderState = "live"
	roleHolderUnclaimed roleHolderState = "unclaimed"
	roleHolderLapsed    roleHolderState = "holder_lapsed"
)

// roleHolderResult is one resolution of a role's holder (resolveRoleHolder). holder names the
// claim's holder whenever one was read, and holderState the state that holder's record puts it in
// (session.StateOf); entry is set only for roleHolderLive; lastSeen and lastSeenAvailable only for
// roleHolderLapsed.
type roleHolderResult struct {
	state             roleHolderState
	holder            string
	holderState       session.State
	entry             session.SessionEntry
	lastSeen          int64
	lastSeenAvailable bool
}

// roleHolderStep names the read a role-holder resolution failed at, so a caller that reports the
// failure in its own words (the role lane's log lines) can tell which read it was.
type roleHolderStep uint8

const (
	// roleHolderClaimRead: the role claim could not be read, or the resolution's context ended
	// before the read began.
	roleHolderClaimRead roleHolderStep = iota
	// roleHolderSessionRead: the holder's session entry could not be read.
	roleHolderSessionRead
)

// roleHolderFailure is the error of a resolution that did not resolve. Its message is what an
// HTTP route names in its answer, so it names the step; cause is the read's own error, which the
// role lane logs under a line of its own per step. It unwraps to cause, so a route still tells
// store.ErrMalformedRoleClaim and a refused or oversized read (writeBusUnavailable) by the read's
// own error.
type roleHolderFailure struct {
	step  roleHolderStep
	cause error
}

func (f *roleHolderFailure) Error() string {
	if f.step == roleHolderSessionRead {
		return "read role holder session: " + f.cause.Error()
	}
	return f.cause.Error()
}

func (f *roleHolderFailure) Unwrap() error { return f.cause }

// roleHolderError is the 404 of a role with no live holder. HolderState says which of the three
// states the claim's holder is in, so a caller can tell a holder that is merely unresponsive -
// stalled, still claiming, still being stored for - from one that is gone. ClaimReleased is on
// every lapsed answer and is always false: a lookup takes nothing from a holder, whatever its
// state, so a caller that used to read this answer as "the role is free now" is told plainly that
// the claim still stands.
type roleHolderError struct {
	Error         string          `json:"error"`
	Reason        roleHolderState `json:"reason"`
	Holder        string          `json:"holder,omitempty"`
	HolderState   session.State   `json:"holder_state,omitempty"`
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
		response.HolderState = result.holderState
		lastSeen := " (last seen unavailable)"
		if result.lastSeenAvailable {
			response.LastSeen = &result.lastSeen
			lastSeen = fmt.Sprintf(" (last seen %d)", result.lastSeen)
		}
		claimReleased := false
		response.ClaimReleased = &claimReleased
		response.Error = fmt.Sprintf(
			"role %s holder %s lapsed; claim retained%s",
			role,
			result.holder,
			lastSeen,
		)
	}
	writeJSON(w, http.StatusNotFound, response)
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

// liveSessionEntry is sessionID's record and the state that record puts its session in, for a
// decision that acts on a session having stopped answering: a role lookup, a soft claim, the
// reapers, a session-addressed send and an agent delivery. accept is the caller's rule about that
// state - every caller that may not act on a stalled session passes notGone, and only the two
// that push to a session's own port pass sessionIsLive.
//
// The cache answers when it holds a record accept takes (session.SessionRegistry.Lookup, which
// judges the record's own age and never its membership of the cache); otherwise the session is
// read from the session bucket itself (Refresh), because the cache follows the bucket through a
// watcher and so trails it: a session another listener registered a moment ago, or one this
// listener has not re-read since its bus connection came back, is in the bucket before it is
// here. A role claim is read from the role bucket itself, so during a rolling deploy the old task
// resolves the claims the replacement accepts; a send asks the same question for the same reason,
// since a sender told its destination is gone acts on that answer - it routes around the pane,
// reports it, or stops - and the cache alone cannot tell a session that ended from one it has not
// caught up with.
//
// nats.ErrKeyNotFound is the answer for a session the bucket holds no record of and for one whose
// record accept refuses, and the state returned with it says which: session.Gone for the first,
// and the state the record really carries for the second, so a caller can report what it refused
// on rather than call every such session gone. Any other error is a read that did not answer, on
// which nothing is taken from a holder and no send is refused as unaddressed. The bucket read gets
// bound, derived from ctx, so a lookup, a soft claim, a role send through the publish route and a
// send that cannot read the bucket answer their 503 inside the deadline their own caller holds.
func liveSessionEntry(
	ctx context.Context,
	sessions *session.SessionRegistry,
	sessionID string,
	bound time.Duration,
	accept func(session.State) bool,
) (session.SessionEntry, session.State, error) {
	if entry, state := sessions.Lookup(sessionID); accept(state) {
		return entry, state, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	entry, err := sessions.Refresh(readCtx, sessionID)
	if err != nil {
		return session.SessionEntry{}, session.Gone, err
	}
	state := sessions.EntryState(entry)
	if !accept(state) {
		return entry, state, nats.ErrKeyNotFound
	}
	return entry, state, nil
}

// roleHolderSession is liveSessionEntry as the role paths that hold no request context ask it: the
// reaper answers no caller at all, and shares one bound with role delivery's resolution
// (resolveRoleHolder under context.Background()), so every such decision that can take a role from
// its holder waits the same roleHolderReadTimeout. A route does not come here: GET
// /v1/roles/<role>, a role send through the publish route (resolveRoleHolder), a soft claim (POST
// /v1/roles/set) and GET /v1/registry/<session_id> read through liveSessionEntry under their
// request's own context, with roleHolderReadTimeout as the cap.
func roleHolderSession(sessions *session.SessionRegistry, sessionID string, accept func(session.State) bool) (session.SessionEntry, session.State, error) {
	return liveSessionEntry(context.Background(), sessions, sessionID, roleHolderReadTimeout, accept)
}

// notGone is the state test of every caller that may not act on a session for having stopped
// answering: a lookup, a soft claim's liveness read, both reapers, a session-addressed send, an
// agent delivery and the publish route's first resolution of a role. A session within the gone
// horizon of its last registration is unresponsive, not dead: it keeps its claims, its messages
// are stored for it, and no replacement is started for it, so only session.Gone is an absence
// these callers may act on.
func notGone(state session.State) bool { return state != session.Gone }

// sessionIsLive is the state test of the two decisions that may act only on a session that is
// answering now: a push to a session's serving port, and a soft claim that takes a role from the
// session that holds it.
func sessionIsLive(state session.State) bool { return state == session.Live }

// roleHolderReadTimeout bounds each of the role paths' session-bucket reads, well inside the
// listener's 10 s HTTP write timeout (main.go) and the 10 s JetStream MaxWait the read would
// otherwise wait out. A route's resolveRoleHolder read is bounded by its caller's context as well,
// and so ends at the earlier of the two.
const roleHolderReadTimeout = 2 * time.Second

// resolveRoleHolder resolves role's current holder inside ctx: the claim's holder, live when
// accept takes the state of its session record (liveSessionEntry). A holder whose record accept
// refuses has lapsed, and its claim is left exactly as it is - this resolution writes nothing to
// the role bucket, so a stalled holder's claim and its claimed_at outlive any number of lookups,
// and only the role reaper, finding the holder gone, ever releases one.
//
// Every caller passes sessionIsLive: a role reaches its holder only while that holder is
// answering, and a holder past the live window answers neither a forward's receipt nor anything
// else. What changed is what that costs the holder - nothing. The lookup and the publish route
// report it lapsed with its state and its claim intact, and role delivery fails the one attempt
// and leaves the claim for the holder to come back to, where both used to release it. accept
// stays an argument because the state a caller may act on is the one thing these paths could
// reasonably differ on, and reading which rule a call uses at the call site is how the two kinds
// of caller stay told apart (liveSessionEntry's own callers do differ: notGone).
//
// Every bus call it makes waits no longer than ctx: the claim read, which store.Registry bounds by
// ctx, and the holder's session read through liveSessionEntry, which also keeps
// roleHolderReadTimeout as its own cap. A route passes its request's own context, bounded by its
// budget, so a bus that does not answer is reported to the caller within its attempt rather than
// after it; role delivery passes context.Background().
//
// An error is a *roleHolderFailure naming the read that failed; with it the result still names
// the claim's holder when one was read, so the role lane's log line and the publish route's 504
// can name it. It unwraps to the read's own error: an ended ctx, wrapped naming the call that did
// not answer, which a route answers as a bus that did not answer; or store.ErrMalformedRoleClaim,
// a claim record that cannot be read (writeRoleClaimUnreadable).
func resolveRoleHolder(ctx context.Context, registry roleClaimResolver, sessions *session.SessionRegistry, role string, accept func(session.State) bool) (roleHolderResult, error) {
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
	entry, state, err := liveSessionEntry(ctx, sessions, holder, roleHolderReadTimeout, accept)
	if err == nil {
		return roleHolderResult{state: roleHolderLive, holder: holder, holderState: state, entry: entry}, nil
	}
	if !errors.Is(err, nats.ErrKeyNotFound) {
		return roleHolderResult{holder: holder, holderState: state}, &roleHolderFailure{step: roleHolderSessionRead, cause: err}
	}
	// A holder whose record still stands reports that record's own last registration; one the
	// bucket holds nothing for reports the final heartbeat the registry still remembers for it,
	// and nothing when that too has been forgotten.
	lastSeen := entry.UpdatedAt
	if lastSeen == 0 {
		lastSeen = sessions.LastSeen(holder)
	}
	return roleHolderResult{
		state:             roleHolderLapsed,
		holder:            holder,
		holderState:       state,
		lastSeen:          lastSeen,
		lastSeenAvailable: lastSeen != 0,
	}, nil
}
