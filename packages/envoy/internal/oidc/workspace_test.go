package oidc

import (
	"errors"
	"testing"
)

// workspaceAccount is a verified Google ID token's claims, as a domain-mode code flow reads them,
// for an account of the Workspace domain flowDomain.
func workspaceAccount() Claims {
	return Claims{Subject: "110169484474386276334", Email: "A.Person@example.com", EmailVerified: true, HostedDomain: flowDomain}
}

// The login is the lowercased email of an account whose hd is the domain and whose email Google
// has verified. The domain is read from hd alone: an account of the domain whose address is at
// another of the Workspace's domains is the domain's.
func TestWorkspaceLoginIsTheLowercasedVerifiedEmailOfTheDomain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		email string
		want  string
	}{
		{"an address at the domain", "A.Person@example.com", "a.person@example.com"},
		{"an address at another of the Workspace's domains", "A.Person@Alias.example", "a.person@alias.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := workspaceAccount()
			claims.Email = tc.email
			login, err := WorkspaceLogin(claims, flowDomain)
			if err != nil || login != tc.want {
				t.Fatalf("WorkspaceLogin = %q, %v; want %q", login, err, tc.want)
			}
		})
	}
}

// Each refusal wraps its own reason, and still names the login when the email has an address's
// shape, so the refusal can name the person.
func TestWorkspaceLoginRefusesEachWayWithItsReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Claims)
		domain string
		want   error
		login  string
	}{
		{"an hd of another domain", func(c *Claims) { c.HostedDomain = "other.example" }, flowDomain, ErrNotInDomain, "a.person@example.com"},
		{"an hd that only matches when lowercased", func(c *Claims) { c.HostedDomain = "Example.com" }, flowDomain, ErrNotInDomain, "a.person@example.com"},
		{"no hd, with an email at the domain: a personal account", func(c *Claims) { c.HostedDomain = "" }, flowDomain, ErrNotInDomain, "a.person@example.com"},
		{"no domain configured, for an account with no hd", func(c *Claims) { c.HostedDomain = "" }, "", ErrNotInDomain, "a.person@example.com"},
		{"an email Google has not verified", func(c *Claims) { c.EmailVerified = false }, flowDomain, ErrEmailNotVerified, "a.person@example.com"},
		{"no email", func(c *Claims) { c.Email = "" }, flowDomain, ErrNoEmail, ""},
		{"an email of no address's shape", func(c *Claims) { c.Email = "not-an-address" }, flowDomain, ErrNoEmail, ""},
		{"an email with two @", func(c *Claims) { c.Email = "a@b@example.com" }, flowDomain, ErrNoEmail, ""},
		{"no subject", func(c *Claims) { c.Subject = "" }, flowDomain, ErrNoSubject, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := workspaceAccount()
			tc.change(&claims)
			login, err := WorkspaceLogin(claims, tc.domain)
			if !errors.Is(err, tc.want) || login != tc.login {
				t.Errorf("WorkspaceLogin = %q, %v; want %q, %v", login, err, tc.login, tc.want)
			}
		})
	}
}
