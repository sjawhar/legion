package agentsecrets

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func mustTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// decodeCompactJWSRaw splits a compact JWS on "." and base64url-decodes the header and payload
// into generic maps, exactly as a golden-claims test must: no library re-verification, just the
// wire bytes a broker (or an attacker) would see. It reports errors rather than failing a test
// directly so it is safe to call from an HTTP handler goroutine (fakeBroker in client_test.go),
// where testing.T.Fatal is not.
func decodeCompactJWSRaw(compact string) (header, payload map[string]any, err error) {
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return nil, nil, fmt.Errorf("compact JWS has %d dot-separated parts, want 3: %s", len(parts), compact)
	}
	decode := func(part string) (map[string]any, error) {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			return nil, fmt.Errorf("base64url decode: %w", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("json unmarshal %s: %w", raw, err)
		}
		return m, nil
	}
	if header, err = decode(parts[0]); err != nil {
		return nil, nil, err
	}
	if payload, err = decode(parts[1]); err != nil {
		return nil, nil, err
	}
	return header, payload, nil
}

// decodeCompactJWS is decodeCompactJWSRaw for the main test goroutine, where failing the test
// outright on a malformed JWS is the right behavior.
func decodeCompactJWS(t *testing.T, compact string) (header, payload map[string]any) {
	t.Helper()
	header, payload, err := decodeCompactJWSRaw(compact)
	if err != nil {
		t.Fatal(err)
	}
	return header, payload
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// thumbprintOfJWKForTest recomputes RFC 7638 over a JWK decoded from a generic map (as a broker
// would receive it), so TestSignRequestObjectIssuerIsTheKeysThumbprint never trusts jws.go's own
// notion of "thumbprint" — it recomputes independently from the raw wire header.
func thumbprintOfJWKForTest(m map[string]any) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	var jwk jose.JSONWebKey
	if err := jwk.UnmarshalJSON(raw); err != nil {
		return "", err
	}
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

func TestSignRequestObjectHeaderAndClaims(t *testing.T) {
	key := mustTestKey(t)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	compact, err := signRequestObject(key, "https://secrets.test", "sami-agents.legion", "legion-daemon", "sjawhar", now)
	if err != nil {
		t.Fatal(err)
	}
	header, payload := decodeCompactJWS(t, compact)

	if header["alg"] != "ES256" {
		t.Fatalf("header alg = %v, want ES256", header["alg"])
	}
	if header["typ"] != "agent-secrets-request+jwt" {
		t.Fatalf("header typ = %v, want agent-secrets-request+jwt", header["typ"])
	}
	if _, ok := header["jwk"]; !ok {
		t.Fatalf("header has no embedded jwk: %+v", header)
	}

	// login_hint is non-empty in this call so it is present; reason is always empty for a
	// machine login and so, mirroring envoy's omitempty requestClaims, is entirely absent from
	// the wire — pinning that no extra or misnamed claim leaks onto a machine-login object.
	want := []string{"aud", "authorization_details", "exp", "iat", "iss", "jti", "login_hint"}
	if got := sortedKeys(payload); !reflect.DeepEqual(got, want) {
		t.Fatalf("payload claim names = %v, want %v", got, want)
	}

	iat, ok := payload["iat"].(float64)
	if !ok {
		t.Fatalf("iat = %v, not a number", payload["iat"])
	}
	exp, ok := payload["exp"].(float64)
	if !ok {
		t.Fatalf("exp = %v, not a number", payload["exp"])
	}
	if exp != iat+600 {
		t.Fatalf("exp = %v, iat = %v, want exp == iat+600", exp, iat)
	}
	if iat != float64(now.Unix()) {
		t.Fatalf("iat = %v, want %v", iat, now.Unix())
	}

	if payload["aud"] != "https://secrets.test" {
		t.Fatalf("aud = %v", payload["aud"])
	}
	if payload["login_hint"] != "sjawhar" {
		t.Fatalf("login_hint = %v", payload["login_hint"])
	}

	details, ok := payload["authorization_details"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("authorization_details = %+v, want exactly one entry", payload["authorization_details"])
	}
	detail, ok := details[0].(map[string]any)
	if !ok {
		t.Fatalf("authorization_details[0] = %+v, not an object", details[0])
	}
	wantDetailKeys := []string{"identifier", "service", "type"}
	if got := sortedKeys(detail); !reflect.DeepEqual(got, wantDetailKeys) {
		t.Fatalf("authorization_details[0] keys = %v, want %v", got, wantDetailKeys)
	}
	if detail["type"] != "launcher_credential" || detail["identifier"] != "sami-agents.legion" || detail["service"] != "legion-daemon" {
		t.Fatalf("detail = %+v", detail)
	}
}

// TestSignRequestObjectWithNoServiceOmitsIt pins that an empty service (an operator machine
// credential rather than a service one) never puts an empty string on the wire.
func TestSignRequestObjectWithNoServiceOmitsIt(t *testing.T) {
	key := mustTestKey(t)
	compact, err := signRequestObject(key, "https://secrets.test", "sami-agents.legion", "", "sjawhar", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, payload := decodeCompactJWS(t, compact)
	details := payload["authorization_details"].([]any)
	detail := details[0].(map[string]any)
	if _, present := detail["service"]; present {
		t.Fatalf("detail = %+v, want no service key when service is empty", detail)
	}
}

// TestSignRequestObjectIssuerIsTheKeysThumbprint pins that iss is exactly the embedded key's
// RFC 7638 thumbprint (the value a broker recomputes and compares), not an arbitrary identifier.
func TestSignRequestObjectIssuerIsTheKeysThumbprint(t *testing.T) {
	key := mustTestKey(t)
	compact, err := signRequestObject(key, "https://secrets.test", "host", "legion-daemon", "sjawhar", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	header, payload := decodeCompactJWS(t, compact)
	jwk, ok := header["jwk"].(map[string]any)
	if !ok {
		t.Fatalf("header jwk = %+v", header["jwk"])
	}
	tp, err := thumbprintOfJWKForTest(jwk)
	if err != nil {
		t.Fatal(err)
	}
	if payload["iss"] != tp {
		t.Fatalf("iss = %v, want the embedded key's thumbprint %v", payload["iss"], tp)
	}
}

// TestSignLauncherProofHeaderAndClaims pins the launcher proof's wire shape: typ
// "agent-secrets-proof+jwt", claim names exactly jti/iat/htm/htu/lid (eid never present — a
// launcher proof always authenticates directly by lid, never through an enrollment).
func TestSignLauncherProofHeaderAndClaims(t *testing.T) {
	key := mustTestKey(t)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	compact, err := signLauncherProof(key, "cred-123", "POST", "https://secrets.test/v1/enrollments", now)
	if err != nil {
		t.Fatal(err)
	}
	header, payload := decodeCompactJWS(t, compact)

	if header["alg"] != "ES256" {
		t.Fatalf("header alg = %v, want ES256", header["alg"])
	}
	if header["typ"] != "agent-secrets-proof+jwt" {
		t.Fatalf("header typ = %v, want agent-secrets-proof+jwt", header["typ"])
	}
	if _, ok := header["jwk"]; !ok {
		t.Fatalf("header has no embedded jwk: %+v", header)
	}

	want := []string{"htm", "htu", "iat", "jti", "lid"}
	if got := sortedKeys(payload); !reflect.DeepEqual(got, want) {
		t.Fatalf("payload claim names = %v, want %v (eid never present on a launcher proof)", got, want)
	}
	if payload["lid"] != "cred-123" {
		t.Fatalf("lid = %v", payload["lid"])
	}
	if payload["htm"] != "POST" {
		t.Fatalf("htm = %v", payload["htm"])
	}
	if payload["htu"] != "https://secrets.test/v1/enrollments" {
		t.Fatalf("htu = %v", payload["htu"])
	}
	if payload["jti"] == "" {
		t.Fatalf("jti is empty")
	}
	if iat, _ := payload["iat"].(float64); iat != float64(now.Unix()) {
		t.Fatalf("iat = %v, want %v", payload["iat"], now.Unix())
	}
}

func TestSignLauncherProofEachCallGetsAFreshJTI(t *testing.T) {
	key := mustTestKey(t)
	now := time.Now()
	a, err := signLauncherProof(key, "cred-1", "POST", "https://secrets.test/v1/enrollments", now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signLauncherProof(key, "cred-1", "POST", "https://secrets.test/v1/enrollments", now)
	if err != nil {
		t.Fatal(err)
	}
	_, pa := decodeCompactJWS(t, a)
	_, pb := decodeCompactJWS(t, b)
	if pa["jti"] == pb["jti"] {
		t.Fatalf("two proofs signed at the same instant share a jti: %v", pa["jti"])
	}
}
