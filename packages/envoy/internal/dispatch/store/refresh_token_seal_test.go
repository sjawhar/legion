package store

import "testing"

// Each seal draws a fresh nonce: the same token sealed twice for one person stores two values. A
// repeated GCM nonce under one key reveals the XOR of the two tokens and the authentication key.
func TestRefreshTokenSealDrawsAFreshNonce(t *testing.T) {
	seal := newRefreshTokenSeal("signing-key")
	if a, b := seal.seal("alice@d.example", "refresh-v1"), seal.seal("alice@d.example", "refresh-v1"); a == b {
		t.Fatalf("sealing one token twice stored %q both times, want a fresh nonce each time", a)
	}
}

// A value a released Dispatch stored still opens: every stored row depends on the v1 format (the
// HKDF derivation, its info and salt, the cipher, the nonce layout and base64url without padding).
func TestRefreshTokenSealOpensAStoredV1Value(t *testing.T) {
	token, err := newRefreshTokenSeal("signing-key").open("alice@d.example", "v1:r0BoP-FEdLZtGu05-m8CAo8qzvy77FVWOv8itleQYmrMtd_r_rQ")
	if err != nil || token != "refresh-v1" {
		t.Fatalf("open = %q, %v; want refresh-v1", token, err)
	}
}
