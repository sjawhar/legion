package auth

import "crypto/subtle"

// MatchesSharedAgentToken reports whether a bearer token is the deployment's shared agent token
// (DISPATCH_AGENT_TOKEN). The comparison takes time that depends on the two lengths and not on
// their bytes, so a caller cannot time how much of a guess matched. An empty configured token
// matches nothing, the empty bearer included. The HTTP API and the document websocket both
// authenticate the shared token through it.
func MatchesSharedAgentToken(token, configured string) bool {
	return configured != "" && subtle.ConstantTimeCompare([]byte(token), []byte(configured)) == 1
}
