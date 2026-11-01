package verify

import (
	"crypto/hmac"
	"strings"
)

const ghostWisprSignaturePrefix = "sha256="

// GhostWispr verifies an HMAC SHA256 signature from a Ghost Wispr webhook.
// Uses the same sha256= prefix scheme as GitHub webhooks.
// If secret is empty, verification is skipped and returns true.
func GhostWispr(secret string, body []byte, signature string) bool {
	if secret == "" {
		return true
	}
	signature = strings.ToLower(strings.TrimSpace(signature))
	provided, ok := strings.CutPrefix(signature, ghostWisprSignaturePrefix)
	if !ok {
		return false
	}
	return hmac.Equal([]byte(sha256mac(secret, body)), []byte(provided))
}
