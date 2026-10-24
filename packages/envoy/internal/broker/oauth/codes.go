package oauth

import (
	"context"
	"crypto/rand"
	"strings"

	hoauth2 "authelia.com/provider/oauth2/handler/oauth2"
)

// userCodeAlphabet has 32 symbols, none easily confused with another (no 0/O, no 1/I), so a random
// byte maps onto it without bias.
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewUserCode is the code a person types to approve a machine login: eight random symbols from
// userCodeAlphabet as XXXX-XXXX. The device grant mints its user code with it.
func NewUserCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := make([]byte, 0, 9)
	for i, b := range raw {
		if i == 4 {
			code = append(code, '-')
		}
		code = append(code, userCodeAlphabet[int(b)%len(userCodeAlphabet)])
	}
	return string(code), nil
}

// NormalizeUserCode is the code as NewUserCode prints it, from the code as a person typed it:
// upper-cased, with spaces dropped and the hyphen in place. The library compares codes exactly,
// so every code is normalized before it is looked up.
func NormalizeUserCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(s))
	if len(s) != 8 {
		return s
	}
	return s[:4] + "-" + s[4:]
}

// codeStrategy is the library's HMAC strategy minting the broker's own user codes.
type codeStrategy struct {
	*hoauth2.HMACCoreStrategy
}

// GenerateRFC8628UserCode mints a NewUserCode and its signature, the HMAC the library stores it
// under.
func (s *codeStrategy) GenerateRFC8628UserCode(ctx context.Context) (string, string, error) {
	code, err := NewUserCode()
	if err != nil {
		return "", "", err
	}
	signature, err := s.RFC8628UserCodeSignature(ctx, code)
	if err != nil {
		return "", "", err
	}
	return code, signature, nil
}
