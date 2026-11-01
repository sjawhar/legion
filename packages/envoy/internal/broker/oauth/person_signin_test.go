package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The machine a person signs in on in these tests and the fleet its entry lists it for. The
// fleet's entry names no person, so nothing but the sign-in decides who the token acts for.
const (
	signInMachine = "example-fleet@example-project.iam.gserviceaccount.com"
	signInFleet   = "example-fleet"
	// strangerMachine is a Google account no entry lists: a box the deployment does not run.
	strangerMachine = "example-stranger@example-project.iam.gserviceaccount.com"
	// otherPersonDomain is any Workspace but signInDomain, and domainStranger a person of it.
	otherPersonDomain = "other.example"
	domainStranger    = "mallory@other.example"
	// machineAudience is what a machine's own Google identity token must carry, beside
	// deviceAudience, which a person's carries.
	machineAudience = "https://broker.example/machines"
)

// signInFixture is an exchange fixture with the Google half and the person half configured: a
// third issuer standing in for Google, the one fleet above listed, the device-flow client's
// audience, and a recorder in place of the broker's sign-in table, so what the exchange handed
// it is what this suite reads.
type signInFixture struct {
	*exchangeFixture
	google    *oidctest.Issuer
	googleKey *oidctest.Key

	mu sync.Mutex
	// recorded is every sign-in the exchange asked to record, in order.
	recorded []oauth.PersonSignIn
	// revoked is the box key thumbprints the recorder answers ErrSignInRevoked for, as the
	// broker's own table answers a sign-in the person has revoked.
	revoked map[string]bool
}

// newSignInFixture starts the Google issuer and builds a provider that takes a person's sign-in
// on a machine of the one listed fleet.
func newSignInFixture(t *testing.T) *signInFixture {
	t.Helper()
	google := oidctest.New(t)
	googleKey := google.PublishKey(t, "google-key")
	googleKeys, err := oidc.NewKeys(context.Background(), google.URL())
	if err != nil {
		t.Fatalf("Google keys: %v", err)
	}
	f := &signInFixture{google: google, googleKey: googleKey, revoked: map[string]bool{}}
	f.exchangeFixture = newExchangeFixture(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicy(t, nil, nil, exchange.GoogleEntry{Account: signInMachine, Service: signInFleet})
		c.Exchange.Google = oauth.IssuerConfig{Issuer: google.URL(), Audience: machineAudience, Keys: googleKeys}
		c.Exchange.PersonAudience = deviceAudience
		// A machine of this fleet never trades for itself here, but a broker that takes a
		// Google subject token needs somewhere to mint its credential, so the configuration is
		// complete as a deployment's is.
		c.Exchange.MachineCredentials = func(context.Context, oauth.MachineCredential) (string, error) {
			return "machine-credential-1", nil
		}
		c.Exchange.PersonSignIns = f.record
	})
	return f
}

// record is oauth.ExchangeConfig.PersonSignIns over the fixture: it keeps what the exchange
// handed it and refuses a box key the test has revoked.
func (f *signInFixture) record(_ context.Context, in oauth.PersonSignIn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked[in.Thumbprint] {
		return fmt.Errorf("%w: the person revoked it", oauth.ErrSignInRevoked)
	}
	f.recorded = append(f.recorded, in)
	return nil
}

// signIns is every sign-in the exchange asked to record so far.
func (f *signInFixture) signIns() []oauth.PersonSignIn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]oauth.PersonSignIn(nil), f.recorded...)
}

// revoke makes the recorder refuse key's thumbprint from now on, as the broker's table does
// once the person has revoked that sign-in.
func (f *signInFixture) revoke(t *testing.T, key *ecdsa.PrivateKey) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[thumbprint(t, key)] = true
}

// personToken is a person's own Google identity token as the device grant answers one: the
// account Google gives it, the audience of the device-flow client, whether Google holds the
// address verified, and the Workspace domain in `hd`.
func (f *signInFixture) personToken(t *testing.T, login, audience string, verified any, domain string) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":            f.google.URL(),
		"sub":            "109" + login,
		"aud":            audience,
		"iat":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(30 * time.Minute).Unix(),
		"email":          login,
		"email_verified": verified,
	}
	if domain != "" {
		claims["hd"] = domain
	}
	return f.google.Mint(t, f.googleKey, claims)
}

// machineToken is the machine's own Google identity token, the one its metadata server answers
// and the one it presents as the actor token while the person signed in on it trades theirs.
func (f *signInFixture) machineToken(t *testing.T, account, audience string, verified any) string {
	t.Helper()
	now := time.Now()
	return f.google.Mint(t, f.googleKey, map[string]any{
		"iss":            f.google.URL(),
		"sub":            "110" + account,
		"aud":            audience,
		"iat":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(30 * time.Minute).Unix(),
		"email":          account,
		"email_verified": verified,
	})
}

// signInForm is a complete sign-in trade: the person's token as the subject and the machine's as
// the actor, under the two types the exchange client is registered for.
func signInForm(personToken, machineToken, session string) url.Values {
	form := exchangeForm(personToken, oauthwire.SubjectTokenTypePerson, session)
	form.Set("actor_token", machineToken)
	form.Set("actor_token_type", oauthwire.ActorTokenTypeMachine)
	return form
}

// TestAPersonSignedInOnAMachineExchangesForATokenNamingBoth is the whole of the grant at this
// layer: the person's own Google identity token with the machine's as the actor is answered an
// access token bound to the box key the proof named, which introspects as the person, the
// machine and the session asked for, held to no one project; and the sign-in the exchange
// recorded names that person, that box key and that machine.
func TestAPersonSignedInOnAMachineExchangesForATokenNamingBoth(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	token := f.exchanged(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1"), key)

	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	for _, field := range []struct {
		name      string
		got, want any
		proves    string
	}{
		{"active", answer.Active, true, "the token the trade minted is live"},
		{"session", answer.Session, "ada-box-1", "the session the person asked to write under"},
		{"person", answer.Person, person, "the person whose own token was the subject"},
		{"machine", answer.Machine, signInMachine, "the machine whose token acted for them"},
		{"service", answer.Service, "", "no service: a sign-in's token names a person"},
		{"project_scope", answer.ProjectScope, oauthwire.ProjectScopePerson, "that the token is held to no one project"},
		{"project", answer.Project, "", "no project, which the person scope names none of"},
		{"jkt", answer.Thumbprint, thumbprint(t, key), "the box key the request's proof presented"},
	} {
		if field.got != field.want {
			t.Errorf("introspection answered %s %v, want %v: it must name %s", field.name, field.got, field.want, field.proves)
		}
	}

	want := []oauth.PersonSignIn{{Person: person, Thumbprint: thumbprint(t, key), Machine: signInMachine}}
	if got := f.signIns(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("the exchange recorded %+v, want %+v: the first trade of a person and a box key is the sign-in", got, want)
	}
}

// TestEachTradeOfOneSignInRecordsTheSameKeyAndASecondBoxIsItsOwn: the second trade of one box is
// the same sign-in used again, and the person signing in on a second box records a second one,
// so revoking either leaves the other to be decided on its own row.
func TestEachTradeOfOneSignInRecordsTheSameKeyAndASecondBoxIsItsOwn(t *testing.T) {
	f := newSignInFixture(t)
	first, second := newKey(t), newKey(t)
	for _, c := range []struct {
		key     *ecdsa.PrivateKey
		session string
	}{{first, "ada-box-1"}, {first, "ada-box-1"}, {second, "ada-box-2"}} {
		f.exchanged(t, signInForm(
			f.personToken(t, person, deviceAudience, true, signInDomain),
			f.machineToken(t, signInMachine, machineAudience, true),
			c.session), c.key)
	}
	got := f.signIns()
	want := []oauth.PersonSignIn{
		{Person: person, Thumbprint: thumbprint(t, first), Machine: signInMachine},
		{Person: person, Thumbprint: thumbprint(t, first), Machine: signInMachine},
		{Person: person, Thumbprint: thumbprint(t, second), Machine: signInMachine},
	}
	if len(got) != len(want) {
		t.Fatalf("the exchange recorded %d sign-ins, want %d: every trade names the key it proved", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sign-in %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestARevokedSignInIsRefusedAndNamesTheSignInCommand: once the person has revoked the box key,
// the very next trade of it is invalid_grant and the refusal says what to run, while the
// person's other box keeps trading.
func TestARevokedSignInIsRefusedAndNamesTheSignInCommand(t *testing.T) {
	f := newSignInFixture(t)
	revoked, kept := newKey(t), newKey(t)
	trade := func(key *ecdsa.PrivateKey, session string) (int, map[string]any) {
		return f.exchange(t, signInForm(
			f.personToken(t, person, deviceAudience, true, signInDomain),
			f.machineToken(t, signInMachine, machineAudience, true),
			session), key)
	}
	f.exchanged(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1"), revoked)
	f.revoke(t, revoked)

	status, body := trade(revoked, "ada-box-1")
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("the revoked box's trade = %d %v, want 400 invalid_grant and no access token", status, body)
	}
	for _, names := range []string{"agent-secrets login", person} {
		if !strings.Contains(description, names) {
			t.Errorf("the refusal reads %q, which does not name %q", description, names)
		}
	}
	if status, body := trade(kept, "ada-box-2"); status != http.StatusOK {
		t.Fatalf("the person's other box = %d %v, want 200: revoking one sign-in leaves the others working", status, body)
	}
}

// TestASignInTradeIsRefusedWhatItCannotDecide walks the refusals this layer owns, each the trade
// that works but for one thing: another domain, an unverified address, either token minted for
// the other's audience, a machine no entry lists, and no actor token at all. Each gives its own
// message, so the person reading it knows which of the five to fix.
func TestASignInTradeIsRefusedWhatItCannotDecide(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	said := map[string]string{}
	for _, c := range []struct {
		name   string
		form   url.Values
		code   string
		says   string
		proves string
	}{
		{
			name: "a person of another Workspace domain",
			form: signInForm(f.personToken(t, domainStranger, deviceAudience, true, otherPersonDomain),
				f.machineToken(t, signInMachine, machineAudience, true), "ada-box"),
			code: "invalid_request", says: domainStranger,
			proves: "the domain comes from the hd claim, which BROKER_SIGNIN_DOMAIN must equal",
		},
		{
			name: "a person's address Google has not verified",
			form: signInForm(f.personToken(t, person, deviceAudience, false, signInDomain),
				f.machineToken(t, signInMachine, machineAudience, true), "ada-box"),
			code: "invalid_request", says: "email_verified",
			proves: "the broker checks email_verified itself, since a token for an address nobody verified proves no person",
		},
		{
			name: "a person's token minted for the machines' audience",
			form: signInForm(f.personToken(t, person, machineAudience, true, signInDomain),
				f.machineToken(t, signInMachine, machineAudience, true), "ada-box"),
			code: "invalid_request", says: deviceAudience,
			proves: "a person's token is spendable only at the device-flow client's audience",
		},
		{
			name: "a machine's token minted for the device client's audience",
			form: signInForm(f.personToken(t, person, deviceAudience, true, signInDomain),
				f.machineToken(t, signInMachine, deviceAudience, true), "ada-box"),
			code: "invalid_request", says: machineAudience,
			proves: "the actor token is held to the audience a machine's own token carries",
		},
		{
			name: "an actor token of an account no entry lists",
			form: signInForm(f.personToken(t, person, deviceAudience, true, signInDomain),
				f.machineToken(t, strangerMachine, machineAudience, true), "ada-box"),
			code: "invalid_request", says: strangerMachine,
			proves: "a person signs in only from a box the deployment lists",
		},
		{
			name: "no actor token at all",
			form: exchangeForm(f.personToken(t, person, deviceAudience, true, signInDomain),
				oauthwire.SubjectTokenTypePerson, "ada-box"),
			code: "invalid_request", says: "actor",
			proves: "a person's token alone says nothing about which box is asking",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.exchange(t, c.form, key)
			description, _ := body["error_description"].(string)
			if status != http.StatusBadRequest || body["error"] != c.code || body["access_token"] != nil {
				t.Fatalf("that trade = %d %v, want 400 %s and no access token: %s", status, body, c.code, c.proves)
			}
			if !strings.Contains(description, c.says) {
				t.Fatalf("the refusal reads %q, which does not carry %q: %s", description, c.says, c.proves)
			}
			said[c.name] = description
		})
	}
	for name, text := range said {
		for other, otherText := range said {
			if name != other && text == otherText {
				t.Errorf("the refusals of %s and of %s read alike: %q", name, other, text)
			}
		}
	}
	if got := f.signIns(); len(got) != 0 {
		t.Errorf("a refused trade recorded %+v, want nothing: a sign-in is written only for a trade the policy answered", got)
	}
}

// TestABrokerWithNoDeviceAudienceRegistersNeitherSignInType: with
// BROKER_SIGNIN_DEVICE_AUDIENCE unset, neither the person's subject type nor the machine's actor
// type is registered, so the library refuses both by name before any handler of the broker's
// runs and nothing of the sign-in path is reachable.
func TestABrokerWithNoDeviceAudienceRegistersNeitherSignInType(t *testing.T) {
	f := newSignInFixture(t)
	plain := newExchangeFixtureWithoutPerson(t, f)
	key := newKey(t)
	status, body := plain.exchange(t, signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box"), key)
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("the trade = %d %v, want 400 invalid_request: a broker told about no device audience takes no person's token", status, body)
	}
	if !strings.Contains(description, oauthwire.SubjectTokenTypePerson) {
		t.Errorf("the refusal reads %q, which does not name the type it refuses", description)
	}
}

// newExchangeFixtureWithoutPerson is a second provider on a database of its own, listing the same
// fleet and the same Google issuer as f and no device-flow audience at all.
func newExchangeFixtureWithoutPerson(t *testing.T, f *signInFixture) *exchangeFixture {
	t.Helper()
	googleKeys, err := oidc.NewKeys(context.Background(), f.google.URL())
	if err != nil {
		t.Fatalf("Google keys: %v", err)
	}
	return newExchangeFixture(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicy(t, nil, nil, exchange.GoogleEntry{Account: signInMachine, Service: signInFleet})
		c.Exchange.Google = oauth.IssuerConfig{Issuer: f.google.URL(), Audience: machineAudience, Keys: googleKeys}
		c.Exchange.MachineCredentials = func(context.Context, oauth.MachineCredential) (string, error) {
			return "machine-credential-1", nil
		}
	})
}

// TestAProviderNeedsItsSignInRecorder: a configuration that takes a person's token and has
// nowhere to record the sign-in is refused at build, as one that takes a machine's and cannot
// mint its credential is, since a sign-in nothing records could never be revoked.
func TestAProviderNeedsItsSignInRecorder(t *testing.T) {
	for _, c := range []struct {
		name   string
		adjust func(*oauth.ExchangeConfig)
		says   string
	}{
		{name: "no recorder", adjust: func(e *oauth.ExchangeConfig) { e.PersonSignIns = nil }, says: "PersonSignIns"},
		{name: "no Google issuer", adjust: func(e *oauth.ExchangeConfig) {
			e.Google = oauth.IssuerConfig{}
			e.MachineCredentials = nil
		}, says: "Google issuer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("building the provider panicked rather than refusing: %v", r)
				}
			}()
			err := buildRefusal(t, c.adjust)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("oauth.New = %v, want a refusal naming %q", err, c.says)
			}
		})
	}
}

// buildRefusal builds a provider whose exchange takes a person's token, with adjust applied to
// the exchange's configuration last, and answers whatever oauth.New said.
func buildRefusal(t *testing.T, adjust func(*oauth.ExchangeConfig)) error {
	t.Helper()
	google := oidctest.New(t)
	google.PublishKey(t, "google-key")
	keys, err := oidc.NewKeys(context.Background(), google.URL())
	if err != nil {
		t.Fatalf("Google keys: %v", err)
	}
	cfg := oauth.ExchangeConfig{
		Policy:             newPolicy(t, nil, nil, exchange.GoogleEntry{Account: signInMachine, Service: signInFleet}),
		Google:             oauth.IssuerConfig{Issuer: google.URL(), Audience: machineAudience, Keys: keys},
		PersonAudience:     deviceAudience,
		MachineCredentials: func(context.Context, oauth.MachineCredential) (string, error) { return "", nil },
		PersonSignIns:      func(context.Context, oauth.PersonSignIn) error { return nil },
	}
	adjust(&cfg)
	_, err = oauth.New(oauth.Config{
		Secret:          make([]byte, 32),
		DeviceLifetime:  time.Minute,
		AccessLifetime:  time.Minute,
		RefreshLifetime: time.Minute,
		ProofSkew:       time.Second,
		VerificationURL: "https://dispatch.example/credentials/machine",
		Exchange:        cfg,
	}, nil)
	return err
}
