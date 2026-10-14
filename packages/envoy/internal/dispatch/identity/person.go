package identity

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/oidc"
)

var (
	// ErrNoPerson means a verified ID token's username was not issued by a federated provider for
	// an email address, so it names no person Dispatch can trust.
	ErrNoPerson = errors.New("the sign-in names no person by email")
	// ErrNotMember means the person is not in the group that may use Dispatch.
	ErrNotMember = errors.New("not a member of the group that may use Dispatch")
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

// Person names the person a verified sign-in pool ID token signs in, and checks they may use
// Dispatch under rule. The email comes from the username a federated provider issued,
// `<provider>_<email>` for a provider the token's identities name, and never from the email claim,
// which a person can write. It is lowercased. A person outside rule's group is ErrNotMember,
// returned with their email so a refusal can name them.
func Person(claims oidc.Claims, rule SignInRule) (string, error) {
	email, ok := providerEmail(claims)
	if !ok {
		return "", fmt.Errorf("%w: username %q", ErrNoPerson, claims.Username)
	}
	if !slices.Contains(claims.Groups, rule.Group) {
		return email, fmt.Errorf("%s: %w", email, ErrNotMember)
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
