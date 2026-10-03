// credential_requests.go relays Dispatch's credential-request UI to the secrets broker
// (the "UI routes" of the shared broker contract). Every handler does the same five things:
// require a human caller, require the broker to be configured, resolve or read its input, call
// the matching agentsecrets.Client method, and forward the broker's exact status and body — the
// broker decides. The pending list alone answers null rather than 404 FEATURE_OFF without a broker.
// The one thing Dispatch supplies is who decides: approve, deny and revoke send the
// login requireHuman resolved, in Dispatch's canonical lowercase form, as the approver, and never
// forward the browser's body, so nothing a browser sends can name the approver.
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
// (DISPATCH_AGENT_SECRETS_URL unset); every credential-request route but the pending list needs it
// after requireHuman.
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

// decisionFor builds the broker's decision body for an approve or deny: the approver is the
// caller's canonical login, and the one field read from the browser's body is a machine login's
// typed code. Any other field the browser sends, an approver among them, is ignored, never
// forwarded. An empty body is a decision with no code.
func decisionFor(w http.ResponseWriter, r *http.Request, actor model.Actor) (agentsecrets.Decision, bool) {
	body, ok := readRelayBody(w, r)
	if !ok {
		return agentsecrets.Decision{}, false
	}
	var input struct {
		Code *string `json:"code"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &input); err != nil {
			writeError(w, "INVALID_DECISION", http.StatusBadRequest, "request body must be a JSON object")
			return agentsecrets.Decision{}, false
		}
	}
	return agentsecrets.Decision{Approver: canonicalLogin(actor.ID), Code: input.Code}, true
}

// --- GET /api/v1/credential-requests, GET .../{id} ---

// listCredentialPending answers the viewer's pending list, or null when this Dispatch has no broker.
// Every page reads this list (the Needs-you badge counts it), and a deployment without a broker is
// an ordinary one, so "no broker" is an answer here rather than the 404 FEATURE_OFF the other
// credential routes give: a 404 made every page of such a deployment log a failed request.
func (s *server) listCredentialPending(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	login, ok := resolveApproverMe(w, r, actor)
	if !ok {
		return
	}
	if s.deps.AgentSecrets == nil {
		WriteJSON(w, http.StatusOK, nil)
		return
	}
	body, err := s.deps.AgentSecrets.Pending(r.Context(), login)
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
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	decision, ok := decisionFor(w, r, actor)
	if !ok {
		return
	}
	body, err := client.Approve(r.Context(), r.PathValue("id"), decision)
	relayBrokerResponse(w, body, err)
}

func (s *server) denyCredentialRecord(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	decision, ok := decisionFor(w, r, actor)
	if !ok {
		return
	}
	body, err := client.Deny(r.Context(), r.PathValue("id"), decision)
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

// revokeCredentialGrant ends a grant as the caller: the broker allows it only when the caller's
// login is the grant's approver or its enrollment's operator. The browser's body carries nothing
// Dispatch reads.
func (s *server) revokeCredentialGrant(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.RevokeByApprover(r.Context(), r.PathValue("id"), canonicalLogin(actor.ID))
	relayBrokerResponse(w, body, err)
}
