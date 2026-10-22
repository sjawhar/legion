package machine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// withProvider gives svc an OAuth provider on its own database whose device codes last
// deviceLifetime.
func withProvider(t *testing.T, svc *Service, deviceLifetime time.Duration) {
	t.Helper()
	st := oauthstore.New(svc.Store.Pool)
	secret, err := st.Secret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	svc.OAuth, err = oauth.New(oauth.Config{
		Secret: secret, DeviceLifetime: deviceLifetime, RefreshLifetime: time.Hour, VerificationURL: testAudience,
	}, st)
	if err != nil {
		t.Fatal(err)
	}
}

// deviceLogin opens a device login for a fresh machine key through svc.OAuth with LoginDevice as
// its onRequest, answering the user code.
func deviceLogin(t *testing.T, svc *Service) string {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	compact, err := record.Sign(key, testAudience, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: "example-host-devbox"},
	}, "", testApprover, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"client_id": {oauthstore.ClientID}, "scope": {"machine"}, "request": {compact}, "dpop_jkt": {jkt}}
	r := httptest.NewRequest(http.MethodPost, "/device_authorization", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	svc.OAuth.DeviceAuthorize(w, r, svc.LoginDevice)
	var body struct {
		UserCode string `json:"user_code"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.UserCode == "" {
		t.Fatalf("device authorization = %d %s", w.Code, w.Body)
	}
	return body.UserCode
}

func TestADeviceLoginsRecordIsLoginsWithItsUserCodeAndNoPollRow(t *testing.T) {
	svc := newFixture(t)
	withProvider(t, svc, 10*time.Minute)
	ctx := context.Background()
	code := deviceLogin(t, svc)
	view, err := svc.LookupByCode(ctx, code)
	if err != nil || view.Approver != testApprover || view.Host != "example-host-devbox" || view.State != "pending" {
		t.Fatalf("LookupByCode(%s) = %+v, %v; want a pending login for example-host-devbox", code, view, err)
	}
	var polls int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from machine_login_polls`).Scan(&polls); err != nil || polls != 0 {
		t.Fatalf("machine_login_polls = %d (%v), want 0", polls, err)
	}
}

func TestLoginDeviceRunsInTheProvidersTransaction(t *testing.T) {
	svc := newFixture(t)
	if err := svc.LoginDevice(context.Background(), url.Values{}, "ABCD-EFGH"); err == nil {
		t.Fatal("LoginDevice outside a transaction = nil, want an error")
	}
}

// TestALoginWithNoDeviceCodeIsDecidedAsBefore pins that a record POST /v1/launcher-credentials
// opened, which has no device code, is decided as it was with a provider wired.
func TestALoginWithNoDeviceCodeIsDecidedAsBefore(t *testing.T) {
	svc := newFixture(t)
	withProvider(t, svc, 10*time.Minute)
	if _, credentialID := approvedCredential(t, svc); credentialID == "" {
		t.Fatal("approval minted no credential")
	}
}

func TestADeviceCodePastItsLifetimeMintsNothing(t *testing.T) {
	svc := newFixture(t)
	withProvider(t, svc, time.Second)
	ctx := context.Background()
	code := deviceLogin(t, svc)
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, _, err := svc.ApplyDecision(ctx, view.RecordID, true, testApprover, code); !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("ApplyDecision after the device code expired = %v, want ErrLoginExpired", err)
	}
	var n int
	if err := svc.Store.Pool.QueryRow(ctx, `select (select count(*) from launcher_credentials) + (select count(*) from credential_request_events)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("credentials and events after the refusal = %d (%v), want 0", n, err)
	}
}

func TestRefreshedMovesALiveCredentialAndRefusesAnEndedOne(t *testing.T) {
	svc := newFixture(t)
	ctx := context.Background()
	_, credentialID := approvedCredential(t, svc)
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() + interval '1 hour' where id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	at, err := svc.Refreshed(ctx, credentialID)
	if err != nil || at.Before(before.Add(svc.CredentialLifetime-time.Minute)) || at.After(time.Now().Add(svc.CredentialLifetime+time.Minute)) {
		t.Fatalf("Refreshed = %s, %v; want about now + %s", at, err, svc.CredentialLifetime)
	}
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() - interval '1 second' where id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refreshed(ctx, credentialID); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Refreshed(expired) = %v, want ErrNotLive", err)
	}
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() + interval '1 hour', revoked_at = now() where id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refreshed(ctx, credentialID); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Refreshed(revoked) = %v, want ErrNotLive", err)
	}
}
