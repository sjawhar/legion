// packages/envoy/cmd/agent-secrets/machine.go
//
// "agent-secrets machine login|login-status|list|revoke": the machine login the host helper holds
// for every session it registers, started and read through the helper, and the operator's own
// machines' logins, listed and ended under it (the broker's operator routes, signed by the
// helper's sign-launcher op). groupForms (main.go) dispatches them.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// cmdMachineLogin implements "machine login". It asks agent-secrets-helper to start a machine
// login, prints the broker's confirmation code for the operator to type on the Dispatch
// credential page, then polls the helper until the login reaches a terminal state.
func cmdMachineLogin(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("machine login", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets machine login: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	sock, _ := helperSocket()
	resp, err := helper.Call(sock, helper.Request{Op: "login"}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets machine login: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets machine login: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	fmt.Fprintf(stdout, "machine login code: %s\n", resp.Code)
	if base := approveURL(); base != "" {
		fmt.Fprintf(stdout, "enter it at %s/credentials/machine — approve only if the code matches this terminal\n", base)
	} else {
		fmt.Fprintln(stdout, "enter it on the Dispatch credential page")
	}
	backoff := 2 * time.Second
	for {
		resp, err := helper.Call(sock, helper.Request{Op: "login-status"}, 10*time.Second)
		if err != nil {
			fmt.Fprintf(stderr, "agent-secrets machine login: %v\n", err)
			return 1
		}
		if !resp.OK {
			fmt.Fprintf(stderr, "agent-secrets machine login: %s: %s\n", resp.Code, resp.Error)
			return 1
		}
		switch resp.LoginState {
		case "issued":
			return 0
		case "denied", "expired":
			fmt.Fprintf(stderr, "agent-secrets machine login: %s\n", resp.LoginState)
			return 1
		}
		time.Sleep(backoff)
		backoff = nextBackoff(backoff)
	}
}

// cmdMachineLoginStatus implements "machine login-status": a read-only, single-shot query of
// the helper's machine login, with no side effect — unlike re-running "login" itself, which mints
// a fresh key and opens a brand-new pending machine-login record even while a credential is
// already issued (Broker.Login only short-circuits a login that is still *pending*, per its own
// doc comment). Its exit code is a liveness probe scripts can use directly, the distinction the
// dotfiles doctor and installer checks need: 0 while the helper holds a launcher credential, which
// prints "issued"; 1 while it holds none, printing the state the helper reports ("pending",
// "denied", "expired", or "none" when no login has run). A re-login that was denied, expired
// unapproved or is still pending leaves the credential an earlier login installed in place, and
// the helper keeps enrolling sessions with it, so that still prints "issued" and exits 0, and
// stderr names the most recent login and its code. While a credential is held, stderr also says
// when it expires and how long that is from now: the broker mints no renewal, so before then a
// new machine login a human approves must replace it (the helper warns in its journal a day ahead
// and drops the credential at that moment). A helper or broker from before the expiry was
// reported gets a line saying it is unknown. A helper from before credential_held reports
// only the most recent login, which reads "issued" exactly while its credential is held. The
// helper reports a credential it dropped, because the broker refused it or it reached its expiry,
// as "expired", the word the dotfiles launcher gate matches, until a login starts or settles (a
// login still pending reads "pending"). Every answer with no credential says on stderr why, in
// the words the helper's journal uses for the same answer (helper.NoCredentialReason): the cause
// the helper names (credential_dropped), a broker refusal on a helper from before that field,
// which sets login_refused only for one, and otherwise the most recent login's state; and what to
// do about it.
func cmdMachineLoginStatus(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("machine login-status", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets machine login-status: unexpected argument %q\n", positional[0])
		return exitUsageError
	}
	sock, _ := helperSocket()
	resp, err := helper.Call(sock, helper.Request{Op: "login-status"}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets machine login-status: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets machine login-status: %s: %s\n", resp.Code, resp.Error)
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
			fmt.Fprintf(stderr, "agent-secrets machine login-status: a machine login is waiting for approval (code %s); %s\n", resp.Code, held)
		case "denied":
			fmt.Fprintf(stderr, "agent-secrets machine login-status: the most recent machine login (code %s) was denied; %s\n", resp.Code, held)
		case "expired":
			fmt.Fprintf(stderr, "agent-secrets machine login-status: the most recent machine login (code %s) expired before anyone approved it; %s\n", resp.Code, held)
		}
		fmt.Fprintln(stderr, "agent-secrets machine login-status: "+credentialExpiry(resp.CredentialExpiresAt, time.Now()))
		return 0
	}
	fmt.Fprintln(stdout, state)
	reason := helper.NoCredentialReason(resp.LoginState, resp.LoginRefused, resp.CredentialDropped)
	if state == "pending" {
		fmt.Fprintf(stderr, "agent-secrets machine login-status: %s (code %s)\n", reason, resp.Code)
	} else {
		fmt.Fprintf(stderr, "agent-secrets machine login-status: %s; run: agent-secrets machine login\n", reason)
	}
	return 1
}

// credentialExpiry is login-status's line about when the held launcher credential expires.
func credentialExpiry(expiresAt string, now time.Time) string {
	if expiresAt == "" {
		return "the helper does not know when the launcher credential expires (it, or its broker, is older than this client)"
	}
	at, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return fmt.Sprintf("the helper reported an unreadable launcher credential expiry %q", expiresAt)
	}
	left := at.Sub(now)
	if left <= 0 {
		return fmt.Sprintf("the launcher credential expired at %s; run: agent-secrets machine login", expiresAt)
	}
	return fmt.Sprintf("the launcher credential expires at %s (in %s); the broker has no renewal, so a new machine login a human approves must replace it before then", expiresAt, roughDuration(left))
}

// roughDuration is d to the minute, with days: 6d23h59m, 3h5m, 0m.
func roughDuration(d time.Duration) string {
	minutes := int(d / time.Minute)
	days, hours := minutes/(24*60), minutes/60%24
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh%dm", days, hours, minutes%60)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes%60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// cmdMachineList implements "machine list": the logins of the operator's own machines that can
// still reach a secret, newest first, each row as Dispatch's machine-login page lists it; that
// page also lists every service's login, which this leaves out, so the table has no service
// column. --json prints the broker's answer verbatim.
func cmdMachineList(args []string, stdout, stderr io.Writer) int {
	const form = "machine list"
	flagArgs, positional := splitArgs(args, nil)
	flags := newFlagSet(form, stderr)
	asJSON := flags.Bool("json", false, "print the broker's response body verbatim")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "agent-secrets %s: unexpected argument %q\n", form, positional[0])
		return exitUsageError
	}
	base, signer, err := machineContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return exitUsageError
	}
	machines, raw, err := newClient(base).OperatorMachines(context.Background(), signer)
	if err != nil {
		return operatorFail(stderr, form, err)
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CREDENTIAL_ID\tHOST\tAPPROVED_BY\tISSUED\tEXPIRES\tSTATE")
	for _, m := range machines.Credentials {
		state := "ok"
		if m.Expired {
			state = "expired"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.CredentialID, m.Host, orDash(m.ApprovedBy),
			m.IssuedAt.UTC().Format(time.RFC3339), m.ExpiresAt.UTC().Format(time.RFC3339), state)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return 1
	}
	// The legend goes to stderr, so the table on stdout stays one row per login for a script.
	if slices.ContainsFunc(machines.Credentials, func(m OperatorMachine) bool { return m.Expired }) {
		fmt.Fprintln(stderr, "STATE expired: the login enrolls no more sessions, but sessions it enrolled still run until they end; revoking it ends them.")
	}
	return 0
}

// cmdMachineRevoke implements "machine revoke CREDENTIAL_ID": it ends one of the operator's own
// machines' logins, as Dispatch's machine-login page does, and every session it enrolled; a
// service's login is not found here. Revoking this machine's own login, its id in any case the
// broker reads, ends this machine's broker access too, which it warns about on stderr.
func cmdMachineRevoke(args []string, stdout, stderr io.Writer) int {
	const form = "machine revoke"
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet(form, stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "agent-secrets %s: exactly one CREDENTIAL_ID is required\n", form)
		return exitUsageError
	}
	id := positional[0]
	base, signer, err := machineContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return exitUsageError
	}
	if err := newClient(base).RevokeOperatorMachine(context.Background(), signer, id); err != nil {
		return operatorFail(stderr, form, err)
	}
	if sameID(id, signer.credentialID) {
		fmt.Fprintf(stderr, "agent-secrets %s: revoking this machine's own login ends every session it enrolled, this one's broker access included\n", form)
	}
	fmt.Fprintf(stdout, "revoked %s\n", id)
	return 0
}

// sameID reports whether a and b are one UUID, in whatever case or form uuid.Parse reads, as the
// broker reads the id a revoke names.
func sameID(a, b string) bool {
	x, errA := uuid.Parse(a)
	y, errB := uuid.Parse(b)
	return errA == nil && errB == nil && x == y
}

// operatorFail prints a machine or grant command's failure and answers 1. The helper's refusal to
// sign (launcherRefusal) prints as it stands, and the broker refusing the machine's login says to
// log it in again.
func operatorFail(stderr io.Writer, form string, err error) int {
	var broker *apiError
	var refused *launcherRefusal
	switch {
	case errors.As(err, &refused):
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, refused)
	case errors.As(err, &broker) && broker.Status == http.StatusUnauthorized && broker.Code == "LAUNCHER_INVALID":
		fmt.Fprintf(stderr, "agent-secrets %s: %v; this machine's login is expired or revoked (run: agent-secrets machine login)\n", form, broker)
	default:
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
	}
	return 1
}
