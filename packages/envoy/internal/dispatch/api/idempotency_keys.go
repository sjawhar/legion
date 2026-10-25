package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A person's send names itself with an idempotency key, the same on every retry of it, so the
// server can tell a retry of a send from a second send: a broadcast (POST /api/v1/broadcasts,
// broadcasts.go) and a single-session send (POST /api/v1/agents/{session_id}/messages,
// agent_message_keys.go) each take one, and both hold it to the rule here. A key is stored beside
// what it made, keyed on the person's canonical login and the key, with a digest of the request it
// was first used for: a repeat of the request is answered what the key made, and a reuse of the key
// for another request is refused.

// maxIdempotencyKey bounds the caller's key. The dashboard sends a UUID (36); the cap keeps a
// runaway key out of the index. idempotencyKeyPattern keeps a key printable wherever it is echoed -
// a refusal, a log line, a psql session - and out of it whitespace and control characters; the
// pattern makes a key ASCII, so its length is its byte length.
const maxIdempotencyKey = 128

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// validateIdempotencyKey holds a key a send carries to that rule: one outside the pattern, the empty
// key among them, is refused under code, and a longer one is CAP_EXCEEDED. Whether a send may carry
// none is its route's own rule.
func validateIdempotencyKey(key, code string) error {
	if !idempotencyKeyPattern.MatchString(key) {
		return errorf(http.StatusBadRequest, code, "idempotency_key may use only letters, digits, '.', '_', ':' and '-'")
	}
	if len(key) > maxIdempotencyKey {
		return capExceededError("idempotency_key", len(key), maxIdempotencyKey)
	}
	return nil
}

// idempotencyKey is what a send's key is judged by: the person it belongs to, the key, and the
// digest of the request it was first used for.
type idempotencyKey struct{ login, key, digest string }

// newIdempotencyKey is the key actor's send carries: actor's canonical login, so one person's key is
// never another's and Alice is alice, the key, and a SHA-256 of request's JSON - what the send asked
// for, after validation - so a repeat of the key can be told from a reuse of it.
func newIdempotencyKey(actor model.Actor, key string, request any) idempotencyKey {
	encoded, err := json.Marshal(request)
	if err != nil {
		// Every caller's request is a struct of strings, string slices and string pointers, which
		// always encodes.
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return idempotencyKey{login: model.CanonicalLogin(actor.ID), key: key, digest: hex.EncodeToString(sum[:])}
}
