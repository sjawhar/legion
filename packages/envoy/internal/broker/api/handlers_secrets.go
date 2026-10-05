package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// settingsResponse is GET /v1/settings's answer.
type settingsResponse struct {
	// The Secrets Manager name prefix every agent secret sits under, ending in "/": the secret a
	// session asks for as DEEL_API_KEY is <prefix>deel-api-key.
	SecretsPrefix string `json:"secrets_prefix"`
	// The ARN of the agent-secrets KMS key, which every agent secret must be encrypted with.
	KMSKeyARN string `json:"kms_key_arn"`
	// The AWS account the agent-secrets key, and so every agent secret, is in.
	AWSAccountID string `json:"aws_account_id"`
	// The AWS region the agent-secrets key, and so every agent secret, is in.
	AWSRegion string `json:"aws_region"`
}

// readSettings is GET /v1/settings: public, since it answers only the broker's own configuration,
// which names no secret.
func (s *server) readSettings(w http.ResponseWriter, r *http.Request) {
	region, account := policy.KeyARNParts(s.deps.SecretsKMSKeyARN)
	writeJSON(w, http.StatusOK, settingsResponse{
		SecretsPrefix: s.deps.SecretsPrefix,
		KMSKeyARN:     s.deps.SecretsKMSKeyARN,
		AWSAccountID:  account,
		AWSRegion:     region,
	})
}

// rereadResponse is POST /v1/secrets/{name}/reread's answer.
type rereadResponse struct {
	// The name reread, as the path gave it.
	Name string `json:"name"`
	// True when the broker now serves the secret.
	Served bool `json:"served"`
	// Why the broker does not serve it, absent when it does: "absent" (no secret under the prefix
	// carries the name, or the one that does is scheduled for deletion) or the reason the broker
	// refused the secret, the one its policy-refused log line names (such as owner-tag-missing or
	// no-current-value).
	Reason string `json:"reason,omitempty"`
}

// rereadSecret is POST /v1/secrets/{name}/reread: public and rate-limited per source address and
// broker-wide (refuseReread) before any work, it reads the one secret from Secrets Manager now
// (policy.Current.RefreshOne) and answers whether the broker serves it. A reread whose request
// ended first - its caller went away, or its deadline passed - is the request's failure, not the
// broker's, so it logs nothing, as the miss path logs nothing for one: anyone can end a request,
// and the "broker: reread secret failed" line is the one an operator's alert counts.
func (s *server) rereadSecret(w http.ResponseWriter, r *http.Request) {
	if s.refuseReread(w, r) {
		return
	}
	name := r.PathValue("name")
	lk, err := s.deps.Policy.RefreshOne(r.Context(), name)
	switch {
	case errors.Is(err, policy.ErrNameInvalid):
		writeError(w, http.StatusBadRequest, "SECRET_NAME_INVALID", err.Error())
		return
	case err != nil && r.Context().Err() != nil:
		writeError(w, http.StatusServiceUnavailable, "REQUEST_ENDED", "the request ended before the reread finished; send it again")
		return
	case err != nil:
		writeInternal(w, "reread secret", err)
		return
	}
	writeJSON(w, http.StatusOK, rereadResponse{Name: name, Served: lk.Served, Reason: lk.Reason})
}
