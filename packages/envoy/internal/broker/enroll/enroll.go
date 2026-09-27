// Package enroll issues key-bound launcher credentials and turns them into live enrollments. A
// launcher (an operator's box, a Kubernetes pod, or the host agent-secrets-helper of Plan B)
// signs its own future requests with the private key whose thumbprint and public JWK a launcher
// credential is minted against — machine.Service.ApplyDecision mints one once a human approves a
// typed-code machine login (AGENTC-393 Plan A) — and, for a pod, also proves a projected
// service-account token bound to that pod, receiving a leased enrollment keyed by its own signing
// key's thumbprint. proof.Verifier reads enrollments back through Lookup and Replay, and launcher
// credentials back through AuthenticateLauncher (its LookupLauncher hook), to authenticate later
// session and machine proofs. There is no bearer token anywhere in this package: a launcher
// credential authenticates by the same key it was minted against, never a shared secret.
package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
)

var (
	ErrUnauthenticated  = errors.New("launcher credential is not valid")
	ErrOperatorMismatch = errors.New("this launcher credential enrols a different operator")
	ErrAlreadyEnrolled  = errors.New("this runtime is already enrolled and live")
	ErrPodIdentity      = errors.New("the projected token does not identify this pod")
	ErrNotLive          = errors.New("enrollment is not live")
)

type Credential struct {
	ID       uuid.UUID
	Operator *string
	Service  *string
	Host     string
}

type Enrollment struct {
	ID         uuid.UUID
	Kind       string
	RuntimeID  string
	Operator   *string
	Thumbprint string
	SessionID  *string
	PodToken   string
	// Subject is a pod enrollment's verified service-account subject, set by Create from the
	// projected token itself and never from the caller; nil for box and host.
	Subject      *string
	LeaseExpires time.Time
	Existing     bool // true when Create returned an already-live enrollment for the same key
}

type PodClaims struct {
	Subject string
	PodUID  string
}

type PodVerifier interface {
	Verify(ctx context.Context, raw string) (PodClaims, error)
}

type Service struct {
	Store *store.Store
	Lease time.Duration
	Pod   PodVerifier

	// Chain re-verifies a launcher credential's issuance chain on every AuthenticateLauncher
	// call: a launcher_credentials row is never trusted on its own, since it must still trace
	// back to a genuinely signed, human-approved credential-request record. NewChainVerifier
	// builds one against this same Store and an approvers.Service.
	Chain *record.ChainVerifier

	// testConflictHook, when set, runs once a 23505 insert conflict is detected in createAttempt,
	// before the recovery lookup. It exists only so a test can deterministically reproduce the
	// race where the conflicting row is revoked between the failed insert and that lookup; no
	// production caller sets it.
	testConflictHook func()
}

// execer is satisfied by both *pgxpool.Pool (via Store.Pool) and pgx.Tx, so mintLauncherCredential
// can run either standalone or joined to a caller's own transaction —
// machine.Service.ApplyDecision's own approval commit, so a crash between minting and recording
// its decision rolls back both together instead of leaving an orphaned, unrecoverable credential.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// mintLauncherCredential inserts a launcher credential bound to a signing key — never a bearer
// token: a launcher's own key (thumbprint, embedded JWK) is exactly what a later
// proof.SignLauncher proof presents, and exactly what AuthenticateLauncher's issuance-chain
// re-verification checks against recordID's approved credential-request record. recordID may be
// "" only for a credential with no backing record at all (test fixtures unrelated to the
// machine-login flow); AuthenticateLauncher refuses such a credential outright.
func (s *Service) mintLauncherCredential(ctx context.Context, exec execer, operator, service *string, host, thumbprint string, jwk json.RawMessage, recordID string, expires time.Time) (uuid.UUID, error) {
	if operator != nil {
		operator = new(record.CanonicalLogin(*operator))
	}
	id := uuid.New()
	_, err := exec.Exec(ctx, `insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, expires_at) values ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, operator, service, host, thumbprint, []byte(jwk), nullable(recordID), expires)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// MintLauncherCredentialTx mints a launcher credential inside tx, joined to the caller's own
// transaction — machine.Service.ApplyDecision's own approval commit.
func (s *Service) MintLauncherCredentialTx(ctx context.Context, tx pgx.Tx, operator, service *string, host, thumbprint string, jwk json.RawMessage, recordID string, expires time.Time) (uuid.UUID, error) {
	return s.mintLauncherCredential(ctx, tx, operator, service, host, thumbprint, jwk, recordID, expires)
}

// NewChainVerifier builds the record.ChainVerifier AuthenticateLauncher's issuance-chain
// re-verification uses, wired against st and approversSvc: FetchRecord and FetchApproval read
// straight from Postgres. VerifyAssertion re-runs the approver's real WebAuthn signature check
// (approvers.Service.VerifyAssertion — full origin/rpID/flags/challenge/signature verification,
// plus the approver key's own current active/revoked state) inside a transaction it always rolls
// back, so a re-check can never persist a side effect. The assertion's own authenticator
// signature counter was already advanced once, for real, by the original (committed) decision at
// ApplyDecision time, so every honest re-check of that exact same stored assertion fails the
// counter-monotonicity check on its own — approvers.ErrCounterReplay — and that is the ONLY error
// this treats as success: the counter check runs strictly after every cryptographic check inside
// VerifyAssertion, so a forged signature, wrong origin/rpID/challenge, or a since-revoked or
// tombstoned key all fail before the counter is ever reached, and none of those wrap
// ErrCounterReplay.
func NewChainVerifier(st *store.Store, approversSvc *approvers.Service, audience string, skew time.Duration) *record.ChainVerifier {
	return &record.ChainVerifier{
		Audience: audience,
		Skew:     skew,
		FetchRecord: func(ctx context.Context, recordID string) (string, time.Time, bool, error) {
			var body string
			var createdAt time.Time
			err := st.Pool.QueryRow(ctx, `select body, created_at from credential_requests where id=$1 and kind='launcher_credential'`, recordID).Scan(&body, &createdAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", time.Time{}, false, nil
			}
			if err != nil {
				return "", time.Time{}, false, err
			}
			return body, createdAt, true, nil
		},
		FetchApproval: func(ctx context.Context, recordID string) (json.RawMessage, bool, error) {
			var assertion json.RawMessage
			err := st.Pool.QueryRow(ctx, `select assertion from credential_request_events where record_id=$1 and event='approved'`, recordID).Scan(&assertion)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, false, nil
			}
			if err != nil {
				return nil, false, err
			}
			return assertion, true, nil
		},
		VerifyAssertion: func(ctx context.Context, login string, challenge [32]byte, assertion json.RawMessage) error {
			tx, err := st.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if _, err := approversSvc.VerifyAssertion(ctx, tx, login, challenge, assertion); err != nil && !errors.Is(err, approvers.ErrCounterReplay) {
				return err
			}
			return nil
		},
	}
}

// AuthenticateLauncher answers proof.Verifier's LookupLauncher hook directly: given a launcher
// credential's own id (the "lid" claim of a proof.SignLauncher proof), it resolves the live,
// unexpired credential row and re-verifies its entire issuance chain through s.Chain — a
// launcher_credentials row is never trusted on its own, so a row inserted without a genuine
// approved credential-request record behind it never authenticates. A missing, expired, revoked,
// or chain-broken credential all answer ("", false, nil); only a genuine dependency failure is a
// non-nil error, matching Lookup's own contract.
func (s *Service) AuthenticateLauncher(ctx context.Context, lid string) (string, bool, error) {
	if _, err := uuid.Parse(lid); err != nil {
		return "", false, nil
	}
	var thumbprint string
	var recordID *string
	var expiresAt time.Time
	err := s.Store.Pool.QueryRow(ctx, `select key_thumbprint, record_id, expires_at from launcher_credentials where id=$1 and revoked_at is null`, lid).
		Scan(&thumbprint, &recordID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !expiresAt.After(time.Now()) {
		return "", false, nil
	}
	if recordID == nil {
		return "", false, nil
	}
	if _, err := s.Chain.Verify(ctx, *recordID); err != nil {
		if errors.Is(err, record.ErrChainBroken) {
			return "", false, nil
		}
		return "", false, err
	}
	return thumbprint, true, nil
}

func (s *Service) Create(ctx context.Context, cred Credential, in Enrollment) (Enrollment, error) {
	if in.Operator != nil {
		in.Operator = new(record.CanonicalLogin(*in.Operator))
	}
	if !authorized(cred, in.Kind, in.Operator) {
		return Enrollment{}, ErrOperatorMismatch
	}
	in.Subject = nil
	if in.Kind == "pod" {
		if s.Pod == nil {
			return Enrollment{}, fmt.Errorf("%w: pod enrollment needs BROKER_K8S_OIDC_ISSUER", ErrPodIdentity)
		}
		claims, err := s.Pod.Verify(ctx, in.PodToken)
		if err != nil || claims.PodUID == "" || claims.PodUID != in.RuntimeID {
			return Enrollment{}, ErrPodIdentity
		}
		in.Subject = &claims.Subject
	}
	in.ID = uuid.New()
	in.LeaseExpires = time.Now().Add(s.Lease)

	// The row that conflicts with our insert can stop blocking it before the recovery lookup
	// runs — a concurrent Revoke racing this Create — or turn out to be dead already, its lease
	// lapsed with no revoke; either way the next attempt's insert can succeed, so Create retries.
	const maxAttempts = 3
	for range maxAttempts {
		result, retry, err := s.createAttempt(ctx, cred, in)
		if retry {
			continue
		}
		return result, err
	}
	return Enrollment{}, fmt.Errorf("create enrollment: exhausted retries after concurrent changes to runtime %s", in.RuntimeID)
}

// createAttempt makes one insert-then-recover attempt. retry is true when the row that conflicted
// with our insert no longer blocks a fresh one — it was revoked before the recovery lookup ran, or
// its lease had lapsed and recovery ended it — in which case Create should try again.
func (s *Service) createAttempt(ctx context.Context, cred Credential, in Enrollment) (result Enrollment, retry bool, err error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, false, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, session_id, subject, launcher_credential_id, lease_expires_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		in.ID, in.Kind, in.RuntimeID, in.Operator, in.Thumbprint, in.SessionID, in.Subject, cred.ID, in.LeaseExpires)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// The failed insert aborted tx, which still holds its pooled connection. Release it before
		// recovery asks the pool for one: holding one connection while waiting for a second is how
		// enough concurrent retries of one enrollment deadlock the whole pool.
		if err := tx.Rollback(ctx); err != nil {
			return Enrollment{}, false, err
		}
		if s.testConflictHook != nil {
			s.testConflictHook()
		}
		return s.recoverConflict(ctx, cred, in)
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ('enrollment.created',$1,$2, jsonb_build_object('kind',$3::text,'runtime_id',$4::text,'thumbprint',$5::text))`,
		in.ID, "launcher:"+cred.ID.String(), in.Kind, in.RuntimeID, in.Thumbprint); err != nil {
		return Enrollment{}, false, err
	}
	return in, false, tx.Commit(ctx)
}

// recoverConflict resolves an insert that collided with an unrevoked enrollment of the same
// runtime under the same launcher credential. The launcher may be retrying after a crash between
// our 201 and its persist: the same key gets its live enrollment back, and a different key for a
// live runtime is a refusal. A colliding row whose lease has lapsed is not live, whatever its
// revoked_at says: it is ended here — like a revoke, with an enrollment.expired audit row — and
// the caller retries, so a lapsed lease never leaves the runtime id permanently locked.
func (s *Service) recoverConflict(ctx context.Context, cred Credential, in Enrollment) (Enrollment, bool, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, false, err
	}
	defer tx.Rollback(ctx)
	var existing Enrollment
	var live bool
	err = tx.QueryRow(ctx, `select id, thumbprint, lease_expires_at, lease_expires_at > now() from enrollments
		where launcher_credential_id=$1 and runtime_id=$2 and revoked_at is null for update`, cred.ID, in.RuntimeID).
		Scan(&existing.ID, &existing.Thumbprint, &existing.LeaseExpires, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, true, nil
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	if !live {
		if err := endEnrollment(ctx, tx, existing.ID.String(), "broker", "enrollment.expired", "its lease lapsed"); err != nil {
			return Enrollment{}, false, err
		}
		return Enrollment{}, true, tx.Commit(ctx)
	}
	if existing.Thumbprint != in.Thumbprint {
		return Enrollment{}, false, ErrAlreadyEnrolled
	}
	in.ID, in.LeaseExpires, in.Existing = existing.ID, existing.LeaseExpires, true
	return in, false, nil
}

// endEnrollment ends enrollment id inside tx: it is marked revoked, every live grant under it is
// revoked, and every request still pending under it is cancelled — an approval must never land
// on a session that has ended — with one audit row per cancelled request and one kind row for
// the enrollment. The caller holds the enrollment row's lock.
func endEnrollment(ctx context.Context, tx pgx.Tx, id, actor, kind, reason string) error {
	if _, err := tx.Exec(ctx, `update enrollments set revoked_at=now() where id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where enrollment_id=$1 and revoked_at is null`, id, actor); err != nil {
		return err
	}
	detail := "the requesting enrollment ended: " + reason
	rows, err := tx.Query(ctx, `update requests set state='cancelled', decided_at=now(), decided_by=$2, decision_detail=$3
		where enrollment_id=$1 and state='pending' returning id`, id, actor, detail)
	if err != nil {
		return err
	}
	var cancelled []string
	for rows.Next() {
		var requestID string
		if err := rows.Scan(&requestID); err != nil {
			rows.Close()
			return err
		}
		cancelled = append(cancelled, requestID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, requestID := range cancelled {
		if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, actor, detail) values ('request.cancelled',$1,$2,$3, jsonb_build_object('reason',$4::text))`,
			id, requestID, actor, detail); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ($1,$2,$3, jsonb_build_object('reason',$4::text))`, kind, id, actor, reason)
	return err
}

func (s *Service) Renew(ctx context.Context, id string) (time.Time, error) {
	expires := time.Now().Add(s.Lease)
	tag, err := s.Store.Pool.Exec(ctx, `update enrollments set lease_expires_at=$2 where id=$1 and revoked_at is null and lease_expires_at > now()`, id, expires)
	if err != nil {
		return time.Time{}, err
	}
	if tag.RowsAffected() != 1 {
		return time.Time{}, ErrNotLive
	}
	return expires, nil
}

// authorized reports whether cred may act on an enrollment of the given kind and operator — the
// trust boundary both Create (when creating one) and Revoke (when ending one) enforce: an
// operator credential may only act on that same operator's own non-pod enrollments; a service
// credential (no operator) may only act on pod enrollments, which never carry an operator.
func authorized(cred Credential, kind string, operator *string) bool {
	switch {
	case cred.Operator != nil && (operator == nil || *operator != *cred.Operator):
		return false
	case cred.Operator != nil && kind == "pod":
		return false
	case cred.Operator == nil && (kind != "pod" || operator != nil):
		return false
	}
	return true
}

// Revoke ends the enrollment — revoking every live grant and cancelling every pending request
// under it — in one transaction. cred must be authorized for the target enrollment's own kind and
// operator — the same trust boundary Create enforces — checked before anything else, so a
// wrong-operator or wrong-kind caller can never revoke an enrollment it doesn't own, whether that
// enrollment is live, already revoked, or (were its id guessed rather than read back) merely
// plausible-looking. A row that does not exist at all has no operator/kind to check ownership
// against, so that case alone falls through to ErrNotLive below rather than ErrOperatorMismatch.
// Once ownership passes, Revoke returns ErrNotLive, changing nothing, when the enrollment is
// already revoked — the guard that keeps the audit trail honest: without it a no-op call would
// still write an "enrollment.revoked" audit row for an enrollment that never transitioned.
func (s *Service) Revoke(ctx context.Context, cred Credential, id, by string) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var operator *string
	var kind string
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `select operator, kind, revoked_at from enrollments where id=$1 for update`, id).Scan(&operator, &kind, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotLive
	}
	if err != nil {
		return err
	}
	if !authorized(cred, kind, operator) {
		return ErrOperatorMismatch
	}
	if revokedAt != nil {
		return ErrNotLive
	}
	if err := endEnrollment(ctx, tx, id, by, "enrollment.revoked", "revoked by its launcher"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Lookup answers proof.Verifier's Lookup callback. id comes from an untrusted proof's eid claim,
// so a value that is not even a well-formed UUID is refused as "not live" here — never handed to
// Postgres, whose uuid column would otherwise reject it with a type-mismatch error (SQLSTATE
// 22P02) that propagates out as a Lookup error instead of ("", false, nil), which
// proof.Verifier.Verify would in turn surface as a 500 instead of the 401 a forged eid deserves.
func (s *Service) Lookup(ctx context.Context, id string) (string, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", false, nil
	}
	var thumbprint string
	var live bool
	err := s.Store.Pool.QueryRow(ctx, `select thumbprint, revoked_at is null and lease_expires_at > now() from enrollments where id=$1`, id).Scan(&thumbprint, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return thumbprint, live, err
}

// Get answers this enrollment's own metadata (kind, operator, lease expiry) for
// GET /v1/enrollments/self. Added in Task 11 for that read, following the same query pattern as
// Lookup above. Unlike Lookup, which proof.Verifier calls with an untrusted, possibly malformed id
// straight off a forged proof, Get is only ever called with an id a proof has already
// authenticated, so it need not guard against a non-UUID id the way Lookup does.
func (s *Service) Get(ctx context.Context, id string) (Enrollment, error) {
	var e Enrollment
	err := s.Store.Pool.QueryRow(ctx, `select id, kind, runtime_id, operator, lease_expires_at from enrollments where id=$1 and revoked_at is null`, id).
		Scan(&e.ID, &e.Kind, &e.RuntimeID, &e.Operator, &e.LeaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, ErrNotLive
	}
	return e, err
}

// SessionID answers a live (non-revoked) enrollment's own session_id — the Envoy session a
// launcher recorded at Create time — for wake.Envoy's notification target. It follows Get's own
// "non-revoked" filter, not Lookup's stricter lease-not-expired one: main.go's waker calls this
// only as a fallback after requests.Machine.SessionID and discards its error, so a null
// session_id or an id that matches no live row both answer ("", nil); only a genuine Postgres
// failure is a non-nil error.
func (s *Service) SessionID(ctx context.Context, id string) (string, error) {
	var sessionID *string
	err := s.Store.Pool.QueryRow(ctx, `select session_id from enrollments where id=$1 and revoked_at is null`, id).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if sessionID == nil {
		return "", nil
	}
	return *sessionID, nil
}

// Replay records a jti; a unique violation means it was seen. Expired rows are pruned here too,
// so the table stays bounded without a separate job.
func (s *Service) Replay(ctx context.Context, jti string, expires time.Time) (bool, error) {
	_, err := s.Store.Pool.Exec(ctx, `insert into proof_jtis (jti, expires_at) values ($1,$2)`, jti, expires)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, _ = s.Store.Pool.Exec(ctx, `delete from proof_jtis where expires_at < now()`)
	return true, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
