// The role paths' reads of a session: the record a role-holder resolution judges its holder by
// (roleHolderSessions, the session side of internal/roles' one resolution), the reads a soft
// claim weighs the holder it found by, and the 404 that names a role whose holder is not
// answering. Nothing here, and nothing in internal/roles, releases a claim: a holder that
// stopped answering keeps its role until the role reaper finds it gone (session.Gone), which is
// the one place a claim is taken away.

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

// roleHolderSessions is the session side of a role-holder resolution (roles.SessionLookup): this
// listener's session registry, which judges a record on its own windows (EntryState) and
// remembers a departed session's last registration (LastSeen) already, with the one read a
// resolution adds.
type roleHolderSessions struct{ *session.SessionRegistry }

// Holder is the claim holder's record for a resolution, whatever state that record is in: the
// cached one while it reads live, and otherwise the session bucket itself (Refresh), because the
// cache follows the bucket through a watcher and so trails it - a session another listener
// registered a moment ago, or one this listener has not re-read since its bus connection came
// back, is in the bucket before it is here, and a holder that re-registered while its cached
// record stood stalled must be found. The bucket read is bounded by roleHolderReadTimeout within
// ctx, as every role path's session read is, so a route answers inside the budget its own caller
// holds.
//
// nats.ErrKeyNotFound is the registry's word that no record stands, a key no session can register
// under among them (session.SessionRegistry.Refresh, bus.ErrRefused); the resolution reads it as
// a holder with nothing to date it, and takes nothing from it either way. Any other error is a
// read that did not answer.
func (s roleHolderSessions) Holder(ctx context.Context, sessionID string) (session.SessionEntry, error) {
	if entry, state := s.Lookup(sessionID); state == session.Live {
		return entry, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, roleHolderReadTimeout)
	defer cancel()
	return s.Refresh(readCtx, sessionID)
}

// roleHolderError is the 404 of a role with no live holder. HolderState says which of the three
// states the claim's holder is in, so a caller can tell a holder that is merely unresponsive -
// stalled, still claiming, still being stored for - from one that is gone. ClaimReleased is on
// every lapsed answer and is always false: a lookup takes nothing from a holder, whatever its
// state, so a caller that used to read this answer as "the role is free now" is told plainly that
// the claim still stands.
type roleHolderError struct {
	Error       string        `json:"error"`
	Reason      roles.State   `json:"reason"`
	Holder      string        `json:"holder,omitempty"`
	HolderState session.State `json:"holder_state,omitempty"`
	// ClientInstanceID is the process the lapsed holder's claim names
	// (store.RoleClaim.ClientInstanceID), as the 200 reports it beside the live holder: it is
	// how a client reading this answer sees that the role is held under its own session id by
	// another process. Absent for a role no claim stands for, and for a claim that names no
	// process - one an earlier build wrote, or one whose claimant stated none.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
	// HandedBack is the hand-back mark the lapsed holder's claim carries
	// (store.RoleClaim.HandedBack), as the 200 reports it beside the live holder: the holder
	// gave this role back through a release that could not free the claim, so no registration
	// renews it any more and the role claim reaper frees the role once that renewal has
	// lapsed, a live window and up to one reap interval later.
	// Absent for a role no claim stands for, and for a claim no such release reached.
	HandedBack bool `json:"handed_back,omitempty"`
	// ClaimRevision is the role's claim revision, as the 200 reports it: the revision of the
	// claim a lapsed holder still holds, and zero for an unclaimed role, so a scheduler
	// reading this 404 can claim the role with if_claim_revision and have that claim refused
	// if the role moved in between.
	ClaimRevision uint64 `json:"claim_revision"`
	LastSeen      *int64 `json:"last_seen,omitempty"`
	ClaimReleased *bool  `json:"claim_released,omitempty"`
}

func writeRoleHolderError(w http.ResponseWriter, role string, result roles.Resolution) {
	response := roleHolderError{
		Reason:        result.State,
		ClaimRevision: result.ClaimRevision,
	}
	switch result.State {
	case roles.Unclaimed:
		response.Error = fmt.Sprintf("no holder for role %s", role)
	case roles.Lapsed:
		response.Holder = result.Holder
		response.HolderState = result.HolderState
		response.ClientInstanceID = result.HolderClientInstanceID
		response.HandedBack = result.HolderHandedBack
		lastSeen := " (last seen unavailable)"
		if result.LastSeen != 0 {
			response.LastSeen = &result.LastSeen
			lastSeen = fmt.Sprintf(" (last seen %d)", result.LastSeen)
		}
		claimReleased := false
		response.ClaimReleased = &claimReleased
		response.Error = fmt.Sprintf(
			"role %s holder %s lapsed; claim retained%s",
			role,
			result.Holder,
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
// decision that turns on whether that session is still answering: a soft claim, the reapers, a
// session lookup, a session-addressed send and an agent delivery. accept is the caller's rule
// about that state.
//
// A caller that may act on a session which merely stopped answering passes notGone: the send
// route, whose message is stored for such a session; the agent delivery path; both reapers
// (sessionMayBeLive); and GET /v1/registry/<id> asked with include=unresponsive. The two that
// may act only on a session answering now pass sessionIsLive: that same lookup asked without
// include, and a soft claim weighing the holder it found (roleSupersessionVerdict). A
// role-holder resolution reads its holder through roleHolderSessions.Holder instead, which asks
// the same two reads in the same order and leaves the judgement to internal/roles.
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
// read did not answer. One of two functions answers it - roleSupersessionVerdict for a holder
// that is another session, roleOwnSessionVerdict for the role held under the claimant's own
// session id - so the route states the rule that decides a holder nowhere and words only the
// answers.
type roleSupersession uint8

const (
	// roleStaysWithHolder: nothing is taken. A live holder the claimant did not name as its
	// predecessor keeps the role, and so does a named predecessor still live inside another
	// client instance; store.Registry.SetRoleWithPrevious then answers the 409 that names it.
	roleStaysWithHolder roleSupersession = iota
	// roleTakenFromHolder: the holder gives the role up, because it is not live - stalled past
	// the live window, or with no record at all - or because it is the claimant's named
	// predecessor and its entry carries the claimant's own client instance id, the same
	// conversation continuing inside the instance that held the role. The role held under the
	// claimant's own session id is taken on this verdict too, where the standing claim names
	// the claimant's own process or nobody renewed it inside the live window
	// (roleOwnSessionVerdict).
	roleTakenFromHolder
	// roleTakenFromUnidentifiedPredecessor is roleTakenFromHolder for the one case
	// session.InstanceMatches accepts without two ids to compare: a live named predecessor
	// where its entry or the claimant carries no client instance id, a client predating the
	// field. It is a verdict of its own only so the route can log that takeover without
	// restating the rule that reached it.
	roleTakenFromUnidentifiedPredecessor
	// roleHeldByAnotherProcess: the role is held under the claimant's own session id by a
	// process that still answers for its claim, so nothing is taken and
	// store.Registry.SetRoleWithPrevious answers the 409 naming that session id
	// (roleOwnSessionVerdict). It is roleStaysWithHolder's refusal told apart from it only so
	// the route can log the second process being held off, which a bare 409 naming the
	// claimant's own session id does not show an operator.
	roleHeldByAnotherProcess
	// roleHolderUnread: a read that did not answer - the holder's session record, or the role
	// claim view a renewal is only worth reading while it can answer - so nothing is taken
	// from anyone. It comes with the error the route answers (writeBusUnavailable), already
	// worded for the read that failed.
	roleHolderUnread
	// roleUnclaimed: no claim stands for the role, so there is no holder to supersede and
	// nothing to log. Only the route's own read of the claim answers it (roleSetHandler),
	// never the two verdict functions, which are asked about a holder.
	roleUnclaimed
)

// roleOwnSessionVerdict decides a soft claim of a role whose standing claim is held under the
// claimant's own session id, which is no reason to hand the role over: a second process can
// resume a live session's id, and the session record names whichever process registered that id
// last, not the one holding the role. So this verdict is made on the standing claim alone -
// claim, as the route read it - and reads no session record at all, which is what tells it apart
// from roleSupersessionVerdict. claimantInstanceID is the client instance the claimant's request
// stated, with no fallback to the registration: an absent instance names no process and matches
// no claim, so a request that states none reaches the renewal test rather than a case of its own.
//
// A claim naming the claimant's very process is the holder re-claiming its own role
// (store.RoleClaim.HeldByProcess), granted at once however recently it was renewed: it rests on
// no renewal, and refusing it would strand the holder's own process after a listener restart.
//
// Otherwise the claim goes to the claimant only once nobody has answered for it inside liveWindow
// (store.Registry.ClaimRenewedWithin, over the registry's own clock). A claim an earlier build
// wrote names no process and is weighed at the moment it was claimed
// (store.RoleClaim.LastAnsweredAt), so it is kept while it is newer than the window and granted
// once it is older - the same rule, not a case of its own.
//
// That reading is only worth anything while renewals can still move. What moves them is a
// registration (POST /v1/interests/subscribe), which renews every claim the registering session
// holds through the role claim view (store.Registry.RenewRoleClaims, over RolesHeldBy); while
// that view cannot answer - the state a listener restart leaves for as long as its first read of
// the bucket takes - no registration renews any claim, so every claim's renewal is ageing with
// nothing able to move it. A refill longer than the live window would then read a live holder's
// claim as abandoned and hand its role to a second process, the very takeover the refusal exists
// to make. So the renewal test fails closed on the view's own state (store.Registry.RoleViewErr,
// the one test of it, which makes no bucket call): roleHolderUnread with the error the route
// answers, nothing claimed and nothing written, and the claimant retries.
func roleOwnSessionVerdict(registry *store.Registry, claim store.RoleClaim, claimantInstanceID string, liveWindow time.Duration) (roleSupersession, error) {
	if claim.HeldByProcess(claimantInstanceID) {
		return roleTakenFromHolder, nil
	}
	if registry.ClaimRenewedWithin(claim, liveWindow) {
		return roleHeldByAnotherProcess, nil
	}
	if viewErr := registry.RoleViewErr(); viewErr != nil {
		return roleHolderUnread, fmt.Errorf("read role claim view: %w", viewErr)
	}
	return roleTakenFromHolder, nil
}

// roleSupersessionVerdict decides whether a soft claim takes the role from the holder it found,
// for the one route that asks (roleSetHandler). holder is the session the role's claim names,
// neither empty nor the claimant itself; previous is the predecessor the claimant declared,
// already trimmed; claimantInstanceID is the client instance the claimant's request stated, and
// where it stated none the one its own registration carries - the route's single fallback to
// that record, which keeps a client that sends no instance deciding here exactly as it did
// before requests carried one.
//
// A holder the claimant did not name as its predecessor is weighed on its state alone
// (liveSessionEntry with sessionIsLive): anything but live - unresponsive past the live window,
// or gone - and the role is the claimant's to take, which is how a relaunched Legion role
// recovers its own role once its predecessor stopped answering; live, it keeps the role.
//
// The declared predecessor is weighed on that same state and on the client instance it belongs
// to, and is read from the bucket itself (bucketSessionEntry, which says why a cached entry
// cannot decide this). Not live, it gives the role up whatever instance it ran in. Live, it
// gives it up only to a claimant session.InstanceMatches accepts, which is its own client
// instance, or a side that carries no id at all - the one of those two arms the route logs, and
// the only reason this function tells them apart. So knowing a live predecessor's session id is
// not enough to take its role out of the client instance that still runs it.
//
// The error is non-nil only with roleHolderUnread, wrapped as the route reports it, naming the
// read that failed. A key the bucket does not hold is that bucket's word that the session is
// gone, not a failure; every other read failure takes nothing from a holder. Both reads are
// bounded by roleHolderReadTimeout within ctx, so the route answers inside the budget its own
// caller holds.
func roleSupersessionVerdict(ctx context.Context, sessions *session.SessionRegistry, holder, previous, claimantInstanceID string) (roleSupersession, error) {
	if previous == "" || holder != previous {
		_, _, err := liveSessionEntry(ctx, sessions, holder, roleHolderReadTimeout, sessionIsLive)
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
	case !sessionIsLive(sessions.EntryState(predecessor)):
		return roleTakenFromHolder, nil
	case !session.InstanceMatches(predecessor.ClientInstanceID, claimantInstanceID):
		return roleStaysWithHolder, nil
	case predecessor.ClientInstanceID == "" || claimantInstanceID == "":
		return roleTakenFromUnidentifiedPredecessor, nil
	default:
		return roleTakenFromHolder, nil
	}
}

// notGone is the state test of every caller that may act on a session that stopped answering as
// it acts on one that answers: a session-addressed send, an agent delivery, both reapers
// (sessionMayBeLive), a session lookup asked for the unresponsive ones (include=unresponsive)
// and the listing of an ad-hoc topic send's recipients (topicRecipients). A session within the
// gone horizon of its last registration is unresponsive, not dead: it keeps its claims, its
// messages are stored for it, and no replacement is started for it, so only session.Gone is an
// absence these callers may act on.
func notGone(state session.State) bool { return state != session.Gone }

// sessionIsLive is the state test of the decisions that may act only on a session that is
// answering now, because each of them waits for that session to answer or moves its role: a soft
// claim weighing the holder it found and the predecessor it was given (roleSupersessionVerdict);
// a role send's test of whether its first resolution's holder still needs resolving again
// (sendToRole); and GET /v1/registry/<id> asked without include=unresponsive, which answers for
// the live sessions alone. A role-holder resolution asks the same question of its holder, and
// asks it of internal/roles, which reads the holder's state itself (roles.ResolveHolder):
// a role reaches its holder only while that holder can answer a forward's receipt.
func sessionIsLive(state session.State) bool { return state == session.Live }

// roleHolderReadTimeout bounds each of the role paths' session-bucket reads, well inside the
// listener's 10 s HTTP write timeout (main.go) and the 10 s JetStream MaxWait the read would
// otherwise wait out. A resolution's read (roleHolderSessions.Holder) is bounded by its caller's
// context as well, and so ends at the earlier of the two.
const roleHolderReadTimeout = 2 * time.Second
