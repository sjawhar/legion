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
)

// sealedRefreshTokenPrefix leads every refresh token people.refresh_token holds sealed: the
// format's version, so a value in any other form (a token stored in plain text before migration
// 0069, or a later format) is told apart instead of tried. Migration 0069 matches it in SQL.
const sealedRefreshTokenPrefix = "v1:"

// refreshTokenKeyInfo is the HKDF info the refresh-token key is derived with: its one purpose, so
// the key differs from anything else derived from the same signing key.
const refreshTokenKeyInfo = "dispatch people.refresh_token v1"

// refreshTokenSeal seals the sign-in pool's refresh tokens Dispatch keeps in people, with
// AES-256-GCM under a key derived from the session signing key (DISPATCH_SIGNING_KEY) with HKDF.
// The person's email is the additional data, so a value moved to another person's row does not
// open. A database dump alone therefore holds no usable refresh token, and a new signing key
// opens none sealed under the old one.
type refreshTokenSeal struct {
	aead cipher.AEAD
}

func newRefreshTokenSeal(signingKey string) refreshTokenSeal {
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
	return refreshTokenSeal{aead: aead}
}

// seal returns token sealed for email: the version prefix, then the random nonce, ciphertext and
// tag, base64url without padding.
func (s refreshTokenSeal) seal(email, token string) string {
	return sealedRefreshTokenPrefix + base64.RawURLEncoding.EncodeToString(s.aead.Seal(nil, nil, []byte(token), []byte(email)))
}

// errRefreshTokenFormat is a stored value without the sealed format's prefix.
var errRefreshTokenFormat = errors.New("the stored value is not in the sealed format " + sealedRefreshTokenPrefix)

// open returns the refresh token sealed holds for email. Its error never carries the value: a value
// in another format, one that does not decode, and one sealed under another key or for another
// person each fail with a fixed message.
func (s refreshTokenSeal) open(email, sealed string) (string, error) {
	encoded, ok := strings.CutPrefix(sealed, sealedRefreshTokenPrefix)
	if !ok {
		return "", errRefreshTokenFormat
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("the sealed value is not base64url")
	}
	token, err := s.aead.Open(nil, nil, ciphertext, []byte(email))
	if err != nil {
		return "", errors.New("the sealed value does not open under this signing key for this person")
	}
	return string(token), nil
}
