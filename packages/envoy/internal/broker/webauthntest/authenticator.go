// Package webauthntest is a software WebAuthn authenticator for tests. It produces real packed
// attestation (a CBOR attestationObject, a COSE ES256 public key, and an x5c leaf chaining to a
// generated test CA carrying the FIDO AAGUID extension) and real assertions, both parseable by the
// real go-webauthn protocol package. It is never wired into production; production attestation
// verification trusts the embedded Yubico roots instead of a CA built here.
package webauthntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
)

// oidFIDOGenCeAAGUID is the id-fido-gen-ce-aaguid X.509 extension OID (WebAuthn §8.2.1), which
// carries an attestation certificate's AAGUID DER-encoded as an OCTET STRING.
var oidFIDOGenCeAAGUID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 45724, 1, 1, 4}

// ctap2EncMode encodes CBOR in the canonical CTAP2 form (bytewise-lexicographic map key order, no
// indefinite-length items, no tags) real authenticators use and the real go-webauthn decoder
// requires.
var ctap2EncMode cbor.EncMode

func init() {
	mode, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	ctap2EncMode = mode
}

// Authenticator data flag bits (WebAuthn §6.1, table "Authenticator Data").
const (
	flagUserPresent            byte = 1 << 0
	flagUserVerified           byte = 1 << 2
	flagBackupEligible         byte = 1 << 3
	flagAttestedCredentialData byte = 1 << 6
)

// COSE_Key parameters used for every credential and attestation key this package produces: EC2,
// ES256, P-256 (RFC 9053 §7.1, §7.2).
const (
	coseKeyTypeEC2 = 2
	coseAlgES256   = -7
	coseCurveP256  = 1
)

// CA is a generated attestation CA (ECDSA P-256 root) tests hand to the verifier as its trust
// root. Never used in production; production wiring passes the embedded Yubico roots instead.
type CA struct {
	Root    *x509.Certificate
	RootPEM []byte
	key     *ecdsa.PrivateKey
}

// NewCA generates a fresh ECDSA P-256 self-signed root certificate.
func NewCA(t testing.TB) *CA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("webauthntest: generate CA key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: mustSerial(t),
		Subject: pkix.Name{
			CommonName:   "webauthntest root CA",
			Organization: []string{"webauthntest"},
			Country:      []string{"US"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("webauthntest: create CA certificate: %v", err)
	}

	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("webauthntest: parse CA certificate: %v", err)
	}

	return &CA{
		Root:    root,
		RootPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:     key,
	}
}

// Pool returns an *x509.CertPool trusting only the CA's root, for handing to a verifier under
// test as its attestation trust root.
func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Root)
	return pool
}

// Authenticator is a software authenticator holding one credential, plus an attestation leaf
// certificate signed by its CA for producing basic (x5c) packed attestation.
type Authenticator struct {
	CredentialID []byte
	AAGUID       uuid.UUID
	signCount    uint32
	key          *ecdsa.PrivateKey

	attCert *x509.Certificate
	attKey  *ecdsa.PrivateKey
}

// NewAuthenticator generates a fresh credential key pair, a credential id, and an attestation
// leaf certificate signed by ca that carries the id-fido-gen-ce-aaguid extension.
func (ca *CA) NewAuthenticator(t testing.TB, aaguid uuid.UUID) *Authenticator {
	t.Helper()
	return ca.newAuthenticator(t, aaguid, aaguid)
}

// NewAuthenticatorWithCertAAGUID is NewAuthenticator except the attestation certificate's
// id-fido-gen-ce-aaguid extension carries certAAGUID while the attested credential data
// (authData, what a Relying Party reads as "the AAGUID") carries dataAAGUID — for negative-case
// testing of a Relying Party that must refuse a certificate whose extension AAGUID disagrees with
// the attested one.
func (ca *CA) NewAuthenticatorWithCertAAGUID(t testing.TB, dataAAGUID, certAAGUID uuid.UUID) *Authenticator {
	t.Helper()
	return ca.newAuthenticator(t, dataAAGUID, certAAGUID)
}

func (ca *CA) newAuthenticator(t testing.TB, dataAAGUID, certAAGUID uuid.UUID) *Authenticator {
	t.Helper()

	credKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("webauthntest: generate credential key: %v", err)
	}

	credentialID := make([]byte, 16)
	if _, err := rand.Read(credentialID); err != nil {
		t.Fatalf("webauthntest: generate credential id: %v", err)
	}

	attKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("webauthntest: generate attestation key: %v", err)
	}

	aaguidValue, err := asn1.Marshal(certAAGUID[:])
	if err != nil {
		t.Fatalf("webauthntest: marshal AAGUID extension: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: mustSerial(t),
		Subject: pkix.Name{
			CommonName:         certAAGUID.String(),
			Organization:       []string{"webauthntest"},
			OrganizationalUnit: []string{"Authenticator Attestation"},
			Country:            []string{"US"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  false,
		ExtraExtensions: []pkix.Extension{{
			Id:       oidFIDOGenCeAAGUID,
			Critical: false,
			Value:    aaguidValue,
		}},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Root, &attKey.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("webauthntest: create leaf certificate: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("webauthntest: parse leaf certificate: %v", err)
	}

	return &Authenticator{
		CredentialID: credentialID,
		AAGUID:       dataAAGUID,
		key:          credKey,
		attCert:      leaf,
		attKey:       attKey,
	}
}

func mustSerial(t testing.TB) *big.Int {
	t.Helper()

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("webauthntest: generate certificate serial: %v", err)
	}

	return serial
}

// collectedClientData is the wire shape of protocol.CollectedClientData that this package
// produces; a software authenticator plays the client's role too, so it builds clientDataJSON
// directly rather than through a browser.
type collectedClientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

func clientDataJSON(t testing.TB, ceremonyType, origin string, challenge []byte, crossOrigin bool) []byte {
	t.Helper()

	raw, err := json.Marshal(collectedClientData{
		Type:        ceremonyType,
		Challenge:   base64.RawURLEncoding.EncodeToString(challenge),
		Origin:      origin,
		CrossOrigin: crossOrigin,
	})
	if err != nil {
		t.Fatalf("webauthntest: marshal clientDataJSON: %v", err)
	}

	return raw
}

// authDataHeader is the fixed 37-byte prefix of authenticatorData: rpIdHash(32) + flags(1) +
// counter(4), per the table at WebAuthn §6.1.
func authDataHeader(rpID string, flags byte, counter uint32) []byte {
	rpIDHash := sha256.Sum256([]byte(rpID))

	header := make([]byte, 0, 37)
	header = append(header, rpIDHash[:]...)
	header = append(header, flags)

	var counterBytes [4]byte
	binary.BigEndian.PutUint32(counterBytes[:], counter)

	return append(header, counterBytes[:]...)
}

// coseEC2Key is a COSE_Key EC2 public key (RFC 9053 §7.1.1), encoded with integer map keys as
// CTAP2 canonical CBOR requires.
type coseEC2Key struct {
	KeyType   int64  `cbor:"1,keyasint"`
	Algorithm int64  `cbor:"3,keyasint"`
	Curve     int64  `cbor:"-1,keyasint"`
	X         []byte `cbor:"-2,keyasint"`
	Y         []byte `cbor:"-3,keyasint"`
}

func cosePublicKey(t testing.TB, pub *ecdsa.PublicKey) []byte {
	t.Helper()

	x := make([]byte, 32)
	y := make([]byte, 32)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)

	raw, err := ctap2EncMode.Marshal(coseEC2Key{
		KeyType:   coseKeyTypeEC2,
		Algorithm: coseAlgES256,
		Curve:     coseCurveP256,
		X:         x,
		Y:         y,
	})
	if err != nil {
		t.Fatalf("webauthntest: marshal COSE public key: %v", err)
	}

	return raw
}

// attestedCredentialData is AAGUID(16) + credentialIdLength(2, big-endian) + credentialId +
// credentialPublicKey, per WebAuthn §6.5.2.
func (a *Authenticator) attestedCredentialData(t testing.TB) []byte {
	t.Helper()

	var idLen [2]byte
	binary.BigEndian.PutUint16(idLen[:], uint16(len(a.CredentialID)))

	data := make([]byte, 0, 16+2+len(a.CredentialID)+64)
	data = append(data, a.AAGUID[:]...)
	data = append(data, idLen[:]...)
	data = append(data, a.CredentialID...)
	data = append(data, cosePublicKey(t, &a.key.PublicKey)...)

	return data
}

// sign returns a DER (ASN.1) ECDSA signature over SHA-256(message) with key, the encoding every
// packed-attestation and assertion signature this package produces uses.
func sign(t testing.TB, key *ecdsa.PrivateKey, message []byte) []byte {
	t.Helper()

	digest := sha256.Sum256(message)

	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("webauthntest: sign: %v", err)
	}

	return sig
}

// attestationObjectWire is the CBOR wire shape of an attestation object (WebAuthn §6.5): fmt,
// attStmt, authData. It has no counterpart exported by go-webauthn, so it is defined locally.
type attestationObjectWire struct {
	Fmt      string         `cbor:"fmt"`
	AttStmt  map[string]any `cbor:"attStmt"`
	AuthData []byte         `cbor:"authData"`
}

// publicKeyCredentialJSON is the common envelope of both a RegistrationResponseJSON and an
// AuthenticationResponseJSON (protocol.PublicKeyCredential's wire shape).
type publicKeyCredentialJSON struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response any    `json:"response"`
}

type attestationResponseJSON struct {
	ClientDataJSON    string `json:"clientDataJSON"`
	AttestationObject string `json:"attestationObject"`
}

type assertionResponseJSON struct {
	ClientDataJSON    string `json:"clientDataJSON"`
	AuthenticatorData string `json:"authenticatorData"`
	Signature         string `json:"signature"`
}

// registerOptions selects the negative-case variants covered by the Register* methods below.
type registerOptions struct {
	fmtNone        bool
	selfAttested   bool
	backupEligible bool
}

// Register produces a RegistrationResponseJSON (protocol.CredentialCreationResponse wire shape):
// clientDataJSON {type: webauthn.create, challenge, origin, crossOrigin: false}; authData with
// rpIdHash=SHA-256(rpID), flags UP|UV|AT, the AAGUID, credential id, COSE ES256 public key;
// attestationObject CBOR {fmt: "packed", attStmt: {alg: -7, sig, x5c: [leaf]}, authData} where the
// leaf is signed by the CA and carries id-fido-gen-ce-aaguid (1.3.6.1.4.1.45724.1.1.4) = AAGUID.
func (a *Authenticator) Register(t testing.TB, rpID, origin string, challenge []byte) json.RawMessage {
	t.Helper()
	return a.register(t, rpID, origin, challenge, registerOptions{})
}

// RegisterWithFmtNone produces a registration whose attestation format is "none" (an empty
// attStmt), for negative-case testing of a Relying Party that must refuse it.
func (a *Authenticator) RegisterWithFmtNone(t testing.TB, rpID, origin string, challenge []byte) json.RawMessage {
	t.Helper()
	return a.register(t, rpID, origin, challenge, registerOptions{fmtNone: true})
}

// RegisterSelfAttested produces a registration attested with the credential's own private key
// (WebAuthn §8.2 self attestation) rather than the CA-signed leaf, and carries no x5c.
func (a *Authenticator) RegisterSelfAttested(t testing.TB, rpID, origin string, challenge []byte) json.RawMessage {
	t.Helper()
	return a.register(t, rpID, origin, challenge, registerOptions{selfAttested: true})
}

// RegisterBackupEligible produces an otherwise ordinary basic-attestation registration with the
// authData BE (backup eligible) flag set, for negative-case testing of a Relying Party that
// requires BE=0.
func (a *Authenticator) RegisterBackupEligible(t testing.TB, rpID, origin string, challenge []byte) json.RawMessage {
	t.Helper()
	return a.register(t, rpID, origin, challenge, registerOptions{backupEligible: true})
}

func (a *Authenticator) register(t testing.TB, rpID, origin string, challenge []byte, opts registerOptions) json.RawMessage {
	t.Helper()

	flags := flagUserPresent | flagUserVerified | flagAttestedCredentialData
	if opts.backupEligible {
		flags |= flagBackupEligible
	}

	// Registration does not advance the signature counter; only Assert does.
	authData := append(authDataHeader(rpID, flags, 0), a.attestedCredentialData(t)...)

	cdj := clientDataJSON(t, "webauthn.create", origin, challenge, false)
	clientDataHash := sha256.Sum256(cdj)
	signedData := append(append([]byte{}, authData...), clientDataHash[:]...)

	var (
		format  string
		attStmt map[string]any
	)

	switch {
	case opts.fmtNone:
		format = "none"
		attStmt = map[string]any{}
	case opts.selfAttested:
		format = "packed"
		attStmt = map[string]any{
			"alg": int64(coseAlgES256),
			"sig": sign(t, a.key, signedData),
		}
	default:
		format = "packed"
		attStmt = map[string]any{
			"alg": int64(coseAlgES256),
			"sig": sign(t, a.attKey, signedData),
			"x5c": []any{a.attCert.Raw},
		}
	}

	attObj, err := ctap2EncMode.Marshal(attestationObjectWire{Fmt: format, AttStmt: attStmt, AuthData: authData})
	if err != nil {
		t.Fatalf("webauthntest: marshal attestationObject: %v", err)
	}

	return marshalCredential(t, a.CredentialID, attestationResponseJSON{
		ClientDataJSON:    base64.RawURLEncoding.EncodeToString(cdj),
		AttestationObject: base64.RawURLEncoding.EncodeToString(attObj),
	})
}

// assertOptions selects the negative-case variants covered by the Assert* methods below.
type assertOptions struct {
	crossOrigin bool
	counter     *uint32
}

// Assert produces an AuthenticationResponseJSON over challenge with flags UP|UV, BE=0, BS=0,
// crossOrigin false, and an incremented signature counter.
func (a *Authenticator) Assert(t testing.TB, rpID, origin string, challenge []byte) json.RawMessage {
	t.Helper()
	return a.assert(t, rpID, origin, challenge, assertOptions{})
}

// AssertWithCounter produces an assertion carrying counter as its authData signature counter
// instead of the authenticator's ordinary auto-increment, for negative-case testing of a Relying
// Party that must refuse a stalled or replayed counter. It also becomes the authenticator's
// stored counter, so a later ordinary Assert continues incrementing from it.
func (a *Authenticator) AssertWithCounter(t testing.TB, rpID, origin string, challenge []byte, counter uint32) json.RawMessage {
	t.Helper()
	return a.assert(t, rpID, origin, challenge, assertOptions{counter: &counter})
}

// AssertCrossOrigin produces an otherwise ordinary assertion with clientDataJSON crossOrigin
// true, for negative-case testing of a Relying Party that must refuse cross-origin assertions.
func (a *Authenticator) AssertCrossOrigin(t testing.TB, rpID, origin string, challenge []byte) json.RawMessage {
	t.Helper()
	return a.assert(t, rpID, origin, challenge, assertOptions{crossOrigin: true})
}

// AssertAtOrigin is Assert named for test readability at its call site: the assertion's
// clientDataJSON carries origin verbatim, so a caller passing a value other than the Relying
// Party's configured origin produces a wrong-origin assertion for negative-case testing.
func (a *Authenticator) AssertAtOrigin(t testing.TB, rpID, origin string, challenge []byte) json.RawMessage {
	t.Helper()
	return a.Assert(t, rpID, origin, challenge)
}

func (a *Authenticator) assert(t testing.TB, rpID, origin string, challenge []byte, opts assertOptions) json.RawMessage {
	t.Helper()

	counter := a.signCount + 1
	if opts.counter != nil {
		counter = *opts.counter
	}
	a.signCount = counter

	authData := authDataHeader(rpID, flagUserPresent|flagUserVerified, counter)

	cdj := clientDataJSON(t, "webauthn.get", origin, challenge, opts.crossOrigin)
	clientDataHash := sha256.Sum256(cdj)
	signedData := append(append([]byte{}, authData...), clientDataHash[:]...)

	return marshalCredential(t, a.CredentialID, assertionResponseJSON{
		ClientDataJSON:    base64.RawURLEncoding.EncodeToString(cdj),
		AuthenticatorData: base64.RawURLEncoding.EncodeToString(authData),
		Signature:         base64.RawURLEncoding.EncodeToString(sign(t, a.key, signedData)),
	})
}

func marshalCredential(t testing.TB, credentialID []byte, response any) json.RawMessage {
	t.Helper()

	id := base64.RawURLEncoding.EncodeToString(credentialID)

	raw, err := json.Marshal(publicKeyCredentialJSON{
		ID:       id,
		RawID:    id,
		Type:     "public-key",
		Response: response,
	})
	if err != nil {
		t.Fatalf("webauthntest: marshal credential response: %v", err)
	}

	return raw
}
