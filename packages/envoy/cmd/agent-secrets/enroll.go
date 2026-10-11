// packages/envoy/cmd/agent-secrets/enroll.go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// ttlFlag is --ttl's usage line, the request form's and the exec form's alike.
const ttlFlag = "how long the grant lasts, a whole number of seconds such as 2m or 90s; unset, it lasts until this session ends"

// ttlSeconds is --ttl's value as the whole seconds a request object signs: nil when flags never
// set it, and an error for a value that is not a positive whole number of seconds or is past the
// most the broker records (record.ValidLifetime), so `--ttl 0` is refused rather than read as
// unset.
func ttlSeconds(flags *flag.FlagSet, ttl time.Duration) (*int, error) {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == "ttl" })
	if !set {
		return nil, nil
	}
	if ttl%time.Second != 0 || !record.ValidLifetime(int(min(ttl/time.Second, math.MaxInt32+1))) {
		return nil, fmt.Errorf("--ttl %s is not a whole number of seconds from 1s to %ds", ttl, math.MaxInt32)
	}
	seconds := int(ttl / time.Second)
	return &seconds, nil
}

// grantedLifetime checks that the broker gave result's grant the lifetime --ttl asked for. A
// broker or agent-secrets-helper from before --ttl drops the lifetime without refusing the
// request, and the grant would then last longer than asked. A mismatched result this invocation
// made is ended before it can release a secret; an existing approved grant is never revoked. This
// check stays: a client can meet an older broker or helper on every future independent rollout.
func grantedLifetime(ctx context.Context, c *client, signer Signer, result RequestResult, ttl *int) error {
	if ttl == nil || result.State == "denied" || (result.LifetimeSeconds != nil && *result.LifetimeSeconds == *ttl) {
		return nil
	}
	given := "no lifetime"
	if result.LifetimeSeconds != nil {
		given = fmt.Sprintf("a lifetime of %ds", *result.LifetimeSeconds)
	}
	ended := "nothing"
	switch {
	case result.GrantID != nil && result.RecordID != nil:
		ended = "the existing grant " + *result.GrantID
	case result.GrantID != nil:
		if err := c.RevokeGrant(ctx, signer, *result.GrantID); err != nil {
			return fmt.Errorf("the broker gave request %s %s, not --ttl %ds, and revoking its grant %s failed: %w", result.RequestID, given, *ttl, *result.GrantID, err)
		}
		ended = "its grant " + *result.GrantID
	case result.State == "pending":
		if err := c.CancelRequest(ctx, signer, result.RequestID); err != nil {
			return fmt.Errorf("the broker gave request %s %s, not --ttl %ds, and cancelling it failed: %w", result.RequestID, given, *ttl, err)
		}
		ended = "the request"
	}
	return fmt.Errorf("the broker gave request %s %s, not --ttl %ds, so %s was ended: the broker or this machine's agent-secrets-helper is from before --ttl; restart the helper on the pinned release, or request without --ttl", result.RequestID, given, *ttl, ended)
}
func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// writeFileAtomic writes data to path by renaming a finished temporary file into place, so no
// reader ever sees path half-written: a call waiting out a box's enrollment (awaitEnrollment)
// reads key.pem and enrollment the moment each appears.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has taken it
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

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
