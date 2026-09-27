// credential_requests.go relays Dispatch's credential-request UI to the secrets broker
// (contract v9's "UI routes"). Every handler here does the same five things: require a human
// caller, require the broker to be configured, resolve or read its input, call the matching
// agentsecrets.Client method, and forward the broker's exact status and body — Dispatch relays,
// it never decides (design v4, "The approval signal is a WebAuthn assertion... Dispatch renders
// and relays; it never decides").
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// requireAgentSecrets answers 404 FEATURE_OFF when this Dispatch has no broker configured
// (DISPATCH_AGENT_SECRETS_URL unset); every credential-request route needs it after requireHuman.
func (s *server) requireAgentSecrets(w http.ResponseWriter) (*agentsecrets.Client, bool) {
	if s.deps.AgentSecrets == nil {
		writeError(w, "FEATURE_OFF", http.StatusNotFound, "agent-secrets is not configured on this Dispatch")
		return nil, false
	}
	return s.deps.AgentSecrets, true
}

// relayBrokerResponse writes the broker's answer verbatim: its raw JSON body on success, its
// exact status and body for a *agentsecrets.Error (the browser must see the broker's own status
// and code, e.g. 409 RECORD_TERMINAL — Dispatch never re-wraps it), or 503
// AGENT_SECRETS_UNAVAILABLE for anything else (a transport failure).
func relayBrokerResponse(w http.ResponseWriter, body json.RawMessage, err error) {
	if err != nil {
		var brokerErr *agentsecrets.Error
		if errors.As(err, &brokerErr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(brokerErr.Status)
			_, _ = w.Write(brokerErr.Body)
			return
		}
		writeError(w, "AGENT_SECRETS_UNAVAILABLE", http.StatusServiceUnavailable, "the secrets broker is unreachable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// resolveApproverMe reads ?approver=, which the UI only ever sends as the literal string "me" —
// it asks for the viewer's own list, never a login it names itself — and resolves it to the
// caller's canonical login. Anything else is refused before it ever reaches the broker.
func resolveApproverMe(w http.ResponseWriter, r *http.Request, actor model.Actor) (string, bool) {
	if approver := r.URL.Query().Get("approver"); approver != "me" {
		writeError(w, "APPROVER_ME_ONLY", http.StatusBadRequest, `approver must be the literal value "me"`)
		return "", false
	}
	return canonicalLogin(actor.ID), true
}

// readRelayBody reads a mutation's body verbatim, capped like every other JSON mutation, and
// hands it to the broker unparsed: Dispatch relays, it does not model these shapes.
func readRelayBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONRequestBytes))
	if err != nil {
		writeError(w, "REQUEST_TOO_LARGE", http.StatusRequestEntityTooLarge, "request body exceeds the size limit")
		return nil, false
	}
	return json.RawMessage(body), true
}

// --- GET /api/v1/credential-requests, GET .../{id} ---

func (s *server) listCredentialPending(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	login, ok := resolveApproverMe(w, r, actor)
	if !ok {
		return
	}
	body, err := client.Pending(r.Context(), login)
	relayBrokerResponse(w, body, err)
}

func (s *server) getCredentialRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.Record(r.Context(), r.PathValue("id"))
	relayBrokerResponse(w, body, err)
}

// --- POST .../{id}/approve, .../{id}/deny, .../machine-lookup ---

func (s *server) approveCredentialRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	relayBody, ok := readRelayBody(w, r)
	if !ok {
		return
	}
	body, err := client.Approve(r.Context(), r.PathValue("id"), relayBody)
	relayBrokerResponse(w, body, err)
}

func (s *server) denyCredentialRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	relayBody, ok := readRelayBody(w, r)
	if !ok {
		return
	}
	body, err := client.Deny(r.Context(), r.PathValue("id"), relayBody)
	relayBrokerResponse(w, body, err)
}

func (s *server) lookupMachineCredential(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	relayBody, ok := readRelayBody(w, r)
	if !ok {
		return
	}
	body, err := client.MachineLookup(r.Context(), relayBody)
	relayBrokerResponse(w, body, err)
}

// --- GET /api/v1/credential-keys/{login}, POST .../{kind}/{step} ---

func (s *server) getCredentialKeys(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.Keys(r.Context(), r.PathValue("login"))
	relayBrokerResponse(w, body, err)
}

// credentialKeyCeremony drives one step of a key registration or endorsement ceremony. kind and
// step are path segments, not a wildcard the client method validates itself (agentsecrets.Client.
// Ceremony trusts its caller) — anything outside the four valid combinations is a plain 404, the
// same shape routes/router.go uses for an unmatched path, minus its top-level "hint" field.
func (s *server) credentialKeyCeremony(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	kind, step := r.PathValue("kind"), r.PathValue("step")
	if (kind != "register" && kind != "endorse") || (step != "begin" && step != "finish") {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "no route for "+r.Method+" "+r.URL.Path)
		return
	}
	relayBody, ok := readRelayBody(w, r)
	if !ok {
		return
	}
	body, err := client.Ceremony(r.Context(), r.PathValue("login"), kind, step, relayBody)
	relayBrokerResponse(w, body, err)
}

// --- GET /api/v1/credential-grants, POST .../{id}/revoke ---

func (s *server) listCredentialGrants(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	login, ok := resolveApproverMe(w, r, actor)
	if !ok {
		return
	}
	body, err := client.Grants(r.Context(), login)
	relayBrokerResponse(w, body, err)
}

func (s *server) revokeCredentialGrant(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	relayBody, ok := readRelayBody(w, r)
	if !ok {
		return
	}
	body, err := client.RevokeByApprover(r.Context(), r.PathValue("id"), relayBody)
	relayBrokerResponse(w, body, err)
}
