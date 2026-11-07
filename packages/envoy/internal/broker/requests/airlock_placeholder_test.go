package requests

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// The proxy the fixture's releases name, and the certificate they hand over: any two strings the
// release carries through unread, since nothing in this package dials or parses them.
const (
	testProxyURL      = "http://127.0.0.1:13381"
	testCACertificate = "-----BEGIN CERTIFICATE-----\ntest-airlock-ca\n-----END CERTIFICATE-----\n"
)

// newPlaceholderFixture is newAirlockFixture with the airlock actually in force: the Machine
// holds rules, the proxy's URL and its certificate, so a release of a name the rules name takes
// the placeholder path. Each named secret's recorded fingerprint is that entry's own, as a real
// broker's is; the digests map is returned so a test can move one entry under a live grant.
func newPlaceholderFixture(t *testing.T, rules airlock.Rules) (m *Machine, enrollmentID string, key *ecdsa.PrivateKey, digests map[string]string) {
	t.Helper()
	digests = map[string]string{}
	for _, secret := range rules.Secrets {
		digests[secret.Name] = rules.Digest(secret.Name)
	}
	m, enrollmentID, key = newAirlockFixture(t, digests)
	m.Airlock, m.AirlockProxyURL, m.AirlockCACertificate = rules, testProxyURL, testCACertificate
	return m, enrollmentID, key, digests
}

// rule is one entry of a test's rules file: the name, the hosts it is bound to, and its kind.
func rule(name, kind string) airlock.SecretRule {
	return airlock.SecretRule{Name: name, Hosts: []string{"api.example.com"}, Kind: kind}
}

// deliveries reads what each name of a request was released as: 'inject' or 'proxy'.
func deliveries(t *testing.T, m *Machine, requestID string) map[string]string {
	t.Helper()
	rows, err := m.Store.Pool.Query(t.Context(), `select name, delivery from request_secrets where request_id=$1`, requestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, delivery string
		if err := rows.Scan(&name, &delivery); err != nil {
			t.Fatal(err)
		}
		out[name] = delivery
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// placeholderRows reads the grant's rows as "<name>/<slot>" -> placeholder, which is what says
// whether a second release minted anything.
func placeholderRows(t *testing.T, m *Machine, grantID string) map[string]string {
	t.Helper()
	held, err := placeholdersOf(t.Context(), m.Store.Pool, grantID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for name, slots := range held {
		for slot, placeholder := range slots {
			out[name+"/"+slot] = placeholder
		}
	}
	return out
}

// wellFormed fails t unless placeholder is one: the prefix and 32 characters of the alphabet.
func wellFormed(t *testing.T, what, placeholder string) {
	t.Helper()
	rest, ok := strings.CutPrefix(placeholder, PlaceholderPrefix)
	if !ok || len(rest) != placeholderLength || strings.ContainsFunc(rest, func(r rune) bool { return !strings.ContainsRune(placeholderAlphabet, r) }) {
		t.Errorf("%s = %q; want %s and %d characters of %s", what, placeholder, PlaceholderPrefix, placeholderLength, placeholderAlphabet)
	}
}

// holdsNothingOf fails t when the release carries secret anywhere at all — in a value, in a
// placeholder, in a slot name or in a document it re-emitted. The whole answer is compared as one
// string, so a value that escaped into a field nobody thought to assert on is still caught.
func holdsNothingOf(t *testing.T, release Release, secret string) {
	t.Helper()
	answer, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(answer), secret) {
		t.Errorf("the release carries the real value %q: %s", secret, answer)
	}
}

// grantOf creates a request for names and answers its request and grant ids, failing t unless the
// policy granted it at once.
func grantOf(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, reason string, names ...string) (requestID, grantID string) {
	t.Helper()
	request, err := m.Create(t.Context(), enrollmentID, signRequest(t, m, key, reason, names...), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create(%v) = %+v, %v; want a grant at once", names, request, err)
	}
	return request.ID, *request.GrantID
}

// TestAReleaseHoldsBackTheAirlocksNameAndHandsOverTheRest: a grant of one name the rules name and
// one they do not releases the ordinary value and no other, answers the proxy, the certificate,
// the placeholder and the entry's hosts for the name it holds back, and records which of the two
// went out as a placeholder.
func TestAReleaseHoldsBackTheAirlocksNameAndHandsOverTheRest(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AUTO_TOKEN", "")}})
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	requestID, grantID := grantOf(t, m, enr, key, "read both", "AUTO_TOKEN", "SECOND_TOKEN")

	release, err := m.Values(t.Context(), grantID, enr, nil)
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if got, want := release.Values, map[string]string{"SECOND_TOKEN": "second-v1"}; len(got) != 1 || got["SECOND_TOKEN"] != want["SECOND_TOKEN"] {
		t.Fatalf("values = %v, want %v: the airlock's name is left out so a CLI from before the airlock runs nothing", got, want)
	}
	if release.Airlock == nil {
		t.Fatal("the release carries no airlock, so the session could never reach AUTO_TOKEN")
	}
	if release.Airlock.ProxyURL != testProxyURL || release.Airlock.CACertificate != testCACertificate {
		t.Errorf("airlock proxy = %q with certificate %q; want %q and %q",
			release.Airlock.ProxyURL, release.Airlock.CACertificate, testProxyURL, testCACertificate)
	}
	held, named := release.Airlock.Names["AUTO_TOKEN"]
	if !named || len(release.Airlock.Names) != 1 {
		t.Fatalf("airlock names = %v; want AUTO_TOKEN alone", release.Airlock.Names)
	}
	wellFormed(t, "AUTO_TOKEN's placeholder", held.Value)
	if got := held.Slots; len(got) != 1 || got[SlotValue] != held.Value {
		t.Errorf("AUTO_TOKEN's slots = %v; want the value slot holding %q", got, held.Value)
	}
	if !slices.Equal(held.Hosts, []string{"api.example.com"}) {
		t.Errorf("AUTO_TOKEN's hosts = %v; want the entry's, which the command keeps off its NO_PROXY", held.Hosts)
	}
	if release.Airlock.ProxyPassword != held.Value {
		t.Errorf("proxy password = %q; want AUTO_TOKEN's only placeholder %q", release.Airlock.ProxyPassword, held.Value)
	}
	holdsNothingOf(t, release, "auto-v1")
	if got := deliveries(t, m, requestID); got["AUTO_TOKEN"] != "proxy" || got["SECOND_TOKEN"] != "inject" {
		t.Errorf("delivery = %v; want AUTO_TOKEN proxy and SECOND_TOKEN inject", got)
	}
}

// TestASecondReleaseHandsBackTheSamePlaceholders: the placeholders a grant was given are its own
// for as long as it lives, so a session that releases twice — a retry, or a second process —
// finds the strings already in its environment still working, and the table grows no row.
func TestASecondReleaseHandsBackTheSamePlaceholders(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AUTO_TOKEN", "")}})
	_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")

	first, err := m.Values(t.Context(), grantID, enr, nil)
	if err != nil {
		t.Fatalf("first Values: %v", err)
	}
	rows := placeholderRows(t, m, grantID)
	second, err := m.Values(t.Context(), grantID, enr, nil)
	if err != nil {
		t.Fatalf("second Values: %v", err)
	}
	if got, want := second.Airlock.Names["AUTO_TOKEN"], first.Airlock.Names["AUTO_TOKEN"]; got.Value != want.Value || got.Slots[SlotValue] != want.Slots[SlotValue] {
		t.Errorf("the second release = %+v, want the first's placeholders %+v", got, want)
	}
	if second.Airlock.ProxyPassword != first.Airlock.ProxyPassword {
		t.Errorf("the second release's proxy password = %q, want the first's %q", second.Airlock.ProxyPassword, first.Airlock.ProxyPassword)
	}
	if got := placeholderRows(t, m, grantID); len(got) != len(rows) {
		t.Errorf("the second release left %v; want the first's %v and no new row", got, rows)
	}
}

// TestTheReleasedShapeOfEachKind: each kind keeps the shape it releases today, with a placeholder
// wherever its secret was and every other field as it was read or minted, and no answer carries a
// real value anywhere.
func TestTheReleasedShapeOfEachKind(t *testing.T) {
	const (
		awsStored   = `{"access_key_id":"AKIAREALKEYID","secret_access_key":"real-secret-access-key","session_token":"real-session-token","region":"us-west-2"}`
		githubMint  = `{"installations":{"Acme":{"installation_id":42,"token":"ghs_realinstallationtoken","expires_at":"2100-01-01T00:00:00Z"}}}`
		googleToken = "ya29.real-google-access-token"
	)
	for _, c := range []struct {
		name string
		// entryKind is the airlock entry's kind, secret the secret the fixture serves, and
		// minted what its minter answers ("" for a stored secret, which has none).
		entryKind string
		secret    secrets.LocalSecret
		minted    string
		// real is the value that must appear nowhere in the answer, and want the slots the
		// release must mint, each checked against where its placeholder landed.
		real  string
		want  []string
		check func(t *testing.T, held AirlockName)
	}{
		{
			name:   "a stored secret",
			secret: policytest.Secret("SHAPE_KEY", fixtureOperator, policy.TierAgent, "stored-real-value"),
			real:   "stored-real-value",
			want:   []string{SlotValue},
			check: func(t *testing.T, held AirlockName) {
				if held.Value != held.Slots[SlotValue] {
					t.Errorf("value = %q; want the value slot's placeholder %q", held.Value, held.Slots[SlotValue])
				}
			},
		},
		{
			name:   "a google login",
			secret: policytest.KindSecret("SHAPE_KEY", fixtureOperator, policy.TierAgent, policy.KindGoogleLogin, `{"type":"authorized_user"}`),
			minted: googleToken,
			real:   googleToken,
			want:   []string{SlotValue},
			check: func(t *testing.T, held AirlockName) {
				if held.Value != held.Slots[SlotValue] {
					t.Errorf("value = %q; want the value slot's placeholder %q", held.Value, held.Slots[SlotValue])
				}
			},
		},
		{
			name:      "an AWS key pair",
			entryKind: "aws",
			secret:    policytest.Secret("SHAPE_KEY", fixtureOperator, policy.TierAgent, awsStored),
			real:      "real-secret-access-key",
			want:      []string{SlotAWSAccessKeyID, SlotAWSSecretAccessKey, SlotAWSSessionToken},
			check: func(t *testing.T, held AirlockName) {
				var got map[string]string
				if err := json.Unmarshal([]byte(held.Value), &got); err != nil {
					t.Fatalf("the released key pair is not a JSON object: %v", err)
				}
				want := map[string]string{
					"access_key_id":     held.Slots[SlotAWSAccessKeyID],
					"secret_access_key": held.Slots[SlotAWSSecretAccessKey],
					"session_token":     held.Slots[SlotAWSSessionToken],
					"region":            "us-west-2",
				}
				for field, value := range want {
					if got[field] != value {
						t.Errorf("released %s = %q, want %q", field, got[field], value)
					}
				}
				if len(got) != len(want) {
					t.Errorf("the released key pair = %v; want exactly the stored object's fields", got)
				}
			},
		},
		{
			name:   "a GitHub App",
			secret: policytest.KindSecret("SHAPE_KEY", fixtureOperator, policy.TierAgent, policy.KindGitHubApp, "stored-app-key"),
			minted: githubMint,
			real:   "ghs_realinstallationtoken",
			want:   []string{SlotGitHubPrefix + "acme"},
			check: func(t *testing.T, held AirlockName) {
				var got struct {
					Installations map[string]struct {
						ID        int64  `json:"installation_id"`
						Token     string `json:"token"`
						ExpiresAt string `json:"expires_at"`
					} `json:"installations"`
				}
				if err := json.Unmarshal([]byte(held.Value), &got); err != nil {
					t.Fatalf("the released installations are not the minted shape: %v", err)
				}
				acme, named := got.Installations["Acme"]
				if !named || len(got.Installations) != 1 {
					t.Fatalf("released installations = %+v; want the minted Acme alone, under the key GitHub answered with", got.Installations)
				}
				if acme.Token != held.Slots[SlotGitHubPrefix+"acme"] {
					t.Errorf("released Acme token = %q; want its owner's placeholder %q", acme.Token, held.Slots[SlotGitHubPrefix+"acme"])
				}
				if acme.ID != 42 || acme.ExpiresAt != "2100-01-01T00:00:00Z" {
					t.Errorf("released Acme = %+v; want every other field as minted", acme)
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("SHAPE_KEY", c.entryKind)}})
			retag(t, m, c.secret)
			if c.minted != "" {
				m.Minters = map[string]Minter{c.secret.Tags[policy.TagKind]: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
					return secrets.Minted{Value: c.minted}, nil
				}}
			}
			_, grantID := grantOf(t, m, enr, key, "read the secret", "SHAPE_KEY")
			release, err := m.Values(t.Context(), grantID, enr, nil)
			if err != nil {
				t.Fatalf("Values: %v", err)
			}
			if release.Values["SHAPE_KEY"] != "" {
				t.Fatalf("values hold SHAPE_KEY = %q; want the name left out", release.Values["SHAPE_KEY"])
			}
			held := release.Airlock.Names["SHAPE_KEY"]
			if got := slices.Sorted(maps.Keys(held.Slots)); !slices.Equal(got, c.want) {
				t.Fatalf("slots = %v, want %v", got, c.want)
			}
			for slot, placeholder := range held.Slots {
				wellFormed(t, "the placeholder of "+slot, placeholder)
			}
			c.check(t, held)
			holdsNothingOf(t, release, c.real)
		})
	}
}

// TestTheReleasedShapeOfAnAzureLogin is the azure-login row of the shape table, which needs a
// person's approval before its minter ever runs (release refuses an automatic one), so it cannot
// share that table's automatic grant.
func TestTheReleasedShapeOfAnAzureLogin(t *testing.T) {
	const azureMint = `{"tenant_id":"tenant","client_id":"client","federated_token":"real-federated-token","expires_at":"2100-01-01T00:00:00Z"}`
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AZURE_LOGIN", "azure")}})
	retag(t, m, policytest.KindSecret("AZURE_LOGIN", fixtureOperator, policy.TierHuman, policy.KindAzureLogin, `{"tenant_id":"tenant","client_id":"client"}`))
	m.Minters = map[string]Minter{policy.KindAzureLogin: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
		return secrets.Minted{Value: azureMint}, nil
	}}
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "sign in to Azure", "AZURE_LOGIN"), "")
	if err != nil || request.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want a pending request", request, err)
	}
	decision, err := m.ApplyDecision(t.Context(), *request.RecordID, true, fixtureOperator)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decision, err)
	}

	release, err := m.Values(t.Context(), decision.GrantID, enr, nil)
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	held := release.Airlock.Names["AZURE_LOGIN"]
	if got := slices.Sorted(maps.Keys(held.Slots)); !slices.Equal(got, []string{SlotAzureFederated}) {
		t.Fatalf("slots = %v, want the federated token's alone", got)
	}
	wellFormed(t, "the federated token's placeholder", held.Slots[SlotAzureFederated])
	var got map[string]string
	if err := json.Unmarshal([]byte(held.Value), &got); err != nil {
		t.Fatalf("the released login is not a JSON object: %v", err)
	}
	want := map[string]string{"tenant_id": "tenant", "client_id": "client", "federated_token": held.Slots[SlotAzureFederated], "expires_at": "2100-01-01T00:00:00Z"}
	if !maps.Equal(got, want) {
		t.Errorf("released login = %v, want %v: the federated token replaced and every other field as minted", got, want)
	}
	holdsNothingOf(t, release, "real-federated-token")
}

// TestAirlockValuesAnswersEachSlotsRealValue: a placeholder stands for one slot, not for a
// name's whole released value, so what the proxy is given is the real value behind each slot -
// one per field of an AWS pair, one per installation of a github-app - and the slots are the
// ones the release mints placeholders for. The expiry a minter reported comes with them, which
// is what the proxy mints again before.
func TestAirlockValuesAnswersEachSlotsRealValue(t *testing.T) {
	const (
		awsStored  = `{"access_key_id":"AKIAREALKEYID","secret_access_key":"real-secret-access-key","session_token":"real-session-token","region":"us-west-2"}`
		githubMint = `{"installations":{"Acme":{"installation_id":42,"token":"ghs_acme_token","expires_at":"2100-01-01T00:00:00Z"},` +
			`"Zed":{"installation_id":43,"token":"ghs_zed_token","expires_at":"2100-01-01T00:00:00Z"}}}`
		azureMint = `{"tenant_id":"tenant","client_id":"client","federated_token":"real-federated-token","expires_at":"2100-01-01T00:00:00Z"}`
	)
	minted := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		entryKind string
		secret    secrets.LocalSecret
		mint      string
		// approve is for a kind whose release refuses an automatic grant (azure-login).
		approve  bool
		want     map[string]string
		wantKind string
	}{
		{
			name:   "a stored secret",
			secret: policytest.Secret("SHAPE_KEY", fixtureOperator, policy.TierAgent, "stored-real-value"),
			want:   map[string]string{SlotValue: "stored-real-value"},
		},
		{
			name:      "a stored AWS key pair",
			entryKind: "aws",
			secret:    policytest.Secret("SHAPE_KEY", fixtureOperator, policy.TierAgent, awsStored),
			want: map[string]string{
				SlotAWSAccessKeyID:     "AKIAREALKEYID",
				SlotAWSSecretAccessKey: "real-secret-access-key",
				SlotAWSSessionToken:    "real-session-token",
			},
		},
		{
			name:     "a GitHub App of two installations",
			secret:   policytest.KindSecret("SHAPE_KEY", fixtureOperator, policy.TierAgent, policy.KindGitHubApp, "stored-app-key"),
			mint:     githubMint,
			want:     map[string]string{SlotGitHubPrefix + "acme": "ghs_acme_token", SlotGitHubPrefix + "zed": "ghs_zed_token"},
			wantKind: policy.KindGitHubApp,
		},
		{
			name:      "an Azure login",
			entryKind: "azure",
			secret:    policytest.KindSecret("SHAPE_KEY", fixtureOperator, policy.TierHuman, policy.KindAzureLogin, `{"tenant_id":"tenant","client_id":"client"}`),
			mint:      azureMint,
			approve:   true,
			want:      map[string]string{SlotAzureFederated: "real-federated-token"},
			wantKind:  policy.KindAzureLogin,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("SHAPE_KEY", c.entryKind)}})
			retag(t, m, c.secret)
			wantExpires := time.Time{}
			if c.mint != "" {
				wantExpires = minted
				m.Minters = map[string]Minter{c.secret.Tags[policy.TagKind]: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
					return secrets.Minted{Value: c.mint, Expires: minted}, nil
				}}
			}
			var grantID string
			if c.approve {
				request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "sign in", "SHAPE_KEY"), "")
				if err != nil || request.RecordID == nil {
					t.Fatalf("Create = %+v, %v; want a pending request", request, err)
				}
				decision, err := m.ApplyDecision(t.Context(), *request.RecordID, true, fixtureOperator)
				if err != nil || decision.GrantID == "" {
					t.Fatalf("ApplyDecision = %+v, %v; want a grant", decision, err)
				}
				grantID = decision.GrantID
			} else {
				_, grantID = grantOf(t, m, enr, key, "read the secret", "SHAPE_KEY")
			}

			values, _, err := m.AirlockValues(t.Context(), grantID)
			if err != nil {
				t.Fatalf("AirlockValues: %v", err)
			}
			held, named := values["SHAPE_KEY"]
			if !named || len(values) != 1 {
				t.Fatalf("AirlockValues = %v; want SHAPE_KEY alone", values)
			}
			if !maps.Equal(held.Slots, c.want) {
				t.Fatalf("slots = %v, want %v: the real value behind each slot", held.Slots, c.want)
			}
			if held.Kind != c.wantKind {
				t.Errorf("kind = %q, want %q", held.Kind, c.wantKind)
			}
			if !held.Expires.Equal(wantExpires) {
				t.Errorf("expiry = %v, want %v: the minter's own, and none for a stored value", held.Expires, wantExpires)
			}
			// The slots the proxy holds values for are exactly the slots the release mints
			// placeholders for, which is what one walk of the value rather than two buys.
			release, err := m.Values(t.Context(), grantID, enr, nil)
			if err != nil {
				t.Fatalf("Values: %v", err)
			}
			if got, want := slices.Sorted(maps.Keys(release.Airlock.Names["SHAPE_KEY"].Slots)), slices.Sorted(maps.Keys(c.want)); !slices.Equal(got, want) {
				t.Fatalf("the release's slots = %v, want the ones the proxy holds values for, %v", got, want)
			}
		})
	}
}

// TestTheProxyPasswordIsTheFirstPairInSortedOrder: a grant of two airlock names opens its
// connections with the placeholder of the first (name, slot) pair, sorted by name and then by
// slot, so the command and the proxy agree on which of the grant's equally powerful placeholders
// is the password.
func TestTheProxyPasswordIsTheFirstPairInSortedOrder(t *testing.T) {
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("AWS_KEY", "aws"), rule("ZULU_TOKEN", "")}})
	retag(t, m,
		policytest.Secret("AWS_KEY", fixtureOperator, policy.TierAgent, `{"access_key_id":"AKIAONE","secret_access_key":"two"}`),
		policytest.Secret("ZULU_TOKEN", fixtureOperator, policy.TierAgent, "zulu-v1"),
	)
	_, grantID := grantOf(t, m, enr, key, "read both", "AWS_KEY", "ZULU_TOKEN")

	release, err := m.Values(t.Context(), grantID, enr, nil)
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	// AWS_KEY sorts before ZULU_TOKEN, and aws-access-key-id before aws-secret-access-key.
	want := release.Airlock.Names["AWS_KEY"].Slots[SlotAWSAccessKeyID]
	if release.Airlock.ProxyPassword != want {
		t.Fatalf("proxy password = %q; want AWS_KEY's %s placeholder %q", release.Airlock.ProxyPassword, SlotAWSAccessKeyID, want)
	}
}
