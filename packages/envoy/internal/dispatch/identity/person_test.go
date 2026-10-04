package identity

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc"
)

func TestPersonIsTheLowercasedEmailTheProviderUsernameCarries(t *testing.T) {
	email, err := Person(oidc.Claims{
		Username:  "ExampleIdP_A.B+C@D.example",
		Groups:    []string{"other-group", "dispatch-members"},
		Providers: []string{"ExampleIdP"},
	}, "dispatch-members")
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	if email != "a.b+c@d.example" {
		t.Fatalf("Person = %q, want a.b+c@d.example", email)
	}
}

func TestPersonOutsideTheGroupIsRefusedByName(t *testing.T) {
	email, err := Person(oidc.Claims{
		Username:  "ExampleIdP_contractor@d.example",
		Groups:    []string{"other-group"},
		Providers: []string{"ExampleIdP"},
	}, "dispatch-members")
	if !errors.Is(err, ErrNotMember) {
		t.Fatalf("Person outside the group: err = %v, want ErrNotMember", err)
	}
	if email != "contractor@d.example" {
		t.Fatalf("refused person = %q, want the email, so the refusal can name them", email)
	}
	if _, err := Person(oidc.Claims{Username: "ExampleIdP_x@d.example", Providers: []string{"ExampleIdP"}}, "dispatch-members"); !errors.Is(err, ErrNotMember) {
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
		if email, err := Person(claims, "dispatch-members"); !errors.Is(err, ErrNoPerson) {
			t.Errorf("Person(%+v) = %q, %v; want ErrNoPerson", claims, email, err)
		}
	}
}
