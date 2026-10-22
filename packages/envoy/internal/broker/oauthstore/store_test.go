package oauthstore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

func newStore(t *testing.T) (*oauthstore.Store, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), storetest.URL(t))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return oauthstore.New(pool), pool
}

// tokenRequest is a token session for requestID carrying credentialID ("" for none) in its
// session's extra claims, its tokens expiring in an hour.
func tokenRequest(t *testing.T, s *oauthstore.Store, requestID, credentialID string) *oauth2.Request {
	t.Helper()
	client, err := s.GetClient(context.Background(), oauthstore.ClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	session := &oauth2.DefaultSession{Extra: map[string]any{}}
	if credentialID != "" {
		session.Extra["credential_id"] = credentialID
	}
	session.SetExpiresAt(oauth2.AccessToken, time.Now().Add(time.Hour))
	session.SetExpiresAt(oauth2.RefreshToken, time.Now().Add(time.Hour))
	request := oauth2.NewRequest()
	request.SetID(requestID)
	request.Client = client
	request.GrantScope("machine")
	request.SetSession(session)
	return request
}

func TestDeviceCodeRoundTripsAndIsFoundByUserCode(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	client, err := s.GetClient(ctx, oauthstore.ClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	session := &oauth2.DefaultSession{Extra: map[string]any{"record_id": "record-1"}}
	session.SetExpiresAt(oauth2.DeviceCode, time.Now().Add(10*time.Minute))
	request := oauth2.NewDeviceAuthorizeRequest()
	request.Client = client
	request.Form.Set("request", "signed-request-object")
	request.SetSession(session)
	request.SetDeviceCodeSignature("device-signature")
	request.SetUserCodeSignature("user-signature")
	request.SetStatus(oauth2.DeviceAuthorizeStatusApproved)
	if err := s.CreateDeviceCodeSession(ctx, "device-signature", request); err != nil {
		t.Fatalf("CreateDeviceCodeSession: %v", err)
	}

	for name, get := range map[string]func() (oauth2.DeviceAuthorizeRequester, error){
		"by device code": func() (oauth2.DeviceAuthorizeRequester, error) {
			return s.GetDeviceCodeSession(ctx, "device-signature", nil)
		},
		"by user code": func() (oauth2.DeviceAuthorizeRequester, error) {
			return s.GetDeviceCodeSessionByUserCode(ctx, "user-signature", nil)
		},
	} {
		got, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.GetStatus() != oauth2.DeviceAuthorizeStatusApproved {
			t.Errorf("%s: status = %v, want approved", name, got.GetStatus())
		}
		if form := got.GetRequestForm().Get("request"); form != "signed-request-object" {
			t.Errorf("%s: form request = %q", name, form)
		}
		if got.GetID() != request.GetID() || got.GetClient().GetID() != oauthstore.ClientID {
			t.Errorf("%s: id %q client %q, want %q %q", name, got.GetID(), got.GetClient().GetID(), request.GetID(), oauthstore.ClientID)
		}
		if got.GetDeviceCodeSignature() != "device-signature" || got.GetUserCodeSignature() != "user-signature" {
			t.Errorf("%s: signatures %q %q", name, got.GetDeviceCodeSignature(), got.GetUserCodeSignature())
		}
		extra, ok := got.GetSession().(*oauth2.DefaultSession)
		if !ok || extra.Extra["record_id"] != "record-1" {
			t.Errorf("%s: session = %#v, want a DefaultSession with record_id", name, got.GetSession())
		}
	}

	if err := s.InvalidateDeviceCodeSession(ctx, "device-signature"); err != nil {
		t.Fatalf("InvalidateDeviceCodeSession: %v", err)
	}
	for name, get := range map[string]func() (oauth2.DeviceAuthorizeRequester, error){
		"by device code": func() (oauth2.DeviceAuthorizeRequester, error) {
			return s.GetDeviceCodeSession(ctx, "device-signature", nil)
		},
		"by user code": func() (oauth2.DeviceAuthorizeRequester, error) {
			return s.GetDeviceCodeSessionByUserCode(ctx, "user-signature", nil)
		},
	} {
		if got, err := get(); !errors.Is(err, oauth2.ErrNotFound) || got != nil {
			t.Fatalf("%s after invalidation: (%v, %v), want ErrNotFound", name, got, err)
		}
	}
	if err := s.InvalidateDeviceCodeSession(ctx, "device-signature"); !errors.Is(err, oauth2.ErrInvalidatedDeviceCode) {
		t.Fatalf("second invalidation = %v, want ErrInvalidatedDeviceCode", err)
	}
	if _, err := s.GetDeviceCodeSession(ctx, "no-such-signature", nil); !errors.Is(err, oauth2.ErrNotFound) {
		t.Fatalf("unknown device code = %v, want ErrNotFound", err)
	}
}

func TestARotatedRefreshTokenIsRedeemableOnlyWithinItsGrace(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	request := tokenRequest(t, s, "request-1", "")
	if err := s.CreateRefreshTokenSession(ctx, "refresh-1", "access-1", request); err != nil {
		t.Fatalf("CreateRefreshTokenSession: %v", err)
	}
	if err := s.RotateRefreshToken(ctx, "request-1", "refresh-1"); err != nil {
		t.Fatalf("RotateRefreshToken: %v", err)
	}
	got, err := s.GetRefreshTokenSession(ctx, "refresh-1", nil)
	if err != nil || got == nil {
		t.Fatalf("within the grace: (%v, %v), want the requester and no error", got, err)
	}
	var graceSeconds float64
	if err := pool.QueryRow(ctx, `select extract(epoch from grace_until - now())::float8 from oauth_refresh_tokens where signature = 'refresh-1'`).
		Scan(&graceSeconds); err != nil {
		t.Fatalf("read grace_until: %v", err)
	}
	if graceSeconds <= 50 || graceSeconds > 60 {
		t.Errorf("grace left = %.1fs, want about 60s", graceSeconds)
	}

	if _, err := pool.Exec(ctx, `update oauth_refresh_tokens set grace_until = now() - interval '1 second' where signature = 'refresh-1'`); err != nil {
		t.Fatalf("expire the grace: %v", err)
	}
	got, err = s.GetRefreshTokenSession(ctx, "refresh-1", nil)
	if !errors.Is(err, oauth2.ErrInactiveToken) || got == nil || got.GetID() != "request-1" {
		t.Fatalf("after the grace: (%v, %v), want the requester and ErrInactiveToken", got, err)
	}
}

func TestRevokeRefreshTokenEndsTheWholeFamily(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	signatures := []string{"refresh-1", "refresh-2", "refresh-3"}
	for i, signature := range signatures {
		if err := s.CreateRefreshTokenSession(ctx, signature, "", tokenRequest(t, s, "request-1", "")); err != nil {
			t.Fatalf("CreateRefreshTokenSession %s: %v", signature, err)
		}
		if i < len(signatures)-1 {
			if err := s.RotateRefreshToken(ctx, "request-1", signature); err != nil {
				t.Fatalf("RotateRefreshToken %s: %v", signature, err)
			}
		}
	}
	if err := s.CreateRefreshTokenSession(ctx, "other-family", "", tokenRequest(t, s, "request-2", "")); err != nil {
		t.Fatalf("CreateRefreshTokenSession other: %v", err)
	}

	if err := s.RevokeRefreshToken(ctx, "request-1"); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	for _, signature := range signatures {
		if _, err := s.GetRefreshTokenSession(ctx, signature, nil); !errors.Is(err, oauth2.ErrInactiveToken) {
			t.Errorf("%s after the revoke = %v, want ErrInactiveToken", signature, err)
		}
	}
	if _, err := s.GetRefreshTokenSession(ctx, "other-family", nil); err != nil {
		t.Errorf("another family's token after the revoke = %v, want it live", err)
	}
}

func TestEndCredentialEndsItsTokensAndNoOthers(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	ended, kept := uuid.NewString(), uuid.NewString()
	for _, id := range []string{ended, kept} {
		if _, err := pool.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
			values ($1, 'ada@example.com', 'example-host', $2, '{}'::jsonb, now() + interval '30 days')`, id, "thumbprint-"+id); err != nil {
			t.Fatalf("insert launcher_credentials: %v", err)
		}
		if err := s.CreateAccessTokenSession(ctx, "access-"+id, tokenRequest(t, s, "request-"+id, id)); err != nil {
			t.Fatalf("CreateAccessTokenSession: %v", err)
		}
		if err := s.CreateRefreshTokenSession(ctx, "refresh-"+id, "access-"+id, tokenRequest(t, s, "request-"+id, id)); err != nil {
			t.Fatalf("CreateRefreshTokenSession: %v", err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := s.EndCredential(ctx, tx, ended); err != nil {
		t.Fatalf("EndCredential: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := s.GetAccessTokenSession(ctx, "access-"+ended, nil); !errors.Is(err, oauth2.ErrInactiveToken) {
		t.Errorf("ended credential's access token = %v, want ErrInactiveToken", err)
	}
	if _, err := s.GetRefreshTokenSession(ctx, "refresh-"+ended, nil); !errors.Is(err, oauth2.ErrInactiveToken) {
		t.Errorf("ended credential's refresh token = %v, want ErrInactiveToken", err)
	}
	if _, err := s.GetAccessTokenSession(ctx, "access-"+kept, nil); err != nil {
		t.Errorf("other credential's access token = %v, want it live", err)
	}
	if _, err := s.GetRefreshTokenSession(ctx, "refresh-"+kept, nil); err != nil {
		t.Errorf("other credential's refresh token = %v, want it live", err)
	}
}

func TestADPoPProofIsUsedOnce(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	exp := time.Now().Add(time.Minute)
	for i, want := range []bool{false, true} {
		used, err := s.CheckAndSetDPoPProofUsed(ctx, "jti-1", "jkt", "", "POST", "https://broker.example/v1/oauth/token", exp)
		if err != nil || used != want {
			t.Fatalf("presentation %d: (%v, %v), want (%v, nil)", i+1, used, err, want)
		}
	}
	if used, err := s.CheckAndSetDPoPProofUsed(ctx, "jti-1", "jkt", "", "POST", "https://broker.example/v1/oauth/device_authorization", exp); err != nil || used {
		t.Fatalf("same jti at another htu: (%v, %v), want (false, nil)", used, err)
	}
	if _, err := pool.Exec(ctx, `update oauth_dpop_proofs set expires_at = now() - interval '1 second'`); err != nil {
		t.Fatalf("expire the proofs: %v", err)
	}
	if used, err := s.CheckAndSetDPoPProofUsed(ctx, "jti-1", "jkt", "", "POST", "https://broker.example/v1/oauth/token", exp); err != nil || used {
		t.Fatalf("an expired record: (%v, %v), want (false, nil)", used, err)
	}
}

func TestADPoPNonceIsValidUntilItExpires(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.CreateDPoPNonce(ctx, "live", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("CreateDPoPNonce: %v", err)
	}
	if err := s.CreateDPoPNonce(ctx, "stale", time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("CreateDPoPNonce: %v", err)
	}
	for nonce, want := range map[string]bool{"live": true, "stale": false, "unknown": false} {
		valid, err := s.IsDPoPNonceValid(ctx, nonce)
		if err != nil || valid != want {
			t.Errorf("IsDPoPNonceValid(%q) = (%v, %v), want (%v, nil)", nonce, valid, err, want)
		}
	}
}

func TestTheSecretIsCreatedOnceAndKept(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	first, err := s.Secret(ctx)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	second, err := s.Secret(ctx)
	if err != nil {
		t.Fatalf("Secret again: %v", err)
	}
	if len(first) != 32 || !bytes.Equal(first, second) {
		t.Fatalf("secrets %x and %x, want the same 32 bytes", first, second)
	}
}

func TestAuthorizeCodeSessionsAreRefused(t *testing.T) {
	s, _ := newStore(t)
	if err := s.CreateAuthorizeCodeSession(context.Background(), "code", tokenRequest(t, s, "request-1", "")); !errors.Is(err, oauth2.ErrInvalidRequest) {
		t.Fatalf("CreateAuthorizeCodeSession = %v, want ErrInvalidRequest", err)
	}
}

func TestOnlyTheOneClientExists(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	client, err := s.GetClient(ctx, oauthstore.ClientID)
	if err != nil || !client.IsPublic() || client.GetID() != oauthstore.ClientID {
		t.Fatalf("GetClient(%q) = (%#v, %v)", oauthstore.ClientID, client, err)
	}
	if _, err := s.GetClient(ctx, "someone-else"); !errors.Is(err, oauth2.ErrNotFound) {
		t.Fatalf("GetClient(other) = %v, want ErrNotFound", err)
	}
}

// A transaction the caller passes with WithTx spans the store's writes, and the library's own
// BeginTX/Commit inside it neither nests a transaction nor commits the caller's.
func TestBeginTXJoinsTheCallersTransaction(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	inner, err := s.BeginTX(oauthstore.WithTx(ctx, tx))
	if err != nil {
		t.Fatalf("BeginTX: %v", err)
	}
	if err := s.CreateRefreshTokenSession(inner, "refresh-1", "", tokenRequest(t, s, "request-1", "")); err != nil {
		t.Fatalf("CreateRefreshTokenSession: %v", err)
	}
	if err := s.Commit(inner); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := s.GetRefreshTokenSession(ctx, "refresh-1", nil); !errors.Is(err, oauth2.ErrNotFound) {
		t.Fatalf("outside the caller's transaction before it commits = %v, want ErrNotFound", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("the caller's rollback: %v", err)
	}
	if _, err := s.GetRefreshTokenSession(ctx, "refresh-1", nil); !errors.Is(err, oauth2.ErrNotFound) {
		t.Fatalf("after the caller rolled back = %v, want ErrNotFound", err)
	}

	owned, err := s.BeginTX(ctx)
	if err != nil {
		t.Fatalf("BeginTX without a transaction: %v", err)
	}
	if err := s.CreateRefreshTokenSession(owned, "refresh-2", "", tokenRequest(t, s, "request-2", "")); err != nil {
		t.Fatalf("CreateRefreshTokenSession: %v", err)
	}
	if err := s.Commit(owned); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := s.GetRefreshTokenSession(ctx, "refresh-2", nil); err != nil {
		t.Fatalf("after BeginTX's own commit = %v, want it stored", err)
	}
}
