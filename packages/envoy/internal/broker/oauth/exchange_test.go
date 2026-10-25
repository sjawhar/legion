package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The listed identities every exchange test is written against: pods of three accounts of one
// cluster and runs of two workflows of one repository, each with the project and person an entry
// gives it.
const (
	exchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	account       = "system:serviceaccount:example-ns:example-worker"
	person        = "ada@example.com"
	project       = "ACME"
	repository    = "example-org/example-repo"
	workflow      = ".github/workflows/release.yaml"
	ref           = "refs/heads/main"
	// daemonAccount is an account whose entry fixes the session daemonSession, as the Legion
	// daemon's does; deployWorkflow is a workflow whose entry fixes deploySession.
	daemonAccount  = "system:serviceaccount:example-ns:example-daemon"
	daemonSession  = "legion-daemon:ACME"
	deployWorkflow = ".github/workflows/deploy.yaml"
	deploySession  = "ci-deploy"
	// otherAccount is listed for another project and another person: a caller who may exchange,
	// and who must not write under any of the session names above.
	otherAccount = "system:serviceaccount:other-ns:example-worker"
	otherProject = "OTHER"
	otherPerson  = "mallory@example.com"
	// podAudience and workflowAudience are what each issuer's tokens must be minted for; a token
	// carrying anything else is not spendable at this broker.
	podAudience      = "https://broker.example"
	workflowAudience = "https://broker.example/ci"
)

// newPolicy builds the exchange's listed identities, failing the test on an entry it refuses.
func newPolicy(t *testing.T, accounts []exchange.AccountEntry, workflows []exchange.WorkflowEntry) *exchange.Set {
	t.Helper()
	set, err := exchange.NewSet(accounts, workflows)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	return set
}

// exchangeFixture is a fixture whose token exchange lists the accounts and workflows above, with
// a local issuer standing in for the cluster's and another for GitHub's.
type exchangeFixture struct {
	*fixture
	cluster    *oidctest.Issuer
	clusterKey *oidctest.Key
	github     *oidctest.Issuer
	githubKey  *oidctest.Key
}

// newExchangeFixture starts both issuers and builds the provider against them, with adjust
// applied to the configuration last so a test can shorten a lifetime or drop an issuer.
func newExchangeFixture(t *testing.T, adjust func(*oauth.Config)) *exchangeFixture {
	t.Helper()
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey, githubKey := cluster.PublishKey(t, "cluster-key"), github.PublishKey(t, "github-key")
	clusterKeys, err := oidc.NewKeys(context.Background(), cluster.URL())
	if err != nil {
		t.Fatalf("cluster keys: %v", err)
	}
	githubKeys, err := oidc.NewKeys(context.Background(), github.URL())
	if err != nil {
		t.Fatalf("GitHub keys: %v", err)
	}
	inner := newFixtureWith(t, func(c *oauth.Config) {
		c.Exchange = oauth.ExchangeConfig{
			Policy: newPolicy(t,
				[]exchange.AccountEntry{
					{Account: account, Project: project, Person: person},
					{Account: daemonAccount, Project: project, Person: person, Session: daemonSession},
					{Account: otherAccount, Project: otherProject, Person: otherPerson},
				},
				[]exchange.WorkflowEntry{
					{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
					{Repository: repository, Workflow: deployWorkflow, Ref: ref, Project: project, Person: person, Session: deploySession},
				}),
			Kubernetes: oauth.IssuerConfig{Issuer: cluster.URL(), Audience: podAudience, Keys: clusterKeys},
			GitHub:     oauth.IssuerConfig{Issuer: github.URL(), Audience: workflowAudience, Keys: githubKeys},
		}
		adjust(c)
	})
	return &exchangeFixture{fixture: inner, cluster: cluster, clusterKey: clusterKey, github: github, githubKey: githubKey}
}

// podToken is a projected service-account token of the account above bound to the pod uid, with
// change applied to its claims so a test can spoil exactly one of them.
func (f *exchangeFixture) podToken(t *testing.T, uid string, change func(map[string]any)) string {
	t.Helper()
	claims := f.cluster.Claims(account, podAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
	if change != nil {
		change(claims)
	}
	return f.cluster.Mint(t, f.clusterKey, claims)
}

// podTokenOf is a projected service-account token of another listed account, bound to the pod uid.
func (f *exchangeFixture) podTokenOf(t *testing.T, of, uid string) string {
	t.Helper()
	return f.podToken(t, uid, func(claims map[string]any) { claims["sub"] = of })
}

// workflowToken is a continuous-integration workflow run's GitHub token, with change applied to
// its claims. Each call mints a fresh jti, as a run does: the exchange accepts one once.
func (f *exchangeFixture) workflowToken(t *testing.T, run string, change func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":              f.github.URL(),
		"sub":              "repo:" + repository + ":ref:" + ref,
		"aud":              workflowAudience,
		"iat":              now.Add(-time.Minute).Unix(),
		"exp":              now.Add(15 * time.Minute).Unix(),
		"jti":              "run-" + run,
		"repository":       repository,
		"workflow_ref":     repository + "/" + workflow + "@" + ref,
		"job_workflow_ref": repository + "/" + workflow + "@" + ref,
		"ref":              ref,
		"run_id":           run,
	}
	if change != nil {
		change(claims)
	}
	return f.github.Mint(t, f.githubKey, claims)
}

// exchangeForm is a complete token-exchange request a test spoils one field at a time: the
// exchange client with no credential of its own, the subject token it presents, and the session
// name it asks to write under.
func exchangeForm(subjectToken, subjectTokenType, session string) url.Values {
	return url.Values{
		"grant_type":         {exchangeGrant},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {subjectToken},
		"subject_token_type": {subjectTokenType},
		"session":            {session},
	}
}

// exchange posts form to the token endpoint with a DPoP proof of key, the key the issued token
// is bound to.
func (f *exchangeFixture) exchange(t *testing.T, form url.Values, key *ecdsa.PrivateKey) (int, map[string]any) {
	t.Helper()
	status, _, body := f.post(t, "/token", form, key)
	return status, body
}

// exchanged is the access token an exchange was answered with, failing the test when it was
// refused: a test that means to be refused reads the answer itself.
func (f *exchangeFixture) exchanged(t *testing.T, form url.Values, key *ecdsa.PrivateKey) string {
	t.Helper()
	status, body := f.exchange(t, form, key)
	return accessTokenOf(t, status, body)
}

// accessTokenOf is the access token an exchange answered with, failing the test when it was
// refused.
func accessTokenOf(t *testing.T, status int, body map[string]any) string {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("exchange = %d %v, want 200", status, body)
	}
	token, _ := body["access_token"].(string)
	if token == "" {
		t.Fatalf("exchange answered %v, which carries no access token", body)
	}
	return token
}

// TestAPodExchangesItsTokenForOneThatIntrospects: a pod presents its projected service-account
// token and is answered an access token bound to the key its proof named, which introspects as
// the session it asked for, the person and project its account is listed with, and that key.
func TestAPodExchangesItsTokenForOneThatIntrospects(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	token := accessTokenOf(t, status, body)
	if body["issued_token_type"] != "urn:ietf:params:oauth:token-type:access_token" {
		t.Errorf("exchange answered issued_token_type %v, want the access token type", body["issued_token_type"])
	}
	if body["token_type"] != "DPoP" {
		t.Errorf("exchange answered token_type %v, want DPoP", body["token_type"])
	}
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	want := oauth.Introspection{
		Active: true, Session: "implementer-g1", Person: person, ProjectScope: oauth.ProjectScopeNamed,
		Project: project, ClientID: oauthwire.ExchangeClientID, Thumbprint: thumbprint(t, key),
		ExpiresAt: answer.ExpiresAt,
	}
	if answer != want {
		t.Errorf("Introspect = %+v, want %+v", answer, want)
	}
	if answer.ExpiresAt <= time.Now().Unix() {
		t.Errorf("the token expires at %d, which is not in the future", answer.ExpiresAt)
	}
}

// TestAPodExchangesTheSameTokenTwice: a pod's projected token is a standing credential its
// kubelet refreshes, so a role that renews exchanges the same one again. Neither the replay store
// nor the session name's claim may refuse the second exchange.
func TestAPodExchangesTheSameTokenTwice(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.podToken(t, "pod-uid-1", nil)
	first := f.exchanged(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	second := f.exchanged(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if first == second {
		t.Fatal("the renewal answered the token the first exchange did")
	}
	for _, token := range []string{first, second} {
		answer, err := f.provider.Introspect(context.Background(), token)
		if err != nil || !answer.Active {
			t.Fatalf("Introspect of a renewed pod's token = %+v, %v, want it live", answer, err)
		}
	}
}

// TestAWorkflowTokenIsRefusedTheSecondTime: a workflow run mints a fresh token per request, so
// one presentation is all a token needs and the second is a replay.
func TestAWorkflowTokenIsRefusedTheSecondTime(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.workflowToken(t, "4242", nil)
	f.exchanged(t, exchangeForm(subject, oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("the second exchange of one workflow token = %d %v, want 400 invalid_request", status, body)
	}
	// A fresh run presents a fresh token, and is the same workflow, so it writes under the same
	// session name.
	fresh := f.workflowToken(t, "4243", nil)
	f.exchanged(t, exchangeForm(fresh, oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
}

// TestASubjectTokenMintedForAnotherAudienceIsRefused: the library checks a subject token's issuer
// and its expiry and never its audience, so this is the broker's own check and a token a pod was
// given for another service is not spendable here.
func TestASubjectTokenMintedForAnotherAudienceIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.podToken(t, "pod-uid-1", func(claims map[string]any) {
		claims["aud"] = []string{"https://another.example"}
	})
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange of a token for another audience = %d %v, want 400 invalid_request", status, body)
	}
	if hint, _ := body["error_description"].(string); !strings.Contains(hint, podAudience) {
		t.Errorf("the refusal reads %q, which does not name the audience this broker accepts", hint)
	}
}

// TestAnUnlistedAccountIsRefused: the pod's token verifies and the cluster is the configured one;
// what refuses it is that no entry lists the service account it was minted for.
func TestAnUnlistedAccountIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.podToken(t, "pod-uid-1", func(claims map[string]any) {
		claims["sub"] = "system:serviceaccount:example-ns:example-other"
	})
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange by an unlisted account = %d %v, want 400 invalid_request", status, body)
	}
}

// TestAPodTokenBoundToNoPodIsRefused: a token carrying no pod uid proves only that the account
// exists, not that Kubernetes minted it for a pod of this cluster.
func TestAPodTokenBoundToNoPodIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.podToken(t, "pod-uid-1", func(claims map[string]any) {
		delete(claims, "kubernetes.io")
	})
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange of a token bound to no pod = %d %v, want 400 invalid_request", status, body)
	}
}

// TestASessionNameAnotherPrincipalHoldsIsRefused: a pod of another listed account is another
// principal, so it may not mint a token naming the session a pod of the first is writing under,
// and the refusal names it; under a name of its own it is answered.
func TestASessionNameAnotherPrincipalHoldsIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	status, body := f.exchange(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-2"), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	refusedTheName(t, status, body, "implementer-g1")
	f.exchanged(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-2"), oauthwire.SubjectTokenTypeKubernetes, "implementer-g2"), key)
}

// refusedTheName fails the test unless an exchange was refused invalid_grant for naming session,
// a name another principal holds, with a refusal that names it.
func refusedTheName(t *testing.T, status int, body map[string]any, session string) {
	t.Helper()
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("an exchange naming the session %s another principal holds = %d %v, want 400 invalid_grant", session, status, body)
	}
	if hint, _ := body["error_description"].(string); !strings.Contains(hint, "'"+session+"' belongs to another identity") {
		t.Errorf("the refusal reads %q, which does not name the session %s as another identity's", hint, session)
	}
	if body["access_token"] != nil {
		t.Errorf("the refusal carries an access token: %v", body)
	}
}

// TestAnExchangeAskingForAJSONWebTokenIsRefused: this provider signs no claim set of its own, so
// it registers no JSON Web Token type and the library refuses a caller asking for one.
func TestAnExchangeAskingForAJSONWebTokenIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	form := exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:jwt")
	status, body := f.exchange(t, form, key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange asking for a JSON Web Token = %d %v, want 400 invalid_request", status, body)
	}
}

// TestASubjectTokenSignedWithASharedSecretIsRefused: the library parses a subject token allowing
// every algorithm, 'none' among them, and leaves the choice to the type's key function. An
// issuer publishes public keys, so a token signed with a secret is refused before any key is
// resolved.
func TestASubjectTokenSignedWithASharedSecretIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	claims := f.cluster.Claims(account, podAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "pod-uid-1"}}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: make([]byte, 32)},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange of a token signed with a shared secret = %d %v, want 400 invalid_request", status, body)
	}
}

// devBoxToken is a dev box's machine access token: a machine login of key approved for person,
// polled for its token pair.
func (f *exchangeFixture) devBoxToken(t *testing.T, key *ecdsa.PrivateKey, person string) string {
	t.Helper()
	return f.devBoxTokenOf(t, key, person, f.newCredential(t))
}

// devBoxTokenOf is devBoxToken for a login whose approval minted the launcher credential named.
func (f *exchangeFixture) devBoxTokenOf(t *testing.T, key *ecdsa.PrivateKey, person, credential string) string {
	t.Helper()
	deviceCode, userCode := f.authorize(t, key)
	if err := f.provider.ApproveDevice(context.Background(), userCode, true,
		map[string]any{"operator": person, "credential_id": credential}); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
	status, body := f.poll(t, deviceCode, key)
	return accessTokenOf(t, status, body)
}

// TestADevBoxSessionExchangesItsMachineToken: the machine access token's subject type never
// reaches the library's subject hook, because the library reads the subject off the stored
// session, so the exchange's own step runs after the request validates. The person is the
// approver the login recorded and the token names no project.
func TestADevBoxSessionExchangesItsMachineToken(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	machineToken := f.devBoxToken(t, key, person)
	exchanged := f.exchanged(t, exchangeForm(machineToken, "urn:ietf:params:oauth:token-type:access_token", "ada-devbox"), key)
	answer, err := f.provider.Introspect(context.Background(), exchanged)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if !answer.Active || answer.Session != "ada-devbox" || answer.Person != person {
		t.Fatalf("Introspect = %+v, want a live token of ada-devbox acting for %s", answer, person)
	}
	if answer.ProjectScope != oauth.ProjectScopePerson || answer.Project != "" {
		t.Errorf("Introspect = %+v, want every project the person may write and no project named", answer)
	}
	if answer.Thumbprint != thumbprint(t, key) {
		t.Errorf("Introspect answered thumbprint %q, want the machine's own key %q", answer.Thumbprint, thumbprint(t, key))
	}
}

// tokenAnswer is a token endpoint answer read off a goroutine, where a test may not fail.
type tokenAnswer struct {
	status int
	body   map[string]any
	err    error
}

// postToken posts form to the token endpoint at tokenURL with the DPoP proof already signed, for a
// goroutine to run: it fails nothing itself and answers what came back.
func postToken(tokenURL string, form url.Values, proof string) tokenAnswer {
	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenAnswer{err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tokenAnswer{err: err}
	}
	defer resp.Body.Close()
	answer := tokenAnswer{status: resp.StatusCode}
	answer.err = json.NewDecoder(resp.Body).Decode(&answer.body)
	return answer
}

// waitForBlocked returns once a backend running a statement matching like waits on a lock that
// blocker holds, failing after ten seconds. Matching on the blocker, not only the statement,
// keeps a test of another package sharing the database from answering for this one.
func waitForBlocked(t *testing.T, pool *pgxpool.Pool, blocker uint32, like string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where wait_event_type = 'Lock' and $1 = any(pg_blocking_pids(pid)) and query like $2`, int32(blocker), like).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no statement matching %s waited on a lock backend %d holds", like, blocker)
}

// waitForWaiting returns once backend pid waits on a lock in a statement matching like, failing
// at once if the operation running there returns first, and after ten seconds.
func waitForWaiting(t *testing.T, pool *pgxpool.Pool, pid uint32, like string, returned <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-returned:
			t.Fatalf("the operation returned (%v) before it waited on a lock in a statement matching %s", err, like)
		default:
		}
		var waiting int
		if err := pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where pid = $1 and wait_event_type = 'Lock' and query like $2`, int32(pid), like).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("backend %d never waited on a lock in a statement matching %s", pid, like)
}

// TestAMachineExchangeHoldsItsCredentialAgainstARevoke: an exchange of a machine access token
// holds its launcher credential's row from before its token is issued until it commits, so a
// revoke arriving in between waits for it and then ends the exchanged token with the rest. Without
// the lock the revoke would commit first, ending only the rows it could see, and the exchanged
// token stored after it would introspect live for its whole lifetime.
//
// The exchange is held between its lock and its commit by a share lock on oauth_access_tokens,
// which lets its plain read of the subject token through and stops its insert of the new one. The
// revoke is enroll.RevokeReusedCredential, the revoke that runs in a caller's transaction: it
// takes the credential's row first and ends its tokens as enroll.Service.RevokeCredential does.
func TestAMachineExchangeHoldsItsCredentialAgainstARevoke(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	key := newKey(t)
	credential := f.newCredential(t)
	machineToken := f.devBoxTokenOf(t, key, person, credential)

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table oauth_access_tokens in share mode`); err != nil {
		t.Fatalf("hold oauth_access_tokens: %v", err)
	}
	form := exchangeForm(machineToken, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox")
	proof := dpopProof(t, key, f.url+"/token")
	exchanged := make(chan tokenAnswer, 1)
	go func() { exchanged <- postToken(f.url+"/token", form, proof) }()
	waitForBlocked(t, f.pool, holder.Conn().PgConn().PID(), "insert into oauth_access_tokens%")

	pids, revoked := make(chan uint32, 1), make(chan error, 1)
	go func() {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			close(pids)
			revoked <- err
			return
		}
		defer tx.Rollback(ctx)
		pids <- tx.Conn().PgConn().PID()
		if err := enroll.RevokeReusedCredential(ctx, tx, credential); err != nil {
			revoked <- err
			return
		}
		revoked <- tx.Commit(ctx)
	}()
	pid, ok := <-pids
	if !ok {
		t.Fatalf("begin the revoke: %v", <-revoked)
	}
	waitForWaiting(t, f.pool, pid, "%from launcher_credentials%", revoked)

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release oauth_access_tokens: %v", err)
	}
	answer := <-exchanged
	if answer.err != nil {
		t.Fatalf("the exchange: %v", answer.err)
	}
	token := accessTokenOf(t, answer.status, answer.body)
	if err := <-revoked; err != nil {
		t.Fatalf("the revoke: %v", err)
	}

	if got, err := f.provider.Introspect(ctx, token); err != nil || got.Active {
		t.Fatalf("Introspect of the token exchanged as the machine was revoked = %+v, %v, want it not live", got, err)
	}
	var named, active int
	if err := f.pool.QueryRow(ctx, `select count(*), count(*) filter (where active) from oauth_access_tokens where credential_id = $1`, credential).Scan(&named, &active); err != nil {
		t.Fatal(err)
	}
	if named != 2 || active != 0 {
		t.Fatalf("access tokens naming the revoked credential = %d, %d of them active; want the machine's and the exchanged one, neither active", named, active)
	}
}

// TestAMachineExchangeOfAnEndedCredentialIsRefused: a machine access token whose row is still
// active, but whose launcher credential has lapsed, is refused invalid_grant by the exchange's
// credential check, and no token is stored for it.
func TestAMachineExchangeOfAnEndedCredentialIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	credential := f.newCredential(t)
	machineToken := f.devBoxTokenOf(t, key, person, credential)
	if _, err := f.pool.Exec(context.Background(), `update launcher_credentials set expires_at = now() - interval '1 minute' where id = $1`, credential); err != nil {
		t.Fatal(err)
	}
	before := f.count(t, "oauth_access_tokens")
	status, body := f.exchange(t, exchangeForm(machineToken, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("an exchange of a lapsed machine login's token = %d %v, want 400 invalid_grant", status, body)
	}
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Fatalf("access tokens after the refused exchange = %d, want the %d there were", after, before)
	}
}

// introspectsAs fails the test unless token is live and introspects under session, acting for
// person and writing project.
func (f *exchangeFixture) introspectsAs(t *testing.T, token, session, person, project string) {
	t.Helper()
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if !answer.Active || answer.Session != session || answer.Person != person || answer.Project != project {
		t.Fatalf("Introspect = %+v, want a live token of %s acting for %s in %s", answer, session, person, project)
	}
}

// TestEveryPodOfAFixedSessionAccountWritesUnderItsName: the session name an entry fixes belongs
// to the account, not to a pod of it, so the replacement pod of a rolling update is answered while
// the pod it replaces still holds a live token, and the old pod's renewals go on being answered
// beside it.
func TestEveryPodOfAFixedSessionAccountWritesUnderItsName(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	for _, uid := range []string{"pod-uid-old", "pod-uid-new", "pod-uid-old", "pod-uid-new"} {
		token := f.exchanged(t, exchangeForm(f.podTokenOf(t, daemonAccount, uid), oauthwire.SubjectTokenTypeKubernetes, ""), key)
		f.introspectsAs(t, token, daemonSession, person, project)
	}
}

// TestEveryRunOfAFixedSessionWorkflowWritesUnderItsName: the session name an entry fixes belongs
// to the workflow, not to a run of it, so runs that start within one access lifetime of each
// other, as two merges in a row do, are each answered under it.
func TestEveryRunOfAFixedSessionWorkflowWritesUnderItsName(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	deploy := func(claims map[string]any) {
		claims["workflow_ref"] = repository + "/" + deployWorkflow + "@" + ref
		claims["job_workflow_ref"] = claims["workflow_ref"]
	}
	for _, run := range []string{"4242", "4243", "4244"} {
		token := f.exchanged(t, exchangeForm(f.workflowToken(t, run, deploy), oauthwire.SubjectTokenTypeGitHub, deploySession), key)
		f.introspectsAs(t, token, deploySession, person, project)
	}
}

// TestAnotherPrincipalIsRefusedANameAfterItsHoldersTokenLapses: the review's reproduction. With
// an access lifetime of one second, a pod writes under a Legion role's session name and its token
// lapses; a pod of another account, listed for another project and another person, then names the
// same session and is refused, because the lapse of a token hands no name over. The first pod's
// next renewal is answered under its own person and project.
func TestAnotherPrincipalIsRefusedANameAfterItsHoldersTokenLapses(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Second })
	key := newKey(t)
	const role = "019a-role-session"
	first := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, role), key)
	time.Sleep(time.Second + 500*time.Millisecond)
	if answer, err := f.provider.Introspect(context.Background(), first); err != nil || answer.Active {
		t.Fatalf("Introspect of the holder's token past its lifetime = %+v, %v, want it lapsed", answer, err)
	}
	status, body := f.exchange(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-other"), oauthwire.SubjectTokenTypeKubernetes, role), key)
	refusedTheName(t, status, body, role)
	renewed := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, role), key)
	f.introspectsAs(t, renewed, role, person, project)
}

// TestANameAnEntryFixesIsRefusedToEveryOtherCaller: the names entries fix are held for their
// entries from the broker's first exchange, so a dev box — even one of the very person the
// daemon acts for — and a pod of another account are refused them before the entry's own caller
// has ever exchanged, and still once it has.
func TestANameAnEntryFixesIsRefusedToEveryOtherCaller(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	machine := f.devBoxToken(t, key, person)
	for _, session := range []string{daemonSession, deploySession} {
		status, body := f.exchange(t, exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, session), key)
		refusedTheName(t, status, body, session)
	}
	status, body := f.exchange(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-other"), oauthwire.SubjectTokenTypeKubernetes, daemonSession), key)
	refusedTheName(t, status, body, daemonSession)
	token := f.exchanged(t, exchangeForm(f.podTokenOf(t, daemonAccount, "pod-uid-daemon"), oauthwire.SubjectTokenTypeKubernetes, ""), key)
	f.introspectsAs(t, token, daemonSession, person, project)
	status, body = f.exchange(t, exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, daemonSession), key)
	refusedTheName(t, status, body, daemonSession)
}

// TestAPrincipalReclaimsItsOwnNameAsOftenAsItLikes: one account's pods, a pod relaunched under a
// new uid among them, renew one Legion session's name as often as they like, and so do one
// person's two machines; each claim runs SessionRetention past the expiry of the token it was
// made for, which is the issued token's own expiry rather than a moment computed before it.
func TestAPrincipalReclaimsItsOwnNameAsOftenAsItLikes(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	const role = "019a-role-session"
	var token string
	for _, uid := range []string{"pod-uid-1", "pod-uid-1", "pod-uid-relaunched", "pod-uid-1", "pod-uid-relaunched"} {
		token = f.exchanged(t, exchangeForm(f.podToken(t, uid, nil), oauthwire.SubjectTokenTypeKubernetes, role), key)
		f.introspectsAs(t, token, role, person, project)
	}
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	var claimed time.Time
	if err := f.pool.QueryRow(context.Background(), `select expires_at from oauth_session_bindings where session = $1`, role).Scan(&claimed); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	// The answer's exp is whole seconds, so the claim lies within a second of it either way.
	if issued := time.Unix(answer.ExpiresAt, 0).Add(oauthstore.SessionRetention); claimed.Before(issued.Add(-time.Second)) || claimed.After(issued.Add(time.Second)) {
		t.Errorf("the claim runs out at %s, want SessionRetention past the issued token's expiry, %s", claimed, issued)
	}
	for _, machineKey := range []*ecdsa.PrivateKey{newKey(t), newKey(t)} {
		machine := f.devBoxToken(t, machineKey, person)
		for range 2 {
			devBox := f.exchanged(t, exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), machineKey)
			f.introspectsAs(t, devBox, "ada-devbox", person, "")
		}
	}
}

// TestIntrospectingATokenTheBrokerCannotStandBehind: a token it never minted, one it has revoked
// and one whose life has passed are each answered as simply not live, with no error and nothing
// else disclosed. The expired case is why the access lifetime is a setting: a second proves it.
func TestIntrospectingATokenTheBrokerCannotStandBehind(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Second })
	ctx := context.Background()
	key := newKey(t)
	revoked := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if _, err := f.pool.Exec(ctx, `update oauth_access_tokens set active = false`); err != nil {
		t.Fatalf("revoke the token: %v", err)
	}
	expired := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	live, err := f.provider.Introspect(ctx, expired)
	if err != nil || !live.Active {
		t.Fatalf("Introspect of a fresh token = %+v, %v, want it live", live, err)
	}
	time.Sleep(time.Second + 100*time.Millisecond)
	for name, token := range map[string]string{
		"one the broker never minted": "a-token-nobody-issued",
		"a revoked one":               revoked,
		"an expired one":              expired,
	} {
		t.Run(name, func(t *testing.T) {
			answer, err := f.provider.Introspect(ctx, token)
			if err != nil {
				t.Fatalf("Introspect = %v, want no error", err)
			}
			if answer != (oauth.Introspection{}) {
				t.Fatalf("Introspect = %+v, want not live and nothing else", answer)
			}
		})
	}
}

// TestIntrospectionAnswersOnlyAnAccessToken: the introspection route is how Dispatch reads one
// of the broker's access tokens back, and its answer carries no token type, so a resource server
// cannot tell a refresh token from an access token by reading it. A machine login's refresh token
// outlives the access token issued beside it; answered live, it would pass for a credential after
// that access token has expired. It is answered as simply not live — both while the access token
// issued beside it is live and once that access token has expired.
func TestIntrospectionAnswersOnlyAnAccessToken(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Second })
	ctx := context.Background()
	key := newKey(t)
	deviceCode, userCode := f.authorize(t, key)
	if err := f.provider.ApproveDevice(ctx, userCode, true,
		map[string]any{"operator": person, "credential_id": f.newCredential(t)}); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
	status, body := f.poll(t, deviceCode, key)
	accessTokenOf(t, status, body)
	refreshToken, _ := body["refresh_token"].(string)
	if refreshToken == "" {
		t.Fatalf("the machine login's poll answered %v, which carries no refresh token", body)
	}
	check := func(t *testing.T) {
		t.Helper()
		answer, err := f.provider.Introspect(ctx, refreshToken)
		if err != nil {
			t.Fatalf("Introspect = %v, want no error", err)
		}
		if answer != (oauth.Introspection{}) {
			t.Fatalf("Introspect of a refresh token = %+v, want not live and nothing else", answer)
		}
	}
	t.Run("while the access token issued beside it is live", check)
	time.Sleep(time.Second + 100*time.Millisecond)
	t.Run("once that access token has expired", check)
}

// TestTheExchangeClientAuthenticatesWithNoCredentialAndOnlyThere: the broker answers the exchange
// client as authenticated when it presents nothing at the token-exchange grant, because the
// subject token is the credential and shipping a secret to every pod is what this removes. Every
// neighbouring case stays the library's own refusal.
func TestTheExchangeClientAuthenticatesWithNoCredentialAndOnlyThere(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	for _, c := range []struct {
		name   string
		form   func(*testing.T) url.Values
		status int
		code   string
	}{
		{
			name: "the exchange client presenting no credential is the one case",
			form: func(t *testing.T) url.Values {
				return exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
			},
			status: http.StatusOK,
		},
		{
			name: "another client id presenting no credential is refused",
			form: func(t *testing.T) url.Values {
				form := exchangeForm(f.podToken(t, "pod-uid-2", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g2")
				form.Set("client_id", "example-client")
				return form
			},
			status: http.StatusUnauthorized,
			code:   "invalid_client",
		},
		{
			name: "the machine login's client presenting no credential is refused",
			form: func(t *testing.T) url.Values {
				form := exchangeForm(f.podToken(t, "pod-uid-3", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g3")
				form.Set("client_id", oauthwire.ClientID)
				return form
			},
			status: http.StatusBadRequest,
			code:   "invalid_grant",
		},
		{
			name: "the exchange client presenting no credential on another grant is refused",
			form: func(*testing.T) url.Values {
				return url.Values{
					"grant_type":    {"refresh_token"},
					"client_id":     {oauthwire.ExchangeClientID},
					"refresh_token": {"a-token-nobody-issued"},
				}
			},
			status: http.StatusUnauthorized,
			code:   "invalid_client",
		},
		{
			name: "the exchange client presenting a wrong secret is refused",
			form: func(t *testing.T) url.Values {
				form := exchangeForm(f.podToken(t, "pod-uid-4", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g4")
				form.Set("client_secret", "not-the-secret")
				return form
			},
			status: http.StatusUnauthorized,
			code:   "invalid_client",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.exchange(t, c.form(t), key)
			if status != c.status {
				t.Fatalf("= %d %v, want %d", status, body, c.status)
			}
			if c.code != "" && body["error"] != c.code {
				t.Fatalf("= %v, want the error %s", body, c.code)
			}
		})
	}
}

// TestAnIssuerTheBrokerWasNotToldAboutRegistersNoType: a type whose issuer is not configured is
// not registered at all, so a caller presenting it is refused by the library before any handler
// of the broker's runs.
func TestAnIssuerTheBrokerWasNotToldAboutRegistersNoType(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.GitHub = oauth.IssuerConfig{} })
	key := newKey(t)
	status, body := f.exchange(t, exchangeForm(f.workflowToken(t, "4242", nil), oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange against an unconfigured issuer = %d %v, want 400 invalid_request", status, body)
	}
}

// TestAnExchangeWithNoListedIdentityIsRefusedByName: a broker whose deployment has listed no
// identity still composes the exchange, because the token-exchange grant is the exchange
// client's only one, and refuses every caller by name rather than faulting on a policy nobody
// built.
func TestAnExchangeWithNoListedIdentityIsRefusedByName(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Policy = nil })
	key := newKey(t)
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange against an empty policy = %d %v, want 400 invalid_request", status, body)
	}
	if hint, _ := body["error_description"].(string); !strings.Contains(hint, account) {
		t.Errorf("the refusal reads %q, which does not name the account no entry lists", hint)
	}
}
