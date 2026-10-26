package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// userCodePattern is the shape of every user code the provider mints.
var userCodePattern = regexp.MustCompile(`^[0-9A-Z]{4}-[0-9A-Z]{4}$`)

// fixture is a provider on a fresh schema (dsn names it), mounted alone on an httptest.Server:
// its device authorization endpoint at /device_authorization and its token endpoint at /token.
// onRequest records each user code it is given and answers refuse (nil by default); the token
// hooks record what they saw and answer before (nil by default).
type fixture struct {
	url      string
	dsn      string
	pool     *pgxpool.Pool
	provider *oauth.Provider

	mu        sync.Mutex
	userCodes []string
	sawTx     bool
	refuse    error
	before    error
	grants    []oauth2.GrantType
	extras    []map[string]any
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, func(*oauth.Config) {})
}

// newFixtureWith is newFixture with adjust applied to the provider's Config.
func newFixtureWith(t *testing.T, adjust func(*oauth.Config)) *fixture {
	t.Helper()
	return newFixtureOn(t, storetest.URL(t), adjust)
}

// newFixtureOn is newFixtureWith on the database dsn names rather than a fresh one, which is how a
// test starts a second broker process beside a first.
func newFixtureOn(t *testing.T, dsn string, adjust func(*oauth.Config)) *fixture {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	store := oauthstore.New(pool)
	store.Reused = enroll.RevokeReusedCredential
	secret, err := store.Secret(context.Background())
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	cfg := oauth.Config{
		Secret:          secret,
		DeviceLifetime:  15 * time.Minute,
		AccessLifetime:  15 * time.Minute,
		RefreshLifetime: time.Hour,
		ProofSkew:       10 * time.Second,
		VerificationURL: "https://dispatch.example/credentials/machine",
		Exchange:        oauth.ExchangeConfig{Policy: newPolicy(t, nil, nil)},
	}
	adjust(&cfg)
	provider, err := oauth.New(cfg, store)
	if err != nil {
		t.Fatalf("oauth.New: %v", err)
	}
	f := &fixture{dsn: dsn, pool: pool, provider: provider}
	hooks := oauth.TokenHooks{
		Before: func(_ context.Context, _ oauth2.GrantType, _ map[string]any) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.before
		},
		After: func(_ context.Context, grant oauth2.GrantType, extra map[string]any) (map[string]any, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.grants = append(f.grants, grant)
			f.extras = append(f.extras, extra)
			return map[string]any{"credential_id": extra["credential_id"]}, nil
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device_authorization", func(w http.ResponseWriter, r *http.Request) {
		provider.DeviceAuthorize(w, r, func(ctx context.Context, _ url.Values, userCode string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			_, f.sawTx = oauthstore.TxFrom(ctx)
			f.userCodes = append(f.userCodes, userCode)
			return f.refuse
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		provider.Token(w, r, hooks)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// post posts form to path with a DPoP proof signed by key for that path (none when key is nil),
// and decodes the JSON answer.
func (f *fixture) post(t *testing.T, path string, form url.Values, key *ecdsa.PrivateKey) (int, http.Header, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if key != nil {
		req.Header.Set("DPoP", dpopProof(t, key, f.url+path))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s answer: %v", path, err)
	}
	return resp.StatusCode, resp.Header, body
}

// dpopProof is an RFC 9449 proof of key for a POST to htu.
func dpopProof(t *testing.T, key *ecdsa.PrivateKey, htu string) string {
	t.Helper()
	return dpopProofAt(t, key, htu, time.Now())
}

// dpopProofAt is dpopProof issued at iat.
func dpopProofAt(t *testing.T, key *ecdsa.PrivateKey, htu string, iat time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"jti": uuid.NewString(), "htm": http.MethodPost, "htu": htu, "iat": iat.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

func thumbprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return jkt
}

// authorize starts a device login bound to key, answering its device and user codes.
func (f *fixture) authorize(t *testing.T, key *ecdsa.PrivateKey) (deviceCode, userCode string) {
	t.Helper()
	status, _, body := f.post(t, "/device_authorization", url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {"machine"}, "dpop_jkt": {thumbprint(t, key)},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("device authorization = %d %v, want 200", status, body)
	}
	deviceCode, _ = body["device_code"].(string)
	userCode, _ = body["user_code"].(string)
	return deviceCode, userCode
}

// poll asks the token endpoint for the device code's tokens with a proof of key. The poll's
// last-checked time is cleared first, so a test's back-to-back polls are not slow_down.
func (f *fixture) poll(t *testing.T, deviceCode string, key *ecdsa.PrivateKey) (int, map[string]any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `update oauth_device_codes set request = request - 'last_checked'`); err != nil {
		t.Fatalf("clear last poll: %v", err)
	}
	status, _, body := f.post(t, "/token", url.Values{
		"grant_type": {deviceGrant}, "device_code": {deviceCode}, "client_id": {oauthwire.ClientID},
	}, key)
	return status, body
}

func (f *fixture) refresh(t *testing.T, refreshToken string, key *ecdsa.PrivateKey) (int, map[string]any) {
	t.Helper()
	status, _, body := f.post(t, "/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {oauthwire.ClientID},
	}, key)
	return status, body
}

// newCredential inserts a launcher credential for a session to name, as an approval mints one.
func (f *fixture) newCredential(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := f.pool.Exec(context.Background(), `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1, 'ada@example.com', 'example-host', $2, '{}'::jsonb, now() + interval '30 days')`, id, "thumbprint-"+id); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	return id
}

// login runs a device login with key to its first token pair, answering its refresh token.
func (f *fixture) login(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	deviceCode, userCode := f.authorize(t, key)
	if err := f.provider.ApproveDevice(context.Background(), userCode, true, map[string]any{"credential_id": f.newCredential(t)}); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
	status, body := f.poll(t, deviceCode, key)
	if status != http.StatusOK {
		t.Fatalf("token = %d %v, want 200", status, body)
	}
	refreshToken, _ := body["refresh_token"].(string)
	if refreshToken == "" {
		t.Fatalf("token answer %v has no refresh_token", body)
	}
	return refreshToken
}

func (f *fixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `select count(*) from `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestDeviceAuthorizationOmitsTheCompleteURI(t *testing.T) {
	f := newFixture(t)
	status, header, body := f.post(t, "/device_authorization", url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {"machine"}, "dpop_jkt": {thumbprint(t, newKey(t))},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("device authorization = %d %v, want 200", status, body)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if code, _ := body["device_code"].(string); code == "" {
		t.Errorf("device_code missing from %v", body)
	}
	userCode, _ := body["user_code"].(string)
	if !userCodePattern.MatchString(userCode) {
		t.Errorf("user_code = %q, want XXXX-XXXX", userCode)
	}
	if got := body["verification_uri"]; got != "https://dispatch.example/credentials/machine" {
		t.Errorf("verification_uri = %v", got)
	}
	if got := body["interval"]; got != float64(5) {
		t.Errorf("interval = %v, want 5", got)
	}
	if _, ok := body["verification_uri_complete"]; ok {
		t.Errorf("answer carries verification_uri_complete: %v", body)
	}
	if len(f.userCodes) != 1 || f.userCodes[0] != userCode || !f.sawTx {
		t.Errorf("onRequest saw codes %v (in a transaction: %v), want [%s] in one", f.userCodes, f.sawTx, userCode)
	}
}

// A request onRequest refuses is answered with its broker code and stores no device code.
func TestARefusedDeviceRequestStoresNothing(t *testing.T) {
	f := newFixture(t)
	f.refuse = &oauth.Refusal{Status: http.StatusBadRequest, Code: "REQUEST_INVALID", Message: "the request object does not verify"}
	status, _, body := f.post(t, "/device_authorization", url.Values{"client_id": {oauthwire.ClientID}, "scope": {"machine"}}, nil)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" ||
		body["error_description"] != "REQUEST_INVALID: the request object does not verify" {
		t.Fatalf("refused request = %d %v, want 400 invalid_request naming REQUEST_INVALID", status, body)
	}
	if n := f.count(t, "oauth_device_codes"); n != 0 {
		t.Fatalf("%d device codes stored, want none", n)
	}

	f.refuse = errors.New("the database is down")
	if status, _, body := f.post(t, "/device_authorization", url.Values{"client_id": {oauthwire.ClientID}}, nil); status != http.StatusInternalServerError || body["error"] != "server_error" {
		t.Fatalf("an unknown onRequest error = %d %v, want 500 server_error", status, body)
	}
}

func TestARateLimitRefusalNamesItsRetryAfter(t *testing.T) {
	f := newFixture(t)
	f.refuse = &oauth.Refusal{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED", Message: "too many", RetryAfter: 30}
	status, header, body := f.post(t, "/device_authorization", url.Values{"client_id": {oauthwire.ClientID}}, nil)
	if status != http.StatusTooManyRequests || header.Get("Retry-After") != "30" || body["error_description"] != "RATE_LIMITED: too many" {
		t.Fatalf("rate-limited request = %d Retry-After=%q %v", status, header.Get("Retry-After"), body)
	}
}

func TestTheTokenEndpointWaitsUntilTheCodeIsApproved(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	deviceCode, userCode := f.authorize(t, key)
	if status, body := f.poll(t, deviceCode, key); status != http.StatusBadRequest || body["error"] != "authorization_pending" {
		t.Fatalf("poll before approval = %d %v, want authorization_pending", status, body)
	}
	credentialID := f.newCredential(t)
	// The person types the code as they read it: lower case, with a space for the hyphen.
	typed := strings.ToLower(strings.Replace(userCode, "-", " ", 1))
	if err := f.provider.ApproveDevice(context.Background(), typed, true, map[string]any{"credential_id": credentialID}); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
	status, body := f.poll(t, deviceCode, key)
	if status != http.StatusOK {
		t.Fatalf("poll after approval = %d %v, want 200", status, body)
	}
	if body["token_type"] != "DPoP" || body["access_token"] == nil || body["refresh_token"] == nil {
		t.Fatalf("token answer = %v, want a DPoP access token and a refresh token", body)
	}
	if body["credential_id"] != credentialID {
		t.Errorf("token answer credential_id = %v, want After's %s", body["credential_id"], credentialID)
	}
	if len(f.grants) != 1 || f.grants[0] != deviceGrant || f.extras[0]["credential_id"] != credentialID {
		t.Fatalf("After saw grants %v extras %v, want one device_code grant naming %s", f.grants, f.extras, credentialID)
	}
	if status, body := f.poll(t, deviceCode, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("second exchange = %d %v, want invalid_grant", status, body)
	}
}

// TestAPollThatReadThePendingCodeBeforeTheApprovalKeepsTheApproval: the library records a pending
// poll's time by writing back the device code it read. That write landing after an approval
// committed leaves the approval in place, so the next poll is issued its tokens.
func TestAPollThatReadThePendingCodeBeforeTheApprovalKeepsTheApproval(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	deviceCode, userCode := f.authorize(t, key)
	ctx := context.Background()
	store := oauthstore.New(f.pool)
	var signature string
	if err := f.pool.QueryRow(ctx, `select signature from oauth_device_codes`).Scan(&signature); err != nil {
		t.Fatal(err)
	}
	pending, err := store.GetDeviceCodeSession(ctx, signature, nil)
	if err != nil {
		t.Fatalf("read the pending code: %v", err)
	}
	if err := f.provider.ApproveDevice(ctx, userCode, true, map[string]any{"credential_id": f.newCredential(t)}); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
	pending.SetLastChecked(time.Now())
	if err := store.UpdateDeviceCodeSession(ctx, signature, pending); err != nil {
		t.Fatalf("the poll's late write: %v", err)
	}
	if status, body := f.poll(t, deviceCode, key); status != http.StatusOK {
		t.Fatalf("poll after the late write = %d %v, want 200", status, body)
	}
}

// TestATokenRequestsDPoPSkewIsTheBrokersProofSkew: the token endpoint accepts a proof from a
// clock as far off as BROKER_PROOF_SKEW_SECONDS allows every other broker proof, not the library's
// ten seconds, and refuses one past it.
func TestATokenRequestsDPoPSkewIsTheBrokersProofSkew(t *testing.T) {
	f := newFixtureWith(t, func(c *oauth.Config) { c.ProofSkew = time.Minute })
	key := newKey(t)
	first := f.login(t, key)
	post := func(refreshToken string, iat time.Time) (int, map[string]any) {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {oauthwire.ClientID}}
		req, err := http.NewRequest(http.MethodPost, f.url+"/token", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProofAt(t, key, f.url+"/token", iat))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	status, body := post(first, time.Now().Add(15*time.Second))
	if status != http.StatusOK {
		t.Fatalf("refresh with a proof 15 s ahead under a 60 s skew = %d %v, want 200", status, body)
	}
	next, _ := body["refresh_token"].(string)
	if status, body := post(next, time.Now().Add(90*time.Second)); status != http.StatusBadRequest || body["error"] != "invalid_dpop_proof" {
		t.Fatalf("refresh with a proof 90 s ahead = %d %v, want invalid_dpop_proof", status, body)
	}
}

func TestADeniedCodeAnswersAccessDenied(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	deviceCode, userCode := f.authorize(t, key)
	if err := f.provider.ApproveDevice(context.Background(), userCode, false, nil); err != nil {
		t.Fatalf("ApproveDevice(deny): %v", err)
	}
	if status, body := f.poll(t, deviceCode, key); status == http.StatusOK || body["error"] != "access_denied" {
		t.Fatalf("poll after denial = %d %v, want access_denied", status, body)
	}
}

func TestApprovingAnUnknownCodeIsNotFound(t *testing.T) {
	f := newFixture(t)
	if err := f.provider.ApproveDevice(context.Background(), "ABCD-EFGH", true, nil); !errors.Is(err, oauth2.ErrNotFound) {
		t.Fatalf("ApproveDevice(unknown) = %v, want oauth2.ErrNotFound", err)
	}
}

func TestTheTokenEndpointRefusesAProofFromAnotherKey(t *testing.T) {
	f := newFixture(t)
	keyA, keyB := newKey(t), newKey(t)
	deviceCode, userCode := f.authorize(t, keyA)
	if err := f.provider.ApproveDevice(context.Background(), userCode, true, map[string]any{"credential_id": f.newCredential(t)}); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
	if status, body := f.poll(t, deviceCode, keyB); status != http.StatusBadRequest || body["error"] != "invalid_dpop_proof" {
		t.Fatalf("poll with another key = %d %v, want invalid_dpop_proof", status, body)
	}
	if status, body := f.poll(t, deviceCode, nil); status != http.StatusBadRequest || body["error"] != "invalid_dpop_proof" {
		t.Fatalf("poll with no proof = %d %v, want invalid_dpop_proof", status, body)
	}
	if status, body := f.poll(t, deviceCode, keyA); status != http.StatusOK {
		t.Fatalf("poll with the named key = %d %v, want 200", status, body)
	}
}

func TestARefreshRotatesAndTheOldTokenIsRefusedAfterItsGrace(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	first := f.login(t, key)

	status, body := f.refresh(t, first, key)
	second, _ := body["refresh_token"].(string)
	if status != http.StatusOK || second == "" || second == first || body["token_type"] != "DPoP" {
		t.Fatalf("refresh = %d %v, want a new DPoP-bound pair", status, body)
	}
	if len(f.grants) != 2 || f.grants[1] != "refresh_token" || f.extras[1]["credential_id"] == nil {
		t.Fatalf("After saw grants %v extras %v, want the refresh with the login's credential", f.grants, f.extras)
	}
	if status, body := f.refresh(t, second, newKey(t)); status != http.StatusBadRequest || body["error"] != "invalid_dpop_proof" {
		t.Fatalf("refresh with another key = %d %v, want invalid_dpop_proof", status, body)
	}
	status, body = f.refresh(t, second, key)
	if third, _ := body["refresh_token"].(string); status != http.StatusOK || third == "" || third == second {
		t.Fatalf("refresh of the rotated token = %d %v, want a new pair", status, body)
	}

	if _, err := f.pool.Exec(context.Background(), `update oauth_refresh_tokens set grace_until = now() - interval '1 second' where grace_until is not null`); err != nil {
		t.Fatalf("end the grace: %v", err)
	}
	if status, body := f.refresh(t, first, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("the first token after its grace = %d %v, want invalid_grant", status, body)
	}
}

// shiftGrace moves every rotated token's grace earlier by elapsed, as if elapsed had passed since
// its rotation.
func (f *fixture) shiftGrace(t *testing.T, elapsed time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `update oauth_refresh_tokens set grace_until = grace_until - make_interval(secs => $1) where grace_until is not null`, elapsed.Seconds()); err != nil {
		t.Fatalf("shift the grace: %v", err)
	}
}

// TestALostRefreshAnswerRetriedTwoMinutesLaterKeepsTheLogin: a rotated token stays redeemable for
// five minutes, so a helper whose answer was lost and whose retries back off past a minute still
// keeps its login, and the token it is answered refreshes on.
func TestALostRefreshAnswerRetriedTwoMinutesLaterKeepsTheLogin(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	first := f.login(t, key)
	if status, body := f.refresh(t, first, key); status != http.StatusOK {
		t.Fatalf("refresh whose answer is lost = %d %v", status, body)
	}
	f.shiftGrace(t, 2*time.Minute)
	status, body := f.refresh(t, first, key)
	retried, _ := body["refresh_token"].(string)
	if status != http.StatusOK || retried == "" {
		t.Fatalf("retry at +2 min = %d %v, want a new pair", status, body)
	}
	if status, body := f.refresh(t, retried, key); status != http.StatusOK {
		t.Fatalf("refresh of the retried answer's token = %d %v, want 200", status, body)
	}
	f.shiftGrace(t, 4*time.Minute)
	if status, body := f.refresh(t, first, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("the first token past its five minutes = %d %v, want invalid_grant", status, body)
	}
}

// TestTwoRedemptionsWithinTheGraceLeaveOneBranch: redeeming a rotated token again within its
// grace ends the tokens its earlier redemption issued, so the family never forks into two live
// branches; the earlier branch's token, presented afterwards, is a reuse.
func TestTwoRedemptionsWithinTheGraceLeaveOneBranch(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	first := f.login(t, key)
	_, body := f.refresh(t, first, key)
	earlier, _ := body["refresh_token"].(string)
	_, body = f.refresh(t, first, key)
	later, _ := body["refresh_token"].(string)
	if earlier == "" || later == "" || earlier == later {
		t.Fatalf("two redemptions answered %q and %q, want two tokens", earlier, later)
	}
	var live int
	if err := f.pool.QueryRow(context.Background(), `select count(*) from oauth_refresh_tokens where active and grace_until is null`).Scan(&live); err != nil || live != 1 {
		t.Fatalf("unrotated live refresh tokens = %d (%v), want 1", live, err)
	}
	status, body := f.refresh(t, later, key)
	next, _ := body["refresh_token"].(string)
	if status != http.StatusOK || next == "" {
		t.Fatalf("refresh of the later branch = %d %v, want 200", status, body)
	}
	if status, body := f.refresh(t, earlier, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("refresh of the earlier branch = %d %v, want invalid_grant", status, body)
	}
}

func TestReusingARotatedTokenEndsTheLogin(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	first := f.login(t, key)
	_, body := f.refresh(t, first, key)
	second, _ := body["refresh_token"].(string)
	if second == "" {
		t.Fatalf("refresh answer %v has no refresh_token", body)
	}
	if _, err := f.pool.Exec(context.Background(), `update oauth_refresh_tokens set grace_until = now() - interval '1 second' where grace_until is not null`); err != nil {
		t.Fatalf("end the grace: %v", err)
	}
	if status, body := f.refresh(t, first, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reusing the rotated token = %d %v, want invalid_grant", status, body)
	}
	if status, body := f.refresh(t, second, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("the newest token after a reuse = %d %v, want invalid_grant", status, body)
	}
}

// TestReusingARotatedTokenEndsTheLoginAfterASweep: a family whose newest token is live keeps its
// rotated rows through a sweep, so a replay is still found and still revokes the family.
func TestReusingARotatedTokenEndsTheLoginAfterASweep(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	first := f.login(t, key)
	_, body := f.refresh(t, first, key)
	second, _ := body["refresh_token"].(string)
	if second == "" {
		t.Fatalf("refresh answer %v has no refresh_token", body)
	}
	if _, err := f.pool.Exec(context.Background(), `update oauth_refresh_tokens set grace_until = now() - interval '1 second' where grace_until is not null`); err != nil {
		t.Fatalf("end the grace: %v", err)
	}
	if err := oauthstore.New(f.pool).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if status, body := f.refresh(t, first, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reusing the rotated token after a sweep = %d %v, want invalid_grant", status, body)
	}
	if status, body := f.refresh(t, second, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("the newest token after a reuse = %d %v, want invalid_grant", status, body)
	}
}

func TestBeforeCanRefuse(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	refreshToken := f.login(t, key)
	access, refresh := f.count(t, "oauth_access_tokens"), f.count(t, "oauth_refresh_tokens")
	f.before = fmt.Errorf("the credential is revoked: %w", oauth.ErrLoginEnded)
	if status, body := f.refresh(t, refreshToken, key); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("refused refresh = %d %v, want invalid_grant", status, body)
	}
	if a, r := f.count(t, "oauth_access_tokens"), f.count(t, "oauth_refresh_tokens"); a != access || r != refresh {
		t.Fatalf("after a refused refresh: %d access and %d refresh tokens, want %d and %d", a, r, access, refresh)
	}
	if len(f.grants) != 1 {
		t.Fatalf("After ran for grants %v, want only the login's", f.grants)
	}
}

// TestABeforeFailureIsAServerError: only ErrLoginEnded refuses invalid_grant, which a client
// takes as the login's end; any other Before error (a lost database connection) is a 500
// server_error the client retries, and the refresh token stays redeemable.
func TestABeforeFailureIsAServerError(t *testing.T) {
	f := newFixture(t)
	key := newKey(t)
	refreshToken := f.login(t, key)
	f.before = errors.New("the database is down")
	if status, body := f.refresh(t, refreshToken, key); status != http.StatusInternalServerError || body["error"] != "server_error" {
		t.Fatalf("refresh with a failing Before = %d %v, want 500 server_error", status, body)
	}
	f.before = nil
	if status, body := f.refresh(t, refreshToken, key); status != http.StatusOK {
		t.Fatalf("refresh once Before succeeds = %d %v, want 200", status, body)
	}
	if got := f.grants; len(got) != 2 || got[0] != oauth2.GrantTypeDeviceCode || got[1] != oauth2.GrantTypeRefreshToken {
		t.Fatalf("After saw grants %v, want the device code then the refresh grant", got)
	}
}

// TestNewRefusesAConfigurationItCouldNotServe: every lifetime and the proof skew must be
// positive, the secret long enough, and an issuer named must be named whole.
func TestNewRefusesAConfigurationItCouldNotServe(t *testing.T) {
	secret := make([]byte, 32)
	sound := oauth.Config{
		Secret: secret, DeviceLifetime: time.Minute, AccessLifetime: time.Minute, RefreshLifetime: time.Hour,
		VerificationURL: "https://dispatch.example/credentials/machine", ProofSkew: time.Minute,
		Exchange: oauth.ExchangeConfig{Policy: newPolicy(t, nil, nil)},
	}
	for name, spoil := range map[string]func(*oauth.Config){
		"no device lifetime":          func(c *oauth.Config) { c.DeviceLifetime = 0 },
		"no refresh lifetime":         func(c *oauth.Config) { c.RefreshLifetime = 0 },
		"a negative refresh lifetime": func(c *oauth.Config) { c.RefreshLifetime = -1 },
		"no access lifetime":          func(c *oauth.Config) { c.AccessLifetime = 0 },
		"no proof skew":               func(c *oauth.Config) { c.ProofSkew = 0 },
		"a secret too short":          func(c *oauth.Config) { c.Secret = make([]byte, 31) },
		"an issuer configured in part": func(c *oauth.Config) {
			c.Exchange.Kubernetes = oauth.IssuerConfig{Issuer: "https://kubernetes.example"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := sound
			spoil(&c)
			if _, err := oauth.New(c, nil); err == nil {
				t.Errorf("oauth.New with %s = nil, want an error", name)
			}
		})
	}
}

func TestNormalizeUserCode(t *testing.T) {
	for typed, want := range map[string]string{
		"ABCD-EFGH":   "ABCD-EFGH",
		"abcd-efgh":   "ABCD-EFGH",
		"abcdefgh":    "ABCD-EFGH",
		" ab cd efgh": "ABCD-EFGH",
		"abcd efgh":   "ABCD-EFGH",
		"abc":         "ABC",
	} {
		if got := oauth.NormalizeUserCode(typed); got != want {
			t.Errorf("NormalizeUserCode(%q) = %q, want %q", typed, got, want)
		}
	}
}

func TestNewUserCodeIsTheBrokersFormat(t *testing.T) {
	for range 50 {
		code, err := oauth.NewUserCode()
		if err != nil {
			t.Fatal(err)
		}
		if !userCodePattern.MatchString(code) || strings.ContainsAny(code, "01IO") {
			t.Fatalf("NewUserCode() = %q, want XXXX-XXXX with no 0, 1, I or O", code)
		}
	}
}
