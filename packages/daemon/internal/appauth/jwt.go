package appauth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// GenerateJWT creates the RS256 App JWT GitHub accepts: iat a minute back for clock skew, and exp
// nine minutes on. GitHub refuses an exp more than ten minutes ahead of its own clock, so an exp of
// exactly ten minutes fails on a host whose clock is ahead of GitHub's by any amount, even a second
// ("'Expiration time' claim ('exp') is too far in the future"); the minute of margin absorbs that.
func GenerateJWT(appID, privateKeyPEM string, now time.Time) (string, error) {
	key, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}{Algorithm: "RS256", Type: "JWT"})
	if err != nil {
		return "", fmt.Errorf("encode App JWT header: %w", err)
	}
	claims, err := json.Marshal(struct {
		Issuer  string `json:"iss"`
		Issued  int64  `json:"iat"`
		Expires int64  `json:"exp"`
	}{
		Issuer:  appID,
		Issued:  now.Add(-time.Minute).Unix(),
		Expires: now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("encode App JWT claims: %w", err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign App JWT: %w", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parsePrivateKey(privateKeyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("parse App private key: PEM block is missing")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse App private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("parse App private key: key is not RSA")
	}
	return key, nil
}
