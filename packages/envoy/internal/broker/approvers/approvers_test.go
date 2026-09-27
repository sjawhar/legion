package approvers

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/broker/webauthntest"
)

const testOrigin = "https://dispatch.test"
const testRPID = "dispatch.test"

var testAAGUID = uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a")

func newFixture(t *testing.T) (*Service, *webauthntest.CA) {
	t.Helper()
	st := storetest.Open(t)
	ca := webauthntest.NewCA(t)
	v := &Verifier{Roots: ca.Pool(), Origin: testOrigin, AAGUIDs: map[uuid.UUID]bool{testAAGUID: true}}
	return &Service{Store: st, Verifier: v}, ca
}

// register builds a KeyEntry for a fresh authenticator's registration under login.
func register(t *testing.T, ca *webauthntest.CA, login, noncePrefix string) (KeyEntry, *webauthntest.Authenticator) {
	t.Helper()
	auth := ca.NewAuthenticator(t, testAAGUID)
	nonce := strings.Repeat(noncePrefix, 64)
	challenge := record.RegisterChallenge(login, nonce)
	return KeyEntry{
		CredentialID:   b64(auth.CredentialID),
		ChallengeNonce: nonce,
		Registration:   auth.Register(t, testRPID, testOrigin, challenge[:]),
	}, auth
}

// endorse has endorser sign newKey's endorsement challenge, returning newKey with its Endorsement
// field populated.
func endorse(t *testing.T, login string, endorser *webauthntest.Authenticator, endorserCredentialID string, newKey KeyEntry) KeyEntry {
	t.Helper()
	newCredID, err := base64.RawURLEncoding.DecodeString(newKey.CredentialID)
	if err != nil {
		t.Fatalf("decode new key credential id: %v", err)
	}
	keyHash := credentialHash(newCredID)
	challenge := record.EndorseChallenge(login, keyHash)
	newKey.Endorsement = &Endorsement{
		By:        endorserCredentialID,
		Assertion: endorser.Assert(t, testRPID, testOrigin, challenge[:]),
	}
	return newKey
}

func assertKeyState(t *testing.T, ctx context.Context, st *store.Store, credentialID, want string) {
	t.Helper()
	var state string
	if err := st.Pool.QueryRow(ctx, `select state from approver_keys where credential_id=$1`, credentialID).Scan(&state); err != nil {
		t.Fatalf("read state of %s: %v", credentialID, err)
	}
	if state != want {
		t.Fatalf("state of %s = %q, want %q", credentialID, state, want)
	}
}

func TestReconcileSeedsEndorsesTombstonesAndCascades(t *testing.T) {
	ctx := context.Background()
	svc, ca := newFixture(t)
	login := "sjawhar"

	key1, auth1 := register(t, ca, login, "1")
	key2, _ := register(t, ca, login, "2")

	// (1) a fresh login's endorsed-only key is refused: no live keys yet, so an endorsement can't
	// be trusted regardless of whose assertion it carries.
	key2Endorsed := endorse(t, login, auth1, key1.CredentialID, key2)
	err := svc.Reconcile(ctx, map[string][]KeyEntry{login: {key2Endorsed}})
	if !errors.Is(err, ErrNoLiveKeys) {
		t.Fatalf("endorsed-only key with no live keys: err=%v, want %v", err, ErrNoLiveKeys)
	}

	// (2) insert an approver_key_seeds row; reconcile accepts the seeded key.
	if _, err := svc.Store.Pool.Exec(ctx, `insert into approver_key_seeds (login, credential_id) values ($1,$2)`, login, key1.CredentialID); err != nil {
		t.Fatalf("insert seed row: %v", err)
	}
	key1Seeded := key1
	key1Seeded.Seed = true
	if err := svc.Reconcile(ctx, map[string][]KeyEntry{login: {key1Seeded}}); err != nil {
		t.Fatalf("reconcile seeded key1: %v", err)
	}
	assertKeyState(t, ctx, svc.Store, key1.CredentialID, "active")

	// (3) a second key endorsed by the first (now live) is accepted.
	if err := svc.Reconcile(ctx, map[string][]KeyEntry{login: {key1Seeded, key2Endorsed}}); err != nil {
		t.Fatalf("reconcile key1+key2: %v", err)
	}
	assertKeyState(t, ctx, svc.Store, key2.CredentialID, "active")

	// (4) removing key1 from the file tombstones it, and its later endorsement attempts are
	// refused.
	if err := svc.Reconcile(ctx, map[string][]KeyEntry{login: {key2Endorsed}}); err != nil {
		t.Fatalf("reconcile removing key1: %v", err)
	}
	assertKeyState(t, ctx, svc.Store, key1.CredentialID, "tombstoned")

	key3, _ := register(t, ca, login, "3")
	key3Endorsed := endorse(t, login, auth1, key1.CredentialID, key3)
	err = svc.Reconcile(ctx, map[string][]KeyEntry{login: {key2Endorsed, key3Endorsed}})
	if !errors.Is(err, ErrEndorserNotFound) {
		t.Fatalf("endorsement by tombstoned key1: err=%v, want %v", err, ErrEndorserNotFound)
	}

	// (5) RevokeKey on key1 revokes key2 too (key2's endorsed_by names key1).
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := svc.RevokeKey(ctx, tx, key1.CredentialID, "human:sjawhar"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertKeyState(t, ctx, svc.Store, key1.CredentialID, "revoked")
	assertKeyState(t, ctx, svc.Store, key2.CredentialID, "revoked")
}

func TestVerifyAssertionEnforcesOriginRpIDCounterAndLogin(t *testing.T) {
	ctx := context.Background()
	svc, ca := newFixture(t)

	seedAndReconcile := func(login string) (KeyEntry, *webauthntest.Authenticator) {
		entry, auth := register(t, ca, login, login[:1])
		if _, err := svc.Store.Pool.Exec(ctx, `insert into approver_key_seeds (login, credential_id) values ($1,$2)`, login, entry.CredentialID); err != nil {
			t.Fatalf("insert seed row: %v", err)
		}
		entry.Seed = true
		if err := svc.Reconcile(ctx, map[string][]KeyEntry{login: {entry}}); err != nil {
			t.Fatalf("reconcile %s: %v", login, err)
		}
		return entry, auth
	}

	alice, aliceAuth := seedAndReconcile("alice")
	bob, bobAuth := seedAndReconcile("bob")

	actionChallenge := record.ApproveChallenge("some-record-id")

	// A good assertion passes and bumps sign_count.
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	assertion := aliceAuth.Assert(t, testRPID, testOrigin, actionChallenge[:])
	asserted, err := svc.VerifyAssertion(ctx, tx, "alice", actionChallenge, assertion)
	if err != nil {
		t.Fatalf("good assertion: %v", err)
	}
	if asserted.CredentialID != alice.CredentialID || asserted.Login != "alice" {
		t.Fatalf("asserted = %+v, want credential_id=%s login=alice", asserted, alice.CredentialID)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var signCount int64
	if err := svc.Store.Pool.QueryRow(ctx, `select sign_count from approver_keys where credential_id=$1`, alice.CredentialID).Scan(&signCount); err != nil || signCount != 1 {
		t.Fatalf("sign_count after good assertion = %d, %v, want 1", signCount, err)
	}

	// A replayed assertion at the same counter value is refused.
	func() {
		tx, err := svc.Store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := svc.VerifyAssertion(ctx, tx, "alice", actionChallenge, assertion); !errors.Is(err, ErrCounterReplay) {
			t.Fatalf("replayed assertion: err=%v, want %v", err, ErrCounterReplay)
		}
	}()

	// AssertAtOrigin("https://evil.test") is refused (origin mismatch).
	func() {
		tx, err := svc.Store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		wrongOrigin := aliceAuth.AssertAtOrigin(t, testRPID, "https://evil.test", actionChallenge[:])
		if _, err := svc.VerifyAssertion(ctx, tx, "alice", actionChallenge, wrongOrigin); !errors.Is(err, ErrAssertionInvalid) {
			t.Fatalf("wrong origin assertion: err=%v, want %v", err, ErrAssertionInvalid)
		}
	}()

	// An assertion signed by a different login's registered key is refused when the required
	// login doesn't match the key's own login.
	func() {
		tx, err := svc.Store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		bobChallenge := record.ApproveChallenge("another-record-id")
		bobAssertion := bobAuth.Assert(t, testRPID, testOrigin, bobChallenge[:])
		if _, err := svc.VerifyAssertion(ctx, tx, "alice", bobChallenge, bobAssertion); !errors.Is(err, ErrWrongLogin) {
			t.Fatalf("bob's key required as alice: err=%v, want %v", err, ErrWrongLogin)
		}
	}()

	// An assertion by a tombstoned key (removed from the file in a prior Reconcile) is refused.
	if err := svc.Reconcile(ctx, map[string][]KeyEntry{"alice": {}, "bob": {bob}}); err != nil {
		t.Fatalf("reconcile removing alice's key: %v", err)
	}
	assertKeyState(t, ctx, svc.Store, alice.CredentialID, "tombstoned")
	func() {
		tx, err := svc.Store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		tombstonedChallenge := record.ApproveChallenge("yet-another-record-id")
		tombstonedAssertion := aliceAuth.Assert(t, testRPID, testOrigin, tombstonedChallenge[:])
		if _, err := svc.VerifyAssertion(ctx, tx, "", tombstonedChallenge, tombstonedAssertion); !errors.Is(err, ErrKeyNotLive) {
			t.Fatalf("tombstoned key assertion: err=%v, want %v", err, ErrKeyNotLive)
		}
	}()
}

func TestFinishRegisterReturnsPasteableYAML(t *testing.T) {
	ctx := context.Background()
	svc, ca := newFixture(t)
	login := "sjawhar"

	ceremony, err := svc.BeginRegister(ctx, login)
	if err != nil {
		t.Fatalf("BeginRegister: %v", err)
	}
	var challenge [32]byte
	copy(challenge[:], ceremony.Challenge)

	auth := ca.NewAuthenticator(t, testAAGUID)
	response := auth.Register(t, testRPID, testOrigin, ceremony.Challenge)

	out, err := svc.FinishRegister(ctx, login, ceremony.ID, response)
	if err != nil {
		t.Fatalf("FinishRegister: %v", err)
	}

	var doc struct {
		Logins map[string]struct {
			Keys []struct {
				CredentialID string `yaml:"credential_id"`
				Registration struct {
					ChallengeNonce string         `yaml:"challenge_nonce"`
					Response       map[string]any `yaml:"response"`
				} `yaml:"registration"`
			} `yaml:"keys"`
		} `yaml:"logins"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("parse returned YAML: %v\n%s", err, out)
	}
	loginBlock, ok := doc.Logins[login]
	if !ok || len(loginBlock.Keys) != 1 {
		t.Fatalf("parsed YAML logins[%s].keys = %+v, want exactly one key", login, doc.Logins[login])
	}
	got := loginBlock.Keys[0]

	wantCredentialID := b64(auth.CredentialID)
	if got.CredentialID != wantCredentialID {
		t.Fatalf("round-tripped credential_id = %q, want %q", got.CredentialID, wantCredentialID)
	}
	if got.Registration.ChallengeNonce == "" {
		t.Fatal("round-tripped challenge_nonce is empty")
	}
	expectedChallenge := record.RegisterChallenge(login, got.Registration.ChallengeNonce)
	if expectedChallenge != challenge {
		t.Fatal("round-tripped challenge_nonce does not reproduce the ceremony's challenge")
	}
}

// TestEndorseCeremonyRoundTripsAndKeysListsBothKeys drives BeginEndorse/FinishEndorse end to end
// (a live approver signs a real assertion over the ceremony's challenge) and confirms the
// returned YAML endorsement block round-trips the endorsing credential id, then that Reconcile
// accepts a new key carrying that endorsement, and that Keys lists both keys with the endorsement
// wired up.
func TestEndorseCeremonyRoundTripsAndKeysListsBothKeys(t *testing.T) {
	ctx := context.Background()
	svc, ca := newFixture(t)
	login := "sjawhar"

	key1, auth1 := register(t, ca, login, "1")
	if _, err := svc.Store.Pool.Exec(ctx, `insert into approver_key_seeds (login, credential_id) values ($1,$2)`, login, key1.CredentialID); err != nil {
		t.Fatalf("insert seed row: %v", err)
	}
	key1Seeded := key1
	key1Seeded.Seed = true
	if err := svc.Reconcile(ctx, map[string][]KeyEntry{login: {key1Seeded}}); err != nil {
		t.Fatalf("reconcile seeded key1: %v", err)
	}

	key2, _ := register(t, ca, login, "2")
	newCredID, err := base64.RawURLEncoding.DecodeString(key2.CredentialID)
	if err != nil {
		t.Fatalf("decode key2 credential id: %v", err)
	}
	keyHash := credentialHash(newCredID)

	ceremony, err := svc.BeginEndorse(ctx, login, key1.CredentialID, keyHash)
	if err != nil {
		t.Fatalf("BeginEndorse: %v", err)
	}
	var challenge [32]byte
	copy(challenge[:], ceremony.Challenge)
	wantChallenge := record.EndorseChallenge(login, keyHash)
	if challenge != wantChallenge {
		t.Fatal("BeginEndorse challenge does not match record.EndorseChallenge(login, keyHash)")
	}

	assertion := auth1.Assert(t, testRPID, testOrigin, ceremony.Challenge)
	out, err := svc.FinishEndorse(ctx, login, ceremony.ID, assertion)
	if err != nil {
		t.Fatalf("FinishEndorse: %v", err)
	}

	var doc struct {
		Endorsement struct {
			By        string         `yaml:"by"`
			Assertion map[string]any `yaml:"assertion"`
		} `yaml:"endorsement"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("parse returned YAML: %v\n%s", err, out)
	}
	if doc.Endorsement.By != key1.CredentialID {
		t.Fatalf("round-tripped endorsement.by = %q, want %q", doc.Endorsement.By, key1.CredentialID)
	}

	// The ceremony is consumed: finishing it again is refused.
	if _, err := svc.FinishEndorse(ctx, login, ceremony.ID, assertion); !errors.Is(err, ErrCeremonyNotFound) {
		t.Fatalf("re-finishing a consumed ceremony: err=%v, want %v", err, ErrCeremonyNotFound)
	}

	key2Endorsed := key2
	key2Endorsed.Endorsement = &Endorsement{By: doc.Endorsement.By, Assertion: assertion}
	if err := svc.Reconcile(ctx, map[string][]KeyEntry{login: {key1Seeded, key2Endorsed}}); err != nil {
		t.Fatalf("reconcile with the round-tripped endorsement: %v", err)
	}

	keys, err := svc.Keys(ctx, login)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	byCredentialID := map[string]KeyInfo{}
	for _, k := range keys {
		byCredentialID[k.CredentialID] = k
	}
	if len(byCredentialID) != 2 {
		t.Fatalf("Keys returned %d entries, want 2: %+v", len(byCredentialID), keys)
	}
	if !byCredentialID[key1.CredentialID].Seeded {
		t.Fatalf("key1 Seeded = false, want true")
	}
	if byCredentialID[key2.CredentialID].EndorsedBy != key1.CredentialID {
		t.Fatalf("key2 EndorsedBy = %q, want %q", byCredentialID[key2.CredentialID].EndorsedBy, key1.CredentialID)
	}
}
