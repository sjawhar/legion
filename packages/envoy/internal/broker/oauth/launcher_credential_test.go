package oauth_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// machineForm is a machine's own trade of its Google identity token for a Dispatch access token:
// what `agent-secrets`, the helper and every TypeScript client on a Compute Engine box send, bar
// the one parameter each test below sets for itself.
func machineForm(t *testing.T, f *signInFixture, session string) url.Values {
	t.Helper()
	return exchangeForm(f.machineToken(t, signInMachine, machineAudience, true), oauthwire.SubjectTokenTypeGoogle, session)
}

// TestATradeThatDeclinesItsLauncherCredentialLeavesNoRow is the TypeScript clients' shape: a
// caller that wants a Dispatch access token and nothing else sends launcher_credential=false and
// is answered one, with no launcher credential minted for it and nothing in the answer naming
// one. Each of those clients makes a key per process and per session, so a trade that asked
// would leave a row on the operator's machine-logins list for every `dispatch` call, every hook
// run and every session of a long-lived process, and nothing removes those rows.
func TestATradeThatDeclinesItsLauncherCredentialLeavesNoRow(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	before := f.count(t, "launcher_credentials")

	form := machineForm(t, f, "ada-box-session")
	form.Set(oauthwire.FormLauncherCredential, oauthwire.FormLauncherCredentialDeclined)
	status, body := f.exchange(t, form, key)
	token := accessTokenOf(t, status, body)

	if after := f.count(t, "launcher_credentials"); after != before {
		t.Errorf("the trade left %d launcher credentials, want the %d it started with: a trade that declines mints none", after, before)
	}
	for _, named := range []string{oauthwire.ExtraCredentialID, oauthwire.ExtraCredentialExpiresAt} {
		if value, carried := body[named]; carried {
			t.Errorf("the answer carries %s %v, and a trade that declined the credential has none to name", named, value)
		}
	}
	// The access token itself is whole: declining the credential costs the caller nothing it
	// asked for.
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if !answer.Active || answer.Session != "ada-box-session" || answer.Service != signInFleet {
		t.Errorf("the token introspects as active=%t session=%q service=%q, want a live token of this machine's session and fleet",
			answer.Active, answer.Session, answer.Service)
	}
}

// TestTheHelpersTradeStillGetsItsLauncherCredential is the other half of the same rule: a trade
// that sends no such parameter is asking, which is what `agent-secrets`, `agent-secrets-helper`
// and every client written before the parameter send, and it is answered the credential its
// sessions enrol under. A second trade of the same key moves that one credential rather than
// minting another, as it always has.
func TestTheHelpersTradeStillGetsItsLauncherCredential(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	before := f.count(t, "launcher_credentials")

	status, body := f.exchange(t, machineForm(t, f, "box-session-1"), key)
	accessTokenOf(t, status, body)
	credential, _ := body[oauthwire.ExtraCredentialID].(string)
	if credential == "" {
		t.Fatalf("the answer %v names no %s, and a machine whose token committed without one could enroll nothing", body, oauthwire.ExtraCredentialID)
	}
	if expires, _ := body[oauthwire.ExtraCredentialExpiresAt].(string); expires == "" {
		t.Errorf("the answer %v names no %s", body, oauthwire.ExtraCredentialExpiresAt)
	}
	if after := f.count(t, "launcher_credentials"); after != before+1 {
		t.Fatalf("the trade left %d launcher credentials, want %d: a trade that asks mints one", after, before+1)
	}

	// Asking in so many words is the same request, and the machine's one credential is moved
	// rather than added to.
	asking := machineForm(t, f, "box-session-1")
	asking.Set(oauthwire.FormLauncherCredential, "true")
	status, body = f.exchange(t, asking, key)
	accessTokenOf(t, status, body)
	if again, _ := body[oauthwire.ExtraCredentialID].(string); again != credential {
		t.Errorf("the second trade names credential %q, want the %q the first minted", again, credential)
	}
	if after := f.count(t, "launcher_credentials"); after != before+1 {
		t.Errorf("two trades of one key left %d launcher credentials, want %d: the machine holds one", after, before+1)
	}
}

// TestASignInThatDeclinesItsCredentialIsRefused: a sign-in holds its credential, so a person's
// trade is the one trade that may not decline it. `agent-secrets login`, its only caller, reads
// the credential id and refuses an answer without one, and the sessions that sign-in enrols read
// the person's own secrets under it; a sign-in holding none would do nothing. Nothing is
// recorded for the refused trade, so the box signs in again rather than carrying a half-made
// sign-in.
func TestASignInThatDeclinesItsCredentialIsRefused(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	before := f.count(t, "launcher_credentials")

	form := signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1")
	form.Set(oauthwire.FormLauncherCredential, oauthwire.FormLauncherCredentialDeclined)
	status, body := f.exchange(t, form, key)

	if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil {
		t.Fatalf("the sign-in = %d %v, want 400 invalid_request and no access token", status, body)
	}
	if description, _ := body["error_description"].(string); !strings.Contains(description, "A sign-in holds its credential") {
		t.Errorf("the refusal reads %q, which does not say that a sign-in holds its credential", description)
	}
	if after := f.count(t, "launcher_credentials"); after != before {
		t.Errorf("the refused sign-in left %d launcher credentials, want the %d it started with", after, before)
	}
	if got := f.signIns(); len(got) != 0 {
		t.Errorf("the exchange recorded %+v, and a refused trade records no sign-in", got)
	}
}

// TestRevokingASignInEndsTheTokenItsTradeMinted is the invariant the refusal above keeps whole:
// no token of a person's outlives the sign-in it was minted under. The revoke ends every live
// access token whose stored session names that person and that box key
// (store.RevokePersonSignInTx) — the sign-in row here is the real table's, since this suite's
// own recorder stands in for it only while the trade runs — and the credential the sign-in
// holds ends with it (enroll.Service.RevokePersonSignIn, whose half this test does not run). That
// second half is what ends a token from a trade that commits while the revoke's statement waits,
// which the first half's statement does not see; it reaches only a token that names a credential.
func TestRevokingASignInEndsTheTokenItsTradeMinted(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	ctx := context.Background()

	form := signInForm(
		f.personToken(t, person, deviceAudience, true, signInDomain),
		f.machineToken(t, signInMachine, machineAudience, true),
		"ada-box-1")
	token := f.exchanged(t, form, key)
	if answer, err := f.provider.Introspect(ctx, token); err != nil || !answer.Active {
		t.Fatalf("Introspect before the revoke = %+v, %v; want a live token", answer, err)
	}

	signIn := recordSignIn(t, f, person, thumbprint(t, key), signInMachine)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	revoked, ended, err := store.RevokePersonSignInTx(ctx, tx, signIn, person)
	if err != nil {
		t.Fatalf("RevokePersonSignInTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if revoked != thumbprint(t, key) || ended != 1 {
		t.Errorf("the revoke ended %d token(s) of key %q, want the one token this box key traded for", ended, revoked)
	}

	answer, err := f.provider.Introspect(ctx, token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if answer.Active {
		t.Errorf("the token introspects active after the sign-in was revoked, and no token outlives the sign-in it was minted under")
	}
}

// recordSignIn writes the person_signins row the trade's own recorder stood in for and answers
// its id, which the revoke route takes.
func recordSignIn(t *testing.T, f *signInFixture, person, thumbprint, machine string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := store.RecordPersonSignIn(ctx, tx, person, thumbprint, machine); err != nil {
		t.Fatalf("RecordPersonSignIn: %v", err)
	}
	var id string
	if err := tx.QueryRow(ctx, `select id::text from person_signins where person = lower($1) and thumbprint = $2`,
		person, thumbprint).Scan(&id); err != nil {
		t.Fatalf("read the sign-in's id: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

// TestALauncherCredentialValueTheBrokerCannotReadIsRefused: the parameter takes one of two
// spellings and nothing else. A caller that misspelled "false" and was read as asking would go
// on filling an operator's machine-logins list, and one misspelled the other way would lose the
// credential its secrets depend on with nothing naming the cause, so neither is guessed.
func TestALauncherCredentialValueTheBrokerCannotReadIsRefused(t *testing.T) {
	f := newSignInFixture(t)
	key := newKey(t)
	before := f.count(t, "launcher_credentials")

	form := machineForm(t, f, "box-session-1")
	form.Set(oauthwire.FormLauncherCredential, "no")
	status, body := f.exchange(t, form, key)

	if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil {
		t.Fatalf("the trade = %d %v, want 400 invalid_request and no access token", status, body)
	}
	if description, _ := body["error_description"].(string); !strings.Contains(description, oauthwire.FormLauncherCredential) {
		t.Errorf("the refusal reads %q, which does not name the parameter it refuses", description)
	}
	if after := f.count(t, "launcher_credentials"); after != before {
		t.Errorf("the refused trade left %d launcher credentials, want the %d it started with", after, before)
	}
}
