package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
)

// DomainRule signs in the people of Domain, a Google Workspace domain.
type DomainRule struct {
	Domain string
}

var _ SignInRule = DomainRule{}

// Person names the person by the ID token's `email` claim, which Google writes, and checks the
// account is of the domain by `hd`, the account's Workspace domain, never by the email's own
// domain. A token with no subject is ErrNoPerson, one with no email of an address's shape
// ErrNoEmail, one of another domain or none ErrNotInDomain, and one whose email Google has not
// verified ErrEmailNotVerified.
func (r DomainRule) Person(claims oidc.Claims) (string, error) {
	if claims.Subject == "" {
		return "", fmt.Errorf("%w: the Google ID token carries no subject", ErrNoPerson)
	}
	if !emailShape.MatchString(claims.Email) {
		return "", ErrNoEmail
	}
	email := strings.ToLower(claims.Email)
	if claims.HostedDomain != r.Domain {
		return email, fmt.Errorf("%s (hd %q): %w", email, claims.HostedDomain, ErrNotInDomain)
	}
	if !claims.EmailVerified {
		return email, fmt.Errorf("%s: %w", email, ErrEmailNotVerified)
	}
	return email, nil
}

// RefusalLogFields names the email and hd Google's token carried.
func (r DomainRule) RefusalLogFields(claims oidc.Claims) []any {
	return []any{"email", claims.Email, "hd", claims.HostedDomain}
}

// Subject is the Google account (claims.Subject) a domain sign-in records with the person.
func (r DomainRule) Subject(claims oidc.Claims) string {
	return claims.Subject
}

// BootLog logs the issuer, client and Workspace domain boot is signing people in under.
func (r DomainRule) BootLog(issuer, clientID string) {
	slog.Info("dispatch: signing people in with Google for a Workspace domain", "issuer", issuer, "client_id", clientID, "domain", r.Domain)
}

// CanRefresh is true only while stored, the account the address's row recorded, is set: a row
// that records none (a sign-in under a group, from before the column, or any other way) has
// nothing to check a refresh against, so the caller ends without asking Google.
func (r DomainRule) CanRefresh(stored string) bool {
	return stored != ""
}

// ConfirmSubject requires the refreshed token to still name the account stored records; a
// refreshed token naming another account is an error whose text names both, for the caller to end
// the sign-in with.
func (r DomainRule) ConfirmSubject(claims oidc.Claims, stored string) (string, error) {
	if claims.Subject != stored {
		return "", fmt.Errorf("the refreshed sign-in is Google account %q, and the address's sign-in recorded %q", claims.Subject, stored)
	}
	return stored, nil
}

// End ends email's sign-in only while the address's row still records subject, the Google
// account this refresh read (auth.PeopleStore.EndSubject); when another account has signed in to
// the address since, the failure was the old account's, so it writes nothing and the old
// account's request is simply not signed in.
func (r DomainRule) End(ctx context.Context, people auth.PeopleStore, sessions auth.SessionStore, email, subject, reason string) error {
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

// ConfirmCookie reads the address's row (auth.PeopleStore.Membership) and refuses with
// ErrSignInSuperseded when it no longer names subject: a sign-in by another Google account
// committed in between and has already taken the address.
func (r DomainRule) ConfirmCookie(ctx context.Context, people auth.PeopleStore, email, subject string) error {
	membership, _, err := people.Membership(ctx, email)
	if err != nil {
		return err
	}
	if membership.Subject != subject {
		return ErrSignInSuperseded
	}
	return nil
}

// WriteSignIn records a sign-in for the Workspace domain (auth.PeopleStore.SignInSubject).
func (r DomainRule) WriteSignIn(ctx context.Context, people auth.PeopleStore, email, subject, refreshToken string, confirmedAt time.Time) error {
	return people.SignInSubject(ctx, email, subject, refreshToken, confirmedAt)
}

// WriteConfirm records a refresh that confirmed email's domain-mode membership
// (auth.PeopleStore.ConfirmSubject).
func (r DomainRule) WriteConfirm(ctx context.Context, people auth.PeopleStore, email, subject, refreshToken string, confirmedAt time.Time) error {
	return people.ConfirmSubject(ctx, email, subject, refreshToken, confirmedAt)
}
