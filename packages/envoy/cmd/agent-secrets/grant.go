// packages/envoy/cmd/agent-secrets/grant.go
//
// "agent-secrets grant list|revoke": the live grants of the operator's sessions and those the
// operator approved, listed and ended under this machine's login (the broker's operator routes,
// signed by the helper's sign-launcher op), as Dispatch's Live grants page lists and ends them.
package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// cmdGrant dispatches the grant subcommands.
func cmdGrant(args []string, stdout, stderr io.Writer) int {
	const verbs = "list, revoke"
	if len(args) == 0 {
		fmt.Fprintf(stderr, "agent-secrets grant: a subcommand is required: %s\n", verbs)
		return exitUsageError
	}
	switch args[0] {
	case "-h", "-help", "--help":
		writeCommandHelp(stderr, lookupCommand("grant list"))
		fmt.Fprintln(stderr)
		writeCommandHelp(stderr, lookupCommand("grant revoke"))
		return 0
	case "list":
		return cmdGrantList(args[1:], stdout, stderr)
	case "revoke":
		return cmdGrantRevoke(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-secrets grant: unknown subcommand %q; the subcommands are %s\n", args[0], verbs)
		return exitUsageError
	}
}

// cmdGrantList implements "grant list": every live grant of a session the operator runs, given
// automatically or on anyone's approval, and every grant the operator approved on anyone's
// session, newest first. --json prints the broker's answer verbatim, the body Dispatch's Live
// grants page reads.
func cmdGrantList(args []string, stdout, stderr io.Writer) int {
	const form = "grant list"
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
	grants, raw, err := newClient(base).OperatorGrants(context.Background(), signer)
	if err != nil {
		return operatorFail(stderr, form, err)
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "GRANT_ID\tSECRETS\tGRANTED\tAPPROVER\tSESSION\tOPERATOR\tEXPIRES")
	for _, g := range grants.Grants {
		approver := "-"
		if g.Approver != nil {
			approver = *g.Approver
		}
		session := g.Enrollment.Kind + "/" + g.Enrollment.RuntimeID
		if g.Enrollment.Slot != nil {
			session += "/" + *g.Enrollment.Slot
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", g.GrantID, strings.Join(g.Names, ","), g.Granted, approver,
			session, orDash(g.Enrollment.Operator), g.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return 1
	}
	return 0
}

// cmdGrantRevoke implements "grant revoke GRANT_ID": it ends a grant as the operator, as Dispatch's
// Live grants page does. The operator's revoke of a grant a session got automatically also
// withholds those secrets from that session: it asks before it gets them again.
func cmdGrantRevoke(args []string, stdout, stderr io.Writer) int {
	const form = "grant revoke"
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet(form, stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "agent-secrets %s: exactly one GRANT_ID is required\n", form)
		return exitUsageError
	}
	base, signer, err := machineContext()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		return exitUsageError
	}
	if err := newClient(base).RevokeOperatorGrant(context.Background(), signer, positional[0]); err != nil {
		return operatorFail(stderr, form, err)
	}
	fmt.Fprintf(stdout, "revoked %s\n", positional[0])
	return 0
}
