package identity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	// MembershipMaxAge is how long a confirmation of a person's membership stands: the first
	// request after it lapses refreshes the person's sign-in with the issuer, so someone the rule
	// no longer admits loses Dispatch within the hour.
	MembershipMaxAge = time.Hour
	// refreshTimeout bounds a refresh, which runs on a context of its own.
	refreshTimeout = 15 * time.Second
)

// Refresher renews a sign-in with its refresh token; *oidc.CodeFlow is the one Dispatch uses.
type Refresher interface {
	Refresh(ctx context.Context, refreshToken string) (oidc.Session, error)
}

// Membership confirms that a person signed in through the sign-in issuer may still use Dispatch:
// a confirmation younger than MembershipMaxAge stands, and an older one is renewed by refreshing
// the person's sign-in. A refresh the issuer refuses, or one whose ID token no longer names this
// person under Rule, ends every session the person holds: it advances their session generation
// and forgets their refresh token, so they sign in again or not at all. Under a domain the
// refreshed token must also name the Google account the address's row records (Subject): an
// address Google gave to another account, or a row no domain sign-in recorded, ends the same way.
type Membership struct {
	People   auth.PeopleStore
	Sessions auth.SessionStore
	SignIn   Refresher
	Rule     SignInRule
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
	// hanging up must neither cancel it nor have its cancellation read as the issuer's refusal.
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
	if !m.Rule.CanRefresh(membership.Subject) {
		return m.end(ctx, email, membership.Subject, "the address's sign-in recorded no Google account to confirm")
	}
	session, err := m.SignIn.Refresh(ctx, membership.RefreshToken)
	if err != nil {
		return m.end(ctx, email, membership.Subject, err.Error())
	}
	person, err := Person(session.Claims, m.Rule)
	if err != nil {
		return m.end(ctx, email, membership.Subject, err.Error())
	}
	if person != email {
		return m.end(ctx, email, membership.Subject, "the refreshed sign-in names "+person)
	}
	subject, err := m.Rule.ConfirmSubject(session.Claims, membership.Subject)
	if err != nil {
		return m.end(ctx, email, membership.Subject, err.Error())
	}
	return m.People.Confirm(ctx, email, subject, session.RefreshToken, m.now())
}

// end ends email's sign-in under m.Rule's end policy (SignInRule.End).
func (m *Membership) end(ctx context.Context, email, subject, reason string) error {
	return m.Rule.End(ctx, m.People, m.Sessions, email, subject, reason)
}

// CanRefresh says whether stored, the subject a membership's row records
// (auth.PersonMembership.Subject, empty when it records none), lets r's rule confirm it without
// asking the issuer first. Under a group there is always something to refresh. Under a domain,
// the refreshed token is checked against the account the row recorded; a row that records none (a
// sign-in under a group, from before the column, or any other way) has nothing to check it
// against, so the caller ends without asking Google.
func (r SignInRule) CanRefresh(stored string) bool {
	return r.Domain == "" || stored != ""
}

// ConfirmSubject is the subject Membership.Confirm should record for a refresh whose verified
// claims, and whose membership's stored subject, are given: empty under a group, which never
// records one. Under a domain the issuer must still name the account stored records; a refreshed
// token naming another account is an error whose text names both, for the caller to end the
// sign-in with.
func (r SignInRule) ConfirmSubject(claims oidc.Claims, stored string) (string, error) {
	if r.Domain == "" {
		return "", nil
	}
	if claims.Subject != stored {
		return "", fmt.Errorf("the refreshed sign-in is Google account %q, and the address's sign-in recorded %q", claims.Subject, stored)
	}
	return stored, nil
}

// End ends email's sign-in under r: under a domain, only while the address's row still records
// subject, the Google account this refresh read (auth.PeopleStore.EndSubject); when another
// account has signed in to the address since, the failure was the old account's, so it writes
// nothing and the old account's request is simply not signed in. Under a group it always ends the
// address's sessions and forgets its refresh token (auth.SessionStore.RevokeSessions,
// auth.PeopleStore.End). Either way it returns ErrNoIdentity once email's sign-in no longer
// stands.
func (r SignInRule) End(ctx context.Context, people auth.PeopleStore, sessions auth.SessionStore, email, subject, reason string) error {
	if r.Domain != "" {
		ended, err := people.EndSubject(ctx, email, subject)
		if err != nil {
			return err
		}
		if !ended {
			slog.Info("dispatch: a failed confirmation ended nothing: another Google account has signed in to the address since", "email", email, "reason", reason)
			return ErrNoIdentity
		}
		slog.Warn("dispatch: sign-in ended: Google did not confirm the person's membership of the domain", "email", email, "reason", reason)
		return ErrNoIdentity
	}
	slog.Warn("dispatch: sign-in ended: the sign-in pool did not confirm the person's membership", "email", email, "reason", reason)
	if err := sessions.RevokeSessions(ctx, email); err != nil {
		return err
	}
	if err := people.End(ctx, email); err != nil {
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
