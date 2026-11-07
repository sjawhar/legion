// requester_rule_e2e_test.go is INFRA-1417's production-like proof: the REAL compiled broker
// binary and the REAL compiled agent-secrets client, driven over sockets against a real Postgres,
// with the requesters tag deciding every answer. Nothing here is a unit test — the binaries are
// the ones cmd/broker and cmd/agent-secrets build, the schema is a fresh one on the test server,
// the policy comes from a fake secrets store the broker reads off disk, and every refusal is read
// off the client's own output rather than out of the broker's internals.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// The three people of this run, each at the Workspace domain the broker signs people in from
	// (brokertest.SignInDomain). They are named rather than spelled at each assertion so a
	// failure reads "aditya@example.com was refused", and they are example.com addresses because
	// this repository is public.
	jason  = "jason@example.com"
	aditya = "aditya@example.com"
	sami   = "sami@example.com"

	// The four secrets of the spec's own table. The first three list who may ask for them and so
	// must bound their grants to an hour (policy.RequesterRules); requesterOpenSecret lists
	// nobody, which is the negative control: it behaves exactly as it did before the change.
	requesterGCloudSami = "GCLOUD_SAMI"
	requesterAzureAdmin = "AZURE_ADMIN"
	requesterDevAditya  = "DEV_ADITYA"
	requesterOpenSecret = "TEAM_WIKI_TOKEN"

	requesterGCloudValue = "gcloud-sami-value"
	requesterAzureValue  = "azure-admin-value"
	requesterOpenValue   = "team-wiki-value"

	// requesterMaxGrant is the hour cap every listed secret carries, and requesterTTL the --ttl
	// a request for one must sign: the broker refuses a longer lifetime before it ever reads the
	// requesters tag (requests.ErrGrantRequiresLifetime), so a request that named none would be
	// refused for the wrong reason.
	requesterMaxGrant = "1h"
	requesterTTL      = "1h"

	// requesterDaemonService is the registered service the two human secrets list beside their
	// people, and requesterDaemonAccount the ServiceAccount BROKER_SERVICES registers it by: a
	// requesters entry that is not an email must name a service the broker knows, or the policy
	// refuses the whole secret as requesters-malformed.
	requesterDaemonService = "legion-daemon"
	requesterDaemonAccount = "system:serviceaccount:example:example-daemon"

	// The shared box aditya signs in on: a fleet BROKER_VM_SERVICES lists, whose Google identity
	// token is the actor token of the sign-in trade.
	requesterBoxService = "shared-box"
	requesterBoxAccount = "example-sharedbox@example-project.iam.gserviceaccount.com"

	// requesterServiceAccount is the Google service account DEV_ADITYA mints a token as, in the
	// one shape secrets.GoogleServiceAccount accepts, and requesterMintedToken what the fake IAM
	// Service Account Credentials endpoint answers for it.
	requesterServiceAccount = "dev-aditya@example-project.iam.gserviceaccount.com"
	requesterMintedToken    = "ya29.fake-dev-aditya-access-token"
	// requesterBrokerOwnToken is the broker's OWN Google credential, the token its application
	// default credentials answer and the one it must present to impersonate the account above.
	requesterBrokerOwnToken = "fake-broker-own-google-token"

	// googleIAMHost is the host secrets.GoogleServiceAccount mints at
	// (internal/broker/secrets/googleserviceaccount.go's googleIAMCredentialsURL). The binary
	// holds it, so this test reaches it by name rather than by a setting.
	googleIAMHost = "iamcredentials.googleapis.com"

	// The two boxes' runtime ids and the session names their enrollments record, which is what
	// the grant log names each refusal by.
	requesterJasonBox      = "jason-devbox"
	requesterJasonSession  = "jason-devbox-1"
	requesterAdityaBox     = "shared-box-7"
	requesterAdityaSession = "aditya-shared-box-1"

	// The two exit codes agent-secrets answers a request with, as cmd/agent-secrets names them
	// (exitPending and exitDenied): a package of its own, so they are spelled here, and a drift
	// in either shows up as a row of this test failing on the code it read.
	cliExitPending = 75
	cliExitDenied  = 77
)

// TestRequesterRuleEndToEnd drives the whole requesters feature through the two real binaries:
// one broker process, one fake secrets store, one Postgres schema, and two sessions enrolled the
// two ways a person's session is enrolled — jason's through his own machine login (the device
// grant, approved by him through the routes Dispatch relays) and aditya's through his sign-in on
// a shared box (the RFC 8693 sign-in trade, his Google identity token with the box's as the actor
// token). Every ask below is the real agent-secrets binary talking to that broker over a socket,
// signing with a key its own keygen made.
//
// It proves, in one run: that each of the three refusals the spec names is answered
// requester-not-listed in the CLIENT's own output; that the one session a list does name is
// granted and reads a minted Google token; that a secret with no requesters still opens an
// approval request to its owner, as before the change; that approving a pending request of a
// secret whose list has since dropped that person records a policy denial rather than a grant;
// and that the quarterly access review reads every refusal in GET /v1/grant-log with its session
// and its person.
//
// What this test reaches of the google-service-account kind, and what it does not:
// cmd/broker/main.go builds secrets.GoogleServiceAccount{} with no BaseURL and no HTTP client,
// so no BROKER_* setting points the compiled binary at another IAM endpoint, and this test adds
// none. It reaches one instead through seams every Go program already honours and a deployment
// may already set: GOOGLE_APPLICATION_CREDENTIALS gives the broker its own Google credential
// from an endpoint of this test's, and HTTPS_PROXY with SSL_CERT_FILE sends the mint's own call
// — to the name the binary holds, iamcredentials.googleapis.com — to a TLS endpoint of this
// test's that answers for that name. So the whole path is driven here, request to grant to
// release to mint to the token in the injected command's environment, and the half NOT exercised
// is Google itself: what the minter sends and the error classes it builds out of what comes back
// are pinned by internal/broker/secrets' own TestGoogleServiceAccountMintImpersonatesTheStoredAccount,
// TestGoogleServiceAccountMintNamesGooglesVerdict, TestGoogleServiceAccountMintErrorsQuoteNoToken
// and TestGoogleServiceAccountMintRefusesAValueThatIsNoServiceAccount, and the wiring of the kind
// to that minter by cmd/broker's own TestEachKindReachesItsOwnMinter.
//
// It skips when BROKER_TEST_DATABASE_URL is unset, as every Postgres-backed broker test does.
func TestRequesterRuleEndToEnd(t *testing.T) {
	databaseURL := storetest.URL(t)

	// One local provider stands in for all three issuers the binary is told about, as one
	// provider is two of them in production: the cluster's ServiceAccount tokens
	// (BROKER_K8S_OIDC_ISSUER), the Google identity tokens a machine and a person present
	// (BROKER_GOOGLE_OIDC_ISSUER), and the approval page's own sign-in (BROKER_SIGNIN_ISSUER).
	issuer := oidctest.New(t)
	clusterKey := issuer.PublishKey(t, "cluster-key")
	googleKey := issuer.PublishKey(t, "google-key")
	brokertest.SignIn(issuer, issuer.PublishKey(t, "signin-key"))

	google := startFakeGoogle(t)
	fakeStore := newRequesterStore(t)
	cli := buildAgentSecrets(t)

	broker := startBroker(t, brokerOptions{Env: append([]string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeStore.path,
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_GOOGLE_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_GOOGLE_OIDC_AUDIENCE=" + machineAudience,
		"BROKER_SIGNIN_DEVICE_AUDIENCE=" + deviceAudience,
		"BROKER_SIGNIN_ISSUER=" + issuer.URL(),
		"BROKER_SIGNIN_CLIENT_ID=" + brokertest.SignInClientID,
		"BROKER_SIGNIN_CLIENT_SECRET_FILE=" + signInClientSecretFile(t),
		"BROKER_SIGNIN_DOMAIN=" + brokertest.SignInDomain,
		"BROKER_SERVICES=" + requesterDaemonService + "=" + requesterDaemonAccount,
		"BROKER_VM_SERVICES=" + requesterBoxService + "=" + requesterBoxAccount,
		"BROKER_API_ACCOUNTS=" + dispatchCaller + "=" + apiAccount + " " + reviewCaller + "=" + reviewAccount,
		// Nothing is left in flight when a row ends, so the broker exits at once.
		"BROKER_DRAIN_SECONDS=0",
	}, google.env()...)})
	base := "http://" + broker.Addr
	requireHealthy(t, broker.Addr)
	if strings.Contains(broker.Boot, "agent secret policy refused") {
		t.Fatalf("the broker refused one of this run's secrets at boot; every one of them must be served:\n%s", broker.Boot)
	}

	ctx := context.Background()
	db, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the broker's database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(ctx) })

	dispatchToken := accountToken(t, issuer, clusterKey, apiAccount)
	reviewToken := accountToken(t, issuer, clusterKey, reviewAccount)
	plain := newPlainBroker(broker.Addr)

	t.Log("--- jason's session: his own machine login on his own box, then a session enrolled under it")
	jasonLogin := machineLoginFor(t, plain, base, jason, requesterJasonBox, dispatchToken)
	jasonSession := enrolCLISession(t, cli, base, jasonLogin, jason, requesterJasonBox, requesterJasonSession)

	t.Log("--- aditya's session: his sign-in on a shared box, then a session enrolled under it")
	adityaBoxKey := proofKey(t)
	adityaLogin := signInOnSharedBox(t, newLiveBroker(broker.Addr, adityaBoxKey), adityaBoxKey, issuer, googleKey, aditya, requesterAdityaSession)
	adityaSession := enrolCLISession(t, cli, base, adityaLogin, aditya, requesterAdityaBox, requesterAdityaSession)

	// Each refusal's request id, read off the client's own denial line, so the grant-log row the
	// review reads below is held against the very request the session was refused.
	refused := map[string]string{}

	for _, row := range []struct {
		name    string
		session requesterSession
		secret  string
		proves  string
	}{
		{
			name: "jason's session is refused DEV_ADITYA, which lists aditya alone",
			// The google-service-account kind is served to the one person who owns it, so the
			// list is exactly aditya and jason is off it whatever the tier would have given him.
			session: jasonSession, secret: requesterDevAditya,
			proves: "a secret whose requesters name one person is refused to every other session before owner and tier are read",
		},
		{
			name:    "jason's session is refused GCLOUD_SAMI, which lists the daemon and sami",
			session: jasonSession, secret: requesterGCloudSami,
			proves: "a list of a service and one person admits neither jason's person nor his session's service, which is none",
		},
		{
			name:    "aditya's session is refused AZURE_ADMIN, which lists jason, the daemon and sami",
			session: adityaSession, secret: requesterAzureAdmin,
			proves: "being on one secret's list is not being on another's",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			code, stdout, stderr := row.session.run(t, cli, base, row.secret, "--ttl", requesterTTL, "--", "sh", "-c", "echo ran")
			if code != cliExitDenied {
				t.Fatalf("agent-secrets %s as %s exited %d, want %d (denied): %s\nstdout: %s\nstderr: %s",
					row.secret, row.session.person, code, cliExitDenied, row.proves, stdout, stderr)
			}
			if strings.Contains(stdout, "ran") {
				t.Fatalf("agent-secrets %s as %s ran its command; a denied request must run nothing:\nstdout: %s", row.secret, row.session.person, stdout)
			}
			for _, want := range []string{row.secret, policy.ReasonRequesterNotListed} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("the client's own output does not carry %q:\nstderr: %s\nit must say which secret refused %s and that the rule was the requesters tag",
						want, stderr, row.session.person)
				}
			}
			id := requestIDFromDenial(t, stderr)
			refused[row.secret+" as "+row.session.person] = id
			t.Logf("%s was refused %s as request %s", row.session.person, row.secret, id)
		})
	}

	t.Run("aditya's session is granted DEV_ADITYA and reads the token the fake IAM endpoint minted", func(t *testing.T) {
		code, stdout, stderr := adityaSession.run(t, cli, base, requesterDevAditya, "--ttl", requesterTTL,
			"--", "sh", "-c", `printf %s "$`+requesterDevAditya+`"`)
		if code != 0 || stdout != requesterMintedToken {
			t.Fatalf("agent-secrets %s as %s exited %d with stdout %q, want 0 and the minted token %q: the one person the list names is served at once\nstderr: %s",
				requesterDevAditya, aditya, code, stdout, requesterMintedToken, stderr)
		}
		if got := google.connects(); !slices.Contains(got, googleIAMHost+":443") {
			t.Fatalf("the broker's mint reached %v, want a call to %s:443: the release must go to the IAM Service Account Credentials API", got, googleIAMHost)
		}
		want := "/v1/projects/-/serviceAccounts/" + requesterServiceAccount + ":generateAccessToken"
		if got := google.mints(); !slices.Contains(got, want) {
			t.Fatalf("the IAM endpoint was asked for %v, want %s: the mint impersonates the account the stored value names", got, want)
		}
	})

	// The negative control, in this same run: a secret that lists nobody is decided by owner and
	// tier alone, exactly as it was before the requesters tag existed.
	var openRecordID string
	t.Run("a secret with no requesters is still an approval request to its owner", func(t *testing.T) {
		code, stdout, stderr := jasonSession.run(t, cli, base, "request", requesterOpenSecret)
		if code != cliExitPending {
			t.Fatalf("agent-secrets request %s as %s exited %d, want %d (pending): a secret with no requesters reaches its owner as it always has\nstdout: %s\nstderr: %s",
				requesterOpenSecret, jason, code, cliExitPending, stdout, stderr)
		}
		if state := cliField(t, stdout, "state"); state != "pending" {
			t.Fatalf("agent-secrets request %s printed state %q, want pending:\n%s", requesterOpenSecret, state, stdout)
		}
		if strings.Contains(stdout, policy.ReasonRequesterNotListed) {
			t.Fatalf("a secret with no requesters was answered %s:\n%s", policy.ReasonRequesterNotListed, stdout)
		}
		approve := approveURLFrom(t, stdout)
		if !strings.Contains(stdout, "waiting for "+sami+" to approve it") {
			t.Fatalf("agent-secrets request %s does not name %s as the approver:\n%s", requesterOpenSecret, sami, stdout)
		}
		if !strings.HasPrefix(approve, base+"/approve/") {
			t.Fatalf("the approve URL is %q, want a page of the broker at %s", approve, base)
		}
		openRecordID = path.Base(approve)
		t.Logf("%s waits on %s at %s", requesterOpenSecret, sami, approve)
	})

	// The approver's own path, which no other row reaches: a request that was pending while the
	// list still named its person, approved after the list dropped them.
	t.Run("approving a pending request the list no longer allows records a policy denial", func(t *testing.T) {
		code, stdout, stderr := jasonSession.run(t, cli, base, "request", requesterAzureAdmin, "--ttl", requesterTTL)
		if code != cliExitPending {
			t.Fatalf("agent-secrets request %s as %s exited %d, want %d (pending): the list still names jason, and the secret is sami's to approve\nstdout: %s\nstderr: %s",
				requesterAzureAdmin, jason, code, cliExitPending, stdout, stderr)
		}
		requestID := cliField(t, stdout, "request_id")
		recordID := path.Base(approveURLFrom(t, stdout))

		// The list drops jason while his request waits. The store is the file the broker follows,
		// so the retag is a write to it and a reread of that one name, as a `agent-secrets secret
		// retag` would be in production.
		fakeStore.retag(t, requesterAzureAdmin, policy.TagRequesters, requesterDaemonService+" "+sami)
		fakeStore.reread(t, base, requesterAzureAdmin)

		// sami approves it on the broker's own page, as the owner does.
		browser := brokertest.NewBrowser(t, base, http.DefaultTransport, issuer)
		browser.SignIn(t, recordID, brokertest.Person(sami))
		form := brokertest.DecisionForm(t, browser.Page(t, recordID).Body, recordID)
		if form == nil {
			t.Fatalf("sami's page for %s offers no decision; it is hers to approve", recordID)
		}
		res := browser.FromPage(t, "/approve", brokertest.Decision(form, "approve"))
		if want := "/approve/" + recordID + "?decided=" + recordID; res.Status != http.StatusSeeOther || res.Header.Get("Location") != want {
			t.Fatalf("sami's Approve = %d to %q, want 303 to %s:\n%s", res.Status, res.Header.Get("Location"), want, res.Body)
		}
		answer := brokertest.PageText(t, browser.Get(t, res.Header.Get("Location")).Body)
		t.Logf("the page after sami's Approve reads: %s", answer)
		for _, want := range []string{requesterAzureAdmin, policy.ReasonRequesterNotListed} {
			if !strings.Contains(answer, want) {
				t.Fatalf("the page's answer does not carry %q:\n%s\nit must say which secret refused the approval and why", want, answer)
			}
		}

		var state, decidedBy, detail string
		if err := db.QueryRow(ctx, `select state, coalesce(decided_by,''), coalesce(decision_detail,'') from requests where id = $1`,
			requestID).Scan(&state, &decidedBy, &detail); err != nil {
			t.Fatalf("read request %s: %v", requestID, err)
		}
		if state != "denied" || decidedBy != requests.PolicyDeniedActor || detail != requesterAzureAdmin+": "+policy.ReasonRequesterNotListed {
			t.Fatalf("request %s = state %q decided_by %q detail %q, want denied by %s with %q: the policy and not sami made that answer",
				requestID, state, decidedBy, detail, requests.PolicyDeniedActor, requesterAzureAdmin+": "+policy.ReasonRequesterNotListed)
		}
		var grants int
		if err := db.QueryRow(ctx, `select count(*) from grants where request_id = $1`, requestID).Scan(&grants); err != nil {
			t.Fatalf("count the grants of request %s: %v", requestID, err)
		}
		if grants != 0 {
			t.Fatalf("request %s holds %d grants, want none: an approval the policy denied releases nothing", requestID, grants)
		}

		// The session reads that answer itself, through the client, which is the only way it ever
		// learns why its request died.
		code, stdout, stderr = jasonSession.run(t, cli, base, "status", requestID)
		if code != 0 {
			t.Fatalf("agent-secrets status %s exited %d:\nstdout: %s\nstderr: %s", requestID, code, stdout, stderr)
		}
		for _, want := range []struct{ field, value string }{
			{"state", "denied"},
			{"decided_by", requests.PolicyDeniedActor},
			{"detail", requesterAzureAdmin + ": " + policy.ReasonRequesterNotListed},
		} {
			if got := cliField(t, stdout, want.field); got != want.value {
				t.Fatalf("agent-secrets status %s printed %s %q, want %q:\n%s", requestID, want.field, got, want.value, stdout)
			}
		}
		if openRecordID == recordID {
			t.Fatalf("the two pending records are one (%s); each row must have a record of its own", recordID)
		}
	})

	t.Run("the access review reads every refusal in the grant log, with its session and its person", func(t *testing.T) {
		// The route refuses a period whose end is less than an hour old, because an audit id is
		// not commit order: a row of a period still open could be written after the review read
		// it. This run's rows were written seconds ago, so the test moves them into a period that
		// closed two days back rather than waiting the hour out, exactly as
		// internal/broker/api's own TestGrantLog does.
		at := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
		if _, err := db.Exec(ctx, `update audit set at = $1`, at); err != nil {
			t.Fatalf("move this run's audit rows into a closed period: %v", err)
		}
		params := url.Values{
			"since": {at.Add(-time.Hour).Format(time.RFC3339)},
			"until": {at.Add(time.Hour).Format(time.RFC3339)},
		}
		status, answer := plain.send(t, plain.plain, brokerCall{
			method: http.MethodGet, path: grantLogRoute + "?" + params.Encode(),
			bearer: reviewToken, shown: "(as <access-review-token>)",
		})
		if status != http.StatusOK {
			t.Fatalf("GET %s as %s = %d %s, want 200", grantLogRoute, reviewCaller, status, answer)
		}
		var page struct {
			Events []store.GrantLogEvent `json:"events"`
		}
		if err := json.Unmarshal(answer, &page); err != nil {
			t.Fatalf("decode the grant log: %v", err)
		}

		for _, want := range []struct {
			secret  string
			session requesterSession
		}{
			{requesterDevAditya, jasonSession},
			{requesterGCloudSami, jasonSession},
			{requesterAzureAdmin, adityaSession},
		} {
			requestID := refused[want.secret+" as "+want.session.person]
			if requestID == "" {
				t.Fatalf("no refusal of %s as %s was recorded above, so there is nothing to look for", want.secret, want.session.person)
			}
			var found []store.GrantLogEvent
			for _, ev := range page.Events {
				if ev.Kind == "request.created" && ev.RequestID == requestID {
					found = append(found, ev)
				}
			}
			if len(found) != 1 {
				t.Fatalf("the grant log holds %d request.created events for request %s (%s as %s), want exactly one: every refusal is in the review",
					len(found), requestID, want.secret, want.session.person)
			}
			ev := found[0]
			if ev.State != "denied" || !strings.Contains(ev.Detail, policy.ReasonRequesterNotListed) {
				t.Errorf("the grant log's %s refusal = state %q detail %q, want denied carrying %q",
					want.secret, ev.State, ev.Detail, policy.ReasonRequesterNotListed)
			}
			if ev.Person != want.session.person {
				t.Errorf("the grant log's %s refusal names person %q, want %q: the review reads who was refused",
					want.secret, ev.Person, want.session.person)
			}
			if ev.Session.Kind != "session" || ev.Session.SessionID != want.session.name || ev.Session.RuntimeID != want.session.runtimeID {
				t.Errorf("the grant log's %s refusal names session %+v, want the session enrollment %s on %s: the review reads which session was refused",
					want.secret, ev.Session, want.session.name, want.session.runtimeID)
			}
			if ev.EnrollmentID != want.session.enrollmentID {
				t.Errorf("the grant log's %s refusal names enrollment %q, want %q", want.secret, ev.EnrollmentID, want.session.enrollmentID)
			}
			secretRows := []store.GrantLogSecret{{Name: want.secret, Decision: policy.Deny}}
			if !slices.Equal(ev.Secrets, secretRows) {
				t.Errorf("the grant log's %s refusal lists secrets %+v, want %+v", want.secret, ev.Secrets, secretRows)
			}
		}
	})
}

// requesterSession is one enrolled session of this run as the test drives it: the key dir the
// real client signs every call from, the enrollment that key holds, the name and runtime the
// enrollment recorded — which is how the grant log names it — and the person it acts for, so
// every assertion says who was refused what.
type requesterSession struct {
	keyDir       string
	enrollmentID string
	name         string
	runtimeID    string
	person       string
}

// run runs the real agent-secrets binary as this session and answers its exit code and both
// streams. It fails t only when the process could not be run at all: a nonzero exit is an answer
// this test reads, not a failure of the harness.
func (s requesterSession) run(t *testing.T, cli, brokerURL string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(cli, args...)
	cmd.Env = agentSecretsEnv(brokerURL, s.keyDir)
	var out, errs strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errs
	var exit *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exit) {
		t.Fatalf("agent-secrets %s as %s: %v", strings.Join(args, " "), s.person, err)
	}
	t.Logf("$ agent-secrets %s (as %s on %s)\n< exit %d\n< stdout: %s\n< stderr: %s",
		strings.Join(args, " "), s.person, s.name, cmd.ProcessState.ExitCode(), out.String(), errs.String())
	return cmd.ProcessState.ExitCode(), out.String(), errs.String()
}

// enrolCLISession is one agent session as its launcher makes one: the real client's own keygen
// writes the key, by's launcher credential enrols that key's thumbprint as a session of person's,
// and the enrollment id is written beside the key, which is the whole identity every later
// agent-secrets call in this test signs with. The enrollment's operator is the credential's own,
// which is what makes the session that person's to the requesters rule.
func enrolCLISession(t *testing.T, cli, base string, by launcher, person, runtimeID, sessionName string) requesterSession {
	t.Helper()
	keyDir := filepath.Join(t.TempDir(), runtimeID)
	keygen := exec.Command(cli, "keygen", "--out", keyDir)
	keygen.Env = agentSecretsEnv(base, keyDir)
	thumbprint, err := keygen.Output()
	if err != nil {
		t.Fatalf("agent-secrets keygen --out %s: %v", keyDir, err)
	}
	b := podBroker{t: t, base: base}
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodPost, base+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, answer := b.post("/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "session", "runtime_id": runtimeID, "operator": person,
		"thumbprint": strings.TrimSpace(string(thumbprint)), "session_id": sessionName,
	})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	b.decode(answer, &enrolled)
	if status != http.StatusCreated || enrolled.EnrollmentID == "" {
		t.Fatalf("enrol %s's session %s = %d %s, want 201 with an enrollment_id", person, sessionName, status, answer)
	}
	// Written whole and renamed into place, as a launcher writes it, so no agent-secrets call
	// ever reads half an enrollment id.
	writeWhole(t, filepath.Join(keyDir, "enrollment"), enrolled.EnrollmentID+"\n")
	return requesterSession{keyDir: keyDir, enrollmentID: enrolled.EnrollmentID, name: sessionName, runtimeID: runtimeID, person: person}
}

// machineLoginFor is a person's own machine login, the whole RFC 8628 device grant over HTTP: the
// box posts a device authorization signed with a key of its own naming the person who approves
// it, Dispatch's server looks the typed code up and approves it as that person, and the
// credential that approval mints is the one the box enrols its sessions under. That credential
// carries the person as its operator, which is what the requesters rule reads a session by.
func machineLoginFor(t *testing.T, live *liveBroker, base, person, host, dispatchToken string) launcher {
	t.Helper()
	key := proofKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	request, err := record.Sign(key, base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: host},
	}, "", person, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope},
		"dpop_jkt": {thumbprint}, "request": {request},
	}
	status, answer := live.send(t, live.plain, brokerCall{
		method: http.MethodPost, path: "/v1/oauth/device_authorization",
		contentType: formContentType, body: form.Encode(), shown: shownForm(form),
	})
	var started struct {
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(answer, &started); err != nil || status != http.StatusOK || started.UserCode == "" {
		t.Fatalf("%s's device authorization on %s = %d %s, want 200 with a user code", person, host, status, answer)
	}

	body, err := json.Marshal(map[string]string{"code": started.UserCode})
	if err != nil {
		t.Fatal(err)
	}
	status, looked := live.callAs(t, http.MethodPost, "/v1/machine-logins/lookup", dispatchToken, "application/json", string(body))
	recordID, _ := looked["record_id"].(string)
	if status != http.StatusOK || recordID == "" {
		t.Fatalf("Dispatch's lookup of %s's login code = %d %v, want 200 with a record_id", person, status, looked)
	}

	decision, err := json.Marshal(map[string]string{"approver": person, "code": started.UserCode})
	if err != nil {
		t.Fatal(err)
	}
	status, approved := live.callAs(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve", dispatchToken, "application/json", string(decision))
	credentialID, _ := approved["credential_id"].(string)
	if status != http.StatusOK || approved["state"] != "approved" || credentialID == "" {
		t.Fatalf("%s's approval of their own machine login = %d %v, want 200 approved with a credential_id", person, status, approved)
	}
	return launcher{key: key, credentialID: credentialID}
}

// signInOnSharedBox is a person's sign-in on a box they do not own, the RFC 8693 sign-in trade:
// their own Google identity token as the subject, the shared box's as the actor token, and a box
// key of their own that the launcher credential the trade mints is keyed on. That credential
// carries the person as its operator, as a machine login's does, so a session enrolled under it
// is that person's to the requesters rule and no one else's.
func signInOnSharedBox(t *testing.T, live *liveBroker, boxKey *ecdsa.PrivateKey, issuer *oidctest.Issuer, key *oidctest.Key, person, session string) launcher {
	t.Helper()
	// The box is a fleet BROKER_VM_SERVICES lists and pins nothing of, so its token carries no
	// Compute Engine claims, as a shared box reached through a workload identity pool does not.
	status, body := live.exchange(t, signInForm(
		mintPersonToken(t, issuer, key, person, deviceAudience, true, brokertest.SignInDomain),
		mintMachineToken(t, issuer, key, requesterBoxAccount, machineAudience, true, "", ""), session))
	credentialID, _ := body[oauthwire.ExtraCredentialID].(string)
	if status != http.StatusOK || credentialID == "" {
		t.Fatalf("%s's sign-in on %s = %d %v, want 200 with the launcher credential the sign-in holds", person, requesterBoxAccount, status, body)
	}
	// The credential is keyed on the box key the trade's own DPoP proof presented, so that key
	// is what signs the launcher proof enrolling a session under it.
	return launcher{key: boxKey, credentialID: credentialID}
}

// cliField is the value agent-secrets printed for the line "<name>: <value>", failing t when it
// printed no such line: every assertion that reads one names the field it wanted.
func cliField(t *testing.T, out, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if value, ok := strings.CutPrefix(line, name+": "); ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("agent-secrets printed no %q line:\n%s", name, out)
	return ""
}

// approveURLPattern reads the page a pending request waits on out of the sentence agent-secrets
// prints for it (the CLI's waitingOn), which ends in ": <url>".
var approveURLPattern = regexp.MustCompile(`approve it on the secrets broker: (\S+)`)

// approveURLFrom is the approval page agent-secrets named for a pending request.
func approveURLFrom(t *testing.T, out string) string {
	t.Helper()
	match := approveURLPattern.FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("agent-secrets named no approval page for the pending request:\n%s", out)
	}
	return match[1]
}

// denialPattern reads the request id out of the exec form's own denial line on stderr
// ("agent-secrets: request <id> to <source> was denied: <detail>").
var denialPattern = regexp.MustCompile(`agent-secrets: request (\S+) to .* was denied`)

// requestIDFromDenial is the request the client says was denied, so the grant-log row the review
// reads is held against the very request the session was refused.
func requestIDFromDenial(t *testing.T, stderr string) string {
	t.Helper()
	match := denialPattern.FindStringSubmatch(stderr)
	if match == nil {
		t.Fatalf("the client printed no denial line to read a request id from:\n%s", stderr)
	}
	return match[1]
}

// requesterStore is the fake Secrets Manager this run serves, the file
// BROKER_FAKE_SECRETS_FILE names: the four secrets of the spec's table, which a row may retag and
// have the broker read again, as `agent-secrets secret retag` does in production.
type requesterStore struct {
	path string
	rows []secrets.LocalSecret
}

// newRequesterStore writes the run's four secrets and answers the store that holds them.
func newRequesterStore(t *testing.T) *requesterStore {
	t.Helper()
	s := &requesterStore{path: filepath.Join(t.TempDir(), "fake-secrets.json"), rows: []secrets.LocalSecret{
		requesterRow(requesterGCloudSami, sami, policy.TierHuman, requesterGCloudValue, map[string]string{
			policy.TagRequesters: requesterDaemonService + " " + sami,
			policy.TagMaxGrant:   requesterMaxGrant,
		}),
		requesterRow(requesterAzureAdmin, sami, policy.TierHuman, requesterAzureValue, map[string]string{
			policy.TagRequesters: jason + " " + requesterDaemonService + " " + sami,
			policy.TagMaxGrant:   requesterMaxGrant,
		}),
		// A google-service-account secret is served to the one person who owns it, so its
		// requesters are exactly that person (policy.RequesterRules), and its stored value names
		// the account the broker impersonates.
		requesterRow(requesterDevAditya, aditya, policy.TierAgent, `{"service_account":"`+requesterServiceAccount+`"}`, map[string]string{
			policy.TagKind:       policy.KindGoogleServiceAccount,
			policy.TagRequesters: aditya,
			policy.TagMaxGrant:   requesterMaxGrant,
		}),
		// The negative control: no requesters tag at all, and so no max-grant either.
		requesterRow(requesterOpenSecret, sami, policy.TierHuman, requesterOpenValue, nil),
	}}
	s.write(t)
	return s
}

// requesterStoreName is where the secret a session asks for as name sits in the store: its slug
// under the prefix every broker these tests start serves (testSecretsPrefix), the name the
// policy loader reads that secret back out of. The slug is the loader's own (policy.NameToSlug),
// so a fixture naming a secret no session could ask for is a test bug and panics here rather
// than being written and never served, as policytest's own mustSlug does.
func requesterStoreName(name string) string {
	slug, err := policy.NameToSlug(name)
	if err != nil {
		panic(fmt.Sprintf("requester_rule_e2e_test: %q: %v", name, err))
	}
	return testSecretsPrefix + slug
}

// requesterRow is one secret of the fake store: its place in the store (requesterStoreName), the
// key every secret of these tests is encrypted with (testSecretsKMSKeyARN), its owner, its tier
// and whatever of this feature's tags the row sets.
func requesterRow(name, owner, tier, value string, extra map[string]string) secrets.LocalSecret {
	row := secrets.LocalSecret{
		Name:     requesterStoreName(name),
		KmsKeyID: testSecretsKMSKeyARN,
		Tags:     map[string]string{policy.TagOwner: owner, policy.TagTier: tier},
		Value:    value,
	}
	for tag, setting := range extra {
		row.Tags[tag] = setting
	}
	return row
}

// write puts the store's rows in the file the broker follows.
func (s *requesterStore) write(t *testing.T) {
	t.Helper()
	content, err := json.MarshalIndent(map[string]any{"secrets": s.rows}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// retag sets one tag of the secret a session asks for as name and writes the file again. The
// broker reads the new tags only once the name is reread (reread), as it does in production.
func (s *requesterStore) retag(t *testing.T, name, tag, value string) {
	t.Helper()
	stored := requesterStoreName(name)
	for i := range s.rows {
		if s.rows[i].Name != stored {
			continue
		}
		s.rows[i].Tags[tag] = value
		s.write(t)
		return
	}
	t.Fatalf("the fake store holds no secret served as %s", name)
}

// reread has the broker read one name's tags again, the route a retag is followed by, and fails t
// unless the broker still serves it.
func (s *requesterStore) reread(t *testing.T, base, name string) {
	t.Helper()
	response, err := http.Post(base+"/v1/secrets/"+name+"/reread", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/secrets/%s/reread: %v", name, err)
	}
	defer response.Body.Close()
	var answer struct {
		Served bool   `json:"served"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil || response.StatusCode != http.StatusOK || !answer.Served {
		t.Fatalf("reread %s = status %d, %+v, %v; want it served under its new tags", name, response.StatusCode, answer, err)
	}
}

// fakeGoogle is the Google this run gives the broker, which reaches no network at all: a plain
// HTTP endpoint answering the broker's OWN credential, and a TLS endpoint standing in for the IAM
// Service Account Credentials API.
//
// The IAM endpoint has to be reached by name, because the compiled binary holds that name:
// cmd/broker/main.go builds secrets.GoogleServiceAccount{} with no BaseURL, so no BROKER_*
// setting points the minter anywhere else, and this test adds none. It is reached instead through
// seams every Go program already honours and a deployment may already set — HTTPS_PROXY for the
// call and SSL_CERT_FILE for the certificate authority that signed this endpoint's certificate
// for iamcredentials.googleapis.com — so the binary under test is the production one, unchanged.
type fakeGoogle struct {
	credentialsFile string
	caFile          string
	proxyURL        string

	mu      sync.Mutex
	connect []string
	minted  []string
}

// env is what the broker is started with so that its application default credentials and its mint
// both land here.
func (g *fakeGoogle) env() []string {
	return []string{
		"GOOGLE_APPLICATION_CREDENTIALS=" + g.credentialsFile,
		"HTTPS_PROXY=" + g.proxyURL,
		"https_proxy=" + g.proxyURL,
		// Emptied rather than left to the test process's own: a NO_PROXY naming this host would
		// send the mint straight at the real Google instead of at this endpoint.
		"NO_PROXY=",
		"no_proxy=",
		"SSL_CERT_FILE=" + g.caFile,
	}
}

// connects is every host the broker asked the proxy to reach, newest last.
func (g *fakeGoogle) connects() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.connect)
}

// minted is every IAM path the broker asked for a token at.
func (g *fakeGoogle) mints() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.minted)
}

// startFakeGoogle starts both endpoints and the proxy in front of the TLS one, and stops them
// when t ends.
func startFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{}

	// The broker's own credential: gcloud's authorized_user shape, whose token_uri this file
	// names, so the credential is resolved from this endpoint and never from the machine's HOME
	// or a metadata server.
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Errorf("the broker's own-credential endpoint was asked %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": requesterBrokerOwnToken, "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	t.Cleanup(own.Close)

	g.credentialsFile = filepath.Join(t.TempDir(), "application_default_credentials.json")
	credentials, err := json.Marshal(map[string]string{
		"type":          "authorized_user",
		"client_id":     "example-broker.apps.googleusercontent.example",
		"client_secret": "example-broker-secret",
		"refresh_token": "example-broker-refresh-token",
		"token_uri":     own.URL + "/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.credentialsFile, credentials, 0o600); err != nil {
		t.Fatal(err)
	}

	caPEM, leaf := fakeGoogleCertificate(t, googleIAMHost)
	g.caFile = filepath.Join(t.TempDir(), "iam-ca.pem")
	if err := os.WriteFile(g.caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	iam := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.minted = append(g.minted, r.URL.Path)
		g.mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer "+requesterBrokerOwnToken {
			t.Errorf("the IAM endpoint was called with Authorization %q, want the broker's own Google token", got)
			http.Error(w, `{"error":{"message":"unauthenticated"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": requesterMintedToken})
	}))
	iam.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	iam.StartTLS()
	t.Cleanup(iam.Close)

	g.proxyURL = "http://" + startConnectProxy(t, g, iam.Listener.Addr().String())
	return g
}

// startConnectProxy starts the HTTP proxy the broker's mint is pointed at and answers its
// address. It takes the one thing a proxy is for here — a CONNECT to the IAM host — and passes
// the bytes of that tunnel to upstream, this test's TLS endpoint, which is what lets the binary
// reach a name it holds without a setting of its own. It stops when t ends.
func startConnectProxy(t *testing.T, g *fakeGoogle, upstream string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the mint's proxy: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go proxyOneTunnel(g, client, upstream)
		}
	}()
	return listener.Addr().String()
}

// proxyOneTunnel answers one CONNECT and copies the tunnel both ways until either side closes.
// Everything that is not a CONNECT is closed unanswered: the broker makes no other call here, and
// a run where it did must fail on the missing mint rather than quietly be served something else.
func proxyOneTunnel(g *fakeGoogle, client net.Conn, upstream string) {
	defer client.Close()
	buffered := bufio.NewReader(client)
	request, err := http.ReadRequest(buffered)
	if err != nil || request.Method != http.MethodConnect {
		return
	}
	g.mu.Lock()
	g.connect = append(g.connect, request.Host)
	g.mu.Unlock()
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer server.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// The buffered reader, not the connection: a client that sent its first TLS bytes with the
	// CONNECT would have left them in the buffer, and reading the connection would lose them.
	go func() {
		_, _ = io.Copy(server, buffered)
		if half, ok := server.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, server)
}

// fakeGoogleCertificate mints the certificate authority SSL_CERT_FILE gives the broker and a
// certificate for host signed by it, so the broker's own TLS verification of the name it holds
// succeeds against this test's endpoint and nothing is skipped.
func fakeGoogleCertificate(t *testing.T, host string) (caPEM []byte, leaf tls.Certificate) {
	t.Helper()
	authorityKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "agent-secrets test certificate authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	authorityDER, err := x509.CreateCertificate(rand.Reader, authority, authority, &authorityKey.PublicKey, authorityKey)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := x509.ParseCertificate(authorityDER)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, parent, &serverKey.PublicKey, authorityKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authorityDER}),
		tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}
}
