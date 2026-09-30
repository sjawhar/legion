// packages/envoy/cmd/agent-secrets/launcher.go
//
// "agent-secrets launcher login" and "agent-secrets launcher login-status": the machine login the
// host helper holds for every session it registers, started and read through the helper.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// cmdLauncher dispatches this CLI's two launcher subcommands: "login" and "login-status".
func cmdLauncher(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, `agent-secrets launcher: "login" or "login-status" is required`)
		return exitUsageError
	}
	switch args[0] {
	case "login":
		return cmdLauncherLogin(args[1:], stdout, stderr)
	case "login-status":
		return cmdLauncherLoginStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-secrets launcher: unknown subcommand %q\n", args[0])
		return exitUsageError
	}
}

// cmdLauncherLogin implements "launcher login". It asks agent-secrets-helper to start a machine
// login, prints the broker's confirmation code for the operator to type on the Dispatch
// credential page, then polls the helper until the login reaches a terminal state.
func cmdLauncherLogin(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "agent-secrets launcher login: unexpected argument %q\n", args[0])
		return exitUsageError
	}
	sock, _ := helperSocket()
	resp, err := helper.Call(sock, helper.Request{Op: "login"}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets launcher login: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets launcher login: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	fmt.Fprintf(stdout, "machine login code: %s\n", resp.Code)
	if approveURL := strings.TrimSuffix(os.Getenv("AGENT_SECRETS_APPROVE_URL"), "/"); approveURL != "" {
		fmt.Fprintf(stdout, "enter it at %s/credentials/machine — approve only if the code matches this terminal\n", approveURL)
	} else {
		fmt.Fprintln(stdout, "enter it on the Dispatch credential page")
	}
	backoff := 2 * time.Second
	for {
		resp, err := helper.Call(sock, helper.Request{Op: "login-status"}, 10*time.Second)
		if err != nil {
			fmt.Fprintf(stderr, "agent-secrets launcher login: %v\n", err)
			return 1
		}
		if !resp.OK {
			fmt.Fprintf(stderr, "agent-secrets launcher login: %s: %s\n", resp.Code, resp.Error)
			return 1
		}
		switch resp.LoginState {
		case "issued":
			return 0
		case "denied", "expired":
			fmt.Fprintf(stderr, "agent-secrets launcher login: %s\n", resp.LoginState)
			return 1
		}
		time.Sleep(backoff)
		backoff = nextBackoff(backoff)
	}
}

// cmdLauncherLoginStatus implements "launcher login-status": a read-only, single-shot query of
// the helper's current (or most recently settled) machine login state, with no side effect —
// unlike re-running "login" itself, which mints a fresh key and opens a brand-new pending
// machine-login record even while a credential is already issued (Broker.Login only
// short-circuits a login that is still *pending*, per its own doc comment). Its exit code is a
// liveness probe scripts can use directly: 0 only when the state is "issued", 1 for
// "pending"/"denied"/"expired" and for "" (login never run) — the same distinction the doctor
// and installer checks in ~/.dotfiles need and, before this verb existed, had no side-effect-free
// way to make (AGENTC-834). An issued login whose credential the broker later refused reads
// "expired", the word the dotfiles launcher gate matches, and when the helper says so
// (login_refused) stderr says the broker refused it and why that can happen. Any other
// "expired" gets the neutral line: a login that expired before anyone approved it reads the
// same, and so does a refused credential on a helper from before login_refused, which keeps
// running until it restarts. The state is the most recent login's: a re-login that was denied,
// expired or is still pending reads that way even while the credential an earlier login
// installed is still held. Every state but "issued" says on stderr what to do about it.
func cmdLauncherLoginStatus(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "agent-secrets launcher login-status: unexpected argument %q\n", args[0])
		return exitUsageError
	}
	sock, _ := helperSocket()
	resp, err := helper.Call(sock, helper.Request{Op: "login-status"}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets launcher login-status: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets launcher login-status: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	state := resp.LoginState
	if state == "" {
		state = "none"
	}
	fmt.Fprintln(stdout, state)
	switch {
	case state == "issued":
		return 0
	case state == "pending":
		fmt.Fprintf(stderr, "agent-secrets launcher login-status: a machine login is waiting for approval (code %s)\n", resp.Code)
	case state == "none":
		fmt.Fprintln(stderr, "agent-secrets launcher login-status: no machine login has run on this helper; run: agent-secrets launcher login")
	case state == "expired" && resp.LoginRefused:
		fmt.Fprintln(stderr, "agent-secrets launcher login-status: the broker refused this machine's launcher credential (expired, revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch); run: agent-secrets launcher login")
	default:
		fmt.Fprintf(stderr, "agent-secrets launcher login-status: the last machine login is %s; run: agent-secrets launcher login\n", state)
	}
	return 1
}
