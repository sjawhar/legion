// Package roles resolves a role's holder: the session its claim names, read as the listener's role
// lane and Dispatch's outbox both read it, so the two cannot drift.
package roles

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// ClaimStore is where a role's claim is read and an expired one released: store.Registry in the
// listener, Buckets in Dispatch.
type ClaimStore interface {
	// RoleClaim returns role's claim, read within ctx, or a zero claim without an error when the
	// role is unclaimed.
	RoleClaim(ctx context.Context, role string) (store.RoleClaim, error)
	// ReleaseExpiredRoleClaim deletes role's claim while sessionID still holds it, as a compare and
	// set on the claim's revision, waiting no longer than ctx, and says what came of it
	// (store.ExpiredRoleClaimRelease).
	ReleaseExpiredRoleClaim(ctx context.Context, role, sessionID string, sessionTTL time.Duration) (store.ExpiredRoleClaimRelease, error)
}

// SessionLookup reads a role holder's session entry.
type SessionLookup interface {
	// Holder returns sessionID's entry when live accepts it, read within ctx. nats.ErrKeyNotFound
	// says the session is gone or live refuses it; any other error is a read that did not answer.
	Holder(ctx context.Context, sessionID string, live func(session.SessionEntry) bool) (session.SessionEntry, error)
	// TTL is the session bucket's liveness window, which a release passes on.
	TTL() time.Duration
}

// State is what a role's claim came to when its holder was resolved.
type State uint8

const (
	// Live: the role is claimed and its holder's session is there, as live accepted it.
	Live State = iota
	// Unclaimed: the role has no claim.
	Unclaimed
	// Lapsed: the claim's holder is gone, or live refused it, and the claim's release was tried;
	// Resolution.Release says what came of it.
	Lapsed
	// Churned: every attempt found the claim it tried to release superseded by a fresh one.
	Churned
)

// Resolution is a resolved role holder. Holder is the session the claim named (Live, Lapsed),
// Entry that session's entry (Live), and Release what the release of a lapsed claim came to:
// store.ExpiredRoleClaimReleased, store.ExpiredRoleClaimMissing when the claim was already gone,
// or store.ExpiredRoleClaimRetained when the claim store kept it (a listener's grace for a claim
// it restored).
type Resolution struct {
	State   State
	Holder  string
	Entry   session.SessionEntry
	Release store.ExpiredRoleClaimRelease
}

// Step names the read a resolution failed at.
type Step uint8

const (
	// ClaimRead: the role's claim could not be read.
	ClaimRead Step = iota
	// SessionRead: the holder's session could not be read (an error other than
	// nats.ErrKeyNotFound).
	SessionRead
	// Release: the expired claim's release failed.
	Release
)

func (s Step) String() string {
	switch s {
	case ClaimRead:
		return "read the role's claim"
	case SessionRead:
		return "read the holder's session"
	case Release:
		return "release the expired claim"
	}
	return fmt.Sprintf("step %d", uint8(s))
}

// ReadError is a resolution that failed at Step, about Holder for SessionRead and Release.
type ReadError struct {
	Step   Step
	Holder string
	Err    error
}

func (e *ReadError) Error() string {
	if e.Holder == "" {
		return fmt.Sprintf("%s: %v", e.Step, e.Err)
	}
	return fmt.Sprintf("%s of holder %s: %v", e.Step, e.Holder, e.Err)
}

func (e *ReadError) Unwrap() error { return e.Err }

// resolutionAttempts bounds how often ResolveHolder reads the claim again after a release found it
// superseded.
const resolutionAttempts = 2

// Registered is the liveness that counts any registered session as live: any entry.
func Registered(session.SessionEntry) bool { return true }

// Fresh reports whether entry heartbeated within session.ClaimStaleAfter of now.
func Fresh(entry session.SessionEntry, now time.Time) bool {
	return entry.UpdatedAt > 0 && now.UnixMilli()-entry.UpdatedAt < session.ClaimStaleAfter.Milliseconds()
}

// readClaim reads role's claim for attempt of a resolution (ResolveHolderFrom, whose attempts are
// numbered from 0). Attempt 0 of a resolution given a firstHolder reads it from there - the
// listener's registry passes its watched copy of the role bucket
// (store.Registry.CachedRoleHolder), which costs no round trip, so a role message the lane
// forwards reads no bucket - and every later attempt from claims itself, since a retry after a
// superseded release must see the claim that superseded it, which the copy can trail. A caller
// that answers within its own deadline, a route among them, passes no firstHolder and reads the
// claim store on every attempt. The copy names a holder, not a whole claim, and the resolution
// reads nothing else from it.
func readClaim(ctx context.Context, claims ClaimStore, firstHolder func(role string) (string, error), role string, attempt int) (store.RoleClaim, error) {
	if attempt > 0 || firstHolder == nil {
		return claims.RoleClaim(ctx, role)
	}
	holder, err := firstHolder(role)
	if err != nil {
		return store.RoleClaim{}, err
	}
	return store.RoleClaim{HolderSessionID: holder}, nil
}

// ResolveHolder resolves role's holder within ctx. It reads the role's claim (Unclaimed when there
// is none), then the holder's session through sessions, which live judges (Live when it is there).
// A holder that is gone, or that live refuses, has its claim released
// (claims.ReleaseExpiredRoleClaim, a compare and set on the claim it read), and the resolution is
// Lapsed with what the release came to - unless the release found the claim superseded by a fresh
// one, which is read again, at most resolutionAttempts times in all, after which it is Churned.
// ExpiredRoleClaimSuperseded is interpreted here and nowhere else, so a claim raced by a live
// replacement resolves to that replacement for every caller. A read that fails returns a
// *ReadError naming its step, with the claim's holder in the Resolution when it was read. Every
// read waits no longer than ctx, and no attempt starts once ctx has ended: that is ctx's error,
// wrapped to name the role, and no *ReadError.
//
// It reads every claim from claims. A caller whose first read is a watched copy of the role
// bucket resolves through ResolveHolderFrom instead.
func ResolveHolder(ctx context.Context, claims ClaimStore, sessions SessionLookup, role string, live func(session.SessionEntry) bool) (Resolution, error) {
	return ResolveHolderFrom(ctx, claims, nil, sessions, role, live)
}

// ResolveHolderFrom is ResolveHolder reading each attempt's claim through readClaim, so a
// resolution given a firstHolder spends no claim-store round trip on its first attempt.
func ResolveHolderFrom(ctx context.Context, claims ClaimStore, firstHolder func(role string) (string, error), sessions SessionLookup, role string, live func(session.SessionEntry) bool) (Resolution, error) {
	for attempt := range resolutionAttempts {
		if err := ctx.Err(); err != nil {
			return Resolution{}, fmt.Errorf("resolve role %s's holder: %w", role, err)
		}
		claim, err := readClaim(ctx, claims, firstHolder, role, attempt)
		if err != nil {
			return Resolution{}, &ReadError{Step: ClaimRead, Err: err}
		}
		holder := claim.HolderSessionID
		if holder == "" {
			return Resolution{State: Unclaimed}, nil
		}
		entry, err := sessions.Holder(ctx, holder, live)
		if err == nil {
			return Resolution{State: Live, Holder: holder, Entry: entry}, nil
		}
		if !errors.Is(err, nats.ErrKeyNotFound) {
			return Resolution{Holder: holder}, &ReadError{Step: SessionRead, Holder: holder, Err: err}
		}
		release, err := claims.ReleaseExpiredRoleClaim(ctx, role, holder, sessions.TTL())
		if err != nil {
			return Resolution{Holder: holder}, &ReadError{Step: Release, Holder: holder, Err: err}
		}
		if release == store.ExpiredRoleClaimSuperseded {
			continue
		}
		return Resolution{State: Lapsed, Holder: holder, Release: release}, nil
	}
	return Resolution{State: Churned}, nil
}
