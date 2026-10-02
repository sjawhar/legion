package identity

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc"
)

func TestPersonIsTheLowercasedEmailTheProviderUsernameCarries(t *testing.T) {
	email, err := Person(oidc.Claims{
		Username:  "GoogleWorkspace_A.B+C@D.example",
		Groups:    []string{"hawk-users", "platform-managers"},
		Providers: []string{"GoogleWorkspace"},
	}, "platform-managers")
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	if email != "a.b+c@d.example" {
		t.Fatalf("Person = %q, want a.b+c@d.example", email)
	}
}

func TestPersonOutsideTheGroupIsRefusedByName(t *testing.T) {
	email, err := Person(oidc.Claims{
		Username:  "GoogleWorkspace_contractor@d.example",
		Groups:    []string{"hawk-users"},
		Providers: []string{"GoogleWorkspace"},
	}, "platform-managers")
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("Person outside the group: err = %v, want ErrNotMember", err)
	}
	if email != "contractor@d.example" {
		t.Fatalf("refused person = %q, want the email, so the refusal can name them", email)
	}
	if _, err := Person(oidc.Claims{Username: "GoogleWorkspace_x@d.example", Providers: []string{"GoogleWorkspace"}}, "platform-managers"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("Person in no group: err = %v, want ErrNotMember", err)
	}
}

// Only a username a federated provider issued carries an email Dispatch trusts: a pool's own
// account, or a username whose remainder is not an address, names nobody.
func TestPersonRefusesAUsernameNoProviderIssued(t *testing.T) {
	for _, claims := range []oidc.Claims{
		{Username: "sami@d.example", Groups: []string{"platform-managers"}},
		{Username: "GoogleWorkspace_sami@d.example", Groups: []string{"platform-managers"}},
		{Username: "OtherProvider_sami@d.example", Groups: []string{"platform-managers"}, Providers: []string{"GoogleWorkspace"}},
		{Username: "GoogleWorkspace_not-an-address", Groups: []string{"platform-managers"}, Providers: []string{"GoogleWorkspace"}},
		{Username: "GoogleWorkspace_two@at@d.example", Groups: []string{"platform-managers"}, Providers: []string{"GoogleWorkspace"}},
		{Username: "_sami@d.example", Groups: []string{"platform-managers"}, Providers: []string{""}},
	} {
		if email, err := Person(claims, "platform-managers"); !errors.Is(err, ErrNoPerson) {
			t.Errorf("Person(%+v) = %q, %v; want ErrNoPerson", claims, email, err)
		}
	}
}
