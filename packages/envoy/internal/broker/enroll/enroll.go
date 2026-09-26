// Package enroll issues launcher credentials and turns them into live enrollments: a launcher
// (an operator's box, a Kubernetes pod, or the host agent-secrets-helper of Plan B) proves it
// holds a bearer credential and, for a pod, a projected service-account token bound to that pod,
// and receives a leased enrollment keyed by its own signing key's thumbprint. proof.Verifier reads
// enrollments back through Lookup and Replay to authenticate later session proofs.
package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
	ID            uuid.UUID
	Kind          string
	RuntimeID     string
	Operator      *string
	ApproverKind  string
	ApproverIssue *string
	Thumbprint    string
	SessionID     *string
	PodToken      string
	LeaseExpires  time.Time
	Existing      bool // true when Create returned an already-live enrollment for the same key
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

	// testConflictHook, when set, runs once a 23505 insert conflict is detected in createAttempt,
	// before the recovery lookup. It exists only so a test can deterministically reproduce the
	// race where the conflicting row is revoked between the failed insert and that lookup; no
	// production caller sets it.
	testConflictHook func()
}

func hash(token string) []byte { s := sha256.Sum256([]byte(token)); return s[:] }

func (s *Service) MintLauncherCredential(ctx context.Context, operator *string, service *string, host, askID string) (uuid.UUID, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return uuid.Nil, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	id := uuid.New()
	_, err := s.Store.Pool.Exec(ctx, `insert into launcher_credentials (id, operator, service, host, token_hash, issued_via_ask) values ($1,$2,$3,$4,$5,$6)`,
		id, operator, service, host, hash(token), nullable(askID))
	if err != nil {
		return uuid.Nil, "", err
	}
	return id, token, nil
}

func (s *Service) AuthenticateLauncher(ctx context.Context, bearer string) (Credential, error) {
	var c Credential
	err := s.Store.Pool.QueryRow(ctx, `select id, operator, service, host from launcher_credentials where token_hash=$1 and revoked_at is null`, hash(bearer)).
		Scan(&c.ID, &c.Operator, &c.Service, &c.Host)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrUnauthenticated
	}
	return c, err
}

func (s *Service) Create(ctx context.Context, cred Credential, in Enrollment) (Enrollment, error) {
	// An operator's credential enrols that operator's boxes and host sessions; a service credential
	// (operator null, service set) enrols pods only, with no operator. Nothing else is accepted.
	switch {
	case cred.Operator != nil && (in.Operator == nil || *in.Operator != *cred.Operator):
		return Enrollment{}, ErrOperatorMismatch
	case cred.Operator != nil && in.Kind == "pod":
		return Enrollment{}, ErrOperatorMismatch
	case cred.Operator == nil && (in.Kind != "pod" || in.Operator != nil):
		return Enrollment{}, ErrOperatorMismatch
	}
	if in.Kind == "pod" {
		if s.Pod == nil {
			return Enrollment{}, fmt.Errorf("%w: pod enrollment needs BROKER_K8S_OIDC_ISSUER", ErrPodIdentity)
		}
		claims, err := s.Pod.Verify(ctx, in.PodToken)
		if err != nil || claims.PodUID == "" || claims.PodUID != in.RuntimeID {
			return Enrollment{}, ErrPodIdentity
		}
	}
	in.ID = uuid.New()
	in.LeaseExpires = time.Now().Add(s.Lease)

	// A row that conflicts with our insert can be revoked between our failed insert and the
	// recovery lookup below — a concurrent Revoke racing this Create. The recovery lookup then
	// finds no live row at all (pgx.ErrNoRows), even though the partial unique index that rejected
	// our insert a moment ago no longer blocks a fresh one. Retrying the whole attempt resolves
	// that race instead of surfacing a spurious "not found" as an internal error.
	const maxAttempts = 3
	for range maxAttempts {
		result, retry, err := s.createAttempt(ctx, cred, in)
		if retry {
			continue
		}
		return result, err
	}
	return Enrollment{}, fmt.Errorf("create enrollment: exhausted retries after a concurrent revoke race for runtime %s", in.RuntimeID)
}

// createAttempt makes one insert-then-recover attempt. retry is true only when the row that
// conflicted with our insert was revoked before the recovery lookup ran, in which case Create
// should try the whole attempt again rather than treat the result as final.
func (s *Service) createAttempt(ctx context.Context, cred Credential, in Enrollment) (result Enrollment, retry bool, err error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, false, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, approver_kind, approver_issue, thumbprint, session_id, launcher_credential_id, lease_expires_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		in.ID, in.Kind, in.RuntimeID, in.Operator, in.ApproverKind, in.ApproverIssue, in.Thumbprint, in.SessionID, cred.ID, in.LeaseExpires)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if s.testConflictHook != nil {
			s.testConflictHook()
		}
		// The launcher may be retrying after a crash between our 201 and its persist: the same key
		// gets its live enrollment back; a different key for a live runtime is a refusal.
		var live Enrollment
		lookupErr := s.Store.Pool.QueryRow(ctx, `select id, thumbprint, lease_expires_at from enrollments where launcher_credential_id=$1 and runtime_id=$2 and revoked_at is null`, cred.ID, in.RuntimeID).
			Scan(&live.ID, &live.Thumbprint, &live.LeaseExpires)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return Enrollment{}, true, nil
		}
		if lookupErr != nil {
			return Enrollment{}, false, lookupErr
		}
		if live.Thumbprint != in.Thumbprint {
			return Enrollment{}, false, ErrAlreadyEnrolled
		}
		in.ID, in.LeaseExpires, in.Existing = live.ID, live.LeaseExpires, true
		return in, false, nil
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

// Revoke ends the enrollment and every live grant under it in one transaction. It returns
// ErrNotLive, changing nothing, when the enrollment does not exist or is already revoked — the
// guard that keeps the audit trail honest: without it a no-op call would still revoke grants and
// write an "enrollment.revoked" audit row for an enrollment that never transitioned.
func (s *Service) Revoke(ctx context.Context, id, by string) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `update enrollments set revoked_at=now() where id=$1 and revoked_at is null`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotLive
	}
	if _, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where enrollment_id=$1 and revoked_at is null`, id, by); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor) values ('enrollment.revoked',$1,$2)`, id, by); err != nil {
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
