package record

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

func secretDetail() []AuthorizationDetail {
	return []AuthorizationDetail{
		{Type: "agent_secret", Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
	}
}

func TestCanonicalBodyIsByteExactAndIDIsItsSHA256(t *testing.T) {
	b := Body{
		Request:         "eyJ.fake.jws",
		Approver:        "sjawhar",
		Enrollment:      Enrollment{Kind: "box", RuntimeID: "agentbox-1234", Operator: "sjawhar"},
		LifetimeSeconds: 43200,
		RulesVersion:    "ab12",
		ExpiresAt:       time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
	}
	want := "agent-secrets-record/v1\n" +
		"request: eyJ.fake.jws\n" +
		"approver: sjawhar\n" +
		"enrollment: box\tagentbox-1234\tsjawhar\n" +
		"lifetime_seconds: 43200\n" +
		"rules_version: ab12\n" +
		"expires_at: 2026-09-27T12:00:00Z\n" +
		"code: -\n"
	if got := b.Canonical(); got != want {
		t.Fatalf("canonical body:\n%q\nwant:\n%q", got, want)
	}
	sum := sha256.Sum256([]byte(want))
	if b.ID() != hex.EncodeToString(sum[:]) {
		t.Fatalf("id %s is not the body's sha256", b.ID())
	}
	back, err := ParseBody(want)
	if err != nil || back != b {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestChallengesAreDomainSeparated(t *testing.T) {
	id := strings.Repeat("ab", 32)
	approve, deny := ApproveChallenge(id), DenyChallenge(id)
	if approve == deny {
		t.Fatal("approve and deny hash the same bytes")
	}
	want := sha256.Sum256([]byte("agent-secrets/approve/v1\n" + id))
	if approve != want {
		t.Fatalf("approve challenge = %x, want %x", approve, want)
	}
	if EndorseChallenge("sjawhar", id) != sha256.Sum256([]byte("agent-secrets/endorse/v1\nsjawhar\n"+id)) {
		t.Fatal("endorse construction")
	}
}

func TestVerifyRequestObjectAcceptsItsOwnSignAndRefusesTheProofTyp(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", []AuthorizationDetail{
		{Type: "agent_secret", Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
	}, "deel sync for AGENTC-1", "", now)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	tp, _ := proof.Thumbprint(&key.PublicKey)
	if ro.Thumbprint != tp || ro.Details[0].Identifier != "DEEL_API_KEY" || ro.LoginHint != "" {
		t.Fatalf("claims: %+v", ro)
	}
	// A per-call proof must never pass as a request object.
	p, _ := proof.Sign(key, "enr-1", "POST", "https://secrets.test/v1/requests", now)
	if _, err := VerifyRequestObject(p, "https://secrets.test", time.Minute, now); err == nil {
		t.Fatal("a proof-typ JWS passed as a request object")
	}
}

func TestVerifyRequestObjectRefusals(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	for name, tc := range map[string]struct {
		details []AuthorizationDetail
		reason  string
		hint    string
		aud     string
	}{
		"wrong audience":        {details: secretDetail(), aud: "https://evil.test"},
		"bidi in reason":        {details: secretDetail(), reason: "ok\u202Ekcatta", aud: "https://secrets.test"},
		"zero-width in reason":  {details: secretDetail(), reason: "a\u200bb", aud: "https://secrets.test"},
		"401-rune reason":       {details: secretDetail(), reason: strings.Repeat("r", 401), aud: "https://secrets.test"},
		"mixed types":           {details: append(secretDetail(), AuthorizationDetail{Type: "launcher_credential", Identifier: "h.example"}), aud: "https://secrets.test"},
		"bad hostname":          {details: []AuthorizationDetail{{Type: "launcher_credential", Identifier: "UPPER_case!"}}, aud: "https://secrets.test"},
		"empty details":         {details: nil, aud: "https://secrets.test"},
		"login_hint on session": {details: secretDetail(), hint: "sjawhar", aud: "https://secrets.test"},
	} {
		t.Run(name, func(t *testing.T) {
			compact, err := Sign(key, tc.aud, tc.details, tc.reason, tc.hint, now)
			if err != nil {
				t.Fatal(err)
			}
			ro, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
			if name == "login_hint on session" {
				// Verify returns the claims; the login_hint-forbidden rule is per caller
				// (session route refuses it, machine route requires it), so here it parses.
				if err != nil || ro.LoginHint != "sjawhar" {
					t.Fatalf("login_hint should parse: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestParseBodyRefusesAnyEditedLine(t *testing.T) {
	valid := "agent-secrets-record/v1\n" +
		"request: eyJ.fake.jws\n" +
		"approver: sjawhar\n" +
		"enrollment: box\tagentbox-1234\tsjawhar\n" +
		"lifetime_seconds: 43200\n" +
		"rules_version: ab12\n" +
		"expires_at: 2026-09-27T12:00:00Z\n" +
		"code: -\n"
	if _, err := ParseBody(valid); err != nil {
		t.Fatalf("valid body should parse: %v", err)
	}
	lines := strings.Split(valid, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		t.Run(line, func(t *testing.T) {
			mutated := make([]string, len(lines))
			copy(mutated, lines)
			b := []byte(line)
			b[0] ^= 0x01 // flip a bit of the line's first byte
			mutated[i] = string(b)
			if _, err := ParseBody(strings.Join(mutated, "\n")); err == nil {
				t.Fatalf("edited line %d (%q) parsed without error", i, line)
			}
		})
	}
}

func TestSignExpiresTenMinutesAfterIat(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", secretDetail(), "reason", "", now)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ro.Expires.Unix(), ro.IssuedAt.Unix()+600; got != want {
		t.Fatalf("exp = %d, want iat+600 = %d", got, want)
	}
}

func TestMachineBodyUsesDashEnrollmentAndCarriesCode(t *testing.T) {
	b := Body{
		Request:         "eyJ.fake.jws",
		Approver:        "sjawhar",
		Enrollment:      Enrollment{Kind: "-", RuntimeID: "-", Operator: ""},
		LifetimeSeconds: 604800,
		RulesVersion:    "ab12",
		ExpiresAt:       time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
		Code:            "ABCD-EFGH",
	}
	got := b.Canonical()
	if !strings.Contains(got, "enrollment: -\t-\t-\n") {
		t.Fatalf("canonical body missing dash enrollment:\n%q", got)
	}
	if !strings.Contains(got, "code: ABCD-EFGH\n") {
		t.Fatalf("canonical body missing code:\n%q", got)
	}
	back, err := ParseBody(got)
	if err != nil || back != b {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestForgedSignatureFailsForRequestObject(t *testing.T) {
	attackerKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	victimKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	victimThumbprint, err := proof.Thumbprint(&victimKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: attackerKey}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: RequestTyp},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	payload, err := json.Marshal(requestClaims{
		Issuer:               victimThumbprint,
		Audience:             "https://secrets.test",
		JTI:                  "forged-jti",
		IssuedAt:             now.Unix(),
		Expires:              now.Unix() + 600,
		AuthorizationDetails: secretDetail(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}

	// Swap the embedded JWK in the protected header for the victim's public key, so the request
	// object claims to be signed by the victim while the bytes were actually signed by the
	// attacker. Re-verifying against the swapped protected header must fail: the signing input
	// no longer matches what the attacker actually signed.
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatalf("expected compact JWS with 3 parts, got %d", len(parts))
	}
	rawProtected, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var protected map[string]any
	if err := json.Unmarshal(rawProtected, &protected); err != nil {
		t.Fatal(err)
	}
	victimJWK, err := (jose.JSONWebKey{Key: &victimKey.PublicKey}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var jwkMap map[string]any
	if err := json.Unmarshal(victimJWK, &jwkMap); err != nil {
		t.Fatal(err)
	}
	protected["jwk"] = jwkMap
	forgedProtected, err := json.Marshal(protected)
	if err != nil {
		t.Fatal(err)
	}
	forged := base64.RawURLEncoding.EncodeToString(forgedProtected) + "." + parts[1] + "." + parts[2]

	_, err = VerifyRequestObject(forged, "https://secrets.test", time.Minute, now)
	if err == nil || !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("forged signature (embedded key not the actual signer) must be rejected with ErrRequestInvalid mentioning signature, got %v", err)
	}
}

func TestUnsignedTypHeaderFailsForRequestObject(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{EmbedJWK: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(requestClaims{
		Issuer:               thumbprint,
		Audience:             "https://secrets.test",
		JTI:                  "no-typ-jti",
		IssuedAt:             now.Unix(),
		Expires:              now.Unix() + 600,
		AuthorizationDetails: secretDetail(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatalf("expected compact JWS with 3 parts, got %d", len(parts))
	}
	// The protected header this JWS was actually signed over carries no "typ" claim at all. A
	// JSON-serialized JWS lets an attacker attach one through the unsigned "header" field
	// instead; the typ check must only trust the protected (signed) header.
	jsonSerialized, err := json.Marshal(map[string]any{
		"payload":   parts[1],
		"protected": parts[0],
		"header":    map[string]string{"typ": RequestTyp},
		"signature": parts[2],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRequestObject(string(jsonSerialized), "https://secrets.test", time.Minute, now); err == nil || !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("typ present only in the unsigned header must be rejected with ErrRequestInvalid, got %v", err)
	}
}
