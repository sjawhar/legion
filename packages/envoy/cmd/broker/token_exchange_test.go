// token_exchange_test.go drives the REAL compiled broker's RFC 8693 token exchange and its RFC
// 7662 introspection route over HTTP, on a real Postgres, against a local OpenID issuer standing
// in for the cluster's: a pod carrying nothing but its projected service-account token and a key
// of its own is answered a Dispatch access token; a listed service account reads that token back;
// a role whose token's life has passed renews with the standing token it still holds; and each
// way in that must not work is refused. Nothing here is a unit test — the binary is the one
// cmd/broker builds, the schema is a fresh one on the test server, and every request crosses a
// socket.
package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// exchangeGrantType is RFC 8693's own grant type, which the library spells in a package its
	// users cannot import.
	exchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"
	// podAccount is the one identity BROKER_EXCHANGE_ACCOUNTS lists here, with the project and
	// the person its entry gives it; otherAccount is listed nowhere; apiAccount is the listed
	// caller of the introspection route, BROKER_API_ACCOUNTS' own.
	podAccount   = "system:serviceaccount:example-ns:example-worker"
	otherAccount = "system:serviceaccount:example-ns:example-other"
	apiAccount   = "system:serviceaccount:example-ns:example-dispatch"
	person       = "ada@example.com"
	project      = "ACME"
	// enrolmentAudience is what a pod's enrolment token and a listed caller's own token carry;
	// exchangeAudience is what a token presented to the exchange must carry, and workflowAudience
	// what a GitHub workflow run's would. A token minted for one is not spendable at another.
	enrolmentAudience = "https://broker.example"
	exchangeAudience  = "https://broker.example/exchange"
	workflowAudience  = "https://broker.example/ci"
	// heldSession is the session name the first pod writes under, which a second pod of the same
	// account may not take; renewalSession is the renewing role's own.
	heldSession    = "example-session"
	renewalSession = "example-renewal"
	// firstPodUID is the pod that holds heldSession; secondPodUID is another pod of the very same
	// listed account, which is another identity.
	firstPodUID  = "pod-uid-1"
	secondPodUID = "pod-uid-2"
	// longAccessSeconds is the access-token lifetime of the run every step but the renewal drives,
	// long enough that a token minted at its start is still live at its end; shortAccessSeconds is
	// the few seconds the renewal run is started with, so that proof waits seconds, not minutes.
	longAccessSeconds  = 900
	shortAccessSeconds = 2
)

// TestTheBinaryExchangesAPodsTokenIntrospectsItAndRenewsIt is the live proof of the exchange: the
// real binary on a fresh schema, a pod's projected token exchanged with a DPoP proof of a key this
// test generates and no client credential at all, the minted token read back by the listed API
// account, a lapsed token renewed from the same standing pod token, and the eight refusals that
// keep the credential-less path from being a way in for anyone else. It skips when
// BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestTheBinaryExchangesAPodsTokenIntrospectsItAndRenewsIt(t *testing.T) {
	databaseURL := storetest.URL(t)
	// Two issuers the test controls: the cluster's, which mints every service-account token here,
	// and GitHub's, which mints none but is what the broker reports its second exchange issuer on.
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "cluster-key")
	github.PublishKey(t, "github-key")

	// The key the caller itself holds: every token the exchange mints here is bound to it, and an
	// introspection answers its thumbprint.
	key := proofKey(t)
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)
	strangerToken := accountToken(t, cluster, clusterKey, otherAccount)

	broker := startBroker(t, brokerOptions{Env: exchangeEnv(t, databaseURL, cluster, github, longAccessSeconds)})
	addr := broker.Addr
	live := newLiveBroker(addr, key)

	t.Run("the real binary boots on a fresh schema with its token exchange on", func(t *testing.T) {
		// The binary logs the state of its exchange after the bind line startBroker synchronises
		// on, so the boot is read once that line has arrived.
		boot := broker.Await(t, "token exchange")
		t.Logf("the broker's own boot lines:\n%s", boot)
		requireHealthy(t, addr)
		for _, want := range []string{
			"broker listening addr=" + addr,
			"token exchange state=on kubernetes=true github=true accounts=1",
		} {
			if !strings.Contains(boot, want) {
				t.Fatalf("the boot log carries no %q; the real binary must log the address it bound — the one that just answered /healthz — and report its token exchange on, with both issuers and the one listed account:\n%s", want, boot)
			}
		}
	})

	standingPodToken := mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, firstPodUID)
	var accessToken string

	t.Run("a pod's token, a proof of the caller's own key and no client credential mint an access token", func(t *testing.T) {
		status, body := live.exchange(t, exchangeForm(standingPodToken, heldSession))
		if status != http.StatusOK {
			t.Fatalf("the exchange of %s's pod token = %d %v, want 200: a listed account's pod, presenting its projected token with a DPoP proof of its own key and no client credential, is answered a Dispatch access token", podAccount, status, body)
		}
		accessToken, _ = body["access_token"].(string)
		if accessToken == "" || body["token_type"] != "DPoP" || body["issued_token_type"] != oauthwire.SubjectTokenTypeAccessToken {
			t.Fatalf("the exchange answered token_type %v, issued_token_type %v and %d bytes of access token, want a DPoP-bound access token of type %s",
				body["token_type"], body["issued_token_type"], len(accessToken), oauthwire.SubjectTokenTypeAccessToken)
		}
	})

	t.Run("a listed service account introspects it and reads the session, the person, the project, the expiry and the key", func(t *testing.T) {
		if accessToken == "" {
			t.Fatal("the exchange minted no access token, so there is nothing to read back")
		}
		status, body := live.introspect(t, accessToken, apiToken)
		if status != http.StatusOK || body["active"] != true {
			t.Fatalf("introspection by the listed API account %s = %d %v, want 200 with the token live", apiAccount, status, body)
		}
		for _, field := range []struct {
			name      string
			got, want any
			proves    string
		}{
			{"session", body["session"], heldSession, "the session the token's writes are recorded as"},
			{"person", body["person"], person, "the person the entry gives that account"},
			{"project", body["project"], project, "the project the entry gives that account"},
			{"project_scope", body["project_scope"], oauth.ProjectScopeNamed, "that the project is the one named project it may write"},
			{"client_id", body["client_id"], oauthwire.ExchangeClientID, "the client the exchange issued it to"},
			{"jkt", body["jkt"], jkt, "the thumbprint of this test's own key, which is what makes a stolen token useless"},
		} {
			if field.got != field.want {
				t.Errorf("introspection answered %s %v, want %v: the answer must name %s", field.name, field.got, field.want, field.proves)
			}
		}
		expiry, _ := body["exp"].(float64)
		if int64(expiry) <= time.Now().Unix() {
			t.Errorf("introspection answered exp %d, which is not in the future: the answer must carry an expiry, so a caller that remembers it keeps it no longer than the token lives", int64(expiry))
		}
	})

	t.Run("a lapsed token is renewed by the same standing pod token", func(t *testing.T) {
		// Its own broker run, on a schema of its own, started with an access-token lifetime of a
		// few seconds: the proof is that the first token lapses, not that the test waits.
		renewalBroker := startBroker(t, brokerOptions{Env: exchangeEnv(t, storetest.URL(t), cluster, github, shortAccessSeconds)})
		renewalBroker.Await(t, "token exchange")
		renewalAddr := renewalBroker.Addr
		renewal := newLiveBroker(renewalAddr, key)
		podToken := mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, "pod-uid-renewal")

		status, body := renewal.exchange(t, exchangeForm(podToken, renewalSession))
		first, _ := body["access_token"].(string)
		if status != http.StatusOK || first == "" {
			t.Fatalf("the renewing role's first exchange = %d %v, want 200 with an access token", status, body)
		}

		time.Sleep((shortAccessSeconds + 1) * time.Second)
		status, body = renewal.introspect(t, first, apiToken)
		if status != http.StatusOK || body["active"] != false {
			t.Fatalf("introspection of the first token %ds after it was minted = %d %v, want 200 with active false: this broker was started with a %ds access-token lifetime, past which its token is not live",
				shortAccessSeconds+1, status, body, shortAccessSeconds)
		}

		status, body = renewal.exchange(t, exchangeForm(podToken, renewalSession))
		second, _ := body["access_token"].(string)
		if status != http.StatusOK || second == "" {
			t.Fatalf("the second exchange of the same standing pod token = %d %v, want 200: a pod's projected token is a standing credential its kubelet refreshes, so a role renewing under its own session name presents it again and is not refused as a replay", status, body)
		}
		if second == first {
			t.Fatal("the renewal answered the very token that lapsed, want a token of its own")
		}
		status, body = renewal.introspect(t, second, apiToken)
		if status != http.StatusOK || body["active"] != true || body["session"] != renewalSession {
			t.Fatalf("introspection of the renewed token = %d %v, want 200, live, under the session %s", status, body, renewalSession)
		}
	})

	// The negative controls on the exchange, each the request that works but for one thing. The
	// broker's own refusals name what they refused; the two the library answers are pinned by
	// their status and OAuth error code alone, which is all the broker decides there.
	for _, refusal := range []struct {
		name   string
		form   url.Values
		status int
		code   string
		says   string
		proves string
	}{
		{
			name:   "an unlisted service account mints nothing",
			form:   exchangeForm(mintPodToken(t, cluster, clusterKey, otherAccount, exchangeAudience, "pod-uid-unlisted"), "example-unlisted"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "no entry lists the service account " + otherAccount,
			proves: "a sound, correctly-audienced pod token of an account BROKER_EXCHANGE_ACCOUNTS does not list is refused, naming the account",
		},
		{
			name:   "a token minted for another audience mints nothing",
			form:   exchangeForm(mintPodToken(t, cluster, clusterKey, podAccount, enrolmentAudience, firstPodUID), "example-wrong-audience"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "not minted for " + exchangeAudience,
			proves: "the listed account's own token, minted for the enrolment audience instead of the exchange's, is refused naming the audience this broker accepts",
		},
		{
			name:   "a token with no pod uid mints nothing",
			form:   exchangeForm(mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, ""), "example-unbound"),
			status: http.StatusBadRequest, code: "invalid_request",
			says:   "bound to no pod",
			proves: "the listed account's token carrying no kubernetes.io pod uid — the kind `kubectl create token` mints — proves only that the account exists, and the session name is bound to a pod",
		},
		{
			name:   "a second pod asking for the session name the first holds mints nothing",
			form:   exchangeForm(mintPodToken(t, cluster, clusterKey, podAccount, exchangeAudience, secondPodUID), heldSession),
			status: http.StatusBadRequest, code: "invalid_grant",
			says:   "Another identity is writing under the session name '" + heldSession + "'",
			proves: "two pods of one account are two identities, and the session name is bound to the first pod's uid rather than to the account",
		},
		{
			name:   "the same exchange carrying a client secret mints nothing",
			form:   with(exchangeForm(standingPodToken, "example-with-secret"), "client_secret", "an-example-secret"),
			status: http.StatusUnauthorized, code: "invalid_client",
			proves: "the credential-less path is not a way in for a caller that presents a credential, right or wrong",
		},
		{
			name:   "the same credential-less request from the machine-login client id mints nothing",
			form:   with(exchangeForm(standingPodToken, "example-other-client"), "client_id", oauthwire.ClientID),
			status: http.StatusBadRequest, code: "invalid_grant",
			proves: "the credential-less path is " + oauthwire.ExchangeClientID + " on the token-exchange grant alone",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			status, body := live.exchange(t, refusal.form)
			description, _ := body["error_description"].(string)
			if status != refusal.status || body["error"] != refusal.code || body["access_token"] != nil {
				t.Fatalf("that exchange = %d %v %q, want %d %s and no access token: %s",
					status, body["error"], description, refusal.status, refusal.code, refusal.proves)
			}
			if refusal.says != "" && !strings.Contains(description, refusal.says) {
				t.Fatalf("the refusal reads %q, which does not carry %q: %s", description, refusal.says, refusal.proves)
			}
		})
	}

	// The negative controls on introspection, which no caller reaches without a listed account's
	// own token: both are refused before the token they ask about is ever read back.
	for _, refusal := range []struct {
		name   string
		bearer string
		status int
		code   string
		proves string
	}{
		{
			name: "an introspection with no caller token is refused", bearer: "",
			status: http.StatusUnauthorized, code: "ACCOUNT_TOKEN_INVALID",
			proves: "the route takes the calling service's own Kubernetes ServiceAccount token, and a request with no Authorization header at all presents none",
		},
		{
			name: "an introspection by an unlisted service account is refused", bearer: strangerToken,
			status: http.StatusForbidden, code: "ACCOUNT_NOT_LISTED",
			proves: "a sound token of this cluster, for the right audience, but of an account BROKER_API_ACCOUNTS does not name, reads nothing back",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			status, body := live.introspect(t, "a-token-nobody-issued", refusal.bearer)
			if status != refusal.status || body["code"] != refusal.code {
				t.Fatalf("that introspection = %d %v, want %d %s: %s", status, body, refusal.status, refusal.code, refusal.proves)
			}
		})
	}
}

// exchangeEnv is what one broker run of this test needs beyond productionEnv: the fresh schema it
// migrates and serves from, the cluster issuer with the audience a pod enrols with and the one it
// exchanges with, the GitHub issuer, the one listed account with its project and person, the
// listed caller of the introspection route, and how long an access token it mints lasts.
func exchangeEnv(t *testing.T, databaseURL string, cluster, github *oidctest.Issuer, accessSeconds int) []string {
	t.Helper()
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_EXCHANGE_K8S_AUDIENCE=" + exchangeAudience,
		"BROKER_EXCHANGE_ACCOUNTS=" + podAccount + "=" + project + "=" + person,
		"BROKER_EXCHANGE_GITHUB_ISSUER=" + github.URL(),
		"BROKER_EXCHANGE_GITHUB_AUDIENCE=" + workflowAudience,
		"BROKER_API_ACCOUNTS=dispatch=" + apiAccount,
		"BROKER_ACCESS_TOKEN_SECONDS=" + strconv.Itoa(accessSeconds),
	}
}

// mintPodToken is a projected service-account token as the cluster's issuer mints one: the account
// it belongs to, the audience it is spendable at, and the pod it is bound to — none when uid is
// empty, which is what `kubectl create token` mints.
func mintPodToken(t *testing.T, cluster *oidctest.Issuer, key *oidctest.Key, account, audience, uid string) string {
	t.Helper()
	claims := cluster.Claims(account, audience)
	if uid != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
	}
	return cluster.Mint(t, key, claims)
}

// accountToken is a service account's own token for the enrolment audience, the one a caller of
// the broker's named-caller routes presents. Its issue time is recent because a token that lives
// longer than an hour from its issue is refused there.
func accountToken(t *testing.T, cluster *oidctest.Issuer, key *oidctest.Key, account string) string {
	t.Helper()
	claims := cluster.Claims(account, enrolmentAudience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	return cluster.Mint(t, key, claims)
}

// exchangeForm is a complete RFC 8693 token exchange request as a pod makes it: the exchange
// client with no credential of its own, the projected token it presents, and the session name it
// asks to write under.
func exchangeForm(subjectToken, session string) url.Values {
	return url.Values{
		"grant_type":         {exchangeGrantType},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {subjectToken},
		"subject_token_type": {oauthwire.SubjectTokenTypeKubernetes},
		"session":            {session},
	}
}

// with is form with one field set, for a control that differs from the request that works by
// exactly that field.
func with(form url.Values, name, value string) url.Values {
	changed := url.Values{}
	maps.Copy(changed, form)
	changed.Set(name, value)
	return changed
}

// liveBroker is one running broker binary as this test drives it: a client that signs an RFC 9449
// DPoP proof of the caller's key for every request, which is what the exchange judges a
// credential-less caller on, and a plain one for the introspection route, which takes a listed
// caller's ServiceAccount token instead.
type liveBroker struct {
	base  string
	dpop  *http.Client
	plain *http.Client
}

// newLiveBroker drives the broker listening at addr, signing a proof of key for every request the
// exchange judges a credential-less caller on.
func newLiveBroker(addr string, key *ecdsa.PrivateKey) *liveBroker {
	return &liveBroker{
		base:  "http://" + addr,
		dpop:  &http.Client{Transport: helper.NewDPoPTransport(key, http.DefaultTransport)},
		plain: &http.Client{},
	}
}

// exchange posts an RFC 8693 token exchange, with a DPoP proof of the caller's key, and answers
// the status and the body the broker gave.
func (l *liveBroker) exchange(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	return l.post(t, l.dpop, "/v1/oauth/token", form, "")
}

// introspect reads token back the way a listed caller does: its own ServiceAccount token on the
// Authorization header, no proof, the token in the form. An empty bearer sends no header at all.
func (l *liveBroker) introspect(t *testing.T, token, bearer string) (int, map[string]any) {
	t.Helper()
	return l.post(t, l.plain, "/v1/oauth/introspect", url.Values{"token": {token}}, bearer)
}

// post sends one form to the broker and logs both halves as the live run's transcript, with every
// credential named rather than repeated.
func (l *liveBroker) post(t *testing.T, client *http.Client, path string, form url.Values, bearer string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, l.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(answer, &body); err != nil {
		t.Fatalf("POST %s answered %d %s, which is not JSON: %v", path, response.StatusCode, answer, err)
	}
	t.Logf("> POST %s %s\n< %d %s", path, shownForm(form), response.StatusCode, shownAnswer(answer, body))
	return response.StatusCode, body
}

// credentialFields are the request fields whose value is a credential; the transcript names those
// and shows every other field in full, since a refusal's own sentence is the evidence.
var credentialFields = map[string]bool{"subject_token": true, "token": true, "client_secret": true}

// shownForm is a request's form as the transcript shows it.
func shownForm(form url.Values) string {
	shown := url.Values{}
	for name, values := range form {
		if credentialFields[name] {
			shown.Set(name, "<"+name+">")
			continue
		}
		shown[name] = values
	}
	return shown.Encode()
}

// shownAnswer is the broker's answer as the transcript shows it: the JSON it sent, with the access
// token it carries named rather than repeated.
func shownAnswer(answer []byte, body map[string]any) string {
	shown := strings.TrimSpace(string(answer))
	if token, _ := body["access_token"].(string); token != "" {
		shown = strings.ReplaceAll(shown, token, "<access_token>")
	}
	return shown
}
