package policy_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const kindOwner = "ada@example.com"

func kindSecret(name, kind, value string) secrets.LocalSecret {
	return policytest.KindSecret(name, kindOwner, policy.TierAgent, kind, value)
}

func TestKindSecretsAreServed(t *testing.T) {
	githubApp := kindSecret("GITHUB_APP_KEY", policy.KindGitHubApp, "github-app-value")
	googleLogin := kindSecret("GOOGLE_LOGIN_KEY", policy.KindGoogleLogin, "google-login-value")
	googleLogin.Tags[policy.TagMaxGrant] = "1h"
	store := secrets.NewLocal(githubApp, googleLogin)

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for name, kind := range map[string]string{
		"GITHUB_APP_KEY":   policy.KindGitHubApp,
		"GOOGLE_LOGIN_KEY": policy.KindGoogleLogin,
	} {
		secret, ok := set.Secrets[name]
		if !ok {
			t.Errorf("Load did not serve %s", name)
			continue
		}
		store := secrets.LocalARN(policytest.ID(name))
		if want := policy.KindSource(kind, store); secret.Source != want {
			t.Errorf("%s Source = %q, want %q", name, secret.Source, want)
		}
		if gotKind, gotStore, minted, err := policy.ParseKindSource(secret.Source); err != nil || !minted || gotKind != kind || gotStore != store {
			t.Errorf("ParseKindSource(%q) = %q, %q, %t, %v; want %q, %q, true", secret.Source, gotKind, gotStore, minted, err, kind, store)
		}
	}
	if got := set.Secrets["GOOGLE_LOGIN_KEY"].MaxGrant; got != time.Hour {
		t.Fatalf("GOOGLE_LOGIN_KEY MaxGrant = %v, want 1h", got)
	}
	if got := set.Secrets["GITHUB_APP_KEY"].MaxGrant; got != 0 {
		t.Fatalf("GITHUB_APP_KEY MaxGrant = %v, want 0 (unset)", got)
	}
}

func TestParseKindSourceRefusesAMalformedMintedSource(t *testing.T) {
	for _, source := range []string{"mint:", "mint:github-app", "mint:github-app:", "mint::arn:aws:x"} {
		if _, _, minted, err := policy.ParseKindSource(source); !minted || !errors.Is(err, policy.ErrKindSourceMalformed) {
			t.Errorf("ParseKindSource(%q) = minted %t, %v; want minted and ErrKindSourceMalformed", source, minted, err)
		}
	}
	if kind, store, minted, err := policy.ParseKindSource("arn:aws:secretsmanager:x"); minted || err != nil || kind != "" || store != "arn:aws:secretsmanager:x" {
		t.Errorf("ParseKindSource(stored) = %q, %q, %t, %v; want the source whole and not minted", kind, store, minted, err)
	}
}

func TestParseMaxGrant(t *testing.T) {
	for raw, want := range map[string]time.Duration{"1h": time.Hour, "1s": time.Second, "90m": 90 * time.Minute} {
		if got, err := policy.ParseMaxGrant(raw); err != nil || got != want {
			t.Errorf("ParseMaxGrant(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "soon", "0s", "500ms", "-1h"} {
		if _, err := policy.ParseMaxGrant(raw); !errors.Is(err, policy.ErrMaxGrantMalformed) {
			t.Errorf("ParseMaxGrant(%q) = %v; want ErrMaxGrantMalformed", raw, err)
		}
	}
}

func TestUnknownKindIsRefused(t *testing.T) {
	logged := policytest.CaptureLog(t)
	store := secrets.NewLocal(kindSecret("UNKNOWN_KIND_KEY", "other", "value"))

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Secrets["UNKNOWN_KIND_KEY"]; ok {
		t.Fatal("Load served a secret with an unknown kind")
	}
	if !strings.Contains(logged.String(), policy.ReasonKindUnknown) {
		t.Fatalf("log %q does not name %q", logged.String(), policy.ReasonKindUnknown)
	}
}

func TestKindSecretWithoutValueIsRefused(t *testing.T) {
	logged := policytest.CaptureLog(t)
	store := secrets.NewLocal(kindSecret("EMPTY_KIND_KEY", policy.KindGitHubApp, ""))

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Secrets["EMPTY_KIND_KEY"]; ok {
		t.Fatal("Load served a kind secret without a current value")
	}
	if !strings.Contains(logged.String(), policy.ReasonNoCurrentValue) {
		t.Fatalf("log %q does not name %q", logged.String(), policy.ReasonNoCurrentValue)
	}
}

func TestMalformedMaxGrantIsRefused(t *testing.T) {
	logged := policytest.CaptureLog(t)
	secret := kindSecret("BAD_MAX_GRANT", policy.KindGoogleLogin, "value")
	secret.Tags[policy.TagMaxGrant] = "soon"
	store := secrets.NewLocal(secret)

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Secrets["BAD_MAX_GRANT"]; ok {
		t.Fatal("Load served a secret with a malformed max-grant")
	}
	if !strings.Contains(logged.String(), policy.ReasonMaxGrantMalformed) {
		t.Fatalf("log %q does not name %q", logged.String(), policy.ReasonMaxGrantMalformed)
	}
}

func TestKindRetagChangesVersionAndSource(t *testing.T) {
	first := kindSecret("MUTABLE_KIND_KEY", policy.KindGitHubApp, "value")
	store := secrets.NewLocal(first)
	loader := policytest.Loader(store)

	before, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second := first
	second.Tags = map[string]string{}
	for key, value := range first.Tags {
		second.Tags[key] = value
	}
	second.Tags[policy.TagKind] = policy.KindGoogleLogin
	store.Put(second)
	after, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}

	if before.Version == after.Version {
		t.Fatal("retagging a kind left Version unchanged")
	}
	if before.Secrets["MUTABLE_KIND_KEY"].Source == after.Secrets["MUTABLE_KIND_KEY"].Source {
		t.Fatal("retagging a kind left Source unchanged")
	}
}

func TestPlainSecretDigestIsUnchanged(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("PLAIN_KEY", kindOwner, policy.TierAgent, "value"))
	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const want = "522fe0fdc2ad3192cb04f81de72d295cf8846ed212ae744918dd9a5e078fe7fa"
	if set.Version != want {
		t.Fatalf("plain Version = %s, want pre-kind digest %s", set.Version, want)
	}
}

func TestLegacyMintTagIsIgnoredAndEmptySecretIsRefused(t *testing.T) {
	logged := policytest.CaptureLog(t)
	secret := policytest.Secret("LEGACY_MINT_KEY", kindOwner, policy.TierAgent, "")
	secret.Tags["mint-service-account"] = "ignored"
	store := secrets.NewLocal(secret)

	set, err := policytest.Loader(store).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := set.Secrets["LEGACY_MINT_KEY"]; ok {
		t.Fatal("Load served an empty legacy minted secret")
	}
	if !strings.Contains(logged.String(), policy.ReasonNoCurrentValue) {
		t.Fatalf("log %q does not name %q", logged.String(), policy.ReasonNoCurrentValue)
	}
}

func TestEmptyKindIsAPlainSecret(t *testing.T) {
	secret := kindSecret("EMPTY_KIND", "", "value")
	set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := set.Secrets["EMPTY_KIND"]; got.Source != secrets.LocalARN(policytest.ID("EMPTY_KIND")) {
		t.Fatalf("EMPTY_KIND = %+v, want plain stored source", got)
	}
}

// TestAGitHubAppSecretWithoutOwnersIsRefused: a public App can be installed by any account, so a
// github-app secret names the accounts the broker mints for, and one that names none, by a missing
// tag or a tag of only white space, is refused github-owners-missing rather than served.
func TestAGitHubAppSecretWithoutOwnersIsRefused(t *testing.T) {
	for name, owners := range map[string]*string{"missing": nil, "empty": new("  \t ")} {
		t.Run(name, func(t *testing.T) {
			logged := policytest.CaptureLog(t)
			secret := kindSecret("APP_KEY", policy.KindGitHubApp, "value")
			delete(secret.Tags, policy.TagGitHubOwners)
			if owners != nil {
				secret.Tags[policy.TagGitHubOwners] = *owners
			}
			set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, ok := set.Secrets["APP_KEY"]; ok {
				t.Fatal("Load served a github-app secret naming no github-owners")
			}
			if !strings.Contains(logged.String(), policy.ReasonGitHubOwnersMissing) {
				t.Fatalf("log %q does not name %q", logged.String(), policy.ReasonGitHubOwnersMissing)
			}
		})
	}
}

// TestGitHubOwnersAreReadWithoutRegardToCase: one list in two spellings is one policy, and a
// google-login secret carries no owners whatever its tags say.
func TestGitHubOwnersAreReadWithoutRegardToCase(t *testing.T) {
	load := func(owners string) *policy.Set {
		t.Helper()
		secret := kindSecret("APP_KEY", policy.KindGitHubApp, "value")
		secret.Tags[policy.TagGitHubOwners] = owners
		login := kindSecret("LOGIN_KEY", policy.KindGoogleLogin, "value")
		login.Tags[policy.TagGitHubOwners] = "ignored"
		set, err := policytest.Loader(secrets.NewLocal(secret, login)).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set
	}
	first, second := load("Acme-Org  ada"), load("ADA acme-org\tada")
	if got := first.Secrets["APP_KEY"].GitHubOwnerLogins(); !slices.Equal(got, []string{"acme-org", "ada"}) {
		t.Fatalf("GitHubOwnerLogins = %v, want [acme-org ada]", got)
	}
	if got := first.Secrets["APP_KEY"].GitHubOwners; got != "acme-org ada" {
		t.Fatalf("GitHubOwners = %q, want the canonical single-space list %q", got, "acme-org ada")
	}
	if first.Version != second.Version {
		t.Fatal("two spellings of one github-owners list made two policy versions")
	}
	if got := first.Secrets["LOGIN_KEY"].GitHubOwners; got != "" {
		t.Fatalf("google-login GitHubOwners = %q, want none", got)
	}
	if narrowed := load("ada"); narrowed.Version == first.Version {
		t.Fatal("narrowing github-owners left the policy version unchanged")
	}
}

// TestAGitHubOwnersCommaIsRefused: github-owners separates its logins with spaces, the form both
// stores hold, and a comma anywhere in it is refused github-owners-malformed rather than read as
// a separator or as part of a login.
func TestAGitHubOwnersCommaIsRefused(t *testing.T) {
	for _, owners := range []string{"acme-org,ada", "acme-org, ada", "ada,"} {
		t.Run(owners, func(t *testing.T) {
			logged := policytest.CaptureLog(t)
			secret := kindSecret("APP_KEY", policy.KindGitHubApp, "value")
			secret.Tags[policy.TagGitHubOwners] = owners
			set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, ok := set.Secrets["APP_KEY"]; ok {
				t.Fatalf("Load served a github-app secret whose github-owners is %q", owners)
			}
			if !strings.Contains(logged.String(), policy.ReasonGitHubOwnersMalformed) {
				t.Fatalf("log %q does not name %q", logged.String(), policy.ReasonGitHubOwnersMalformed)
			}
			if _, err := policy.ParseGitHubOwners(owners); !errors.Is(err, policy.ErrGitHubOwnersComma) || !strings.Contains(err.Error(), "spaces") {
				t.Fatalf("ParseGitHubOwners(%q) = %v; want ErrGitHubOwnersComma naming the space-separated form", owners, err)
			}
		})
	}
}

// TestAGitHubOwnerThatIsNoGitHubLoginIsRefused: each github-owners entry must be GitHub's login
// form, 1 to 39 letters and digits with single hyphens between them, or the secret is refused
// github-owners-malformed; the longest login and single inner hyphens are read.
func TestAGitHubOwnerThatIsNoGitHubLoginIsRefused(t *testing.T) {
	longest := strings.Repeat("a", 39)
	for _, owners := range []string{"-ada", "ada-", "a--b", "ada_lovelace", "ada.org", "@ada", "ada/b", strings.Repeat("a", 40)} {
		t.Run(owners, func(t *testing.T) {
			logged := policytest.CaptureLog(t)
			secret := kindSecret("APP_KEY", policy.KindGitHubApp, "value")
			secret.Tags[policy.TagGitHubOwners] = "acme-org " + owners
			set, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, ok := set.Secrets["APP_KEY"]; ok {
				t.Fatalf("Load served a github-app secret whose github-owners names %q", owners)
			}
			if !strings.Contains(logged.String(), policy.ReasonGitHubOwnersMalformed) {
				t.Fatalf("log %q does not name %q", logged.String(), policy.ReasonGitHubOwnersMalformed)
			}
			if _, err := policy.ParseGitHubOwners(owners); !errors.Is(err, policy.ErrGitHubOwnersLogin) {
				t.Fatalf("ParseGitHubOwners(%q) = %v; want ErrGitHubOwnersLogin", owners, err)
			}
		})
	}
	if got, err := policy.ParseGitHubOwners("A-b-C9 " + longest + " x"); err != nil || got != "a-b-c9 "+longest+" x" {
		t.Fatalf("ParseGitHubOwners of valid logins = %q, %v; want %q", got, err, "a-b-c9 "+longest+" x")
	}
}

// legacyMintTargetPattern is the mint-service-account rule of the brokers before kinds, copied
// from legion-envoy-v13.19.0 through 313fdc221's policy/load.go: a broker of that range refuses a
// secret whose tag does not match it as mint-target-malformed, before anything else it decides.
var legacyMintTargetPattern = regexp.MustCompile(`^[a-z0-9-]{6,30}@[a-z0-9-]+\.iam\.gserviceaccount\.com$`)

// TestTheRollbackGuardIsRefusedByEarlierBrokersAndIgnoredHere: the value agent-secrets writes
// beside a kind fails the earlier brokers' rule, so they refuse a kind secret rather than serve its
// stored key, while this broker serves the secret as the kind it is and never reads the tag.
func TestTheRollbackGuardIsRefusedByEarlierBrokersAndIgnoredHere(t *testing.T) {
	if legacyMintTargetPattern.MatchString(policy.RollbackGuardValue) {
		t.Fatalf("RollbackGuardValue %q matches the earlier brokers' mint-service-account rule, so they would mint for it", policy.RollbackGuardValue)
	}
	for _, kind := range policy.Kinds {
		secret := kindSecret("GUARDED_KEY", kind, "value")
		secret.Tags[policy.TagRollbackGuard] = policy.RollbackGuardValue
		guarded, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		delete(secret.Tags, policy.TagRollbackGuard)
		bare, err := policytest.Loader(secrets.NewLocal(secret)).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if guarded.Secrets["GUARDED_KEY"] != bare.Secrets["GUARDED_KEY"] || guarded.Version != bare.Version {
			t.Fatalf("%s: the rollback guard changed what this broker serves: %+v against %+v", kind, guarded.Secrets["GUARDED_KEY"], bare.Secrets["GUARDED_KEY"])
		}
	}
}
