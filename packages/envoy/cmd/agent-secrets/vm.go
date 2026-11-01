// packages/envoy/cmd/agent-secrets/vm.go
//
// A Google Cloud VM's own enrollment. A lane VM holds no credential a person installed: it asks
// its metadata server for an identity token its own service account signs, logs in with that
// token at POST /v1/launcher-credentials/token for a launcher credential, and enrolls a session
// key of kind `vm` with that credential's launcher proof. Nothing is stored but the session key
// and its enrollment id, the two files file mode already reads (identity.go), so every later
// form — the exec form, request, self, renew — works from AGENT_SECRETS_KEY_DIR unchanged.
//
// The launcher key is ephemeral: it is generated in memory for the login, signs the one
// enrollment call, and is dropped when the process ends. A VM that must enrol again logs in
// again; its launcher credential is keyed by the VM's own runtime id, so a second login moves
// that one credential rather than taking another VM's.
//
// This file never verifies the metadata token — the broker does, against Google's keys — and
// never prints it. It reads the token's claims only to name the VM: the runtime id the broker
// keys the enrollment by, the instance name the login's request object names, and the identity a
// refusal names so an operator can see which VM was refused.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

const (
	// vmAudienceVar is the audience the broker configured for Google tokens
	// (BROKER_GOOGLE_OIDC_AUDIENCE); a token minted for any other audience is refused, so the VM
	// and the broker must name the same one. --audience overrides it.
	vmAudienceVar = "AGENT_SECRETS_AUDIENCE"
	// metadataHostVar replaces the metadata server's host (and port), as the GCP client libraries
	// and infra/gcp's own fetch script read it. Its value is a host, with no scheme.
	metadataHostVar = "GCE_METADATA_HOST"
	// defaultMetadataHost is the metadata server every Google VM answers on.
	defaultMetadataHost = "metadata.google.internal"
	// metadataIdentityPath mints an identity token for the instance's default service account.
	metadataIdentityPath = "/computeMetadata/v1/instance/service-accounts/default/identity"
	// metadataTimeout bounds one token fetch. The metadata server is on the host's own link, so a
	// fetch that takes longer than this is a fault, not a slow network.
	metadataTimeout = 10 * time.Second
)

// metadataClient asks the metadata server for identity tokens on a transport with no proxy at
// all, as cloud.google.com/go/compute/metadata does. http.DefaultClient honours HTTP_PROXY, and
// the metadata server's address is link-local, not loopback, so the default transport would hand a
// proxy the VM's identity token, which logs in as that VM for up to an hour. renew --vm fetches
// one before every renewal, so it would hand over a fresh one each time.
var metadataClient = &http.Client{Transport: &http.Transport{}}

// vmIdentity is what a VM's identity token says about the VM, as the broker reads it too.
type vmIdentity struct {
	Account    string // the instance's service account, the token's `email`
	Project    string
	Zone       string
	Instance   string // the instance name
	InstanceID string
}

// runtimeID is the one string that names this VM at the broker: its launcher credential's
// subject and its enrollment's runtime_id. An instance id is unique only within a project and a
// zone, so neither the id nor the account alone names one VM.
func (v vmIdentity) runtimeID() string {
	return v.Account + "@" + v.Project + "/" + v.Zone + "/" + v.InstanceID
}

// String names the VM in a refusal, so an operator reading a lane's log knows which VM the broker
// refused and under which account.
func (v vmIdentity) String() string {
	return fmt.Sprintf("account %s, project %s, zone %s, instance %s (id %s)", v.Account, v.Project, v.Zone, v.Instance, v.InstanceID)
}

// metadataURL is the identity-token URL for audience, honouring GCE_METADATA_HOST.
func metadataURL(audience string) string {
	host := os.Getenv(metadataHostVar)
	if host == "" {
		host = defaultMetadataHost
	}
	return "http://" + host + metadataIdentityPath + "?audience=" + url.QueryEscape(audience) + "&format=full"
}

// metadataToken asks the metadata server for an identity token for audience. format=full is
// required: without it Google mints a token carrying no google.compute_engine claims, which names
// no project, zone or instance and so cannot be enrolled.
func metadataToken(ctx context.Context, audience string) (string, error) {
	callURL := metadataURL(audience)
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, callURL, nil)
	if err != nil {
		return "", fmt.Errorf("ask the metadata server at %s: %w", callURL, err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := metadataClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("the metadata server at %s could not be reached (%s names its host): %w", callURL, metadataHostVar, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read the metadata server's answer from %s: %w", callURL, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("the metadata server at %s answered HTTP %d", callURL, resp.StatusCode)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("the metadata server at %s answered an empty identity token", callURL)
	}
	return token, nil
}

// vmClaims reads the identity token's claims without verifying anything: the broker verifies the
// signature, the issuer, the audience and the expiry, and this process has no key to check them
// with. It reads only what names the VM. A claim that is missing is named, together with the
// format=full the token must be minted with, since a token minted without it carries none of the
// compute-engine claims at all.
func vmClaims(token string) (vmIdentity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return vmIdentity{}, errors.New("the metadata server's identity token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return vmIdentity{}, fmt.Errorf("the identity token's claims are not base64url: %w", err)
	}
	var claims struct {
		Email  string `json:"email"`
		Google struct {
			ComputeEngine struct {
				ProjectID    string          `json:"project_id"`
				Zone         string          `json:"zone"`
				InstanceName string          `json:"instance_name"`
				InstanceID   json.RawMessage `json:"instance_id"`
			} `json:"compute_engine"`
		} `json:"google"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return vmIdentity{}, fmt.Errorf("the identity token's claims are not JSON: %w", err)
	}
	ce := claims.Google.ComputeEngine
	id := vmIdentity{
		Account:    claims.Email,
		Project:    ce.ProjectID,
		Zone:       ce.Zone,
		Instance:   ce.InstanceName,
		InstanceID: jsonScalar(ce.InstanceID),
	}
	var missing []string
	for _, c := range []struct {
		name, value string
	}{
		{"email", id.Account},
		{"google.compute_engine.project_id", id.Project},
		{"google.compute_engine.zone", id.Zone},
		{"google.compute_engine.instance_name", id.Instance},
		{"google.compute_engine.instance_id", id.InstanceID},
	} {
		if c.value == "" {
			missing = append(missing, c.name)
		}
	}
	if len(missing) > 0 {
		return vmIdentity{}, fmt.Errorf("the identity token names no %s; it must be requested with format=full, which is the only form carrying the google.compute_engine claims", strings.Join(missing, ", "))
	}
	return id, nil
}

// jsonScalar is a JSON scalar as its text: Google writes instance_id as a string, and a quoted
// value is unquoted here so a number and a string read the same, with no float rounding of an id
// too large for one.
func jsonScalar(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	if strings.HasPrefix(text, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	return text
}

// vmRenewToken is the token a VM's renewer carries: a fresh identity token from the metadata
// server before every renewal, as a pod's renewer re-reads its projected file, since a VM
// enrollment's renewal must carry one the broker can still verify. A metadata server that is not
// answering is a transient fault of the VM's own link, so the loop retries it with backoff
// rather than refusing at startup.
func vmRenewToken(ctx context.Context, audience string) renewToken {
	return renewToken{
		field: "vm_token",
		what:  "VM identity token from the metadata server",
		until: "the metadata server answers",
		read:  func() (string, error) { return metadataToken(ctx, audience) },
	}
}

// vmLauncherSigner signs the VM's one enrollment call with the ephemeral launcher key the token
// login bound its credential to. The key never leaves this process's memory.
type vmLauncherSigner struct {
	key          *ecdsa.PrivateKey
	credentialID string
}

func (s *vmLauncherSigner) Sign(method, url string) (string, error) {
	return proof.SignLauncher(s.key, s.credentialID, method, url, time.Now())
}

// TokenLogin is POST /v1/launcher-credentials/token, the public route a service or a VM logs in
// at with a token its own issuer signed: no proof, no person. It answers the launcher credential
// the signed request object's key now holds.
func (c *client) TokenLogin(ctx context.Context, token, request string) (string, error) {
	body, err := json.Marshal(map[string]string{"token": token, "request": request})
	if err != nil {
		return "", err
	}
	raw, err := c.do(ctx, http.MethodPost, c.baseURL+"/v1/launcher-credentials/token", body, nil)
	if err != nil {
		return "", err
	}
	var result struct {
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode token login response: %w", err)
	}
	if result.CredentialID == "" {
		return "", errors.New("the broker's token login answered no credential_id")
	}
	return result.CredentialID, nil
}

// CreateVMEnrollment is POST /v1/enrollments of kind `vm`, authenticated by the launcher proof of
// the credential the token login just issued, and carrying the same identity token again so the
// broker checks the runtime id against the VM the token names. A key already holding this VM's
// live enrollment is answered 200 with that enrollment, so re-running enroll on a key dir that
// already has key.pem gets its own enrollment back; alreadyLive reports that answer, because the
// broker returns such an enrollment without extending its lease and the caller must renew it.
func (c *client) CreateVMEnrollment(ctx context.Context, signer proofSigner, runtimeID, thumbprint, vmToken string) (enrollmentID string, alreadyLive bool, err error) {
	body, err := json.Marshal(map[string]string{
		"kind":       "vm",
		"runtime_id": runtimeID,
		"thumbprint": thumbprint,
		"vm_token":   vmToken,
	})
	if err != nil {
		return "", false, err
	}
	status, raw, err := c.doProofStatus(ctx, signer, http.MethodPost, "/v1/enrollments", body)
	if err != nil {
		return "", false, err
	}
	var result struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", false, fmt.Errorf("decode enrollment response: %w", err)
	}
	if result.EnrollmentID == "" {
		return "", false, errors.New("the broker's enrollment answered no enrollment_id")
	}
	return result.EnrollmentID, status == http.StatusOK, nil
}

// vmSessionKey is the session key in dir: the one already there, so re-running enroll on a dir
// keeps the key whose enrollment the broker holds live, or a new one written exactly as keygen
// writes it (PKCS8 PEM, 0600, in a 0700 directory, renamed into place).
func vmSessionKey(dir string) (*ecdsa.PrivateKey, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, keyFile)
	if exists(path) {
		return loadKey(path)
	}
	key, err := proof.NewKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	pemBytes, err := encodeKeyPEM(key)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// enrollLockFile is the lock a VM's enrollment is serialized on, in the key dir it writes. The
// lane's credential helper re-runs `enroll --vm` over one directory whenever it needs a secret,
// so two runs can overlap; without the lock the second would log in again (moving the launcher
// credential the first is still enrolling under) and the two would race over key.pem and
// enrollment.
const enrollLockFile = "enroll.lock"

// errNoEnrollLock is lockEnrollDir's answer on a platform with no flock, where no Compute Engine
// VM runs either: asking for --vm there is a usage error, not a failure of the machine.
var errNoEnrollLock = errors.New("enroll --vm runs only on a Linux VM: this platform has no flock to serialize concurrent enrollments on")

// cmdEnrollVM implements "enroll --vm --service <name> [--audience <aud>] [--out <dir>]": the
// whole of a lane VM's enrollment, from the metadata server to the enrollment file the exec form
// reads next. The lane re-runs it over the same directory rather than keeping a renewer, so it
// is idempotent: it keeps key.pem (a new key while the old enrollment is live would be refused
// ALREADY_ENROLLED), and when the broker answers that this key's enrollment is already live —
// which it returns without extending the lease — it renews that lease before it answers. Only
// the enrollment id reaches stdout: the lane's credential helper reads it as git's credential
// protocol reads a helper's output, so every other line is stderr's.
func cmdEnrollVM(service, audience, out string, stdout, stderr io.Writer) int {
	const prefix = "agent-secrets enroll"
	base := brokerBaseURL()
	for _, missing := range []struct{ value, say string }{
		{service, "--service is required: it names the broker-side service this VM's account is bound to"},
		{audience, "--audience (or " + vmAudienceVar + ") is required: it is the audience the broker mints and verifies Google identity tokens for"},
		{out, "--out (or AGENT_SECRETS_KEY_DIR) is required"},
		{base, brokerURLVar + " is required: it names the secrets broker this VM enrolls at"},
	} {
		if missing.value == "" {
			fmt.Fprintf(stderr, "%s --vm: %s\n", prefix, missing.say)
			return exitUsageError
		}
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		fmt.Fprintf(stderr, "%s --vm: %v\n", prefix, err)
		return 1
	}
	unlock, err := lockEnrollDir(filepath.Join(out, enrollLockFile))
	if err != nil {
		fmt.Fprintf(stderr, "%s --vm: %v\n", prefix, err)
		if errors.Is(err, errNoEnrollLock) {
			return exitUsageError
		}
		return 1
	}
	defer unlock()
	return enrolVM(service, audience, out, base, prefix, stdout, stderr)
}

// enrolVM is cmdEnrollVM's work, with the directory made and the lock held.
func enrolVM(service, audience, out, base, prefix string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	token, err := metadataToken(ctx, audience)
	if err != nil {
		fmt.Fprintf(stderr, "%s --vm: %v\n", prefix, err)
		return 1
	}
	identity, err := vmClaims(token)
	if err != nil {
		fmt.Fprintf(stderr, "%s --vm: %v\n", prefix, err)
		return 1
	}
	launcherKey, err := proof.NewKey()
	if err != nil {
		fmt.Fprintf(stderr, "%s --vm: generate the login key: %v\n", prefix, err)
		return 1
	}
	request, err := record.Sign(launcherKey, base, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: identity.Instance, Service: service},
	}, "", "", nil, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "%s --vm: sign the login request: %v\n", prefix, err)
		return 1
	}
	c := newClient(base)
	credentialID, err := c.TokenLogin(ctx, token, request)
	if err != nil {
		fmt.Fprintln(stderr, vmRefusal(prefix, "log in", identity, base, err))
		return 1
	}
	key, err := vmSessionKey(out)
	if err != nil {
		fmt.Fprintf(stderr, "%s --vm: %v\n", prefix, err)
		return 1
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		fmt.Fprintf(stderr, "%s --vm: thumbprint: %v\n", prefix, err)
		return 1
	}
	enrollmentID, alreadyLive, err := c.CreateVMEnrollment(ctx, &vmLauncherSigner{key: launcherKey, credentialID: credentialID}, identity.runtimeID(), thumbprint, token)
	if err != nil {
		fmt.Fprintln(stderr, vmRefusal(prefix, "enrol", identity, base, err))
		return 1
	}
	if alreadyLive {
		// The broker gave back the enrollment this key already holds without extending its
		// lease, so a VM that only ever re-runs enroll would let that lease lapse.
		if _, err := c.RenewEnrollment(ctx, &fileSigner{key: key, enrollmentID: enrollmentID}, enrollmentID, "vm_token", token); err != nil {
			fmt.Fprintln(stderr, vmRefusal(prefix, "renew the lease of the enrollment it already holds", identity, base, err))
			return 1
		}
	}
	if err := writeFileAtomic(filepath.Join(out, enrollmentFile), []byte(enrollmentID+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "%s --vm: %v\n", prefix, err)
		return 1
	}
	fmt.Fprintln(stdout, enrollmentID)
	return 0
}

// vmRefusal is the one stderr line a refused login or enrolment prints: what was refused, which
// VM the token names, the HTTP status, the broker's code and message, and the broker it asked.
// It never prints the token.
func vmRefusal(prefix, what string, identity vmIdentity, base string, err error) string {
	var refused *apiError
	if errors.As(err, &refused) {
		code := refused.Code
		if code == "" {
			code = "UNKNOWN"
		}
		return fmt.Sprintf("%s --vm: this VM (%s) could not %s: HTTP %d %s: %s; asked %s", prefix, identity, what, refused.Status, code, refused.Message, brokerSource(base))
	}
	return fmt.Sprintf("%s --vm: this VM (%s) could not %s: %s", prefix, identity, what, refusalText(brokerSource(base), err))
}
