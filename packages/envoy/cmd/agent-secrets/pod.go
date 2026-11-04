// packages/envoy/cmd/agent-secrets/pod.go
//
// A Kubernetes pod's own enrollment, the pod-side mirror of vm.go. A pod an operator launches by
// hand holds no credential a person installed and has no agent-secrets-helper: what it has is the
// projected ServiceAccount token its own kubelet mounts, which the broker verifies against the
// cluster's OIDC issuer. So it logs in with that token at POST /v1/launcher-credentials/token for
// a launcher credential, and enrolls a session key of kind `pod` with that credential's launcher
// proof. Nothing is stored but the session key and its enrollment id, the two files file mode
// already reads (identity.go), so every later form — the exec form, request, self, renew — works
// from AGENT_SECRETS_KEY_DIR unchanged.
//
// It differs from a VM's enrollment in three ways and no more: the token is read from the file the
// pod's projection mounts rather than fetched from a metadata server, the runtime id is the pod's
// own UID, and the enrollment's kind is `pod`.
//
// The token must be the pod's own projected token, not one `kubectl create token` mints: the
// broker lets a pod's own machine login enrol that pod alone, and a login bound to no pod enrols
// nothing.
//
// This file never verifies the token — the broker does, against the cluster's issuer — and never
// prints it. It reads the token's claims only to name the pod.
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// defaultPodTokenFile is where a Legion pod and a hand-launched session pod both mount their
// projected ServiceAccount token for the agent-secrets audience, so a pod with the standard
// projection needs no --pod-token-file at all.
const defaultPodTokenFile = "/var/run/legion/agent-secrets-token/token"

// podIdentity is what a pod's projected token says about the pod, as the broker reads it too
// (enroll.AccountClaims).
type podIdentity struct {
	// Subject is the ServiceAccount the token was minted for,
	// system:serviceaccount:<namespace>:<name>; the broker names the launcher credential after it.
	Subject string
	// PodUID is the UID of the pod the token is bound to, the broker's runtime id for it.
	PodUID string
}

// runtimeID is the one string that names this pod at the broker: its enrollment's runtime_id. A
// pod UID is unique across the cluster and across time, so it names one pod by itself — unlike a
// VM's instance id, which needs its project and zone.
func (p podIdentity) runtimeID() string { return p.PodUID }

// String names the pod in a refusal, so an operator reading the pod's log knows which pod the
// broker refused and under which ServiceAccount.
func (p podIdentity) String() string {
	return fmt.Sprintf("account %s, pod uid %s", p.Subject, p.PodUID)
}

// podClaims reads the projected token's claims without verifying anything: the broker verifies the
// signature, the issuer, the audience and the expiry against the cluster's OIDC issuer, and this
// process has no key to check them with. It reads only what names the pod, and the broker refuses
// an enrollment whose runtime_id is not the pod UID its own verification of this same token found,
// so a claim read here can name this pod and nothing else.
//
// Reading the UID from the token rather than taking it as a flag is what spares the pod a
// downwardAPI entry for its own UID: the one thing the broker will accept is already in the file
// the pod must read anyway.
func podClaims(token string) (podIdentity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return podIdentity{}, errors.New("the projected service-account token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return podIdentity{}, fmt.Errorf("the projected token's claims are not base64url: %w", err)
	}
	var claims struct {
		Subject    string `json:"sub"`
		Kubernetes struct {
			Pod struct {
				UID string `json:"uid"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return podIdentity{}, fmt.Errorf("the projected token's claims are not JSON: %w", err)
	}
	id := podIdentity{Subject: claims.Subject, PodUID: claims.Kubernetes.Pod.UID}
	if id.PodUID == "" {
		return podIdentity{}, errors.New("the projected token is bound to no pod (it names no kubernetes.io.pod.uid), so it enrolls nothing: a pod must present the token its own projection mounts, not one `kubectl create token` minted")
	}
	if id.Subject == "" {
		return podIdentity{}, errors.New("the projected token names no subject (sub), so it names no ServiceAccount to log in as")
	}
	return id, nil
}

// podLauncherSigner signs the pod's one enrollment call with the ephemeral launcher key the token
// login bound its credential to. The key never leaves this process's memory.
type podLauncherSigner struct {
	key          *ecdsa.PrivateKey
	credentialID string
}

func (s *podLauncherSigner) Sign(method, url string) (string, error) {
	return proof.SignLauncher(s.key, s.credentialID, method, url, time.Now())
}

// CreatePodEnrollment is POST /v1/enrollments of kind `pod`, authenticated by the launcher proof
// of the credential the token login just issued, and carrying the same projected token again so
// the broker checks the runtime id against the pod the token names. A key already holding this
// pod's live enrollment is answered 200 with that enrollment, so re-running enroll on a key dir
// that already has key.pem gets its own enrollment back; alreadyLive reports that answer, because
// the broker returns such an enrollment without extending its lease and the caller must renew it.
func (c *client) CreatePodEnrollment(ctx context.Context, signer proofSigner, runtimeID, thumbprint, podToken string) (enrollmentID string, alreadyLive bool, err error) {
	body, err := json.Marshal(map[string]string{
		"kind":       "pod",
		"runtime_id": runtimeID,
		"thumbprint": thumbprint,
		"pod_token":  podToken,
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

// cmdEnrollPod implements "enroll --pod --service <name> [--pod-token-file <file>] [--out <dir>]":
// the whole of a pod's enrollment, from the projected token to the enrollment file the exec form
// reads next. It is idempotent over one directory, as `enroll --vm` is: it keeps key.pem (a new
// key while the old enrollment is live would be refused ALREADY_ENROLLED), and when the broker
// answers that this key's enrollment is already live — which it returns without extending the
// lease — it renews that lease before it answers. Only the enrollment id reaches stdout; every
// other line, the next command included, is stderr's.
func cmdEnrollPod(service, tokenFile, out string, stdout, stderr io.Writer) int {
	const prefix = "agent-secrets enroll"
	base := brokerBaseURL()
	for _, missing := range []struct{ value, say string }{
		{service, "--service is required: it names the broker-side service this pod's ServiceAccount is bound to"},
		{tokenFile, "--pod-token-file is required: it names the pod's projected service-account token file"},
		{out, "--out (or AGENT_SECRETS_KEY_DIR) is required"},
		{base, brokerURLVar + " is required: it names the secrets broker this pod enrolls at"},
	} {
		if missing.value == "" {
			fmt.Fprintf(stderr, "%s --pod: %s\n", prefix, missing.say)
			return exitUsageError
		}
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		fmt.Fprintf(stderr, "%s --pod: %v\n", prefix, err)
		return 1
	}
	unlock, err := lockEnrollDir(filepath.Join(out, enrollLockFile))
	if err != nil {
		if errors.Is(err, errNoEnrollLock) {
			// The sentinel's own text names a VM, since that is the mode that reaches it from a
			// lane; this is the same platform fault told as a pod's.
			fmt.Fprintf(stderr, "%s --pod: enroll --pod runs only on a Linux pod: this platform has no flock to serialize concurrent enrollments on\n", prefix)
			return exitUsageError
		}
		fmt.Fprintf(stderr, "%s --pod: %v\n", prefix, err)
		return 1
	}
	defer unlock()
	return enrolPod(service, tokenFile, out, base, prefix, stdout, stderr)
}

// enrolPod is cmdEnrollPod's work, with the directory made and the lock held.
func enrolPod(service, tokenFile, out, base, prefix string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	token, err := readPodToken(tokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s --pod: %v\n", prefix, err)
		return 1
	}
	identity, err := podClaims(token)
	if err != nil {
		fmt.Fprintf(stderr, "%s --pod: %v\n", prefix, err)
		return 1
	}
	launcherKey, err := proof.NewKey()
	if err != nil {
		fmt.Fprintf(stderr, "%s --pod: generate the login key: %v\n", prefix, err)
		return 1
	}
	// The identifier names this pod in the signed request object; the broker names the credential
	// it issues from the token's own subject, never from this.
	request, err := record.Sign(launcherKey, base, []record.AuthorizationDetail{
		{Type: "launcher_credential", Identifier: identity.PodUID, Service: service},
	}, "", "", nil, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "%s --pod: sign the login request: %v\n", prefix, err)
		return 1
	}
	c := newClient(base)
	credentialID, err := c.TokenLogin(ctx, token, request)
	if err != nil {
		fmt.Fprintln(stderr, podRefusal(prefix, "log in", identity, base, err))
		return 1
	}
	key, err := enrollSessionKey(out)
	if err != nil {
		fmt.Fprintf(stderr, "%s --pod: %v\n", prefix, err)
		return 1
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		fmt.Fprintf(stderr, "%s --pod: thumbprint: %v\n", prefix, err)
		return 1
	}
	enrollmentID, alreadyLive, err := c.CreatePodEnrollment(ctx, &podLauncherSigner{key: launcherKey, credentialID: credentialID}, identity.runtimeID(), thumbprint, token)
	if err != nil {
		fmt.Fprintln(stderr, podRefusal(prefix, "enrol", identity, base, err))
		return 1
	}
	if alreadyLive {
		// The broker gave back the enrollment this key already holds without extending its
		// lease, so a pod that only ever re-runs enroll would let that lease lapse.
		if _, err := c.RenewEnrollment(ctx, &fileSigner{key: key, enrollmentID: enrollmentID}, enrollmentID, "pod_token", token); err != nil {
			fmt.Fprintln(stderr, podRefusal(prefix, "renew the lease of the enrollment it already holds", identity, base, err))
			return 1
		}
	}
	if err := writeFileAtomic(filepath.Join(out, enrollmentFile), []byte(enrollmentID+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "%s --pod: %v\n", prefix, err)
		return 1
	}
	// A pod keeps no credential helper to re-run this command, so it needs a renewer: say which
	// one, on stderr, where it cannot be mistaken for the enrollment id.
	fmt.Fprintf(stderr, "%s --pod: enrolled; keep this lease alive with: agent-secrets renew --pod-token-file %s\n", prefix, tokenFile)
	fmt.Fprintln(stdout, enrollmentID)
	return 0
}

// podRefusal is the one stderr line a refused login or enrolment prints: what was refused, which
// pod the token names, the HTTP status, the broker's code and message, and the broker it asked.
// It never prints the token.
func podRefusal(prefix, what string, identity podIdentity, base string, err error) string {
	var refused *apiError
	if errors.As(err, &refused) {
		code := refused.Code
		if code == "" {
			code = "UNKNOWN"
		}
		return fmt.Sprintf("%s --pod: this pod (%s) could not %s: HTTP %d %s: %s; asked %s", prefix, identity, what, refused.Status, code, refused.Message, brokerSource(base))
	}
	return fmt.Sprintf("%s --pod: this pod (%s) could not %s: %s", prefix, identity, what, refusalText(brokerSource(base), err))
}
