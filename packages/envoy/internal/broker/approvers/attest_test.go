package approvers

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/webauthntest"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ch converts a domain-separated challenge to the []byte webauthntest's authenticator methods
// take.
func ch(c [32]byte) []byte { return c[:] }

// withReg returns a copy of base with its Registration replaced, for building the negative-case
// KeyEntry variants below without repeating every other field.
func withReg(base KeyEntry, reg json.RawMessage) KeyEntry {
	base.Registration = reg
	return base
}

// offListAAGUID registers a fresh authenticator under an AAGUID that is not on the Verifier's
// allowed list, everything else about the registration otherwise valid.
func offListAAGUID(t *testing.T, ca *webauthntest.CA) KeyEntry {
	t.Helper()
	aaguid := uuid.New()
	auth := ca.NewAuthenticator(t, aaguid)
	nonce := strings.Repeat("a", 64)
	challenge := record.RegisterChallenge("sjawhar", nonce)
	return KeyEntry{
		CredentialID:   b64(auth.CredentialID),
		ChallengeNonce: nonce,
		Registration:   auth.Register(t, "dispatch.test", "https://dispatch.test", challenge[:]),
		Seed:           true,
	}
}

// zeroAAGUID registers a fresh authenticator whose AAGUID is the zero UUID, everything else
// about the registration otherwise valid.
func zeroAAGUID(t *testing.T, ca *webauthntest.CA) KeyEntry {
	t.Helper()
	auth := ca.NewAuthenticator(t, uuid.Nil)
	nonce := strings.Repeat("b", 64)
	challenge := record.RegisterChallenge("sjawhar", nonce)
	return KeyEntry{
		CredentialID:   b64(auth.CredentialID),
		ChallengeNonce: nonce,
		Registration:   auth.Register(t, "dispatch.test", "https://dispatch.test", challenge[:]),
		Seed:           true,
	}
}

// certAAGUIDMismatch registers a fresh authenticator whose attested credential data carries
// allowedAAGUID (so it passes the allow-list check) but whose attestation certificate's
// id-fido-gen-ce-aaguid extension carries a different AAGUID, everything else about the
// registration otherwise valid.
func certAAGUIDMismatch(t *testing.T, ca *webauthntest.CA, allowedAAGUID uuid.UUID) KeyEntry {
	t.Helper()
	certAAGUID := uuid.New()
	for certAAGUID == allowedAAGUID {
		certAAGUID = uuid.New()
	}
	auth := ca.NewAuthenticatorWithCertAAGUID(t, allowedAAGUID, certAAGUID)
	nonce := strings.Repeat("c", 64)
	challenge := record.RegisterChallenge("sjawhar", nonce)
	return KeyEntry{
		CredentialID:   b64(auth.CredentialID),
		ChallengeNonce: nonce,
		Registration:   auth.Register(t, "dispatch.test", "https://dispatch.test", challenge[:]),
		Seed:           true,
	}
}

func TestVerifyRegistrationAcceptsPackedAndRefusesEverythingElse(t *testing.T) {
	ca := webauthntest.NewCA(t)
	aaguid := uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a")
	v := &Verifier{Roots: ca.Pool(), Origin: "https://dispatch.test", AAGUIDs: map[uuid.UUID]bool{aaguid: true}}
	nonce := strings.Repeat("0", 64)
	challenge := record.RegisterChallenge("sjawhar", nonce)
	auth := ca.NewAuthenticator(t, aaguid)
	good := KeyEntry{CredentialID: b64(auth.CredentialID), ChallengeNonce: nonce,
		Registration: auth.Register(t, "dispatch.test", "https://dispatch.test", challenge[:]), Seed: true}
	if _, err := v.VerifyRegistration("sjawhar", good); err != nil {
		t.Fatal(err)
	}
	for name, entry := range map[string]KeyEntry{
		"fmt none":         withReg(good, auth.RegisterWithFmtNone(t, "dispatch.test", "https://dispatch.test", challenge[:])),
		"self attestation": withReg(good, auth.RegisterSelfAttested(t, "dispatch.test", "https://dispatch.test", challenge[:])),
		"backup eligible":  withReg(good, auth.RegisterBackupEligible(t, "dispatch.test", "https://dispatch.test", challenge[:])),
		"wrong origin":     withReg(good, auth.Register(t, "dispatch.test", "https://evil.test", challenge[:])),
		"wrong login nonce": {CredentialID: good.CredentialID, ChallengeNonce: nonce,
			Registration: auth.Register(t, "dispatch.test", "https://dispatch.test", ch(record.RegisterChallenge("mallory", nonce))), Seed: true},
		"aaguid off list":                offListAAGUID(t, ca),
		"zero aaguid":                    zeroAAGUID(t, ca),
		"wrong rpID":                     withReg(good, auth.Register(t, "evil-rp-id.test", "https://dispatch.test", challenge[:])),
		"aaguid cert extension mismatch": certAAGUIDMismatch(t, ca, aaguid),
	} {
		if _, err := v.VerifyRegistration("sjawhar", entry); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
