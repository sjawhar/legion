package oauthstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
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

// newCredential inserts a live launcher credential and answers its id, which every token session
// names.
func newCredential(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(context.Background(), `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1, 'ada@example.com', 'example-host', $2, '{}'::jsonb, now() + interval '30 days')`, id, "thumbprint-"+id); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	return id
}

// tokenRequest is a token session for requestID carrying credentialID ("" for none) in its
// session's extra claims, its tokens expiring in an hour.
func tokenRequest(t *testing.T, s *oauthstore.Store, requestID, credentialID string) *oauth2.Request {
	t.Helper()
	client, err := s.GetClient(context.Background(), oauthwire.ClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	session := &oauth2.DefaultSession{Extra: map[string]any{}}
	if credentialID != "" {
		session.Extra[oauthwire.ExtraCredentialID] = credentialID
	}
	session.SetExpiresAt(oauth2.AccessToken, time.Now().Add(time.Hour))
	session.SetExpiresAt(oauth2.RefreshToken, time.Now().Add(time.Hour))
	request := oauth2.NewRequest()
	request.SetID(requestID)
	request.Client = client
	request.GrantScope(oauthwire.Scope)
	request.SetSession(session)
	return request
}

// TestATokenSessionMustNameItsCredential: a token is issued only after an approval, which names
// the credential, so a session naming none (or not as a string) is refused rather than stored
// with no credential a revoke could end it by.
func TestATokenSessionMustNameItsCredential(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.CreateAccessTokenSession(ctx, "access-1", tokenRequest(t, s, "request-1", "")); err == nil {
		t.Error("CreateAccessTokenSession with no credential_id = nil, want an error")
	}
	if err := s.CreateRefreshTokenSession(ctx, "refresh-1", "", tokenRequest(t, s, "request-1", "")); err == nil {
		t.Error("CreateRefreshTokenSession with no credential_id = nil, want an error")
	}
	wrong := tokenRequest(t, s, "request-2", "")
	wrong.Session.(*oauth2.DefaultSession).Extra[oauthwire.ExtraCredentialID] = 7
	if err := s.CreateRefreshTokenSession(ctx, "refresh-2", "", wrong); err == nil {
		t.Error("CreateRefreshTokenSession with a numeric credential_id = nil, want an error")
	}
}

// TestReadingIntoAnotherSessionTypeIsRefused: the store keeps *oauth2.DefaultSession sessions, so
// a read into any other session type is an error, never a silent partial decode.
func TestReadingIntoAnotherSessionTypeIsRefused(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.CreateRefreshTokenSession(ctx, "refresh-1", "", tokenRequest(t, s, "request-1", newCredential(t, pool))); err != nil {
		t.Fatalf("CreateRefreshTokenSession: %v", err)
	}
	if _, err := s.GetRefreshTokenSession(ctx, "refresh-1", &otherSession{}); err == nil {
		t.Fatal("GetRefreshTokenSession into another session type = nil, want an error")
	}
}

// otherSession is an oauth2.Session that is not an *oauth2.DefaultSession.
type otherSession struct{ oauth2.DefaultSession }

func TestDeviceCodeRoundTripsAndIsFoundByUserCode(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	client, err := s.GetClient(ctx, oauthwire.ClientID)
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
		if got.GetID() != request.GetID() || got.GetClient().GetID() != oauthwire.ClientID {
			t.Errorf("%s: id %q client %q, want %q %q", name, got.GetID(), got.GetClient().GetID(), request.GetID(), oauthwire.ClientID)
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
	request := tokenRequest(t, s, "request-1", newCredential(t, pool))
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
	s, pool := newStore(t)
	credential := newCredential(t, pool)
	signatures := []string{"refresh-1", "refresh-2", "refresh-3"}
	for i, signature := range signatures {
		if err := s.CreateRefreshTokenSession(ctx, signature, "", tokenRequest(t, s, "request-1", credential)); err != nil {
			t.Fatalf("CreateRefreshTokenSession %s: %v", signature, err)
		}
		if i < len(signatures)-1 {
			if err := s.RotateRefreshToken(ctx, "request-1", signature); err != nil {
				t.Fatalf("RotateRefreshToken %s: %v", signature, err)
			}
		}
	}
	if err := s.CreateRefreshTokenSession(ctx, "other-family", "", tokenRequest(t, s, "request-2", credential)); err != nil {
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
	ended, kept := newCredential(t, pool), newCredential(t, pool)
	for _, id := range []string{ended, kept} {
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
	if err := oauthstore.EndCredential(ctx, tx, ended); err != nil {
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

// TestSweepDeletesOnlySpentState pins each of Sweep's rules against a row it must delete and one
// it must keep.
func TestSweepDeletesOnlySpentState(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	for _, q := range []string{
		`insert into oauth_device_codes (signature, user_code_signature, request, expires_at) values
			('device-long-gone', 'u1', '{}', now() - interval '2 hours'),
			('device-just-expired', 'u2', '{}', now() - interval '30 minutes'),
			('device-live', 'u3', '{}', now() + interval '10 minutes')`,
		`insert into oauth_dpop_proofs (jti, htu, expires_at) values ('proof-expired', 'h', now() - interval '1 second'), ('proof-live', 'h', now() + interval '1 minute')`,
		`insert into oauth_dpop_nonces (nonce, expires_at) values ('nonce-expired', now() - interval '1 second'), ('nonce-live', now() + interval '1 minute')`,
		`insert into oauth_access_tokens (signature, request_id, request, expires_at) values
			('access-expired', 'r', '{}', now() - interval '1 second'), ('access-live', 'r', '{}', now() + interval '1 hour')`,
		// rotated: its old token's grace passed, its newest is live, so all of it stays.
		// revoked: every row inactive. lapsed: every row past its expiry. spent: its only token's
		// grace passed with no successor. retrying: its only token is inside its grace.
		`insert into oauth_refresh_tokens (signature, request_id, request, active, grace_until, expires_at) values
			('rotated-old', 'rotated', '{}', true, now() - interval '1 second', now() + interval '1 day'),
			('rotated-new', 'rotated', '{}', true, null, now() + interval '1 day'),
			('revoked-old', 'revoked', '{}', false, now() - interval '1 second', now() + interval '1 day'),
			('revoked-new', 'revoked', '{}', false, null, now() + interval '1 day'),
			('lapsed-1', 'lapsed', '{}', true, null, now() - interval '1 second'),
			('spent-1', 'spent', '{}', true, now() - interval '1 second', now() + interval '1 day'),
			('retrying-1', 'retrying', '{}', true, now() + interval '30 seconds', now() + interval '1 day')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for table, want := range map[string]string{
		"select string_agg(signature, ',' order by signature) from oauth_device_codes":   "device-just-expired,device-live",
		"select string_agg(jti, ',' order by jti) from oauth_dpop_proofs":                "proof-live",
		"select string_agg(nonce, ',' order by nonce) from oauth_dpop_nonces":            "nonce-live",
		"select string_agg(signature, ',' order by signature) from oauth_access_tokens":  "access-live",
		"select string_agg(signature, ',' order by signature) from oauth_refresh_tokens": "retrying-1,rotated-new,rotated-old",
	} {
		var got string
		if err := pool.QueryRow(ctx, table).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", table, got, err, want)
		}
	}
}

// TestSweepDeletesABacklogOfSeveralBatches: a backlog of several batches, within the per-call cap,
// is all deleted in one Sweep, and a live family survives it.
func TestSweepDeletesABacklogOfSeveralBatches(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	oauthstore.SetSweepBatch(t, 3)
	for _, q := range []string{
		`insert into oauth_device_codes (signature, user_code_signature, request, expires_at)
			select 'd'||i, 'u'||i, '{}', now() - interval '2 hours' from generate_series(1, 10) i`,
		`insert into oauth_dpop_proofs (jti, htu, expires_at) select 'p'||i, 'h', now() - interval '1 second' from generate_series(1, 10) i`,
		`insert into oauth_dpop_nonces (nonce, expires_at) select 'n'||i, now() - interval '1 second' from generate_series(1, 10) i`,
		`insert into oauth_access_tokens (signature, request_id, request, expires_at) select 'a'||i, 'r', '{}', now() - interval '1 second' from generate_series(1, 10) i`,
		`insert into oauth_refresh_tokens (signature, request_id, request, active, expires_at)
			select 'f'||i||'-'||j, 'family-'||i, '{}', false, now() + interval '1 day' from generate_series(1, 10) i, generate_series(1, 2) j`,
		`insert into oauth_refresh_tokens (signature, request_id, request, expires_at) values ('live', 'live', '{}', now() + interval '1 day')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	var left string
	if err := pool.QueryRow(ctx, `select concat_ws(',', (select count(*) from oauth_device_codes), (select count(*) from oauth_dpop_proofs),
		(select count(*) from oauth_dpop_nonces), (select count(*) from oauth_access_tokens), (select string_agg(signature, ',') from oauth_refresh_tokens))`).Scan(&left); err != nil || left != "0,0,0,0,live" {
		t.Fatalf("rows left after the sweep = %q (%v), want 0,0,0,0,live", left, err)
	}
}

// TestSweepWorkIsBoundedPerCall: one Sweep runs at most sweepMaxBatches batches of each kind and
// leaves the rest to the next; a refresh token family larger than a batch is deleted row by row
// over several Sweeps, and a live family is never touched.
func TestSweepWorkIsBoundedPerCall(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	oauthstore.SetSweepBatch(t, 3)
	oauthstore.SetSweepMaxBatches(t, 2)
	for _, q := range []string{
		`insert into oauth_dpop_proofs (jti, htu, expires_at) select 'p'||i, 'h', now() - interval '1 second' from generate_series(1, 10) i`,
		`insert into oauth_refresh_tokens (signature, request_id, request, active, expires_at)
			select 'dead-'||i, 'dead', '{}', false, now() + interval '1 day' from generate_series(1, 10) i`,
		`insert into oauth_refresh_tokens (signature, request_id, request, active, grace_until, expires_at)
			select 'live-'||i, 'live', '{}', true, now() - interval '1 second', now() + interval '1 day' from generate_series(1, 4) i`,
		`insert into oauth_refresh_tokens (signature, request_id, request, expires_at) values ('live-newest', 'live', '{}', now() + interval '1 day')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	left := func() (proofs, dead, live int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select (select count(*) from oauth_dpop_proofs), (select count(*) from oauth_refresh_tokens where request_id = 'dead'),
			(select count(*) from oauth_refresh_tokens where request_id = 'live')`).Scan(&proofs, &dead, &live); err != nil {
			t.Fatalf("count: %v", err)
		}
		return proofs, dead, live
	}
	for i, want := range [][3]int{{4, 4, 5}, {0, 0, 5}} {
		if err := s.Sweep(ctx); err != nil {
			t.Fatalf("Sweep %d: %v", i+1, err)
		}
		if p, d, l := left(); [3]int{p, d, l} != want {
			t.Fatalf("after Sweep %d: %d proofs, %d dead-family and %d live-family rows, want %v", i+1, p, d, l, want)
		}
	}
}

// TestEverySweepStatementHasAnIndexPath: each of Sweep's statements reads a bounded set of rows
// through an index, so a pass's work is its batch however many rows the table holds. A statement
// that picks its batch does so with one index scan cut off by its LIMIT: nothing under the Limit
// gathers, merges, dedupes or sorts rows before it cuts. The seeded tables are small enough that
// the planner would pick a sequential or bitmap scan (which reads every match, then sorts) on its
// own, so the plan is read with both priced out: one that remains has no index to use.
func TestEverySweepStatementHasAnIndexPath(t *testing.T) {
	ctx := context.Background()
	_, pool := newStore(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off; set local enable_bitmapscan = off`); err != nil {
		t.Fatal(err)
	}
	type node struct {
		NodeType string `json:"Node Type"`
		Plans    []node `json:"Plans"`
	}
	var walk func(n node, visit func(node))
	walk = func(n node, visit func(node)) {
		visit(n)
		for _, c := range n.Plans {
			walk(c, visit)
		}
	}
	for _, q := range oauthstore.SweepStatements() {
		var raw []byte
		if err := tx.QueryRow(ctx, "explain (format json) "+q.SQL, q.Args...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", q.What, err)
		}
		var plans []struct{ Plan node }
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("explain %s: %v in %s", q.What, err, raw)
		}
		limits := 0
		walk(plans[0].Plan, func(n node) {
			if n.NodeType == "Seq Scan" {
				t.Errorf("sweeping %s reads a table whole:\n%s", q.What, raw)
			}
			if n.NodeType != "Limit" {
				return
			}
			limits++
			indexed := false
			for _, c := range n.Plans {
				walk(c, func(m node) {
					switch m.NodeType {
					case "Aggregate", "Append", "Merge Append", "Unique", "Sort", "Incremental Sort", "SetOp", "Hash", "Materialize":
						t.Errorf("sweeping %s runs %s under its Limit:\n%s", q.What, m.NodeType, raw)
					case "Index Scan", "Index Only Scan":
						indexed = true
					}
				})
			}
			if !indexed {
				t.Errorf("sweeping %s has no index scan under its Limit:\n%s", q.What, raw)
			}
		})
		if q.PicksBatch && limits == 0 {
			t.Errorf("sweeping %s has no Limit:\n%s", q.What, raw)
		}
	}
}

// TestSweepExaminesALiveFamilyOncePerPass: the refresh sweep walks the table with a cursor, so a
// tick examines rows the last tick did not, a live family's rows are read once per pass over the
// table rather than every tick, and a family that dies after the cursor passed its rows loses
// them on the next pass.
func TestSweepExaminesALiveFamilyOncePerPass(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	oauthstore.SetSweepBatch(t, 3)
	oauthstore.SetSweepMaxBatches(t, 1)
	if _, err := pool.Exec(ctx, `insert into oauth_refresh_tokens (signature, request_id, request, active, grace_until, expires_at)
		select 'live-'||i, 'live', '{}'::jsonb, true, now() - interval '1 second', now() + interval '1 day' from generate_series(1, 6) i
		union all select 'live-newest', 'live', '{}'::jsonb, true, null, now() + interval '2 days'`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatalf("Sweep 1: %v", err)
	}
	// Sweep 1 examined live-1 to live-3 and kept them; the family dies now.
	if _, err := pool.Exec(ctx, `update oauth_refresh_tokens set active = false where request_id = 'live'`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for i, want := range []int{4, 3, 0} {
		if err := s.Sweep(ctx); err != nil {
			t.Fatalf("Sweep %d: %v", i+2, err)
		}
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from oauth_refresh_tokens`).Scan(&n); err != nil || n != want {
			t.Fatalf("after Sweep %d: %d rows (%v), want %d", i+2, n, err, want)
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
	client, err := s.GetClient(ctx, oauthwire.ClientID)
	if err != nil || !client.IsPublic() || client.GetID() != oauthwire.ClientID {
		t.Fatalf("GetClient(%q) = (%#v, %v)", oauthwire.ClientID, client, err)
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
	if err := s.CreateRefreshTokenSession(inner, "refresh-1", "", tokenRequest(t, s, "request-1", newCredential(t, pool))); err != nil {
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
	if err := s.CreateRefreshTokenSession(owned, "refresh-2", "", tokenRequest(t, s, "request-2", newCredential(t, pool))); err != nil {
		t.Fatalf("CreateRefreshTokenSession: %v", err)
	}
	if err := s.Commit(owned); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := s.GetRefreshTokenSession(ctx, "refresh-2", nil); err != nil {
		t.Fatalf("after BeginTX's own commit = %v, want it stored", err)
	}
}
