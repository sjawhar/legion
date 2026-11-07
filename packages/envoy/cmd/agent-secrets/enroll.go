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

// cmdEnroll implements the three enrollments this CLI makes.
//
// --helper is the one a launcher asks for: the broker's HTTP API has no launcher bearer token,
// so the host's one launcher credential lives only in agent-secrets-helper's memory (a human
// installs it with `agent-secrets machine login`). It enrolls a session key a launcher keeps
// outside the helper's process tree (agentbox's, for a box); a session the helper registers
// enrolls itself through `agent-secrets register`, and a Legion pod's enrollment is its own
// launcher's job.
//
// --pod is a Kubernetes pod enrolling itself (pod.go): it holds no credential a person
// installed, and proves who it is with the projected ServiceAccount token its own kubelet
// mounts, so it needs no helper and no launcher. It is how a pod an operator launches by hand
// gets a broker identity with no daemon enrolling it.
//
// The two are mutually exclusive: each is a whole, different way of proving the caller may
// enrol.
func cmdEnroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{
		"runtime-id": true, "thumbprint": true, "session-id": true,
		"service": true, "out": true, "pod-token-file": true,
	})
	flags := newFlagSet("enroll", stderr)
	runtimeID := flags.String("runtime-id", "", "this runtime's stable id")
	thumbprint := flags.String("thumbprint", "", "the signing key's RFC 7638 thumbprint")
	sessionID := flags.String("session-id", "", "the agent session the broker notifies if one of this session's pending requests expires")
	helperFlag := flags.Bool("helper", false, "enroll through this machine's agent-secrets-helper, which holds its launcher credential (required without --pod: a launcher credential is a key held only by a helper or a launcher, never a token this CLI could read)")
	podFlag := flags.Bool("pod", false, "enroll this Kubernetes pod with the projected service-account token its own kubelet mounts")
	service := flags.String("service", "", "--pod: the broker-side service this pod's service account is bound to")
	// PrintDefaults appends the default itself, so neither usage names it twice.
	podTokenFile := flags.String("pod-token-file", defaultPodTokenFile, "--pod: a pod's projected service-account token file")
	out := flags.String("out", os.Getenv("AGENT_SECRETS_KEY_DIR"), "--pod: the directory holding key.pem and enrollment; AGENT_SECRETS_KEY_DIR when not given")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets enroll: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	if *podFlag && *helperFlag {
		fmt.Fprintln(stderr, "agent-secrets enroll: --pod and --helper are mutually exclusive: a pod proves who it is with its own projected token, and needs no machine login")
		return exitUsageError
	}
	if *podFlag {
		// --runtime-id, --thumbprint and --session-id are --helper's. A pod's runtime id is the
		// pod UID its own token carries, its thumbprint is the key this command writes, and it
		// registers no agent session, so each of them given here asked for something this mode
		// does not do. Refused by name, as --helper beside --pod is one line above, rather than
		// silently ignored.
		for _, name := range []string{"runtime-id", "thumbprint", "session-id"} {
			if flagGiven(flags, name) {
				fmt.Fprintf(stderr, "agent-secrets enroll: --%s is --helper's and --pod takes none of it: a pod's runtime id is the pod UID of the token it presents, its thumbprint is the key this writes into --out, and it registers no agent session\n", name)
				return exitUsageError
			}
		}
		return cmdEnrollPod(*service, *podTokenFile, *out, stdout, stderr)
	}
	if !*helperFlag {
		fmt.Fprintln(stderr, "agent-secrets enroll: --helper is required (this CLI enrolls a session key only through the local agent-secrets-helper socket; a Kubernetes pod enrolls itself with --pod)")
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
	resp, err := enrollRetryingDrain(sock, helper.Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: thumbprint, SessionID: sessionIDPtr})
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

// enrollDrainWait is how long `enroll --helper` retries a DRAINING answer (a helper install is
// about to restart the helper): the 30 s it already gives one call, so a box launched inside
// install's drain-to-restart window enrolls with the restarted helper. Only tests shorten it.
var enrollDrainWait = 30 * time.Second

// enrollRetryingDrain asks the helper req, again every 250 ms while it answers DRAINING, until
// enrollDrainWait has passed; the last answer is returned as it came.
func enrollRetryingDrain(sock string, req helper.Request) (helper.Response, error) {
	deadline := time.Now().Add(enrollDrainWait)
	for {
		resp, err := helper.Call(sock, req, 30*time.Second)
		if err != nil || resp.OK || resp.Code != helper.CodeDraining || !time.Now().Before(deadline) {
			return resp, err
		}
		time.Sleep(250 * time.Millisecond)
	}
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
	resp, err := helper.Call(sock, helper.Request{Op: "unenroll-session-key", EnrollmentID: enrollmentID}, 30*time.Second)
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
