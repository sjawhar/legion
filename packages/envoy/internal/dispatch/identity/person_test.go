package identity

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc"
)

// dispatchMembers is the group rule the Cognito tests sign in under.
var dispatchMembers = GroupRule{Group: "dispatch-members"}

func TestPersonIsTheLowercasedEmailTheProviderUsernameCarries(t *testing.T) {
	email, err := dispatchMembers.Person(oidc.Claims{
		Username:  "ExampleIdP_A.B+C@D.example",
		Groups:    []string{"other-group", "dispatch-members"},
		Providers: []string{"ExampleIdP"},
	})
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	if email != "a.b+c@d.example" {
		t.Fatalf("Person = %q, want a.b+c@d.example", email)
	}
}

func TestPersonOutsideTheGroupIsRefusedByName(t *testing.T) {
	email, err := dispatchMembers.Person(oidc.Claims{
		Username:  "ExampleIdP_contractor@d.example",
		Groups:    []string{"other-group"},
		Providers: []string{"ExampleIdP"},
	})
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("Person outside the group: err = %v, want ErrNotMember", err)
	}
	if email != "contractor@d.example" {
		t.Fatalf("refused person = %q, want the email, so the refusal can name them", email)
	}
	if _, err := dispatchMembers.Person(oidc.Claims{Username: "ExampleIdP_x@d.example", Providers: []string{"ExampleIdP"}}); !errors.Is(err, ErrNotMember) {
		t.Fatalf("Person in no group: err = %v, want ErrNotMember", err)
	}
}

// Only a username a federated provider issued carries an email Dispatch trusts: a pool's own
// account, or a username whose remainder is not an address, names nobody.
func TestPersonRefusesAUsernameNoProviderIssued(t *testing.T) {
	for _, claims := range []oidc.Claims{
		{Username: "sami@d.example", Groups: []string{"dispatch-members"}},
		{Username: "ExampleIdP_sami@d.example", Groups: []string{"dispatch-members"}},
		{Username: "OtherProvider_sami@d.example", Groups: []string{"dispatch-members"}, Providers: []string{"ExampleIdP"}},
		{Username: "ExampleIdP_not-an-address", Groups: []string{"dispatch-members"}, Providers: []string{"ExampleIdP"}},
		{Username: "ExampleIdP_two@at@d.example", Groups: []string{"dispatch-members"}, Providers: []string{"ExampleIdP"}},
		{Username: "_sami@d.example", Groups: []string{"dispatch-members"}, Providers: []string{""}},
	} {
		if email, err := dispatchMembers.Person(claims); !errors.Is(err, ErrNoPerson) {
			t.Errorf("Person(%+v) = %q, %v; want ErrNoPerson", claims, email, err)
		}
	}
}

// workspace is the domain rule the Google tests sign in under.
var workspace = DomainRule{Domain: "example.com"}

// googleAccount is a verified Google ID token's claims, as a domain-mode code flow reads them, for
// an account of the Workspace domain example.com.
func googleAccount() oidc.Claims {
	return oidc.Claims{Subject: "110169484474386276334", Email: "A.Person@example.com", EmailVerified: true, HostedDomain: "example.com"}
}

// Under the domain rule a person is the lowercased email of a token whose hd is the domain and
// whose email Google has verified.
func TestPersonUnderTheDomainIsTheVerifiedEmailOfTheDomain(t *testing.T) {
	email, err := workspace.Person(googleAccount())
	if err != nil || email != "a.person@example.com" {
		t.Fatalf("Person = %q, %v; want a.person@example.com", email, err)
	}
}

// Each refusal has its own reason, and names the email when the token carries one, so the refusal
// page and the log can name the person.
func TestPersonUnderTheDomainRefusesEachWayWithItsReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*oidc.Claims)
		want   error
		email  string
	}{
		{"another domain", func(c *oidc.Claims) { c.HostedDomain = "other.example" }, ErrNotInDomain, "a.person@example.com"},
		{"a domain that only matches when lowercased", func(c *oidc.Claims) { c.HostedDomain = "Example.com" }, ErrNotInDomain, "a.person@example.com"},
		{"no hd: an account outside any Workspace", func(c *oidc.Claims) { c.HostedDomain = "" }, ErrNotInDomain, "a.person@example.com"},
		{"an email Google has not verified", func(c *oidc.Claims) { c.EmailVerified = false }, ErrEmailNotVerified, "a.person@example.com"},
		{"no email", func(c *oidc.Claims) { c.Email = "" }, ErrNoEmail, ""},
		{"an email of no address's shape", func(c *oidc.Claims) { c.Email = "not-an-address" }, ErrNoEmail, ""},
		{"no subject", func(c *oidc.Claims) { c.Subject = "" }, ErrNoPerson, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := googleAccount()
			tc.change(&claims)
			email, err := workspace.Person(claims)
			if !errors.Is(err, tc.want) || email != tc.email {
				t.Errorf("Person = %q, %v; want %q, %v", email, err, tc.email, tc.want)
			}
		})
	}
	for want, reason := range map[error]string{ErrNotInDomain: "not in the domain", ErrEmailNotVerified: "email not verified", ErrNoEmail: "no email"} {
		if !strings.Contains(want.Error(), reason) {
			t.Errorf("%v does not say %q", want, reason)
		}
	}
}

// The domain rule reads none of a Cognito token's claims: a username and groups a pool issued sign
// nobody in, and the group rule reads none of the Workspace claims.
func TestEachRuleReadsOnlyItsOwnClaims(t *testing.T) {
	cognito := oidc.Claims{Username: "ExampleIdP_a@example.com", Groups: []string{"dispatch-members"}, Providers: []string{"ExampleIdP"}, Subject: "pool-subject"}
	if _, err := workspace.Person(cognito); !errors.Is(err, ErrNoEmail) {
		t.Errorf("a Cognito token under the domain rule: err = %v, want ErrNoEmail", err)
	}
	if _, err := dispatchMembers.Person(googleAccount()); !errors.Is(err, ErrNoPerson) {
		t.Errorf("a Google token under the group rule: err = %v, want ErrNoPerson", err)
	}
}
