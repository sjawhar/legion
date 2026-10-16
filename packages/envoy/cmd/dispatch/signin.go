package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

// plainRefreshTokenRetireTimeout bounds the boot's retirement of the refresh tokens people holds
// in plain text, so a sign-in pool that does not answer delays the start by this much at most.
const plainRefreshTokenRetireTimeout = 30 * time.Second

// googleIssuer is Google's OpenID Connect issuer, the one issuer whose ID tokens carry `hd`, the
// Workspace domain of the account. go-oidc also accepts a token whose `iss` is Google's schemeless
// `accounts.google.com` when this is the configured issuer, as Google documents.
const googleIssuer = "https://accounts.google.com"

// domainShape is a lowercase DNS name of two or more labels, each 1 to 63 letters, digits and
// inner hyphens: the shape of a Google Workspace domain.
var domainShape = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// maxDomainLength is the longest DNS name, in its dotted text form.
const maxDomainLength = 253

// signInClientSettings are the three settings every Google sign-in takes, each with boot's value:
// the issuer and Dispatch's confidential client there.
func signInClientSettings(boot bootConfig) []struct{ name, value string } {
	return []struct{ name, value string }{
		{"DISPATCH_SIGNIN_ISSUER", boot.SignInIssuer},
		{"DISPATCH_SIGNIN_CLIENT_ID", boot.SignInClientID},
		{"DISPATCH_SIGNIN_CLIENT_SECRET", boot.SignInClientSecret},
	}
}

// signInSettings are all five sign-in settings, each with boot's value: the client's three, then
// the group a Cognito sign-in pool admits and the Workspace domain Google admits, of which a
// sign-in takes exactly one.
func signInSettings(boot bootConfig) []struct{ name, value string } {
	return append(signInClientSettings(boot),
		struct{ name, value string }{"DISPATCH_SIGNIN_GROUP", boot.SignInGroup},
		struct{ name, value string }{"DISPATCH_SIGNIN_DOMAIN", boot.SignInDomain},
	)
}

// signInRuleChoice is how a refusal names the group-or-domain pair as one item.
const signInRuleChoice = "DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN"

// signInSettingNames is what a Google sign-in takes, as a refusal lists it: "A, B, C and one of D
// or E".
func signInSettingNames() string {
	settings := signInClientSettings(bootConfig{})
	names := make([]string, len(settings))
	for i, setting := range settings {
		names[i] = setting.name
	}
	return strings.Join(names, ", ") + " and one of " + signInRuleChoice
}

// checkSignInSettings refuses the sign-in settings identityMode (DISPATCH_IDENTITY) cannot take.
// Any of them beside header identity, which is only for tests and local harnesses, is refused
// first. Then the first of these refuses: a group and a domain both set; some of the settings but
// not the issuer, client ID, client secret and one of the group or domain; a domain under an
// issuer other than Google's, whose tokens carry no `hd`; a domain that is not a lowercase DNS
// name; a group under Google's issuer, whose tokens carry no `cognito:groups`. A deployment with
// the issuer, client ID, client secret and group of a Cognito pool reaches none of them.
func checkSignInSettings(boot bootConfig, identityMode string) error {
	var set []string
	for _, setting := range signInSettings(boot) {
		if setting.value != "" {
			set = append(set, setting.name)
		}
	}
	if strings.HasPrefix(identityMode, "header:") && len(set) > 0 {
		return fmt.Errorf("%s must be unset because header identity and Google sign-in cannot share a deployment: header identity is only for tests and local harnesses", strings.Join(set, ", "))
	}
	if boot.SignInGroup != "" && boot.SignInDomain != "" {
		return errors.New("DISPATCH_SIGNIN_GROUP and DISPATCH_SIGNIN_DOMAIN are both set; set one: a Cognito pool's group, or a Google Workspace domain")
	}
	if len(set) > 0 {
		var missing []string
		for _, setting := range signInClientSettings(boot) {
			if setting.value == "" {
				missing = append(missing, setting.name)
			}
		}
		if boot.SignInGroup == "" && boot.SignInDomain == "" {
			if len(missing) == 0 {
				return fmt.Errorf("Google sign-in needs %s: %s set, DISPATCH_SIGNIN_GROUP and DISPATCH_SIGNIN_DOMAIN missing", signInSettingNames(), strings.Join(set, ", "))
			}
			missing = append(missing, signInRuleChoice)
		}
		if len(missing) > 0 {
			return fmt.Errorf("Google sign-in needs %s: %s set, %s missing", signInSettingNames(), strings.Join(set, ", "), strings.Join(missing, ", "))
		}
	}
	if boot.SignInDomain != "" {
		if boot.SignInIssuer != googleIssuer {
			return fmt.Errorf("DISPATCH_SIGNIN_DOMAIN needs DISPATCH_SIGNIN_ISSUER %s, since only Google's tokens carry hd; DISPATCH_SIGNIN_ISSUER is %s", googleIssuer, boot.SignInIssuer)
		}
		if len(boot.SignInDomain) > maxDomainLength || !domainShape.MatchString(boot.SignInDomain) {
			return fmt.Errorf("DISPATCH_SIGNIN_DOMAIN must be a lowercase domain name such as example.com; got a value of %d characters", utf8.RuneCountInString(boot.SignInDomain))
		}
	}
	if boot.SignInGroup != "" && boot.SignInIssuer == googleIssuer {
		return fmt.Errorf("DISPATCH_SIGNIN_GROUP is a Cognito pool's group, and a token from %s carries none; set DISPATCH_SIGNIN_DOMAIN instead", googleIssuer)
	}
	return nil
}

// requireCookieSignIn refuses cookie identity with no way to sign in: a cookie server signs people
// in through its sign-in issuer, or, under dev sign-in, at /auth/_dev/signin.
func requireCookieSignIn(boot bootConfig, devSignIn bool) error {
	if boot.SignInIssuer != "" || devSignIn {
		return nil
	}
	return fmt.Errorf("cookie identity signs people in: %s are required", signInSettingNames())
}

// signInRule is who boot's sign-in admits: the members of a Cognito pool's group, or the people of
// a Google Workspace domain; nil when boot configures neither (header identity, or dev sign-in),
// matching discoverSignIn.
func signInRule(boot bootConfig) identity.SignInRule {
	switch {
	case boot.SignInDomain != "":
		return identity.DomainRule{Domain: boot.SignInDomain}
	case boot.SignInGroup != "":
		return identity.GroupRule{Group: boot.SignInGroup}
	default:
		return nil
	}
}

// discoverSignIn is the sign-in issuer's code flow for boot's DISPATCH_SIGNIN_* settings, its
// endpoints read from the issuer's discovery document, asking Google for boot's Workspace domain
// in domain mode; nil when boot configures no sign-in (header identity, or dev sign-in).
func discoverSignIn(ctx context.Context, boot bootConfig) (*oidc.CodeFlow, error) {
	if boot.SignInIssuer == "" {
		return nil, nil
	}
	return oidc.DiscoverCodeFlow(ctx, boot.SignInIssuer, boot.SignInClientID, boot.SignInClientSecret, boot.SignInDomain, oidc.DiscoveryTimeout)
}

// openPeople is the people store main serves boot with over pool, and the key session cookies are
// signed with (sessionSigningKey): the store seals each refresh token under that same key, and
// revokes a token stored in plain text at signIn's pool, nil when boot configures none.
func openPeople(boot bootConfig, dataDir string, pool *store.Pool, signIn *oidc.CodeFlow) (*store.PgPeopleStore, string, error) {
	signingKey, err := sessionSigningKey(boot, dataDir)
	if err != nil {
		return nil, "", err
	}
	var revoker store.RefreshTokenRevoker
	if signIn != nil {
		revoker = signIn
	}
	return store.NewPgPeopleStore(pool, signingKey, revoker), signingKey, nil
}

// retirePlainRefreshTokens is the boot's retirement of the refresh tokens people holds in plain
// text (store.PgPeopleStore.RetirePlainRefreshTokens), bounded by timeout (bootTask): a pool that
// does not answer fails each token not yet revoked, and the start goes on. It logs the counts,
// never a token; each token the pool did not revoke has its own ERROR line, and stays for the next
// boot.
func retirePlainRefreshTokens(ctx context.Context, people *store.PgPeopleStore, timeout time.Duration) {
	bootTask(ctx, timeout, "dispatch: retire the refresh tokens stored in plain text", "dispatch: retired the refresh tokens stored in plain text",
		func(ctx context.Context) ([]any, error) {
			retired, failed, err := people.RetirePlainRefreshTokens(ctx)
			return []any{"retired", retired, "failed", failed}, err
		})
}

// requestIdentityFor is how a browser request names its person under boot: a header-identity
// server takes the header's email; a cookie server verifies its signed session cookie and, with
// sign-in configured, confirms at least hourly that the person is still in DISPATCH_SIGNIN_GROUP
// or DISPATCH_SIGNIN_DOMAIN (signInRule).
func requestIdentityFor(boot bootConfig, signingKey string, people auth.PeopleStore, sessions auth.SessionStore, signIn *oidc.CodeFlow) identity.Identity {
	if boot.IdentityHeader != "" {
		return identity.HeaderIdentity{Header: boot.IdentityHeader, People: people}
	}
	cookieIdentity := identity.CookieIdentity{SigningKey: signingKey, Sessions: sessions}
	if signIn != nil {
		cookieIdentity.Membership = &identity.Membership{People: people, Sessions: sessions, SignIn: signIn, Rule: signInRule(boot)}
	}
	return cookieIdentity
}
