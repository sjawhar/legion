// Package enroll issues key-bound launcher credentials and turns them into live enrollments. A
// launcher (an operator's box, a Kubernetes pod, or the host agent-secrets-helper of Plan B)
// signs its own future requests with the private key whose thumbprint and public JWK a launcher
// credential is minted against — machine.Service.ApplyDecision mints one once a human approves a
// typed-code machine login — and, for a pod, also proves a projected
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

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
)

var (
	ErrUnauthenticated  = errors.New("launcher credential is not valid")
	ErrOperatorMismatch = errors.New("this launcher credential enrols a different operator")
	ErrAlreadyEnrolled  = errors.New("this runtime is already enrolled and live")
	ErrPodIdentity      = errors.New("the projected token does not identify this pod")
	ErrNotLive          = errors.New("enrollment is not live")
	ErrInvalidSlot      = errors.New("a slot is only for a pod enrollment and must match ^[a-z][a-z0-9-]{0,62}$")
	ErrNoCredential     = errors.New("no such machine login")
	ErrNotApprover      = errors.New("only the person who approved the machine login may revoke it")
)

type Credential struct {
	ID       uuid.UUID
	Operator *string
	Service  *string
	Host     string
}

// LiveCredential is one of the machine logins a person approved, as their machine-login page lists
// it: a launcher credential minted by their approval and not revoked, either unexpired or still
// holding a live session it enrolled (Lookup's live: not revoked, its lease not lapsed).
type LiveCredential struct {
	ID   uuid.UUID
	Host string
	// Service names the service a service's login is for (legion-daemon); nil for a person's own
	// machine.
	Service   *string
	IssuedAt  time.Time
	ExpiresAt time.Time
	// Expired is true once ExpiresAt has passed: the login enrolls no more sessions, but the ones
	// it enrolled renew with their own keys and still run, so it stays listed until they end or
	// lapse, or it is revoked.
	Expired bool
}

type Enrollment struct {
	ID         uuid.UUID
	Kind       string
	RuntimeID  string
	Operator   *string
	Thumbprint string
	SessionID  *string
	PodToken   string
	// Slot names one of several independent identities in one pod (record.ValidSlot): each slot
	// of a pod is an enrollment of its own, with its own key, lease and grants. "" is the one
	// identity of every box, host and single-identity pod enrollment.
	Slot string
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
	// builds one against this same Store.
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
// re-verification uses, scoped to launcher_credential records.
func NewChainVerifier(st *store.Store, audience string, skew time.Duration) *record.ChainVerifier {
	return st.ChainVerifier(record.KindLauncherCredential, audience, skew)
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

// Credential loads a launcher credential's own identity (operator, service, host) by its id, for
// a handler that has already authenticated the caller via a launcher proof (proof.Subject's
// LauncherID, verified live by AuthenticateLauncher through proof.Verifier.LookupLauncher) and
// needs that identity to pass to Create or Revoke. err is a genuine dependency failure; a launcher
// id a live proof just verified vanishing before this read is treated the same way (never
// silently swallowed into an empty Credential).
func (s *Service) Credential(ctx context.Context, id string) (Credential, error) {
	var cred Credential
	if err := s.Store.Pool.QueryRow(ctx, `select id, operator, service, host from launcher_credentials where id=$1`, id).
		Scan(&cred.ID, &cred.Operator, &cred.Service, &cred.Host); err != nil {
		return Credential{}, err
	}
	return cred, nil
}

// Create enrolls in under cred. An enrollment's operator is its credential's: the email of the
// person who approved the machine login that minted it (machine.Service.ApplyDecision), recorded
// whether or not the launcher states one. A stated operator that names anyone else, or any operator
// stated under a service credential, is ErrOperatorMismatch. A live enrollment is unique per
// launcher credential, runtime id and slot: the same key in the same slot gets its live enrollment
// back (Existing), a different key in a live slot is ErrAlreadyEnrolled, and another slot of the
// same pod is an enrollment of its own. A slot is valid only on a pod enrollment (ErrInvalidSlot);
// a pod's runtime id is always the pod UID its projected token proves, whichever slot it enrolls.
// A credential revoked by the person who approved it (RevokeCredential), even after the caller's
// launcher proof was verified, is ErrUnauthenticated.
func (s *Service) Create(ctx context.Context, cred Credential, in Enrollment) (Enrollment, error) {
	if in.Slot != "" && (in.Kind != "pod" || !record.ValidSlot(in.Slot)) {
		return Enrollment{}, ErrInvalidSlot
	}
	if in.Operator != nil && (cred.Operator == nil || record.CanonicalLogin(*in.Operator) != *cred.Operator) {
		return Enrollment{}, ErrOperatorMismatch
	}
	in.Operator = cred.Operator
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
// its lease had lapsed and recovery ended it — in which case Create should try again. It first
// takes the credential's row for share, which RevokeCredential's lock waits behind, and refuses a
// revoked credential (ErrUnauthenticated): an insert either commits before a revoke reads the
// credential's enrollments, and is ended with them, or finds the credential revoked.
func (s *Service) createAttempt(ctx context.Context, cred Credential, in Enrollment) (result Enrollment, retry bool, err error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, false, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `select 1 from launcher_credentials where id=$1 and revoked_at is null for share`, cred.ID).Scan(nil)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, false, ErrUnauthenticated
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	_, err = tx.Exec(ctx, `insert into enrollments (id, kind, runtime_id, slot, operator, thumbprint, session_id, subject, launcher_credential_id, lease_expires_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		in.ID, in.Kind, in.RuntimeID, in.Slot, in.Operator, in.Thumbprint, in.SessionID, in.Subject, cred.ID, in.LeaseExpires)
	if store.IsUniqueViolation(err) {
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
	if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ('enrollment.created',$1,$2,
		jsonb_strip_nulls(jsonb_build_object('kind',$3::text,'runtime_id',$4::text,'thumbprint',$5::text,'slot',nullif($6::text,''))))`,
		in.ID, "launcher:"+cred.ID.String(), in.Kind, in.RuntimeID, in.Thumbprint, in.Slot); err != nil {
		return Enrollment{}, false, err
	}
	return in, false, tx.Commit(ctx)
}

// recoverConflict resolves an insert that collided with an unrevoked enrollment of the same
// runtime and slot under the same launcher credential. The launcher may be retrying after a crash
// between our 201 and its persist: the same key gets its live enrollment back, and a different key
// for a live slot is a refusal. A colliding row whose lease has lapsed is not live, whatever its
// revoked_at says: it is ended here — like a revoke, with an enrollment.expired audit row — and
// the caller retries, so a lapsed lease never leaves the slot permanently locked.
func (s *Service) recoverConflict(ctx context.Context, cred Credential, in Enrollment) (Enrollment, bool, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, false, err
	}
	defer tx.Rollback(ctx)
	var existing Enrollment
	var live bool
	err = tx.QueryRow(ctx, `select id, thumbprint, lease_expires_at, lease_expires_at > now() from enrollments
		where launcher_credential_id=$1 and runtime_id=$2 and slot=$3 and revoked_at is null for update`, cred.ID, in.RuntimeID, in.Slot).
		Scan(&existing.ID, &existing.Thumbprint, &existing.LeaseExpires, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, true, nil
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	if !live {
		if _, _, err := endLapsed(ctx, tx, existing.ID.String()); err != nil {
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
// on a session that has ended — with its audit row and its record's cancelled event
// (store.EndPendingRequests, so no approver's pending list keeps showing it), and one kind row
// for the enrollment. It reports how many grants it revoked and requests it cancelled. The caller
// holds the enrollment row's lock.
func endEnrollment(ctx context.Context, tx pgx.Tx, id, actor, kind, reason string) (grantsRevoked int64, requestsCancelled int, err error) {
	if _, err := tx.Exec(ctx, `update enrollments set revoked_at=now() where id=$1`, id); err != nil {
		return 0, 0, err
	}
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where enrollment_id=$1 and revoked_at is null`, id, actor)
	if err != nil {
		return 0, 0, err
	}
	detail := "the requesting enrollment ended: " + reason
	cancelled, err := store.EndPendingRequests(ctx, tx, store.RequestsOfEnrollment, id, store.RequestEnd{
		State: "cancelled", Actor: actor, DecidedBy: &actor, Detail: detail, AuditDetail: map[string]any{"reason": detail},
	})
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ($1,$2,$3, jsonb_build_object('reason',$4::text))`, kind, id, actor, reason); err != nil {
		return 0, 0, err
	}
	return tag.RowsAffected(), len(cancelled), nil
}

// endLapsed ends an enrollment whose lease lapsed: endEnrollment with actor broker and an
// enrollment.expired audit row, for recoverConflict and the sweep alike.
func endLapsed(ctx context.Context, tx pgx.Tx, id string) (grantsRevoked int64, requestsCancelled int, err error) {
	return endEnrollment(ctx, tx, id, "broker", "enrollment.expired", "its lease lapsed")
}

// LapsedEnrollment is one enrollment EndLapsed ended.
type LapsedEnrollment struct {
	ID                string
	Kind              string
	RuntimeID         string
	Slot              string
	LeaseExpires      time.Time
	GrantsRevoked     int64
	RequestsCancelled int
}

// lapsedBatch is how many lapsed enrollments EndLapsed ends in one transaction.
const lapsedBatch = 100

// EndLapsed ends every enrollment whose lease has lapsed and that nothing has ended yet: a pod that
// is gone, a box whose launcher stopped renewing, a host session whose helper died. Each is ended
// as Revoke ends one (endEnrollment, actor "broker", an enrollment.expired audit row), so its
// grants are revoked and its pending requests cancelled and dropped from the approver's list,
// rather than waiting there for their own expiry with an approval that could grant nothing.
//
// Lapsed means what Lookup means by not live: lease_expires_at no later than Postgres's now(). A
// session that keeps renewing keeps its lease ahead of now() and is never selected, and Renew
// refuses an enrollment once its lease has lapsed, so ending one takes nothing a session could
// still use. Rows a concurrent Renew or Revoke has locked are skipped (the next sweep sees them
// as they are then), and an ended row has revoked_at set and is never selected again, so a sweep
// that finds nothing changes nothing. It works in batches of lapsedBatch, each one transaction,
// and returns what it ended.
func (s *Service) EndLapsed(ctx context.Context) ([]LapsedEnrollment, error) {
	var ended []LapsedEnrollment
	for {
		batch, err := s.endLapsedBatch(ctx)
		ended = append(ended, batch...)
		if err != nil || len(batch) < lapsedBatch {
			return ended, err
		}
	}
}

func (s *Service) endLapsedBatch(ctx context.Context) ([]LapsedEnrollment, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `select id, kind, runtime_id, slot, lease_expires_at from enrollments
		where revoked_at is null and lease_expires_at <= now()
		order by lease_expires_at limit $1 for update skip locked`, lapsedBatch)
	if err != nil {
		return nil, err
	}
	batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LapsedEnrollment, error) {
		var e LapsedEnrollment
		err := row.Scan(&e.ID, &e.Kind, &e.RuntimeID, &e.Slot, &e.LeaseExpires)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	for i := range batch {
		batch[i].GrantsRevoked, batch[i].RequestsCancelled, err = endLapsed(ctx, tx, batch[i].ID)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return batch, nil
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
	if _, _, err := endEnrollment(ctx, tx, id, by, "enrollment.revoked", "revoked by its launcher"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LiveCredentials lists the machine logins approver approved that can still reach a secret, newest
// first: every unrevoked launcher credential minted from a launcher_credential record whose
// approver is that person, while it is unexpired or a session it enrolled is live as Lookup means
// it: not revoked, its lease not lapsed. A login's sessions outlive its expiry, since each renews
// with its own key (Renew never reads the credential), so an expired login stays listed, Expired
// set, until its last session ends or lapses (a lapsed session renews no more, swept or not):
// revoking every listed login ends every session the person's machines started. That covers a
// person's own machines (whose operator is their approver, machine.Service.ApplyDecision) and a
// service's login, such as the Legion daemon's, which has no operator and is listed for the person
// who approved it, with its Service set.
func (s *Service) LiveCredentials(ctx context.Context, approver string) ([]LiveCredential, error) {
	rows, err := s.Store.Pool.Query(ctx, `select c.id, c.host, c.service, c.created_at, c.expires_at, c.expires_at <= now() from launcher_credentials c
		join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential'
		where r.approver = $1 and c.revoked_at is null
			and (c.expires_at > now() or exists (select 1 from enrollments e where e.launcher_credential_id = c.id and e.revoked_at is null and e.lease_expires_at > now()))
		order by c.created_at desc, c.id`, record.CanonicalLogin(approver))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LiveCredential])
}

// RevokeCredential ends launcher credential id on the word of the person who approved it, expired
// or not: from then on no launcher proof signed with it authenticates (AuthenticateLauncher), and
// every enrollment it made that has not ended — a person's host sessions and boxes, or every pod a
// service's login enrolled, which outlive the credential's expiry — is ended as Revoke ends one
// (endEnrollment, actor "human:<approver>", an enrollment.revoked audit row), revoking each one's
// grants and cancelling its pending requests, all in one transaction with one
// launcher_credential.revoked audit row. approver must be the approver of the launcher_credential
// record the credential was minted from (ErrNotApprover); an unknown id, like a credential minted
// from no such record (which only ApplyDecision mints, always from one), is ErrNoCredential.
// Revoking a credential already revoked succeeds and changes nothing. It takes the credential's
// row before its enrollments' rows, so an enrollment Create is inserting under the credential
// (which holds the row for share) commits first and is ended here, or waits and finds the
// credential revoked; and it takes those enrollments' rows, so a launcher's own Revoke of one
// either commits first, leaving it out of the list here, or waits and finds it ended.
func (s *Service) RevokeCredential(ctx context.Context, id, approver string) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var approvedBy, host string
	var service *string
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `select r.approver, c.service, c.host, c.revoked_at from launcher_credentials c
		join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential'
		where c.id = $1 for no key update of c`, id).Scan(&approvedBy, &service, &host, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoCredential
	}
	if err != nil {
		return err
	}
	person := record.CanonicalLogin(approver)
	if approvedBy != person {
		return ErrNotApprover
	}
	if revokedAt != nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `update launcher_credentials set revoked_at=now() where id=$1`, id); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `select id from enrollments where launcher_credential_id=$1 and revoked_at is null order by id for update`, id)
	if err != nil {
		return err
	}
	ended, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	actor := "human:" + person
	for _, enrollment := range ended {
		if _, _, err := endEnrollment(ctx, tx, enrollment, actor, "enrollment.revoked", "its machine login was revoked"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, actor, detail) values ('launcher_credential.revoked',$1,
		jsonb_strip_nulls(jsonb_build_object('credential_id',$2::text,'host',$3::text,'service',$4::text,'enrollments',coalesce(to_jsonb($5::text[]),'[]'::jsonb))))`,
		actor, id, host, service, ended); err != nil {
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

// Get answers this enrollment's own metadata (kind, operator, slot, lease expiry) for
// GET /v1/enrollments/self. Added in Task 11 for that read, following the same query pattern as
// Lookup above. Unlike Lookup, which proof.Verifier calls with an untrusted, possibly malformed id
// straight off a forged proof, Get is only ever called with an id a proof has already
// authenticated, so it need not guard against a non-UUID id the way Lookup does.
func (s *Service) Get(ctx context.Context, id string) (Enrollment, error) {
	var e Enrollment
	err := s.Store.Pool.QueryRow(ctx, `select id, kind, runtime_id, operator, slot, lease_expires_at from enrollments where id=$1 and revoked_at is null`, id).
		Scan(&e.ID, &e.Kind, &e.RuntimeID, &e.Operator, &e.Slot, &e.LeaseExpires)
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
	if store.IsUniqueViolation(err) {
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
