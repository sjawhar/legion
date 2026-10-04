package identity

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	// MembershipMaxAge is how long a confirmation of a person's membership stands: the first
	// request after it lapses refreshes the person's sign-in with the pool, so someone the pool
	// drops from the group loses Dispatch within the hour.
	MembershipMaxAge = time.Hour
	// refreshTimeout bounds a refresh, which runs on a context of its own.
	refreshTimeout = 15 * time.Second
)

// Refresher renews a sign-in with its refresh token; *oidc.CodeFlow is the one Dispatch uses.
type Refresher interface {
	Refresh(ctx context.Context, refreshToken string) (oidc.Session, error)
}

// Membership confirms that a person signed in through the sign-in pool may still use Dispatch: a
// confirmation younger than MembershipMaxAge stands, and an older one is renewed by refreshing
// the person's sign-in. A refresh the pool refuses, or one whose ID token no longer names this
// person in Group, ends every session the person holds: it advances their session generation and
// forgets their refresh token, so they sign in again or not at all.
type Membership struct {
	People   auth.PeopleStore
	Sessions auth.SessionStore
	SignIn   Refresher
	Group    string
	// Now is the clock confirmations are dated by; nil is time.Now.
	Now func() time.Time

	// refreshes holds one refresh per person in flight: a page load's many requests share it.
	// It is per process; Dispatch serves one process at a time (the sign-in's pending states are
	// held in memory too).
	refreshes singleflight.Group
}

// Confirm returns nil when email may still use Dispatch, ErrNoIdentity when their sign-in has
// ended (now or earlier), and any other error when the membership could not be read or recorded.
func (m *Membership) Confirm(ctx context.Context, email string) error {
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return err
	}
	if found && m.fresh(membership) {
		return nil
	}
	// The refresh belongs to the person, not to the request that found it due: a browser
	// hanging up must neither cancel it nor have its cancellation read as the pool's refusal.
	_, err, _ = m.refreshes.Do(email, func() (any, error) {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
		defer cancel()
		return nil, m.refresh(refreshCtx, email)
	})
	return err
}

func (m *Membership) refresh(ctx context.Context, email string) error {
	// Read again under the flight: a refresh that finished just before this one began has
	// already confirmed the person.
	membership, found, err := m.People.Membership(ctx, email)
	if err != nil {
		return err
	}
	if !found || membership.RefreshToken == "" {
		return ErrNoIdentity
	}
	if m.fresh(membership) {
		return nil
	}
	session, err := m.SignIn.Refresh(ctx, membership.RefreshToken)
	if err != nil {
		return m.end(ctx, email, err.Error())
	}
	person, err := Person(session.Claims, m.Group)
	if err != nil {
		return m.end(ctx, email, err.Error())
	}
	if person != email {
		return m.end(ctx, email, "the refreshed sign-in names "+person)
	}
	return m.People.Confirm(ctx, email, session.RefreshToken, m.now())
}

// end revokes every session of email and forgets their refresh token.
func (m *Membership) end(ctx context.Context, email, reason string) error {
	slog.Warn("dispatch: sign-in ended: the sign-in pool did not confirm the person's membership", "email", email, "reason", reason)
	if err := m.Sessions.RevokeSessions(ctx, email); err != nil {
		return err
	}
	if err := m.People.End(ctx, email); err != nil {
		return err
	}
	return ErrNoIdentity
}

func (m *Membership) fresh(membership auth.PersonMembership) bool {
	return !membership.ConfirmedAt.IsZero() && m.now().Sub(membership.ConfirmedAt) < MembershipMaxAge
}

func (m *Membership) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}
