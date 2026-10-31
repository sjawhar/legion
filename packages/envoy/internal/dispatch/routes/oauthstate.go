package routes

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

const (
	oauthStateCookie = "dispatch_oauth_state"
	oauthStateMaxAge = 10 * 60
	// oauthStateFormat leads every state cookie value, so a later shape can be told from this one.
	oauthStateFormat = "s1"
	// oauthStatePurpose is the purpose the state cookie is signed for (auth.SigningKeys.SignFor): a
	// key derived for it alone, so a state cookie never verifies as a dsession cookie or the reverse.
	oauthStatePurpose = "dispatch oauth state v1"
	// maxNextBytes caps the page a sign-in returns to; a longer one returns to "/". JSON escapes a
	// byte of a sanitised next into at most two, so the cookie stays far below a browser's 4 KiB.
	maxNextBytes = 1024
)

// oauthState is a sign-in in flight, carried by the browser that started it in the
// dispatch_oauth_state cookie rather than held by the process that started it, so whichever
// process the issuer's redirect reaches completes the sign-in (authCallback).
type oauthState struct {
	// State is the value the issuer returns in the callback's URL.
	State string `json:"state"`
	// Nonce is the value the issuer's ID token must carry (oidc.CodeFlow.Exchange).
	Nonce string `json:"nonce"`
	// Next is the local page the sign-in returns to (sanitizeNext).
	Next string `json:"next"`
	// Exp is when the sign-in lapses, in Unix seconds.
	Exp int64 `json:"exp"`
}

// signInNext is the page a sign-in started with the raw next returns to: sanitizeNext's answer,
// or "/" for a next over maxNextBytes or one that is not UTF-8, which the cookie's JSON would
// carry as a different page.
func signInNext(raw string) string {
	if len(raw) > maxNextBytes || !utf8.ValidString(raw) {
		return "/"
	}
	return sanitizeNext(raw)
}

// signOAuthState returns the state cookie's value for state: the format, the base64url JSON of
// state, and its MAC under keys' current key for oauthStatePurpose, joined by dots.
func signOAuthState(keys auth.SigningKeys, state oauthState) (string, error) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	// HTML escaping would write "<", ">" and "&" as six bytes each and break the size bound.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(state); err != nil {
		return "", err
	}
	payload := oauthStateFormat + "." + base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(body.Bytes(), []byte("\n")))
	return payload + "." + keys.SignFor(oauthStatePurpose, payload), nil
}

// verifyOAuthState returns the sign-in a state cookie's value carries when its MAC verifies under
// any key of keys for oauthStatePurpose and it has not lapsed at now.
func verifyOAuthState(keys auth.SigningKeys, value string, now time.Time) (oauthState, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != oauthStateFormat {
		return oauthState{}, false
	}
	if !keys.VerifyFor(oauthStatePurpose, parts[0]+"."+parts[1], parts[2]) {
		return oauthState{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return oauthState{}, false
	}
	var state oauthState
	if err := json.Unmarshal(body, &state); err != nil || state.State == "" || state.Nonce == "" || now.Unix() >= state.Exp {
		return oauthState{}, false
	}
	return state, true
}
