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

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// withProvider gives svc an OAuth provider on its own database whose device codes last
// deviceLifetime.
func withProvider(t *testing.T, svc *Service, deviceLifetime time.Duration) {
	t.Helper()
	st := oauthstore.New(svc.Store.Pool)
	st.Reused = enroll.RevokeReusedCredential
	secret, err := st.Secret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	svc.OAuth, err = oauth.New(oauth.Config{
		Secret: secret, DeviceLifetime: deviceLifetime, RefreshLifetime: time.Hour, VerificationURL: testAudience, ProofSkew: time.Minute,
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
	form := url.Values{"client_id": {oauthwire.ClientID}, "scope": {"machine"}, "request": {compact}, "dpop_jkt": {jkt}}
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
	withProvider(t, svc, 10*time.Minute)
	ctx := context.Background()
	code := deviceLogin(t, svc)
	view, err := svc.LookupByCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	// The library reads a code's expiry from its stored session, so the codes expire there.
	if _, err := svc.Store.Pool.Exec(ctx, `update oauth_device_codes set expires_at = now() - interval '1 second',
		request = jsonb_set(jsonb_set(request, '{session,expires_at,device_code}', to_jsonb(now() - interval '1 second')),
			'{session,expires_at,user_code}', to_jsonb(now() - interval '1 second'))`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ApplyDecision(ctx, view.RecordID, true, testApprover, code); !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("ApplyDecision after the device code expired = %v, want ErrLoginExpired", err)
	}
	var n int
	if err := svc.Store.Pool.QueryRow(ctx, `select (select count(*) from launcher_credentials) + (select count(*) from credential_request_events)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("credentials and events after the refusal = %d (%v), want 0", n, err)
	}
}

// inTx runs f with a context carrying a fresh transaction on svc's database, as the provider's
// token hooks run, and commits it after.
func inTx(t *testing.T, svc *Service, f func(ctx context.Context)) {
	t.Helper()
	tx, err := svc.Store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	f(oauthstore.WithTx(context.Background(), tx))
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
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
	inTx(t, svc, func(ctx context.Context) {
		at, err := svc.Refreshed(ctx, credentialID)
		if err != nil || at.Before(before.Add(svc.CredentialLifetime-time.Minute)) || at.After(time.Now().Add(svc.CredentialLifetime+time.Minute)) {
			t.Fatalf("Refreshed = %s, %v; want about now + %s", at, err, svc.CredentialLifetime)
		}
	})
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() - interval '1 second' where id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	inTx(t, svc, func(ctx context.Context) {
		if _, err := svc.Refreshed(ctx, credentialID); !errors.Is(err, ErrNotLive) || !errors.Is(err, oauth.ErrLoginEnded) {
			t.Fatalf("Refreshed(expired) = %v, want ErrNotLive, a login-ended error", err)
		}
	})
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() + interval '1 hour', revoked_at = now() where id=$1`, credentialID); err != nil {
		t.Fatal(err)
	}
	inTx(t, svc, func(ctx context.Context) {
		if _, err := svc.Refreshed(ctx, credentialID); !errors.Is(err, ErrNotLive) {
			t.Fatalf("Refreshed(revoked) = %v, want ErrNotLive", err)
		}
	})
}

// TestRefreshedAndLiveRunOnlyInATransaction: their row lock is what serializes a refresh with a
// revoke, and on the pool it would end with the statement, so without a transaction they refuse.
func TestRefreshedAndLiveRunOnlyInATransaction(t *testing.T) {
	svc := newFixture(t)
	_, credentialID := approvedCredential(t, svc)
	if _, err := svc.Refreshed(context.Background(), credentialID); err == nil {
		t.Error("Refreshed outside a transaction = nil, want an error")
	}
	if err := svc.Live(context.Background(), credentialID); err == nil {
		t.Error("Live outside a transaction = nil, want an error")
	}
}

// TestTheTokenHooksRefuseOnlyAnEndedLogin: a session naming no credential, and a credential that
// ended, are login-ended refusals; a grant the hooks do not know is an error that is not.
func TestTheTokenHooksRefuseOnlyAnEndedLogin(t *testing.T) {
	svc := newFixture(t)
	_, credentialID := approvedCredential(t, svc)
	hooks := svc.TokenHooks()
	inTx(t, svc, func(ctx context.Context) {
		for _, grant := range []oauth2.GrantType{oauth2.GrantTypeDeviceCode, oauth2.GrantTypeRefreshToken} {
			if err := hooks.Before(ctx, grant, map[string]any{oauthwire.ExtraCredentialID: credentialID}); err != nil {
				t.Errorf("Before(%s) = %v, want nil", grant, err)
			}
		}
		if err := hooks.Before(ctx, oauth2.GrantTypeRefreshToken, map[string]any{}); !errors.Is(err, oauth.ErrLoginEnded) {
			t.Errorf("Before with no credential = %v, want a login-ended error", err)
		}
		err := hooks.Before(ctx, oauth2.GrantTypeClientCredentials, map[string]any{oauthwire.ExtraCredentialID: credentialID})
		if err == nil || errors.Is(err, oauth.ErrLoginEnded) {
			t.Errorf("Before(client_credentials) = %v, want an error that is not a login-ended one", err)
		}
	})
}
