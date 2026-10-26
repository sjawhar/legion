// Package approval holds the broker's own approval challenges: one row per pending record a
// person has been sent to the broker's own page to approve with their own Google sign-in, and the
// pages that page is rendered from (page.go).
//
// A challenge is what makes the decision the person's rather than the calling service's. The
// service that relays Dispatch's pages opens one by naming a record and the login it believes is
// deciding (POST /v1/credential-requests/{record}/approval), which proves nothing on its own; the
// person then signs in to the issuer the broker alone holds a client of, and the broker compares
// the email that sign-in returns with the challenge's approver before it applies anything. So a
// service's own credential opens a challenge nobody has to honour, and only the person named on it
// can spend it.
//
// A challenge is single-use and short-lived: Spend marks it used in the statement that reads it,
// so two callbacks of one challenge decide once, and a challenge nobody spends expires. A spent
// one stays, naming the issuer and the subject of the sign-in that spent it, which is the broker's
// own record that a decision carried a person's sign-in rather than a caller's word.
package approval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/store"
)

// DefaultLifetime is how long a challenge lives when a Service names none: long enough for a
// person to read the page, pick an account and sign in, short enough that a link left in a chat
// or a browser history is spent or dead by the time anyone else opens it.
const DefaultLifetime = 10 * time.Minute

// The three ways a challenge cannot be spent. Each is a page of its own (page.go), and none
// repeats anything of the sign-in.
var (
	// ErrNotFound is an id no challenge has: a mistyped link, or one from a database this broker
	// does not serve.
	ErrNotFound = errors.New("approval: no such approval")
	// ErrUsed is a challenge that has already decided its record.
	ErrUsed = errors.New("approval: this approval has already been used")
	// ErrExpired is a challenge whose window has passed; the record can be approved again from
	// Dispatch, which opens a new one.
	ErrExpired = errors.New("approval: this approval has expired")
)

// Challenge is one approval a person was sent to the broker's page for.
type Challenge struct {
	// ID is the challenge's own id, the last segment of the page's URL: 32 bytes of randomness,
	// so it is unguessable, and no part of it is derived from the record.
	ID string
	// RecordID is the credential-request record the challenge decides.
	RecordID string
	// Kind is that record's kind, record.KindAgentSecret or record.KindLauncherCredential.
	Kind string
	// Approver is the canonical login the sign-in must return; no other person may spend it.
	Approver string
	// CodeHash is the SHA-256 of a machine login's confirmation code as its record stores it, and
	// nil for an agent_secret record. The code itself is never kept here: the person typed it in
	// Dispatch, the broker checked it when the challenge was opened, and the callback reads the
	// record's own code back through that hash (machine.Service.CodeFor).
	CodeHash  []byte
	CreatedAt time.Time
	ExpiresAt time.Time
	// UsedAt is when the challenge decided its record; zero while it has not.
	UsedAt time.Time
	// SignedInIssuer and SignedInSubject are the issuer and the subject (sub) of the ID token the
	// person signed in with, written when the challenge was spent: the broker's own record of
	// whose sign-in a decision carried, which nothing but a sign-in through the broker's own
	// client can write. Empty while the challenge has not been spent.
	SignedInIssuer  string
	SignedInSubject string
	// SignedInAuthTime is that token's auth_time, when the person last authenticated to the
	// issuer, recorded as the issuer stated it; zero while the challenge has not been spent, or
	// when the token carried none.
	SignedInAuthTime time.Time
}

// Service reads and writes the approval challenges of one broker.
type Service struct {
	Store *store.Store
	// Lifetime is how long a challenge it opens lives; DefaultLifetime when zero.
	Lifetime time.Duration
}

func (s *Service) lifetime() time.Duration {
	if s.Lifetime <= 0 {
		return DefaultLifetime
	}
	return s.Lifetime
}

// Open writes a challenge for recordID, to be spent by approver alone, and answers it. codeHash is
// a machine login's CodeHash and nil for a request for secrets. The caller has already checked
// that the record is pending and that approver may decide it; this only records what the person's
// sign-in will be compared against.
//
// It also removes the challenges that expired over pruneAfter ago without being spent, so a
// deployment's table holds the live ones and the spent ones, which are its record of who signed
// in, and not every link anyone ever opened and abandoned.
func (s *Service) Open(ctx context.Context, recordID, kind, approver string, codeHash []byte) (Challenge, error) {
	id, err := NewID()
	if err != nil {
		return Challenge{}, err
	}
	now := time.Now().UTC()
	challenge := Challenge{
		ID: id, RecordID: recordID, Kind: kind, Approver: approver, CodeHash: codeHash,
		CreatedAt: now, ExpiresAt: now.Add(s.lifetime()),
	}
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Challenge{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into approval_challenges (id, record_id, kind, approver, code_hash, created_at, expires_at)
		values ($1,$2,$3,$4,$5,$6,$7)`, challenge.ID, recordID, kind, approver, codeHash, challenge.CreatedAt, challenge.ExpiresAt); err != nil {
		return Challenge{}, err
	}
	if _, err := tx.Exec(ctx, `delete from approval_challenges where used_at is null and expires_at < $1`, now.Add(-pruneAfter)); err != nil {
		return Challenge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Challenge{}, err
	}
	return challenge, nil
}

// pruneAfter is how long past its expiry an unspent challenge is kept before Open removes it: an
// hour, so a row is still there to explain a link a person opened just after it lapsed.
const pruneAfter = time.Hour

// Read answers the challenge id names, whatever its state: ErrNotFound for an id no challenge
// has. The page shows what a challenge covers before anyone signs in, so it reads a used or an
// expired one too and says so (ErrUsed and ErrExpired are Spend's).
func (s *Service) Read(ctx context.Context, id string) (Challenge, error) {
	challenge := Challenge{ID: id}
	var usedAt, authTime *time.Time
	var issuer, subject *string
	err := s.Store.Pool.QueryRow(ctx, `select record_id, kind, approver, code_hash, created_at, expires_at, used_at,
			signed_in_issuer, signed_in_subject, signed_in_auth_time
		from approval_challenges where id=$1`, id).
		Scan(&challenge.RecordID, &challenge.Kind, &challenge.Approver, &challenge.CodeHash,
			&challenge.CreatedAt, &challenge.ExpiresAt, &usedAt, &issuer, &subject, &authTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return Challenge{}, ErrNotFound
	}
	if err != nil {
		return Challenge{}, err
	}
	if usedAt != nil {
		challenge.UsedAt = *usedAt
	}
	if issuer != nil {
		challenge.SignedInIssuer = *issuer
	}
	if subject != nil {
		challenge.SignedInSubject = *subject
	}
	if authTime != nil {
		challenge.SignedInAuthTime = *authTime
	}
	return challenge, nil
}

// Spend marks the challenge id names used, in the one statement that reads it, and answers it with
// the sign-in recorded on it. Only an unspent, unexpired challenge is spent, so two callbacks of
// one challenge decide once: the second is ErrUsed. issuer, subject and authTime are the iss, sub
// and auth_time of the ID token the person signed in with; a zero authTime is recorded as none.
//
// A caller applies the decision after Spend has returned, so a decision that then fails leaves the
// challenge spent: the person starts again from Dispatch, which is the safe way round. The other
// order would let a crash between the two apply one decision twice.
func (s *Service) Spend(ctx context.Context, id, issuer, subject string, authTime, now time.Time) (Challenge, error) {
	challenge := Challenge{ID: id, SignedInIssuer: issuer, SignedInSubject: subject, SignedInAuthTime: authTime}
	var recordedAuthTime *time.Time
	if !authTime.IsZero() {
		utc := authTime.UTC()
		recordedAuthTime = &utc
	}
	err := s.Store.Pool.QueryRow(ctx, `update approval_challenges
		set used_at=$2, signed_in_issuer=$3, signed_in_subject=$4, signed_in_auth_time=$5
		where id=$1 and used_at is null and expires_at > $2
		returning record_id, kind, approver, code_hash, created_at, expires_at, used_at`,
		id, now.UTC(), issuer, subject, recordedAuthTime).
		Scan(&challenge.RecordID, &challenge.Kind, &challenge.Approver, &challenge.CodeHash,
			&challenge.CreatedAt, &challenge.ExpiresAt, &challenge.UsedAt)
	if err == nil {
		return challenge, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Challenge{}, err
	}
	// The update matched nothing: the row is gone, already spent, or expired. Reading it says
	// which, so the page tells the person what to do rather than "no".
	existing, readErr := s.Read(ctx, id)
	switch {
	case readErr != nil:
		return Challenge{}, readErr
	case !existing.UsedAt.IsZero():
		return Challenge{}, ErrUsed
	default:
		return Challenge{}, ErrExpired
	}
}

// NewID is a challenge's own id: 32 bytes of randomness, URL-safe.
func NewID() (string, error) {
	return randomValue(32)
}

// NewBinding is the value the browser's own cookie carries: 32 bytes of randomness, URL-safe,
// which the sign-in's nonce is derived from (Nonce). It never reaches the issuer, so only the
// browser that started the sign-in can produce the nonce the ID token has to carry.
func NewBinding() (string, error) {
	return randomValue(32)
}

func randomValue(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("approval: read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Nonce is the nonce the sign-in for challenge id and cookie value binding asks the issuer to put
// in its ID token: the SHA-256 of the two, so the token proves the browser that completes the
// sign-in is the one that started it, and the broker keeps nothing per sign-in to check it. A
// browser that lost or replaced its cookie cannot produce it.
func Nonce(id, binding string) string {
	sum := sha256.Sum256([]byte("agent-secrets-approval/v1\x00" + id + "\x00" + binding))
	return hex.EncodeToString(sum[:])
}

// CookieName is the name of the cookie that carries challenge id's binding: one name per
// challenge, so a person with two approvals open in one browser starts the second without
// overwriting the first's binding, and each callback reads its own. The name is a short hash of
// the id rather than the id itself, so the id — the approval's capability — is not repeated in
// every Cookie header the browser sends.
//
// On a broker reached over https (secure) the name carries the __Host- prefix, which a browser
// accepts only on a cookie that is Secure, has Path=/ and names no Domain. That is what stops a
// sibling host of the broker's registrable domain from planting a binding of its own choosing —
// it could otherwise set one for the parent domain with a longer path, which a browser sends
// first, compute the nonce from it, and send the person's browser to the issuer itself. A local
// http broker cannot use the prefix, since a Secure cookie is never sent back over http, and runs
// without that protection.
func CookieName(id string, secure bool) string {
	sum := sha256.Sum256([]byte("agent-secrets-approval-cookie/v1\x00" + id))
	name := "approval_binding_" + hex.EncodeToString(sum[:8])
	if secure {
		return "__Host-" + name
	}
	return name
}
