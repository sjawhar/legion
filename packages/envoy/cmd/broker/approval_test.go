package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestTheBinaryApprovesOnlyWithTheApprovingPersonsOwnSignIn drives the REAL compiled broker with
// the BROKER_APPROVAL_* group set, against a local OIDC issuer standing in for the deployment's
// (Google's) and a real Postgres, over HTTP alone: it says at boot which way it takes an approval
// and the redirect URI the operator registers on its OAuth client; the listed caller — the
// identity Dispatch's own server presents — is refused outright when it tries to approve; and
// ada's machine login is minted only once ada's own ID token has come back to the broker's
// callback. A broker with that group unset is the other half of the rollout, which
// service_login_test.go drives.
func TestTheBinaryApprovesOnlyWithTheApprovingPersonsOwnSignIn(t *testing.T) {
	databaseURL := storetest.URL(t)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	const (
		podAudience     = "legion-broker-pod"
		dispatchAccount = "system:serviceaccount:example-ns:example-dispatch"
		clientID        = "broker-approval-client"
		clientSecret    = "broker-approval-secret"
		domain          = "example.com"
		approver        = "ada@example.com"
	)
	// The broker's own approval client, which nothing else holds: the ID token it reads is issued
	// for this client alone, to the browser the person signs in with.
	issuer.EnableCodeFlow(podKey, clientID, clientSecret)
	dispatchClaims := issuer.Claims(dispatchAccount, podAudience)
	dispatchClaims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	dispatchToken := issuer.Mint(t, podKey, dispatchClaims)
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(t.TempDir(), "approval-client-secret")
	if err := os.WriteFile(secretFile, []byte(clientSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + podAudience,
		"BROKER_API_ACCOUNTS=dispatch=" + dispatchAccount,
		"BROKER_APPROVAL_ISSUER=" + issuer.URL(),
		"BROKER_APPROVAL_CLIENT_ID=" + clientID,
		// The secret through its file, as a deployment mounts it rather than putting it in the
		// environment.
		"BROKER_APPROVAL_CLIENT_SECRET_FILE=" + secretFile,
		"BROKER_APPROVAL_DOMAIN=" + domain,
	}})
	base := "http://" + broker.Addr

	// What the broker says about itself: the group is on, and the redirect URI an operator
	// registers on the client is its own public URL's callback.
	boot := broker.Await(t, "approval sign-in")
	if !strings.Contains(boot, `state=on`) || !strings.Contains(boot, "redirect_uri="+base+"/approve/callback") {
		t.Fatalf("the boot log does not name the approval sign-in and its redirect URI:\n%s", boot)
	}
	if strings.Contains(boot, "every approval is taken on the calling service's word") {
		t.Fatalf("the broker warned that approvals are on the caller's word with its group set:\n%s", boot)
	}

	call := func(method, path string, headers map[string]string, body any) (int, []byte) {
		t.Helper()
		var payload []byte
		if body != nil {
			var err error
			if payload, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		request, err := http.NewRequest(method, base+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("> %s %s\n< %d %s", method, path, response.StatusCode, bytes.TrimSpace(answer))
		return response.StatusCode, answer
	}
	ui := func(method, path string, body any) (int, []byte) {
		t.Helper()
		return call(method, path, map[string]string{"Authorization": "Bearer " + dispatchToken}, body)
	}
	field := func(answer []byte, name string) string {
		t.Helper()
		var fields map[string]any
		if err := json.Unmarshal(answer, &fields); err != nil {
			t.Fatalf("decode %s: %v", answer, err)
		}
		value, _ := fields[name].(string)
		return value
	}

	t.Log("--- ada's machine asks for a login, and ada looks its code up")
	machineKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	code, recordID := startApprovalLogin(t, base, machineKey, approver, ui)

	t.Log("--- the listed caller cannot approve it at all")
	status, answer := ui(http.MethodPost, "/v1/credential-requests/"+recordID+"/approve",
		map[string]any{"approver": approver, "code": code})
	if status != http.StatusForbidden || field(answer, "code") != "APPROVAL_NEEDS_PERSON" {
		t.Fatalf("the listed caller's approve = %d %s, want 403 APPROVAL_NEEDS_PERSON", status, answer)
	}

	t.Log("--- it opens an approval instead, and ada signs in on the broker's own page")
	status, answer = ui(http.MethodPost, "/v1/credential-requests/"+recordID+"/approval",
		map[string]any{"approver": approver, "code": code})
	approvalURL := field(answer, "url")
	if status != http.StatusOK || !strings.HasPrefix(approvalURL, base+"/approve/") {
		t.Fatalf("open an approval = %d %s, want the broker's own page", status, answer)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	page, err := browser.Get(approvalURL)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := io.ReadAll(page.Body)
	page.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The page's form answers with a redirect to the issuer, which Chromium checks against
	// form-action: the policy names the issuer's authorization origin beside 'self'.
	if page.StatusCode != http.StatusOK || !strings.Contains(string(shown), approver) ||
		!strings.Contains(page.Header.Get("Content-Security-Policy"), "form-action 'self' "+issuer.URL()+";") ||
		page.Header.Get("X-Frame-Options") != "DENY" || page.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the approval page = %d %v: %s", page.StatusCode, page.Header, shown)
	}
	// The button on the broker's own page: a same-origin form post, which a browser marks with
	// the broker's own Origin. The same post with none is refused, which is the guard against
	// another site's form skipping the page.
	unmarked, err := browser.Post(approvalURL+"/start", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	unmarked.Body.Close()
	if unmarked.StatusCode != http.StatusForbidden {
		t.Fatalf("a start with no Origin = %d, want 403", unmarked.StatusCode)
	}
	startRequest, err := http.NewRequest(http.MethodPost, approvalURL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	startRequest.Header.Set("Origin", base)
	started, err := browser.Do(startRequest)
	if err != nil {
		t.Fatal(err)
	}
	started.Body.Close()
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("the sign-in's start = %d, want 303", started.StatusCode)
	}
	issuer.SignInAs(map[string]any{
		"sub": "google-subject-ada", "email": approver, "email_verified": true,
		"hd": domain, "auth_time": time.Now().Add(-time.Minute).Unix(),
	})
	signedIn, state := issuer.Authorize(t, started.Header.Get("Location"))
	done, err := browser.Get(base + "/approve/callback?code=" + url.QueryEscape(signedIn) + "&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	decided, err := io.ReadAll(done.Body)
	done.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if done.StatusCode != http.StatusOK || !strings.Contains(string(decided), "Approved") {
		t.Fatalf("the callback = %d: %s", done.StatusCode, decided)
	}

	t.Log("--- the credential is minted, for ada, by that sign-in alone")
	status, answer = ui(http.MethodGet, "/v1/credential-requests/"+recordID, nil)
	var read struct {
		State   string `json:"state"`
		Decided *struct {
			CredentialID *string `json:"credential_id"`
		} `json:"decided"`
	}
	if err := json.Unmarshal(answer, &read); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || read.State != "approved" || read.Decided == nil || read.Decided.CredentialID == nil {
		t.Fatalf("the machine login after the sign-in = %d %s, want approved with a credential", status, answer)
	}
	if !strings.Contains(broker.Log(), "approval decided with the approver's own sign-in") {
		t.Fatalf("the broker logged no line naming the sign-in that decided it:\n%s", broker.Log())
	}
}

// startApprovalLogin asks the running broker for a machine login of key on behalf of approver and
// looks its code up as the listed caller does, answering the code and the record it names.
func startApprovalLogin(t *testing.T, base string, key *ecdsa.PrivateKey, approver string,
	ui func(string, string, any) (int, []byte),
) (code, recordID string) {
	t.Helper()
	compact, err := record.Sign(key, base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: "example-host-devbox"},
	}, "", approver, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {oauthwire.Scope}, "dpop_jkt": {jkt},
		"request": {compact},
	}
	response, err := http.Post(base+"/v1/oauth/device_authorization", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var opened struct {
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(answer, &opened); response.StatusCode != http.StatusOK || err != nil || opened.UserCode == "" {
		t.Fatalf("POST /v1/oauth/device_authorization = %d %s (%v), want a user code", response.StatusCode, answer, err)
	}
	status, looked := ui(http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": opened.UserCode})
	var record struct {
		RecordID string `json:"record_id"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(looked, &record); status != http.StatusOK || err != nil || record.State != "pending" {
		t.Fatalf("look the login up = %d %s (%v), want a pending record", status, looked, err)
	}
	return opened.UserCode, record.RecordID
}
