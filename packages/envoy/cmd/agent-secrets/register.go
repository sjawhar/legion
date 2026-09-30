// packages/envoy/cmd/agent-secrets/register.go
//
// cmdRegister implements "agent-secrets register [--wait SECONDS] [--exec -- COMMAND
// [ARGS...]]": the one command a host session (not an agent box) runs at launch to become a
// registered session root with agent-secrets-helper. The helper pins THIS process by pidfd, so
// with --exec the command execs as the very pid that was registered — shims/omp wraps omp this
// way, and omp becomes the session root every later `agent-secrets` invocation in its process
// tree signs through.
package main

import (
	"flag"
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
	flags := flag.NewFlagSet("agent-secrets register", flag.ContinueOnError)
	flags.SetOutput(stderr)
	wait := flags.Int("wait", 0, "block until the helper has enrolled the session (seconds; 0 returns at once)")
	doExec := flags.Bool("exec", false, "after registering, exec the command that follows as this same process")
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

	sock := os.Getenv("AGENT_SECRETS_HELPER_SOCK")
	if sock == "" {
		fmt.Fprintln(stderr, "agent-secrets register: AGENT_SECRETS_HELPER_SOCK is unset (host sessions only; an agent box has a key dir instead)")
		return exitUsageError
	}

	resp, err := registerWithPatience(sock, *wait, helperConnectPatience)
	if err != nil && !*doExec {
		fmt.Fprintf(stderr, "agent-secrets register: %v\n", err)
		return 1
	}
	if err != nil {
		// --exec never blocks a launch on the broker: warn and fall through to exec anyway.
		fmt.Fprintf(stderr, "agent-secrets: helper at %s unreachable (%v); this session has no secrets access until it is relaunched with the helper running\n", sock, err)
	} else {
		if !*doExec {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", resp.RuntimeID, resp.EnrollmentID, resp.State)
		}
		if *wait > 0 && resp.State != "enrolled" {
			if !*doExec {
				fmt.Fprintf(stderr, "agent-secrets register: not enrolled yet: %s\n", resp.Error)
				return 1
			}
			// The launch still goes ahead, but not silently: the agent starts with a session
			// whose broker calls fail (NOT_ENROLLED) until the helper's enroll loop succeeds.
			fmt.Fprintf(stderr, "agent-secrets register: not enrolled after %ds (%s); launching anyway, and this session's secrets calls fail until the helper enrolls it\n", *wait, resp.Error)
		}
	}

	if !*doExec {
		return 0
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets register: %v\n", err)
		return 127
	}
	if err := syscall.Exec(path, command, os.Environ()); err != nil {
		fmt.Fprintf(stderr, "agent-secrets register: exec: %v\n", err)
		return 126
	}
	return 0 // unreachable: syscall.Exec replaces this process image on success
}

// registerWithPatience retries the connection while the helper is absent (it may be starting
// with the user manager at boot), for patience at most.
func registerWithPatience(sock string, wait int, patience time.Duration) (helper.Response, error) {
	deadline := time.Now().Add(patience)
	for {
		resp, err := helper.Call(sock, helper.Request{Op: "register", WaitSeconds: wait}, time.Duration(wait+10)*time.Second)
		if err == nil {
			if !resp.OK {
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
