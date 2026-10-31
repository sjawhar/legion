package routes

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
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
	// appHandoffFormat and appHandoffPurpose are the app handoff's own (signAppHandoff), so a
	// handoff never verifies as a state cookie or a dsession cookie, nor either of them as a handoff.
	appHandoffFormat  = "h1"
	appHandoffPurpose = "dispatch app handoff v1"
	// appHandoffTTL is how long the app has to redeem a handoff at /auth/app: the hop from the
	// browser to the app, never a person's wait.
	appHandoffTTL = 2 * time.Minute
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
	// App is the Android app's code challenge (appChallenge) when the app started the sign-in in
	// the browser, empty for a sign-in the dashboard started: the callback then hands the session
	// to the app (appHandoff) instead of setting a cookie in the browser.
	App string `json:"app,omitempty"`
}

// appHandoff is a sign-in the callback completed for the Android app, carried to it through the
// browser's redirect and redeemed at /auth/app for the session cookie in the app's own WebView,
// whose cookie store the browser does not share. Only the holder of the verifier whose challenge
// it names can redeem it, so another app that receives the redirect gets nothing it can use.
type appHandoff struct {
	Email     string `json:"email"`
	Subject   string `json:"subject,omitempty"`
	Challenge string `json:"challenge"`
	Next      string `json:"next"`
	Exp       int64  `json:"exp"`
}

// appChallenge is raw when it is a code challenge the app can have made, the unpadded base64url
// SHA-256 of its verifier (RFC 7636's S256), and "" otherwise.
func appChallenge(raw string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return ""
	}
	return raw
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
	return signValue(keys, oauthStateFormat, oauthStatePurpose, state)
}

// verifyOAuthState returns the sign-in a state cookie's value carries when its MAC verifies under
// any key of keys for oauthStatePurpose and it has not lapsed at now.
func verifyOAuthState(keys auth.SigningKeys, value string, now time.Time) (oauthState, bool) {
	var state oauthState
	if !openValue(keys, oauthStateFormat, oauthStatePurpose, value, &state) || state.State == "" || state.Nonce == "" || now.Unix() >= state.Exp {
		return oauthState{}, false
	}
	return state, true
}

// signAppHandoff returns the handoff's value, signed as a state cookie is but for appHandoffPurpose.
func signAppHandoff(keys auth.SigningKeys, handoff appHandoff) (string, error) {
	return signValue(keys, appHandoffFormat, appHandoffPurpose, handoff)
}

// verifyAppHandoff returns the sign-in a handoff carries when its MAC verifies under any key of
// keys for appHandoffPurpose, it has not lapsed at now, and verifier is the one its challenge was
// made from.
func verifyAppHandoff(keys auth.SigningKeys, value, verifier string, now time.Time) (appHandoff, bool) {
	var handoff appHandoff
	if !openValue(keys, appHandoffFormat, appHandoffPurpose, value, &handoff) || handoff.Email == "" || now.Unix() >= handoff.Exp {
		return appHandoff{}, false
	}
	sum := sha256.Sum256([]byte(verifier))
	if verifier == "" || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(handoff.Challenge)) != 1 {
		return appHandoff{}, false
	}
	return handoff, true
}

// signValue returns format, the base64url JSON of v, and its MAC under keys' current key for
// purpose, joined by dots.
func signValue(keys auth.SigningKeys, format, purpose string, v any) (string, error) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	// HTML escaping would write "<", ">" and "&" as six bytes each and break the size bound.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return "", err
	}
	payload := format + "." + base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(body.Bytes(), []byte("\n")))
	return payload + "." + keys.SignFor(purpose, payload), nil
}

// openValue decodes into v the JSON a value signValue made carries, when it has format and its
// MAC verifies under any key of keys for purpose.
func openValue(keys auth.SigningKeys, format, purpose, value string, v any) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != format {
		return false
	}
	if !keys.VerifyFor(purpose, parts[0]+"."+parts[1], parts[2]) {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	return err == nil && json.Unmarshal(body, v) == nil
}
