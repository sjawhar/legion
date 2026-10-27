package store

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// testSigningKey is the one-key set the package's people stores seal under.
var testSigningKey = auth.SingleSigningKey("signing-key")

// signingKeys is auth.ParseSigningKeys for a setting the test knows is valid.
func signingKeys(t *testing.T, setting string) auth.SigningKeys {
	t.Helper()
	keys, err := auth.ParseSigningKeys(setting)
	if err != nil {
		t.Fatalf("ParseSigningKeys(%q): %v", setting, err)
	}
	return keys
}

// Each seal draws a fresh nonce: the same token sealed twice for one person stores two values. A
// repeated GCM nonce under one key reveals the XOR of the two tokens and the authentication key.
func TestRefreshTokenSealDrawsAFreshNonce(t *testing.T) {
	seal := newRefreshTokenSeal(testSigningKey)
	if a, b := seal.seal("alice@d.example", "refresh-v1"), seal.seal("alice@d.example", "refresh-v1"); a == b {
		t.Fatalf("sealing one token twice stored %q both times, want a fresh nonce each time", a)
	}
}

// A value a released Dispatch stored still opens: every stored row depends on the v1 format (the
// HKDF derivation, its info and salt, the cipher, the nonce layout and base64url without padding).
// It opens under its key alone and under a set that holds its key second, as after a rotation.
func TestRefreshTokenSealOpensAStoredV1Value(t *testing.T) {
	for _, keys := range []auth.SigningKeys{testSigningKey, signingKeys(t, "rotated-signing-key signing-key")} {
		token, err := newRefreshTokenSeal(keys).open("alice@d.example", "v1:r0BoP-FEdLZtGu05-m8CAo8qzvy77FVWOv8itleQYmrMtd_r_rQ")
		if err != nil || token != "refresh-v1" {
			t.Fatalf("open under %d keys = %q, %v; want refresh-v1", len(keys.Keys()), token, err)
		}
	}
}

// During a rotation the processes hold "K1 K0" and "K0 K1": a seal either one makes opens under
// both, and under the set before the rotation began ("K0") only when K0 sealed it.
func TestRefreshTokenSealOpensASealMadeUnderEitherKey(t *testing.T) {
	for _, sealer := range []string{"K0", "K1 K0", "K0 K1"} {
		sealed := newRefreshTokenSeal(signingKeys(t, sealer)).seal("alice@d.example", "refresh-"+sealer)
		for _, opener := range []string{"K1 K0", "K0 K1"} {
			token, err := newRefreshTokenSeal(signingKeys(t, opener)).open("alice@d.example", sealed)
			if err != nil || token != "refresh-"+sealer {
				t.Errorf("a seal made under %q, opened under %q = %q, %v; want refresh-%s", sealer, opener, token, err, sealer)
			}
		}
	}
	if _, err := newRefreshTokenSeal(signingKeys(t, "K0")).open("alice@d.example", newRefreshTokenSeal(signingKeys(t, "K1 K0")).seal("alice@d.example", "refresh")); err == nil {
		t.Errorf("a seal made under \"K1 K0\" opened under K0 alone, want it sealed under K1")
	}
}

// A seal under a key the set does not hold, or for another person, opens under no key.
func TestRefreshTokenSealRefusesAValueSealedUnderNoKeyOfTheSet(t *testing.T) {
	sealed := newRefreshTokenSeal(signingKeys(t, "K2")).seal("alice@d.example", "refresh")
	if token, err := newRefreshTokenSeal(signingKeys(t, "K1 K0")).open("alice@d.example", sealed); err == nil {
		t.Errorf("a seal made under K2 opened under \"K1 K0\" as %q", token)
	}
	ownSeal := newRefreshTokenSeal(signingKeys(t, "K1 K0")).seal("alice@d.example", "refresh")
	if token, err := newRefreshTokenSeal(signingKeys(t, "K1 K0")).open("bob@d.example", ownSeal); err == nil {
		t.Errorf("alice's seal opened for bob as %q", token)
	}
}
