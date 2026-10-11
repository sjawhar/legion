// packages/envoy/cmd/agent-secrets/enroll.go
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// ---------------------------------------------------------------------------
// enroll / unenroll (a session key a launcher holds, through the local agent-secrets-helper)
// ---------------------------------------------------------------------------

// cmdEnroll implements "enroll --helper --runtime-id <id> --thumbprint <tp> [--session-id <id>]".
// --helper is required: the broker's HTTP API has no launcher bearer token, so this CLI has no
// other way to enroll anything — the host's one launcher credential lives only in
// agent-secrets-helper's memory (a human installs it with `agent-secrets machine login`). It
// enrolls a session key a launcher keeps outside the helper's process tree (agentbox's, for a
// box); a session the helper registers enrolls itself through `agent-secrets register`, and a
// pod's enrollment is its own launcher's job.
func cmdEnroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{
		"kind": true, "runtime-id": true, "thumbprint": true, "session-id": true,
	})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets enroll: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := newFlagSet("enroll", stderr)
	// --kind box is accepted because dotfiles' scripts/agentbox passes it on every box it enrolls,
	// and must until its pinned agent-secrets is legion-envoy 10 or later: an agent-secrets before
	// that refuses `enroll --helper` without --kind box. Any other --kind is refused. The flag goes
	// once dotfiles' pin is legion-envoy 10 or later and scripts/agentbox on dotfiles' main passes
	// no --kind.
	kind := flags.String("kind", "", `"box" (accepted for agentbox compatibility)`)
	runtimeID := flags.String("runtime-id", "", "this runtime's stable id")
	thumbprint := flags.String("thumbprint", "", "the signing key's RFC 7638 thumbprint")
	sessionID := flags.String("session-id", "", "the agent session the broker notifies if one of this session's pending requests expires")
	helperFlag := flags.Bool("helper", false, "enroll through this machine's agent-secrets-helper, which holds its launcher credential (required: a launcher credential is a key held only by a helper or a launcher, never a token this CLI could read)")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if !*helperFlag {
		fmt.Fprintln(stderr, "agent-secrets enroll: --helper is required (this CLI enrolls only through the local agent-secrets-helper socket)")
		return exitUsageError
	}
	if *kind != "" && *kind != "box" {
		fmt.Fprintf(stderr, "agent-secrets enroll: --kind %q is not a kind enroll takes: every session a launcher enrolls is one kind, so leave --kind out\n", *kind)
		return exitUsageError
	}
	return cmdEnrollHelper(*runtimeID, *thumbprint, *sessionID, stdout, stderr)
}

// cmdEnrollHelper asks the local agent-secrets-helper daemon, over its unix socket, to enroll a
// session key using the machine credential `agent-secrets machine login` installed. On success
// it writes the enrollment id into AGENT_SECRETS_KEY_DIR/enrollment (buildSigner reads it back
// for every later session-authenticated subcommand) and prints the enrollment id alone on stdout
// — the contract scripts/agentbox parses.
func cmdEnrollHelper(runtimeID, thumbprint, sessionID string, stdout, stderr io.Writer) int {
	if runtimeID == "" || thumbprint == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: --helper needs --runtime-id and --thumbprint")
		return exitUsageError
	}
	dir := os.Getenv("AGENT_SECRETS_KEY_DIR")
	if dir == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: AGENT_SECRETS_KEY_DIR is required")
		return exitUsageError
	}
	sock, _ := helperSocket()
	var sessionIDPtr *string
	if sessionID != "" {
		sessionIDPtr = &sessionID
	}
	resp, err := callSessionKeyOp(sock, "enroll", helper.Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: thumbprint, SessionID: sessionIDPtr}, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets enroll: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	if err := writeFileAtomic(filepath.Join(dir, enrollmentFile), []byte(resp.EnrollmentID+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, resp.EnrollmentID)
	return 0
}

// oldHelperSessionKeyOps names, for each kind-free op, the op an agent-secrets-helper from before
// this release serves the call under. This CLI ships with the helper in one release, but a helper
// restarts onto it only once no session is registered with it, so until then this CLI meets the
// older helper; the map goes once no helper older than this release runs.
var oldHelperSessionKeyOps = map[string]string{"enroll-session-key": "enroll-box", "unenroll-session-key": "unenroll-box"}

// callSessionKeyOp asks the helper req, and asks it again under the old-helper op when the helper
// answers that it does not know the new one, saying so on stderr under command's name.
func callSessionKeyOp(sock, command string, req helper.Request, stderr io.Writer) (helper.Response, error) {
	resp, err := helper.Call(sock, req, 30*time.Second)
	if err != nil || resp.OK || resp.Code != helper.CodeBadRequest || resp.Error != "unknown op "+req.Op {
		return resp, err
	}
	legacy := oldHelperSessionKeyOps[req.Op]
	fmt.Fprintf(stderr, "agent-secrets %s: this machine's agent-secrets-helper is older than this client (it has no %s op); asking its %s instead. It moves onto this release when it restarts with no session registered.\n", command, req.Op, legacy)
	req.Op = legacy
	return helper.Call(sock, req, 30*time.Second)
}

// cmdUnenroll implements "unenroll --helper --enrollment <id>". --helper is required for the
// same reason cmdEnroll requires it: the broker's HTTP API has no launcher bearer token.
func cmdUnenroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{"enrollment": true})
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets unenroll: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	flags := newFlagSet("unenroll", stderr)
	enrollmentID := flags.String("enrollment", "", "the enrollment id to revoke")
	helperFlag := flags.Bool("helper", false, "unenroll through the local agent-secrets-helper socket")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if !*helperFlag {
		fmt.Fprintln(stderr, "agent-secrets unenroll: --helper is required (this CLI unenrolls only through the local agent-secrets-helper socket)")
		return exitUsageError
	}
	if *enrollmentID == "" {
		fmt.Fprintln(stderr, "agent-secrets unenroll: --enrollment is required")
		return exitUsageError
	}
	return cmdUnenrollHelper(*enrollmentID, stderr)
}

// cmdUnenrollHelper asks the local agent-secrets-helper daemon to end an enrollment
// `enroll --helper` made, over its unix socket, using the machine credential `agent-secrets
// machine login` installed. Idempotent: the helper treats the broker's 204 and 404 as done.
// Prints nothing on success, matching the exit-code-only unenroll contract.
func cmdUnenrollHelper(enrollmentID string, stderr io.Writer) int {
	sock, _ := helperSocket()
	resp, err := callSessionKeyOp(sock, "unenroll", helper.Request{Op: "unenroll-session-key", EnrollmentID: enrollmentID}, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets unenroll: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets unenroll: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	return 0
}
