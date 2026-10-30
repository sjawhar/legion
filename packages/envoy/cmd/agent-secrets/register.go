// packages/envoy/cmd/agent-secrets/register.go
//
// cmdRegister implements "agent-secrets register [--wait SECONDS] [--session-id ID] [--exec --
// COMMAND [ARGS...]]": the one command a machine session (not an agent box) runs at launch to
// become a registered session root with agent-secrets-helper. The helper pins THIS process by
// pidfd, so with --exec the command execs as the very pid that was registered — shims/omp wraps
// omp this way, and omp becomes the session root every later `agent-secrets` invocation in its
// process tree signs through.
//
// --session-id is the launcher saying which agent session this is. The helper records it at
// registration and sends it as the enrollment's session_id, so Dispatch's approval page can name
// the session a credential request came from. The launcher names it because the session itself
// cannot be believed about it: a process may rewrite its own environment, so neither this
// command's environment nor the pinned process's /proc environ is evidence. It is a correlation
// key, not an entitlement, and a launcher that does not know the id leaves it out: the session
// registers, enrolls and gets secrets exactly the same, and the page says no session is named.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// helperConnectPatience is how long cmdRegister retries connecting to a still-starting helper
// before giving up (or, under --exec, warning and launching anyway) — the launch-ordering case
// tmux-resurrect hits at boot.
const helperConnectPatience = 10 * time.Second

func cmdRegister(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("register", stderr)
	wait := flags.Int("wait", 0, "wait up to this many seconds for the helper to enroll the session (0 returns at once, as does a helper holding no launcher credential)")
	doExec := flags.Bool("exec", false, "after registering, exec the command that follows as this same process")
	sessionID := flags.String("session-id", "", "the agent session this process is, as the launcher knows it; recorded as the enrollment's session_id so a credential request names its session")
	if err := flags.Parse(args); err != nil {
		return exitUsage(err)
	}
	command := flags.Args()
	if *doExec && len(command) == 0 {
		fmt.Fprintln(stderr, "agent-secrets register --exec needs -- COMMAND [ARGS...]")
		return exitUsageError
	}
	if !*doExec && len(command) > 0 {
		fmt.Fprintf(stderr, "agent-secrets register: unexpected argument %q\n", command[0])
		return exitUsageError
	}
	if !helper.ValidSessionID(*sessionID) {
		fmt.Fprintf(stderr, "agent-secrets register: --session-id must be at most %d characters with no whitespace or control character\n", helper.MaxSessionIDLength)
		return exitUsageError
	}

	// The helper's socket as every other helper-mode form finds it (identity.go): the variable when
	// set, else the default socket when it is there. An agent box has neither: its key dir sits
	// where the default socket's directory would be, and no helper listens in it.
	sock, named := helperSocket()
	if !named && !exists(sock) {
		fmt.Fprintf(stderr, "agent-secrets register: AGENT_SECRETS_HELPER_SOCK is unset and no helper socket is at %s (machine sessions only; an agent box has a key dir instead)\n", sock)
		return exitUsageError
	}

	resp, err := registerWithPatience(sock, *wait, *sessionID, helperConnectPatience)
	// A reply whose id is not the one asked for is not a failure, and never stops a launch: the
	// pid may already be inside a session, which keeps its own id, and a helper from before
	// --session-id ignores the field and answers a plain success. Either way the enrollment will
	// not carry this id, so say so once rather than let a launcher believe it was recorded.
	if err == nil && *sessionID != "" && resp.SessionID != *sessionID {
		recorded := resp.SessionID
		if recorded == "" {
			recorded = "none (this process is already inside a session that named none, or the helper is from before --session-id)"
		}
		fmt.Fprintf(stderr, "agent-secrets register: the helper recorded %s, not %q; this session's enrollment names what the helper recorded\n", recorded, *sessionID)
	}
	if !*doExec {
		if err != nil {
			fmt.Fprintf(stderr, "agent-secrets register: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", resp.RuntimeID, resp.EnrollmentID, resp.State, resp.SessionID)
		if *wait > 0 && resp.State != "enrolled" {
			fmt.Fprintf(stderr, "agent-secrets register: not enrolled yet: %s\n", resp.Error)
			return 1
		}
		return 0
	}
	// --exec never blocks a launch on the broker, but never launches silently either: the agent
	// starts with no secrets access (helper unreachable), with broker calls that fail NOT_ENROLLED
	// until the helper's enroll loop succeeds, or, while the helper holds no launcher credential,
	// with no broker identity at all: its agent-secrets calls fail NO_CREDENTIAL, and identity
	// exits 1, until the machine is logged in.
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "agent-secrets: helper at %s unreachable (%v); this session has no secrets access until it is relaunched with the helper running\n", sock, err)
	case *wait > 0 && resp.State != "enrolled" && resp.Code == helper.CodeNoCredential:
		fmt.Fprintln(stderr, "agent-secrets register: this machine is not logged in to the secrets broker; launching anyway, and until it is (run: agent-secrets machine login) this session's agent-secrets calls fail")
	case *wait > 0 && resp.State != "enrolled":
		fmt.Fprintf(stderr, "agent-secrets register: not enrolled yet (%s); launching anyway, and this session's secrets calls fail until the helper enrolls it\n", resp.Error)
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets register: %v\n", err)
		return exitNotFound
	}
	if err := syscall.Exec(path, command, os.Environ()); err != nil {
		fmt.Fprintf(stderr, "agent-secrets register: exec: %v\n", err)
		return exitCannotRun
	}
	return 0 // unreachable: syscall.Exec replaces this process image on success
}

// registerWithPatience retries the connection while the helper is absent (it may be starting
// with the user manager at boot), for patience at most, and retries a DRAINING answer (a helper
// install is about to restart) until the larger of patience and the --wait seconds has passed, so
// the session registers with the restarted helper rather than launching with no broker identity.
// sessionID is sent only when the launcher named one, so a helper from before this field sees
// exactly the request it saw before.
func registerWithPatience(sock string, wait int, sessionID string, patience time.Duration) (helper.Response, error) {
	req := helper.Request{Op: "register", WaitSeconds: wait}
	if sessionID != "" {
		req.SessionID = &sessionID
	}
	start := time.Now()
	deadline := start.Add(patience)
	drainDeadline := start.Add(max(patience, time.Duration(wait)*time.Second))
	for {
		resp, err := helper.Call(sock, req, time.Duration(wait+10)*time.Second)
		if err == nil {
			if !resp.OK {
				if resp.Code == helper.CodeDraining && time.Now().Before(drainDeadline) {
					time.Sleep(250 * time.Millisecond)
					continue
				}
				return resp, fmt.Errorf("%s: %s", resp.Code, resp.Error)
			}
			return resp, nil
		}
		if time.Now().After(deadline) {
			return helper.Response{}, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}
