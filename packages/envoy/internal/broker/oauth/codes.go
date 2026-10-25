package oauth

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"

	hoauth2 "authelia.com/provider/oauth2/handler/oauth2"
	"authelia.com/provider/oauth2/token/jose"
	"authelia.com/provider/oauth2/token/jwt"
)

// userCodeAlphabet has 32 symbols, none easily confused with another (no 0/O, no 1/I), so a random
// byte maps onto it without bias.
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewUserCode is the code a person types to approve a machine login: eight random symbols from
// userCodeAlphabet as XXXX-XXXX. Both the device grant and the older pending-id login mint theirs
// with it.
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

// codeStrategy is the library's HMAC strategy minting the broker's own user codes, and the whole
// of jwt.Strategy as refusals.
//
// The RFC 8693 custom JWT type factory asserts the composed strategy to jwt.Strategy, and the
// handler it builds uses that strategy only to issue a JSON Web Token: it validates a subject
// token through the token type's own key function instead. This provider issues no JSON Web
// Token — its exchange client may ask for an access token and nothing else — so the method set
// is here to satisfy the assertion and to refuse. No signing key is generated anywhere in this
// provider, so nothing can silently mint a signed token whatever calls these.
type codeStrategy struct {
	*hoauth2.HMACCoreStrategy
}

// errNoJWT is what every jwt.Strategy method of codeStrategy answers: a caller reaching one is
// asking this provider to sign or read a JSON Web Token of its own, which it does not do.
var errNoJWT = errors.New("oauth: the broker's OAuth provider issues no JSON Web Tokens")

// Encode refuses: nothing here mints a signed token.
func (s *codeStrategy) Encode(context.Context, jwt.Claims, ...jwt.StrategyOpt) (string, string, error) {
	return "", "", errNoJWT
}

// Decrypt refuses: nothing here holds a key a JSON Web Token could be encrypted to.
func (s *codeStrategy) Decrypt(context.Context, string, ...jwt.StrategyOpt) (string, string, *jose.JSONWebEncryption, error) {
	return "", "", nil, errNoJWT
}

// Decode refuses: a subject token is read through its token type's own key function, against the
// issuer's published keys, and never through this.
func (s *codeStrategy) Decode(context.Context, string, ...jwt.StrategyOpt) (*jwt.Token, error) {
	return nil, errNoJWT
}

// Validate refuses, for the same reason Decode does.
func (s *codeStrategy) Validate(context.Context, *jwt.Token, ...jwt.StrategyOpt) error {
	return errNoJWT
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
