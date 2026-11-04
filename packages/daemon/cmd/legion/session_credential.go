package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// sessionCommands is the operator's hand on a session pod: a pod the operator launches by hand in
// the deployment's namespace, outside every claim, which the daemon neither starts, stops nor
// supervises. Each subcommand is one session route (internal/api/session.go), and each gives the
// pod a thing only the daemon holds — its key's enrollment with the secrets broker, and a gh
// bundle written into a Secret of the pod's own.
//
// It is not `legion sessions`, which is the one-off copy of recorded Oh My Pi session files into
// the session database: that command's noun is a transcript, this one's is a pod.
var sessionCommands = map[string]command{
	"enroll":            runSessionEnroll,
	"github-credential": runSessionGitHubCredential,
}

func runSession(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "session", sessionCommands, args, stdout, stderr)
}

// sessionRoute is the root of the daemon's session routes.
const sessionRoute = "/legion/v1/sessions"

// newSessionCall is one `legion session` subcommand's flag set: the operator's three flags, with
// the bearer required, and --json to print the daemon's answer as it served it.
func newSessionCall(sub string, stdout, stderr io.Writer) (*operatorCall, *bool) {
	c := newOperatorCall("session "+sub, "usage: legion session "+sub+" [flags]",
		"file holding the operator bearer the daemon's operator_token_file names (required)", stdout, stderr)
	return c, c.flags.Bool("json", false, "print the daemon's answer as it served it")
}

// runSessionEnroll enrolls a session pod's key with the secrets broker: the daemon logs in with
// its own ServiceAccount token and enrolls the key under that login, proving the pod with the
// pod's own projected token, exactly as it enrolls a claim pod's shim.
//
// The two things that are the pod's own, not the daemon's, are inputs here: the thumbprint of the
// key `agent-secrets keygen` wrote inside the pod, and the pod's projected token for the broker's
// audience, which the daemon can neither generate nor read. The pod's launcher runs the keygen and
// the `agent-secrets renew` loop, as `legion worker-shim` does in a claim's pod; this command and
// the daemon run no `kubectl` and reach into no pod.
//
// The pod's uid is the daemon's to resolve, not this command's to assert: the daemon holds a
// Kubernetes client for its own namespace already, and resolving there is what lets the route
// refuse a pod that does not exist and refuse one of Legion's own. --pod-uid is optional and is
// compared, not trusted: it is how a caller that read the pod says which pod it meant, so a pod of
// that name replaced since is refused rather than enrolled.
//
// The token is only ever a file's contents (--pod-token-file), never a flag value: a value would
// stand in this process's argv for anyone on the box to read.
func runSessionEnroll(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c, asJSON := newSessionCall("enroll", stdout, stderr)
	pod := c.flags.String("pod", "", "name of the session pod in the daemon's namespace (required)")
	podUID := c.flags.String("pod-uid", "", "uid the caller read for that pod; the daemon refuses another pod of the same name")
	thumbprint := c.flags.String("thumbprint", "", "thumbprint `agent-secrets keygen` printed inside the pod (required)")
	tokenFile := c.flags.String("pod-token-file", "", "file holding the pod's projected token for the broker's audience (required)")
	session := c.flags.String("session", "", "Oh My Pi session id to record the enrollment under")
	if code, ok := c.parse(args, "operator-token-file", "pod", "thumbprint", "pod-token-file"); !ok {
		return code
	}
	podToken, err := config.ReadPrivateSecretPointer("--pod-token-file", *tokenFile)
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", c.name, err)
		return 1
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	return c.send(ctx, op, http.MethodPost, sessionRoute+"/agent-secrets", api.SessionEnrollRequest{
		Pod: *pod, PodUID: *podUID, Thumbprint: *thumbprint, PodToken: podToken, Session: *session,
	}, answerPrinter(*asJSON, printSessionEnrollment))
}

// runSessionGitHubCredential writes a session pod's gh bundle into a Secret of the pod's own: the
// implement App's write token for --owner, that App's read-only token for every other owner of
// the project, and the default owner naming the directory the pod's GH_CONFIG_DIR points at. The
// daemon keeps that Secret fresh on its minute tick for as long as the pod runs, so the hour-long
// installation tokens in it never go stale under a running session.
//
// No token reaches this command: the bundle goes from the daemon into the Secret, and the answer
// names the Secret, the owners and the expiry.
func runSessionGitHubCredential(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c, asJSON := newSessionCall("github-credential", stdout, stderr)
	pod := c.flags.String("pod", "", "name of the session pod in the daemon's namespace (required)")
	podUID := c.flags.String("pod-uid", "", "uid the caller read for that pod; the daemon refuses another pod of the same name")
	owner := c.flags.String("owner", "", "GitHub owner the session writes to, its bundle's default (required)")
	if code, ok := c.parse(args, "operator-token-file", "pod", "owner"); !ok {
		return code
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	return c.send(ctx, op, http.MethodPost, sessionRoute+"/github-credential", api.SessionCredentialRequest{
		Pod: *pod, PodUID: *podUID, Owner: *owner,
	}, answerPrinter(*asJSON, printSessionCredential))
}

// answerPrinter is print, or under --json the answer as the daemon served it.
func answerPrinter(asJSON bool, print func(io.Writer, []byte) error) func(io.Writer, []byte) error {
	if asJSON {
		return writeAnswer
	}
	return print
}

// printSessionEnrollment is the one line an enrollment prints as: the pod uid enrolled, the
// enrollment id the pod writes beside its key, and when the lease expires without a renewal.
func printSessionEnrollment(w io.Writer, answer []byte) error {
	var enrolled api.SessionEnrollResponse
	if err := decodeAnswer(answer, &enrolled); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "enrolled pod %s as %s, lease expires %s\n",
		enrolled.PodUID, enrolled.EnrollmentID, enrolled.LeaseExpiresAt)
	return err
}

// printSessionCredential is the one line a gh bundle prints as: the Secret the pod projects, the
// App its write token acts as, the default owner, every owner the bundle holds, and the soonest
// of the tokens' expiries.
func printSessionCredential(w io.Writer, answer []byte) error {
	var written api.SessionCredentialResponse
	if err := decodeAnswer(answer, &written); err != nil {
		return err
	}
	owners, err := json.Marshal(written.Owners)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "wrote Secret %s for pod %s: app %s, default %s, owners %s, expires %s\n",
		written.SecretName, written.PodUID, written.App, written.Default, owners, written.ExpiresAt)
	return err
}
