package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// issueStatusSynopsis is `legion status <issue> <status>`, the second line of `legion status`'s usage.
const issueStatusSynopsis = "legion status <issue> todo|backlog|icebox --operator-token-file <file> [--config <legion.yaml> | --port <port>]"

// runIssueStatus is `legion status <issue> <status>`, the operator's: Dispatch moves the issue to
// one of the three statuses a controller sets, through the daemon's issue status route, which takes
// a controller grant. --operator-token-file, required, presents the operator's bearer to the grants
// route's operator form for that grant; the bearer is read and sent as every operator command reads
// and sends it (operatorCall). The daemon is found as `legion state` finds it.
func runIssueStatus(ctx context.Context, issue, status string, args []string, stdout, stderr io.Writer) int {
	c := newOperatorCall("status", "usage: "+issueStatusSynopsis, "file holding the operator bearer, which mints the controller grant (required)", stdout, stderr)
	if code, ok := c.parse(args, "operator-token-file"); !ok {
		return code
	}
	if status != "todo" && status != "backlog" && status != "icebox" {
		c.flags.Usage()
		return 2
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	var grant string
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
