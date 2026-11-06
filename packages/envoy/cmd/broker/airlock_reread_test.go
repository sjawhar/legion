// cmd/broker/airlock_reread_test.go
// One row against the REAL compiled broker: a reread of a secret's name reaches the airlock's
// proxy, so the values the proxy holds for a grant are read again before the next request is
// decided. It is driven through the reread route a person actually uses — POST
// /v1/secrets/{name}/reread on the broker's own port — rather than by calling the proxy's
// DropName, because what this row is about is the wiring between the two.
//
// What it reads is the audit trail: every read the proxy makes writes one grant.used row marked
// airlock, which is the same row a session's own release writes. The rules file the broker is
// given allows no method or path at all, so every request through the proxy is refused by the
// airlock's own decision — which runs after the values are read — and the row needs no service
// for the proxy to reach.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// airlockProxyLine is the line main logs once the proxy port is bound, and the pattern that
// reads the address out of it. The address the release names is BROKER_AIRLOCK_PROXY_URL, which
// a deployment sets to the Service a command reaches; a test binds port 0 and so reads the
// address the process actually took.
const airlockProxyLine = "airlock proxy listening"

var airlockAddrPattern = regexp.MustCompile(`airlock proxy listening addr=(\S+)`)

// airlockProxyAddr is the address the broker bound its proxy port to.
func airlockProxyAddr(t *testing.T, broker *brokerProcess) string {
	t.Helper()
	log := broker.Await(t, airlockProxyLine)
	match := airlockAddrPattern.FindStringSubmatch(log)
	if match == nil {
		t.Fatalf("the broker's %q line names no address:\n%s", airlockProxyLine, log)
	}
	return match[1]
}

// airlockOf is the airlock half of a grant's release: the password every connection to the proxy
// opens with, and each name's slots, which hold the placeholders the command puts in its
// requests. A grant whose one name is behind the airlock gets no value at all, so this is all
// the session has.
func airlockOf(b podBroker, p pod, grantID string) (password string, slots map[string]string) {
	b.t.Helper()
	status, answer := b.signed(p, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusOK {
		b.t.Fatalf("%s's grant values = %d %s, want 200", p.runtimeID, status, answer)
	}
	var release struct {
		Airlock *struct {
			ProxyPassword string `json:"proxy_password"`
			Names         map[string]struct {
				Slots map[string]string `json:"slots"`
			} `json:"names"`
		} `json:"airlock"`
	}
	b.decode(answer, &release)
	if release.Airlock == nil || len(release.Airlock.Names) != 1 {
		b.t.Fatalf("the release carries no airlock for one name: %s", answer)
	}
	for _, held := range release.Airlock.Names {
		slots = held.Slots
	}
	return release.Airlock.ProxyPassword, slots
}

// TestARereadOfASecretReachesTheAirlocksProxy: a new version of a stored value moves neither the
// policy's version nor the set of names it serves, so the reread of the name is the only signal
// the broker gets that the value its proxy is holding is the old one. This row drives that
// signal through the real route and reads, in the audit trail, that the proxy read the grant's
// values again.
func TestARereadOfASecretReachesTheAirlocksProxy(t *testing.T) {
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		podAccount   = "system:serviceaccount:example:example-worker"
		secretName   = "AIRLOCK_TOKEN"
		// boundAddress is a public address literal the entry binds, so that the CONNECT this
		// row makes is neither refused as a local address nor dependent on a resolver this
		// process may not have: an address literal resolves to itself, and the proxy signs the
		// bound host's certificate with that address as its IP SAN.
		boundAddress = "93.184.216.34"
	)
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	writeWhole(t, fakeSecretsFile, `{"secrets": [
		{"name": "`+testSecretsPrefix+`airlock-token", "kms_key_id": "`+testSecretsKMSKeyARN+`",
		 "tags": {"owner": "`+service+`", "tier": "agent"}, "value": "the-real-airlock-token"}
	]}`)
	certFile, keyFile := airlockCAFiles(t)
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + podAudience,
		"BROKER_SERVICE_LOGINS=" + service + "=" + loginAccount,
		"BROKER_SERVICES=" + service + "=" + podAccount,
		"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
		"BROKER_AIRLOCK_CA_CERT_FILE=" + certFile,
		"BROKER_AIRLOCK_CA_KEY_FILE=" + keyFile,
		"BROKER_AIRLOCK_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_AIRLOCK_RULES_FILE=" + airlockRulesFile(t,
			`{"secrets": [{"name": "`+secretName+`", "hosts": ["`+boundAddress+`"], "headers": ["authorization"]}]}`),
	}})
	requireHealthy(t, broker.Addr)
	proxyAddr := airlockProxyAddr(t, broker)

	api := podBroker{t: t, base: "http://" + broker.Addr, issuer: issuer, key: podKey, audience: podAudience}
	launcher := api.logIn(service, loginAccount)
	p := api.enroll(launcher, "airlock-pod", podAccount)
	granted := api.ask(p, secretName)
	if granted.GrantID == nil {
		t.Fatalf("the pod's request for %s = %+v, want a grant at once", secretName, granted)
	}
	grantID := *granted.GrantID
	password, slots := airlockOf(api, p, grantID)
	placeholder := slots[requests.SlotValue]
	if password == "" || placeholder == "" {
		t.Fatalf("the release carries no proxy password or no value placeholder: %q, %v", password, slots)
	}

	// A command's own client: the proxy as its HTTPS_PROXY, the broker's airlock authority as
	// the one thing it trusts, and the placeholder where the token would be.
	anchor := x509.NewCertPool()
	certificate, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if !anchor.AppendCertsFromPEM(certificate) {
		t.Fatalf("the airlock authority's certificate did not parse: %s", certFile)
	}
	proxyURL, err := url.Parse("http://agent:" + password + "@" + proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: anchor, MinVersion: tls.VersionTLS12},
	}}
	call := func(path string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, "https://"+boundAddress+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+placeholder)
		answer, err := client.Do(request)
		if err != nil {
			t.Fatalf("GET %s through the airlock's proxy: %v", path, err)
		}
		defer answer.Body.Close()
		// The rules file allows no method or path, so the decision refuses every request -
		// after the values the decision needs have been read, which is what this row counts.
		if answer.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s through the airlock's proxy = %d, want 403: the rules allow nothing", path, answer.StatusCode)
		}
	}

	ctx := context.Background()
	db, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the broker's database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(ctx) })
	// Every read the proxy makes writes one grant.used row marked airlock; a session's own
	// release writes one without that mark, which is why the mark is in the predicate.
	proxyReads := func() int {
		t.Helper()
		var reads int
		if err := db.QueryRow(ctx, `select count(*) from audit
			where kind='grant.used' and grant_id=$1 and detail->>'airlock'='true'`, grantID).Scan(&reads); err != nil {
			t.Fatalf("count the proxy's reads of grant %s: %v", grantID, err)
		}
		return reads
	}

	call("/v1/first")
	if reads := proxyReads(); reads != 1 {
		t.Fatalf("the proxy read the grant's values %d times for its first request; want once", reads)
	}
	call("/v1/second")
	if reads := proxyReads(); reads != 1 {
		t.Fatalf("the proxy read the grant's values %d times for two requests; want once: the answer is held", reads)
	}

	answer, err := http.Post("http://"+broker.Addr+"/v1/secrets/"+secretName+"/reread", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/secrets/%s/reread: %v", secretName, err)
	}
	defer answer.Body.Close()
	var reread struct {
		Served bool   `json:"served"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(answer.Body).Decode(&reread); err != nil || answer.StatusCode != http.StatusOK || !reread.Served {
		t.Fatalf("reread %s = status %d, %+v, %v; want 200 and served", secretName, answer.StatusCode, reread, err)
	}

	call("/v1/third")
	if reads := proxyReads(); reads != 2 {
		t.Fatalf("the proxy read the grant's values %d times after the reread; want twice: the reread of the name is what says the value may be new", reads)
	}
}
