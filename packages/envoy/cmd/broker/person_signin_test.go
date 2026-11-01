// person_signin_test.go drives the REAL compiled broker's person half of the RFC 8693 token
// exchange over HTTP, on a real Postgres, against a local OpenID issuer standing in for Google:
// a person who signed in on a machine with `agent-secrets login` trades their own Google
// identity token, with that machine's as the actor token, for a Dispatch access token naming
// both; each way in that must not work is refused with a message of its own; and once the
// person revokes that sign-in through the route Dispatch relays, that box trades nothing more
// while the person's other box goes on trading. Nothing here is a unit test — the binary is the
// one cmd/broker builds, the schema is a fresh one on the test server, and every request
// crosses a socket.
package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// deviceAudience is the OAuth client id of the device-flow client `agent-secrets login` runs
	// Google's device grant as, which BROKER_SIGNIN_DEVICE_AUDIENCE names and a person's own ID
	// token carries in its `aud`. It is an audience of its own beside machineAudience, so
	// neither token is spendable where the other is.
	deviceAudience = "example-device-client.apps.googleusercontent.example"
	// signInDomain is the Workspace domain BROKER_SIGNIN_DOMAIN gives, which person belongs to;
	// outsider is a sound Google account of any other domain.
	signInDomain = "example.com"
	outsider     = "mallory@other.example"
)

// TestTheBinaryTakesAPersonsSignInOnAMachine is the live proof of the sign-in grant: the real
// binary on a fresh schema, a person's own Google identity token presented with the machine's as
// the actor token and a proof of the box key `agent-secrets login` made, answered a DPoP-bound
// access token that the listed caller reads back as that person on that machine, held to no one
// project; the four refusals the spec names, each with its own message; and the revoke —
// relayed by the dispatch caller through the route Dispatch's page calls — after which that
// box's next trade is refused naming the sign-in command while the person's second box trades
// on.
//
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestTheBinaryTakesAPersonsSignInOnAMachine(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	github.PublishKey(t, "github-key")
	clusterKey := cluster.PublishKey(t, "cluster-key")
	googleKey := google.PublishKey(t, "google-key")

	// Two boxes of the one person, each with a box key of its own, as two real machines have:
	// a sign-in is told from the person's others by that key alone.
	firstBox, secondBox := proofKey(t), proofKey(t)
	firstJKT, err := proof.Thumbprint(&firstBox.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)

	broker := startBroker(t, brokerOptions{Env: personExchangeEnv(t, databaseURL, cluster, github, google, longAccessSeconds)})
	live := newLiveBroker(broker.Addr, firstBox)
	secondLive := newLiveBroker(broker.Addr, secondBox)
	plain := newPlainBroker(broker.Addr)

	t.Run("the real binary boots with the device-flow audience its sign-in settings make", func(t *testing.T) {
		boot := broker.Await(t, "google person sign-in")
		t.Logf("the broker's own boot lines:\n%s", boot)
		requireHealthy(t, broker.Addr)
		want := "google person sign-in issuer=" + google.URL() + " audience=" + deviceAudience + " domain=" + signInDomain
		if !strings.Contains(boot, want) {
			t.Fatalf("the boot log carries no %q; the binary must report the device-flow audience and the domain it signs people in from:\n%s", want, boot)
		}
	})

	// The machine both boxes run on: a listed fleet whose entry pins its Compute Engine project
	// and instance name, so the actor token is held to that box as well as to that account.
	machineToken := func() string {
		return mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName)
	}

	t.Run("a person's token with the machine's actor token mints a token naming both", func(t *testing.T) {
		status, body := live.exchange(t, signInForm(
			mintPersonToken(t, google, googleKey, person, deviceAudience, true, signInDomain),
			machineToken(), "ada-box-1"))
		token, _ := body["access_token"].(string)
		if status != http.StatusOK || token == "" {
			t.Fatalf("the trade of %s's sign-in on %s = %d %v, want 200: a person signed in on a listed machine is answered a Dispatch access token", person, pinnedFleetAccount, status, body)
		}
		if body["token_type"] != "DPoP" || body["issued_token_type"] != oauthwire.SubjectTokenTypeAccessToken {
			t.Fatalf("the trade answered token_type %v and issued_token_type %v, want a DPoP-bound access token", body["token_type"], body["issued_token_type"])
		}
		status, answer := live.introspect(t, token, apiToken)
		if status != http.StatusOK || answer["active"] != true {
			t.Fatalf("introspection of the person's token = %d %v, want 200 with the token live", status, answer)
		}
		for _, field := range []struct {
			name      string
			got, want any
			proves    string
		}{
			{"session", answer["session"], "ada-box-1", "the session the person asked to write under"},
			{"person", answer["person"], person, "the person whose own Google token was the subject"},
			{"machine", answer["machine"], pinnedFleetAccount, "the machine whose token acted for them"},
			{"service", answer["service"], nil, "no service, since this token names a person"},
			{"project", answer["project"], nil, "no project: a person reaches whichever projects they reach"},
			{"project_scope", answer["project_scope"], oauthwire.ProjectScopePerson, "that the token is held to no one project"},
			{"jkt", answer["jkt"], firstJKT, "the box key the sign-in is bound to"},
		} {
			if field.got != field.want {
				t.Errorf("introspection answered %s %v, want %v: the answer must name %s", field.name, field.got, field.want, field.proves)
			}
		}
	})

	// The negative controls on the sign-in grant, each the trade that works but for one thing.
	said := map[string]string{}
	for _, refusal := range []struct {
		name   string
		form   url.Values
		says   string
		proves string
	}{
		{
			name:   "a person of another Workspace domain mints nothing",
			form:   signInForm(mintPersonToken(t, google, googleKey, outsider, deviceAudience, true, "other.example"), machineToken(), "example-outsider"),
			says:   outsider,
			proves: "the domain comes from the hd claim alone, and this broker takes the one BROKER_SIGNIN_DOMAIN names",
		},
		{
			name:   "an email the provider has not verified mints nothing",
			form:   signInForm(mintPersonToken(t, google, googleKey, person, deviceAudience, false, signInDomain), machineToken(), "example-unverified"),
			says:   "email_verified",
			proves: "the broker checks email_verified itself, since a token for an address nobody verified proves no person",
		},
		{
			name:   "a person's token minted for another audience mints nothing",
			form:   signInForm(mintPersonToken(t, google, googleKey, person, machineAudience, true, signInDomain), machineToken(), "example-wrong-audience"),
			says:   deviceAudience,
			proves: "a person's token is spendable only at the device-flow client's audience, never at the machines' one",
		},
		{
			name: "an actor token for an account no entry lists mints nothing",
			form: signInForm(mintPersonToken(t, google, googleKey, person, deviceAudience, true, signInDomain),
				mintMachineToken(t, google, googleKey, strangerFleet, machineAudience, true, fleetCloudProject, fleetInstanceName), "example-unlisted-box"),
			says:   strangerFleet,
			proves: "a person signs in only from a box of an account BROKER_VM_SERVICES lists",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			status, body := live.exchange(t, refusal.form)
			description, _ := body["error_description"].(string)
			if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil {
				t.Fatalf("that trade = %d %v %q, want 400 invalid_request and no access token: %s", status, body["error"], description, refusal.proves)
			}
			if !strings.Contains(description, refusal.says) {
				t.Fatalf("the refusal reads %q, which does not carry %q: %s", description, refusal.says, refusal.proves)
			}
			said[refusal.name] = description
		})
	}
	t.Run("each refusal reads differently", func(t *testing.T) {
		for name, text := range said {
			for other, otherText := range said {
				if name != other && text == otherText {
					t.Errorf("the refusals of %s and of %s read alike: %q", name, other, text)
				}
			}
		}
	})

	t.Run("the person's second box signs in beside the first", func(t *testing.T) {
		status, body := secondLive.exchange(t, signInForm(
			mintPersonToken(t, google, googleKey, person, deviceAudience, true, signInDomain),
			machineToken(), "ada-box-2"))
		if status != http.StatusOK {
			t.Fatalf("the second box's trade = %d %v, want 200: a box key that is simply new is a second sign-in", status, body)
		}
	})

	var firstSignIn string
	t.Run("the dispatch caller lists the person's sign-ins by machine", func(t *testing.T) {
		status, body := plain.callAs(t, http.MethodGet, "/v1/person-signins?person="+url.QueryEscape(person), apiToken, "", "")
		if status != http.StatusOK {
			t.Fatalf("GET /v1/person-signins = %d %v, want 200", status, body)
		}
		rows, _ := body["signins"].([]any)
		if len(rows) != 2 {
			t.Fatalf("the list answered %v, want the two boxes %s signed in on", body, person)
		}
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			if row["machine"] != pinnedFleetAccount {
				t.Errorf("a sign-in names the machine %v, want %s", row["machine"], pinnedFleetAccount)
			}
			if row["thumbprint"] == firstJKT {
				firstSignIn, _ = row["signin_id"].(string)
			}
		}
		if firstSignIn == "" {
			t.Fatalf("the list %v carries no sign-in on the first box's key", body)
		}
	})

	t.Run("revoking one sign-in ends that box and leaves the other trading", func(t *testing.T) {
		status, body := plain.callAs(t, http.MethodPost, "/v1/person-signins/"+firstSignIn+"/revoke", apiToken,
			"application/json", `{"person":"`+person+`"}`)
		if status != http.StatusOK || body["state"] != "revoked" {
			t.Fatalf("the revoke = %d %v, want 200 revoked", status, body)
		}
		status, body = live.exchange(t, signInForm(
			mintPersonToken(t, google, googleKey, person, deviceAudience, true, signInDomain),
			machineToken(), "ada-box-1"))
		description, _ := body["error_description"].(string)
		if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
			t.Fatalf("the revoked box's trade = %d %v %q, want 400 invalid_grant and no access token", status, body["error"], description)
		}
		for _, names := range []string{"agent-secrets login", person} {
			if !strings.Contains(description, names) {
				t.Errorf("the refusal reads %q, which does not name %q", description, names)
			}
		}
		if status, body := secondLive.exchange(t, signInForm(
			mintPersonToken(t, google, googleKey, person, deviceAudience, true, signInDomain),
			machineToken(), "ada-box-2")); status != http.StatusOK {
			t.Fatalf("the person's second box = %d %v, want 200: revoking one sign-in ends one", status, body)
		}
	})
}

// personExchangeEnv is googleExchangeEnv with the person half configured: the audience of the
// device-flow client `agent-secrets login` runs Google's device grant as. The Workspace domain
// is productionEnv's BROKER_SIGNIN_DOMAIN, the very setting the broker's own approval sign-in
// reads, so no second domain is configured anywhere.
func personExchangeEnv(t *testing.T, databaseURL string, cluster, github, google *oidctest.Issuer, accessSeconds int) []string {
	t.Helper()
	return append(googleExchangeEnv(t, databaseURL, cluster, github, google, accessSeconds),
		"BROKER_SIGNIN_DEVICE_AUDIENCE="+deviceAudience)
}

// mintPersonToken is a person's own Google identity token as the device grant answers one: the
// account it was minted for, the audience of the device-flow client, whether Google holds the
// address verified, and the Workspace domain in `hd`.
func mintPersonToken(t *testing.T, google *oidctest.Issuer, key *oidctest.Key, login, audience string, verified any, domain string) string {
	t.Helper()
	claims := google.Claims("109"+login, audience)
	claims["email"] = login
	claims["email_verified"] = verified
	if domain != "" {
		claims["hd"] = domain
	}
	return google.Mint(t, key, claims)
}

// signInForm is a complete RFC 8693 token exchange request as a signed-in person's client makes
// one: the exchange client with no credential of its own, the person's own Google identity token
// as the subject, the machine's as the actor, and the session name the box asks to write under.
func signInForm(personToken, machineToken, session string) url.Values {
	form := exchangeForm(personToken, session)
	form.Set("subject_token_type", oauthwire.SubjectTokenTypePerson)
	form.Set("actor_token", machineToken)
	form.Set("actor_token_type", oauthwire.ActorTokenTypeMachine)
	return form
}
