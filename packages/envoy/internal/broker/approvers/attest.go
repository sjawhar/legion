// Package approvers is the persisted, attested WebAuthn key set that everything downstream —
// rules evaluation, credential-request actions, machine logins, the UI API — checks approver
// assertions against (AGENTC-393 design v4, contract v9).
//
// This file, attest.go, verifies one registration ceremony's attestation. It parses with
// go-webauthn's low-level protocol package (ParseCredentialCreationResponseBytes) and never calls
// go-webauthn's own AttestationObject.Verify/VerifyAttestation: those accept attestation format
// "none" and self attestation by default (red-team N4), which contract v9 explicitly refuses.
// Every check below is therefore this package's own, copied from contract v9's "Registration
// verification" section rather than reinvented.
package approvers

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// ErrRegistrationInvalid is wrapped around every registration verification failure.
var ErrRegistrationInvalid = errors.New("registration invalid")

// oidFIDOGenCeAAGUID is the id-fido-gen-ce-aaguid X.509 extension OID (WebAuthn §8.2.1), carrying
// an attestation certificate's AAGUID DER-encoded as an OCTET STRING. go-webauthn keeps its own
// copy of this OID unexported (protocol's const.go), so it is defined again here.
var oidFIDOGenCeAAGUID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 45724, 1, 1, 4}

// Verifier holds the trust material every attestation and assertion check needs. Roots is always
// a constructor parameter — the embedded Yubico roots (internal/broker/approvers/roots) in
// production, a generated test CA (internal/broker/webauthntest) in tests — never read from the
// environment.
type Verifier struct {
	Roots   *x509.CertPool
	Origin  string // BROKER_UI_ORIGIN
	AAGUIDs map[uuid.UUID]bool
}

// Registered is one KeyEntry's verified attestation: the credential id, AAGUID, and public key an
// approver key set persists.
type Registered struct {
	CredentialID []byte
	AAGUID       uuid.UUID
	COSEKey      []byte
	PublicKey    any
}

// VerifyRegistration runs the contract v9 "Registration verification" checks and returns the
// parsed key:
//
//   - fmt == "packed" with x5c (basic attestation) chaining to a pinned root (Roots); fmt "none"
//     and self attestation (no x5c) are refused explicitly.
//   - AAGUID non-zero, on Roots.AAGUIDs, equal to the certificate's id-fido-gen-ce-aaguid
//     extension when present.
//   - authData flags UP, UV, AT set, BE=0, BS=0.
//   - clientDataJSON.type == "webauthn.create" at exactly Origin, crossOrigin false, no
//     topOrigin, challenge equal to record.RegisterChallenge(login, e.ChallengeNonce).
func (v *Verifier) VerifyRegistration(login string, e KeyEntry) (Registered, error) {
	parsed, err := protocol.ParseCredentialCreationResponseBytes(e.Registration)
	if err != nil {
		return Registered{}, fmt.Errorf("%w: parse response: %s", ErrRegistrationInvalid, err)
	}

	ccd := parsed.Response.CollectedClientData
	if ccd.Type != protocol.CreateCeremony {
		return Registered{}, fmt.Errorf("%w: clientData.type is %q, not webauthn.create", ErrRegistrationInvalid, ccd.Type)
	}
	if ccd.Origin != v.Origin {
		return Registered{}, fmt.Errorf("%w: clientData.origin %q does not match %q", ErrRegistrationInvalid, ccd.Origin, v.Origin)
	}
	if ccd.CrossOrigin {
		return Registered{}, fmt.Errorf("%w: clientData.crossOrigin must be false", ErrRegistrationInvalid)
	}
	if ccd.TopOrigin != "" {
		return Registered{}, fmt.Errorf("%w: clientData.topOrigin must be absent", ErrRegistrationInvalid)
	}
	challengeBytes, err := base64.RawURLEncoding.DecodeString(ccd.Challenge)
	if err != nil {
		return Registered{}, fmt.Errorf("%w: challenge is not base64url", ErrRegistrationInvalid)
	}
	expectedChallenge := record.RegisterChallenge(login, e.ChallengeNonce)
	if !bytes.Equal(challengeBytes, expectedChallenge[:]) {
		return Registered{}, fmt.Errorf("%w: challenge does not match record.RegisterChallenge(login, nonce)", ErrRegistrationInvalid)
	}

	att := parsed.Response.AttestationObject
	if att.Format != "packed" {
		return Registered{}, fmt.Errorf("%w: attestation format %q is refused (only packed basic attestation is accepted; go-webauthn defaults to accepting fmt:none, which this function overrides)", ErrRegistrationInvalid, att.Format)
	}
	x5cRaw, ok := att.AttStatement["x5c"]
	if !ok {
		return Registered{}, fmt.Errorf("%w: self attestation (no x5c) is refused", ErrRegistrationInvalid)
	}
	x5cList, ok := x5cRaw.([]any)
	if !ok || len(x5cList) == 0 {
		return Registered{}, fmt.Errorf("%w: attStmt.x5c is not a non-empty certificate array", ErrRegistrationInvalid)
	}
	leafDER, ok := x5cList[0].([]byte)
	if !ok {
		return Registered{}, fmt.Errorf("%w: attStmt.x5c[0] is not a byte string", ErrRegistrationInvalid)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return Registered{}, fmt.Errorf("%w: parse attestation certificate: %s", ErrRegistrationInvalid, err)
	}
	intermediates := x509.NewCertPool()
	for i, raw := range x5cList[1:] {
		der, ok := raw.([]byte)
		if !ok {
			return Registered{}, fmt.Errorf("%w: attStmt.x5c[%d] is not a byte string", ErrRegistrationInvalid, i+1)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return Registered{}, fmt.Errorf("%w: parse attStmt.x5c[%d]: %s", ErrRegistrationInvalid, i+1, err)
		}
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: v.Roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return Registered{}, fmt.Errorf("%w: attestation certificate does not chain to a trusted root: %s", ErrRegistrationInvalid, err)
	}

	sigRaw, ok1 := att.AttStatement["sig"]
	sig, ok2 := sigRaw.([]byte)
	if !ok1 || !ok2 {
		return Registered{}, fmt.Errorf("%w: attStmt.sig is missing", ErrRegistrationInvalid)
	}
	clientDataHash := sha256.Sum256([]byte(parsed.Raw.AttestationResponse.ClientDataJSON))
	signedData := append(append([]byte{}, att.RawAuthData...), clientDataHash[:]...)
	if err := leaf.CheckSignature(x509.ECDSAWithSHA256, signedData, sig); err != nil {
		return Registered{}, fmt.Errorf("%w: attestation signature: %s", ErrRegistrationInvalid, err)
	}

	ad := att.AuthData
	if !ad.Flags.HasUserPresent() || !ad.Flags.HasUserVerified() || !ad.Flags.HasAttestedCredentialData() {
		return Registered{}, fmt.Errorf("%w: authData flags require UP, UV, and AT set", ErrRegistrationInvalid)
	}
	if ad.Flags.HasBackupEligible() || ad.Flags.HasBackupState() {
		return Registered{}, fmt.Errorf("%w: authData BE and BS must both be 0", ErrRegistrationInvalid)
	}
	host, err := v.originHost()
	if err != nil {
		return Registered{}, fmt.Errorf("%w: %s", ErrRegistrationInvalid, err)
	}
	rpIDHash := sha256.Sum256([]byte(host))
	if !bytes.Equal(ad.RPIDHash, rpIDHash[:]) {
		return Registered{}, fmt.Errorf("%w: authData.rpIdHash does not match sha256(host(origin))", ErrRegistrationInvalid)
	}

	aaguid, err := uuid.FromBytes(ad.AttData.AAGUID)
	if err != nil {
		return Registered{}, fmt.Errorf("%w: attested AAGUID: %s", ErrRegistrationInvalid, err)
	}
	if aaguid == uuid.Nil {
		return Registered{}, fmt.Errorf("%w: AAGUID is zero", ErrRegistrationInvalid)
	}
	if !v.AAGUIDs[aaguid] {
		return Registered{}, fmt.Errorf("%w: AAGUID %s is not on the allowed list", ErrRegistrationInvalid, aaguid)
	}
	certAAGUID, found, err := attestationCertAAGUID(leaf)
	if err != nil {
		return Registered{}, fmt.Errorf("%w: attestation certificate AAGUID extension: %s", ErrRegistrationInvalid, err)
	}
	if found {
		certUUID, err := uuid.FromBytes(certAAGUID)
		if err != nil || certUUID != aaguid {
			return Registered{}, fmt.Errorf("%w: attestation certificate AAGUID does not match the attested AAGUID", ErrRegistrationInvalid)
		}
	}

	credentialID := ad.AttData.CredentialID
	entryCredentialID, err := base64.RawURLEncoding.DecodeString(e.CredentialID)
	if err != nil || !bytes.Equal(entryCredentialID, credentialID) {
		return Registered{}, fmt.Errorf("%w: entry credential_id does not match the attested credential", ErrRegistrationInvalid)
	}

	pub, err := webauthncose.ParsePublicKey(ad.AttData.CredentialPublicKey)
	if err != nil {
		return Registered{}, fmt.Errorf("%w: credential public key: %s", ErrRegistrationInvalid, err)
	}

	return Registered{
		CredentialID: credentialID,
		AAGUID:       aaguid,
		COSEKey:      ad.AttData.CredentialPublicKey,
		PublicKey:    pub,
	}, nil
}

// originHost is the "host" component (RFC 3986 §3.2.2, no port) of Verifier.Origin, the value
// rpIdHash is checked against in both registration and assertion verification.
func (v *Verifier) originHost() (string, error) {
	u, err := url.Parse(v.Origin)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("origin %q is not a valid URL", v.Origin)
	}
	return u.Hostname(), nil
}

// attestationCertAAGUID extracts the AAGUID from an attestation certificate's
// id-fido-gen-ce-aaguid extension (WebAuthn §8.2.1: the DER encoding of the value is itself an
// OCTET STRING, so the AAGUID is wrapped in two OCTET STRINGs). found reports whether the
// extension was present at all.
func attestationCertAAGUID(cert *x509.Certificate) (aaguid []byte, found bool, err error) {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(oidFIDOGenCeAAGUID) {
			continue
		}
		found = true
		if _, err := asn1.Unmarshal(ext.Value, &aaguid); err != nil {
			return nil, true, err
		}
	}
	return aaguid, found, nil
}
