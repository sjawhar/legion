package proof

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func fixture(t *testing.T) (*Verifier, string, string) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tp, err := Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	v := &Verifier{
		Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			if id == "enr-1" {
				return tp, true, nil
			}
			return "", false, nil
		},
		Replay: func(_ context.Context, jti string, _ time.Time) (bool, error) {
			if seen[jti] {
				return false, nil
			}
			seen[jti] = true
			return true, nil
		},
	}
	now := time.Now()
	compact, err := Sign(key, "enr-1", "POST", "https://secrets.test/v1/requests", now)
	if err != nil {
		t.Fatal(err)
	}
	return v, compact, tp
}

func TestValidProofPasses(t *testing.T) {
	v, compact, _ := fixture(t)
	id, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/requests", time.Now())
	if err != nil || id != "enr-1" {
		t.Fatalf("expected enr-1, got %q %v", id, err)
	}
}

func TestReplayedJtiFails(t *testing.T) {
	v, compact, _ := fixture(t)
	ctx := context.Background()
	if _, err := v.Verify(ctx, compact, "POST", "https://secrets.test/v1/requests", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, compact, "POST", "https://secrets.test/v1/requests", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("replay must fail with ErrInvalid, got %v", err)
	}
}

func TestWrongURLFails(t *testing.T) {
	v, compact, _ := fixture(t)
	if _, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/grants/x/values", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("htu mismatch must fail, got %v", err)
	}
}

func TestOtherKeyForSameEnrollmentFails(t *testing.T) {
	v, _, _ := fixture(t)
	other, _ := NewKey()
	compact, _ := Sign(other, "enr-1", "POST", "https://secrets.test/v1/requests", time.Now())
	if _, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/requests", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("thumbprint mismatch must fail, got %v", err)
	}
}

func TestSkewedProofFails(t *testing.T) {
	v, compact, _ := fixture(t)
	if _, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/requests", time.Now().Add(5*time.Minute)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("iat outside skew must fail, got %v", err)
	}
}

func TestUnsignedTypHeaderFails(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tp, err := Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{
		Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			if id == "enr-1" {
				return tp, true, nil
			}
			return "", false, nil
		},
		Replay: func(_ context.Context, _ string, _ time.Time) (bool, error) {
			return true, nil
		},
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{EmbedJWK: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	payload, err := json.Marshal(claims{JTI: "no-typ-jti", IssuedAt: now.Unix(), Method: "POST", URL: "https://secrets.test/v1/requests", EnrollmentID: "enr-1"})
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
	// The protected header this JWS was actually signed over carries no "typ" claim at all.
	// A JSON-serialized JWS lets an attacker attach one through the unsigned "header" field
	// instead; the typ check must only trust the protected (signed) header.
	jsonSerialized, err := json.Marshal(map[string]any{
		"payload":   parts[1],
		"protected": parts[0],
		"header":    map[string]string{"typ": typeHeader},
		"signature": parts[2],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), string(jsonSerialized), "POST", "https://secrets.test/v1/requests", now); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("typ present only in the unsigned header must be rejected with ErrInvalid, got %v", err)
	}
}

func TestForgedSignatureFails(t *testing.T) {
	enrolledKey, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tp, err := Thumbprint(&enrolledKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{
		Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			if id == "enr-1" {
				return tp, true, nil
			}
			return "", false, nil
		},
		Replay: func(_ context.Context, _ string, _ time.Time) (bool, error) {
			return true, nil
		},
	}

	attackerKey, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: attackerKey}, &jose.SignerOptions{
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: typeHeader},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	payload, err := json.Marshal(claims{JTI: "forged-jti", IssuedAt: now.Unix(), Method: "POST", URL: "https://secrets.test/v1/requests", EnrollmentID: "enr-1"})
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

	// Swap the embedded JWK in the protected header for the enrolled key's public key, so the
	// proof claims to be signed by the enrolled key while the bytes were actually signed by the
	// attacker's key. Re-verifying against the swapped protected header must fail: the signing
	// input no longer matches what the attacker actually signed.
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
	enrolledJWK, err := (jose.JSONWebKey{Key: &enrolledKey.PublicKey}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var jwkMap map[string]any
	if err := json.Unmarshal(enrolledJWK, &jwkMap); err != nil {
		t.Fatal(err)
	}
	protected["jwk"] = jwkMap
	forgedProtected, err := json.Marshal(protected)
	if err != nil {
		t.Fatal(err)
	}
	forged := base64.RawURLEncoding.EncodeToString(forgedProtected) + "." + parts[1] + "." + parts[2]

	_, err = v.Verify(context.Background(), forged, "POST", "https://secrets.test/v1/requests", now)
	if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("forged signature (embedded key not the actual signer) must be rejected with ErrInvalid mentioning signature, got %v", err)
	}
}

// TestJtiRetainedForTheWholeAcceptanceWindow pins that a proof's jti is remembered at least as
// long as the proof itself can pass the iat check: with a skew wider than any fixed retention, a
// replay inside the window must still find the jti recorded.
func TestJtiRetainedForTheWholeAcceptanceWindow(t *testing.T) {
	v, compact, _ := fixture(t)
	v.Skew = 20 * time.Minute
	var retainedUntil time.Time
	v.Replay = func(_ context.Context, _ string, expires time.Time) (bool, error) {
		retainedUntil = expires
		return true, nil
	}
	now := time.Now()
	if _, err := v.Verify(context.Background(), compact, "POST", "https://secrets.test/v1/requests", now); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if lastAccepted := now.Add(v.Skew); retainedUntil.Before(lastAccepted) {
		t.Fatalf("jti retained until %s, but a replay is accepted until %s", retainedUntil, lastAccepted)
	}
}
