package approvers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// KeyEntry is one rules-file key, parsed (Task 5's YAML loader produces it): a credential id, the
// nonce its registration challenge was built from, the verbatim RegistrationResponseJSON, and
// either an Endorsement (an existing live key's assertion vouching for it) or Seed (a matching
// approver_key_seeds row vouches for it instead — the break-glass bootstrap path).
type KeyEntry struct {
	CredentialID   string          // base64url
	ChallengeNonce string          // 64 hex
	Registration   json.RawMessage // RegistrationResponseJSON
	Endorsement    *Endorsement    // nil ⇔ Seed
	Seed           bool
}

// Endorsement is an existing live key's vouching for a new one: By is the endorsing key's
// credential id, Assertion the AuthenticationResponseJSON it signed over
// record.EndorseChallenge(login, keyHash-of-the-new-key).
type Endorsement struct {
	By        string
	Assertion json.RawMessage
}

// Asserted is one verified action assertion: which credential signed it and which login it
// belongs to.
type Asserted struct {
	CredentialID string
	Login        string
}

// KeyInfo is one persisted approver key, for the UI route.
type KeyInfo struct {
	CredentialID string
	Login        string
	AAGUID       uuid.UUID
	State        string // active | tombstoned (Keys never returns a revoked key)
	Seeded       bool
	EndorsedBy   string // "" when Seeded
	SignCount    int64
	RegisteredAt time.Time
	LastUsedAt   *time.Time
}

// Ceremony is a live WebAuthn registration or endorsement challenge: its id (to be echoed back to
// Finish) and the challenge bytes to embed in the client's navigator.credentials
// create()/get() options. Ceremony rows expire after 10 minutes.
type Ceremony struct {
	ID        string
	Challenge []byte
}

var (
	// ErrAssertionInvalid wraps every assertion verification failure: bad origin, rpIdHash,
	// flags, challenge, credential ownership, or signature.
	ErrAssertionInvalid    = errors.New("assertion invalid")
	ErrKeyNotFound         = errors.New("no approver key with this credential id")
	ErrKeyNotLive          = errors.New("approver key is not live")
	ErrKeyRevoked          = errors.New("credential was revoked and cannot be re-added")
	ErrWrongLogin          = errors.New("this key does not belong to the required login")
	ErrCounterReplay       = errors.New("signature counter did not advance past the stored value")
	ErrNoLiveKeys          = errors.New("no live keys; break-glass seed required")
	ErrRegistrationChanged = errors.New("credential's registration changed; a credential id cannot silently get new registration data")
	ErrNoMatchingSeed      = errors.New("no matching approver_key_seeds row")
	ErrEndorserNotFound    = errors.New("endorsing key not found or not live")
	ErrEndorsementInvalid  = errors.New("endorsement assertion invalid")
	ErrCeremonyNotFound    = errors.New("ceremony not found or already used")
	ErrCeremonyExpired     = errors.New("ceremony expired")
)

// Service is the persisted approver key set: Reconcile applies a rules file's approvers section
// to it, VerifyAssertion checks one action's assertion against it, and the ceremony methods drive
// the registration and endorsement UI flows that produce rules-file-pasteable YAML.
type Service struct {
	Store    *store.Store
	Verifier *Verifier
}

// approverKeyRow is one persisted (non-revoked) approver_keys row, as Reconcile needs it.
type approverKeyRow struct {
	AAGUID       uuid.UUID
	COSEKey      []byte
	Registration json.RawMessage
	SignCount    int64
	State        string // active | tombstoned
}

// Reconcile applies a rules file's approvers section to the persisted set per contract v9's
// "Reload reconciliation" section:
//
//   - a file key already persisted (by credential id) is kept as-is; its registration must be
//     byte-identical (compared semantically, since jsonb re-serializes whitespace) to what's
//     stored, and a credential id whose registration changed is refused.
//   - a NEW file key is accepted only with a valid endorsement by a LIVE persisted key of that
//     login, or Seed plus a matching approver_key_seeds row. A login with zero live persisted
//     keys accepts no endorsed adds, only seeded ones.
//   - a persisted key absent from its login's file entries is tombstoned; one that reappears with
//     registration is revived, since tombstone means only "currently absent from the file", not
//     "distrusted" (its historical endorsements of other keys stay valid throughout).
//   - every key-set change writes an audit row and, once per call, a "approver key set changed"
//     structured log line with counts.
//
// It returns an error naming the first refused key; a refused reconcile keeps the previous set —
// the whole call runs in one transaction.
func (s *Service) Reconcile(ctx context.Context, file map[string][]KeyEntry) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	canon := make(map[string][]KeyEntry, len(file))
	for login, entries := range file {
		canon[record.CanonicalLogin(login)] = entries
	}
	var added, revived, tombstoned int
	for login, entries := range canon {
		a, r, tmb, err := s.reconcileLogin(ctx, tx, login, entries)
		if err != nil {
			return err
		}
		added += a
		revived += r
		tombstoned += tmb
	}
	if added > 0 || revived > 0 || tombstoned > 0 {
		slog.Info("approver key set changed", "added", added, "revived", revived, "tombstoned", tombstoned)
	}
	return tx.Commit(ctx)
}

func (s *Service) reconcileLogin(ctx context.Context, tx pgx.Tx, login string, entries []KeyEntry) (added, revived, tombstoned int, err error) {
	existing, err := loadApproverKeys(ctx, tx, login)
	if err != nil {
		return 0, 0, 0, err
	}
	liveCount := 0
	for _, row := range existing {
		if row.State == "active" {
			liveCount++
		}
	}

	seen := map[string]bool{}
	for _, entry := range entries {
		credID := entry.CredentialID
		seen[credID] = true

		if row, ok := existing[credID]; ok {
			if !registrationsEqual(row.Registration, entry.Registration) {
				return 0, 0, 0, fmt.Errorf("%s: %w", credID, ErrRegistrationChanged)
			}
			if row.State == "tombstoned" {
				if _, err := tx.Exec(ctx, `update approver_keys set state='active', removed_at=null where credential_id=$1`, credID); err != nil {
					return 0, 0, 0, err
				}
				if err := auditApprover(ctx, tx, "approver_key.revived", "broker", map[string]any{"credential_id": credID, "login": login}); err != nil {
					return 0, 0, 0, err
				}
				row.State = "active"
				existing[credID] = row
				liveCount++
				revived++
			}
			continue
		}

		var priorState string
		err := tx.QueryRow(ctx, `select state from approver_keys where credential_id=$1`, credID).Scan(&priorState)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, 0, err
		}
		if err == nil && priorState == "revoked" {
			return 0, 0, 0, fmt.Errorf("%s: %w", credID, ErrKeyRevoked)
		}

		registered, verr := s.Verifier.VerifyRegistration(login, entry)
		if verr != nil {
			return 0, 0, 0, fmt.Errorf("%s: %w", credID, verr)
		}

		var endorsedBy *string
		seeded := false
		switch {
		case entry.Seed:
			var exists bool
			if err := tx.QueryRow(ctx, `select exists(select 1 from approver_key_seeds where login=$1 and credential_id=$2)`, login, credID).Scan(&exists); err != nil {
				return 0, 0, 0, err
			}
			if !exists {
				return 0, 0, 0, fmt.Errorf("%s: %w", credID, ErrNoMatchingSeed)
			}
			seeded = true
		case entry.Endorsement != nil:
			if liveCount == 0 {
				return 0, 0, 0, fmt.Errorf("%s: %w", credID, ErrNoLiveKeys)
			}
			endorserRow, ok := existing[entry.Endorsement.By]
			if !ok || endorserRow.State != "active" {
				return 0, 0, 0, fmt.Errorf("%s: %w", credID, ErrEndorserNotFound)
			}
			requiredID, err := base64.RawURLEncoding.DecodeString(entry.Endorsement.By)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("%s: %w: endorsement.by is not valid base64url", credID, ErrEndorsementInvalid)
			}
			keyHash := credentialHash(registered.CredentialID)
			challenge := record.EndorseChallenge(login, keyHash)
			_, counter, verr := s.Verifier.verifyAssertion(endorserRow.COSEKey, challenge, entry.Endorsement.Assertion, requiredID)
			if verr != nil {
				return 0, 0, 0, fmt.Errorf("%s: %w: %s", credID, ErrEndorsementInvalid, verr)
			}
			if (counter != 0 || endorserRow.SignCount != 0) && int64(counter) <= endorserRow.SignCount {
				return 0, 0, 0, fmt.Errorf("%s: %w", credID, ErrCounterReplay)
			}
			if _, err := tx.Exec(ctx, `update approver_keys set sign_count=$2, last_used_at=now() where credential_id=$1`, entry.Endorsement.By, counter); err != nil {
				return 0, 0, 0, err
			}
			endorserRow.SignCount = int64(counter)
			existing[entry.Endorsement.By] = endorserRow
			by := entry.Endorsement.By
			endorsedBy = &by
		default:
			return 0, 0, 0, fmt.Errorf("%s: key is neither seeded nor endorsed", credID)
		}

		if _, err := tx.Exec(ctx,
			`insert into approver_keys (credential_id, login, aaguid, cose_key, registration, challenge_nonce, endorsed_by, seeded, sign_count, state)
			 values ($1,$2,$3,$4,$5,$6,$7,$8,0,'active')`,
			credID, login, registered.AAGUID, registered.COSEKey, []byte(entry.Registration), entry.ChallengeNonce, endorsedBy, seeded); err != nil {
			return 0, 0, 0, err
		}
		method := "endorsed"
		if seeded {
			method = "seeded"
		}
		if err := auditApprover(ctx, tx, "approver_key.added", "broker", map[string]any{"credential_id": credID, "login": login, "method": method}); err != nil {
			return 0, 0, 0, err
		}
		existing[credID] = approverKeyRow{AAGUID: registered.AAGUID, COSEKey: registered.COSEKey, Registration: entry.Registration, State: "active"}
		added++
		liveCount++
	}

	for credID, row := range existing {
		if seen[credID] || row.State != "active" {
			continue
		}
		if _, err := tx.Exec(ctx, `update approver_keys set state='tombstoned', removed_at=now() where credential_id=$1`, credID); err != nil {
			return 0, 0, 0, err
		}
		if err := auditApprover(ctx, tx, "approver_key.tombstoned", "broker", map[string]any{"credential_id": credID, "login": login}); err != nil {
			return 0, 0, 0, err
		}
		tombstoned++
	}
	return added, revived, tombstoned, nil
}

func loadApproverKeys(ctx context.Context, tx pgx.Tx, login string) (map[string]approverKeyRow, error) {
	rows, err := tx.Query(ctx,
		`select credential_id, aaguid, cose_key, registration, sign_count, state
		 from approver_keys where login=$1 and state<>'revoked' for update`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]approverKeyRow{}
	for rows.Next() {
		var credID string
		var row approverKeyRow
		if err := rows.Scan(&credID, &row.AAGUID, &row.COSEKey, &row.Registration, &row.SignCount, &row.State); err != nil {
			return nil, err
		}
		out[credID] = row
	}
	return out, rows.Err()
}

// registrationsEqual compares two RegistrationResponseJSON blobs by decoded value rather than
// literal bytes: a value read back from Postgres's jsonb column has been reformatted (whitespace,
// key order is preserved but numeric spelling can change), so a literal byte comparison would
// refuse a credential whose stored registration is semantically unchanged.
func registrationsEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(av, bv)
}

func credentialHash(credentialID []byte) string {
	sum := sha256.Sum256(credentialID)
	return hex.EncodeToString(sum[:])
}

func auditApprover(ctx context.Context, tx pgx.Tx, kind, actor string, fields map[string]any) error {
	detail, err := json.Marshal(fields)
	if err != nil {
		// fields is always built from this package's own string values, so Marshal cannot fail
		// in practice; fall back to an empty object rather than losing the whole audit row.
		detail = []byte("{}")
	}
	_, err = tx.Exec(ctx, `insert into audit (kind, actor, detail) values ($1,$2,$3::jsonb)`, kind, actor, detail)
	return err
}

// VerifyAssertion checks one action assertion per contract v9's "Assertion verification" section
// and, in tx, bumps sign_count and last_used_at in the same transaction that decides the action.
// login "" means "any active key"; a non-empty login requires the asserting key to belong to
// exactly that login. It returns the key's login (RevokeKey's caller uses it to name the actor).
func (s *Service) VerifyAssertion(ctx context.Context, tx pgx.Tx, login string, challenge [32]byte, assertion json.RawMessage) (Asserted, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil {
		return Asserted{}, fmt.Errorf("%w: parse assertion: %s", ErrAssertionInvalid, err)
	}
	credentialID := base64.RawURLEncoding.EncodeToString(parsed.RawID)

	var storedLogin string
	var coseKey []byte
	var state string
	var signCount int64
	err = tx.QueryRow(ctx, `select login, cose_key, state, sign_count from approver_keys where credential_id=$1 for update`, credentialID).
		Scan(&storedLogin, &coseKey, &state, &signCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asserted{}, ErrKeyNotFound
	}
	if err != nil {
		return Asserted{}, err
	}
	if state != "active" {
		return Asserted{}, ErrKeyNotLive
	}
	if login != "" && record.CanonicalLogin(login) != storedLogin {
		return Asserted{}, ErrWrongLogin
	}

	_, counter, err := s.Verifier.verifyAssertion(coseKey, challenge, assertion, parsed.RawID)
	if err != nil {
		return Asserted{}, err
	}
	if (counter != 0 || signCount != 0) && int64(counter) <= signCount {
		return Asserted{}, ErrCounterReplay
	}
	if _, err := tx.Exec(ctx, `update approver_keys set sign_count=$2, last_used_at=now() where credential_id=$1`, credentialID, counter); err != nil {
		return Asserted{}, err
	}
	return Asserted{CredentialID: credentialID, Login: storedLogin}, nil
}

// verifyAssertion runs the contract v9 "Assertion verification" checks against a stored COSE key
// and returns the asserting credential id and its authData signature counter, without touching
// the store: Reconcile's endorsement check and FinishEndorse call it directly against an
// endorsing key already loaded in their own transaction, while VerifyAssertion calls it after its
// own store lookup. requiredCredentialID, when non-nil, must equal the asserting credential id.
func (v *Verifier) verifyAssertion(coseKey []byte, expectedChallenge [32]byte, assertion json.RawMessage, requiredCredentialID []byte) ([]byte, uint32, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: parse assertion: %s", ErrAssertionInvalid, err)
	}
	ccd := parsed.Response.CollectedClientData
	if ccd.Type != protocol.AssertCeremony {
		return nil, 0, fmt.Errorf("%w: clientData.type is %q, not webauthn.get", ErrAssertionInvalid, ccd.Type)
	}
	if ccd.Origin != v.Origin {
		return nil, 0, fmt.Errorf("%w: clientData.origin %q does not match %q", ErrAssertionInvalid, ccd.Origin, v.Origin)
	}
	if ccd.CrossOrigin {
		return nil, 0, fmt.Errorf("%w: clientData.crossOrigin must be false", ErrAssertionInvalid)
	}
	if ccd.TopOrigin != "" {
		return nil, 0, fmt.Errorf("%w: clientData.topOrigin must be absent", ErrAssertionInvalid)
	}
	challengeBytes, err := base64.RawURLEncoding.DecodeString(ccd.Challenge)
	if err != nil || !bytes.Equal(challengeBytes, expectedChallenge[:]) {
		return nil, 0, fmt.Errorf("%w: challenge does not match the expected challenge", ErrAssertionInvalid)
	}
	host, err := v.originHost()
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s", ErrAssertionInvalid, err)
	}
	rpIDHash := sha256.Sum256([]byte(host))
	ad := parsed.Response.AuthenticatorData
	if !bytes.Equal(ad.RPIDHash, rpIDHash[:]) {
		return nil, 0, fmt.Errorf("%w: authData.rpIdHash does not match sha256(host(origin))", ErrAssertionInvalid)
	}
	if !ad.Flags.HasUserPresent() || !ad.Flags.HasUserVerified() {
		return nil, 0, fmt.Errorf("%w: authData flags require UP and UV set", ErrAssertionInvalid)
	}
	if ad.Flags.HasBackupEligible() || ad.Flags.HasBackupState() {
		return nil, 0, fmt.Errorf("%w: authData BE and BS must both be 0", ErrAssertionInvalid)
	}
	if requiredCredentialID != nil && !bytes.Equal(parsed.RawID, requiredCredentialID) {
		return nil, 0, fmt.Errorf("%w: the asserting credential does not belong to the required key", ErrAssertionInvalid)
	}
	pub, err := webauthncose.ParsePublicKey(coseKey)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: stored public key: %s", ErrAssertionInvalid, err)
	}
	clientDataHash := sha256.Sum256([]byte(parsed.Raw.AssertionResponse.ClientDataJSON))
	signedData := append(append([]byte{}, []byte(parsed.Raw.AssertionResponse.AuthenticatorData)...), clientDataHash[:]...)
	ok, err := webauthncose.VerifySignature(pub, signedData, parsed.Response.Signature)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: signature: %s", ErrAssertionInvalid, err)
	}
	if !ok {
		return nil, 0, fmt.Errorf("%w: signature does not verify", ErrAssertionInvalid)
	}
	return parsed.RawID, ad.Counter, nil
}

// RevokeKey tombstones a key as revoked and cascades: any key whose endorsed_by names credID is
// also revoked, recursively — a key endorsed by a since-revoked key is untrusted. It operates on
// a key in any non-revoked state (active or tombstoned): a tombstoned key can still be formally
// revoked by a human decision. Revoking an already-revoked or nonexistent key returns ErrKeyNotLive
// and writes no audit row, so the trail never names an actor who revoked nothing.
func (s *Service) RevokeKey(ctx context.Context, tx pgx.Tx, credentialID, actor string) error {
	queue := []string{credentialID}
	first := true
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		var login, state string
		err := tx.QueryRow(ctx, `select login, state from approver_keys where credential_id=$1 for update`, id).Scan(&login, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			if first {
				return ErrKeyNotLive
			}
			continue
		}
		if err != nil {
			return err
		}
		if state == "revoked" {
			if first {
				return ErrKeyNotLive
			}
			continue
		}
		first = false

		if _, err := tx.Exec(ctx, `update approver_keys set state='revoked', removed_at=now() where credential_id=$1`, id); err != nil {
			return err
		}
		if err := auditApprover(ctx, tx, "approver_key.revoked", actor, map[string]any{"credential_id": id, "login": login}); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `select credential_id from approver_keys where endorsed_by=$1 and state<>'revoked'`, id)
		if err != nil {
			return err
		}
		var children []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return err
			}
			children = append(children, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		queue = append(queue, children...)
	}
	return nil
}

// Keys lists a login's live and tombstoned keys for the UI route; a revoked key never appears.
func (s *Service) Keys(ctx context.Context, login string) ([]KeyInfo, error) {
	login = record.CanonicalLogin(login)
	rows, err := s.Store.Pool.Query(ctx,
		`select credential_id, aaguid, state, seeded, coalesce(endorsed_by,''), sign_count, registered_at, last_used_at
		 from approver_keys where login=$1 and state<>'revoked' order by registered_at`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyInfo
	for rows.Next() {
		k := KeyInfo{Login: login}
		if err := rows.Scan(&k.CredentialID, &k.AAGUID, &k.State, &k.Seeded, &k.EndorsedBy, &k.SignCount, &k.RegisteredAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func randomNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// BeginRegister opens a 10-minute registration ceremony for login: the caller embeds the returned
// challenge in a navigator.credentials.create() call and hands the response to FinishRegister.
func (s *Service) BeginRegister(ctx context.Context, login string) (Ceremony, error) {
	login = record.CanonicalLogin(login)
	nonce, err := randomNonce()
	if err != nil {
		return Ceremony{}, err
	}
	challenge := record.RegisterChallenge(login, nonce)
	id := uuid.New()
	if _, err := s.Store.Pool.Exec(ctx,
		`insert into webauthn_ceremonies (id, login, kind, nonce, challenge, expires_at) values ($1,$2,'register',$3,$4, now() + interval '10 minutes')`,
		id, login, nonce, challenge[:]); err != nil {
		return Ceremony{}, err
	}
	return Ceremony{ID: id.String(), Challenge: challenge[:]}, nil
}

// FinishRegister consumes ceremonyID's row in its own transaction (regardless of outcome — a
// burned challenge cannot be retried against a mangled response), verifies the response with
// Verifier.VerifyRegistration as a sanity check, and returns a pasteable YAML fragment shaped
// like the rules file's "logins: <login>: keys: [{credential_id, registration: {challenge_nonce,
// response}}]" block. The caller adds a "seed: true" line or an "endorsement:" block (from
// FinishEndorse) before pasting it into the rules file — this ceremony proves possession only.
func (s *Service) FinishRegister(ctx context.Context, login, ceremonyID string, response json.RawMessage) (string, error) {
	login = record.CanonicalLogin(login)
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var nonce string
	var expiresAt time.Time
	err = tx.QueryRow(ctx,
		`delete from webauthn_ceremonies where id=$1 and login=$2 and kind='register' returning nonce, expires_at`,
		ceremonyID, login).Scan(&nonce, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCeremonyNotFound
	}
	if err != nil {
		return "", err
	}
	if time.Now().After(expiresAt) {
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", ErrCeremonyExpired
	}

	parsed, perr := protocol.ParseCredentialCreationResponseBytes(response)
	if perr != nil {
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: parse response: %s", ErrRegistrationInvalid, perr)
	}
	entry := KeyEntry{
		CredentialID:   base64.RawURLEncoding.EncodeToString(parsed.Response.AttestationObject.AuthData.AttData.CredentialID),
		ChallengeNonce: nonce,
		Registration:   response,
	}
	if _, verr := s.Verifier.VerifyRegistration(login, entry); verr != nil {
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", verr
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return renderKeyEntryYAML(login, entry)
}

func renderKeyEntryYAML(login string, entry KeyEntry) (string, error) {
	var response any
	if err := json.Unmarshal(entry.Registration, &response); err != nil {
		return "", fmt.Errorf("render key entry YAML: %w", err)
	}
	doc := map[string]any{
		"logins": map[string]any{
			login: map[string]any{
				"keys": []any{
					map[string]any{
						"credential_id": entry.CredentialID,
						"registration": map[string]any{
							"challenge_nonce": entry.ChallengeNonce,
							"response":        response,
						},
					},
				},
			},
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("render key entry YAML: %w", err)
	}
	return string(out), nil
}

// BeginEndorse opens a 10-minute endorsement ceremony: credentialID is the existing, live key
// expected to sign the challenge, keyHash the lowercase-hex SHA-256 of the new key's raw
// credential id bytes (record.EndorseChallenge(login, keyHash) is the domain-separated challenge
// the existing key must assert over).
func (s *Service) BeginEndorse(ctx context.Context, login, credentialID, keyHash string) (Ceremony, error) {
	login = record.CanonicalLogin(login)
	challenge := record.EndorseChallenge(login, keyHash)
	id := uuid.New()
	if _, err := s.Store.Pool.Exec(ctx,
		`insert into webauthn_ceremonies (id, login, kind, challenge, subject, expires_at) values ($1,$2,'endorse',$3,$4, now() + interval '10 minutes')`,
		id, login, challenge[:], credentialID); err != nil {
		return Ceremony{}, err
	}
	return Ceremony{ID: id.String(), Challenge: challenge[:]}, nil
}

// FinishEndorse consumes ceremonyID's row in its own transaction and verifies the assertion
// against the ceremony's subject key (which must still be active), as a fast sanity check before
// handing back a pasteable "endorsement: {by, assertion}" YAML block — like FinishRegister, it
// persists nothing about the key itself. The actual sign_count bump for this assertion happens
// once, in Reconcile, when the endorsement is actually applied to add the new key; bumping it
// here too would make Reconcile see its own already-consumed counter and refuse the very
// endorsement this ceremony produced.
func (s *Service) FinishEndorse(ctx context.Context, login, ceremonyID string, response json.RawMessage) (string, error) {
	login = record.CanonicalLogin(login)
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var subject *string
	var challenge []byte
	var expiresAt time.Time
	err = tx.QueryRow(ctx,
		`delete from webauthn_ceremonies where id=$1 and login=$2 and kind='endorse' returning subject, challenge, expires_at`,
		ceremonyID, login).Scan(&subject, &challenge, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCeremonyNotFound
	}
	if err != nil {
		return "", err
	}
	if subject == nil {
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", errors.New("endorse ceremony has no subject key")
	}
	if time.Now().After(expiresAt) {
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", ErrCeremonyExpired
	}

	var coseKey []byte
	var state string
	var signCount int64
	err = tx.QueryRow(ctx, `select cose_key, state, sign_count from approver_keys where credential_id=$1 and login=$2 for update`, *subject, login).
		Scan(&coseKey, &state, &signCount)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "active") {
		if cerr := tx.Commit(ctx); cerr != nil {
			return "", cerr
		}
		return "", ErrEndorserNotFound
	}
	if err != nil {
		return "", err
	}

	requiredID, derr := base64.RawURLEncoding.DecodeString(*subject)
	if derr != nil {
		if cerr := tx.Commit(ctx); cerr != nil {
			return "", cerr
		}
		return "", fmt.Errorf("%w: stored subject credential id", ErrEndorsementInvalid)
	}
	var challengeArr [32]byte
	copy(challengeArr[:], challenge)
	_, counter, verr := s.Verifier.verifyAssertion(coseKey, challengeArr, response, requiredID)
	if verr != nil {
		if cerr := tx.Commit(ctx); cerr != nil {
			return "", cerr
		}
		return "", fmt.Errorf("%w: %s", ErrEndorsementInvalid, verr)
	}
	if (counter != 0 || signCount != 0) && int64(counter) <= signCount {
		if cerr := tx.Commit(ctx); cerr != nil {
			return "", cerr
		}
		return "", ErrCounterReplay
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return renderEndorsementYAML(*subject, response)
}

func renderEndorsementYAML(by string, assertion json.RawMessage) (string, error) {
	var response any
	if err := json.Unmarshal(assertion, &response); err != nil {
		return "", fmt.Errorf("render endorsement YAML: %w", err)
	}
	doc := map[string]any{
		"endorsement": map[string]any{
			"by":        by,
			"assertion": response,
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("render endorsement YAML: %w", err)
	}
	return string(out), nil
}
