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
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

var (
	ErrUnauthenticated  = errors.New("launcher credential is not valid")
	ErrOperatorMismatch = errors.New("this launcher credential enrols a different operator")
	ErrAlreadyEnrolled  = errors.New("this runtime is already enrolled and live")
	ErrPodIdentity      = errors.New("the projected token does not identify this pod")
	// ErrPodTokenExpired is a pod token whose expiry has passed, wrapped with the expiry the token
	// names: the launcher or renewer presents the pod's current token with the same key, where
	// ErrPodIdentity is a token for another pod, or no token of this cluster at all. An expired
	// token is reported as expired whatever else is wrong with it (oidc.Verifier reads a failed
	// token's claims before blaming its signature), so its pod UID, which nothing verified, is
	// never compared.
	ErrPodTokenExpired = errors.New("the projected token has expired")
	ErrNotLive         = errors.New("enrollment is not live")
	ErrInvalidSlot     = errors.New("a slot is only for a pod enrollment and must match ^[a-z][a-z0-9-]{0,62}$")
	ErrNoCredential    = errors.New("no such machine login")
	ErrNotApprover     = errors.New("only the person who approved a person's machine login may revoke it")
	// ErrNotEnrollingCredential refuses a revoke of an enrollment that carries no operator — a
	// pod's, or a machine's own session — by any launcher credential but the one that enrolled
	// it (Revoke).
	ErrNotEnrollingCredential = errors.New("only the machine login that made an enrollment carrying no operator may end it")
	ErrPodTokenRequired       = errors.New("a pod enrollment's renewal must carry its projected token")
	// ErrPodSelfEnrollOnly refuses a pod enrollment under the enrolling pod's own token login —
	// one whose subject is the very ServiceAccount the presented pod token proves — whose runtime
	// id names another pod. Such a credential belongs to one pod, the pod whose token logged in
	// (launcher_credentials.pod_uid), so it enrols that pod and nothing else.
	ErrPodSelfEnrollOnly = errors.New("a pod's own machine login enrols that pod alone")
	// ErrPodNotLaunched refuses a pod enrollment under a token login that neither belongs to that
	// pod nor launches the pods of the account its token proves: BROKER_SERVICES does not bind
	// that account to the login's service. Nothing but the deployment's own settings says a
	// service launches an account's pods, and a token login is authorized by those settings
	// alone, so a token it was handed proves whose pod it is and never who may enrol it. A
	// service's login a person approved carries no subject and keeps that person's approval as
	// its authorization, so this never reaches one.
	ErrPodNotLaunched = errors.New("this machine login does not launch the pods of that service account")
	// ErrMachineLoginEnded refuses a machine's own session a renewal because the launcher
	// credential its exchange minted is no longer live: revoked, past the expiry its Google
	// identity token gave it, or its account no longer listed for its service (Renew).
	ErrMachineLoginEnded = errors.New("the machine login that enrolled this session has ended")
	// ErrSessionHeld refuses an enrollment whose session name another principal holds, read from
	// the one table that says who holds a name — the same one the token exchange binds every
	// token's name in (oauthstore.Store.BindSession, oauthstore.ErrSessionHeld). Its text names
	// the session and says what to do about it, as that refusal's own text and the exchange's
	// do, and names no holder: whoever holds the name is not this caller's to learn.
	ErrSessionHeld = errors.New("another identity holds this session name")
)

type Credential struct {
	ID       uuid.UUID
	Operator *string
	Service  *string
	Host     string
	// Subject is the identity a token login proved (TokenLogin.Subject): a Kubernetes
	// ServiceAccount, or a machine as the token exchange names it (exchange.MachineSubject); nil
	// for every login a person approved.
	Subject *string
	// PodUID is the pod whose token a Kubernetes ServiceAccount's login presented
	// (AccountClaims.PodUID, launcher_credentials.pod_uid): the one pod that credential belongs
	// to. "" for a token bound to no pod, such as one `kubectl create token` mints, for a VM's
	// login, and for every login a person approved.
	PodUID string
}

// LiveCredential is one of the machine logins a person may revoke, as their machine-login page
// lists it: a launcher credential of one of their own machines, or of any service's login whoever
// approved it, not revoked, and either unexpired or still holding a live session it enrolled
// (Lookup's live: not revoked, its lease not lapsed).
type LiveCredential struct {
	ID   uuid.UUID
	Host string
	// Service names the service a service's login is for (legion-daemon); nil for a person's own
	// machine.
	Service *string
	// Subject is what a token login proved: its ServiceAccount
	// (system:serviceaccount:<namespace>:<name>), or a machine as the token exchange names it
	// (exchange.MachineSubject); nil for every login a person approved.
	Subject *string
	// ApprovedBy is the login of the person who approved it, "" for a service's token login, which
	// no person approves.
	ApprovedBy string
	IssuedAt   time.Time
	ExpiresAt  time.Time
	// Expired is true once ExpiresAt has passed: the login enrols no more sessions, and the ones
	// it enrolled renew with their own keys and still run — every one but a machine's own
	// session, whose renewal is held to its login being live (Renew) — so it stays listed until
	// they end or lapse, or it is revoked.
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
	// identity of every session and single-identity pod enrollment.
	Slot string
	// Subject is a pod enrollment's verified service-account subject, set by Create from the
	// token itself and never from the caller; nil for a session.
	Subject      *string
	LeaseExpires time.Time
	Existing     bool // true when Create returned an already-live enrollment for the same key
	// carry is the principal the enrollment belongs to, as enrollments.carry_principal records
	// it: Create works it out from what the caller proved (carryPrincipal) and nothing reads it
	// back off this struct, so it is the package's own and never a caller's to state.
	carry *string
}

type PodClaims struct {
	Subject string
	PodUID  string
}

// PodVerifier verifies the cluster's projected ServiceAccount tokens (K8sPodVerifier): Verify a
// pod's own token, bound to its pod, for enrollment and renewal; VerifyAccount a service's token,
// bound to a pod or to none, for its token login (machine.Service.TokenLogin).
type PodVerifier interface {
	Verify(ctx context.Context, raw string) (PodClaims, error)
	VerifyAccount(ctx context.Context, raw string) (AccountClaims, error)
}

// SessionBinder binds a session name to the principal that holds it and refuses it to every
// other: oauthstore.Store, whose BindSession the token exchange binds the name of every token it
// mints with. It is an interface here for the same reason PodVerifier is one — so a test wires a
// binder of its own — and it joins the transaction the context carries (oauthstore.WithTx), which
// is how an enrollment's bind and its own rows commit or roll back together.
type SessionBinder interface {
	BindSession(ctx context.Context, session, principal string, fixed bool, expires time.Time) (string, error)
}

type Service struct {
	Store *store.Store
	Lease time.Duration
	// Pod verifies the cluster's projected ServiceAccount tokens against BROKER_K8S_OIDC_ISSUER and
	// BROKER_K8S_OIDC_AUDIENCE; nil on a broker that trusts no Kubernetes token issuer, which enrolls
	// no pod and serves no Kubernetes token login.
	Pod PodVerifier

	// Sessions binds every enrollment that names a session to the principal that enrollment
	// belongs to (carryPrincipal), in the enrollment's own transaction, and refuses it a name
	// another principal holds (Create, ErrSessionHeld). It is the broker's one record of which
	// principal holds a session name, oauth_session_bindings, written here and by the token
	// exchange alike, so the name a session writes its Dispatch tokens under and the name its
	// grant follows have the same holder.
	//
	// nil on a broker that binds nothing: such a broker writes no binding, refuses no
	// enrollment its name, and so lets two principals enrol one name, each adopting nothing of
	// the other's (adopt). cmd/broker/main.go wires the real oauthstore.Store.
	Sessions SessionBinder

	// Chain re-verifies a launcher credential's issuance chain on every AuthenticateLauncher
	// call: a launcher_credentials row is never trusted on its own, since it must still trace
	// back to a genuinely signed, human-approved credential-request record. NewChainVerifier
	// builds one against this same Store.
	Chain *record.ChainVerifier

	// ServiceLogins is BROKER_SERVICE_LOGINS: each ServiceAccount subject that logs in as a service
	// with its token, answering that service. AuthenticateLauncher reads it for every credential a
	// Kubernetes token login issued.
	ServiceLogins map[string]string
	// ServicesByAccount is BROKER_SERVICES: each ServiceAccount a service's pods run as, answering
	// that service. It is the deployment's own statement of which pods a service launches, and so
	// of which pods a service's token login may enrol besides its own (Create, ErrPodNotLaunched);
	// requests.Machine reads the same setting to decide whether an enrolled pod is that service's.
	// Unset, a token login enrols its own pod alone.
	ServicesByAccount map[string]string
	// VMServicesByAccount is BROKER_VM_SERVICES, keyed by a Google service account's email: the
	// service the machines of that account are listed as. AuthenticateLauncher reads it for
	// every credential the token exchange minted for a machine, which keeps that credential
	// authenticating only while its account is still listed.
	VMServicesByAccount map[string]string

	// testConflictHook, when set, runs once a 23505 insert conflict is detected in createAttempt,
	// before the recovery lookup. It exists only so a test can deterministically reproduce the
	// race where the conflicting row is revoked between the failed insert and that lookup; no
	// production caller sets it.
	testConflictHook func()

	// testSchemaReadHook, when set, runs in IssueTokenLoginTx between the schema read that
	// chooses the conflict target (perPodLogins) and the upsert that names it. It exists only
	// so a test can deterministically reproduce the race where the next release's migration
	// drops the index that read saw; no production caller sets it.
	testSchemaReadHook func()
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

// MintLauncherCredentialTx mints a person's launcher credential, operating operator's machine,
// inside tx, joined to the caller's own transaction — machine.Service.ApplyDecision's own approval
// commit.
func (s *Service) MintLauncherCredentialTx(ctx context.Context, tx pgx.Tx, operator, host, thumbprint string, jwk json.RawMessage, recordID string, expires time.Time) (uuid.UUID, error) {
	return s.mintLauncherCredential(ctx, tx, &operator, nil, host, thumbprint, jwk, recordID, expires)
}

// TokenLogin is one verified token login, as IssueTokenLogin records it: the identity the token
// proved (a ServiceAccount subject, a machine as the token exchange names it,
// exchange.MachineSubject, or a person's sign-in on a machine, exchange.PersonSubject), the
// service BROKER_SERVICE_LOGINS or BROKER_VM_SERVICES maps it to,
// the person its sessions act for (a sign-in's, "" for a machine's and for a Kubernetes
// service's, which act as nobody), the host the credential names (a ServiceAccount's
// <namespace>/<name>, a machine's <project>/<zone>/<instance name>), the pod its token is bound
// to ("" for none) and when the token expires, the key the request object or the DPoP proof
// proved (its thumbprint and public JWK), and when the credential expires.
type TokenLogin struct {
	Subject, Service, Host, PodUID string
	// Operator is the person the credential's enrollments run for, which makes them that
	// person's sessions exactly as a dev box's machine login does (authorized). A person's
	// sign-in on a machine sets it and nothing else does: a machine's own credential and a
	// Kubernetes service's act for no person, and their sessions reach their service's secrets
	// alone.
	Operator     string
	TokenExpires time.Time
	Thumbprint   string
	JWK          json.RawMessage
	Expires      time.Time
}

// IssueTokenLogin issues or moves the launcher credential of a token login, which no person
// approves, in a transaction of its own: machine.Service.TokenLogin verified everything in
// login and has none to join.
func (s *Service) IssueTokenLogin(ctx context.Context, login TokenLogin) (uuid.UUID, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	id, err := s.IssueTokenLoginTx(ctx, tx, login)
	if err != nil {
		return uuid.Nil, err
	}
	return id, tx.Commit(ctx)
}

// IssueTokenLoginTx issues or moves the launcher credential of a token login inside tx, joined
// to the caller's own transaction — the RFC 8693 exchange's, which must write the credential
// and the access token that names it together or not at all
// (oauth.ExchangeConfig.MachineCredentials).
//
// A machine login becomes per pod rather than per account over two releases, and this reads
// which of the two schemas it is on and names the conflict target that schema has, so both
// generations of the broker are right on either while a roll serves them together
// (migration 0029, and legacySubjectIndex below).
//
// While 0015's launcher_credentials_live_subject is still there — this release — one subject
// holds one live credential, and every login of that subject moves it: that is what the previous
// broker does, and what the previous broker's own upsert needs the index for. The login's pod is
// written to the row all the same (migration 0028), which is what lets a pod's own login enrol
// that pod and no other (podUnderTokenLogin).
//
// Once the release after this one has dropped that index, a login a pod's own projected token
// made belongs to that pod, so every pod of an account may hold one at the same time, each with
// its own key, and a login from a token bound to no pod — a launcher's, which names no pod of
// its own, a machine's and a person's sign-in — is its subject's one such credential. A pod
// whose credential predates that release carries no pod on its row, so the per-pod conflict
// target cannot match it; such a pod claims that row for itself on its next login, by its
// subject and the key the login signed with, and keeps its id, rather than being refused its own
// key for as long as the row lives.
//
// Either way the credential is created at its first login and each later login of the same pod,
// or of the same subject, moves it to that login's key, person and expiry under the same id, so
// a login proving the key the credential already holds changes only its expiry and every session
// enrolled under it stays there. A machine's subject names that one machine
// (exchange.MachineSubject) and a sign-in's names one person on one box
// (exchange.PersonSubject), so each machine of a fleet, and each person signed in on each of its
// boxes, holds a credential of its own and no login moves another's. Its host and its pod are
// login's. Each login writes a launcher_credential.token_login audit row naming the subject, the
// pod the token is bound to, the key, and both expiries. A key another live credential holds is
// a unique violation (store.IsUniqueViolation), and nothing is written.
func (s *Service) IssueTokenLoginTx(ctx context.Context, tx pgx.Tx, login TokenLogin) (uuid.UUID, error) {
	var operator *string
	if login.Operator != "" {
		operator = new(record.CanonicalLogin(login.Operator))
	}
	// The schema read below and the statement that uses its answer must see one schema. `drop
	// index` takes ACCESS EXCLUSIVE on the table, and this transaction has touched nothing yet,
	// so without a lock of its own the next release's drop lands between the two and the upsert
	// names a conflict target the schema no longer has: 42P10, and a refused login. ROW
	// EXCLUSIVE is the lock the insert takes anyway and is held to commit either way. It
	// conflicts with ACCESS EXCLUSIVE, so the drop waits for a login in flight, or the login
	// waits for the drop and then reads the schema the drop left; and it does not conflict with
	// itself, so two logins never wait for each other.
	if _, err := tx.Exec(ctx, `lock table launcher_credentials in row exclusive mode`); err != nil {
		return uuid.Nil, err
	}
	// Postgres resolves one conflict target against one index, so which index the schema has
	// decides which target this names. perPodLogins is false while 0015's index is still there,
	// and the statement is then the previous broker's exactly.
	perPod, err := perPodLogins(ctx, tx)
	if err != nil {
		return uuid.Nil, err
	}
	if s.testSchemaReadHook != nil {
		s.testSchemaReadHook()
	}
	conflict := `(subject) where subject is not null and revoked_at is null`
	switch {
	case !perPod:
	case login.PodUID == "":
		conflict = `(subject) where subject is not null and pod_uid is null and revoked_at is null`
	default:
		conflict = `(subject, pod_uid) where subject is not null and pod_uid is not null and revoked_at is null`
		// Migration 0028 added pod_uid empty, so the credential a pod's token made before
		// the per-pod schema is a live row of that subject carrying no pod, which the
		// conflict target just named cannot match. Left alone, the insert below would
		// collide with that row on launcher_credentials_live_key -- the key is the one the
		// running daemon is renewing with -- and be refused 409 KEY_HOLDS_LIVE_CREDENTIAL
		// for as long as the row exists, which expiry does not end, since that index covers
		// expired rows too. So the pod the token just proved claims the row first, found by
		// its subject and by the very key this login signed with: only the holder of that
		// key can, and the holder is the pod that made it. The row keeps its id, so the
		// upsert then finds it through (subject, pod_uid) and moves it as any later login of
		// that pod, and every session enrolled under it stays there. Nothing is claimed when
		// the pod already holds a live row of its own -- a pod presenting a second key --
		// which would be a (subject, pod_uid) collision of its own; the upsert decides that
		// login as it decides any other whose key a live credential holds.
		if _, err := tx.Exec(ctx, `update launcher_credentials set pod_uid=$1
			where subject=$2 and key_thumbprint=$3 and pod_uid is null and revoked_at is null
			  and not exists (select 1 from launcher_credentials held
				where held.subject=$2 and held.pod_uid=$1 and held.revoked_at is null)`,
			login.PodUID, login.Subject, login.Thumbprint); err != nil {
			return uuid.Nil, err
		}
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, subject, pod_uid, expires_at)
		values ($1, $2, $3, $4, $5, $6, null, $7, nullif($8::text,''), $9)
		on conflict `+conflict+`
		do update set operator=excluded.operator, service=excluded.service, host=excluded.host,
			key_thumbprint=excluded.key_thumbprint, public_jwk=excluded.public_jwk,
			pod_uid=excluded.pod_uid, expires_at=excluded.expires_at
		returning id`,
		uuid.New(), operator, login.Service, login.Host, login.Thumbprint, []byte(login.JWK), login.Subject, login.PodUID, login.Expires).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, actor, detail) values ('launcher_credential.token_login',$1,
		jsonb_strip_nulls(jsonb_build_object('credential_id',$2::text,'service',$3::text,'subject',$4::text,'pod_uid',nullif($5::text,''),
			'operator',nullif($6::text,''),'key_thumbprint',$7::text,'expires_at',$8::timestamptz,'token_expires_at',$9::timestamptz)))`,
		record.LauncherActor(id.String()), id.String(), login.Service, login.Subject, login.PodUID, login.Operator,
		login.Thumbprint, login.Expires, login.TokenExpires); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// legacySubjectIndex is 0015's one-live-credential-per-subject index, which migration 0029
// deliberately leaves in place and the release after it drops. While it exists the previous
// broker is still able to serve: its upsert names `on conflict (subject) where subject is not
// null and revoked_at is null`, which Postgres resolves against this index and against neither
// index 0029 adds, and a roll of the broker serves both generations at once.
const legacySubjectIndex = "launcher_credentials_live_subject"

// perPodLogins reports whether the schema tx runs on gives each pod of a subject a launcher
// credential of its own, which it does once legacySubjectIndex is gone: that index allows one
// live credential per subject whatever its pod, so while it is there a second pod's row cannot
// exist at all and naming a per-pod conflict target would only turn that into a unique
// violation. It is read per login rather than once at start because the index goes while this
// process is running — the next release's own pod migrates the database at its boot, with this
// one still serving — and a cached answer would name a conflict target the schema no longer has
// and fail with 42P10, which is the failure this whole staging exists to avoid. One catalog
// lookup on a route a service reaches a few times an hour.
//
// Reading it per login is not on its own enough: the drop could still land between this read
// and the statement that uses its answer. What makes the answer hold is the caller's lock on
// launcher_credentials, taken before this and held to commit (IssueTokenLoginTx), which `drop
// index` waits for. This answers only what the lock already holds still.
func perPodLogins(ctx context.Context, tx pgx.Tx) (bool, error) {
	var legacy bool
	if err := tx.QueryRow(ctx, `select to_regclass($1) is not null`, legacySubjectIndex).Scan(&legacy); err != nil {
		return false, err
	}
	return !legacy, nil
}

// NewChainVerifier builds the record.ChainVerifier AuthenticateLauncher's issuance-chain
// re-verification uses, scoped to launcher_credential records.
func NewChainVerifier(st *store.Store, audience string, skew time.Duration) *record.ChainVerifier {
	return st.ChainVerifier(record.KindLauncherCredential, audience, skew)
}

// AuthenticateLauncher answers proof.Verifier's LookupLauncher hook directly: given a launcher
// credential's own id (the "lid" claim of a proof.SignLauncher proof), it resolves the live,
// unexpired credential row and checks what vouches for it. A credential a person approved has its
// entire issuance chain re-verified through s.Chain — a launcher_credentials row is never trusted
// on its own, so a row inserted without a genuine approved credential-request record behind it
// never authenticates. A credential a token login issued (IssueTokenLoginTx, its subject set,
// and so its service, as the table's check constraint holds) authenticates while its subject is
// still listed for its service (tokenLoginListed), so removing an account from
// BROKER_SERVICE_LOGINS or BROKER_VM_SERVICES ends its credential at its next call. A missing,
// expired, revoked, chain-broken or no-longer-listed credential all answer ("", false, nil); only a
// genuine dependency failure is a non-nil error, matching Lookup's own contract.
func (s *Service) AuthenticateLauncher(ctx context.Context, lid string) (string, bool, error) {
	if _, err := uuid.Parse(lid); err != nil {
		return "", false, nil
	}
	var thumbprint string
	var recordID, service, subject *string
	var expiresAt time.Time
	err := s.Store.Pool.QueryRow(ctx, `select key_thumbprint, record_id, service, subject, expires_at from launcher_credentials where id=$1 and revoked_at is null`, lid).
		Scan(&thumbprint, &recordID, &service, &subject, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !expiresAt.After(time.Now()) {
		return "", false, nil
	}
	if subject != nil {
		if !s.tokenLoginListed(*subject, *service) {
			return "", false, nil
		}
		return thumbprint, true, nil
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

// tokenLoginListed reports whether a token login's subject still logs in as service: a machine
// and a person's sign-in on one while BROKER_VM_SERVICES still lists that box's Google account
// for that service, and a Kubernetes ServiceAccount while BROKER_SERVICE_LOGINS names it.
// Which of the three a subject is, and the account a machine's and a sign-in's carry, are one
// reading (exchange.ReadSubject).
//
// A machine's own test is the account alone, because the account is what a listed identity is:
// where the machine runs, and which of two machines of one account this is, were decided by the
// entry when the exchange minted the credential (exchange.Set.Google), and the subject keeps
// only enough to tell the machines apart (exchange.MachineSubject). So dropping an account from
// BROKER_VM_SERVICES ends every one of its machines' credentials at their next call, and
// narrowing an entry's pins takes effect at the next exchange, where the pins are read.
//
// A sign-in's subject carries the same account (exchange.PersonSubject), so the same test holds
// it to the same listed fleet: a person signs in from a box the deployment lists, and dropping
// the account ends their sign-in's credential with the box's own. What ends a sign-in the
// person has revoked is not this but the revoke itself, which ends that credential in the
// transaction it revokes the sign-in in (RevokePersonSignIn): "listed" is a question about the
// configuration, and a revoke is a fact about the row.
func (s *Service) tokenLoginListed(subject, service string) bool {
	if read := exchange.ReadSubject(subject); read.Kind != exchange.SubjectOther {
		listed, isListed := s.VMServicesByAccount[read.Account]
		return isListed && listed == service
	}
	listed, found := s.ServiceLogins[subject]
	return found && listed == service
}

// Credential loads a launcher credential's own identity (operator, service, host, and a token
// login's subject) by its id, for a handler that has already authenticated the caller via a
// launcher proof (proof.Subject's LauncherID, verified live by AuthenticateLauncher through
// proof.Verifier.LookupLauncher) and needs that identity to pass to Create or Revoke. err is a
// genuine dependency failure; a launcher id a live proof just verified vanishing before this read is
// treated the same way (never silently swallowed into an empty Credential).
func (s *Service) Credential(ctx context.Context, id string) (Credential, error) {
	var cred Credential
	if err := s.Store.Pool.QueryRow(ctx, `select id, operator, service, host, subject, coalesce(pod_uid,'') from launcher_credentials where id=$1`, id).
		Scan(&cred.ID, &cred.Operator, &cred.Service, &cred.Host, &cred.Subject, &cred.PodUID); err != nil {
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
// A pod token past its expiry is ErrPodTokenExpired, naming that expiry; any other token that does
// not prove this pod is ErrPodIdentity. Under a token login, whose whole authorization is the
// deployment's settings, the pod must be one of two (podUnderTokenLogin): the login's own pod,
// when the token proves the login's own account (ErrPodSelfEnrollOnly for any other pod of it),
// or a pod of an account BROKER_SERVICES binds to the login's service, which is the deployment
// saying that service launches those pods (ErrPodNotLaunched for anything else). A machine whose
// Google identity token the exchange
// accepted enrolls a session under the credential that exchange minted it, which is why it
// needs no kind and no token of its own here: the credential is the proof. Such a session
// records that credential's subject, which is what makes it its fleet's service's and no
// person's (requests.Machine.requester), as a pod's recorded service account makes it its
// service's.
// A credential revoked (RevokeCredential: by the person who approved a person's machine login, or
// by anyone signed in for a service's), even after the caller's launcher proof was verified, is
// ErrUnauthenticated.
//
// An enrollment that names a session also records the principal that session belongs to
// (carryPrincipal), and takes what every predecessor of that name under that principal still
// holds: its grants, its pending requests and its withholds move to this row, and a predecessor
// a box's end left dormant is finished by the move (adopt). That is how a grant follows a
// session to the next box it comes up on. It changes none of the identity checks above and
// nothing in the answer: an enrollment that names no session, one whose caller proved no
// principal and one with no predecessor each adopt nothing, an idempotent re-enrollment
// (Existing) adopts nothing, and a pod's token check and the pod-uid ratchet run exactly as they
// did. Only the rows move.
//
// Because a grant follows a name under one principal, the name has to belong to one principal,
// and before anything is adopted the enrollment binds its session name to that principal
// (bindSessionName): a name no principal holds becomes this one's, and a name another principal
// holds is ErrSessionHeld, the whole enrollment refused and nothing written. An enrollment that
// names no session, and one whose caller proved no principal, bind nothing and are refused
// nothing.
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
	if in.Kind == "session" && machineCredential(cred) {
		// A machine's session is its fleet's, and the credential's subject is what says which
		// fleet: the enrollment records it as a pod's records the account its token proved.
		in.Subject = cred.Subject
	}
	if in.Kind == "pod" {
		claims, err := s.verifyPodToken(ctx, in.PodToken)
		if err != nil {
			return Enrollment{}, err
		}
		if claims.PodUID != in.RuntimeID {
			return Enrollment{}, ErrPodIdentity
		}
		if err := s.podUnderTokenLogin(cred, claims, in.RuntimeID); err != nil {
			return Enrollment{}, err
		}
		in.Subject = &claims.Subject
	}
	// The other half of the pair a grant follows, read from what the caller has just proved and
	// written beside the session's name in the insert below (carryPrincipal).
	in.carry = carryPrincipal(cred, in)
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
// credential's enrollments, and is ended with them, or finds the credential revoked. Then it
// binds the enrollment's session name to its principal (bindSessionName), which is the one other
// thing that can refuse an attempt outright.
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
	if err := s.bindSessionName(ctx, tx, in); err != nil {
		return Enrollment{}, false, err
	}
	// pod_token_renewed_at starts null on every row, a pod's included: the first renewal carrying
	// the pod's token sets it, and from then on Renew refuses a token-less renewal from every live
	// enrollment naming that pod UID (podTokenRatcheted).
	_, err = tx.Exec(ctx, `insert into enrollments (id, kind, runtime_id, slot, operator, thumbprint, session_id, carry_principal, subject, launcher_credential_id, lease_expires_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		in.ID, in.Kind, in.RuntimeID, in.Slot, in.Operator, in.Thumbprint, in.SessionID, in.carry, in.Subject, cred.ID, in.LeaseExpires)
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
		in.ID, record.LauncherActor(cred.ID.String()), in.Kind, in.RuntimeID, in.Thumbprint, in.Slot); err != nil {
		return Enrollment{}, false, err
	}
	if err := adopt(ctx, tx, in, record.LauncherActor(cred.ID.String())); err != nil {
		return Enrollment{}, false, err
	}
	return in, false, tx.Commit(ctx)
}

// bindSessionName binds in's session name to the principal the enrollment belongs to (in.carry),
// in the enrollment's own transaction, which oauthstore.WithTx is how the binder joins: the name
// is this principal's from here on, and the binding and the enrollment commit or roll back
// together, so a refused name leaves no enrollment and a rolled-back enrollment leaves no
// binding.
//
// It is the same claim the token exchange makes for every token it mints
// (oauthstore.Store.BindSession) on the same table, which is the point: a Legion role's pod
// trades a token under its session name before or after it enrolls under that name, and the two
// claims are the same principal's (exchange.AccountPrincipal of the ServiceAccount subject its
// projected token proved, which carryPrincipal spells the same way), so neither refuses the
// other. A name another principal holds is ErrSessionHeld, naming the session and no holder.
//
// An enrollment with no principal beside its name binds nothing and is refused nothing, as it
// adopts nothing: that is an enrollment stating no session, and one whose credential proved no
// principal at all (carryPrincipal). The claim is made for the enrollment's lease, so the name
// stays this principal's for oauthstore.SessionRetention past the lease, as a token's name stays
// its principal's past the token.
//
// The claim is not fixed: the configuration fixes a name for a principal at the exchange's own
// entries (exchange.Set.Reservations), and an enrollment is no entry, so a name the
// configuration fixes for someone else refuses an enrollment exactly as it refuses a trade.
//
// Locks: this takes the credential's row (createAttempt, for share) and then the name's own row,
// which is the order the exchange takes them in too — it mints the launcher credential before it
// claims the name (oauth.Provider.claimSession) — so neither can deadlock with the other.
func (s *Service) bindSessionName(ctx context.Context, tx pgx.Tx, in Enrollment) error {
	if s.Sessions == nil || in.carry == nil {
		return nil
	}
	_, err := s.Sessions.BindSession(oauthstore.WithTx(ctx, tx), *in.SessionID, *in.carry, false, in.LeaseExpires)
	if errors.Is(err, oauthstore.ErrSessionHeld) {
		return fmt.Errorf("%w: %s. Enrol under a name of your own.", ErrSessionHeld, *in.SessionID)
	}
	if err != nil {
		return fmt.Errorf("bind the enrollment's session name: %w", err)
	}
	return nil
}

// carryPrincipal is the identity an enrollment belongs to, which the insert writes into
// enrollments.carry_principal: the half of the pair a grant follows that keeps one session name
// from reaching another principal's grant (adopt). It is read from what the caller has already
// proved and never from anything the launcher states, and every spelling is exchange's own, the
// broker's one definition of each, so that the name a session writes its Dispatch tokens under
// and the name its grant follows have the same holder.
//
//   - A pod belongs to the service account its own projected token proved
//     (exchange.AccountPrincipal), the same account requests.Machine.requester reads a pod's
//     service authority from.
//   - A session under a credential that carries an operator — a person's own box, and that
//     person's sign-in on a machine — belongs to that person (exchange.PersonPrincipal), so it
//     follows them from one box to the next.
//   - A machine's own session belongs to its fleet's Google account
//     (exchange.GooglePrincipal), which is what its credential's subject names
//     (machineCredential, exchange.ReadSubject).
//
// Anything else belongs to no principal at all: a null carries nothing, is adopted by nobody and
// adopts nothing. So does an enrollment that states no session, whatever its credential proved,
// because a principal with no name beside it is no pair.
func carryPrincipal(cred Credential, in Enrollment) *string {
	if in.SessionID == nil || *in.SessionID == "" {
		return nil
	}
	switch {
	case in.Kind == "pod" && in.Subject != nil:
		return nullable(exchange.AccountPrincipal(*in.Subject))
	case cred.Operator != nil:
		return nullable(exchange.PersonPrincipal(*cred.Operator))
	case machineCredential(cred):
		return nullable(exchange.GooglePrincipal(exchange.ReadSubject(*cred.Subject).Account))
	}
	return nil
}

// holdsForItsSession is the one SQL text for "enrollment e still holds something for its
// session": a live grant, or a request nobody has answered yet. Three statements read it, and
// they must read it identically — endEnrollment's probe, which decides whether an end leaves the
// row dormant instead of revoking what it holds, adopt's predecessor query, which decides which
// ended rows the next enrolment of the name takes a grant or a request from, and
// requests.enrollmentWouldGoDormant, the requests package's copy of endEnrollment's probe
// character for character, which decides whether an approval on a lapsed row the sweep has not
// ended yet mints a grant. Were one wider than another, an end could leave a row dormant that no
// successor ever adopts (its grants standing until the carry sweep finds them), adopt could take
// from a row an end revoked under it, or an approval could be granted onto a row the sweep then
// revokes. What holds this text and that copy together is
// requests.TestTheLapsedRowGrantRuleAnswersWhatTheEndThatFollowsDoes, which runs the copy and
// Service.EndLapsed over every shape of row and compares their answers, so an edit here that
// leaves the copy behind fails that test. All three splice it in with e as the enrollment, and
// each reads the whole pair beside it: the two probes test that the row names a session and
// records a principal, and adopt matches on both. adopt adds the withheld names outside this
// text, because a withhold follows the session too but is no part of what makes an end carry: it
// outlives an end of either kind (adopt's second part).
const holdsForItsSession = `exists (select 1 from grants g where g.enrollment_id=e.id and g.revoked_at is null and (g.expires_at is null or g.expires_at > now()))
				or exists (select 1 from requests r where r.enrollment_id=e.id and r.state='pending')`

// carriesForItsSession is "the carry left something on enrollment e for its session": it still
// holds a live grant or a pending request (holdsForItsSession) and no end has revoked them — it
// is live, or it is dormant, which is the end of a box that left what it held standing
// (endEnrollment). adopt reads it twice in one statement: as the first part of its predecessor
// rule, and as the flag saying whether that predecessor's grants and requests move beside its
// withheld names.
const carriesForItsSession = `(e.revoked_at is null or e.dormant_since is not null) and (` + holdsForItsSession + `)`

// adopt moves what every predecessor of this enrollment's (carry_principal, session_id) holds to
// the enrollment createAttempt has just inserted, inside that same transaction, revoking none of
// it. Both kinds of enrollment adopt, a session's and a pod's.
//
// A predecessor is another enrollment of the same pair, and the rule has two parts, because what
// a grant may follow and what a withhold must follow are not the same thing:
//
//   - An enrollment the carry left something on (carriesForItsSession) is a predecessor for
//     everything under it: its grants, its requests and its withheld names. It is one of two
//     things, a live enrollment, the box the session is moving off while that box is still up, or
//     a dormant one, a box whose end left what it held for the session to take (endEnrollment).
//     What that reading keeps out is a row ended for good, whose grants its own end revoked and
//     whose requests it cancelled: there is no live grant on it to follow the session.
//   - An enrollment of the pair that holds withheld names is a predecessor for those names,
//     whatever state its row is in — live, dormant, or ended for good. That last is the usual
//     case rather than a corner: an operator's revoke of an automatic grant writes the withheld
//     row and takes the grant, so the box is commonly left holding no live grant and no pending
//     request at all, its end is therefore terminal and its row never goes dormant. Were the
//     names left on it, the next enrolment of the session would find the secret automatic again
//     and a replaced box would undo what the operator said. Off a row ended for good, taking
//     them hands nothing over: nothing reaches the broker through an enrollment that is over, so
//     the names only ever sat there. Off a live predecessor it does hand something over, and
//     that is accepted here: requests.Machine reads a requester's withheld names by the
//     enrollment's own id, so the box the names move off asks for that name automatically again.
//     What bounds it is that a withhold binds a session name and never the principal behind it —
//     the same principal can start a session under a name it has not used and get a box with
//     nothing withheld on it at all — and that the session the operator took the name from is
//     left without it, because the names move to the enrollment that session runs on now.
//
// An enrollment of the pair that holds neither is no predecessor and is left exactly as it is,
// which is what keeps a second live session of one name from being ended by its neighbour's
// enrolment.
//
// What moves is the rows keyed by the predecessor: grants.enrollment_id and
// requests.enrollment_id from a predecessor of the first part, withheld_secrets.enrollment_id
// from a predecessor of either. The grants are what the session was approved for, the requests
// are what it is still waiting on a person for — an approval must land on the session as it
// stands when the answer comes, not on the box that asked — and the withholds are the one way a
// person takes a secret back from a session, which a move must not undo. A withheld name the
// successor already holds is kept once, since (enrollment_id, name) is that table's primary key.
// Off a first-part predecessor every grant and request moves, the ended grants and decided
// requests beside the live ones, so it is left holding nothing at all rather than a part of what
// it held; off a row ended for good none does, since its own end revoked and cancelled them, and
// moving those dead rows would only hang a grant.moved trail off grants that grant nothing.
//
// A dormant predecessor is then finished: dormant_since returns to null, because the row was
// kept only for what it held and that is here now. Its revoked_at stands where its own end wrote
// it, because the box ended then and not now. So it is no longer dormant, the carry sweep has
// nothing of it to revoke (EndCarried) and it is a predecessor of nothing. A live predecessor is
// left live: the box is still up and this is no end of it, and its own launcher ends it when the
// session's process goes (Revoke), by which time there is nothing under it to revoke. A
// predecessor already ended for good is left ended, the one thing that outlived its end taken
// off it. Nothing is revoked or cancelled here in any of the three, which is why this does not go
// through endEnrollment, whose terminal path would revoke grants the row no longer has.
//
// The trail names both enrollments: one enrollment.adopted row on the successor naming the
// predecessor it took from and how much, and one grant.moved row per grant, naming the grant, the
// successor it now hangs off and the predecessor it came from, so an operator asking why a
// session reads a secret it never asked this box for lands on the box that did.
//
// Locks, in the order this transaction takes them: the launcher credential's row (for share,
// taken by createAttempt before anything else), then the session name's own row
// (bindSessionName), then the enrollments rows, then the grant,
// request and withheld rows under them. The predecessors are taken for update, in id order, so
// no end, revoke or sweep moves what they hold while it is moving, and two concurrent adoptions
// of one pair cannot hold each other's first row. That is the order every other writer of these
// tables takes them in — endCredential takes the credential, then its enrollments in id order;
// Revoke, EndLapsed and EndCarried take an enrollment before the grants and requests under it;
// and this package's enrollment-then-request writers take the enrollment first too (ApplyDecision
// says so itself) — so none of them can deadlock with this one.
func adopt(ctx context.Context, tx pgx.Tx, in Enrollment, actor string) error {
	if in.carry == nil {
		return nil
	}
	// Both parts of the predecessor rule in one statement, over the pair. The second column says
	// which part a row matched by: a row the carry left something on gives up its grants and
	// requests as well as its withheld names, and a row that matched the withheld clause alone
	// gives up the names.
	rows, err := tx.Query(ctx, `select e.id, e.dormant_since is not null, `+carriesForItsSession+`
		from enrollments e
		where e.carry_principal=$1 and e.session_id=$2 and e.id <> $3
			and (`+carriesForItsSession+`
				or exists (select 1 from withheld_secrets w where w.enrollment_id=e.id))
		order by e.id for update of e`, *in.carry, *in.SessionID, in.ID)
	if err != nil {
		return err
	}
	type predecessor struct {
		ID      uuid.UUID
		Dormant bool
		// Carries is the first part of the rule: the carry left a live grant or a pending request
		// on this row, so its grants and requests move beside its withheld names. False is a row
		// matched by the withheld clause alone, which gives up its names and keeps the rest.
		Carries bool
	}
	predecessors, err := pgx.CollectRows(rows, pgx.RowToStructByPos[predecessor])
	if err != nil {
		return err
	}
	for _, from := range predecessors {
		var grants, requests int64
		if from.Carries {
			// One statement per table, each writing its own trail: every grant that moves gets its
			// row, so the insert's own count is how many grants moved.
			movedGrants, err := tx.Exec(ctx, `with moved as (
					update grants set enrollment_id=$2 where enrollment_id=$1 returning id
				)
				insert into audit (kind, enrollment_id, grant_id, actor, detail)
				select 'grant.moved', $2, id, $3, jsonb_build_object('from',$1::uuid,'to',$2::uuid) from moved`,
				from.ID, in.ID, actor)
			if err != nil {
				return err
			}
			movedRequests, err := tx.Exec(ctx, `update requests set enrollment_id=$2 where enrollment_id=$1`, from.ID, in.ID)
			if err != nil {
				return err
			}
			grants, requests = movedGrants.RowsAffected(), movedRequests.RowsAffected()
		}
		// The delete and the insert are one statement over one snapshot, so a name the successor
		// already holds leaves the predecessor's row deleted and the successor's as it was: the
		// withhold is kept once, from when it was first made.
		withheld, err := tx.Exec(ctx, `with moved as (
				delete from withheld_secrets where enrollment_id=$1 returning name, created_at
			)
			insert into withheld_secrets (enrollment_id, name, created_at)
			select $2, name, created_at from moved on conflict do nothing`, from.ID, in.ID)
		if err != nil {
			return err
		}
		if from.Dormant {
			if _, err := tx.Exec(ctx, `update enrollments set dormant_since=null where id=$1`, from.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ('enrollment.adopted',$1,$2,
			jsonb_build_object('from',$3::uuid,'to',$1::uuid,'dormant',$4::boolean,
				'grants',$5::bigint,'requests',$6::bigint,'withheld_secrets',$7::bigint))`,
			in.ID, actor, from.ID, from.Dormant, grants, requests, withheld.RowsAffected()); err != nil {
			return err
		}
	}
	return nil
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
	err = tx.QueryRow(ctx, `select id, thumbprint, lease_expires_at, enrollment_lease_live(lease_expires_at) from enrollments
		where launcher_credential_id=$1 and runtime_id=$2 and slot=$3 and revoked_at is null for update`, cred.ID, in.RuntimeID, in.Slot).
		Scan(&existing.ID, &existing.Thumbprint, &existing.LeaseExpires, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, true, nil
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	if !live {
		if _, err := endLapsed(ctx, tx, existing.ID.String()); err != nil {
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

// endMode says what an end does to what its enrollment still holds, and every caller states which
// it is, because the two kinds of end are about different things: an end of the box is not an end
// of the session that ran on it, and an end a person made is an end of both.
type endMode int

const (
	// endMayCarry leaves a session's grants and pending requests where they are, the enrollment
	// dormant, when it holds any and names a session. These are the ends a move makes: the lease
	// lapsing (EndLapsed), recoverConflict's end of the lapsed row holding the slot a fresh
	// enrollment wants, and the launcher's own revoke (Revoke) — the helper's when a session's
	// process ends, and the daemon's when it lets a role's process go
	// (packages/daemon/internal/supervise/secrets.go, RevokeEnrollment). A box is replaced far
	// more often than the session it runs, so none of these says the session is over.
	endMayCarry endMode = iota
	// endTerminal revokes the grants and cancels the pending requests, as every end did before
	// the carry. These are the ends that mean every secret this enrollment reached: a
	// machine-login revoke (endCredential, reached from RevokeCredential, RevokeOwnCredential and
	// RevokeReusedCredential), a person's revoke of their sign-in on a box (RevokePersonSignIn,
	// which ends that sign-in's credential the same way), and the carry sweep itself
	// (EndCarried), which is the end a dormant enrollment is waiting for. An operator's or an
	// approver's revoke of a grant is terminal too and never reaches here: it revokes the grant
	// itself (requests.Machine).
	endTerminal
)

// endedEnrollment is what one end did to what its enrollment held: how many live grants it
// revoked, how many pending requests it cancelled, and whether it left both where they were
// instead, the enrollment dormant.
type endedEnrollment struct {
	GrantsRevoked     int64
	RequestsCancelled int
	Dormant           bool
}

// endEnrollment ends enrollment id inside tx: it is marked revoked, every live grant under it is
// revoked, and every request still pending under it is cancelled — an approval must never land
// on a session that has ended — with its audit row and its record's cancelled event
// (store.EndPendingRequests, so no approver's pending list keeps showing it), and one kind row
// for the enrollment. It reports how many grants it revoked and requests it cancelled. The caller
// holds the enrollment row's lock.
//
// One end does none of that. An enrollment that names a session (session_id), records the
// principal that session belongs to (carry_principal), holds a live grant or a pending request,
// and is ended by an end of its box rather than by a person (endMayCarry), is marked revoked and
// dormant_since, and what it holds stays where it is for the next enrollment of that session
// name under the same principal to take. Ending the box's enrollment is how a session moves: a
// helper restarts, a machine is rebuilt, a person signs in on a second box, a Legion pod is
// replaced, and in each of those the box's enrollment ends while the session goes on and asks
// the broker for the same secret again. Both kinds carry, a session's and a pod's: a pod's
// enrollment is always ended before its successor enrols — by the daemon's launcher revoke when
// it lets the role's process go, or by EndLapsed one lease after the pod stopped renewing — so a
// pod carry that waited for a live predecessor would find nothing.
//
// The probe reads the principal as well as the name because dormancy is a promise that some
// later enrolment can take what the row still holds, and adopt matches on the whole pair
// (carry_principal, session_id): a row with no principal can be adopted by nobody, whatever its
// session_id says. Leaving such a row dormant would hold its grants unrevoked for
// BROKER_SESSION_CARRY_SECONDS with no one able to take them, and leave its pending request's
// card standing in an approver's Inbox where approving it is denied, since the enrollment behind
// it has ended and nothing will inherit it. So an enrollment with no principal ends as every end
// did before the carry. That is no corner of the deploy but the whole of it on the day it ships:
// every enrollment live when migration 0027 applies has carry_principal null, among them every
// daemon pod enrollment, which does carry a session_id
// (packages/daemon/internal/supervise/secrets.go). Each of those ends once, as it always did,
// and the enrolments that replace them record a principal and carry.
//
// The probe below is copied character for character into the requests package as
// requests.enrollmentWouldGoDormant, where an approval on a lapsed row no end has reached yet
// reads it to decide whether that approval mints a grant, so an edit to the probe has to be made
// there too. requests.TestTheLapsedRowGrantRuleAnswersWhatTheEndThatFollowsDoes runs that copy
// and Service.EndLapsed over every shape of row and compares their answers, and is what fails
// when the two drift.
//
// A dormant enrollment proves nothing from the moment it goes dormant, because its key died with
// its box: revoked_at is set, and Lookup, Get, Renew, SessionID, recoverConflict, liveCredentials
// and requests.Machine's own grant read all answer on exactly that. What it holds is reachable
// only through the next enrollment of its session name, and is revoked by EndCarried once
// BROKER_SESSION_CARRY_SECONDS has passed with no such enrollment.
func endEnrollment(ctx context.Context, tx pgx.Tx, id, actor, kind, reason string, mode endMode) (endedEnrollment, error) {
	var dormant bool
	if mode == endMayCarry {
		if err := tx.QueryRow(ctx, `select coalesce(e.session_id,'') <> '' and e.carry_principal is not null and (
				`+holdsForItsSession+`)
			from enrollments e where e.id=$1`, id).Scan(&dormant); err != nil {
			return endedEnrollment{}, err
		}
	}
	if dormant {
		if _, err := tx.Exec(ctx, `update enrollments set revoked_at=now(), dormant_since=now() where id=$1`, id); err != nil {
			return endedEnrollment{}, err
		}
		// The end's own audit row, with the one fact that makes it different: nothing under it was
		// revoked. An operator reading the trail of a session that moved sees which end left the
		// session's grants standing and when the window on them started.
		if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ($1,$2,$3,
			jsonb_build_object('reason',$4::text,'dormant',true))`, kind, id, actor, reason); err != nil {
			return endedEnrollment{}, err
		}
		return endedEnrollment{Dormant: true}, nil
	}
	// coalesce keeps the time the enrollment ended where a dormant end already wrote it: EndCarried
	// and a person's revoke each reach a row that ended hours ago, and what they finish is that
	// end, not a second one now. dormant_since returns to null because the row now holds nothing
	// any session could take.
	if _, err := tx.Exec(ctx, `update enrollments set revoked_at=coalesce(revoked_at, now()), dormant_since=null where id=$1`, id); err != nil {
		return endedEnrollment{}, err
	}
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where enrollment_id=$1 and revoked_at is null`, id, actor)
	if err != nil {
		return endedEnrollment{}, err
	}
	detail := "the requesting enrollment ended: " + reason
	cancelled, err := store.EndPendingRequests(ctx, tx, store.RequestsOfEnrollment, id, store.RequestEnd{
		State: "cancelled", Actor: actor, DecidedBy: &actor, Detail: detail, AuditDetail: map[string]any{"reason": detail},
	})
	if err != nil {
		return endedEnrollment{}, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ($1,$2,$3, jsonb_build_object('reason',$4::text))`, kind, id, actor, reason); err != nil {
		return endedEnrollment{}, err
	}
	return endedEnrollment{GrantsRevoked: tag.RowsAffected(), RequestsCancelled: len(cancelled)}, nil
}

// endLapsed ends an enrollment whose lease lapsed: endEnrollment with actor broker and an
// enrollment.expired audit row, for recoverConflict and the sweep alike. A lapse is an end of the
// box (endMayCarry), since a renewer that stopped is what a replaced box looks like from here.
func endLapsed(ctx context.Context, tx pgx.Tx, id string) (endedEnrollment, error) {
	return endEnrollment(ctx, tx, id, "broker", "enrollment.expired", "its lease lapsed", endMayCarry)
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
	// Dormant says the lapse left this enrollment's grants and pending requests where they were,
	// for the next enrollment of its session name to take (endEnrollment): the session is on
	// another box, or about to be, and GrantsRevoked and RequestsCancelled are both 0.
	Dormant bool
}

// lapsedBatch is how many lapsed enrollments EndLapsed ends in one transaction.
const lapsedBatch = 100

// EndLapsed ends every enrollment whose lease has lapsed and that nothing has ended yet: a pod that
// is gone, a box whose launcher stopped renewing, a machine session whose helper died. Each is ended
// as Revoke ends one (endEnrollment, actor "broker", an enrollment.expired audit row), so its
// grants are revoked and its pending requests cancelled and dropped from the approver's list,
// rather than waiting there for their own expiry with an approval that could grant nothing. The
// one lapse that revokes nothing is the lapse of an enrollment holding a named session's grants
// or pending requests, which goes dormant instead and is ended by EndCarried: a renewer that
// stopped is what a replaced box looks like from here, and the session is on another box by then.
//
// Lapsed means what Lookup means by not live: lease_expires_at no later than Postgres's now(), and
// not a lease that ran out while no broker was serving (enrollment_lease_live, migration 0019),
// which is never ended for that: the next ServingClock beat gives it the time back. A session that
// keeps renewing keeps its lease ahead of now() and is never selected, and Renew refuses an
// enrollment once its lease has lapsed, so ending one takes nothing a session could still use. Rows
// a concurrent Renew or Revoke has locked are skipped (the next sweep sees them as they are then),
// and an ended row has revoked_at set and is never selected again, so a sweep that finds nothing
// changes nothing. It works in batches of lapsedBatch, each one transaction, and returns what it
// ended.
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
		where revoked_at is null and lease_expires_at <= now() and not enrollment_lease_live(lease_expires_at)
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
		ended, err := endLapsed(ctx, tx, batch[i].ID)
		if err != nil {
			return nil, err
		}
		batch[i].GrantsRevoked, batch[i].RequestsCancelled, batch[i].Dormant = ended.GrantsRevoked, ended.RequestsCancelled, ended.Dormant
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return batch, nil
}

// DormantEnrollment is one dormant enrollment EndCarried ended for good.
type DormantEnrollment struct {
	ID                string
	Kind              string
	RuntimeID         string
	Slot              string
	SessionID         string
	DormantSince      time.Time
	GrantsRevoked     int64
	RequestsCancelled int
}

// carriedBatch is how many dormant enrollments EndCarried ends in one transaction.
const carriedBatch = 100

// EndCarried ends, for good, every dormant enrollment that has been dormant for carry or longer:
// it revokes the grants and cancels the pending requests that were left for the session to take
// (endEnrollment's terminal path, actor "broker"), with an enrollment.carry_expired audit row
// naming why. carry is BROKER_SESSION_CARRY_SECONDS, which requests.Sweeper holds; 0 ends every
// dormant enrollment the next sweep finds, which is how a deployment turns the carry off.
//
// Dormant is dormant_since is not null: an ended enrollment still holding what the next
// enrollment of its session name may take (endEnrollment). The column returns to null the moment
// the row holds nothing more for anyone, here or in the adoption, so nothing is ended twice and
// a sweep that finds nothing changes nothing. An enrollment is dormant only after it ended, so
// nothing a live session could still use is ever taken here: what passes is the window a person's
// approval waits in, not a lease.
//
// It follows EndLapsed's shape, for the same reasons: batches of carriedBatch, each one
// transaction, rows a concurrent writer has locked skipped for the next sweep, and what it ended
// returned so the sweeper can log each one.
func (s *Service) EndCarried(ctx context.Context, carry time.Duration) ([]DormantEnrollment, error) {
	var ended []DormantEnrollment
	for {
		batch, err := s.endCarriedBatch(ctx, carry)
		ended = append(ended, batch...)
		if err != nil || len(batch) < carriedBatch {
			return ended, err
		}
	}
}

func (s *Service) endCarriedBatch(ctx context.Context, carry time.Duration) ([]DormantEnrollment, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `select id, kind, runtime_id, slot, coalesce(session_id,''), dormant_since from enrollments
		where dormant_since is not null and dormant_since <= now() - make_interval(secs => $1)
		order by dormant_since limit $2 for update skip locked`, carry.Seconds(), carriedBatch)
	if err != nil {
		return nil, err
	}
	batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DormantEnrollment, error) {
		var e DormantEnrollment
		err := row.Scan(&e.ID, &e.Kind, &e.RuntimeID, &e.Slot, &e.SessionID, &e.DormantSince)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	for i := range batch {
		ended, err := endEnrollment(ctx, tx, batch[i].ID, "broker", "enrollment.carry_expired",
			"no enrollment of its session took what it held before the carry window passed", endTerminal)
		if err != nil {
			return nil, err
		}
		batch[i].GrantsRevoked, batch[i].RequestsCancelled = ended.GrantsRevoked, ended.RequestsCancelled
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return batch, nil
}

// Renewal is what Renew answers: the new lease, and, for a pod enrollment renewed without its
// projected token, which pod and slot did so (the API logs each such renewal).
type Renewal struct {
	LeaseExpires    time.Time
	WithoutPodToken bool
	RuntimeID, Slot string
}

// verifyPodToken verifies a pod's projected service-account token (issuer, audience, signature,
// expiry) and requires it to name a pod UID. Create and Renew both call it before any transaction
// opens, so the issuer's key-set fetch on an unknown key id never holds a row lock or a pooled
// connection. A token past its expiry is ErrPodTokenExpired, naming that expiry; every other
// failure, and a broker with no BROKER_K8S_OIDC_ISSUER, is ErrPodIdentity.
func (s *Service) verifyPodToken(ctx context.Context, raw string) (PodClaims, error) {
	if s.Pod == nil {
		return PodClaims{}, fmt.Errorf("%w: a pod's token needs BROKER_K8S_OIDC_ISSUER", ErrPodIdentity)
	}
	claims, err := s.Pod.Verify(ctx, raw)
	if errors.Is(err, oidc.ErrExpired) {
		return PodClaims{}, fmt.Errorf("%w (%v)", ErrPodTokenExpired, err)
	}
	if err != nil || claims.PodUID == "" {
		return PodClaims{}, ErrPodIdentity
	}
	return claims, nil
}

// Renew extends a live enrollment's lease by Lease. The caller's session proof says only that it
// holds the enrollment's key; a pod enrollment's renewal proves only that its renewer holds an
// unexpired token the issuer minted for that pod: no API-server call proves the pod still exists,
// so a deleted pod's enrollment can outlive it for the token's remaining life (at most 3600
// seconds) plus one lease. podToken is verified as Create verifies it (verifyPodToken, before the
// row is locked) and required to name the pod UID (runtime_id) and the service-account subject the
// enrollment recorded at Create, else ErrPodIdentity and the lease stays where it was. A token from
// another pod or account never renews this one. A machine session renews on its proof alone; a
// token on its renewal is ErrPodIdentity.
//
// A machine's own session renews only while the launcher credential that enrolled it is still
// live — ErrMachineLoginEnded otherwise — which is the one thing a session proof cannot say:
// the key and the enrollment id a machine's helper holds are files on its disk, and a copy of
// the pair taken off the box would otherwise renew for ever, long after the box was deleted or
// its fleet dropped from BROKER_VM_SERVICES. "Live" is what AuthenticateLauncher means by it,
// read in this transaction: not revoked, not past its expiry — which is the machine's Google
// identity token's, so a machine that stops being able to prove its identity
// stops renewing within the hour, as requiring a fresh identity token on every renewal once
// did — and its account still listed for its service. A revoke ends such a session outright
// (RevokeCredential), so what this adds is the two endings a revoke is not: the expiry, and the
// delisting. The session ends one lease later at most, swept as any lapsed one is (EndLapsed).
//
// A person's session and a pod's are deliberately not held this way. A pod's renewal already
// proves the pod's own projected token, and a person's sessions must outlive the hour their
// box's credential lives: a person signs in once per box and their sessions are theirs until
// they revoke the sign-in, which ends them at once (RevokePersonSignIn).
//
// The ratchet is keyed on the pod UID. Every pod enrollment starts with pod_token_renewed_at null,
// whether migration 0014 found it or Create made it, and the first renewal carrying an accepted
// token sets that row's column, the record that the pod's renewer sends its token. A renewal
// without a token is ErrPodTokenRequired once any pod enrollment naming the same runtime_id, live or
// ended, has the column set (podTokenRatcheted), whichever launcher credential made that row or this
// one; otherwise it still extends the lease and answers WithoutPodToken, which the API logs, naming
// the enrollment. So the copied-token gap closes for every pod UID a current-image pod has renewed
// under: a key enrolled under another login with a token copied out of that pod gets a row of its
// own (rows are unique per launcher credential, pod UID and slot), and that row is refused
// token-less as soon as any enrollment of the real pod has renewed with its token, whichever came
// first, and stays refused across the pod's role generations and after the pod is gone. What stays
// open: a copied token's renewals before the pod's own first token renewal (seconds while the
// daemon holds an approved machine login; after a daemon restart its pods enroll only once a person
// approves the new login, and a pod that ends before it enrolls never marks its UID), and every
// renewal of an old-image pod, whose renewer never sends a token, until the contract release. A pod
// UID names one pod, so a mark refuses nothing of another. Every deploy order keeps every pod
// renewing: an older broker ignores the token a newer client sends, and this broker accepts a
// token-less renewal of a pod UID no enrollment has marked. That bounds liveness, not exposure: a
// grant lasts until its session ends, so a copy renewing an unmarked UID keeps its grant while it
// renews, which the operate page's rollout closes by replacing old-image pods and revoking the
// logins of the copies left.
// The contract release removes the token-less path once
// `select count(*) from enrollments where kind='pod' and revoked_at is null and pod_token_renewed_at is null and created_at < now() - interval '1 minute'`
// answers 0, the broker has logged no "renewed without its projected token" line for one full
// BROKER_LEASE_SECONDS, and no issue pod runs a worker image older than the one whose renewer sends
// the token (an old-image issue pod between role generations holds no enrollment, so the count and
// the log miss it, yet its next phase enrolls an unmarked row). That release also drops
// podTokenRatcheted and its partial index, which only the ratchet needs.
func (s *Service) Renew(ctx context.Context, id, podToken string) (Renewal, error) {
	withToken := podToken != ""
	var claims PodClaims
	if withToken {
		var err error
		if claims, err = s.verifyPodToken(ctx, podToken); err != nil {
			return Renewal{}, err
		}
	}
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Renewal{}, err
	}
	defer tx.Rollback(ctx)

	var kind string
	var subject *string
	var cred Credential
	var credLive bool
	renewal := Renewal{}
	err = tx.QueryRow(ctx, `select e.kind, e.runtime_id, e.slot, e.subject,
			c.operator, c.service, c.subject, c.revoked_at is null and c.expires_at > now()
		from enrollments e join launcher_credentials c on c.id = e.launcher_credential_id
		where e.id=$1 and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at) for update of e`, id).
		Scan(&kind, &renewal.RuntimeID, &renewal.Slot, &subject, &cred.Operator, &cred.Service, &cred.Subject, &credLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return Renewal{}, ErrNotLive
	}
	if err != nil {
		return Renewal{}, err
	}
	// The row's own half of "live" is credLive, read above; what is left is the configuration's
	// half, the account still listed for the credential's service, which no column holds. cred
	// is one machineCredential has answered for, so its subject and its service are both set.
	if kind == "session" && machineCredential(cred) &&
		!(credLive && s.tokenLoginListed(*cred.Subject, *cred.Service)) {
		return Renewal{}, ErrMachineLoginEnded
	}
	switch {
	case kind != "pod" && withToken:
		return Renewal{}, ErrPodIdentity
	case kind == "pod" && withToken:
		if claims.PodUID != renewal.RuntimeID || subject == nil || claims.Subject != *subject {
			return Renewal{}, ErrPodIdentity
		}
	case kind == "pod":
		ratcheted, err := podTokenRatcheted(ctx, tx, renewal.RuntimeID)
		if err != nil {
			return Renewal{}, err
		}
		if ratcheted {
			return Renewal{}, ErrPodTokenRequired
		}
		renewal.WithoutPodToken = true
	}
	renewal.LeaseExpires = time.Now().Add(s.Lease)
	if _, err := tx.Exec(ctx, `update enrollments set lease_expires_at=$2,
		pod_token_renewed_at = case when $3 then coalesce(pod_token_renewed_at, now()) else pod_token_renewed_at end
		where id=$1`, id, renewal.LeaseExpires, kind == "pod" && withToken); err != nil {
		return Renewal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Renewal{}, err
	}
	return renewal, nil
}

// podTokenRatcheted reports whether any pod enrollment naming podUID has renewed with its token
// (pod_token_renewed_at set), live or ended. A pod UID is the API server's uid of one pod object and
// is never given to another pod, so every row naming it is that one pod's or a copy of its token,
// and a mark refuses nothing of any other pod. Ended rows count on purpose. A pod's enrollments end
// at every role generation (the daemon revokes each one it lets go), so between generations the pod
// can hold no live row at all; were only live rows read, a key enrolled under another login with a
// token copied out of the pod would renew token-less again in every such gap, and forever once the
// pod is gone, since a token-less renewal needs no token and the copied token's expiry stops
// mattering after Create. A later enrollment of the same pod (a new role generation, or under the
// daemon's next login) is the pod's own current-image renewer, which sends its token and is never
// refused. The partial index enrollments_pod_token_renewed (migration 0014) serves the probe.
func podTokenRatcheted(ctx context.Context, tx pgx.Tx, podUID string) (bool, error) {
	var ratcheted bool
	err := tx.QueryRow(ctx, `select exists(select 1 from enrollments
		where kind = 'pod' and runtime_id = $1 and pod_token_renewed_at is not null)`, podUID).Scan(&ratcheted)
	return ratcheted, err
}

// authorized reports whether cred may act on an enrollment of the given kind and operator — the
// trust boundary both Create (when creating one) and Revoke (when ending one) enforce: an
// operator credential may only act on that same operator's own sessions; a service credential
// (no operator) may only act on an enrollment that carries no operator either, a pod's or a
// machine's own session.
//
// A machine's credential, which the token exchange mints, names its fleet's service and never a
// person, whatever that fleet's entry says: secret authority follows the access token, so the
// machine's sessions reach its service's secrets and no person's, and a person reaches theirs on
// that machine only through their own sign-in. Its session is the one kind a service credential
// may enrol beside a pod, and only a machine's credential may: a Kubernetes service's login
// enrols the pods its tokens prove and nothing else.
func authorized(cred Credential, kind string, operator *string) bool {
	serviceKind := kind == "pod" || (kind == "session" && machineCredential(cred))
	switch {
	case cred.Operator != nil && (operator == nil || *operator != *cred.Operator):
		return false
	case cred.Operator != nil && serviceKind:
		return false
	case cred.Operator == nil && (!serviceKind || operator != nil):
		return false
	}
	return true
}

// podUnderTokenLogin holds a service's token login to the pods the deployment's settings give it,
// and lets every other credential through. A token login is authorized by those settings and by
// nothing else — no person approved it — so a projected token handed to it proves whose pod the
// token is and never who may enrol that pod. There are two pods it may enrol:
//
//   - its own, when the token proves the login's own account (cred.Subject). Only a pod running
//     as that account holds such a login, and the login belongs to the one pod whose token made
//     it (Credential.PodUID, from its kubernetes.io pod uid), so it enrols that pod and no other
//     pod of the account. A login from a token
//     bound to no pod has no pod of its own and so enrols none of them.
//
//   - a pod of an account BROKER_SERVICES binds to the login's own service, which is the
//     deployment saying that service launches those pods. That is how the Legion daemon enrols
//     its workers, and it is the same fact requests.Machine reads to decide an enrolled pod is
//     that service's, so nothing new has to be configured for a deployment that already serves a
//     service's secrets to its pods.
//
// A credential a person approved — a person's own machine login, or a service's login approved
// through a machine-login code, neither of which carries a subject — keeps that person's approval
// as its authorization and is not held to either rule.
func (s *Service) podUnderTokenLogin(cred Credential, claims PodClaims, runtimeID string) error {
	if cred.Operator != nil || cred.Subject == nil {
		return nil
	}
	if *cred.Subject == claims.Subject {
		if cred.PodUID != runtimeID {
			return fmt.Errorf("%w: this login is pod %q's, the enrollment names %q", ErrPodSelfEnrollOnly, cred.PodUID, runtimeID)
		}
		return nil
	}
	if !s.launches(cred.Service, claims.Subject) {
		service := ""
		if cred.Service != nil {
			service = *cred.Service
		}
		return fmt.Errorf("%w: BROKER_SERVICES does not bind %s to %q, the service %s logs in as", ErrPodNotLaunched, claims.Subject, service, *cred.Subject)
	}
	return nil
}

// launches reports whether BROKER_SERVICES binds podAccount, the ServiceAccount a pod runs as, to
// service, the one a login logs in as: the deployment's own statement that this service launches
// that pod. The one lookup, read by Create through podUnderTokenLogin and by Revoke through
// sameLauncher, so a pod a login may enrol is exactly a pod it may end.
func (s *Service) launches(service *string, podAccount string) bool {
	if service == nil {
		return false
	}
	bound, listed := s.ServicesByAccount[podAccount]
	return listed && bound == *service
}

// sameLauncher reports whether cred is the same launcher as the credential that made a pod
// enrollment, logging in again from another pod: the same ServiceAccount (both token logins of
// one subject), the same service, and a pod running as an account BROKER_SERVICES binds to that
// service — the launcher relation Create already reads (launches). It is what lets a replaced
// Legion daemon pod end the enrollments its predecessor made, since a machine login is per pod
// and the new pod holds a credential of its own.
//
// It gives a pod that enrolled itself nothing: the service a self-enrolling account logs in as
// binds no pod account, so launches is false for every such enrollment and one session pod can
// never end another's. It gives nothing across services either, and nothing to a credential a
// person approved, which carries no subject. enrolledLoginSubject, enrolledLoginService and
// enrolledPodAccount are the enrolling credential's subject and service and the enrollment's own
// verified pod account, each nil where the row has none.
func (s *Service) sameLauncher(cred Credential, enrolledLoginSubject, enrolledLoginService, enrolledPodAccount *string) bool {
	if cred.Operator != nil || cred.Subject == nil || enrolledLoginSubject == nil || enrolledPodAccount == nil {
		return false
	}
	if *enrolledLoginSubject != *cred.Subject {
		return false
	}
	if cred.Service == nil || enrolledLoginService == nil || *enrolledLoginService != *cred.Service {
		return false
	}
	return s.launches(cred.Service, *enrolledPodAccount)
}

// machineCredential reports whether cred is one the token exchange minted for a machine: a
// credential with a service, no person and a subject that reads as a machine's
// (exchange.SubjectMachine). A Kubernetes service's token login holds a subject of its own
// shape, a person's sign-in on a machine a subject of its own kind, and a person's machine
// login holds an operator.
func machineCredential(cred Credential) bool {
	if cred.Operator != nil || cred.Service == nil || cred.Subject == nil {
		return false
	}
	return exchange.ReadSubject(*cred.Subject).Kind == exchange.SubjectMachine
}

// Revoke ends the enrollment — revoking every live grant and cancelling every pending request
// under it — in one transaction. The one exception is the carry: an enrollment naming a session
// that still holds a live grant or a pending request goes dormant instead, holding both for that
// session's next enrollment (endEnrollment, endMayCarry). This is a launcher ending its own
// enrollment, which happens on every move: the helper revokes when a session's process ends, and
// the daemon revokes when it lets a role's process go
// (packages/daemon/internal/supervise/secrets.go, RevokeEnrollment), while the session itself
// comes up again on the next box.
//
// cred must be authorized for the target enrollment's own kind and
// operator — the same trust boundary Create enforces — checked before anything else, so a
// wrong-operator or wrong-kind caller can never revoke an enrollment it doesn't own, whether that
// enrollment is live, already revoked, or (were its id guessed rather than read back) merely
// plausible-looking. A row that does not exist at all has no operator/kind to check ownership
// against, so that case alone falls through to ErrNotLive below rather than ErrOperatorMismatch.
//
// A pod enrollment and a machine's own session carry no operator, so the operator cannot tell
// one service's sessions from another's: each is ended by the launcher credential that created
// it, and a pod enrollment otherwise only by the same launcher logging in again from another pod
// (sameLauncher), never by another service's credential, nor a second credential of the same
// service, nor another machine of the same fleet, and never by a pod that enrolled itself
// (ErrNotEnrollingCredential for every other caller). A launcher's machine login is per pod
// (IssueTokenLoginTx), so a restarted Legion daemon in the pod it already runs in keeps its
// credential, while a replaced daemon pod holds a credential of its own rather than its
// predecessor's; without that second rule one deploy of the daemon would leave every enrollment
// its previous pods made with nothing able to end it. sameLauncher gives the new credential its
// predecessor's pods and no others, since it takes the same ServiceAccount logging in and the
// pod's own account being one BROKER_SERVICES binds to that login's service — the launcher
// relation POD_NOT_LAUNCHED reads at Create. A service that binds no account, which is every
// service whose pods enrol themselves, gives its logins nothing here: no such pod can end
// another's enrollment. An
// enrollment neither rule reaches — a service login a person approved before token logins, or
// the daemon's credential before someone revoked it — ends when its pod stops renewing, one lease
// (BROKER_LEASE_SECONDS) after its renewer stops, or at once when a person revokes that credential
// (RevokeCredential). A box or host enrollment keeps the operator rule alone: a helper that logs in
// again revokes the enrollments its earlier credential made for the same operator.
//
// Once ownership passes, Revoke returns ErrNotLive, changing nothing, when the enrollment is
// already revoked — the guard that keeps the audit trail honest: without it a no-op call would
// still write an "enrollment.revoked" audit row for an enrollment that never transitioned.
func (s *Service) Revoke(ctx context.Context, cred Credential, id, by string) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var operator, enrolledSubject, enrolledLoginSubject, enrolledLoginService *string
	var kind string
	var enrolledBy uuid.UUID
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `select e.operator, e.kind, e.launcher_credential_id, e.revoked_at, e.subject, c.subject, c.service
		from enrollments e join launcher_credentials c on c.id = e.launcher_credential_id
		where e.id=$1 for update of e`, id).
		Scan(&operator, &kind, &enrolledBy, &revokedAt, &enrolledSubject, &enrolledLoginSubject, &enrolledLoginService)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotLive
	}
	if err != nil {
		return err
	}
	if !authorized(cred, kind, operator) {
		return ErrOperatorMismatch
	}
	if cred.Operator == nil && enrolledBy != cred.ID &&
		!(kind == "pod" && s.sameLauncher(cred, enrolledLoginSubject, enrolledLoginService, enrolledSubject)) {
		return ErrNotEnrollingCredential
	}
	if revokedAt != nil {
		return ErrNotLive
	}
	if _, err := endEnrollment(ctx, tx, id, by, "enrollment.revoked", "revoked by its launcher", endMayCarry); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LiveCredentials lists the machine logins person may list and revoke that can still reach a
// secret, newest first: every unrevoked launcher credential minted from a launcher_credential
// record, a service's whoever approved it, a person's own machine's only when it is the person
// whose record names them, and every service's token login (machine.Service.TokenLogin), while it
// is unexpired or a session it enrolled is live as Lookup means it: not revoked, its lease not
// lapsed. A login's sessions outlive its expiry, since each renews with its own key and Renew
// reads the credential only for a machine's own session, so an expired login stays listed,
// Expired set, until its last session ends or lapses (a lapsed session renews no more, swept or
// not): revoking every listed login ends every session the person's machines started. A
// machine's own login is the exception its sessions make: they stop renewing at its expiry and
// lapse within one lease. A service's login, such as the Legion daemon's, has no
// operator and acts as no person, so anyone signed in to Dispatch lists it and revokes it, its
// Service set. Each login names who approved it, the login its approved event records, or "" for a
// token login, which no person approved. The sentinel record.AnyoneApprover and an empty login list
// nothing. OwnLiveCredentials lists the person's own machines' alone.
//
// A person's sign-in on a machine holds a launcher credential too, and that one is never listed
// here: it is one person's, it is already listed as that person's sign-in
// (store.Store.PersonSignIns), and ending it alone would end nothing, since the sign-in row
// behind it mints it again at the box's next trade. Its subject says so
// (exchange.PersonSubject), which is what signInCredential reads.
func (s *Service) LiveCredentials(ctx context.Context, person string) ([]LiveCredential, error) {
	return s.liveCredentials(ctx, person, true)
}

// OwnLiveCredentials is LiveCredentials without any service's login: the logins of the person's
// own machines alone, those minted from a launcher_credential record that names them, as the
// person's own machine lists them (GET /v1/operator/machines).
func (s *Service) OwnLiveCredentials(ctx context.Context, person string) ([]LiveCredential, error) {
	return s.liveCredentials(ctx, person, false)
}

// signInCredential reports whether a launcher credential's subject is a person's sign-in on a
// machine rather than a machine's own identity or a Kubernetes account: the one fact that tells
// the sign-ins list's credentials from the machine-logins list's, written by
// exchange.PersonSubject and read back by exchange.ReadSubject. nil is a credential a person
// approved, which has no subject at all.
func signInCredential(subject *string) bool {
	if subject == nil {
		return false
	}
	return exchange.ReadSubject(*subject).Kind == exchange.SubjectSignIn
}

// liveCredentials is LiveCredentials, every service's login among them only when services is set.
func (s *Service) liveCredentials(ctx context.Context, person string, services bool) ([]LiveCredential, error) {
	person = record.CanonicalLogin(person)
	if person == "" || person == record.AnyoneApprover {
		return nil, nil
	}
	rows, err := s.Store.Pool.Query(ctx, `select c.id, c.host, c.service, c.subject,
			coalesce((select ev.login from credential_request_events ev where ev.record_id = r.id and ev.event = 'approved'), ''),
			c.created_at, c.expires_at, c.expires_at <= now() from launcher_credentials c
		left join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential'
		where ((c.service is null and r.approver = $1) or ($2 and c.service is not null and (r.id is not null or c.subject is not null)))
			and c.revoked_at is null
			and (c.expires_at > now() or exists (select 1 from enrollments e where e.launcher_credential_id = c.id and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at)))
		order by c.created_at desc, c.id`, person, services)
	if err != nil {
		return nil, err
	}
	live, err := pgx.CollectRows(rows, pgx.RowToStructByPos[LiveCredential])
	if err != nil {
		return nil, err
	}
	// A sign-in's credential names its fleet's service, so the query above would take it for a
	// service's login. Which list a credential belongs on is its subject's to say, and the
	// subject is read here rather than matched in SQL so that exchange.PersonSubject's shape is
	// written down once.
	return slices.DeleteFunc(live, func(c LiveCredential) bool { return signInCredential(c.Subject) }), nil
}

// RevokeCredential ends launcher credential id on the word of person, expired or not: from then on
// no launcher proof signed with it authenticates (AuthenticateLauncher), and every enrollment it
// made that has not ended — a person's machine sessions and boxes, or every pod a service's login
// enrolled, which outlive the credential's expiry — and every dormant one it made, holding a
// moved session's grants, is ended terminally (endEnrollment, an
// enrollment.revoked audit row), revoking each one's grants and cancelling its pending requests,
// all in one transaction with one launcher_credential.revoked audit row. person may revoke a
// service's login whoever approved it, a service's token login (which no person approved, and whose
// service logs in again with its token under a new credential unless BROKER_SERVICE_LOGINS no
// longer names its account), and a person's own machine's only as the approver of the
// launcher_credential record it was minted from (ErrNotApprover); the sentinel
// record.AnyoneApprover and an empty login revoke nothing. An unknown id, like a credential with
// neither a record nor a token login's subject, is ErrNoCredential, and so is a person's
// sign-in's credential, which is no machine login: it is ended whole, with the sign-in row that
// mints it, by RevokePersonSignIn, and ending it here would leave that row to mint it again at
// the box's next trade. actor
// is what every row it writes records as having done it, which the caller proved:
// "human:<person>" for a revoke Dispatch relays from its signed-in person,
// "launcher:<credential id>" for one the person's own machine login makes. Revoking a credential
// already revoked succeeds and changes nothing. It takes the
// credential's row before its enrollments' rows, so an enrollment Create is inserting under the
// credential (which holds the row for share) commits first and is ended here, or waits and finds
// the credential revoked; and it takes those enrollments' rows, so a launcher's own Revoke of one
// either commits first, leaving it out of the list here, or waits and finds it ended.
func (s *Service) RevokeCredential(ctx context.Context, id, person, actor string) error {
	return s.revokeCredential(ctx, id, person, actor, true)
}

// RevokeOwnCredential is RevokeCredential for the logins OwnLiveCredentials lists: a service's
// login is ErrNoCredential, as an unknown id is, and ends nothing.
func (s *Service) RevokeOwnCredential(ctx context.Context, id, person, actor string) error {
	return s.revokeCredential(ctx, id, person, actor, false)
}

// revokeCredential is RevokeCredential, a service's login revocable only when services is set.
func (s *Service) revokeCredential(ctx context.Context, id, person, actor string, services bool) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var approver, host string
	var service, subject *string
	var revokedAt *time.Time
	// A token login's credential has no record, so no approver: as every service's login, anyone
	// signed in may revoke it (record.MayDecide), and its service logs in again with its token at
	// its next call, under a new credential, unless BROKER_SERVICE_LOGINS no longer names its account.
	err = tx.QueryRow(ctx, `select coalesce(r.approver, ''), c.service, c.subject, c.host, c.revoked_at from launcher_credentials c
		left join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential'
		where c.id = $1 and (r.id is not null or c.subject is not null) for no key update of c`, id).Scan(&approver, &service, &subject, &host, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoCredential
	}
	if err != nil {
		return err
	}
	// A sign-in's credential names its fleet's service and no record, so the rule below would
	// read it as a service's login and let anyone signed in end it. It is one person's, it is
	// listed as that person's sign-in and not here (liveCredentials), and the revoke that means
	// anything is RevokePersonSignIn's, which ends the sign-in row with it; so this route does
	// not find it, exactly as it does not find a login of another kind than it lists.
	if signInCredential(subject) {
		return ErrNoCredential
	}
	var svc string
	if service != nil {
		if !services {
			return ErrNoCredential
		}
		svc = *service
	}
	if !record.MayDecide(record.KindLauncherCredential, approver, svc, person) {
		return ErrNotApprover
	}
	if revokedAt != nil {
		return nil
	}
	if err := endCredential(ctx, tx, id, host, service, actor, "its machine login was revoked"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RetireCredential ends launcher credential id as the login a newer login of its machine replaced,
// in one transaction: its expiry becomes now, so its key enrolls nothing more and its refresh is
// refused, and its OAuth access and refresh tokens are deleted, so a retired refresh token
// presented later (an old copy of the machine file, a late answer to an abandoned refresh) is a
// plain invalid_grant rather than a reuse that would revoke the credential. Unlike a revoke it
// leaves every enrollment it made running: a session renews with its own proof, which an expired
// credential's sessions already do. The one exception is a machine's own session, whose renewal
// is held to a live login (Renew) and which therefore lapses within one lease of a retire; no
// machine's credential is ever retired, since a retire follows a device-grant machine login that
// a machine proving its own identity never makes.
// It writes one launcher_credential.retired audit row as actor.
// A credential already revoked or past its expiry changes nothing; an unknown id is
// ErrNoCredential. It takes the credential's row as a revoke does, so an enrollment Create holding
// it for share commits first.
func (s *Service) RetireCredential(ctx context.Context, id, actor string) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var host string
	var service *string
	var live bool
	err = tx.QueryRow(ctx, `select host, service, revoked_at is null and expires_at > now() from launcher_credentials
		where id = $1 for no key update`, id).Scan(&host, &service, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoCredential
	}
	if err != nil {
		return err
	}
	if !live {
		return nil
	}
	if _, err := tx.Exec(ctx, `update launcher_credentials set expires_at = now() where id = $1`, id); err != nil {
		return err
	}
	if err := oauthstore.DeleteCredentialTokens(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, actor, detail) values ('launcher_credential.retired',$1,
		jsonb_strip_nulls(jsonb_build_object('credential_id',$2::text,'host',$3::text,'service',$4::text)))`, actor, id, host, service); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReuseActor is the actor RevokeReusedCredential records: the broker, on finding a rotated
// refresh token redeemed after its grace.
const ReuseActor = "broker:refresh-token-reuse"

// RevokeReusedCredential revokes launcher credential id in tx, as RevokeCredential does, because
// a refresh token of its machine login was redeemed after it had been rotated and its grace had
// passed: someone else holds a copy of the login, and the key it holds keeps enrolling sessions
// until the credential itself ends. Every row it writes names ReuseActor. It takes the
// credential's row first, as RevokeCredential does. A credential already revoked, or unknown,
// changes nothing. It is the broker's oauthstore.Store.Reused.
func RevokeReusedCredential(ctx context.Context, tx pgx.Tx, id string) error {
	var host string
	var service *string
	var revokedAt *time.Time
	err := tx.QueryRow(ctx, `select host, service, revoked_at from launcher_credentials where id = $1 for no key update`, id).Scan(&host, &service, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && revokedAt != nil {
		return nil
	}
	if err != nil {
		return err
	}
	return endCredential(ctx, tx, id, host, service, ReuseActor, "its machine login's refresh token was reused")
}

// RevokePersonSignIn ends the sign-in id of person — the sign-in `agent-secrets login` made on
// one box — and everything that sign-in still holds, in one transaction, and answers how many
// of that person's live access tokens it ended. An id no sign-in of that person has is
// store.ErrNoSignIn; a sign-in already revoked changes nothing and answers no error, as
// revoking a machine login twice does.
//
// Three things end together, because a person revoking a sign-in means all three and would be
// surprised by any one of them outliving the others: the row, so no later trade of that box key
// is answered (store.RevokePersonSignInTx, which refuses it by name); the access tokens already
// minted under it, so none goes on writing Dispatch for the rest of its life; and the launcher
// credential that sign-in's trades minted, so no launcher proof signed with the box key
// authenticates again (AuthenticateLauncher) and every session it enrolled ends with its grants
// and pending requests, exactly as revoking a machine login ends them (endCredential).
//
// The credential is found by the box key's thumbprint, which names it exactly: a live
// credential holds one key (launcher_credentials_live_key), and that key is the one
// `agent-secrets login` made for this sign-in and no other. A sign-in whose box has not traded
// since it was recorded holds no credential yet, and nothing is ended but the row.
func (s *Service) RevokePersonSignIn(ctx context.Context, id, person, actor string) (int, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	thumbprint, ended, err := store.RevokePersonSignInTx(ctx, tx, id, person)
	if err != nil {
		return 0, err
	}
	if thumbprint != "" {
		var credential, host string
		var service *string
		err := tx.QueryRow(ctx, `select id, host, service from launcher_credentials
			where key_thumbprint = $1 and revoked_at is null for no key update`, thumbprint).Scan(&credential, &host, &service)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return 0, err
		default:
			if err := endCredential(ctx, tx, credential, host, service, actor, "the person revoked this machine's sign-in"); err != nil {
				return 0, err
			}
		}
	}
	return ended, tx.Commit(ctx)
}

// endCredential revokes launcher credential id in tx, whose row the caller holds: it sets
// revoked_at, ends its OAuth tokens, ends every enrollment it made that has not ended
// (endEnrollment, reason), and writes one launcher_credential.revoked audit row, each as actor.
//
// A person revoking a machine login, or their sign-in on a box, means every secret that login
// reached, so each of its enrollments ends terminally: the grants are revoked and the pending
// requests cancelled, with no carry (endTerminal). The dormant enrollments it made end here too,
// which is why the read below takes them beside the live ones: a dormant row has revoked_at set
// and still holds its session's grants, and leaving them would let the next enrollment of that
// session name, on a box the person has not revoked, adopt a grant the person has just taken
// back.
func endCredential(ctx context.Context, tx pgx.Tx, id, host string, service *string, actor, reason string) error {
	if _, err := tx.Exec(ctx, `update launcher_credentials set revoked_at=now() where id=$1`, id); err != nil {
		return err
	}
	// A device login's tokens name the credential, so its next refresh is refused too.
	if err := oauthstore.EndCredential(ctx, tx, id); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `select id from enrollments where launcher_credential_id=$1 and (revoked_at is null or dormant_since is not null) order by id for update`, id)
	if err != nil {
		return err
	}
	ended, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, enrollment := range ended {
		if _, err := endEnrollment(ctx, tx, enrollment, actor, "enrollment.revoked", reason, endTerminal); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `insert into audit (kind, actor, detail) values ('launcher_credential.revoked',$1,
		jsonb_strip_nulls(jsonb_build_object('credential_id',$2::text,'host',$3::text,'service',$4::text,'enrollments',coalesce(to_jsonb($5::text[]),'[]'::jsonb))))`,
		actor, id, host, service, ended)
	return err
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
	err := s.Store.Pool.QueryRow(ctx, `select thumbprint, revoked_at is null and enrollment_lease_live(lease_expires_at)
		from enrollments where id=$1`, id).Scan(&thumbprint, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return thumbprint, live, nil
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

// Replay records a jti, answering false when it was already recorded. It runs in the transaction
// ctx carries (oauthstore.TxFrom), as machine.Service.LoginDevice records a device login's request
// object in the OAuth provider's transaction, and on the pool when ctx carries none, so a caller
// holding a transaction never waits for a second pooled connection. A jti recorded on the pool
// also prunes the expired ones, so the table stays bounded without a separate job; one recorded in
// a transaction prunes nothing, since the prune's row locks would hold every concurrent caller's
// prune behind that transaction until it ends.
func (s *Service) Replay(ctx context.Context, jti string, expires time.Time) (bool, error) {
	var db execer = s.Store.Pool
	tx, inTx := oauthstore.TxFrom(ctx)
	if inTx {
		db = tx
	}
	tag, err := db.Exec(ctx, `insert into proof_jtis (jti, expires_at) values ($1,$2) on conflict (jti) do nothing`, jti, expires)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if !inTx {
		_, _ = s.Store.Pool.Exec(ctx, `delete from proof_jtis where expires_at < now()`)
	}
	return true, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
