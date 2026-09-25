package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/api"
)

const issueStatusUsage = "usage: legion status <issue> todo|backlog|icebox [--operator-token-file <file>] [--config <legion.yaml> | --port <port>]"

// runIssueStatus is `legion status <issue> <status>`, the controller's capability: Dispatch moves
// the issue to one of the three statuses a controller sets, through the daemon's issue status
// route, which takes a controller grant. Inside a controller session the grant is the one the
// extension wrote to LEGION_GRANT_FILE for this command. From an operator's shell,
// --operator-token-file presents the operator's bearer to the grants route's operator form for a
// grant of its own; the bearer is read and sent as every operator command reads and sends it
// (operatorCall). The daemon is found as `legion state` finds it.
func runIssueStatus(ctx context.Context, issue, status string, args []string, stdout, stderr io.Writer) int {
	c := newOperatorCall("status", "file holding the operator bearer; mints the controller grant from an operator shell", stdout, stderr)
	if !c.parse(args) || (status != "todo" && status != "backlog" && status != "icebox") {
		fmt.Fprintln(stderr, issueStatusUsage)
		return 2
	}
	// One decision, made once: an operator shell mints the grant with its bearer; otherwise the
	// grant is the session's, read before the daemon is looked for.
	fromShell := *c.tokenFile != ""
	var grant string
	if !fromShell {
		read, err := grantFromEnvironment()
		if err != nil {
			fmt.Fprintf(stderr, "legion status: %v\n", err)
			return 1
		}
		grant = read
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	if fromShell {
		if code := c.send(ctx, op, http.MethodPost, "/legion/v1/grants", struct{}{}, func(_ io.Writer, answer []byte) error {
			var minted api.GrantResponse
			if err := json.Unmarshal(answer, &minted); err != nil || minted.GrantID == "" {
				return fmt.Errorf("no grant in it: %s", answer)
			}
			grant = minted.GrantID
			return nil
		}); code != 0 {
			return code
		}
	}
	// The grant authenticates the status request; the bearer has done its one job.
	return c.send(ctx, operator{base: op.base}, http.MethodPost, "/legion/v1/issues/status",
		api.IssueStatusRequest{GrantID: grant, Issue: issue, Status: status}, writeLine)
}

// writeLine writes a 2xx answer as the daemon served it, ending in a newline.
func writeLine(w io.Writer, answer []byte) error {
	if len(answer) == 0 {
		return nil
	}
	if _, err := w.Write(answer); err != nil {
		return err
	}
	if answer[len(answer)-1] != '\n' {
		_, err := fmt.Fprintln(w)
		return err
	}
	return nil
}
