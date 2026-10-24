// Package machine implements machine logins. A person's machine (an operator's box) logs in
// through a typed-code approval flow that mints a key-bound launcher credential: it signs a
// credential-request object carrying a single launcher_credential authorization detail, naming the
// operator it logs in as in its login_hint, and polls Login's pendingID for that person to approve
// the confirmation code Login also mints. Machine-login records are decided here, not in
// requests.Machine: ApplyDecision takes the deciding human's Dispatch login and the typed code and,
// on approval, mints the credential directly — there is no Dispatch ask anywhere in this flow, and
// no bearer token in any response. A service (the Legion daemon, whose pods it enrolls) logs in
// with its own projected Kubernetes ServiceAccount token instead (TokenLogin), and no person
// approves it.
package machine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
)

var (
	// ErrNotFound is returned by Read and ApplyDecision when no record matches the given
	// pending id / record id.
	ErrNotFound = errors.New("machine login not found")
	// ErrCodeMismatch is ApplyDecision's refusal when the caller's code does not match the
	// record's own: the human is deciding a different login than the one their terminal or
	// dashboard actually shows, refused before the approver is even checked.
	ErrCodeMismatch = errors.New("confirmation code does not match")
	// ErrAlreadyDecided is ApplyDecision's refusal for a record that already carries its one
	// decision: a second approve or deny, or one that lost the race to a concurrent one.
	ErrAlreadyDecided = errors.New("this machine login has already been decided")
	// ErrLoginExpired is ApplyDecision's refusal for a login that expired undecided: the sweeper
	// recorded it expired, or its expires_at has passed before the sweeper got to it.
	ErrLoginExpired = errors.New("this machine login expired before its approver acted on it")
	// ErrKeyHoldsLiveCredential is ApplyDecision's refusal to approve a pending login whose key
	// already holds a live launcher credential under another record: a machine signed two logins
	// with one key and the first was approved. The record stays pending.
	ErrKeyHoldsLiveCredential = errors.New("this machine login's key already holds a live launcher credential")
	// ErrNotLive is Refreshed's and Live's refusal for a launcher credential that is revoked or
	// past its expiry: a refresh never revives a login that has ended. It is an
	// oauth.ErrLoginEnded, so the token endpoint refuses it invalid_grant.
	ErrNotLive = fmt.Errorf("%w: this machine login's credential is revoked or expired", oauth.ErrLoginEnded)
)

type Service struct {
	Store  *store.Store
	Enroll *enroll.Service
	// Policy is the secret policy, whose version a machine-login record carries.
	Policy *policy.Current

	Audience   string
	Skew       time.Duration
	PendingTTL time.Duration
	// CredentialLifetime is how long a minted credential lasts, and how far each refresh of its
	// device login moves its expiry.
	CredentialLifetime time.Duration

	// OAuth is the device grant a person's machine logs in through; nil when the broker runs none.
	// ApplyDecision records each decision on it, so the machine's token poll learns the outcome.
	OAuth *oauth.Provider

	// Replay records a request object's jti (the proof_jtis table), answering false when it was
	// already seen, in the transaction ctx carries (oauthstore.TxFrom) and on the pool when it
	// carries none. Wired to enroll.Service.Replay in production, exactly like
	// requests.Machine.Replay: without it a captured signed machine-login request object could be
	// resubmitted repeatedly within its freshness window, each call minting a fresh pending record
	// and confirmation code — a confirmation-fatigue/notification-spam vector against the named
	// operator.
	Replay func(ctx context.Context, jti string, expires time.Time) (fresh bool, err error)
}

// jtiRetentionMargin is how long past a request object's expiry its jti is remembered, mirroring
// proof.Verifier's and requests.Machine's own retention margin.
const jtiRetentionMargin = time.Minute

// hashPendingID is the sha256 of a pending id — machine_login_polls' primary key, so the raw
// capability a machine polls with is never itself stored.
func hashPendingID(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// randomPendingID mints the opaque capability a machine polls Read with.
func randomPendingID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// embeddedJWK extracts the raw JWK embedded in a request object's JWS header. It is called only
// after record.VerifyRequestObject already checked that JWS's signature and structure, so
// ApplyDecision can store the exact public key material a launcher credential is bound to.
func embeddedJWK(compact string) (json.RawMessage, error) {
	sig, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(sig.Signatures) != 1 || sig.Signatures[0].Protected.JSONWebKey == nil {
		return nil, fmt.Errorf("%w: no embedded key", record.ErrRequestInvalid)
	}
	return sig.Signatures[0].Protected.JSONWebKey.MarshalJSON()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// verifyLauncherRequest verifies a machine request object for a launcher credential at now: its
// signature and freshness (record.VerifyRequestObject), exactly one launcher_credential detail,
// then the caller's own check, then its jti, which it records, so a replayed request is refused.
// Every refusal but check's own is record.ErrRequestInvalid.
func (s *Service) verifyLauncherRequest(ctx context.Context, compact string, now time.Time, check func(record.RequestObject) error) (record.RequestObject, error) {
	obj, err := record.VerifyRequestObject(compact, s.Audience, s.Skew, now)
	if err != nil {
		return record.RequestObject{}, err
	}
	if len(obj.Details) != 1 || obj.Details[0].Type != record.KindLauncherCredential {
		return record.RequestObject{}, fmt.Errorf("%w: exactly one launcher_credential detail is required", record.ErrRequestInvalid)
	}
	if err := check(obj); err != nil {
		return record.RequestObject{}, err
	}
	fresh, err := s.Replay(ctx, obj.JTI, obj.Expires.Add(s.Skew+jtiRetentionMargin))
	if err != nil {
		return record.RequestObject{}, err
	}
	if !fresh {
		return record.RequestObject{}, fmt.Errorf("%w: replayed jti", record.ErrRequestInvalid)
	}
	return obj, nil
}

// Login verifies a signed machine credential-request object and opens a pending record for a
// human to approve or deny. authorization_details must carry exactly one launcher_credential
// entry, and it must name no service (ErrServiceLogsInWithToken): a service logs in with its own
// Kubernetes token (TokenLogin), never through a code a person approves. A person's machine login
// must carry a login_hint naming the person who decides it and whose machine it becomes, so never
// record.AnyoneApprover.
// Nothing here touches Dispatch; the record and its poll row are the whole state, and rate
// limiting this unauthenticated route is the api layer's job, not this one's.
func (s *Service) Login(ctx context.Context, compactRequest string) (pendingID, code string, err error) {
	now := time.Now()
	approver, err := s.verifyPersonLogin(ctx, compactRequest, now, nil)
	if err != nil {
		return "", "", err
	}
	code, err = oauth.NewUserCode()
	if err != nil {
		return "", "", err
	}
	pendingID, err = randomPendingID()
	if err != nil {
		return "", "", err
	}

	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	recordID, err := s.insertRecord(ctx, tx, compactRequest, approver, code, min(s.CredentialLifetime, EarlierRouteLifetime), now)
	if err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `insert into machine_login_polls (pending_id_hash, record_id) values ($1,$2)`, hashPendingID(pendingID), recordID); err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return pendingID, code, nil
}

// LoginDevice opens a person's machine login as an OAuth device authorization: the provider's
// onRequest, run in the transaction ctx carries (oauthstore.TxFrom) with the device request's form
// and the user code the provider minted. form's request is the signed request object Login takes,
// verified the same way with the same refusals (its jti recorded, so a replay is refused), and its
// dpop_jkt must be that object's key thumbprint, so the tokens the login is issued are bound to the
// key its credential is (record.ErrRequestInvalid otherwise). The jti, like the record, is written
// in that transaction (Replay reads it from ctx), so a login holds the one pooled connection its
// transaction took and never waits for another, and a refused login leaves no jti behind. The
// record is Login's, its code the user code, and there is no poll row: the machine polls with its
// device code.
func (s *Service) LoginDevice(ctx context.Context, form url.Values, userCode string) error {
	tx, ok := oauthstore.TxFrom(ctx)
	if !ok {
		return errors.New("machine: LoginDevice runs in the provider's transaction")
	}
	now := time.Now()
	compact := form.Get("request")
	approver, err := s.verifyPersonLogin(ctx, compact, now, func(obj record.RequestObject) error {
		if form.Get("dpop_jkt") != obj.Thumbprint {
			return fmt.Errorf("%w: dpop_jkt is not the request object's key thumbprint", record.ErrRequestInvalid)
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = s.insertRecord(ctx, tx, compact, approver, userCode, s.CredentialLifetime, now)
	return err
}

// verifyPersonLogin verifies a person's machine login request object at now (verifyLauncherRequest)
// and answers the approver its login_hint names. A request naming a service is
// ErrServiceLogsInWithToken, and one naming no person, or record.AnyoneApprover, is
// record.ErrRequestInvalid; check, when set, runs after those and before the jti is recorded.
func (s *Service) verifyPersonLogin(ctx context.Context, compact string, now time.Time, check func(record.RequestObject) error) (approver string, err error) {
	_, err = s.verifyLauncherRequest(ctx, compact, now, func(obj record.RequestObject) error {
		if obj.Service() != "" {
			return ErrServiceLogsInWithToken
		}
		approver = record.CanonicalLogin(obj.LoginHint)
		if approver == "" {
			return fmt.Errorf("%w: login_hint is required for a person's machine login", record.ErrRequestInvalid)
		}
		if approver == record.AnyoneApprover {
			return fmt.Errorf("%w: a person's machine login's login_hint names its operator, never %q", record.ErrRequestInvalid, record.AnyoneApprover)
		}
		if check != nil {
			return check(obj)
		}
		return nil
	})
	return approver, err
}

// EarlierRouteLifetime bounds the credential an approval mints from a record Login opened (POST
// /v1/launcher-credentials): a helper that logged in that way cannot refresh, so its credential
// keeps the seven days it always had, or CredentialLifetime when that is shorter.
const EarlierRouteLifetime = 7 * 24 * time.Hour

// insertRecord writes a person's machine login record in tx: the request object, its approver,
// lifetime (the credential lifetime an approval mints), the policy version, code, and an expiry
// PendingTTL after now. It answers the record's id.
func (s *Service) insertRecord(ctx context.Context, tx pgx.Tx, compact, approver, code string, lifetime time.Duration, now time.Time) (string, error) {
	body := record.Body{
		Request:         compact,
		Approver:        approver,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-", Operator: ""},
		LifetimeSeconds: new(int(lifetime.Seconds())),
		RulesVersion:    s.Policy.Get().Version,
		ExpiresAt:       now.Add(s.PendingTTL).UTC().Truncate(time.Second),
		Code:            code,
	}
	recordID := body.ID()
	if _, err := tx.Exec(ctx, `insert into credential_requests (id, body, kind, approver, code, expires_at) values ($1,$2,'launcher_credential',$3,$4,$5)`,
		recordID, body.Canonical(), body.Approver, body.Code, body.ExpiresAt); err != nil {
		return "", err
	}
	return recordID, nil
}

// ApplyDecision decides a pending machine login. code must match the record's own — a wrong code
// means the human is looking at a different login than the one they're deciding, refused before
// anything else is checked (CODE_MISMATCH). login, the deciding human's Dispatch login, must be one
// the record's rule admits (record.ErrNotApprover), checked next against the request object its
// record embeds, re-verified as of the record's creation: a person's machine login is decided by
// that person alone, and a service's login, which a broker before TokenLogin opened, is approved by
// no one (a service logs in with its token now), though it may still be denied. So another login is
// refused the same way whatever the record's state. A record that already carries a decision is
// ErrAlreadyDecided, and one that expired undecided — recorded expired by the sweeper, or past its
// expires_at before the sweeper got to it — is ErrLoginExpired, both checked under the record's
// row lock before anything is minted, so a second decision, one racing the first and one after
// expiry all answer the same way. Approval mints the
// credential — bound to the request object's own key (thumbprint and embedded JWK), with the lifetime
// its record names (insertRecord) counted from the decision — in the same transaction that records the
// decision, so a crash between the two never orphans a credential no decision names. A key that
// already holds a live credential under another record is ErrKeyHoldsLiveCredential, and the
// record stays pending.
func (s *Service) ApplyDecision(ctx context.Context, recordID string, approve bool, login, code string) (state, credentialID string, err error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)

	// The row lock serializes every decision of this record; a lock fires no update trigger, so
	// the append-only record allows it. It is `for no key update`, not `for update`: the foreign
	// key check of another transaction's event insert (the sweeper's 'expired') takes `for key
	// share` on this row, which `for update` would block while this transaction then waited on
	// that insert's unique-index entry, a deadlock; the weaker lock lets that insert commit, and
	// this decision's own insert then hits that same unique index. Decisions of one record
	// serialize on this lock, so the only other writer that can win that index while this
	// transaction holds it is the sweeper's ExpirePending — a concurrent decision never reaches
	// the insert at all; it queues on this same lock and reads the sweeper's event from the
	// switch below instead. So that collision always means the login expired while this decision
	// was in flight, and the insert answers ErrLoginExpired, never ErrAlreadyDecided.
	var canonical, storedCode string
	var createdAt time.Time
	var expired bool
	err = tx.QueryRow(ctx, `select body, code, created_at, expires_at <= now() from credential_requests where id=$1 and kind='launcher_credential' for no key update`, recordID).
		Scan(&canonical, &storedCode, &createdAt, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if oauth.NormalizeUserCode(code) != storedCode {
		return "", "", ErrCodeMismatch
	}

	body, err := record.VerifyBodyReproducesID(canonical, recordID)
	if err != nil {
		if errors.Is(err, record.ErrBodyIDMismatch) {
			return "", "", fmt.Errorf("%w: stored body does not reproduce its own id", record.ErrRequestInvalid)
		}
		return "", "", err
	}

	// now is the record's own creation time: the request object's iat and exp bound how fresh it
	// had to be when Login accepted it, never how long the login may wait for its decision.
	obj, err := record.VerifyRequestObject(body.Request, s.Audience, s.Skew, createdAt)
	if err != nil {
		return "", "", err
	}
	login, err = body.ApproverLogin(record.KindLauncherCredential, obj.Service(), login)
	if err != nil {
		return "", "", err
	}
	var decided string
	if err := tx.QueryRow(ctx, `select coalesce((select event from credential_request_events where record_id=$1 and event = any($2)), '')`, recordID, record.TerminalEventNames()).
		Scan(&decided); err != nil {
		return "", "", err
	}
	switch {
	case decided == "expired" || decided == "" && expired:
		return "", "", ErrLoginExpired
	case decided != "":
		return "", "", ErrAlreadyDecided
	}
	event := "denied"
	if approve {
		event = "approved"
	}

	var credID *string
	if approve {
		jwk, err := embeddedJWK(body.Request)
		if err != nil {
			return "", "", err
		}
		detail := obj.Details[0]
		if detail.Service != "" {
			return "", "", ErrServiceLogsInWithToken
		}
		if body.LifetimeSeconds == nil {
			return "", "", fmt.Errorf("machine login record has no credential lifetime")
		}
		id, err := s.Enroll.MintLauncherCredentialTx(ctx, tx, login, detail.Identifier, obj.Thumbprint, jwk, recordID,
			time.Now().Add(time.Duration(*body.LifetimeSeconds)*time.Second))
		if store.IsUniqueViolation(err) {
			return "", "", ErrKeyHoldsLiveCredential
		}
		if err != nil {
			return "", "", err
		}
		idStr := id.String()
		credID = &idStr
	}

	if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, login, credential_id, actor) values ($1,$2,$3,$4,$5)`,
		recordID, event, login, credID, record.HumanActor(login)); store.IsUniqueViolation(err) {
		// Only ExpirePending can win this race: see the row-lock comment above.
		return "", "", ErrLoginExpired
	} else if err != nil {
		return "", "", err
	}
	if err := s.decideDevice(ctx, tx, body.Code, approve, recordID, credID, login, obj.Details[0].Identifier); err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	if approve {
		return "issued", *credID, nil
	}
	return "denied", "", nil
}

// decideDevice records a decision on the device login whose user code is code, in tx: an
// approval's session names the record, the minted credential, the operator and the host. A record
// Login opened has no device code (oauth2.ErrNotFound), so there is nothing to record; a device
// code past its lifetime (oauth2.ErrInvalidGrant) is ErrLoginExpired, and the decision rolls back.
// A broker with no OAuth provider records nothing.
func (s *Service) decideDevice(ctx context.Context, tx pgx.Tx, code string, approve bool, recordID string, credID *string, login, host string) error {
	if s.OAuth == nil {
		return nil
	}
	extra := map[string]any{"record_id": recordID, "operator": login, "host": host}
	if credID != nil {
		extra[oauthwire.ExtraCredentialID] = *credID
	}
	err := s.OAuth.ApproveDevice(oauthstore.WithTx(ctx, tx), code, approve, extra)
	switch {
	case errors.Is(err, oauth2.ErrNotFound):
		return nil
	case errors.Is(err, oauth2.ErrInvalidGrant):
		return ErrLoginExpired
	}
	return err
}

// Refreshed moves a live launcher credential's expiry to CredentialLifetime from now, as its
// device login refreshes, and answers the new expiry. A credential revoked or already past its
// expiry is ErrNotLive and is left as it is. Expiry is compared with clock_timestamp(), the
// statement's own moment, not now(), the transaction's start, so a retire (expires_at set to its
// now) that commits after the token request's transaction began still ends the login for it. It
// runs only in the transaction ctx carries (oauthstore.TxFrom), and its update holds the
// credential's row lock until that transaction ends: a revoke (enroll.Service.RevokeCredential
// locks the same row) either commits first, so the update finds no live row, or waits for the
// refresh's tokens to commit and then ends them.
func (s *Service) Refreshed(ctx context.Context, credentialID string) (time.Time, error) {
	tx, err := tokenTx(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var expires time.Time
	err = tx.QueryRow(ctx, `update launcher_credentials set expires_at = now() + make_interval(secs => $2)
		where id = $1 and revoked_at is null and expires_at > clock_timestamp() returning expires_at`, credentialID, s.CredentialLifetime.Seconds()).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotLive
	}
	return expires, err
}

// Live holds a launcher credential's row lock until the transaction ctx carries
// (oauthstore.TxFrom) ends, leaving its expiry as it is; a credential revoked or past its expiry
// (against clock_timestamp(), as in Refreshed) is ErrNotLive. A revoke
// (enroll.Service.RevokeCredential locks the same row) either commits first, so Live finds no live
// row, or waits for the device code grant's tokens to commit and then ends them.
func (s *Service) Live(ctx context.Context, credentialID string) error {
	tx, err := tokenTx(ctx)
	if err != nil {
		return err
	}
	var one int
	err = tx.QueryRow(ctx, `select 1 from launcher_credentials
		where id = $1 and revoked_at is null and expires_at > clock_timestamp() for no key update`, credentialID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotLive
	}
	return err
}

// tokenTx is the token request's transaction ctx carries (oauthstore.TxFrom): the row locks the
// token hooks take hold only within it, so there is no running without one.
func tokenTx(ctx context.Context) (pgx.Tx, error) {
	tx, ok := oauthstore.TxFrom(ctx)
	if !ok {
		return nil, errors.New("machine: the token hooks run in the provider's transaction")
	}
	return tx, nil
}

// TokenHooks are the device login's token hooks, run in the token request's transaction: a
// refresh moves its credential's expiry first (Refreshed), the device code grant locks the
// credential's row as it is (Live), and either is refused when the credential has ended; every
// answer carries credential_id and credential_expires_at (RFC 3339) as the credential's row holds
// it in that transaction: the minted expiry on the device code grant, the one Refreshed moved it
// to on a refresh.
func (s *Service) TokenHooks() oauth.TokenHooks {
	var hooks oauth.TokenHooks
	hooks.Before = func(ctx context.Context, grant oauth2.GrantType, extra map[string]any) error {
		id, err := credentialOf(extra)
		if err != nil {
			return err
		}
		switch grant {
		case oauth2.GrantTypeRefreshToken:
			_, err = s.Refreshed(ctx, id)
			return err
		case oauth2.GrantTypeDeviceCode:
			return s.Live(ctx, id)
		default:
			return fmt.Errorf("machine: a device login has no %q grant", grant)
		}
	}
	hooks.After = func(ctx context.Context, _ oauth2.GrantType, extra map[string]any) (map[string]any, error) {
		id, err := credentialOf(extra)
		if err != nil {
			return nil, err
		}
		tx, err := tokenTx(ctx)
		if err != nil {
			return nil, err
		}
		var expires time.Time
		if err := tx.QueryRow(ctx, `select expires_at from launcher_credentials where id=$1`, id).Scan(&expires); err != nil {
			return nil, err
		}
		return map[string]any{oauthwire.ExtraCredentialID: id, oauthwire.ExtraCredentialExpiresAt: expires.UTC().Format(time.RFC3339)}, nil
	}
	return hooks
}

// credentialOf is the launcher credential a device login's session extra names. A session naming
// none has no login to refresh, so it is an oauth.ErrLoginEnded.
func credentialOf(extra map[string]any) (string, error) {
	id, ok := extra[oauthwire.ExtraCredentialID].(string)
	if !ok || id == "" {
		return "", fmt.Errorf("%w: the login's session names no credential", oauth.ErrLoginEnded)
	}
	return id, nil
}

// Read answers a machine's own poll: the record's current state and, once issued, its minted
// credential's id and the moment that credential expires (CredentialLifetime from the approval),
// so the machine can say ahead of time that it needs a new login — never a token.
func (s *Service) Read(ctx context.Context, pendingID string) (state, credentialID string, expiresAt time.Time, err error) {
	var recordID string
	err = s.Store.Pool.QueryRow(ctx, `select record_id from machine_login_polls where pending_id_hash=$1`, hashPendingID(pendingID)).Scan(&recordID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", "", time.Time{}, err
	}
	state, credentialID, err = s.recordState(ctx, recordID)
	if err != nil || state != "issued" {
		return state, credentialID, time.Time{}, err
	}
	if err := s.Store.Pool.QueryRow(ctx, `select expires_at from launcher_credentials where id=$1`, credentialID).Scan(&expiresAt); err != nil {
		return "", "", time.Time{}, err
	}
	return state, credentialID, expiresAt, nil
}

// recordState answers a record's current state ("pending" absent any terminal event, else the
// event's own name) and, once issued, its minted credential's id.
func (s *Service) recordState(ctx context.Context, recordID string) (state, credentialID string, err error) {
	var event string
	var credID *string
	err = s.Store.Pool.QueryRow(ctx, `select event, credential_id from credential_request_events where record_id=$1 and event = any($2)`, recordID, record.TerminalEventNames()).
		Scan(&event, &credID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "pending", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if event == "approved" {
		return "issued", deref(credID), nil
	}
	return event, "", nil
}

// RecordView is a machine login record as the operator's UI sees it, resolved by its
// human-readable confirmation code: enough to show what is being decided.
type RecordView struct {
	RecordID     string
	Host         string
	Service      string // "" for a personal (non-service) login
	Approver     string
	State        string
	CredentialID string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// LookupByCode resolves a pending machine login by its confirmation code, for the operator's own
// UI: it never needs the machine's opaque pending id, only the code its terminal or dashboard
// shows.
func (s *Service) LookupByCode(ctx context.Context, code string) (RecordView, error) {
	var id, canonical, approver string
	var createdAt, expiresAt time.Time
	err := s.Store.Pool.QueryRow(ctx, `select id, body, approver, created_at, expires_at from credential_requests
		where code=$1 and kind='launcher_credential' order by created_at desc limit 1`, oauth.NormalizeUserCode(code)).
		Scan(&id, &canonical, &approver, &createdAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecordView{}, ErrNotFound
	}
	if err != nil {
		return RecordView{}, err
	}
	body, err := record.ParseBody(canonical)
	if err != nil {
		return RecordView{}, err
	}
	obj, err := record.VerifyRequestObject(body.Request, s.Audience, s.Skew, createdAt)
	if err != nil {
		return RecordView{}, err
	}
	detail := obj.Details[0]
	state, credentialID, err := s.recordState(ctx, id)
	if err != nil {
		return RecordView{}, err
	}
	return RecordView{
		RecordID: id, Host: detail.Identifier, Service: detail.Service, Approver: approver,
		State: state, CredentialID: credentialID, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}, nil
}

// ExpirePending writes an 'expired' event for every launcher_credential record whose own
// expires_at has passed now and that has no terminal event yet. The insert's own ON CONFLICT
// target is credential_request_events' partial unique index on (record_id) where event names a
// decision, so a record a concurrent ApplyDecision just decided is silently left alone rather
// than raising a constraint violation. That target spells the index's own predicate literally,
// not record.TerminalEventNames(): Postgres infers a partial index only from a predicate it can
// prove implies the index's, and a bound parameter proves nothing.
func (s *Service) ExpirePending(ctx context.Context, now time.Time) error {
	_, err := s.Store.Pool.Exec(ctx, `insert into credential_request_events (record_id, event, actor)
		select id, 'expired', 'broker' from credential_requests
		where kind='launcher_credential' and expires_at < $1
		on conflict (record_id) where event in ('approved','denied','expired','cancelled') do nothing`, now)
	return err
}
