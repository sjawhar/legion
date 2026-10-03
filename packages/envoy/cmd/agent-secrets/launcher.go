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
	case "-h", "-help", "--help":
		writeCommandHelp(stderr, lookupCommand("launcher login"))
		fmt.Fprintln(stderr)
		writeCommandHelp(stderr, lookupCommand("launcher login-status"))
		return 0
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
	if flaglessHelp("launcher login", args, stderr) {
		return 0
	}
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
// the helper's machine login, with no side effect — unlike re-running "login" itself, which mints
// a fresh key and opens a brand-new pending machine-login record even while a credential is
// already issued (Broker.Login only short-circuits a login that is still *pending*, per its own
// doc comment). Its exit code is a liveness probe scripts can use directly, the same distinction
// the doctor and installer checks in ~/.dotfiles need and, before this verb existed, had no
// side-effect-free way to make (AGENTC-834): 0 while the helper holds a launcher credential, which
// prints "issued"; 1 while it holds none, printing the state the helper reports ("pending",
// "denied", "expired", or "none" when no login has run). A re-login that was denied, expired
// unapproved or is still pending leaves the credential an earlier login installed in place, and
// the helper keeps enrolling sessions with it, so that still prints "issued" and exits 0, and
// stderr names the most recent login and its code. A helper from before credential_held reports
// only the most recent login, which reads "issued" exactly while its credential is held. The
// helper reports a credential the broker refused as "expired", the word the dotfiles launcher
// gate matches, until a login starts or settles (a login still pending reads "pending"), and
// when it says so (login_refused) stderr says the broker refused it and why that can happen. Any
// other "expired" gets the neutral line: a login that expired before anyone approved it reads the
// same, and so does a refused credential on a helper from before login_refused, which keeps
// running until it restarts. Every answer with no credential says on stderr what to do about it.
func cmdLauncherLoginStatus(args []string, stdout, stderr io.Writer) int {
	if flaglessHelp("launcher login-status", args, stderr) {
		return 0
	}
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
	if resp.CredentialHeld || state == "issued" {
		fmt.Fprintln(stdout, "issued")
		const held = "the helper still holds the launcher credential an earlier login issued"
		switch state {
		case "pending":
			fmt.Fprintf(stderr, "agent-secrets launcher login-status: a machine login is waiting for approval (code %s); %s\n", resp.Code, held)
		case "denied":
			fmt.Fprintf(stderr, "agent-secrets launcher login-status: the most recent machine login (code %s) was denied; %s\n", resp.Code, held)
		case "expired":
			fmt.Fprintf(stderr, "agent-secrets launcher login-status: the most recent machine login (code %s) expired before anyone approved it; %s\n", resp.Code, held)
		}
		return 0
	}
	fmt.Fprintln(stdout, state)
	switch {
	case state == "pending":
		fmt.Fprintf(stderr, "agent-secrets launcher login-status: a machine login is waiting for approval (code %s)\n", resp.Code)
	case state == "none":
		fmt.Fprintln(stderr, "agent-secrets launcher login-status: no machine login has run on this helper; run: agent-secrets launcher login")
	case resp.LoginRefused:
		fmt.Fprintln(stderr, "agent-secrets launcher login-status: the broker refused this machine's launcher credential (expired, revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch); run: agent-secrets launcher login")
	default:
		fmt.Fprintf(stderr, "agent-secrets launcher login-status: the last machine login is %s; run: agent-secrets launcher login\n", state)
	}
	return 1
}
