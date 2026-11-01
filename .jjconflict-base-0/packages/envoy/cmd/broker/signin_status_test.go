package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// signInStatus is GET /signin/status's answer, as a production check reads it.
type signInStatus struct {
	ClientCheck            *string    `json:"client_check"`
	ClientCheckedAt        *time.Time `json:"client_checked_at"`
	LastSignInAt           *time.Time `json:"last_sign_in_at"`
	LastFailedSignInAt     *time.Time `json:"last_failed_sign_in_at"`
	LastFailedSignInReason *string    `json:"last_failed_sign_in_reason"`
	LastDecisionAt         *time.Time `json:"last_decision_at"`
}

// fetchSignInStatus fetches GET /signin/status from base with no credential at all, and answers
// it as parsed and as read.
func fetchSignInStatus(t *testing.T, base string) (signInStatus, string) {
	t.Helper()
	res, err := http.Get(base + "/signin/status")
	if err != nil {
		t.Fatalf("GET /signin/status: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /signin/status = %d:\n%s", res.StatusCode, body)
	}
	var status signInStatus
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("GET /signin/status answered %q: %v", body, err)
	}
	return status, strings.TrimSpace(string(body))
}

// awaitClientCheck waits for the boot pass of a broker's client check to reach the row, and
// answers the status it then reads.
func awaitClientCheck(t *testing.T, base string) (signInStatus, string) {
	t.Helper()
	for deadline := time.Now().Add(awaitTimeout); ; time.Sleep(listeningPoll) {
		status, body := fetchSignInStatus(t, base)
		if status.ClientCheck != nil {
			return status, body
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker took no client check within %s: %s", awaitTimeout, body)
		}
	}
}

// TestClientCheckWritesWhatTheProviderAnsweredAboutTheClient drives one pass of the check
// directly, rather than waiting an hour for the loop's second pass, against a provider that
// serves the broker's client, one that refuses its secret, and one that cannot be reached at
// all. Each pass writes its own word and the time it was taken, and nothing else of the status.
func TestClientCheckWritesWhatTheProviderAnsweredAboutTheClient(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	issuer := oidctest.New(t)
	signIn := brokertest.SignIn(issuer, issuer.PublishKey(t, "signin-key"))

	// An address nothing listens on: the server is closed before anything dials it.
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	goneURL := gone.URL
	gone.Close()

	rejected, unreachable := signIn, signIn
	rejected.ClientSecret = "not-the-client-secret"
	unreachable.Issuer = goneURL

	for _, c := range []struct {
		name   string
		signIn config.SignIn
		want   oidc.ClientCheck
	}{
		{"the provider serving the broker's client", signIn, oidc.ClientCheckReady},
		{"the provider refusing the client's secret", rejected, oidc.ClientCheckRejected},
		{"a provider nothing answers for", unreachable, oidc.ClientCheckIssuerUnreachable},
		{"the provider again, once it is serving", signIn, oidc.ClientCheckReady},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := time.Now().Add(-time.Second)
			checkSignInClient(ctx, c.signIn, st)
			status, err := st.SignInStatus(ctx)
			if err != nil {
				t.Fatalf("SignInStatus: %v", err)
			}
			switch {
			case status.ClientCheckState == nil || *status.ClientCheckState != string(c.want):
				t.Fatalf("the client check wrote %v, want %q", status.ClientCheckState, c.want)
			case status.ClientCheckedAt == nil || status.ClientCheckedAt.Before(before):
				t.Fatalf("the client check wrote the time %v, want the time of this pass", status.ClientCheckedAt)
			case status.LastSignInAt != nil || status.LastFailedSignInAt != nil || status.LastDecisionAt != nil:
				t.Fatalf("the client check wrote a sign-in's or a decision's fact too: %+v", status)
			}
		})
	}
}

// TestTheBinaryAnswersSignInStatusFromPostgres drives the REAL compiled broker: its boot client
// check reaches the public route with no credential, a person's sign-in and decision are written
// there too, and a second broker process on the same database - and a third started after both
// are gone - answers the same facts, which is what keeping them in Postgres is for. Nothing in
// the answer names the person, the record or the secret.
func TestTheBinaryAnswersSignInStatusFromPostgres(t *testing.T) {
	f := newApprovalFixture(t)
	databaseURL := storetest.URL(t)
	public := newFront(t)
	first := f.start(t, databaseURL, public.URL, f.signInEnv()...)
	public.route(t, first)
	base := public.URL

	status, body := awaitClientCheck(t, base)
	t.Logf("the first broker's boot client check: %s", body)
	if *status.ClientCheck != string(oidc.ClientCheckReady) {
		t.Fatalf("the boot client check against the fixture's provider = %s, want ready", body)
	}
	if status.LastSignInAt != nil || status.LastFailedSignInAt != nil || status.LastDecisionAt != nil {
		t.Fatalf("a broker nobody has signed in to answers %s, want only the client check", body)
	}

	pods := f.pods(t, base)
	pod := pods.Enroll(pods.LogIn(approvalLauncherService, approvalLauncherAccount), "pod-status", approvalPodAccount)
	reply := pods.Ask(pod, approvalSecretName)
	if reply.State != "pending" || reply.RecordID == nil {
		t.Fatalf("the pod's request = %+v, want pending on a record", reply)
	}
	record := *reply.RecordID

	b := brokertest.NewBrowser(t, base, http.DefaultTransport, f.issuer)
	b.SignIn(t, record, brokertest.Person(ada))
	b.Decide(t, record, true)
	status, body = fetchSignInStatus(t, base)
	t.Logf("after ada signed in and approved: %s", body)
	if status.LastSignInAt == nil || status.LastDecisionAt == nil || status.LastDecisionAt.Before(*status.LastSignInAt) {
		t.Fatalf("after ada's sign-in and decision the status = %s, want both times, the decision last", body)
	}
	for _, forbidden := range []string{ada, record, approvalSecretName, approvalSecretValue, brokertest.SignInClientSecret} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the public status carries %q: %s", forbidden, body)
		}
	}
	signedIn, decided := *status.LastSignInAt, *status.LastDecisionAt

	// A second process on the same database, with nobody signed in to it: it answers the facts
	// the first one wrote, since neither pod holds them.
	second := f.start(t, databaseURL, public.URL, f.signInEnv()...)
	public.route(t, second)
	status, body = fetchSignInStatus(t, base)
	t.Logf("the second broker process answers: %s", body)
	if !status.LastSignInAt.Equal(signedIn) || !status.LastDecisionAt.Equal(decided) {
		t.Fatalf("the second process answers %s, want ada's sign-in at %s and her decision at %s", body, signedIn, decided)
	}

	// And after a restart: both processes gone, a third started on the same database.
	for _, process := range []*brokerProcess{first, second} {
		process.Terminate(t)
		if err := process.AwaitExit(t, stopTimeout); err != nil {
			t.Fatalf("a broker exited with %v, want 0:\n%s", err, process.Log())
		}
	}
	third := f.start(t, databaseURL, public.URL, f.signInEnv()...)
	public.route(t, third)
	status, body = fetchSignInStatus(t, base)
	t.Logf("a broker started after both of them answers: %s", body)
	switch {
	case !status.LastSignInAt.Equal(signedIn) || !status.LastDecisionAt.Equal(decided):
		t.Fatalf("after the restart the status = %s, want ada's sign-in at %s and her decision at %s", body, signedIn, decided)
	case status.ClientCheck == nil || *status.ClientCheck != string(oidc.ClientCheckReady):
		t.Fatalf("after the restart the client check = %s, want ready", body)
	}
}

// TestTheBinaryEndSessionsEndsALiveSession runs the REAL compiled broker's subcommand against the real
// Postgres: ada's live session, made by signing in to a broker's own page, no longer signs her
// in once `broker end-sessions ada@example.com` has printed that it ended it, while carol's
// session stands. The subcommand binds nothing, and refuses an argument it does not know.
func TestTheBinaryEndSessionsEndsALiveSession(t *testing.T) {
	f := newApprovalFixture(t)
	databaseURL := storetest.URL(t)
	public := newFront(t)
	broker := f.start(t, databaseURL, public.URL, f.signInEnv()...)
	public.route(t, broker)
	base := public.URL

	pods := f.pods(t, base)
	pod := pods.Enroll(pods.LogIn(approvalLauncherService, approvalLauncherAccount), "pod-end-sessions", approvalPodAccount)
	reply := pods.Ask(pod, approvalSecretName)
	if reply.State != "pending" || reply.RecordID == nil {
		t.Fatalf("the pod's request = %+v, want pending on a record", reply)
	}
	record := *reply.RecordID

	adaBrowser := brokertest.NewBrowser(t, base, http.DefaultTransport, f.issuer)
	adaBrowser.SignIn(t, record, brokertest.Person(ada))
	// The form ada's page offered while her session was live, and the cookie that signed it: A2
	// asks what they decide once the operator has ended her sessions, which is nothing.
	form := brokertest.DecisionForm(t, adaBrowser.Page(t, record).Body, record)
	if form == nil {
		t.Fatalf("ada's page for %s offers no decision", record)
	}
	adaCookie := &http.Cookie{Name: sessionCookieName, Value: adaBrowser.Cookie(t, sessionCookieName)}
	carolBrowser := brokertest.NewBrowser(t, base, http.DefaultTransport, f.issuer)
	carolBrowser.SignIn(t, record, brokertest.Person(carol))
	carolBrowser.Page(t, record)

	out := runBrokerCommand(t, f.binary, databaseURL, public.URL, 0, endSessionsCommand, ada)
	t.Logf("broker %s %s printed: %s", endSessionsCommand, ada, strings.TrimSpace(out))
	if want := endSessionsCommand + ": ended 1 session of " + ada + "\n"; out != want {
		t.Fatalf("broker %s %s printed %q, want %q", endSessionsCommand, ada, out, want)
	}
	if strings.Contains(out, "broker listening") {
		t.Fatalf("the subcommand bound a listener: %s", out)
	}

	// Ada's cookie signs nobody in now and decides nothing; carol's still signs her in.
	requireSignInRedirect(t, f.issuer, adaBrowser.Get(t, "/approve/"+record), "ada's page after her sessions were ended")
	replay := brokertest.NewBrowser(t, base, http.DefaultTransport, f.issuer)
	res := replay.Post(t, "/approve", brokertest.Decision(form, "approve"), map[string]string{"Origin": base}, adaCookie)
	if res.Status != http.StatusOK || !strings.Contains(brokertest.PageText(t, res.Body), "Not signed in") {
		t.Fatalf("Approve with the ended session's cookie = %d, want 200 not signed in:\n%s", res.Status, res.Body)
	}
	requirePending(pods, pod, reply.RequestID, "the ended session's POST")
	carolBrowser.Page(t, record)

	// A second run finds nothing left to end, and a login with no session is no failure.
	if out := runBrokerCommand(t, f.binary, databaseURL, public.URL, 0, endSessionsCommand, ada); !strings.Contains(out, "ended 0 sessions of "+ada) {
		t.Fatalf("a second run printed %q, want 0 sessions", out)
	}

	for _, c := range []struct {
		name string
		args []string
	}{
		{"no subcommand of that name", []string{"end-session"}},
		{"a flag", []string{"-login=" + ada}},
		{"no login", []string{endSessionsCommand}},
		{"a blank login", []string{endSessionsCommand, "  "}},
		{"a second argument", []string{endSessionsCommand, ada, carol}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := runBrokerCommand(t, f.binary, databaseURL, public.URL, 2, c.args...)
			t.Logf("broker %s = exit 2: %s", strings.Join(c.args, " "), strings.TrimSpace(out))
			if !strings.Contains(out, "broker: ") {
				t.Fatalf("broker %v printed no usage line: %q", c.args, out)
			}
		})
	}
}

// TestEndSessionsDoesNotMigrateTheDatabase pins the half of the subcommand that matters when an
// image is older than the deployment asking it for one: it works on the schema the servers
// applied and brings nothing up to date itself, so an unmigrated database is an error rather
// than a migration nobody asked for.
func TestEndSessionsDoesNotMigrateTheDatabase(t *testing.T) {
	databaseURL := storetest.DatabaseURL(t)
	out := runBrokerCommand(t, buildBroker(t), databaseURL, "http://127.0.0.1:9", 1, endSessionsCommand, ada)
	t.Logf("broker %s against an unmigrated database = exit 1: %s", endSessionsCommand, strings.TrimSpace(out))
	if !strings.Contains(out, "browser_sessions") {
		t.Fatalf("the refusal does not name the missing table: %q", out)
	}
	st, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var tables int
	if err := st.Pool.QueryRow(context.Background(),
		`select count(*) from information_schema.tables where table_schema = 'public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("the subcommand left %d tables behind, want a database it never migrated", tables)
	}
}

// runBrokerCommand runs the broker binary with args and the configuration a broker pod has,
// requires the exit code wantExit, and answers everything it printed.
func runBrokerCommand(t *testing.T, binary, databaseURL, publicURL string, wantExit int, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(append(os.Environ(), productionEnv(t)...),
		"BROKER_DATABASE_URL="+databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL="+publicURL,
	)
	out, err := cmd.CombinedOutput()
	exit := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("broker %v: %v\n%s", args, err, out)
	}
	if exit != wantExit {
		t.Fatalf("broker %v exited %d, want %d:\n%s", args, exit, wantExit, out)
	}
	return string(out)
}
