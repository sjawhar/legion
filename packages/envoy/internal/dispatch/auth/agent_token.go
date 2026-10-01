package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerToken reads a request's bearer the one way Dispatch reads it: the Authorization header
// trimmed, then the token after "Bearer ". present is false when the header is absent or blank,
// so the caller authenticates the request another way. A header that is present but is not a
// bearer gives present true and an empty token, which no credential matches. The HTTP API and
// the document websocket both read their bearer through it.
func BearerToken(r *http.Request) (token string, present bool) {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization == "" {
		return "", false
	}
	token, isBearer := strings.CutPrefix(authorization, "Bearer ")
	if !isBearer {
		return "", true
	}
	return token, true
}

// MatchesSharedAgentToken reports whether a bearer token is the deployment's shared agent token
// (DISPATCH_AGENT_TOKEN). The comparison takes time that depends on the two lengths and not on
// their bytes, so a caller cannot time how much of a guess matched. An empty configured token
// matches nothing, the empty bearer included. The HTTP API and the document websocket both
// authenticate the shared token through it.
func MatchesSharedAgentToken(token, configured string) bool {
	return configured != "" && subtle.ConstantTimeCompare([]byte(token), []byte(configured)) == 1
}
