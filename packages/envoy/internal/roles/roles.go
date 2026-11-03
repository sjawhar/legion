// Package roles resolves a role's holder: the session its claim names, read as the listener's
// role routes, its role lane and Dispatch's outbox all read it, so the three cannot drift.
//
// No resolution here takes anything from a holder. It reads the role's claim and the holder's
// session record and says what it found; it never deletes a claim, and it never writes to the
// role bucket at all. A holder that stopped answering keeps its role until the role-claim reaper
// finds it gone (session.Gone), which is the one place a claim is taken away (dispatch://INFRA-392,
// R4): a lookup, a publish and a role-lane delivery attempt leave the claim and its claimed_at
// exactly as they were, so a stalled session that comes back still holds the role it had.
//
// A holder is judged by the state its own last registration puts it in (session.StateOf, on the
// windows the lookup runs with), never by how long a bucket keeps its record and never by a
// staleness compiled into this package.
package roles

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// ClaimStore is where a role's claim is read: store.Registry in the listener, Buckets in
// Dispatch. Reading is all a resolution asks of it.
type ClaimStore interface {
	// RoleClaim returns role's claim, read within ctx, or a zero claim without an error when the
	// role is unclaimed.
	RoleClaim(ctx context.Context, role string) (store.RoleClaim, error)
}

// SessionLookup reads a role holder's session record and judges it: the listener's session
// registry (cmd/listener's roleHolderSessions, which adds Holder to session.SessionRegistry) and
// Dispatch's Buckets.
type SessionLookup interface {
	// Holder returns sessionID's record, read within ctx, whatever state that record is in:
	// judging it is EntryState's. nats.ErrKeyNotFound says no record stands here at all; any
	// other error is a read that did not answer, on which nothing is decided about the holder.
	Holder(ctx context.Context, sessionID string) (session.SessionEntry, error)
	// EntryState is the state entry's last registration puts its session in (session.StateOf on
	// the windows this lookup runs with): session.SessionRegistry.EntryState in the listener,
	// Windows.EntryState in Buckets.
	EntryState(entry session.SessionEntry) session.State
	// LastSeen is the last registration this lookup can still name for a session whose record has
	// gone, and 0 when nothing here can date it.
	LastSeen(sessionID string) int64
}

// Windows are the session windows a holder's record is judged by and the clock it is judged at:
// what session.StateOf takes. A zero field is the package's own (session.LiveWindow,
// session.GoneHorizon, time.Now), so a lookup given no windows judges as a listener started with
// neither ENVOY_SESSION_LIVE_WINDOW nor ENVOY_SESSION_GONE_HORIZON does.
type Windows struct {
	Live    time.Duration
	Horizon time.Duration
	Now     func() time.Time
}

// EntryState is entry's state on these windows, which is what a SessionLookup that carries
// Windows judges a holder by (Buckets).
func (w Windows) EntryState(entry session.SessionEntry) session.State {
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	return session.StateOf(entry, now(), cmp.Or(w.Live, session.LiveWindow), cmp.Or(w.Horizon, session.GoneHorizon))
}

// State is what a role's claim came to when its holder was resolved. The strings are what the
// listener's answers carry: the 404's reason and the publish route's recorded outcome.
type State string

const (
	// Live: the role is claimed and its holder's record reads live.
	Live State = "live"
	// Unclaimed: the role has no claim.
	Unclaimed State = "unclaimed"
	// Lapsed: the claim's holder is not answering - unresponsive, or gone - and the claim stands
	// untouched. Resolution.HolderState says which of the two it is.
	Lapsed State = "holder_lapsed"
)

// Resolution is a resolved role holder. Nothing was taken from that holder to reach it.
type Resolution struct {
	// State is what the claim came to.
	State State
	// Holder is the session the claim named, on Live and Lapsed alike.
	Holder string
	// HolderState is the state the holder's own record puts it in (SessionLookup.EntryState):
	// session.Live on Live, and session.Unresponsive or session.Gone on Lapsed. A holder the
	// lookup holds no record of is session.Gone, since there is no registration to date it from.
	HolderState session.State
	// Entry is the holder's record when one was read, and the zero entry when none stands.
	Entry session.SessionEntry
	// LastSeen is when the holder last registered: its record's own time, or what the lookup
	// still remembers of a session whose record has gone (SessionLookup.LastSeen). It is 0 when
	// nothing can date the holder - no record and no memory of one - which is not the same as a
	// holder whose registration is past the gone horizon: a session bucket's maximum age can be
	// shorter than that horizon, so a caller that gives a dead holder's role up reads this before
	// it counts a holder dead.
	LastSeen int64
	// ClaimRevision is the claim store's revision of the claim record this resolution read, which
	// GET /v1/roles/<role> reports as claim_revision and a conditional claim names as its
	// precondition. A claim record stands behind every resolution that names a holder, since no
	// resolution here takes one away, so it is the revision that record was read at on Live and
	// Lapsed alike, and zero for Unclaimed. A resolution whose claim came from a firstHolder
	// (ResolveHolderFrom) read no record and carries zero whatever the store holds, since the
	// copy names a holder and no revision; a caller that reports the revision reads the claim
	// store.
	ClaimRevision uint64
}

// Step names the read a resolution failed at.
type Step uint8

const (
	// ClaimRead: the role's claim could not be read, or the resolution's context ended before the
	// read began.
	ClaimRead Step = iota
	// SessionRead: the holder's session record could not be read (an error other than
	// nats.ErrKeyNotFound).
	SessionRead
)

// ReadError is a resolution that failed at Step, about Holder for SessionRead. Its message is
// what an HTTP route names in its answer, so it names the read rather than the resolution; Err is
// the read's own error, which the role lane logs under a line of its own per step. It unwraps to
// Err, so a caller still tells store.ErrMalformedRoleClaim and a refused or oversized read by the
// read's own error.
type ReadError struct {
	Step   Step
	Holder string
	Err    error
}

func (e *ReadError) Error() string {
	if e.Step == SessionRead {
		return "read role holder session: " + e.Err.Error()
	}
	return e.Err.Error()
}

func (e *ReadError) Unwrap() error { return e.Err }

// readClaim reads role's claim for a resolution (ResolveHolderFrom). A resolution given a
// firstHolder reads it from there - the listener's registry passes its watched copy of the role
// bucket (store.Registry.CachedRoleHolder), which costs no round trip, so a role message the lane
// forwards reads no bucket - and one given none reads the claim store itself, which is what a
// caller answering within its own deadline, a route among them, does. The copy names a holder,
// not a whole claim, and the resolution reads nothing else from it.
func readClaim(ctx context.Context, claims ClaimStore, firstHolder func(role string) (string, error), role string) (store.RoleClaim, error) {
	if firstHolder == nil {
		return claims.RoleClaim(ctx, role)
	}
	holder, err := firstHolder(role)
	if err != nil {
		return store.RoleClaim{}, err
	}
	return store.RoleClaim{HolderSessionID: holder}, nil
}

// ResolveHolder resolves role's holder within ctx. It reads the role's claim (Unclaimed when
// there is none), then that holder's session record through sessions, and judges the record by
// the state it puts its session in (SessionLookup.EntryState): Live when it reads live, and
// Lapsed with that state otherwise - a holder past the live window, one past the gone horizon,
// and one with no record at all. The claim is left exactly as it is on every one of them, so a
// stalled holder's role and its claimed_at outlive any number of resolutions.
//
// A read that fails returns a *ReadError naming its step, with the claim's holder in the
// Resolution when one was read. Every read waits no longer than ctx, and no resolution starts
// once ctx has ended: that is ctx's error, wrapped to name the role, at ClaimRead.
//
// One resolution is one claim read: there is no release for a second read to see past.
//
// It reads the claim from claims. A caller whose first read is a watched copy of the role bucket
// resolves through ResolveHolderFrom instead.
func ResolveHolder(ctx context.Context, claims ClaimStore, sessions SessionLookup, role string) (Resolution, error) {
	return ResolveHolderFrom(ctx, claims, nil, sessions, role)
}

// ResolveHolderFrom is ResolveHolder reading its claim through readClaim, so a resolution given a
// firstHolder spends no claim-store round trip on it.
func ResolveHolderFrom(ctx context.Context, claims ClaimStore, firstHolder func(role string) (string, error), sessions SessionLookup, role string) (Resolution, error) {
	if err := ctx.Err(); err != nil {
		return Resolution{}, &ReadError{Step: ClaimRead, Err: fmt.Errorf("resolve role %s's holder: %w", role, err)}
	}
	claim, err := readClaim(ctx, claims, firstHolder, role)
	if err != nil {
		return Resolution{}, &ReadError{Step: ClaimRead, Err: err}
	}
	holder := claim.HolderSessionID
	if holder == "" {
		return Resolution{State: Unclaimed}, nil
	}
	entry, err := sessions.Holder(ctx, holder)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return Resolution{Holder: holder}, &ReadError{Step: SessionRead, Holder: holder, Err: err}
	}
	// No record to read is no moment to measure the holder from: session.Gone, as session.StateOf
	// judges an undated record, and dated by whatever the lookup still remembers.
	state := session.Gone
	if err == nil {
		state = sessions.EntryState(entry)
	} else {
		entry = session.SessionEntry{}
	}
	lastSeen := entry.UpdatedAt
	if lastSeen == 0 {
		lastSeen = sessions.LastSeen(holder)
	}
	// The claim record stands whatever the holder's state: nothing here releases one. So the
	// revision read is the revision a conditional claim can be made against, on Lapsed as on
	// Live, which is how a scheduler takes a role from a holder that never came back.
	resolved := Resolution{State: Lapsed, Holder: holder, HolderState: state, Entry: entry, LastSeen: lastSeen, ClaimRevision: claim.Revision}
	if state == session.Live {
		resolved.State = Live
	}
	return resolved, nil
}
