package identity

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
)

var (
	// ErrNoPerson means a verified ID token's username was not issued by a federated provider for
	// an email address, or a Google ID token carries no subject, so it names no person Dispatch
	// can trust.
	ErrNoPerson = errors.New("the sign-in names no person by email")
	// ErrNotMember means the person is not in the group that may use Dispatch.
	ErrNotMember = errors.New("not a member of the group that may use Dispatch")
	// ErrNotInDomain means a Google account is not in the Workspace domain that may use Dispatch.
	ErrNotInDomain = errors.New("not in the domain that may use Dispatch")
	// ErrEmailNotVerified means Google has not verified the account's email.
	ErrEmailNotVerified = errors.New("email not verified by Google")
	// ErrNoEmail means a Google ID token carries no email address to name the person by.
	ErrNoEmail = errors.New("no email in the sign-in")
	// ErrSignInSuperseded means another account's sign-in has already taken the address since
	// this one recorded its subject, so the cookie this sign-in would issue must not be given out.
	ErrSignInSuperseded = errors.New("another account signed in to this address meanwhile")
)

// emailShape is an address with one @ and a dotted domain, the shape the deployment repository's
// identity_email accepts from a federated username.
var emailShape = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// SignInRule is who may sign in to Dispatch and how Dispatch treats their sign-in: GroupRule for
// the members of a Cognito sign-in pool's group, or DomainRule for the people of a Google
// Workspace domain. cmd/dispatch's signInRule chooses one from boot's settings, once, and every
// other package consults the value it returns rather than branching on which settings are set.
type SignInRule interface {
	// Person names the person a verified ID token signs in under this rule, and checks they may
	// use Dispatch; it reads only the claims of the rule's own issuer. The email is lowercased,
	// and a refusal that has one returns it so the refusal can name them.
	Person(claims oidc.Claims) (string, error)
	// RefusalLogFields names the slog fields a sign-in refusal's log line carries. Callers append
	// "reason", err to what this returns.
	RefusalLogFields(claims oidc.Claims) []any
	// Subject is the account a sign-in under this rule records with the person
	// (auth.PeopleStore.SignInSubject's subject); empty for a rule that records none.
	Subject(claims oidc.Claims) string
	// BootLog logs that boot is signing people in under this rule through issuer as clientID.
	BootLog(issuer, clientID string)
	// CanRefresh says whether stored, the subject a membership's row records
	// (auth.PersonMembership.Subject, empty when it records none), lets this rule confirm it
	// without asking the issuer first.
	CanRefresh(stored string) bool
	// ConfirmSubject is the subject Membership.Confirm should record for a refresh whose verified
	// claims, and whose membership's stored subject, are given.
	ConfirmSubject(claims oidc.Claims, stored string) (string, error)
	// End ends email's sign-in under this rule, returning ErrNoIdentity once it no longer stands.
	End(ctx context.Context, people auth.PeopleStore, sessions auth.SessionStore, email, subject, reason string) error
	// WriteSignIn records a sign-in under this rule (auth.PeopleStore.SignIn or .SignInSubject),
	// given the subject this rule's own Subject method already resolved.
	WriteSignIn(ctx context.Context, people auth.PeopleStore, email, subject, refreshToken string, confirmedAt time.Time) error
	// WriteConfirm records a refresh that confirmed email's membership under this rule
	// (auth.PeopleStore.Confirm or .ConfirmSubject), given the subject this rule's own
	// ConfirmSubject method already resolved.
	WriteConfirm(ctx context.Context, people auth.PeopleStore, email, subject, refreshToken string, confirmedAt time.Time) error
	// ConfirmCookie confirms, after a sign-in has read the session generation it will put in a
	// cookie, that email's row still names subject as the account that just signed in
	// (auth.PeopleStore.Membership): a sign-in by another account that committed since has
	// already taken the address, and issuing a cookie here would hand it to the wrong account.
	// ErrSignInSuperseded means it has. A rule that records no subject has nothing to confirm.
	ConfirmCookie(ctx context.Context, people auth.PeopleStore, email, subject string) error
}
