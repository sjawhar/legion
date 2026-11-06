// packages/envoy/internal/broker/airlock/airlocktest/airlocktest_test.go
//
// Each stand-in does what it says it does, against the real thing wherever there is one: the
// GitHub and Google stand-ins are driven through the broker's own minters, the AWS one through
// the AWS SDK's own signer, and the Microsoft one through the form az sends. A stand-in that
// quietly stopped answering as the service it stands in for would leave every check that uses
// it passing against nothing, which is what these rows are here to catch.
package airlocktest

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

// The key pair the AWS rows sign with: the one the stand-in holds, in the shape AWS mints them,
// since the stand-in reads the key id out of each call's Credential.
const (
	testAWSKeyID     = "AKIATESTKEYID0000000"
	testAWSSecretKey = "the-test-aws-secret-access-key"
	testAWSRegion    = "us-west-2"
)

// newService starts one stand-in service for a row and closes it when the row ends.
func newService(t *testing.T, o ServiceOptions) *Service {
	t.Helper()
	service, err := NewService(o)
	if err != nil {
		t.Fatalf("the stand-in service: %v", err)
	}
	t.Cleanup(service.Close)
	return service
}

// clientOf is a caller of service: one that trusts its authority and nothing else for an HTTPS
// stand-in, and an ordinary client for a plain one.
func clientOf(service *Service) *http.Client {
	if service.Authority() == nil {
		return &http.Client{Timeout: 10 * time.Second}
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: service.Authority().Pool(), MinVersion: tls.VersionTLS12}},
	}
}

// answerOf sends one request and answers its status and body.
func answerOf(t *testing.T, client *http.Client, request *http.Request) (int, string) {
	t.Helper()
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return answer.StatusCode, string(body)
}

// TestAServiceAnswersItsBodyAndRecordsWhatItGot is what S and T are: a service a check reads a
// request back from, whole - the method, the path, the query, the Host, the headers and the
// body - since a check that cannot see one of those cannot assert about it.
func TestAServiceAnswersItsBodyAndRecordsWhatItGot(t *testing.T) {
	service := newService(t, ServiceOptions{Body: "S answered"})
	request, err := http.NewRequest(http.MethodPost, service.URL()+"/v1/things?page=2", strings.NewReader("the body the command sent"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer the-real-token")
	status, body := answerOf(t, clientOf(service), request)
	if status != http.StatusOK || strings.TrimSpace(body) != "S answered" {
		t.Fatalf("the service answered %d %q; want 200 and its own body", status, body)
	}
	seen := service.Requests()
	if len(seen) != 1 {
		t.Fatalf("the service recorded %d requests; want one", len(seen))
	}
	got := seen[0]
	if got.Method != http.MethodPost || got.Path != "/v1/things" || got.Query != "page=2" {
		t.Errorf("the record reads %s %s?%s; want POST /v1/things?page=2", got.Method, got.Path, got.Query)
	}
	if got.Host != service.Addr() {
		t.Errorf("the record's host is %q; want %q, the authority the caller asked for", got.Host, service.Addr())
	}
	if got.Header.Get("Authorization") != "Bearer the-real-token" {
		t.Errorf("the record's Authorization is %q; want the one the caller sent", got.Header.Get("Authorization"))
	}
	if got.Body != "the body the command sent" {
		t.Errorf("the record's body is %q; want the one the caller sent", got.Body)
	}
}

// TestAServiceTakesACertificateFromTheAuthorityItWasGiven: every stand-in of one run shares one
// authority, so a script points BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE at that one file and the
// proxy trusts them all; and the files the authority writes are the pair a broker mounts as its
// own certificate authority.
func TestAServiceTakesACertificateFromTheAuthorityItWasGiven(t *testing.T) {
	authority, err := NewAuthority("the run's authority")
	if err != nil {
		t.Fatal(err)
	}
	service := newService(t, ServiceOptions{Authority: authority, Hosts: []string{"s.airlock.test", "127.0.0.1"}})
	if service.Authority() != authority {
		t.Fatal("the service made an authority of its own; want the one it was given")
	}
	// The name in the certificate is the one a command asks for, verified against the
	// authority alone: the whole point of handing a stand-in a name of a rules file's.
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: authority.Pool(), ServerName: "s.airlock.test", MinVersion: tls.VersionTLS12},
	}}
	request, err := http.NewRequest(http.MethodGet, service.URL()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := answerOf(t, client, request); status != http.StatusOK {
		t.Fatalf("a call verifying s.airlock.test answered %d %q; want 200", status, body)
	}

	certFile, keyFile, err := authority.WriteFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Fatalf("the files the authority wrote are no key pair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(authority.CertificatePEM()) {
		t.Fatal("the authority's PEM holds no certificate")
	}
}

// TestTheAWSStandVerifiesTheRealKeysSignatureAndRefusesAnother: the stand-in is the check that
// a call reached it signed with the real key, so it has to accept a call the AWS signer made
// with that key and refuse one made with anything else, naming why.
func TestTheAWSStandVerifiesTheRealKeysSignatureAndRefusesAnother(t *testing.T) {
	service := newService(t, ServiceOptions{})
	service.AnswerWith(NewAWSStand(AWSOptions{KeyID: testAWSKeyID, SecretKey: testAWSSecretKey, Body: "the signature verified"}).Answer)
	client := clientOf(service)

	sign := func(t *testing.T, credentials aws.Credentials) (int, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPut, service.URL()+"/v1/upload?Action=Put", strings.NewReader("the artifact"))
		if err != nil {
			t.Fatal(err)
		}
		if err := v4.NewSigner().SignHTTP(t.Context(), credentials, request, hashOfBody("the artifact"), "s3", testAWSRegion, time.Now().UTC()); err != nil {
			t.Fatalf("sign the request: %v", err)
		}
		return answerOf(t, client, request)
	}

	status, body := sign(t, aws.Credentials{AccessKeyID: testAWSKeyID, SecretAccessKey: testAWSSecretKey})
	if status != http.StatusOK || body != "the signature verified" {
		t.Fatalf("a call signed with the real key answered %d %q; want 200 and the stand-in's body", status, body)
	}
	status, body = sign(t, aws.Credentials{AccessKeyID: testAWSKeyID, SecretAccessKey: "another-secret-access-key"})
	if status != http.StatusForbidden || !strings.Contains(body, "the signature is") {
		t.Fatalf("a call signed with another secret answered %d %q; want 403 naming the signature", status, body)
	}
	status, body = sign(t, aws.Credentials{AccessKeyID: "AKIASOMEBODYELSES000", SecretAccessKey: testAWSSecretKey})
	if status != http.StatusForbidden || !strings.Contains(body, "want the real key id") {
		t.Fatalf("a call signed as another key id answered %d %q; want 403 naming the key id", status, body)
	}
	if seen := service.Requests(); len(seen) != 3 || seen[0].Body != "the artifact" {
		t.Fatalf("the stand-in recorded %d calls; want all three, bodies and all", len(seen))
	}
}

// hashOfBody is the hex SHA-256 the signer is given for a body it signs.
func hashOfBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// TestTheGitHubStandAnswersTheInstallationsItWasGiven drives the broker's own GitHub App minter
// against the stand-in: the two routes answer what that minter needs, so a github-app secret
// released against this stand-in holds a token for each owner it was given and for no other.
func TestTheGitHubStandAnswersTheInstallationsItWasGiven(t *testing.T) {
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGitHubStand(GitHubOptions{Installations: map[string]int64{"acme-org": 7, "zed-org": 9}}).Answer)

	_, pemText := githubapptest.Key(t)
	value, err := json.Marshal(map[string]string{"app_id": "123456", "private_key": pemText})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := secrets.GitHubApp{Base: service.URL()}.Mint(t.Context(), secrets.MintRequest{
		Value: string(value), GitHubOwners: []string{"acme-org"},
	})
	if err != nil {
		t.Fatalf("mint a github-app secret against the stand-in: %v", err)
	}
	var held struct {
		Installations map[string]struct {
			InstallationID int64  `json:"installation_id"`
			Token          string `json:"token"`
		} `json:"installations"`
	}
	if err := json.Unmarshal([]byte(minted.Value), &held); err != nil {
		t.Fatalf("the minted value is no JSON object of installations: %v", err)
	}
	if len(held.Installations) != 1 {
		t.Fatalf("the mint holds %d installations; want the one owner it was asked for", len(held.Installations))
	}
	if got := held.Installations["acme-org"]; got.InstallationID != 7 || got.Token != "ghs_standin_acme-org" {
		t.Errorf("acme-org's installation is %+v; want installation 7 and its own token", got)
	}
	if minted.Expires.IsZero() {
		t.Error("the mint has no expiry; want the expires_at the stand-in answered")
	}
	if seen := service.Requests(); len(seen) != 2 || seen[1].Path != "/app/installations/7/access_tokens" {
		t.Errorf("the stand-in was asked %v; want the installations and then acme-org's token", seen)
	}
}

// TestTheGoogleStandsLifetimeReachesTheMinter: the lifetime the stand-in is given is the
// expires_in it answers, and that is what the minted value's expiry is measured from - which is
// what lets a check drive a minted token to within the proxy's five-minute margin by asking for
// a short one.
func TestTheGoogleStandsLifetimeReachesTheMinter(t *testing.T) {
	const lifetime = 7 * time.Minute
	service := newService(t, ServiceOptions{Plain: true})
	service.AnswerWith(NewGoogleStand(GoogleOptions{AccessToken: "the-stand-in-token", Lifetime: lifetime}).Answer)

	asked := time.Now()
	minted, err := secrets.GoogleLogin{TokenURL: service.URL() + "/token"}.Mint(t.Context(), secrets.MintRequest{
		Value: `{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`,
	})
	if err != nil {
		t.Fatalf("mint a google-login secret against the stand-in: %v", err)
	}
	if minted.Value != "the-stand-in-token" {
		t.Errorf("the mint is %q; want the stand-in's access token", minted.Value)
	}
	if got := minted.Expires.Sub(asked); got < lifetime-time.Minute || got > lifetime+time.Minute {
		t.Errorf("the mint lasts %s; want about %s, the stand-in's expires_in", got, lifetime)
	}
	seen := service.Requests()
	if len(seen) != 1 {
		t.Fatalf("the stand-in was asked %d times; want one refresh", len(seen))
	}
	if form, err := url.ParseQuery(seen[0].Body); err != nil || form.Get("refresh_token") != "r" {
		t.Errorf("the stand-in was sent %q; want the refresh token's form", seen[0].Body)
	}
}

// TestTheMicrosoftStandRecordsTheAssertionItWasGiven: what the azure kind is about is which
// assertion reached the token endpoint, so the stand-in has to record each one and answer an
// access token for the right one - and leave every other request to the service behind it,
// since one host plays the endpoint and the API both.
func TestTheMicrosoftStandRecordsTheAssertionItWasGiven(t *testing.T) {
	service := newService(t, ServiceOptions{Body: "the API answered"})
	stand := NewMicrosoftStand(MicrosoftOptions{
		Tokens: []string{"the-real-access-token"}, Assertion: "the.real.federated-token", Lifetime: 3599 * time.Second,
	})
	service.AnswerWith(stand.Answer)
	client := clientOf(service)

	exchange := func(t *testing.T, assertion string) (int, string) {
		t.Helper()
		form := url.Values{
			"grant_type":            {"client_credentials"},
			"client_id":             {"11111111-2222-3333-4444-555555555555"},
			"scope":                 {"https://management.example.com/.default"},
			"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			"client_assertion":      {assertion},
		}
		request, err := http.NewRequest(http.MethodPost, service.URL()+"/the-tenant-id/oauth2/v2.0/token", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return answerOf(t, client, request)
	}

	status, body := exchange(t, "the.real.federated-token")
	if status != http.StatusOK {
		t.Fatalf("the exchange answered %d %q; want 200", status, body)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("the answer is no JSON object: %q", body)
	}
	if answer["access_token"] != "the-real-access-token" || answer["token_type"] != "Bearer" || answer["expires_in"] != float64(3599) {
		t.Errorf("the answer is %v; want Microsoft's own shape around the stand-in's token", answer)
	}

	status, body = exchange(t, "an.assertion.of.its.own")
	if status != http.StatusUnauthorized {
		t.Fatalf("an exchange carrying another assertion answered %d %q; want 401", status, body)
	}

	exchanges := stand.Exchanges()
	if len(exchanges) != 2 {
		t.Fatalf("the stand-in recorded %d exchanges; want both", len(exchanges))
	}
	if got := exchanges[0].Get("client_assertion"); got != "the.real.federated-token" {
		t.Errorf("the first exchange's assertion reads %q; want the real federated token", got)
	}
	if got := exchanges[1].Get("client_assertion"); got != "an.assertion.of.its.own" {
		t.Errorf("the second exchange's assertion reads %q; want the one it refused", got)
	}

	// A request that is no token request is the API's, and the service answers its own body.
	request, err := http.NewRequest(http.MethodGet, service.URL()+"/v1/subscriptions", nil)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := answerOf(t, client, request); status != http.StatusOK || strings.TrimSpace(body) != "the API answered" {
		t.Fatalf("a call that is no exchange answered %d %q; want the service's own body", status, body)
	}
}
