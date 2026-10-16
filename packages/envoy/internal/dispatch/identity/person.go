package identity

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

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
)

// emailShape is an address with one @ and a dotted domain, the shape the deployment repository's
// identity_email accepts from a federated username.
var emailShape = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// SignInRule is who may sign in to Dispatch: the members of Group, a Cognito sign-in pool's group
// (`cognito:groups`), or the people of Domain, a Google Workspace domain. Exactly one is set.
type SignInRule struct {
	Group  string
	Domain string
}

// RefusalLogFields names the slog fields a sign-in refusal's log line carries, under rule's
// issuer: under a domain, the email and hd Google's token carried; under a group, the username
// the pool issued. Callers append "reason", err to what this returns.
func (r SignInRule) RefusalLogFields(claims oidc.Claims) []any {
	if r.Domain != "" {
		return []any{"email", claims.Email, "hd", claims.HostedDomain}
	}
	return []any{"username", claims.Username}
}

// Subject is the Google account a sign-in under rule records with the person
// (auth.PeopleStore.SignIn's subject): claims.Subject under a domain, empty under a group, which
// never records one.
func (r SignInRule) Subject(claims oidc.Claims) string {
	if r.Domain == "" {
		return ""
	}
	return claims.Subject
}

// BootLog logs that boot is signing people in under r through issuer as clientID, naming the
// group or the domain r admits.
func (r SignInRule) BootLog(issuer, clientID string) {
	if r.Domain != "" {
		slog.Info("dispatch: signing people in with Google for a Workspace domain", "issuer", issuer, "client_id", clientID, "domain", r.Domain)
		return
	}
	slog.Info("dispatch: signing people in through the sign-in pool", "issuer", issuer, "client_id", clientID, "group", r.Group)
}

// Person names the person a verified ID token signs in, and checks they may use Dispatch under
// rule; it reads only the claims of rule's issuer.
//
// Under a group, the token is a Cognito sign-in pool's: the email comes from the username a
// federated provider issued, `<provider>_<email>` for a provider the token's identities name, and
// never from the email claim, which a person can write. A person outside the group is
// ErrNotMember.
//
// Under a domain, the token is Google's: the email is its `email` claim, which Google writes, and
// the person must be of the domain by `hd`, the account's Workspace domain, never by the email's
// own domain. A token with no subject is ErrNoPerson, one with no email of an address's shape
// ErrNoEmail, one of another domain or none ErrNotInDomain, and one whose email Google has not
// verified ErrEmailNotVerified.
//
// The email is lowercased, and a refusal that has one returns it so the refusal can name them.
func Person(claims oidc.Claims, rule SignInRule) (string, error) {
	if rule.Domain != "" {
		return workspacePerson(claims, rule.Domain)
	}
	email, ok := providerEmail(claims)
	if !ok {
		return "", fmt.Errorf("%w: username %q", ErrNoPerson, claims.Username)
	}
	if !slices.Contains(claims.Groups, rule.Group) {
		return email, fmt.Errorf("%s: %w", email, ErrNotMember)
	}
	return email, nil
}

func workspacePerson(claims oidc.Claims, domain string) (string, error) {
	if claims.Subject == "" {
		return "", fmt.Errorf("%w: the Google ID token carries no subject", ErrNoPerson)
	}
	if !emailShape.MatchString(claims.Email) {
		return "", ErrNoEmail
	}
	email := strings.ToLower(claims.Email)
	if claims.HostedDomain != domain {
		return email, fmt.Errorf("%s (hd %q): %w", email, claims.HostedDomain, ErrNotInDomain)
	}
	if !claims.EmailVerified {
		return email, fmt.Errorf("%s: %w", email, ErrEmailNotVerified)
	}
	return email, nil
}

func providerEmail(claims oidc.Claims) (string, bool) {
	for _, provider := range claims.Providers {
		if provider == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(claims.Username, provider+"_"); ok && emailShape.MatchString(rest) {
			return strings.ToLower(rest), true
		}
	}
	return "", false
}
