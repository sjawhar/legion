// packages/envoy/cmd/agent-secrets/proofsource.go
//
// Signer produces the compact JWS this process puts in a broker call's Proof header, and the
// signed credential-request object POST /v1/requests embeds (contract v9: authorization_details
// and reason live inside that signed object, not plain top-level fields). An agent box or pod
// has its own key on tmpfs under AGENT_SECRETS_KEY_DIR and signs both directly for its own
// enrollment id (fileSigner); a host session has no key of its own and asks
// agent-secrets-helper, over AGENT_SECRETS_HELPER_SOCK, which signs only for processes that
// descend from a registered session root (helperSigner) — the session's key never leaves the
// helper process, so building the request object is also the helper's job in that mode.
// buildSigner (main.go) is the one place that chooses between them.
package main

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// Signer signs one broker call and returns the compact JWS for its Proof header, or builds and
// signs a credential-request object naming the secrets a session is asking for.
type Signer interface {
	Sign(method, url string) (string, error)
	SignRequestObject(audience string, names []string, reason string) (string, error)
}

// fileSigner signs with the key and enrollment id an agent box or pod keeps under
// AGENT_SECRETS_KEY_DIR.
type fileSigner struct {
	key          *ecdsa.PrivateKey
	enrollmentID string
}

func (f *fileSigner) Sign(method, url string) (string, error) {
	return proof.Sign(f.key, f.enrollmentID, method, url, time.Now())
}

func (f *fileSigner) SignRequestObject(audience string, names []string, reason string) (string, error) {
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}}
	}
	return record.Sign(f.key, audience, details, reason, "", time.Now())
}

// helperSigner asks agent-secrets-helper, over its unix socket, to sign for this process's
// registered host session. The helper resolves the session (and its enrollment id) from the
// caller's own pid — nothing here names an enrollment id at all. audience is ignored: the
// helper signs a request object against its own configured broker URL, not a value this
// (untrusted, merely locally-scoped) peer supplies — the same trust boundary the Sign method
// already has, where the helper's own descendancy check is the only gate, never the values a
// peer names in the request itself.
type helperSigner struct {
	sock string
}

// errNoCredential is what a host session gets from a helper that holds no launcher credential
// (NO_CREDENTIAL): after every reboot or helper restart, until the operator logs the machine in,
// the helper enrolls no one, so the session has no broker identity. identity and every command
// that meets it print the same line for it (reportError).
var errNoCredential = errors.New("this machine is not logged in to the secrets broker; not an agent session (run: agent-secrets-login <login>)")

// helperRefusal is the error for a helper answer that is not OK.
func helperRefusal(resp helper.Response) error {
	if resp.Code == helper.CodeNoCredential {
		return errNoCredential
	}
	return fmt.Errorf("%s: %s", resp.Code, resp.Error)
}

func (h *helperSigner) Sign(method, url string) (string, error) {
	resp, err := helper.Call(h.sock, helper.Request{Op: "sign", Method: method, URL: url}, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("agent-secrets-helper at %s: %w", h.sock, err)
	}
	if !resp.OK {
		return "", helperRefusal(resp)
	}
	return resp.Proof, nil
}

func (h *helperSigner) SignRequestObject(audience string, names []string, reason string) (string, error) {
	resp, err := helper.Call(h.sock, helper.Request{Op: "sign-request", Secrets: names, Reason: reason}, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("agent-secrets-helper at %s: %w", h.sock, err)
	}
	if !resp.OK {
		return "", helperRefusal(resp)
	}
	return resp.RequestObject, nil
}
