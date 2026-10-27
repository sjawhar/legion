package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// sealedRefreshTokenPrefix leads every refresh token people.refresh_token holds sealed: the
// format's version, so a value in any other form (a token stored in plain text by the release before
// sealing, or a later format) is told apart instead of tried. RetirePlainRefreshTokens matches it in
// SQL.
const sealedRefreshTokenPrefix = "v1:"

// refreshTokenKeyInfo is the HKDF info the refresh-token key is derived with: its one purpose, so
// the key differs from anything else derived from the same signing key.
const refreshTokenKeyInfo = "dispatch people.refresh_token v1"

// refreshTokenSeal seals the sign-in pool's refresh tokens Dispatch keeps in people, with
// AES-256-GCM under a key derived from a session signing key (DISPATCH_SIGNING_KEY) with HKDF.
// The person's email is the additional data, so a value moved to another person's row does not
// open. A database dump alone therefore holds no usable refresh token, and a signing key set
// opens none sealed under a key it does not hold.
//
// It seals under the current key and opens under any key of the set, trying each in turn: the
// GCM tag refuses a key the value was not sealed under. The value names no key, so the v1 format
// stays byte for byte what an earlier build writes and reads: a new prefix would read as plain
// text to an earlier build booting during a roll or a rollback, whose RetirePlainRefreshTokens
// would revoke every such token at the issuer.
type refreshTokenSeal struct {
	// aeads holds one AEAD per signing key, the current key's first.
	aeads []cipher.AEAD
}

func newRefreshTokenSeal(keys auth.SigningKeys) refreshTokenSeal {
	if keys.Empty() {
		panic("dispatch: the refresh-token seal needs a signing key")
	}
	var seal refreshTokenSeal
	for _, signingKey := range keys.Keys() {
		key, err := hkdf.Key(sha256.New, []byte(signingKey), nil, refreshTokenKeyInfo, 32)
		if err != nil {
			panic(fmt.Sprintf("dispatch: derive the refresh-token key from the signing key: %v", err))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			panic(fmt.Sprintf("dispatch: refresh-token cipher: %v", err))
		}
		aead, err := cipher.NewGCMWithRandomNonce(block)
		if err != nil {
			panic(fmt.Sprintf("dispatch: refresh-token AEAD: %v", err))
		}
		seal.aeads = append(seal.aeads, aead)
	}
	return seal
}

// seal returns token sealed for email under the current signing key: the version prefix, then the
// random nonce, ciphertext and tag, base64url without padding.
func (s refreshTokenSeal) seal(email, token string) string {
	return sealedRefreshTokenPrefix + base64.RawURLEncoding.EncodeToString(s.aeads[0].Seal(nil, nil, []byte(token), []byte(email)))
}

// open returns the refresh token sealed holds for email, sealed under any of the signing keys. Its
// error never carries the value: a value in another format, one that does not decode, and one
// sealed under no key of the set or for another person each fail with a fixed message.
func (s refreshTokenSeal) open(email, sealed string) (string, error) {
	encoded, ok := strings.CutPrefix(sealed, sealedRefreshTokenPrefix)
	if !ok {
		return "", errors.New("the stored value is not in the sealed format " + sealedRefreshTokenPrefix)
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("the sealed value is not base64url")
	}
	for _, aead := range s.aeads {
		if token, err := aead.Open(nil, nil, ciphertext, []byte(email)); err == nil {
			return string(token), nil
		}
	}
	return "", errors.New("the sealed value does not open under any signing key for this person")
}
