package webauthntest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
)

func TestRegistrationParsesAndItsAttestationChainsToTheCA(t *testing.T) {
	ca := NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a"))
	challenge := sha256.Sum256([]byte("agent-secrets/register/v1\nsjawhar\n" + strings.Repeat("0", 64)))
	raw := auth.Register(t, "dispatch.test", "https://dispatch.test", challenge[:])

	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if parsed.Response.AttestationObject.Format != "packed" {
		t.Fatalf("fmt %q", parsed.Response.AttestationObject.Format)
	}

	leafDER, _ := parsed.Response.AttestationObject.AttStatement["x5c"].([]any)
	if len(leafDER) != 1 {
		t.Fatalf("x5c chain length %d", len(leafDER))
	}

	leaf, err := x509.ParseCertificate(leafDER[0].([]byte))
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Root)

	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("leaf does not chain to the CA: %v", err)
	}
}

func TestAssertionSignatureVerifiesWithTheCredentialKey(t *testing.T) {
	ca := NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a"))
	challenge := sha256.Sum256([]byte("agent-secrets/assert/v1\nsjawhar\n" + strings.Repeat("0", 64)))

	first := auth.Assert(t, "dispatch.test", "https://dispatch.test", challenge[:])

	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}

	clientDataHash := sha256.Sum256(parsed.Raw.AssertionResponse.ClientDataJSON)
	signed := append(append([]byte{}, parsed.Raw.AssertionResponse.AuthenticatorData...), clientDataHash[:]...)
	digest := sha256.Sum256(signed)

	if !ecdsa.VerifyASN1(&auth.key.PublicKey, digest[:], parsed.Response.Signature) {
		t.Fatal("assertion signature does not verify with the credential's public key")
	}

	if parsed.Response.AuthenticatorData.Counter != 1 {
		t.Fatalf("counter after first assertion = %d, want 1", parsed.Response.AuthenticatorData.Counter)
	}

	second := auth.Assert(t, "dispatch.test", "https://dispatch.test", challenge[:])

	parsed2, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(second))
	if err != nil {
		t.Fatal(err)
	}

	if parsed2.Response.AuthenticatorData.Counter != 2 {
		t.Fatalf("counter after second assertion = %d, want 2", parsed2.Response.AuthenticatorData.Counter)
	}
}

func TestRegisterWithFmtNoneProducesFmtNone(t *testing.T) {
	ca := NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a"))
	challenge := sha256.Sum256([]byte("challenge"))

	raw := auth.RegisterWithFmtNone(t, "dispatch.test", "https://dispatch.test", challenge[:])

	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if parsed.Response.AttestationObject.Format != "none" {
		t.Fatalf("fmt %q, want none", parsed.Response.AttestationObject.Format)
	}

	if len(parsed.Response.AttestationObject.AttStatement) != 0 {
		t.Fatalf("attStmt %v, want empty", parsed.Response.AttestationObject.AttStatement)
	}
}

func TestRegisterSelfAttestedCarriesNoX5C(t *testing.T) {
	ca := NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a"))
	challenge := sha256.Sum256([]byte("challenge"))

	raw := auth.RegisterSelfAttested(t, "dispatch.test", "https://dispatch.test", challenge[:])

	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if parsed.Response.AttestationObject.Format != "packed" {
		t.Fatalf("fmt %q", parsed.Response.AttestationObject.Format)
	}

	if _, ok := parsed.Response.AttestationObject.AttStatement["x5c"]; ok {
		t.Fatal("self-attested attStmt carries an x5c chain")
	}
}

func TestRegisterBackupEligibleSetsBEFlag(t *testing.T) {
	ca := NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a"))
	challenge := sha256.Sum256([]byte("challenge"))

	raw := auth.RegisterBackupEligible(t, "dispatch.test", "https://dispatch.test", challenge[:])

	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if !parsed.Response.AttestationObject.AuthData.Flags.HasBackupEligible() {
		t.Fatal("BE flag not set")
	}
}

func TestAssertCrossOriginSetsCrossOriginTrue(t *testing.T) {
	ca := NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a"))
	challenge := sha256.Sum256([]byte("challenge"))

	raw := auth.AssertCrossOrigin(t, "dispatch.test", "https://dispatch.test", challenge[:])

	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if !parsed.Response.CollectedClientData.CrossOrigin {
		t.Fatal("crossOrigin not set")
	}
}

func TestAssertAtOriginCarriesTheGivenOrigin(t *testing.T) {
	ca := NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a"))
	challenge := sha256.Sum256([]byte("challenge"))

	raw := auth.AssertAtOrigin(t, "dispatch.test", "https://evil.test", challenge[:])

	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if parsed.Response.CollectedClientData.Origin != "https://evil.test" {
		t.Fatalf("origin %q", parsed.Response.CollectedClientData.Origin)
	}
}

func TestAssertWithCounterOverridesTheCounter(t *testing.T) {
	ca := NewCA(t)
	auth := ca.NewAuthenticator(t, uuid.MustParse("ee882879-721c-4913-9775-3dfcce97072a"))
	challenge := sha256.Sum256([]byte("challenge"))

	raw := auth.AssertWithCounter(t, "dispatch.test", "https://dispatch.test", challenge[:], 41)

	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	if parsed.Response.AuthenticatorData.Counter != 41 {
		t.Fatalf("counter %d, want 41", parsed.Response.AuthenticatorData.Counter)
	}

	// A replayed (non-increasing) counter is a real negative case: asserting again with the same
	// explicit value must reproduce it rather than silently advancing.
	raw2 := auth.AssertWithCounter(t, "dispatch.test", "https://dispatch.test", challenge[:], 41)

	parsed2, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(raw2))
	if err != nil {
		t.Fatal(err)
	}

	if parsed2.Response.AuthenticatorData.Counter != 41 {
		t.Fatalf("counter %d, want 41 (replayed)", parsed2.Response.AuthenticatorData.Counter)
	}
}
