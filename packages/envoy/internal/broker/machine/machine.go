// Package machine implements AGENTC-393 Plan A machine logins: a typed-code approval flow that
// mints key-bound launcher credentials. A machine (an operator's box, a Kubernetes pod, or an
// automated service like the Legion daemon) signs a credential-request object naming the
// operator it logs in as (login_hint) and a single launcher_credential authorization detail, and
// polls Login's pendingID for a human to approve the confirmation code Login also mints.
// Machine-login records are decided here, not in requests.Machine: ApplyDecision takes the
// deciding human's Dispatch login and the typed code and, on approval, mints the credential
// directly — there is no Dispatch ask anywhere in this flow, and no bearer token in any response.
package machine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/rules"
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
	// ErrAlreadyDecided is ApplyDecision's refusal for a record that is no longer pending: it
	// already carries its one terminal event (a second approve or deny, one that lost the race to a
	// concurrent one, or the sweeper's 'expired'), or its expires_at has passed.
	ErrAlreadyDecided = errors.New("this machine login has already been decided")
	// ErrKeyHoldsLiveCredential is ApplyDecision's refusal to approve a pending login whose key
	// already holds a live launcher credential under another record: a machine signed two logins
	// with one key and the first was approved. The record stays pending.
	ErrKeyHoldsLiveCredential = errors.New("this machine login's key already holds a live launcher credential")
)

type Service struct {
	Store  *store.Store
	Enroll *enroll.Service
	Rules  *rules.Current

	Audience           string
	Skew               time.Duration
	PendingTTL         time.Duration
	CredentialLifetime time.Duration

	// Replay records a request object's jti (the proof_jtis table), answering false when it was
	// already seen. Wired to enroll.Service.Replay in production, exactly like
	// requests.Machine.Replay: without it a captured signed machine-login request object could be
	// resubmitted repeatedly within its freshness window, each call minting a fresh pending record
	// and confirmation code — a confirmation-fatigue/notification-spam vector against the named
	// operator.
	Replay func(ctx context.Context, jti string, expires time.Time) (fresh bool, err error)

	// testDecisionHook, when set, runs inside ApplyDecision once the record's row lock is held and
	// the record found pending, before anything is minted or recorded. It exists only so a test
	// can run the sweeper's 'expired' insert while a decision holds that lock; no production
	// caller sets it.
	testDecisionHook func()
}

// jtiRetentionMargin is how long past a request object's expiry its jti is remembered, mirroring
// proof.Verifier's and requests.Machine's own retention margin.
const jtiRetentionMargin = time.Minute

// confirmationAlphabet has 32 symbols, none easily confused with another (no 0/O, no 1/I), so a
// random byte maps onto it without bias. Moved here verbatim from the deleted launcher package.
const confirmationAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// confirmationCode is eight random symbols from confirmationAlphabet as XXXX-XXXX.
func confirmationCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := make([]byte, 0, 9)
	for i, b := range raw {
		if i == 4 {
			code = append(code, '-')
		}
		code = append(code, confirmationAlphabet[int(b)%len(confirmationAlphabet)])
	}
	return string(code), nil
}

// hashPendingID is the sha256 of a pending id — machine_login_polls' primary key, so the raw
// capability a machine polls with is never itself stored. Moved here verbatim from the deleted
// launcher package.
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

// Login verifies a signed machine credential-request object and opens a pending record for a
// human to approve or deny: login_hint is required (it names the operator who must decide it)
// and authorization_details must carry exactly one launcher_credential entry.
// Nothing here touches Dispatch; the record and its poll row are the whole state, and rate
// limiting this unauthenticated route is the api layer's job, not this one's.
func (s *Service) Login(ctx context.Context, compactRequest string) (pendingID, code string, err error) {
	now := time.Now()
	obj, err := record.VerifyRequestObject(compactRequest, s.Audience, s.Skew, now)
	if err != nil {
		return "", "", err
	}
	if obj.LoginHint == "" {
		return "", "", fmt.Errorf("%w: login_hint is required for a machine login", record.ErrRequestInvalid)
	}
	if len(obj.Details) != 1 || obj.Details[0].Type != "launcher_credential" {
		return "", "", fmt.Errorf("%w: exactly one launcher_credential detail is required", record.ErrRequestInvalid)
	}
	fresh, err := s.Replay(ctx, obj.JTI, obj.Expires.Add(s.Skew+jtiRetentionMargin))
	if err != nil {
		return "", "", err
	}
	if !fresh {
		return "", "", fmt.Errorf("%w: replayed jti", record.ErrRequestInvalid)
	}

	code, err = confirmationCode()
	if err != nil {
		return "", "", err
	}
	pendingID, err = randomPendingID()
	if err != nil {
		return "", "", err
	}

	body := record.Body{
		Request:         compactRequest,
		Approver:        record.CanonicalLogin(obj.LoginHint),
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-", Operator: ""},
		LifetimeSeconds: int(s.CredentialLifetime.Seconds()),
		RulesVersion:    s.Rules.Get().Version,
		ExpiresAt:       now.Add(s.PendingTTL).UTC().Truncate(time.Second),
		Code:            code,
	}
	recordID := body.ID()

	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into credential_requests (id, body, kind, approver, code, expires_at) values ($1,$2,'launcher_credential',$3,$4,$5)`,
		recordID, body.Canonical(), body.Approver, body.Code, body.ExpiresAt); err != nil {
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

// ApplyDecision decides a pending machine login. code must match the record's own — a wrong code
// means the human is looking at a different login than the one they're deciding, refused before
// anything else is checked (CODE_MISMATCH). login, the deciding human's Dispatch login, must be
// the record's own approver (record.ErrNotApprover), checked next, so another login is refused the
// same way whatever the record's state. A record that already carries a terminal event, or whose
// expires_at has passed though the sweeper has not yet recorded it expired, is ErrAlreadyDecided,
// checked under the record's row lock before anything is minted, so a second decision, one racing
// the first and one after expiry all answer the same way. Approval mints the
// credential — bound to the request object's own key (thumbprint and embedded JWK), with lifetime
// CredentialLifetime counted from the decision — in the same transaction that records the
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
	// this decision's own insert then answers ErrAlreadyDecided.
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
	if code != storedCode {
		return "", "", ErrCodeMismatch
	}

	body, err := record.VerifyBodyReproducesID(canonical, recordID)
	if err != nil {
		if errors.Is(err, record.ErrBodyIDMismatch) {
			return "", "", fmt.Errorf("%w: stored body does not reproduce its own id", record.ErrRequestInvalid)
		}
		return "", "", err
	}

	login, err = body.ApproverLogin(login)
	if err != nil {
		return "", "", err
	}
	var decided bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from credential_request_events where record_id=$1 and event = any($2))`, recordID, record.TerminalEventNames).
		Scan(&decided); err != nil {
		return "", "", err
	}
	if decided || expired {
		return "", "", ErrAlreadyDecided
	}
	if s.testDecisionHook != nil {
		s.testDecisionHook()
	}
	event := "denied"
	if approve {
		event = "approved"
	}

	var credID *string
	if approve {
		obj, err := record.VerifyRequestObject(body.Request, s.Audience, s.Skew, createdAt)
		if err != nil {
			return "", "", err
		}
		jwk, err := embeddedJWK(body.Request)
		if err != nil {
			return "", "", err
		}
		detail := obj.Details[0]
		var operator, service *string
		if detail.Service != "" {
			service = &detail.Service
		} else {
			op := login
			operator = &op
		}
		id, err := s.Enroll.MintLauncherCredentialTx(ctx, tx, operator, service, detail.Identifier, obj.Thumbprint, jwk, recordID,
			time.Now().Add(time.Duration(body.LifetimeSeconds)*time.Second))
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
		recordID, event, login, credID, "human:"+login); store.IsUniqueViolation(err) {
		return "", "", ErrAlreadyDecided
	} else if err != nil {
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

// Read answers a machine's own poll: the record's current state and, once issued, its minted
// credential's id — never a token.
func (s *Service) Read(ctx context.Context, pendingID string) (state, credentialID string, err error) {
	var recordID string
	err = s.Store.Pool.QueryRow(ctx, `select record_id from machine_login_polls where pending_id_hash=$1`, hashPendingID(pendingID)).Scan(&recordID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	return s.recordState(ctx, recordID)
}

// recordState answers a record's current state ("pending" absent any terminal event, else the
// event's own name) and, once issued, its minted credential's id.
func (s *Service) recordState(ctx context.Context, recordID string) (state, credentialID string, err error) {
	var event string
	var credID *string
	err = s.Store.Pool.QueryRow(ctx, `select event, credential_id from credential_request_events where record_id=$1 and event = any($2)`, recordID, record.TerminalEventNames).
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
		where code=$1 and kind='launcher_credential' order by created_at desc limit 1`, code).
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
// not record.TerminalEventNames: Postgres infers a partial index only from a predicate it can
// prove implies the index's, and a bound parameter proves nothing.
func (s *Service) ExpirePending(ctx context.Context, now time.Time) error {
	_, err := s.Store.Pool.Exec(ctx, `insert into credential_request_events (record_id, event, actor)
		select id, 'expired', 'broker' from credential_requests
		where kind='launcher_credential' and expires_at < $1
		on conflict (record_id) where event in ('approved','denied','expired','cancelled') do nothing`, now)
	return err
}
