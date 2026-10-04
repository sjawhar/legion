package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/oidc"
)

// signInSettings are the four settings Google sign-in through the sign-in pool takes, all or none,
// each with boot's value.
func signInSettings(boot bootConfig) []struct{ name, value string } {
	return []struct{ name, value string }{
		{"DISPATCH_SIGNIN_ISSUER", boot.SignInIssuer},
		{"DISPATCH_SIGNIN_CLIENT_ID", boot.SignInClientID},
		{"DISPATCH_SIGNIN_CLIENT_SECRET", boot.SignInClientSecret},
		{"DISPATCH_SIGNIN_GROUP", boot.SignInGroup},
	}
}

// signInSettingNames is the four settings' names as a refusal lists them: "A, B, C and D".
func signInSettingNames() string {
	settings := signInSettings(bootConfig{})
	names := make([]string, len(settings))
	for i, setting := range settings {
		names[i] = setting.name
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// checkSignInSettings refuses the sign-in settings identityMode (DISPATCH_IDENTITY) cannot take:
// any of them beside header identity, which is only for tests and local harnesses, and some but
// not all four.
func checkSignInSettings(boot bootConfig, identityMode string) error {
	var missing, set []string
	for _, setting := range signInSettings(boot) {
		if setting.value == "" {
			missing = append(missing, setting.name)
		} else {
			set = append(set, setting.name)
		}
	}
	if strings.HasPrefix(identityMode, "header:") && len(set) > 0 {
		return fmt.Errorf("%s must be unset because header identity and Google sign-in cannot share a deployment: header identity is only for tests and local harnesses", strings.Join(set, ", "))
	}
	if len(set) > 0 && len(missing) > 0 {
		return fmt.Errorf("Google sign-in needs all four of %s: %s set, %s missing", signInSettingNames(), strings.Join(set, ", "), strings.Join(missing, ", "))
	}
	return nil
}

// requireCookieSignIn refuses cookie identity with no way to sign in: a cookie server signs people
// in through the sign-in pool, or, under dev sign-in, at /auth/_dev/signin.
func requireCookieSignIn(boot bootConfig, devSignIn bool) error {
	if boot.SignInIssuer != "" || devSignIn {
		return nil
	}
	return fmt.Errorf("cookie identity signs people in with Google Workspace: %s are required", signInSettingNames())
}

// discoverSignIn is the sign-in pool's code flow for boot's DISPATCH_SIGNIN_* settings, its
// endpoints read from the issuer's discovery document; nil when boot configures no sign-in
// (header identity, or dev sign-in).
func discoverSignIn(ctx context.Context, boot bootConfig) (*oidc.CodeFlow, error) {
	if boot.SignInIssuer == "" {
		return nil, nil
	}
	return oidc.DiscoverCodeFlow(ctx, boot.SignInIssuer, boot.SignInClientID, boot.SignInClientSecret, oidc.DiscoveryTimeout)
}

// requestIdentityFor is how a browser request names its person under boot: a header-identity
// server takes the header's email; a cookie server verifies its signed session cookie and, with
// sign-in configured, confirms at least hourly that the person is still in DISPATCH_SIGNIN_GROUP.
func requestIdentityFor(boot bootConfig, signingKey string, people auth.PeopleStore, sessions auth.SessionStore, signIn *oidc.CodeFlow) identity.Identity {
	if boot.IdentityHeader != "" {
		return identity.HeaderIdentity{Header: boot.IdentityHeader, People: people}
	}
	cookieIdentity := identity.CookieIdentity{SigningKey: signingKey, Sessions: sessions}
	if signIn != nil {
		cookieIdentity.Membership = &identity.Membership{People: people, Sessions: sessions, SignIn: signIn, Group: boot.SignInGroup}
	}
	return cookieIdentity
}
