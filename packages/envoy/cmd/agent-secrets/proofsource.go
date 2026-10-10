// packages/envoy/cmd/agent-secrets/proofsource.go
//
// Signer produces the compact JWS this process puts in a broker call's Proof header, and the
// signed credential-request object POST /v1/requests embeds (the broker puts
// authorization_details and reason inside
// that signed object, not plain top-level fields). An agent box or pod has its own key on tmpfs
// under AGENT_SECRETS_KEY_DIR and signs both directly for its own enrollment id (fileSigner); a
// host session has no key of its own and asks
// agent-secrets-helper, over AGENT_SECRETS_HELPER_SOCK, which signs only for processes that
// descend from a registered session root (helperSigner) — the session's key never leaves the
// helper process, so building the request object is also the helper's job in that mode.
// buildSigner (main.go) is the one place that chooses between them. The operator's own machine and
// grant commands sign as this machine's login instead, through the helper's sign-launcher op
// (launcherSigner, built by machineContext).
package main

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"os"
	"strings"
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
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
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
// the helper enrolls no one, so the session has no broker identity. identity prints it, and
// reportError prints it the same way for every command that meets it.
var errNoCredential = errors.New("this machine is not logged in to the secrets broker; not an agent session (run: agent-secrets machine login)")

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

// errNoMachineLogin is what the operator's own machine and grant commands get from a helper that
// holds no launcher credential (NO_CREDENTIAL to sign-launcher): there is no machine login to act
// as until the operator logs the machine in.
var errNoMachineLogin = errors.New("this machine is not logged in to the secrets broker; run: agent-secrets machine login")

// errHelperTooOld is what a machine or grant command gets from a helper from before sign-launcher,
// which answers it as an unknown op.
var errHelperTooOld = errors.New("this machine's agent-secrets-helper is older than this client and cannot sign for machine and grant commands; restart it on this release")

// launcherRefusal is the helper's refusal of sign-launcher, such as IN_SESSION, which a machine or
// grant command prints as the helper said it.
type launcherRefusal struct{ code, message string }

func (e *launcherRefusal) Error() string { return e.code + ": " + e.message }

// launcherSigner signs broker calls with the machine credential through the helper's
// sign-launcher op: the key never leaves the helper, which refuses a process inside a registered
// session (a session acts on itself alone) and signs only for its own broker. credentialID is the
// credential the last proof named, as the helper answered it. SignRequestObject always errors: a
// machine login requests no secrets.
type launcherSigner struct {
	sock         string
	credentialID string
}

func (l *launcherSigner) Sign(method, url string) (string, error) {
	resp, err := helper.Call(l.sock, helper.Request{Op: "sign-launcher", Method: method, URL: url}, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("agent-secrets-helper at %s: %w", l.sock, err)
	}
	switch {
	case resp.OK:
		l.credentialID = resp.CredentialID
		return resp.Proof, nil
	case resp.Code == helper.CodeNoCredential:
		return "", errNoMachineLogin
	case resp.Code == helper.CodeBadRequest && resp.Error == "unknown op sign-launcher":
		return "", errHelperTooOld
	}
	return "", &launcherRefusal{code: resp.Code, message: resp.Error}
}

func (l *launcherSigner) SignRequestObject(audience string, names []string, reason string) (string, error) {
	return "", errors.New("a machine login requests no secrets; a session signs its own requests")
}

// machineContext answers the broker URL and a launcher signer. Machine and grant commands run
// where the helper does (the devbox); a machine with no helper has no machine login to act as.
func machineContext() (base string, signer *launcherSigner, err error) {
	base = strings.TrimSuffix(os.Getenv("AGENT_SECRETS_URL"), "/")
	if base == "" {
		return "", nil, errors.New("AGENT_SECRETS_URL is required")
	}
	sock, named := helperSocket()
	if !exists(sock) {
		where := "the default socket; AGENT_SECRETS_HELPER_SOCK is unset"
		if named {
			where = "AGENT_SECRETS_HELPER_SOCK"
		}
		return "", nil, fmt.Errorf("no agent-secrets-helper at %s (%s): machine and grant commands act under this machine's login, which its helper holds", sock, where)
	}
	return base, &launcherSigner{sock: sock}, nil
}
