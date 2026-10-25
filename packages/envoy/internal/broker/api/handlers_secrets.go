package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// settingsResponse is GET /v1/settings's answer: the store the agent secrets live in and, per
// store, where.
type settingsResponse struct {
	// Which secret store holds the agent secrets: aws-secrets-manager or gcp-secret-manager. A
	// broker from before this field answered none, and held them in AWS Secrets Manager.
	Store string `json:"store"`
	// The name prefix every agent secret sits under: with aws-secrets-manager a Secrets Manager
	// name prefix ending in "/", with gcp-secret-manager projects/<project number>/secrets/<secret-id
	// prefix>-. The secret a session asks for as DEEL_API_KEY is <prefix>deel-api-key.
	SecretsPrefix string `json:"secrets_prefix"`
	// With aws-secrets-manager, the ARN of the agent-secrets KMS key, which every agent secret
	// must be encrypted with; absent otherwise.
	KMSKeyARN string `json:"kms_key_arn,omitempty"`
	// With aws-secrets-manager, the AWS account the agent-secrets key, and so every agent secret,
	// is in; absent otherwise.
	AWSAccountID string `json:"aws_account_id,omitempty"`
	// With aws-secrets-manager, the AWS region the agent-secrets key, and so every agent secret,
	// is in; absent otherwise.
	AWSRegion string `json:"aws_region,omitempty"`
	// With gcp-secret-manager, the resource name of the agent-secrets Cloud KMS key, which every
	// agent secret must be encrypted with; absent otherwise.
	KMSKeyName string `json:"kms_key_name,omitempty"`
	// With gcp-secret-manager, the number of the project every agent secret is in, the one the
	// prefix names; absent otherwise.
	GCPProject string `json:"gcp_project,omitempty"`
}

// readSettings is GET /v1/settings: public, since it answers only the broker's own configuration,
// which names no secret.
func (s *server) readSettings(w http.ResponseWriter, r *http.Request) {
	resp := settingsResponse{Store: s.deps.SecretsStore, SecretsPrefix: s.deps.SecretsPrefix}
	switch s.deps.SecretsStore {
	case secrets.StoreGCP:
		resp.KMSKeyName = s.deps.SecretsKMSKey
		resp.GCPProject = secrets.GCPProject(s.deps.SecretsPrefix)
	default:
		resp.KMSKeyARN = s.deps.SecretsKMSKey
		resp.AWSRegion, resp.AWSAccountID = secrets.AWSKeyARNParts(s.deps.SecretsKMSKey)
	}
	writeJSON(w, http.StatusOK, resp)
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
	// The kind the broker serves the secret as, while it serves it: github-app or google-login, or
	// absent for a stored secret, whose grants release its stored value. A person who retags a
	// secret to a kind reads it here before storing the key the kind mints from: a stale store read
	// can still serve the secret as stored.
	Kind string `json:"kind,omitempty"`
}

// rereadSecret is POST /v1/secrets/{name}/reread: public and rate-limited per source address and
// broker-wide (refuseReread) before any work, it reads the one secret from Secrets Manager now
// (policy.Current.RefreshOne) and answers whether the broker serves it. A reread whose request
// ended first - its caller went away, or its deadline passed - is the request's failure, not the
// broker's, so it logs nothing, as the miss path logs nothing for one: anyone can end a request,
// and the "broker: reread secret failed" line is the one an operator's alert counts.
// Having reread it here, the handler tells every other pod serving the same database to reread it
// too (Deps.NotifyReread), so one call reaches the whole fleet rather than the pod the load
// balancer happened to pick.
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
	// Tell the other pods, on the 200 path alone: a reread that never happened - a name no secret
	// can carry, a caller that went away, a store that failed, or the 429 refuseReread wrote
	// before any work - is nothing for a peer to copy.
	//
	// It notifies on every 200, whether or not the broker now serves the name. Notifying only
	// when the live answer changed would need RefreshOne to report a change it does not report,
	// and would still leave a hole: a pod whose own five-minute reload had already caught the
	// retag would answer "unchanged" and tell nobody, leaving its peers stale for up to a reload.
	// Notifying always is also the arithmetic DefaultRereadOverallLimit is halved by, and a
	// reread a notification drives takes no token from any of this package's buckets - the pod
	// that served the original call already paid for it - so the fleet's whole metadata traffic
	// is the sending pod's bucket times the replica count, whichever pod was asked.
	//
	// The fan-out deliberately drops r's cancellation, which fires as soon as the answer is
	// written and which a caller that hangs up mid-reread fires earlier still: the local reread
	// has already succeeded and its answer stands, so that caller must not be able to stop the
	// other pods from learning of it. It keeps every value on the request's context, as this
	// repository's other detached work does (context.WithoutCancel): only the cancellation is in
	// the way. The handler answers 200 whatever the fan-out does, since nothing about it changes
	// what this pod now serves.
	//
	// Only this public route notifies; the call is deliberately not inside
	// policy.Current.RefreshOne. RefreshOne's other caller is the miss path in
	// requests/machine_state.go, which is bounded per enrollment and per process and already
	// heals a new secret on first use; fanning that out would spend every peer a store read for
	// a name that peer may never be asked for.
	if s.deps.NotifyReread != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), notifyRereadTimeout)
		defer cancel()
		s.deps.NotifyReread(ctx, name)
	}
	resp := rereadResponse{Name: name, Served: lk.Served, Reason: lk.Reason}
	if lk.Served {
		resp.Kind = lk.Secret.Kind()
	}
	writeJSON(w, http.StatusOK, resp)
}

// notifyRereadTimeout bounds the fan-out one reread sends: a single Postgres NOTIFY on the shared
// pool, which either goes out at once or is waiting for a connection the requests behind it need
// back. It is short because the pod's own reread has already succeeded by the time it runs, and
// every peer's five-minute reload is the backstop for a notification that never goes out.
const notifyRereadTimeout = 5 * time.Second
